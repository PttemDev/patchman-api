package esquery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"patchman-be/internal/models"
)

// Discover-style search over a Kibana data view: the data view's pattern is
// the index target, its time field drives the range filter / sort /
// histogram, and its runtime fields are sent as runtime_mappings. Everything
// goes through rawDo, so direct ES and kibanaProxy connections behave alike.

// DataViewSearchRequest is the FE / MCP wire contract.
type DataViewSearchRequest struct {
	Space string `json:"space"`
	// Tenant = OSD security tenant; nil = the connection's configured one,
	// "" = OSD's default (no header).
	Tenant *string `json:"tenant,omitempty"`
	// From / To accept ISO-8601 or ES date math ("now-15m", "now/d").
	// Defaults: now-15m / now. Ignored when the data view has no time field.
	From  string `json:"from"`
	To    string `json:"to"`
	Query string `json:"query"` // Lucene query_string; empty = match_all
	Size  int    `json:"size"`  // default 100, max 500
	// Sort is "desc" (newest first, default) or "asc".
	Sort string `json:"sort"`
	// SearchAfter = the last hit's `sort` array of the previous page.
	SearchAfter []any `json:"searchAfter,omitempty"`
	// PitID continues a point-in-time opened by the first page.
	PitID     string `json:"pitId,omitempty"`
	Histogram bool   `json:"histogram"`
	// DryRun returns the built request (for "open in console") without
	// running it.
	DryRun bool `json:"dryRun"`
	// Pattern overrides the data view lookup (MCP / tests); normally empty.
	Pattern string `json:"-"`
}

// BuiltQuery is the ES request the search runs (also the console prefill).
type BuiltQuery struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body"`
}

// HistBucket is one auto_date_histogram bucket.
type HistBucket struct {
	Key         int64  `json:"key"`
	KeyAsString string `json:"keyAsString,omitempty"`
	Count       int64  `json:"count"`
}

// DataViewSearchResult is the parsed search response.
type DataViewSearchResult struct {
	DataView      DataView          `json:"dataView"`
	Query         BuiltQuery        `json:"query"`
	Status        int               `json:"status,omitempty"`
	TookMs        int               `json:"tookMs,omitempty"`
	Total         int64             `json:"total"`
	TotalRelation string            `json:"totalRelation,omitempty"` // eq | gte
	Hits          []json.RawMessage `json:"hits"`
	Buckets       []HistBucket      `json:"buckets,omitempty"`
	Interval      string            `json:"interval,omitempty"`
	PitID         string            `json:"pitId,omitempty"`
	// HasMore is a hint: the page came back full.
	HasMore bool `json:"hasMore"`
}

const (
	defaultDVSize = 100
	maxDVSize     = 500
	pitKeepAlive  = "1m" // each page renews it
	histBuckets   = 60
)

func normalizeDVRequest(req *DataViewSearchRequest) error {
	if req.Size <= 0 {
		req.Size = defaultDVSize
	}
	if req.Size > maxDVSize {
		req.Size = maxDVSize
	}
	req.Sort = strings.ToLower(strings.TrimSpace(req.Sort))
	if req.Sort != "asc" {
		req.Sort = "desc"
	}
	req.From = strings.TrimSpace(req.From)
	req.To = strings.TrimSpace(req.To)
	if req.From == "" {
		req.From = "now-15m"
	}
	if req.To == "" {
		req.To = "now"
	}
	if len(req.SearchAfter) > 8 {
		return fmt.Errorf("searchAfter has too many values")
	}
	if len(req.Query) > 32<<10 {
		return fmt.Errorf("query is too long")
	}
	return nil
}

