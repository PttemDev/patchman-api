package mcp

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Grafana tools: all read-only. They go through the same REST routes as the
// Dashboards UI, so the connection's grants, the read-only SQL guard and the
// audit trail apply unchanged. Answers are compacted for an LLM: dashboards
// come back as a panel/query outline, panel data as per-series stats plus a
// few sample points instead of raw frames.

type grafanaSearchIn struct {
	ID    string `json:"id" jsonschema:"Grafana connection id (see list_grafana_connections)"`
	Query string `json:"query,omitempty" jsonschema:"filter dashboards by title"`
}

type grafanaDashboardIn struct {
	ID  string `json:"id" jsonschema:"Grafana connection id"`
	UID string `json:"uid" jsonschema:"dashboard uid (from grafana_dashboards)"`
}

type grafanaPanelDataIn struct {
	ID        string              `json:"id" jsonschema:"Grafana connection id"`
	UID       string              `json:"uid" jsonschema:"dashboard uid"`
	PanelID   int                 `json:"panelId,omitempty" jsonschema:"panel id (from grafana_dashboard); or use title"`
	Title     string              `json:"title,omitempty" jsonschema:"panel title (case-insensitive) when panelId is not known"`
	From      string              `json:"from,omitempty" jsonschema:"range start: now-1h, now-7d, epoch ms or RFC3339 (default: the dashboard's range)"`
	To        string              `json:"to,omitempty" jsonschema:"range end (default now)"`
	Variables map[string][]string `json:"variables,omitempty" jsonschema:"dashboard variable overrides, e.g. {\"job\":[\"api\"]}; [\"$__all\"] selects All"`
	Raw       bool                `json:"raw,omitempty" jsonschema:"return raw data frames instead of the per-series summary (large)"`
}

type grafanaQueryIn struct {
	ID         string         `json:"id" jsonschema:"Grafana connection id"`
	Datasource string         `json:"datasource" jsonschema:"datasource uid or name (see grafana_datasources)"`
	Expr       string         `json:"expr,omitempty" jsonschema:"PromQL / LogQL expression (Prometheus, Loki)"`
	RawSQL     string         `json:"rawSql,omitempty" jsonschema:"SQL for SQL datasources (read-only unless you hold a write grant)"`
	Target     map[string]any `json:"target,omitempty" jsonschema:"full Grafana query target for other datasources (overrides expr/rawSql)"`
	From       string         `json:"from,omitempty" jsonschema:"range start (default now-1h)"`
	To         string         `json:"to,omitempty" jsonschema:"range end (default now)"`
	Instant    bool           `json:"instant,omitempty" jsonschema:"instant query (single point per series)"`
	Raw        bool           `json:"raw,omitempty" jsonschema:"return raw data frames instead of the summary"`
}

func registerGrafanaTools(s *mcpsdk.Server, c *caller) {
	regCtx(s, "grafana_datasources", "List a Grafana connection's datasources (uid, name, type) and the Grafana version. Read-only."+connNote,
		func(ctx context.Context, in grafanaSearchIn) (int, string) {
			return c.callCtx(ctx, "GET", "/api/grafana/"+url.PathEscape(in.ID)+"/info", nil)
		})
	regCtx(s, "grafana_dashboards", "Search Grafana dashboards and folders by title. Read-only; returns uids for grafana_dashboard / grafana_panel_data."+connNote,
		func(ctx context.Context, in grafanaSearchIn) (int, string) {
			v := url.Values{}
			if in.Query != "" {
				v.Set("query", in.Query)
			}
			code, body := c.callCtx(ctx, "GET", withQuery("/api/grafana/"+url.PathEscape(in.ID)+"/search", v), nil)
			if code != 200 {
				return code, body
			}
			return code, compactSearch(body)
		})
	regCtx(s, "grafana_dashboard", "Describe a Grafana dashboard: variables, time range and every panel's id, title, type and queries. Read-only."+connNote,
		func(ctx context.Context, in grafanaDashboardIn) (int, string) {
			code, body := c.callCtx(ctx, "GET", "/api/grafana/"+url.PathEscape(in.ID)+"/dashboards/"+url.PathEscape(in.UID), nil)
			if code != 200 {
				return code, body
			}
			return code, compactDashboard(body)
		})
	regCtx(s, "grafana_panel_data",
		"Run one dashboard panel's queries (variables resolved like the dashboard, optionally overridden) and return per-series last/min/max/avg plus sample points. Read-only."+connNote,
		func(ctx context.Context, in grafanaPanelDataIn) (int, string) {
			sel := map[string]any{}
			for k, v := range in.Variables {
				sel[k] = map[string]any{"values": v}
			}
			return c.callCtx(ctx, "POST", "/api/grafana/"+url.PathEscape(in.ID)+"/panel-data", map[string]any{
				"uid": in.UID, "panelId": in.PanelID, "title": in.Title, "from": in.From, "to": in.To,
				"selected": sel, "summary": !in.Raw,
			})
		})
	regCtx(s, "grafana_query",
		"Run an ad-hoc query on a Grafana datasource (PromQL/LogQL via expr, SQL via rawSql, or a full target) and return per-series stats. Read-only; raw SQL must be a read query unless you hold a write grant."+connNote,
		func(ctx context.Context, in grafanaQueryIn) (int, string) {
			target := map[string]any{"refId": "A"}
			for k, v := range in.Target {
				target[k] = v
			}
			if in.Target == nil {
				if in.Expr != "" {
					target["expr"] = in.Expr
				}
				if in.RawSQL != "" {
					target["rawSql"] = in.RawSQL
					target["rawQuery"] = true
					target["editorMode"] = "code"
					target["format"] = "table"
				}
			}
			if in.Instant {
				target["instant"] = true
				target["range"] = false
				target["queryType"] = "instant"
			}
			target["datasource"] = in.Datasource
			from := in.From
			if from == "" {
				from = "now-1h"
			}
			v := url.Values{}
			if !in.Raw {
				v.Set("summary", "1")
			}
			return c.callCtx(ctx, "POST", withQuery("/api/grafana/"+url.PathEscape(in.ID)+"/query", v), map[string]any{
				"from": from, "to": in.To, "targets": []any{target}, "maxDataPoints": 300, "source": "mcp",
			})
		})
}

