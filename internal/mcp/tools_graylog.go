package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Graylog tools (read-only). They go through the same REST routes as the
// UI, so grants and the search audit apply unchanged. Answers are compacted:
// one line per stream / dashboard, one line per log message with its fields.

type graylogIDIn struct {
	ID string `json:"id" jsonschema:"Graylog connection id (see list_graylog_connections)"`
}

type graylogSearchIn struct {
	ID      string   `json:"id" jsonschema:"Graylog connection id (see list_graylog_connections)"`
	Query   string   `json:"query,omitempty" jsonschema:"Graylog search query (Lucene syntax, e.g. level:3 AND service:order-api, or \"timeout\"); default * (everything)"`
	Range   int      `json:"range,omitempty" jsonschema:"relative time range in seconds back from now (default 900 = 15 minutes); ignored when from/to or keyword is set"`
	From    string   `json:"from,omitempty" jsonschema:"absolute range start, RFC 3339 (use with to)"`
	To      string   `json:"to,omitempty" jsonschema:"absolute range end, RFC 3339"`
	Keyword string   `json:"keyword,omitempty" jsonschema:"keyword range, e.g. \"last 1 hour\" or \"yesterday\""`
	Streams []string `json:"streams,omitempty" jsonschema:"stream ids to search (see graylog_streams); default all streams you can read"`
	Limit   int      `json:"limit,omitempty" jsonschema:"messages to return (default 50, max 200)"`
	Offset  int      `json:"offset,omitempty" jsonschema:"skip this many messages (paging)"`
	Fields  []string `json:"fields,omitempty" jsonschema:"only show these message fields besides timestamp/source/message (default: all fields)"`
	Oldest  bool     `json:"oldest,omitempty" jsonschema:"oldest first instead of newest first"`
}

type graylogViewsIn struct {
	ID    string `json:"id" jsonschema:"Graylog connection id (see list_graylog_connections)"`
	Query string `json:"query,omitempty" jsonschema:"filter by title"`
}

func graylogPath(id, rest string) string { return "/api/graylog/" + url.PathEscape(id) + rest }

func registerGraylogTools(s *mcpsdk.Server, c *caller) {
	regCtx(s, "graylog_streams", "List the Graylog streams you can search (id, title, description). Read-only."+connNote,
		func(ctx context.Context, in graylogIDIn) (int, string) {
			code, body := c.callCtx(ctx, "GET", graylogPath(in.ID, "/streams"), nil)
			if code != 200 {
				return code, body
			}
			return code, compactGraylogStreams(body)
		})
	regCtx(s, "graylog_search", "Search Graylog logs: matching messages (newest first) with their fields, the total hit count, a count-over-time histogram and a link to the same search in Graylog. Use to investigate errors, trace a request id or count events. Read-only; searches are audited."+connNote,
		func(ctx context.Context, in graylogSearchIn) (int, string) {
			body := map[string]any{"query": in.Query, "offset": in.Offset}
			limit := in.Limit
			if limit <= 0 {
				limit = 50
			}
			if limit > 200 {
				limit = 200
			}
			body["limit"] = limit
			switch {
			case in.From != "" || in.To != "":
				body["range"] = map[string]any{"type": "absolute", "from": in.From, "to": in.To}
			case strings.TrimSpace(in.Keyword) != "":
				body["range"] = map[string]any{"type": "keyword", "keyword": in.Keyword}
			default:
				r := in.Range
				if r <= 0 {
					r = 900
				}
				body["range"] = map[string]any{"type": "relative", "range": r}
			}
			if len(in.Streams) > 0 {
				body["streams"] = in.Streams
			}
			if in.Oldest {
				body["sort"] = "asc"
			}
			code, resp := c.callCtx(ctx, "POST", graylogPath(in.ID, "/search"), body)
			if code != 200 {
				return code, resp
			}
			return code, compactGraylogSearch(resp, in.Fields)
		})
	regCtx(s, "graylog_dashboards", "List Graylog dashboards and saved searches with their links (PatchMan does not render Graylog dashboards; open the link, or run the search with graylog_search). Read-only."+connNote,
		func(ctx context.Context, in graylogViewsIn) (int, string) {
			p := graylogPath(in.ID, "/views")
			if q := strings.TrimSpace(in.Query); q != "" {
				p += "?query=" + url.QueryEscape(q)
			}
			code, body := c.callCtx(ctx, "GET", p, nil)
			if code != 200 {
				return code, body
			}
			return code, compactGraylogViews(body)
		})
}

