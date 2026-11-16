package investigation

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/julienschmidt/httprouter"
	"watchdog.onebusaway.org/internal/db/dbgen"
)

const (
	kindOBAMetricsResponse          resourceKind = "oba_metrics_response"
	kindUnmatchedStopStaticContexts resourceKind = "unmatched_stop_static_contexts"
)

// The link name also matches the resource kind registered by the static
// version endpoint (#249).
const stopStaticVersionLink = "gtfs_static_version"

// unmatchedStopEpisodeIncludable uses the kinds the agency, server and reported-stop endpoints register.
var unmatchedStopEpisodeIncludable = map[string]resourceKind{
	"reported_stop":    "reported_stop",
	"agency":           "agency",
	"server":           "server",
	"opening_response": kindOBAMetricsResponse,
	"closing_response": kindOBAMetricsResponse,
	"static_contexts":  kindUnmatchedStopStaticContexts,
}

// unmatchedStopEpisode is the representation of one unmatched-stop episode (E14, E15).
type unmatchedStopEpisode struct {
	UnmatchedStopEpisodeID int64    `json:"unmatched_stop_episode_id"`
	Status                 string   `json:"status"`
	StopID                 string   `json:"stop_id"`
	AgencyID               int64    `json:"agency_id"`
	ServerID               int64    `json:"server_id"`
	OpenedAt               apiTime  `json:"opened_at"`
	LastReportedAt         apiTime  `json:"last_reported_at"`
	ClosedAt               *apiTime `json:"closed_at"`
	DurationSeconds        *int64   `json:"duration_seconds"`
	CloseReason            *string  `json:"close_reason"`
	PreviousEpisodeCount   int64    `json:"previous_episode_count"`
	PreviousClosedAt       *apiTime `json:"previous_closed_at"`
	Links                  links    `json:"links"`
}

// unmatchedStopEpisodeBody is the E15 response.
type unmatchedStopEpisodeBody struct {
	unmatchedStopEpisode
	Included included `json:"included,omitzero"`
}

// unmatchedStopEpisodesBody is the E14 response.
type unmatchedStopEpisodesBody struct {
	Items    []unmatchedStopEpisode `json:"items"`
	Links    links                  `json:"links"`
	Included included               `json:"included,omitzero"`
}

func unmatchedStopEpisodePath(id int64) string {
	return "/v1/unmatched-stop-episodes/" + strconv.FormatInt(id, 10)
}

// unmatchedStopEpisodeLinks builds an episode's links; closing_response only exists once it closed.
func unmatchedStopEpisodeLinks(row dbgen.UnmatchedStopEpisodeView) itemLinks {
	self := unmatchedStopEpisodePath(row.UnmatchedStopEpisodeID)
	l := itemLinks{
		"self": {URL: self},
		"reported_stop": {
			Kind: "reported_stop",
			ID:   row.ReportedStopID,
			URL:  "/v1/reported-stops/" + strconv.FormatInt(row.ReportedStopID, 10),
		},
		"agency": {
			Kind: "agency",
			ID:   row.AgencyID,
			URL:  "/v1/agencies/" + strconv.FormatInt(row.AgencyID, 10),
		},
		"server": {
			Kind: "server",
			ID:   row.ServerID,
			URL:  "/v1/servers/" + strconv.FormatInt(row.ServerID, 10),
		},
		"opening_response": {
			Kind: kindOBAMetricsResponse,
			ID:   row.OpeningResponseID,
			URL:  "/v1/oba-metrics-responses/" + strconv.FormatInt(row.OpeningResponseID, 10),
		},
		"static_contexts": {
			Kind: kindUnmatchedStopStaticContexts,
			ID:   row.UnmatchedStopEpisodeID,
			URL:  self + "/static-contexts",
		},
	}
	if row.ClosingResponseID != nil {
		l["closing_response"] = related{
			Kind: kindOBAMetricsResponse,
			ID:   *row.ClosingResponseID,
			URL:  "/v1/oba-metrics-responses/" + strconv.FormatInt(*row.ClosingResponseID, 10),
		}
	}
	return l
}

// stopStaticContext records what the static GTFS said about the stop during
// part of the episode.
type stopStaticContext struct {
	StopMetadataVersionID int64    `json:"stop_metadata_version_id"`
	Source                string   `json:"source"`
	StaticStopFound       bool     `json:"static_stop_found"`
	StopName              *string  `json:"stop_name"`
	Latitude              *float64 `json:"latitude"`
	Longitude             *float64 `json:"longitude"`
	ParentStationID       *string  `json:"parent_station_id"`
	LocationType          *int16   `json:"location_type"`
	AppliesFrom           apiTime  `json:"applies_from"`
	AppliesUntil          *apiTime `json:"applies_until"`
	Links                 links    `json:"links"`
}

