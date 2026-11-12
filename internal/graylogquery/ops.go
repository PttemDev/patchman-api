package graylogquery

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/models"
)

// ── test ────────────────────────────────────────────────────────────

type TestResult struct {
	Version   string `json:"version"`
	Hostname  string `json:"hostname,omitempty"`
	ClusterID string `json:"clusterId,omitempty"`
	Username  string `json:"username,omitempty"`
	Streams   int    `json:"streams"`
}

// Test checks the URL and credentials: server version, who we are and how
// many streams this user can read.
func (r *Registry) Test(ctx context.Context, conn models.GraylogConnection) (*TestResult, error) {
	var sys struct {
		Version   string `json:"version"`
		Hostname  string `json:"hostname"`
		ClusterID string `json:"cluster_id"`
	}
	if err := r.do(ctx, conn, "GET", "/system", nil, &sys); err != nil {
		return nil, err
	}
	res := &TestResult{Version: sys.Version, Hostname: sys.Hostname, ClusterID: sys.ClusterID}
	// /system/sessions names the authenticated user for Basic (password or
	// token) requests too; it's informative only.
	var sess struct {
		Username string `json:"username"`
	}
	if err := r.do(ctx, conn, "GET", "/system/sessions", nil, &sess); err == nil {
		res.Username = sess.Username
	}
	streams, err := r.Streams(ctx, conn)
	if err != nil {
		return nil, err
	}
	res.Streams = len(streams)
	return res, nil
}

// ── streams ─────────────────────────────────────────────────────────

type Stream struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"`
	IsDefault   bool   `json:"isDefault,omitempty"`
}

// Streams lists the streams the user may read, by title.
func (r *Registry) Streams(ctx context.Context, conn models.GraylogConnection) ([]Stream, error) {
	var raw struct {
		Streams []struct {
			ID          string `json:"id"`
			Title       string `json:"title"`
			Description string `json:"description"`
			Disabled    bool   `json:"disabled"`
			IsDefault   bool   `json:"is_default"`
		} `json:"streams"`
	}
	if err := r.do(ctx, conn, "GET", "/streams", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Stream, 0, len(raw.Streams))
	for _, s := range raw.Streams {
		out = append(out, Stream{ID: s.ID, Title: s.Title, Description: s.Description, Disabled: s.Disabled, IsDefault: s.IsDefault})
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Title) < strings.ToLower(out[j].Title) })
	return out, nil
}

// ── search ──────────────────────────────────────────────────────────

