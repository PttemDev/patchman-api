package apm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Coroot service detail: the application page (GET /api/project/{p}/app/{id})
// — its audit reports' charts, tables and checks, as Coroot draws them.
//
// Format (Coroot source, model/*.go + timeseries): report {name, status,
// widgets, checks}; widget {chart | chart_group | table}; chart {ctx: {from,
// to, step} in ms, title, series: [{name, data: [v|null…]}]}, a series' i-th
// value being at from + i*step.

var _ ServiceDetailer = corootProvider{}

// Reports whose charts / tables the detail shows, in page order.
var corootReports = []string{"SLO", "Instances", "CPU", "Memory", "Net", "Logs", "Postgres", "Redis", "Mysql", "Mongodb", "JVM", ".NET"}

type corootChart struct {
	Ctx struct {
		From int64 `json:"from"`
		Step int64 `json:"step"`
	} `json:"ctx"`
	Title  string `json:"title"`
	Series []struct {
		Name  string     `json:"name"`
		Title string     `json:"title"`
		Data  []*float64 `json:"data"`
	} `json:"series"`
}

type corootAppView struct {
	Data *struct {
		Reports []struct {
			Name    string `json:"name"`
			Status  string `json:"status"`
			Widgets []struct {
				Chart      *corootChart `json:"chart"`
				ChartGroup *struct {
					Title  string         `json:"title"`
					Charts []*corootChart `json:"charts"`
				} `json:"chart_group"`
				Table *struct {
					Title  string   `json:"title"`
					Header []string `json:"header"`
					Rows   []struct {
						Cells []struct {
							Value  string   `json:"value"`
							Values []string `json:"values"`
							Unit   string   `json:"unit"`
						} `json:"cells"`
					} `json:"rows"`
				} `json:"table"`
			} `json:"widgets"`
			Checks []struct {
				Title   string `json:"title"`
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"checks"`
		} `json:"reports"`
	} `json:"data"`
}

func (p corootProvider) ServiceDetail(ctx context.Context, c *Client, req DetailRequest) (*ServiceDetail, error) {
	if c.Conn.AuthKind == "apikey" {
		return nil, &APIError{http.StatusNotImplemented, "unsupported", "Coroot's application page needs a sign-in (token, login or session); an API key only reads metrics."}
	}
	app := strings.TrimSpace(req.Ref.Key)
	if app == "" {
		return nil, &APIError{http.StatusBadRequest, "unsupported", "This service has no Coroot application id."}
	}
	project, err := p.project(ctx, c, req.Ref.Scope)
	if err != nil {
		return nil, err
	}
	q := url.Values{"from": {"now-" + strconv.Itoa(req.Minutes()) + "m"}, "to": {"now"}}
	var v corootAppView
	if err := c.Do(ctx, http.MethodGet, "/api/project/"+url.PathEscape(project)+"/app/"+url.PathEscape(app), q, nil, &v); err != nil {
		return nil, err
	}
	if v.Data == nil {
		return nil, &APIError{http.StatusNotFound, "not_found", "Coroot has no application " + app + " in project " + project}
	}
	return corootDetail(&v, firstNonEmpty(req.Ref.Name, app), req), nil
}

func corootDetail(v *corootAppView, service string, req DetailRequest) *ServiceDetail {
	d := &ServiceDetail{Provider: "coroot", Service: service, RangeSec: int64(req.Range.Seconds()), StepSec: int64(req.Step().Seconds()), Charts: []DetailChart{}}
	byName := map[string]int{}
	for i, r := range v.Data.Reports {
		byName[r.Name] = i
	}
	for _, name := range corootReports {
		i, ok := byName[name]
		if !ok {
			continue
		}
		r := v.Data.Reports[i]
		for _, ch := range r.Checks {
			d.Checks = append(d.Checks, DetailCheck{Group: r.Name, Title: ch.Title, Status: corootHealth(ch.Status), Message: ch.Message})
		}
		for _, w := range r.Widgets {
			switch {
			case w.Chart != nil:
				if c, ok := corootDetailChart(w.Chart, r.Name, ""); ok {
					d.Charts = append(d.Charts, c)
				}
			case w.ChartGroup != nil && len(w.ChartGroup.Charts) > 0:
				// A group is one chart per instance; the first is the overview.
				if c, ok := corootDetailChart(w.ChartGroup.Charts[0], r.Name, w.ChartGroup.Title); ok {
					d.Charts = append(d.Charts, c)
				}
			case w.Table != nil && (name == "SLO" || name == "Instances"):
				t := DetailTable{Title: firstNonEmpty(w.Table.Title, r.Name), Group: r.Name, Header: w.Table.Header}
				for _, row := range w.Table.Rows {
					cells := make([]string, 0, len(row.Cells))
					for _, cell := range row.Cells {
						s := cell.Value
						if s == "" && len(cell.Values) > 0 {
							s = strings.Join(cell.Values, ", ")
						}
						if cell.Unit != "" && s != "" {
							s += " " + cell.Unit
						}
						cells = append(cells, s)
					}
					t.Rows = append(t.Rows, cells)
				}
				if len(t.Rows) > 0 {
					d.Tables = append(d.Tables, t)
				}
			}
			if len(d.Charts) >= 24 {
				break
			}
		}
	}
	return d
}

func corootDetailChart(ch *corootChart, group, title string) (DetailChart, bool) {
	out := DetailChart{ID: "other", Group: group, Title: firstNonEmpty(title, ch.Title)}
	if title != "" && ch.Title != "" && !strings.EqualFold(ch.Title, "overview") {
		out.Title = title + " · " + ch.Title
	}
	step := ch.Ctx.Step
	if step <= 0 {
		step = int64(time.Minute / time.Millisecond)
	}
	for _, s := range ch.Series {
		ds := DetailSeries{Name: firstNonEmpty(s.Title, s.Name)}
		for i, v := range s.Data {
			if v != nil {
				ds.Points = append(ds.Points, [2]float64{float64(ch.Ctx.From + int64(i)*step), *v})
			}
		}
		if len(ds.Points) > 0 {
			out.Series = append(out.Series, ds)
		}
		if len(out.Series) >= 12 {
			break
		}
	}
	return out, len(out.Series) > 0
}

// decodeCorootApp parses an application view (tests).
func decodeCorootApp(raw []byte) (*corootAppView, error) {
	var v corootAppView
	err := json.Unmarshal(raw, &v)
	return &v, err
}