// compactSearch flattens the tree into "Folder/Sub/Dashboard (uid)" lines.
func compactSearch(body string) string {
	var env struct {
		Data struct {
			Folders []struct {
				UID, Title, FolderUID string
			} `json:"folders"`
			Dashboards []struct {
				UID, Title, FolderUID string
				Tags                  []string
			} `json:"dashboards"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(body), &env) != nil {
		return body
	}
	parent := map[string]string{}
	title := map[string]string{}
	for _, f := range env.Data.Folders {
		parent[f.UID], title[f.UID] = f.FolderUID, f.Title
	}
	path := func(uid string) string {
		var parts []string
		for i := 0; uid != "" && i < 20; i++ {
			parts = append([]string{title[uid]}, parts...)
			uid = parent[uid]
		}
		return strings.Join(parts, "/")
	}
	type row struct {
		UID    string   `json:"uid"`
		Title  string   `json:"title"`
		Folder string   `json:"folder,omitempty"`
		Tags   []string `json:"tags,omitempty"`
	}
	out := make([]row, 0, len(env.Data.Dashboards))
	for _, d := range env.Data.Dashboards {
		out = append(out, row{UID: d.UID, Title: d.Title, Folder: path(d.FolderUID), Tags: d.Tags})
	}
	b, _ := json.Marshal(map[string]any{"dashboards": out})
	return string(b)
}

// compactDashboard keeps what an agent needs to pick a panel and understand
// its queries.
func compactDashboard(body string) string {
	var env struct {
		Data struct {
			Dashboard map[string]any `json:"dashboard"`
			Meta      map[string]any `json:"meta"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(body), &env) != nil {
		return body
	}
	d := env.Data.Dashboard
	type panel struct {
		ID         any              `json:"id"`
		Title      any              `json:"title"`
		Type       any              `json:"type"`
		Row        string           `json:"row,omitempty"`
		Datasource any              `json:"datasource,omitempty"`
		Unit       string           `json:"unit,omitempty"`
		Queries    []map[string]any `json:"queries,omitempty"`
	}
	var panels []panel
	var walk func(ps []any, row string)
	walk = func(ps []any, row string) {
		for _, p := range ps {
			pm, _ := p.(map[string]any)
			if pm == nil {
				continue
			}
			if pm["type"] == "row" {
				t, _ := pm["title"].(string)
				row = t
				if nested, ok := pm["panels"].([]any); ok {
					walk(nested, t)
				}
				continue
			}
			pn := panel{ID: pm["id"], Title: pm["title"], Type: pm["type"], Row: row, Datasource: pm["datasource"]}
			if fc, ok := pm["fieldConfig"].(map[string]any); ok {
				if def, ok := fc["defaults"].(map[string]any); ok {
					pn.Unit, _ = def["unit"].(string)
				}
			}
			if ts, ok := pm["targets"].([]any); ok {
				for _, t := range ts {
					tm, _ := t.(map[string]any)
					q := map[string]any{"refId": tm["refId"]}
					for _, k := range []string{"expr", "rawSql", "query", "legendFormat", "datasource"} {
						if v, ok := tm[k]; ok && v != "" {
							q[k] = v
						}
					}
					pn.Queries = append(pn.Queries, q)
				}
			}
			panels = append(panels, pn)
		}
	}
	ps, _ := d["panels"].([]any)
	walk(ps, "")
	var vars []map[string]any
	if t, ok := d["templating"].(map[string]any); ok {
		if l, ok := t["list"].([]any); ok {
			for _, v := range l {
				vm, _ := v.(map[string]any)
				vars = append(vars, map[string]any{
					"name": vm["name"], "type": vm["type"], "query": vm["query"], "multi": vm["multi"],
					"includeAll": vm["includeAll"], "current": vm["current"],
				})
			}
		}
	}
	b, _ := json.Marshal(map[string]any{
		"title": d["title"], "uid": d["uid"], "time": d["time"], "refresh": d["refresh"],
		"folder": env.Data.Meta["folderTitle"], "variables": vars, "panels": panels,
	})
	return string(b)
}
