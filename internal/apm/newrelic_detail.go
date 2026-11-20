package apm

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// New Relic service detail: NRQL over Transaction / TransactionError events
// of one APM application (appName), like the APM summary page.
//
// Result shapes (doğrulanmadı on a live account): percentile() comes back as
// a number or a one-entry map ({"95": 0.12}); nrVal reads both. Apdex is
// computed from apdexPerfZone counts (plain numbers, and the app's own T)
// instead of apdex(), whose result object we haven't seen.

var _ ServiceDetailer = newRelicProvider{}

const nrSelect = "rate(count(*), 1 second) AS rps, percentage(count(*), WHERE error IS true) AS errPct, average(duration) AS avg, " +
	"percentile(duration, 95) AS p95, percentile(duration, 99) AS p99, " +
	"filter(count(*), WHERE apdexPerfZone = 'S') AS apS, filter(count(*), WHERE apdexPerfZone = 'T') AS apT, " +
	"filter(count(*), WHERE apdexPerfZone IN ('S', 'T', 'F')) AS apN"

func (newRelicProvider) ServiceDetail(ctx context.Context, c *Client, req DetailRequest) (*ServiceDetail, error) {
	app := strings.TrimSpace(req.Ref.Name)
	if app == "" {
		return nil, &APIError{400, "unsupported", "service is required"}
	}
	account, err := NewRelicAccount(c, firstNonEmpty(req.Ref.Scope, c.Conn.Scope))
	if err != nil {
		return nil, err
	}
	d := &ServiceDetail{Provider: "newrelic", Service: app, RangeSec: int64(req.Range.Seconds()), StepSec: int64(req.Step().Seconds()), Charts: []DetailChart{}}
	where := "WHERE appName = " + nrqlString(app)
	since := fmt.Sprintf("SINCE %d minutes ago", req.Minutes())
	nrql := func(q string) ([]map[string]any, error) { return NewRelicNRQL(ctx, c, account, q) }
	p := &panels{d: d}

	p.run("summary", func() error {
		rows, err := nrql(fmt.Sprintf("SELECT %s FROM Transaction %s %s", nrSelect, where, since))
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		r := rows[0]
		s := &DetailSummary{RPS: nrVal(r["rps"]), AvgMs: nrMs(r["avg"]), P95Ms: nrMs(r["p95"]), P99Ms: nrMs(r["p99"]), Apdex: nrApdex(r)}
		if e := nrVal(r["errPct"]); e != nil {
			s.ErrorRate = ptr(*e / 100)
		}
		p.with(func(d *ServiceDetail) { d.Summary = s })
		return nil
	})

	p.run("charts", func() error {
		step := int(req.Step().Minutes())
		rows, err := nrql(fmt.Sprintf("SELECT %s FROM Transaction %s %s TIMESERIES %d minutes", nrSelect, where, since, step))
		if err != nil {
			return err
		}
		var rps, errs, avg, p95, p99, apdex DetailSeries
		rps.Name, errs.Name, avg.Name, p95.Name, p99.Name, apdex.Name = "requests", "errors", "avg", "p95", "p99", "apdex"
		add := func(s *DetailSeries, t float64, v *float64) {
			if v != nil {
				s.Points = append(s.Points, [2]float64{t, *v})
			}
		}
		for _, r := range rows {
			tsec := nrVal(r["beginTimeSeconds"])
			if tsec == nil {
				continue
			}
			t := *tsec * 1000
			add(&rps, t, nrVal(r["rps"]))
			if e := nrVal(r["errPct"]); e != nil {
				add(&errs, t, ptr(*e/100))
			}
			add(&avg, t, nrMs(r["avg"]))
			add(&p95, t, nrMs(r["p95"]))
			add(&p99, t, nrMs(r["p99"]))
			add(&apdex, t, nrApdex(r))
		}
		charts := []DetailChart{
			{ID: "throughput", Unit: UnitRPS, Series: []DetailSeries{rps}},
			{ID: "latency", Unit: UnitMs, Series: []DetailSeries{avg, p95, p99}},
			{ID: "errors", Unit: UnitRatio, Series: []DetailSeries{errs}},
		}
		if len(apdex.Points) > 0 {
			charts = append(charts, DetailChart{ID: "apdex", Unit: UnitScore, Series: []DetailSeries{apdex}})
		}
		p.with(func(d *ServiceDetail) { d.Charts = append(d.Charts, charts...) })
		return nil
	})

	p.run("endpoints", func() error {
		rows, err := nrql(fmt.Sprintf("SELECT rate(count(*), 1 second) AS rps, average(duration) AS avg, percentile(duration, 95) AS p95, "+
			"percentage(count(*), WHERE error IS true) AS errPct, sum(duration) AS total FROM Transaction %s FACET name %s LIMIT 30", where, since))
		if err != nil {
			return err
		}
		var total float64
		for _, r := range rows {
			if v := nrVal(r["total"]); v != nil {
				total += *v
			}
		}
		out := make([]DetailEndpoint, 0, len(rows))
		for _, r := range rows {
			e := DetailEndpoint{Name: fmt.Sprint(firstVal(r, "name", "facet")), RPS: nrVal(r["rps"]), AvgMs: nrMs(r["avg"]), P95Ms: nrMs(r["p95"])}
			if v := nrVal(r["errPct"]); v != nil {
				e.ErrorRate = ptr(*v / 100)
			}
			if v := nrVal(r["total"]); v != nil && total > 0 {
				e.TimeShare = ptr(*v / total)
			}
			out = append(out, e)
		}
		sort.SliceStable(out, func(i, j int) bool { return val(out[i].TimeShare) > val(out[j].TimeShare) })
		p.with(func(d *ServiceDetail) { d.Endpoints = out })
		return nil
	})

	p.run("errors", func() error {
		rows, err := nrql(fmt.Sprintf("SELECT count(*) AS count, latest(error.message) AS message, max(timestamp) AS lastSeen FROM TransactionError %s FACET error.class %s LIMIT 25", where, since))
		if err != nil {
			return err
		}
		out := make([]DetailError, 0, len(rows))
		for _, r := range rows {
			e := DetailError{Class: fmt.Sprint(firstVal(r, "error.class", "facet")), Count: int64(num(r["count"])), LastSeen: int64(num(r["lastSeen"]))}
			if m, ok := r["message"].(string); ok {
				e.Message = m
			}
			out = append(out, e)
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
		p.with(func(d *ServiceDetail) { d.Errors = out })
		return nil
	})

	p.run("instances", func() error {
		rows, err := nrql(fmt.Sprintf("SELECT rate(count(*), 1 second) AS rps, average(duration) AS avg, percentage(count(*), WHERE error IS true) AS errPct FROM Transaction %s FACET host %s LIMIT 50", where, since))
		if err != nil {
			return err
		}
		out := make([]DetailInstance, 0, len(rows))
		for _, r := range rows {
			in := DetailInstance{Name: fmt.Sprint(firstVal(r, "host", "facet")), RPS: nrVal(r["rps"]), AvgMs: nrMs(r["avg"])}
			if v := nrVal(r["errPct"]); v != nil {
				in.ErrorRate = ptr(*v / 100)
			}
			out = append(out, in)
		}
		p.with(func(d *ServiceDetail) { d.Instances = out })
		return nil
	})

	p.run("alerts", func() error {
		hours := time.Duration(math.Max(24, math.Ceil(req.Range.Hours()))) * time.Hour
		list, err := newRelicAlertsWhere(ctx, c, account, hours, false, "WHERE entity.name = "+nrqlString(app))
		if err != nil {
			return err
		}
		p.with(func(d *ServiceDetail) { d.Alerts = list })
		return nil
	})

	p.wait()
	sortCharts(d.Charts)
	return d, nil
}

// nrVal reads an NRQL number: a number, or a one-entry map (percentile).
// nil = no value.
func nrVal(v any) *float64 {
	switch x := v.(type) {
	case float64:
		return ptr(x)
	case map[string]any:
		if len(x) == 1 {
			for _, inner := range x {
				if f, ok := inner.(float64); ok {
					return ptr(f)
				}
			}
		}
		if f, ok := x["score"].(float64); ok {
			return ptr(f)
		}
	}
	return nil
}

// nrMs reads a duration in seconds as milliseconds.
func nrMs(v any) *float64 {
	if f := nrVal(v); f != nil {
		return ptr(*f * 1000)
	}
	return nil
}

// nrApdex: (satisfied + tolerating/2) / all, from apdexPerfZone counts.
func nrApdex(r map[string]any) *float64 {
	n := nrVal(r["apN"])
	if n == nil || *n <= 0 {
		return nil
	}
	return ptr((val(nrVal(r["apS"])) + val(nrVal(r["apT"]))/2) / *n)
}

func val(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}
