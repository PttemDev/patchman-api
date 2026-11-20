package apm

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"patchman-be/internal/servicemap"
)

// ServiceDetailer is optional: a provider that can describe one service over
// time (the APM summary page). Providers without it answer "not supported"
// and the UI shows only what the map already knows.
type ServiceDetailer interface {
	ServiceDetail(ctx context.Context, c *Client, req DetailRequest) (*ServiceDetail, error)
}

// DetailRequest names the service the way its backend knows it (the node's
// SourceRef) and the time range.
type DetailRequest struct {
	Ref   servicemap.SourceRef
	Range time.Duration
	End   time.Time
}

// Step is the chart resolution: about 60 points, at least a minute.
func (r DetailRequest) Step() time.Duration {
	s := r.Range / 60
	if s < time.Minute {
		s = time.Minute
	}
	return s.Truncate(time.Minute)
}

// Minutes is the range in whole minutes (at least 5).
func (r DetailRequest) Minutes() int {
	m := int(math.Ceil(r.Range.Minutes()))
	if m < 5 {
		m = 5
	}
	return m
}

// ServiceDetail is one service's page. Every panel is optional: a backend
// fills what it has, and a panel whose query failed leaves its error in
// PanelErrors instead of blanking the page. Nothing is estimated — an
// unknown value is null.
type ServiceDetail struct {
	Provider    string            `json:"provider"`
	Service     string            `json:"service"`
	RangeSec    int64             `json:"rangeSec"`
	StepSec     int64             `json:"stepSec"`
	Summary     *DetailSummary    `json:"summary,omitempty"`
	Charts      []DetailChart     `json:"charts"`
	Endpoints   []DetailEndpoint  `json:"endpoints,omitempty"`
	Errors      []DetailError     `json:"errors,omitempty"`
	Instances   []DetailInstance  `json:"instances,omitempty"`
	Tables      []DetailTable     `json:"tables,omitempty"`
	Checks      []DetailCheck     `json:"checks,omitempty"`
	Alerts      []map[string]any  `json:"alerts,omitempty"`
	PanelErrors map[string]string `json:"panelErrors,omitempty"`
	Notes       []string          `json:"notes,omitempty"`
}

// DetailSummary: the headline numbers over the whole range.
type DetailSummary struct {
	RPS       *float64 `json:"rps"`
	ErrorRate *float64 `json:"errorRate"` // 0..1
	AvgMs     *float64 `json:"avgMs"`
	P95Ms     *float64 `json:"p95Ms"`
	P99Ms     *float64 `json:"p99Ms"`
	Apdex     *float64 `json:"apdex"` // 0..1
}

// Chart units the UI formats: rps (requests/s), ms, ratio (0..1), score
// (0..1, Apdex) and "" (the backend's own numbers, shown as they are).
const (
	UnitRPS   = "rps"
	UnitMs    = "ms"
	UnitRatio = "ratio"
	UnitScore = "score"
)

type DetailChart struct {
	ID     string         `json:"id"` // throughput | latency | errors | apdex | other
	Title  string         `json:"title,omitempty"`
	Group  string         `json:"group,omitempty"` // the backend's section (Coroot report)
	Unit   string         `json:"unit"`
	Series []DetailSeries `json:"series"`
}

type DetailSeries struct {
	Name   string       `json:"name"`
	Points [][2]float64 `json:"points"` // [unix ms, value]; gaps are left out
}

type DetailEndpoint struct {
	Name      string   `json:"name"`
	RPS       *float64 `json:"rps"`
	AvgMs     *float64 `json:"avgMs"`
	P95Ms     *float64 `json:"p95Ms"`
	ErrorRate *float64 `json:"errorRate"`
	TimeShare *float64 `json:"timeShare"` // share of the service's total time, 0..1
}

type DetailError struct {
	Class    string `json:"class"`
	Message  string `json:"message,omitempty"`
	Count    int64  `json:"count"`
	LastSeen int64  `json:"lastSeen,omitempty"` // unix ms
}

type DetailInstance struct {
	Name      string   `json:"name"`
	RPS       *float64 `json:"rps"`
	AvgMs     *float64 `json:"avgMs"`
	ErrorRate *float64 `json:"errorRate"`
	Status    string   `json:"status,omitempty"`
}

type DetailTable struct {
	Title  string     `json:"title"`
	Group  string     `json:"group,omitempty"`
	Header []string   `json:"header"`
	Rows   [][]string `json:"rows"`
}

type DetailCheck struct {
	Group   string `json:"group,omitempty"`
	Title   string `json:"title"`
	Status  string `json:"status"` // ok | warning | critical | unknown
	Message string `json:"message,omitempty"`
}

// ErrDetailUnsupported: the provider has no service detail.
func ErrDetailUnsupported(title string) error {
	return &APIError{http.StatusNotImplemented, "unsupported", title + " has no service detail in PatchMan yet; the map shows its numbers."}
}

// panels runs a detail's independent queries in parallel; each failure is
// recorded under its panel name.
type panels struct {
	mu sync.Mutex
	wg sync.WaitGroup
	d  *ServiceDetail
}

func (p *panels) run(name string, f func() error) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		if err := f(); err != nil {
			msg := err.Error()
			if ae, ok := AsAPIError(err); ok {
				msg = ae.Message
			}
			p.mu.Lock()
			if p.d.PanelErrors == nil {
				p.d.PanelErrors = map[string]string{}
			}
			p.d.PanelErrors[name] = msg
			p.mu.Unlock()
		}
	}()
}

// with runs f holding the detail's lock (panels write shared fields).
func (p *panels) with(f func(d *ServiceDetail)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f(p.d)
}

func (p *panels) wait() { p.wg.Wait() }

// Normalize makes every list a JSON array, never null (a series without
// points, a chart without series), so clients can map over them.
func (d *ServiceDetail) Normalize() {
	if d.Charts == nil {
		d.Charts = []DetailChart{}
	}
	for i := range d.Charts {
		if d.Charts[i].Series == nil {
			d.Charts[i].Series = []DetailSeries{}
		}
		for j := range d.Charts[i].Series {
			if d.Charts[i].Series[j].Points == nil {
				d.Charts[i].Series[j].Points = [][2]float64{}
			}
		}
	}
	for i := range d.Tables {
		if d.Tables[i].Header == nil {
			d.Tables[i].Header = []string{}
		}
		if d.Tables[i].Rows == nil {
			d.Tables[i].Rows = [][]string{}
		}
	}
}

// sortCharts puts the standard charts first, in page order.
func sortCharts(cs []DetailChart) {
	rank := map[string]int{"throughput": 0, "latency": 1, "errors": 2, "apdex": 3}
	sort.SliceStable(cs, func(i, j int) bool {
		ri, ok := rank[cs[i].ID]
		if !ok {
			ri = 9
		}
		rj, ok := rank[cs[j].ID]
		if !ok {
			rj = 9
		}
		return ri < rj
	})
}

func ptr(f float64) *float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return &f
}

// nrqlString quotes s as an NRQL string literal.
func nrqlString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
}