// buildSearchBody builds the _search body. withPit adds the pit clause (and
// the index target then moves out of the path). Pure; unit-tested.
func buildSearchBody(dv DataView, req DataViewSearchRequest, pitID string, opensearch bool) map[string]any {
	body := map[string]any{"size": req.Size}

	var filters []any
	if dv.TimeField != "" {
		filters = append(filters, map[string]any{
			"range": map[string]any{dv.TimeField: map[string]any{
				"gte": req.From, "lte": req.To, "format": "strict_date_optional_time",
			}},
		})
	}
	var must []any
	if q := strings.TrimSpace(req.Query); q != "" {
		must = append(must, map[string]any{"query_string": map[string]any{
			"query": q, "analyze_wildcard": true, "lenient": true,
		}})
	}
	if len(filters) == 0 && len(must) == 0 {
		body["query"] = map[string]any{"match_all": map[string]any{}}
	} else {
		b := map[string]any{}
		if len(filters) > 0 {
			b["filter"] = filters
		}
		if len(must) > 0 {
			b["must"] = must
		}
		body["query"] = map[string]any{"bool": b}
	}

	if dv.TimeField != "" {
		body["sort"] = []any{map[string]any{dv.TimeField: map[string]any{
			"order": req.Sort, "unmapped_type": "boolean",
		}}}
		// ISO time even when _source stores epoch / a custom format.
		body["docvalue_fields"] = []any{map[string]any{
			"field": dv.TimeField, "format": "strict_date_optional_time",
		}}
	}

	if len(dv.RuntimeFields) > 0 && !opensearch {
		rm := map[string]any{}
		names := make([]any, 0, len(dv.RuntimeFields))
		for name, def := range dv.RuntimeFields {
			m, ok := def.(map[string]any)
			if !ok {
				continue
			}
			entry := map[string]any{}
			if t, ok := m["type"].(string); ok && t != "" && t != "composite" {
				entry["type"] = t
			} else {
				continue // composite runtime fields need fields{} — skip, Kibana-only
			}
			if sc, ok := m["script"].(map[string]any); ok {
				if src, ok := sc["source"].(string); ok && strings.TrimSpace(src) != "" {
					entry["script"] = map[string]any{"source": src}
				}
			}
			rm[name] = entry
			names = append(names, name)
		}
		if len(rm) > 0 {
			body["runtime_mappings"] = rm
			body["fields"] = names // runtime values are not in _source
		}
	}

	if len(req.SearchAfter) > 0 {
		body["search_after"] = req.SearchAfter
	} else {
		body["track_total_hits"] = true
		if req.Histogram && dv.TimeField != "" {
			body["aggs"] = map[string]any{"histogram": map[string]any{
				"auto_date_histogram": map[string]any{"field": dv.TimeField, "buckets": histBuckets},
			}}
		}
	}
	if pitID != "" {
		body["pit"] = map[string]any{"id": pitID, "keep_alive": pitKeepAlive}
	}
	return body
}

// patternPath returns the escaped index target for a data view pattern. The
// pattern is passed as-is (commas, -exclusions, remote: CCS); only escaped.
func patternPath(pattern string) string {
	return "/" + url.PathEscape(strings.TrimSpace(pattern))
}

const searchParams = "?ignore_unavailable=true&allow_no_indices=true"

// ConsoleQuery returns the request a console prefill should show (no PIT).
func ConsoleQuery(dv DataView, req DataViewSearchRequest, opensearch bool) (BuiltQuery, error) {
	if err := normalizeDVRequest(&req); err != nil {
		return BuiltQuery{}, err
	}
	req.SearchAfter = nil
	b, _ := json.Marshal(buildSearchBody(dv, req, "", opensearch))
	// Human-readable for the console buffer: the raw pattern, not %-escaped
	// (the console's own request path escapes it when it runs).
	return BuiltQuery{Method: http.MethodPost, Path: "/" + strings.TrimSpace(dv.Pattern) + "/_search" + searchParams, Body: b}, nil
}

func isOpenSearch(conn models.ESConnection) bool {
	return strings.EqualFold(strings.TrimSpace(conn.Flavor), "opensearch")
}

