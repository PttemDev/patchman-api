package grafanaquery

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"patchman-be/internal/models"
)

// Frame is the JSON data frame Grafana returns from /api/ds/query (the
// fields this package reads; the FE gets the raw JSON).
type Frame struct {
	Schema struct {
		Name   string         `json:"name,omitempty"`
		RefID  string         `json:"refId,omitempty"`
		Meta   map[string]any `json:"meta,omitempty"`
		Fields []FrameField   `json:"fields"`
	} `json:"schema"`
	Data struct {
		Values [][]any `json:"values"`
	} `json:"data"`
}

type FrameField struct {
	Name   string            `json:"name"`
	Type   string            `json:"type,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
	Config map[string]any    `json:"config,omitempty"`
}

// RefResult is one query's answer.
type RefResult struct {
	Frames []json.RawMessage `json:"frames"`
	Error  string            `json:"error,omitempty"`
	// Notice explains a degraded path (e.g. "via datasource proxy").
	Notice string `json:"notice,omitempty"`
}

// QueryRequest is a panel's (or Explore's) query batch.
type QueryRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Targets are the panel targets verbatim (classic model).
	Targets []map[string]any `json:"targets"`
	// PanelDatasource is the panel-level ref used when a target has none.
	PanelDatasource any                 `json:"panelDatasource,omitempty"`
	Vars            map[string]VarValue `json:"vars,omitempty"`
	MaxDataPoints   int                 `json:"maxDataPoints,omitempty"`
	// MinInterval is the panel's "Min interval" (e.g. ">1m" or "30s").
	MinInterval string `json:"minInterval,omitempty"`
	// Source is "explore" / "mcp" for ad-hoc queries (audited); dashboard
	// panel queries leave it empty — auto-refresh would flood the audit log.
	Source string `json:"source,omitempty"`
}

// QueryResult is keyed by refId.
type QueryResult struct {
	Results    map[string]*RefResult `json:"results"`
	IntervalMs int64                 `json:"intervalMs"`
	From       int64                 `json:"from"`
	To         int64                 `json:"to"`
}

// SQLGuard vets interpolated raw SQL before it is sent (read-only grants).
type SQLGuard func(sql string) error

type plannedTarget struct {
	refID  string
	ds     *Datasource
	target map[string]any
}

// Query interpolates and runs a panel's targets. Targets are grouped per
// datasource (one /api/ds/query per datasource — old versions reject mixed
// batches); a datasource /api/ds/query can't run (frontend-only plugin,
// pre-backend Prometheus/Loki) falls back to its native HTTP API through the
// datasource proxy.
func (r *Registry) Query(ctx context.Context, conn models.GrafanaConnection, req QueryRequest, guard SQLGuard) (*QueryResult, error) {
	info, err := r.Info(ctx, conn)
	if err != nil {
		return nil, err
	}
	rng, err := ParseRange(req.From, req.To, time.Now())
	if err != nil {
		return nil, err
	}
	out := &QueryResult{Results: map[string]*RefResult{}, From: rng.FromMs(), To: rng.ToMs()}
	baseIP := &Interpolator{Vars: req.Vars, Range: rng}
	panelDS := dsRef(req.PanelDatasource, baseIP)

	maxDP := req.MaxDataPoints
	if maxDP <= 0 {
		maxDP = 1000
	}
	if maxDP > 11000 {
		maxDP = 11000
	}

	groups := map[string][]plannedTarget{}
	var order []string
	var minIntervalAll time.Duration
	if d, err := ParseDuration(baseIP.Replace(req.MinInterval)); err == nil {
		minIntervalAll = d
	}
	for i, t := range req.Targets {
		if truthy(t["hide"]) {
			continue
		}
		refID := str(t["refId"])
		if refID == "" {
			refID = string(rune('A' + i%26))
		}
		ref := dsRef(t["datasource"], baseIP)
		if ref == "-- Dashboard --" || (ref == "" && panelDS == "-- Dashboard --") {
			out.Results[refID] = &RefResult{Frames: []json.RawMessage{}, Error: "Panels that reuse another panel's query (-- Dashboard --) are not supported yet — open it in Grafana."}
			continue
		}
		ds := resolveTargetDS(info, ref, dsType(t["datasource"]), panelDS, dsType(req.PanelDatasource))
		if ds == nil {
			if ref == "" {
				ref = panelDS
			}
			out.Results[refID] = &RefResult{Frames: []json.RawMessage{}, Error: fmt.Sprintf("Datasource %q not found (or not visible to this Grafana user).", ref)}
			continue
		}
		if emptyTarget(t, ds.Type) {
			continue // Grafana's filterQuery drops these too (e.g. a blank PromQL row)
		}
		ip := &Interpolator{Vars: req.Vars, Range: rng, DefaultFormat: DefaultFormatFor(ds.Type)}
		target := ip.InterpolateTarget(t)
		target["refId"] = refID
		target["datasource"] = map[string]any{"uid": ds.UID, "type": ds.Type}
		if ds.ID > 0 {
			target["datasourceId"] = ds.ID // Grafana 8 resolves by id
		}
		if guard != nil {
			for _, k := range []string{"rawSql", "rawQuery"} {
				if s, ok := target[k].(string); ok && strings.TrimSpace(s) != "" {
					if gerr := guard(s); gerr != nil {
						out.Results[refID] = &RefResult{Frames: []json.RawMessage{}, Error: gerr.Error()}
						target = nil
						break
					}
				}
			}
			if target == nil {
				continue
			}
		}
		if _, ok := groups[ds.UID]; !ok {
			order = append(order, ds.UID)
		}
		groups[ds.UID] = append(groups[ds.UID], plannedTarget{refID: refID, ds: ds, target: target})
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, uid := range order {
		pts := groups[uid]
		ds := pts[0].ds
		minIv := minIntervalAll
		if d, err := ParseDuration(ds.TimeInterval); err == nil && d > minIv {
			minIv = d
		}
		iv := CalcInterval(rng, maxDP, minIv)
		mu.Lock()
		if out.IntervalMs == 0 || iv.Milliseconds() < out.IntervalMs {
			out.IntervalMs = iv.Milliseconds()
		}
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			targets := make([]map[string]any, len(pts))
			for i, p := range pts {
				p.target["intervalMs"] = iv.Milliseconds()
				p.target["maxDataPoints"] = maxDP
				targets[i] = p.target
			}
			res := r.runWithFallback(ctx, conn, info, ds, targets, rng, iv, req.Vars)
			mu.Lock()
			for k, v := range res {
				out.Results[k] = v
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out, nil
}

// resolveTargetDS picks the datasource a target runs on, the way Grafana's
// query runner does: the target's own ref when it resolves; otherwise the
// panel datasource (unless the panel is Mixed); otherwise a datasource of the
// referenced type (dashboards imported from another Grafana — e.g. Percona
// PMM's {"type":"prometheus","uid":"prometheus"} under a panel on "Metrics").
func resolveTargetDS(info *ServerInfo, ref, refType, panelRef, panelType string) *Datasource {
	mixed := panelRef == "-- Mixed --" || panelRef == "mixed"
	if ref == "" && refType == "" && !mixed {
		refType = panelType // a ref-less target inherits the panel's {"type":…}
	}
	if ref != "" && ref != "-- Mixed --" {
		if ds := info.FindDatasource(ref); ds != nil {
			return ds
		}
	}
	if mixed && ref == "" {
		if ds := info.FindByType(refType); ds != nil {
			return ds
		}
		return info.FindDatasource("") // Mixed rows without a ref use the default
	}
	if !mixed && (panelRef != "" || ref == "") {
		if ds := info.FindDatasource(panelRef); ds != nil && (refType == "" || panelRef != "" || sameType(ds.Type, refType)) {
			return ds
		}
	}
	if refType == "" {
		refType = panelType
	}
	return info.FindByType(refType)
}

func sameType(a, b string) bool { return strings.EqualFold(a, b) }

// emptyTarget reports a target with no query text (Grafana skips those
// instead of sending them; VictoriaMetrics answers 422 "missing query").
func emptyTarget(t map[string]any, dsType string) bool {
	kind := strings.ToLower(dsType)
	switch {
	case IsPromLike(kind) || strings.Contains(kind, "loki"):
		return strings.TrimSpace(str(t["expr"])) == ""
	case strings.Contains(kind, "sql") || strings.Contains(kind, "postgres") || strings.Contains(kind, "clickhouse"):
		raw, has := t["rawSql"].(string)
		if !has || strings.TrimSpace(raw) != "" {
			return false
		}
		_, builder := t["sql"].(map[string]any)       // core SQL visual builder
		_, ch := t["builderOptions"].(map[string]any) // ClickHouse builder
		return !builder && !ch
	}
	return false
}

func (r *Registry) runWithFallback(ctx context.Context, conn models.GrafanaConnection, info *ServerInfo, ds *Datasource, targets []map[string]any, rng TimeRange, iv time.Duration, vars map[string]VarValue) map[string]*RefResult {
	t := strings.ToLower(ds.Type)
	nativeOK := t == "prometheus" || strings.Contains(t, "loki")
	if ds.Backend || !nativeOK {
		res, err := r.runDSQuery(ctx, conn, ds, targets, rng)
		if err == nil {
			return res
		}
		if !nativeOK || !fallbackWorthy(err) {
			out := map[string]*RefResult{}
			for _, tg := range targets {
				out[str(tg["refId"])] = &RefResult{Frames: []json.RawMessage{}, Error: err.Error()}
			}
			return out
		}
	}
	out := map[string]*RefResult{}
	for _, tg := range targets {
		refID := str(tg["refId"])
		frames, err := r.nativeQuery(ctx, conn, ds, tg, rng, iv)
		if err != nil {
			out[refID] = &RefResult{Frames: []json.RawMessage{}, Error: err.Error()}
			continue
		}
		out[refID] = &RefResult{Frames: frames, Notice: "via datasource proxy"}
	}
	return out
}

func fallbackWorthy(err error) bool {
	ae, ok := AsAPIError(err)
	if !ok {
		return false
	}
	if ae.Code == "auth" || ae.Code == "session_expired" {
		return false
	}
	return ae.Status == http.StatusBadRequest || ae.Status == http.StatusNotFound ||
		ae.Status == http.StatusInternalServerError || ae.Status == http.StatusNotImplemented
}

// runDSQuery posts one batch to /api/ds/query.
func (r *Registry) runDSQuery(ctx context.Context, conn models.GrafanaConnection, ds *Datasource, targets []map[string]any, rng TimeRange) (map[string]*RefResult, error) {
	for _, tg := range targets {
		if _, ok := tg["datasource"]; !ok {
			tg["datasource"] = map[string]any{"uid": ds.UID, "type": ds.Type}
			if ds.ID > 0 {
				tg["datasourceId"] = ds.ID
			}
		}
	}
	body := map[string]any{
		"from":    strconv.FormatInt(rng.FromMs(), 10),
		"to":      strconv.FormatInt(rng.ToMs(), 10),
		"queries": targets,
	}
	var res struct {
		Results map[string]struct {
			Frames     []json.RawMessage `json:"frames"`
			Error      string            `json:"error"`
			Dataframes []string          `json:"dataframes"`
		} `json:"results"`
	}
	resp, err := r.Do(ctx, conn, http.MethodPost, "/api/ds/query", nil, mustJSON(body))
	var raw []byte
	if err != nil {
		// Grafana answers 400/500 when every query failed, with the per-ref
		// errors in the body — those are what the user needs to see.
		ae, ok := AsAPIError(err)
		if !ok || len(ae.body) == 0 || !strings.Contains(string(ae.body), `"results"`) {
			return nil, err
		}
		raw = ae.body
	} else {
		raw = resp.Body
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("decode query response: %w", err)
	}
	out := map[string]*RefResult{}
	for ref, rr := range res.Results {
		frames := rr.Frames
		if frames == nil {
			frames = []json.RawMessage{}
		}
		e := rr.Error
		if len(rr.Frames) == 0 && len(rr.Dataframes) > 0 && e == "" {
			e = "This Grafana version returns Arrow-encoded frames, which PatchMan cannot read — Grafana 8.0+ JSON frames are required."
		}
		out[ref] = &RefResult{Frames: frames, Error: e}
	}
	for _, tg := range targets {
		ref := str(tg["refId"])
		if _, ok := out[ref]; !ok {
			out[ref] = &RefResult{Frames: []json.RawMessage{}}
		}
	}
	return out, nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return b
}

// ── native fallback (Prometheus / Loki HTTP API via the datasource proxy) ──

func (r *Registry) nativeQuery(ctx context.Context, conn models.GrafanaConnection, ds *Datasource, tg map[string]any, rng TimeRange, iv time.Duration) ([]json.RawMessage, error) {
	expr := str(tg["expr"])
	if expr == "" {
		return []json.RawMessage{}, nil
	}
	// No Grafana backend expands the $__ macros on this path.
	scrape := 15 * time.Second
	if d, err := ParseDuration(ds.TimeInterval); err == nil && d > 0 {
		scrape = d
	}
	rate := iv + scrape
	if 4*scrape > rate {
		rate = 4 * scrape
	}
	rangeDur := rng.To.Sub(rng.From)
	scoped := map[string]string{
		"__interval": FormatDuration(iv), "__interval_ms": strconv.FormatInt(iv.Milliseconds(), 10),
		"__rate_interval": FormatDuration(rate), "__range": FormatDuration(rangeDur.Round(time.Second)),
		"__range_s": strconv.FormatInt(int64(rangeDur.Seconds()), 10), "__range_ms": strconv.FormatInt(rangeDur.Milliseconds(), 10),
		"__auto": FormatDuration(iv),
	}
	expr = (&Interpolator{ScopedVars: scoped, Range: rng}).Replace(expr)
	legend := str(tg["legendFormat"])
	refID := str(tg["refId"])
	isLoki := strings.Contains(strings.ToLower(ds.Type), "loki")
	instant := truthy(tg["instant"]) || str(tg["queryType"]) == "instant"

	q := url.Values{"query": {expr}}
	var path string
	switch {
	case isLoki && instant:
		path = "loki/api/v1/query"
		q.Set("time", strconv.FormatInt(rng.To.UnixNano(), 10))
	case isLoki:
		path = "loki/api/v1/query_range"
		q.Set("start", strconv.FormatInt(rng.From.UnixNano(), 10))
		q.Set("end", strconv.FormatInt(rng.To.UnixNano(), 10))
		q.Set("limit", "1000")
		q.Set("step", FormatDuration(iv))
	case instant:
		path = "api/v1/query"
		q.Set("time", strconv.FormatInt(rng.To.Unix(), 10))
	default:
		path = "api/v1/query_range"
		q.Set("start", strconv.FormatInt(rng.From.Unix(), 10))
		q.Set("end", strconv.FormatInt(rng.To.Unix(), 10))
		step := iv
		if step < time.Second {
			step = time.Second
		}
		q.Set("step", strconv.FormatFloat(step.Seconds(), 'f', -1, 64))
	}
	body, err := r.DSResource(ctx, conn, ds, path, q)
	if err != nil {
		return nil, err
	}
	return promToFrames(body, refID, legend, iv)
}

var legendRE = regexp.MustCompile(`\{\{\s*(.+?)\s*\}\}`)

func renderLegend(legend string, labels map[string]string) string {
	if legend == "" || legend == "__auto" {
		return formatSeries(labels)
	}
	return legendRE.ReplaceAllStringFunc(legend, func(m string) string {
		k := strings.TrimSpace(legendRE.FindStringSubmatch(m)[1])
		return labels[k]
	})
}

// promToFrames converts a Prometheus/Loki API answer to JSON data frames in
// the shape /api/ds/query would have produced.
func promToFrames(body []byte, refID, legend string, iv time.Duration) ([]json.RawMessage, error) {
	var res struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string            `json:"resultType"`
			Result     []json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, err
	}
	if res.Status == "error" {
		return nil, fmt.Errorf("%s", res.Error)
	}
	frames := []json.RawMessage{}
	timeField := map[string]any{"name": "Time", "type": "time", "config": map[string]any{"interval": iv.Milliseconds()}}
	for _, raw := range res.Data.Result {
		var s struct {
			Metric map[string]string `json:"metric"`
			Stream map[string]string `json:"stream"`
			Values [][]any           `json:"values"`
			Value  []any             `json:"value"`
		}
		if json.Unmarshal(raw, &s) != nil {
			continue
		}
		if res.Data.ResultType == "streams" {
			times, lines, labels := []any{}, []any{}, []any{}
			for _, v := range s.Values {
				if len(v) != 2 {
					continue
				}
				ns, _ := strconv.ParseInt(str(v[0]), 10, 64)
				times = append(times, ns/1e6)
				lines = append(lines, str(v[1]))
				labels = append(labels, s.Stream)
			}
			f := map[string]any{
				"schema": map[string]any{"refId": refID, "meta": map[string]any{"custom": map[string]any{"frameType": "LabeledTimeValues"}},
					"fields": []any{
						map[string]any{"name": "labels", "type": "other"},
						map[string]any{"name": "Time", "type": "time"},
						map[string]any{"name": "Line", "type": "string"},
					}},
				"data": map[string]any{"values": []any{labels, times, lines}},
			}
			frames = append(frames, mustJSON(f))
			continue
		}
		pts := s.Values
		if len(s.Value) == 2 {
			pts = [][]any{s.Value}
		}
		times, vals := make([]any, 0, len(pts)), make([]any, 0, len(pts))
		for _, p := range pts {
			if len(p) != 2 {
				continue
			}
			ts, _ := p[0].(float64)
			times = append(times, int64(ts*1000))
			if f, err := strconv.ParseFloat(str(p[1]), 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
				vals = append(vals, f)
			} else {
				vals = append(vals, nil)
			}
		}
		labels := s.Metric
		f := map[string]any{
			"schema": map[string]any{"refId": refID, "fields": []any{
				timeField,
				map[string]any{"name": "Value", "type": "number", "labels": labels,
					"config": map[string]any{"displayNameFromDS": renderLegend(legend, labels)}},
			}},
			"data": map[string]any{"values": []any{times, vals}},
		}
		frames = append(frames, mustJSON(f))
	}
	sort.SliceStable(frames, func(i, j int) bool { return string(frames[i]) < string(frames[j]) })
	return frames, nil
}