type stopStaticContextsBody struct {
	Items    []stopStaticContext `json:"items"`
	Links    links               `json:"links"`
	Included included            `json:"included,omitzero"`
}

type stopStaticContexts struct {
	body      stopStaticContextsBody
	itemLinks []itemLinks
	closedAt  *time.Time
}

func queryStopStaticContexts(ctx context.Context, q *dbgen.Queries, ids []int64, includeVersions bool) (map[int64]*stopStaticContexts, error) {
	rows, err := q.ListUnmatchedStopEpisodeStaticContexts(ctx, ids)
	if err != nil {
		return nil, err
	}
	return stopStaticContextsByEpisode(rows, includeVersions), nil
}

// stopStaticContextsByEpisode receives rows in publication order per episode.
// A row without metadata still represents an existing episode.
func stopStaticContextsByEpisode(rows []dbgen.ListUnmatchedStopEpisodeStaticContextsRow, includeVersions bool) map[int64]*stopStaticContexts {
	byEpisode := make(map[int64]*stopStaticContexts)
	for _, row := range rows {
		collection := byEpisode[row.UnmatchedStopEpisodeID]
		if collection == nil {
			episodePath := unmatchedStopEpisodePath(row.UnmatchedStopEpisodeID)
			collection = &stopStaticContexts{
				body: stopStaticContextsBody{
					Items: []stopStaticContext{},
					Links: links{"self": episodePath + "/static-contexts", "episode": episodePath},
				},
				closedAt: row.ClosedAt,
			}
			byEpisode[row.UnmatchedStopEpisodeID] = collection
		}
		if row.StopMetadataVersionID == nil {
			continue
		}

		// The metadata link and version foreign keys guarantee that the version
		// columns are present whenever the metadata ID is present.
		start := row.OpenedAt
		if row.PublishedAt.After(start) {
			start = *row.PublishedAt
		}
		items := collection.body.Items
		if len(items) > 0 {
			items[len(items)-1].AppliesUntil = newOptionalAPITime(&start)
		}
		versionID := *row.GtfsStaticVersionID
		versionURL := "/v1/gtfs-static-versions/" + strconv.FormatInt(versionID, 10)
		if includeVersions {
			collection.itemLinks = append(collection.itemLinks, itemLinks{stopStaticVersionLink: {
				Kind: stopStaticVersionLink,
				ID:   versionID,
				URL:  versionURL,
			}})
		}
		collection.body.Items = append(items, stopStaticContext{
			StopMetadataVersionID: *row.StopMetadataVersionID,
			Source:                "watchdog_static_gtfs",
			StaticStopFound:       *row.StaticStopFound,
			StopName:              row.StopName,
			Latitude:              row.Latitude,
			Longitude:             row.Longitude,
			ParentStationID:       row.ParentStationID,
			LocationType:          row.LocationType,
			AppliesFrom:           newAPITime(start),
			AppliesUntil:          newOptionalAPITime(row.ClosedAt),
			Links:                 links{stopStaticVersionLink: versionURL},
		})
	}
	return byEpisode
}

func loadStopStaticContexts(ctx context.Context, q *dbgen.Queries, ids []int64) (map[int64]any, error) {
	collections, err := queryStopStaticContexts(ctx, q, ids, false)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]any, len(collections))
	for id, collection := range collections {
		out[id] = collection.body
	}
	return out, nil
}