func compactGraylogStreams(body string) string {
	var env struct {
		Data []struct {
			ID          string `json:"id"`
			Title       string `json:"title"`
			Description string `json:"description"`
			Disabled    bool   `json:"disabled"`
			IsDefault   bool   `json:"isDefault"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(body), &env) != nil {
		return body
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d streams\n", len(env.Data))
	for _, s := range env.Data {
		fmt.Fprintf(&b, "%s  %s", s.ID, s.Title)
		if s.IsDefault {
			b.WriteString(" (default)")
		}
		if s.Disabled {
			b.WriteString(" (paused)")
		}
		if s.Description != "" {
			b.WriteString(" — " + s.Description)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func compactGraylogSearch(body string, only []string) string {
	var env struct {
		Data struct {
			Total    int64 `json:"total"`
			Messages []struct {
				Timestamp string         `json:"timestamp"`
				Source    string         `json:"source"`
				Message   string         `json:"message"`
				Fields    map[string]any `json:"fields"`
			} `json:"messages"`
			Histogram []struct {
				Time  string `json:"time"`
				Count int64  `json:"count"`
			} `json:"histogram"`
			Interval string `json:"interval"`
			From     string `json:"from"`
			To       string `json:"to"`
			WebURL   string `json:"webUrl"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(body), &env) != nil {
		return body
	}
	d := env.Data
	var b strings.Builder
	fmt.Fprintf(&b, "%d messages match", d.Total)
	if d.From != "" {
		fmt.Fprintf(&b, " between %s and %s", d.From, d.To)
	}
	fmt.Fprintf(&b, "; showing %d.\n", len(d.Messages))
	if len(d.Histogram) > 0 {
		var peak int64
		peakAt := ""
		for _, h := range d.Histogram {
			if h.Count > peak {
				peak, peakAt = h.Count, h.Time
			}
		}
		fmt.Fprintf(&b, "Histogram: %d buckets of %s, peak %d at %s.\n", len(d.Histogram), d.Interval, peak, peakAt)
	}
	if d.WebURL != "" {
		b.WriteString("In Graylog: " + d.WebURL + "\n")
	}
	keep := map[string]bool{}
	for _, f := range only {
		keep[f] = true
	}
	for _, m := range d.Messages {
		ts := m.Timestamp
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			ts = t.UTC().Format("2006-01-02T15:04:05.000Z")
		}
		fmt.Fprintf(&b, "\n%s  %s  %s", ts, m.Source, oneLine(m.Message, 500))
		keys := make([]string, 0, len(m.Fields))
		for k := range m.Fields {
			if len(keep) == 0 || keep[k] {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			if k == "streams" {
				continue
			}
			fmt.Fprintf(&b, " | %s=%s", k, oneLine(fmt.Sprint(m.Fields[k]), 200))
		}
	}
	return b.String()
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func compactGraylogViews(body string) string {
	var env struct {
		Data []struct {
			Type    string `json:"type"`
			Title   string `json:"title"`
			Summary string `json:"summary"`
			WebURL  string `json:"webUrl"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(body), &env) != nil {
		return body
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d dashboards / saved searches\n", len(env.Data))
	for _, v := range env.Data {
		fmt.Fprintf(&b, "[%s] %s", v.Type, v.Title)
		if v.Summary != "" {
			b.WriteString(" — " + oneLine(v.Summary, 160))
		}
		b.WriteString("  " + v.WebURL + "\n")
	}
	return b.String()
}