// DataViewSearch runs a Discover-style search. The first page opens a point
// in time (ES 7.10+) so "load more" pages are consistent and get the implicit
// _shard_doc tiebreaker; when PIT is unavailable (OpenSearch, old ES, missing
// privilege) it falls back to plain search_after on the time field.
func (r *Registry) DataViewSearch(ctx context.Context, conn models.ESConnection, dv DataView, req DataViewSearchRequest) (*DataViewSearchResult, error) {
	if err := normalizeDVRequest(&req); err != nil {
		return nil, err
	}
	if strings.TrimSpace(dv.Pattern) == "" {
		return nil, fmt.Errorf("data view has no index pattern")
	}
	os := isOpenSearch(conn)
	out := &DataViewSearchResult{DataView: dv, Hits: []json.RawMessage{}}

	if req.DryRun {
		q, err := ConsoleQuery(dv, req, os)
		if err != nil {
			return nil, err
		}
		out.Query = q
		return out, nil
	}

	pit := strings.TrimSpace(req.PitID)
	if pit == "" && len(req.SearchAfter) == 0 && !os {
		pit = r.openPIT(ctx, conn, dv.Pattern) // "" on failure → plain search
	}

	body := buildSearchBody(dv, req, pit, os)
	path := patternPath(dv.Pattern) + "/_search" + searchParams
	if pit != "" {
		path = "/_search" // index target lives in the PIT
	}
	raw, _ := json.Marshal(body)
	out.Query = BuiltQuery{Method: http.MethodPost, Path: path, Body: raw}

	res, err := r.rawDo(ctx, conn, http.MethodPost, path, raw)
	if err != nil {
		return nil, err
	}
	out.Status, out.TookMs = res.Status, res.TookMs
	if res.Status < 200 || res.Status >= 300 {
		if req.PitID != "" && strings.Contains(string(res.Body), "search_context_missing") {
			return nil, fmt.Errorf("the paging session expired — run the search again")
		}
		return nil, upstreamError(res)
	}
	if err := parseSearchResponse(res.Body, out); err != nil {
		return nil, err
	}
	if out.PitID == "" {
		out.PitID = pit
	}
	out.HasMore = len(out.Hits) >= req.Size
	if !out.HasMore && out.PitID != "" {
		// Last page: release the search contexts now instead of waiting for
		// keep_alive to lapse. Best effort.
		r.closePIT(ctx, conn, out.PitID)
		out.PitID = ""
	}
	return out, nil
}

// closePIT releases a point in time; errors are ignored (keep_alive bounds it).
func (r *Registry) closePIT(ctx context.Context, conn models.ESConnection, id string) {
	b, _ := json.Marshal(map[string]string{"id": id})
	_, _ = r.rawDo(ctx, conn, http.MethodDelete, "/_pit", b)
}

// openPIT opens a point in time over the pattern; "" when unsupported.
func (r *Registry) openPIT(ctx context.Context, conn models.ESConnection, pattern string) string {
	res, err := r.rawDo(ctx, conn, http.MethodPost,
		patternPath(pattern)+"/_pit?keep_alive="+pitKeepAlive+"&ignore_unavailable=true", nil)
	if err != nil || res.Status < 200 || res.Status >= 300 {
		return ""
	}
	var b struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(res.Body, &b) != nil {
		return ""
	}
	return b.ID
}

// parseSearchResponse fills hits / total / histogram / pit id. Pure.
func parseSearchResponse(raw []byte, out *DataViewSearchResult) error {
	var body struct {
		PitID string `json:"pit_id"`
		Hits  struct {
			Total json.RawMessage   `json:"total"`
			Hits  []json.RawMessage `json:"hits"`
		} `json:"hits"`
		Aggregations struct {
			Histogram struct {
				Interval string `json:"interval"`
				Buckets  []struct {
					Key         int64  `json:"key"`
					KeyAsString string `json:"key_as_string"`
					DocCount    int64  `json:"doc_count"`
				} `json:"buckets"`
			} `json:"histogram"`
		} `json:"aggregations"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Errorf("unexpected search response: %w", err)
	}
	out.PitID = body.PitID
	if body.Hits.Hits != nil {
		out.Hits = body.Hits.Hits
	}
	// total: {"value":n,"relation":"eq"} (7+) or a bare number (6.x).
	var tot struct {
		Value    int64  `json:"value"`
		Relation string `json:"relation"`
	}
	if json.Unmarshal(body.Hits.Total, &tot) == nil && (tot.Relation != "" || tot.Value > 0) {
		out.Total, out.TotalRelation = tot.Value, tot.Relation
	} else {
		var n int64
		if json.Unmarshal(body.Hits.Total, &n) == nil {
			out.Total, out.TotalRelation = n, "eq"
		}
	}
	out.Interval = body.Aggregations.Histogram.Interval
	for _, b := range body.Aggregations.Histogram.Buckets {
		out.Buckets = append(out.Buckets, HistBucket{Key: b.Key, KeyAsString: b.KeyAsString, Count: b.DocCount})
	}
	return nil
}