// listUnmatchedStopEpisodeStaticContexts serves an episode's static GTFS history.
func (s *InvestigationService) listUnmatchedStopEpisodeStaticContexts(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	id, ok := pathID(ps, "episode_id")
	if !ok {
		s.fail(w, r, pgx.ErrNoRows)
		return
	}
	var bad invalidParams
	values := parseQuery(r, &bad, "include")
	inc := s.parseInclude(values, map[string]resourceKind{stopStaticVersionLink: stopStaticVersionLink}, &bad)
	if len(bad) > 0 {
		writeInvalidParams(w, bad)
		return
	}

	var collection *stopStaticContexts
	err := s.read(r.Context(), func(ctx context.Context, q *dbgen.Queries) error {
		collections, err := queryStopStaticContexts(ctx, q, []int64{id}, inc.has(stopStaticVersionLink))
		if err != nil {
			return err
		}
		collection = collections[id]
		if collection == nil {
			return pgx.ErrNoRows
		}
		collection.body.Included, err = s.loadIncluded(ctx, q, collection.itemLinks, inc)
		return err
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	cache := cacheShort()
	if collection.closedAt != nil {
		cache = cacheImmutable(*collection.closedAt)
	}
	s.writeJSON(w, r, collection.body, cache)
}

// unmatchedStopEpisodeRepresentation maps one view row to its E14 and E15 representation.
func unmatchedStopEpisodeRepresentation(row dbgen.UnmatchedStopEpisodeView, l itemLinks) unmatchedStopEpisode {
	ep := unmatchedStopEpisode{
		UnmatchedStopEpisodeID: row.UnmatchedStopEpisodeID,
		Status:                 "open",
		StopID:                 row.StopID,
		AgencyID:               row.AgencyID,
		ServerID:               row.ServerID,
		OpenedAt:               newAPITime(row.OpenedAt),
		LastReportedAt:         newAPITime(row.LastReportedAt),
		ClosedAt:               newOptionalAPITime(row.ClosedAt),
		CloseReason:            row.CloseReason,
		PreviousEpisodeCount:   row.PreviousEpisodeCount,
		PreviousClosedAt:       newOptionalAPITime(row.PreviousClosedAt),
		Links:                  l.links(),
	}
	if row.ClosingResponseAgencyID != nil {
		ep.Status = "closed"
	}
	if row.ClosedAt != nil {
		seconds := int64(row.ClosedAt.Sub(row.OpenedAt) / time.Second)
		ep.DurationSeconds = &seconds
	}
	return ep
}

// getUnmatchedStopEpisode serves GET /v1/unmatched-stop-episodes/:episode_id.
func (s *InvestigationService) getUnmatchedStopEpisode(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	id, ok := pathID(ps, "episode_id")
	if !ok {
		s.fail(w, r, pgx.ErrNoRows)
		return
	}
	var bad invalidParams
	values := parseQuery(r, &bad, "include")
	inc := s.parseInclude(values, unmatchedStopEpisodeIncludable, &bad)
	if len(bad) > 0 {
		writeInvalidParams(w, bad)
		return
	}

	var body unmatchedStopEpisodeBody
	var closedAt *time.Time
	err := s.read(r.Context(), func(ctx context.Context, q *dbgen.Queries) error {
		row, err := q.GetUnmatchedStopEpisode(ctx, id)
		if err != nil {
			return err
		}
		closedAt = row.ClosedAt
		l := unmatchedStopEpisodeLinks(row)
		body.unmatchedStopEpisode = unmatchedStopEpisodeRepresentation(row, l)
		body.Included, err = s.loadIncluded(ctx, q, []itemLinks{l}, inc)
		return err
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	cache := cacheShort()
	if closedAt != nil && !inc.requested() {
		cache = cacheImmutable(*closedAt)
	}
	s.writeJSON(w, r, body, cache)
}

// listUnmatchedStopEpisodes serves GET /v1/unmatched-stop-episodes.
func (s *InvestigationService) listUnmatchedStopEpisodes(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	var bad invalidParams
	values := parseQuery(r, &bad, "server_id", "agency_id", "stop_id", "status", "active_from", "active_to",
		"close_reason", "static_found", "include", "page_size", "cursor")
	params := dbgen.ListUnmatchedStopEpisodesParams{
		ServerID:    paramID(values, "server_id", &bad),
		AgencyID:    paramID(values, "agency_id", &bad),
		ObaStopID:   paramString(values, "stop_id", &bad),
		Status:      paramEnum(values, "status", []string{"open", "closed"}, &bad),
		CloseReason: paramEnum(values, "close_reason", []string{"not_reported"}, &bad),
		StaticFound: paramBool(values, "static_found", &bad),
	}
	if params.ObaStopID != nil && !values.Has("agency_id") {
		bad.add("stop_id", "requires agency_id")
	}
	statusOpen := params.Status != nil && *params.Status == "open"
	win := s.parseWindow(values, activeWindowOptions(statusOpen, false), &bad)
	inc := s.parseInclude(values, unmatchedStopEpisodeIncludable, &bad)
	p := parsePage(values, &bad)
	if len(bad) > 0 {
		writeInvalidParams(w, bad)
		return
	}
	params.ActiveFrom, params.ActiveTo = win.From, win.To
	params.AfterID, params.PageLimit = p.AfterID, p.Limit()

	var body unmatchedStopEpisodesBody
	err := s.read(r.Context(), func(ctx context.Context, q *dbgen.Queries) error {
		rows, err := q.ListUnmatchedStopEpisodes(ctx, params)
		if err != nil {
			return err
		}
		rows, hasMore := trimPage(rows, p)
		body.Items = make([]unmatchedStopEpisode, 0, len(rows))
		all := make([]itemLinks, 0, len(rows))
		for _, row := range rows {
			l := unmatchedStopEpisodeLinks(row)
			all = append(all, l)
			body.Items = append(body.Items, unmatchedStopEpisodeRepresentation(row, l))
		}
		var lastID int64
		if len(rows) > 0 {
			lastID = rows[len(rows)-1].UnmatchedStopEpisodeID
		}
		body.Links = pageLinks(r, lastID, hasMore)
		body.Included, err = s.loadIncluded(ctx, q, all, inc)
		return err
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, r, body, cacheShort())
}