// TimeRange is a Graylog time range: relative (the last Range seconds,
// default 300), absolute (From/To, RFC 3339) or keyword ("last 1 hour",
// "yesterday" …). An unbounded "all time" search is not offered — it can
// load a large Graylog for minutes.
type TimeRange struct {
	Type    string `json:"type"`
	Range   int    `json:"range,omitempty"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Keyword string `json:"keyword,omitempty"`
}

type SearchRequest struct {
	Query   string    `json:"query"`
	Range   TimeRange `json:"range"`
	Streams []string  `json:"streams,omitempty"`
	Limit   int       `json:"limit,omitempty"`
	Offset  int       `json:"offset,omitempty"`
	// "asc" or "desc" (default) by timestamp.
	Sort string `json:"sort,omitempty"`
	// NoHistogram skips the message-count histogram.
	NoHistogram bool `json:"noHistogram,omitempty"`
}

type Message struct {
	ID        string         `json:"id"`
	Index     string         `json:"index,omitempty"`
	Timestamp string         `json:"timestamp"`
	Source    string         `json:"source,omitempty"`
	Message   string         `json:"message"`
	Fields    map[string]any `json:"fields"`
}

type Bucket struct {
	Time  string `json:"time"`
	Count int64  `json:"count"`
}

type SearchResult struct {
	Total     int64     `json:"total"`
	Messages  []Message `json:"messages"`
	Histogram []Bucket  `json:"histogram,omitempty"`
	// Interval is the histogram bucket size ("1m", "30s" …).
	Interval string `json:"interval,omitempty"`
	// From / To is the range Graylog actually searched.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// TookMs is Graylog's own search time.
	TookMs int64 `json:"tookMs,omitempty"`
}

const (
	DefaultLimit = 100
	MaxLimit     = 1000
)

// Validate fills defaults and rejects what Graylog would answer with a
// cryptic error.
func (s *SearchRequest) Validate() error {
	s.Query = strings.TrimSpace(s.Query)
	if s.Query == "" {
		s.Query = "*"
	}
	if s.Limit <= 0 {
		s.Limit = DefaultLimit
	}
	if s.Limit > MaxLimit {
		s.Limit = MaxLimit
	}
	if s.Offset < 0 {
		s.Offset = 0
	}
	if s.Sort != "asc" {
		s.Sort = "desc"
	}
	tr := &s.Range
	switch tr.Type {
	case "", "relative":
		tr.Type = "relative"
		if tr.Range < 0 {
			return fmt.Errorf("range must be a positive number of seconds")
		}
		if tr.Range == 0 {
			tr.Range = 300
		}
	case "absolute":
		from, err1 := time.Parse(time.RFC3339, tr.From)
		to, err2 := time.Parse(time.RFC3339, tr.To)
		if err1 != nil || err2 != nil {
			return fmt.Errorf("absolute range needs from and to as RFC 3339 times")
		}
		if !to.After(from) {
			return fmt.Errorf("the range's end must be after its start")
		}
	case "keyword":
		if strings.TrimSpace(tr.Keyword) == "" {
			return fmt.Errorf("keyword range needs a keyword, e.g. \"last 1 hour\"")
		}
	default:
		return fmt.Errorf("range type must be relative, absolute or keyword")
	}
	clean := s.Streams[:0]
	for _, id := range s.Streams {
		if id = strings.TrimSpace(id); id != "" {
			clean = append(clean, id)
		}
	}
	s.Streams = clean
	return nil
}

// histogram bucket sizes Graylog's "timeunit" interval accepts, smallest
// first, with their length.
var intervals = []struct {
	unit string
	d    time.Duration
}{
	{"1s", time.Second}, {"5s", 5 * time.Second}, {"10s", 10 * time.Second}, {"30s", 30 * time.Second},
	{"1m", time.Minute}, {"5m", 5 * time.Minute}, {"10m", 10 * time.Minute}, {"30m", 30 * time.Minute},
	{"1h", time.Hour}, {"3h", 3 * time.Hour}, {"6h", 6 * time.Hour}, {"12h", 12 * time.Hour},
	{"1d", 24 * time.Hour}, {"7d", 7 * 24 * time.Hour}, {"30d", 30 * 24 * time.Hour},
}

// bucketFor picks the smallest bucket that keeps a span to at most 80 bars;
// "" (Graylog's auto interval) when the span is unknown.
func bucketFor(span time.Duration) string {
	if span <= 0 {
		return ""
	}
	for _, iv := range intervals {
		if span/iv.d <= 80 {
			return iv.unit
		}
	}
	return intervals[len(intervals)-1].unit
}

func (tr TimeRange) span() time.Duration {
	switch tr.Type {
	case "relative":
		return time.Duration(tr.Range) * time.Second
	case "absolute":
		from, _ := time.Parse(time.RFC3339, tr.From)
		to, _ := time.Parse(time.RFC3339, tr.To)
		return to.Sub(from)
	}
	return 0
}

func (tr TimeRange) graylog() map[string]any {
	switch tr.Type {
	case "absolute":
		return map[string]any{"type": "absolute", "from": tr.From, "to": tr.To}
	case "keyword":
		return map[string]any{"type": "keyword", "keyword": tr.Keyword}
	}
	return map[string]any{"type": "relative", "range": tr.Range}
}

// searchBody builds the Views API sync search: one query with a messages
// search type and, unless skipped, a count-over-time pivot.
func searchBody(s SearchRequest) (map[string]any, string) {
	searchTypes := []any{map[string]any{
		"id": "messages", "type": "messages", "limit": s.Limit, "offset": s.Offset,
		"sort": []any{map[string]any{"field": "timestamp", "order": strings.ToUpper(s.Sort)}},
	}}
	unit := ""
	if !s.NoHistogram {
		unit = bucketFor(s.Range.span())
		interval := map[string]any{"type": "auto", "scaling": 1.0}
		if unit != "" {
			interval = map[string]any{"type": "timeunit", "timeunit": unit}
		}
		searchTypes = append(searchTypes, map[string]any{
			"id": "histogram", "type": "pivot", "rollup": true, "sort": []any{},
			"row_groups": []any{map[string]any{"type": "time", "fields": []string{"timestamp"}, "interval": interval}},
			"series":     []any{map[string]any{"id": "count()", "type": "count"}},
		})
	}
	q := map[string]any{
		"id":           "q",
		"query":        map[string]any{"type": "elasticsearch", "query_string": s.Query},
		"timerange":    s.Range.graylog(),
		"search_types": searchTypes,
	}
	if len(s.Streams) > 0 {
		filters := make([]any, 0, len(s.Streams))
		for _, id := range s.Streams {
			filters = append(filters, map[string]any{"type": "stream", "id": id})
		}
		q["filter"] = map[string]any{"type": "or", "filters": filters}
	}
	return map[string]any{"queries": []any{q}}, unit
}

type syncResponse struct {
	Results map[string]struct {
		Errors []struct {
			Description string `json:"description"`
		} `json:"errors"`
		ExecutionStats struct {
			Duration int64 `json:"duration"`
		} `json:"execution_stats"`
		SearchTypes map[string]struct {
			// messages
			Messages []struct {
				Index   string         `json:"index"`
				Message map[string]any `json:"message"`
			} `json:"messages"`
			TotalResults int64 `json:"total_results"`
			// pivot
			Rows []struct {
				Key    []string `json:"key"`
				Source string   `json:"source"`
				Values []struct {
					Value  any    `json:"value"`
					Source string `json:"source"`
				} `json:"values"`
			} `json:"rows"`
			EffectiveTimerange *struct {
				From string `json:"from"`
				To   string `json:"to"`
			} `json:"effective_timerange"`
		} `json:"search_types"`
	} `json:"results"`
}

// Search runs a log search and returns the page of messages (newest first
// by default), the total hit count and the count histogram.
func (r *Registry) Search(ctx context.Context, conn models.GraylogConnection, s SearchRequest) (*SearchResult, error) {
	if err := s.Validate(); err != nil {
		return nil, &APIError{Code: "bad_request", Message: err.Error()}
	}
	body, unit := searchBody(s)
	var raw syncResponse
	if err := r.do(ctx, conn, "POST", "/views/search/sync", body, &raw); err != nil {
		return nil, err
	}
	q, ok := raw.Results["q"]
	if !ok {
		return nil, &APIError{Code: "upstream", Message: "Graylog returned no search result."}
	}
	if len(q.Errors) > 0 {
		return nil, &APIError{Code: "bad_query", Message: "Graylog could not run the search: " + cleanSearchError(q.Errors[0].Description)}
	}
	res := &SearchResult{Messages: []Message{}, Interval: unit, TookMs: q.ExecutionStats.Duration}
	if m, ok := q.SearchTypes["messages"]; ok {
		res.Total = m.TotalResults
		for _, row := range m.Messages {
			res.Messages = append(res.Messages, toMessage(row.Index, row.Message))
		}
		if m.EffectiveTimerange != nil {
			res.From, res.To = m.EffectiveTimerange.From, m.EffectiveTimerange.To
		}
	}
	if h, ok := q.SearchTypes["histogram"]; ok {
		for _, row := range h.Rows {
			if len(row.Key) == 0 || len(row.Values) == 0 { // the rollup row
				continue
			}
			res.Histogram = append(res.Histogram, Bucket{Time: row.Key[0], Count: toInt(row.Values[0].Value)})
		}
		sort.SliceStable(res.Histogram, func(i, j int) bool { return res.Histogram[i].Time < res.Histogram[j].Time })
		if res.From == "" && h.EffectiveTimerange != nil {
			res.From, res.To = h.EffectiveTimerange.From, h.EffectiveTimerange.To
		}
		res.Histogram = fillBuckets(res.Histogram, unit, res.From, res.To)
	}
	return res, nil
}

// fillBuckets adds the zero-count buckets Graylog leaves out, so bars are
// evenly spaced in time across the searched range. Without a known bucket
// size or range (auto interval) the buckets are returned as they are.
func fillBuckets(in []Bucket, unit, from, to string) []Bucket {
	var step time.Duration
	for _, iv := range intervals {
		if iv.unit == unit {
			step = iv.d
		}
	}
	start, err1 := time.Parse(time.RFC3339Nano, from)
	end, err2 := time.Parse(time.RFC3339Nano, to)
	if step == 0 || err1 != nil || err2 != nil || !end.After(start) || end.Sub(start)/step > 500 {
		return in
	}
	counts := make(map[int64]int64, len(in))
	for _, b := range in {
		t, err := time.Parse(time.RFC3339Nano, b.Time)
		if err != nil {
			return in
		}
		counts[t.UnixMilli()] += b.Count
	}
	// Graylog aligns buckets to the step in the Graylog user's time zone, so
	// anchor the grid on a bucket it returned (UTC-aligned when there is
	// none) and step back to the one that holds the range's start.
	if step > 24*time.Hour {
		return in
	}
	first := start.UTC().Truncate(step)
	if len(in) > 0 {
		if anchor, err := time.Parse(time.RFC3339Nano, in[0].Time); err == nil {
			first = anchor.UTC()
			for first.After(start) {
				first = first.Add(-step)
			}
			for !first.Add(step).After(start) {
				first = first.Add(step)
			}
		}
	}
	out := make([]Bucket, 0, int(end.Sub(first)/step)+1)
	seen := 0
	for t := first; t.Before(end); t = t.Add(step) {
		c, ok := counts[t.UnixMilli()]
		if ok {
			seen++
		}
		out = append(out, Bucket{Time: t.UTC().Format("2006-01-02T15:04:05.000Z"), Count: c})
	}
	if seen != len(counts) { // buckets not on our grid: keep Graylog's own
		return in
	}
	return out
}

// cleanSearchError trims OpenSearch's stack-ish wording to the reason.
func cleanSearchError(s string) string {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "all shards failed") || strings.Contains(s, "search_phase_execution_exception") {
		return "the query is not valid search syntax (check brackets, quotes and field:value pairs). Graylog said: " + clip(s, 200)
	}
	return clip(s, 400)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// internal Graylog bookkeeping fields, left out of Fields.
var hiddenField = map[string]bool{
	"_id": true, "gl2_accounted_message_size": true, "gl2_message_id": true, "gl2_processing_duration_ms": true,
	"gl2_processing_timestamp": true, "gl2_receive_timestamp": true, "gl2_remote_ip": true, "gl2_remote_port": true,
	"gl2_source_input": true, "gl2_source_node": true, "gl2_source_collector": true, "gl2_source_radio": true,
	"gl2_source_radio_input": true,
}

func toMessage(index string, m map[string]any) Message {
	out := Message{Index: index, Fields: map[string]any{}}
	out.ID, _ = m["_id"].(string)
	out.Timestamp = fmt.Sprint(or(m["timestamp"], ""))
	out.Source, _ = m["source"].(string)
	out.Message = fmt.Sprint(or(m["message"], ""))
	for k, v := range m {
		if hiddenField[k] || k == "timestamp" || k == "source" || k == "message" {
			continue
		}
		out.Fields[k] = v
	}
	return out
}

func or(v, def any) any {
	if v == nil {
		return def
	}
	return v
}

func toInt(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	}
	return 0
}

// ── dashboards & saved searches ─────────────────────────────────────

type View struct {
	ID          string `json:"id"`
	Type        string `json:"type"` // dashboard | search
	Title       string `json:"title"`
	Summary     string `json:"summary,omitempty"`
	Description string `json:"description,omitempty"`
	Owner       string `json:"owner,omitempty"`
	UpdatedAt   string `json:"updatedAt,omitempty"`
	WebURL      string `json:"webUrl"`
}

type rawViews struct {
	// 5.x / 6.x paginated lists use "elements"; older ones "views".
	Elements []rawView `json:"elements"`
	Views    []rawView `json:"views"`
}

type rawView struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Summary       string `json:"summary"`
	Description   string `json:"description"`
	Owner         string `json:"owner"`
	LastUpdatedAt string `json:"last_updated_at"`
	CreatedAt     string `json:"created_at"`
}

// Views lists dashboards and saved searches (filtered by title when query is
// set), each with its Graylog web link. A Graylog without the saved-search
// list still returns its dashboards.
func (r *Registry) Views(ctx context.Context, conn models.GraylogConnection, query string) ([]View, error) {
	base, err := BaseURL(conn.URL)
	if err != nil {
		return nil, &APIError{Code: "bad_request", Message: err.Error()}
	}
	q := url.Values{"page": {"1"}, "per_page": {"200"}, "sort": {"title"}, "order": {"asc"}}
	if s := strings.TrimSpace(query); s != "" {
		q.Set("query", s)
	}
	var dash rawViews
	if err := r.do(ctx, conn, "GET", "/dashboards?"+q.Encode(), nil, &dash); err != nil {
		return nil, err
	}
	out := []View{}
	add := func(rows []rawView, kind, path string) {
		for _, v := range rows {
			upd := v.LastUpdatedAt
			if upd == "" {
				upd = v.CreatedAt
			}
			out = append(out, View{ID: v.ID, Type: kind, Title: v.Title, Summary: v.Summary, Description: v.Description,
				Owner: v.Owner, UpdatedAt: upd, WebURL: base + path + url.PathEscape(v.ID)})
		}
	}
	add(append(dash.Elements, dash.Views...), "dashboard", "/dashboards/")
	var saved rawViews
	if err := r.do(ctx, conn, "GET", "/search/saved?"+q.Encode(), nil, &saved); err == nil {
		add(append(saved.Elements, saved.Views...), "search", "/search/")
	}
	return out, nil
}

// WebSearchURL is the Graylog UI link for a search (so a user can continue
// in Graylog itself).
func WebSearchURL(base string, s SearchRequest) string {
	v := url.Values{"q": {s.Query}}
	switch s.Range.Type {
	case "absolute":
		v.Set("rangetype", "absolute")
		v.Set("from", s.Range.From)
		v.Set("to", s.Range.To)
	case "keyword":
		v.Set("rangetype", "keyword")
		v.Set("keyword", s.Range.Keyword)
	default:
		v.Set("rangetype", "relative")
		v.Set("relative", strconv.Itoa(s.Range.Range))
	}
	if len(s.Streams) > 0 {
		v.Set("streams", strings.Join(s.Streams, ","))
	}
	return base + "/search?" + v.Encode()
}
