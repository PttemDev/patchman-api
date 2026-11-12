package k8sclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/models"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Historical pod metrics via an in-cluster Prometheus, reached through the
// kube-apiserver SERVICE-PROXY (GET /api/v1/namespaces/<ns>/services/<svc>:<port>/proxy/…)
// — the same path OpenLens uses. No ingress, no port-forward: the BE's existing
// kube API credentials proxy a PromQL /api/v1/query_range straight to Prometheus.
//
// This is the "real history" tier. When no Prometheus is discoverable (or the
// caller's kubeconfig can't reach it), PodMetricsHistory returns
// {Available:false} and the FE falls back to the metrics-server live snapshot
// (PodMetrics). An RBAC/timeout/TLS failure is a genuine error (propagated) so
// the user learns their kubeconfig lacks services/proxy rights, rather than
// silently losing history.

// promTarget is a resolved Prometheus service reachable via the apiserver
// service-proxy. Port is the raw path token (a number like "9090" or a named
// service port like "web").
type promTarget struct {
	Namespace string
	Service   string
	Port      string
}

func (t promTarget) String() string { return t.Namespace + "/" + t.Service + ":" + t.Port }

// promCacheEntry memoizes discovery per (connection, context). Both a hit and a
// miss are cached (found=false is the negative cache) so repeated chart opens
// don't re-probe the candidate list every time.
type promCacheEntry struct {
	target promTarget
	found  bool
	at     time.Time
}

const promCacheTTL = 5 * time.Minute

// promNamespaces are where an in-cluster Prometheus usually lives. Discovery
// LISTS services in each (in order) and name-matches, rather than guessing exact
// service names — Helm release names are per-cluster (e.g.
// "kube-prometheus-stack-gene-prometheus"), so a hardcoded name list misses them.
var promNamespaces = []string{
	"monitoring",
	"prometheus",
	"observability",
	"kube-prometheus-stack",
	"cattle-monitoring-system", // Rancher's managed monitoring
	"default",
}

// promCandidates is a LAST-DITCH blind-probe list for the rare kubeconfig that
// can hit the services/proxy subresource but cannot LIST services (so discovery
// by listing 403s). Only literal, release-name-independent services appear here.
var promCandidates = []promTarget{
	{"monitoring", "prometheus-operated", "9090"},
	{"monitoring", "prometheus-server", "80"},
	{"monitoring", "prometheus", "9090"},
	{"prometheus", "prometheus-server", "80"},
	{"prometheus", "prometheus-operated", "9090"},
	{"observability", "prometheus-server", "80"},
	{"default", "prometheus-server", "80"},
}

// promNameExclude are substrings that disqualify a "*prometheus*" service — the
// operator, exporters, alerting, and sidecars are not the query endpoint.
var promNameExclude = []string{
	"operator", "node-exporter", "kube-state", "alertmanager",
	"pushgateway", "thanos", "blackbox", "adapter", "webhook",
	"exporter", "grafana",
}

// PromSeriesPoint is one sample: T is unix milliseconds (to sit on the same time
// axis the FE already uses for the live snapshot), Val is millicores (CPU) or
// bytes (memory).
type PromSeriesPoint struct {
	T   int64   `json:"t"`
	Val float64 `json:"val"`
}

// PromContainerSeries carries a single container's CPU + memory history.
type PromContainerSeries struct {
	Container string            `json:"container"`
	CPU       []PromSeriesPoint `json:"cpu"`
	Mem       []PromSeriesPoint `json:"mem"`
}

// PodMetricsHistoryResult is the Prometheus history payload. Available=false with
// a Reason mirrors PodMetricsResult so the FE degrades identically. Source is
// always "prometheus" when Available. CPU/Mem are pod totals (sum of containers).
type PodMetricsHistoryResult struct {
	Available  bool                  `json:"available"`
	Reason     string                `json:"reason,omitempty"`
	Source     string                `json:"source,omitempty"`
	Service    string                `json:"service,omitempty"`
	Range      string                `json:"range,omitempty"`
	StepSec    int                   `json:"stepSec,omitempty"`
	CPU        []PromSeriesPoint     `json:"cpu"`
	Mem        []PromSeriesPoint     `json:"mem"`
	Containers []PromContainerSeries `json:"containers"`
}

// rangeSpec maps a UI range key to a lookback window + query_range step. Steps
// are chosen for ~60–96 points; the client never picks the step.
type rangeSpec struct {
	window time.Duration
	step   int // seconds
}

func rangeSpecFor(key string) (rangeSpec, string) {
	switch strings.TrimSpace(key) {
	case "15m":
		return rangeSpec{15 * time.Minute, 15}, "15m"
	case "6h":
		return rangeSpec{6 * time.Hour, 300}, "6h"
	case "24h":
		return rangeSpec{24 * time.Hour, 900}, "24h"
	case "1h", "":
		return rangeSpec{time.Hour, 60}, "1h"
	default:
		return rangeSpec{time.Hour, 60}, "1h"
	}
}

// PodMetricsHistory resolves the cluster's Prometheus (cached), then runs two
// PromQL range queries (per-container CPU rate + memory working set), degrading
// to {Available:false} when Prometheus is absent/unreachable and erroring only
// on RBAC/timeout/TLS. namespace + podName are required (this is per-pod).
func (r *Registry) PodMetricsHistory(ctx context.Context, conn models.K8sConnection, contextName, namespace, podName, rangeKey string) (*PodMetricsHistoryResult, error) {
	if strings.TrimSpace(namespace) == "" || strings.TrimSpace(podName) == "" {
		return nil, fmt.Errorf("namespace and pod are required")
	}
	// Same app-level namespace scoping as PodMetrics (a restricted connection
	// with a disallowed namespace is rejected identically).
	if err := checkNamespace(conn, "pods", namespace); err != nil {
		return nil, err
	}
	p, err := r.resolve(conn, contextName)
	if err != nil {
		return nil, err
	}

	tgt, found, err := r.resolveProm(ctx, conn, contextName, p)
	if err != nil {
		return nil, err // RBAC / explicit-override failure — surface it
	}
	if !found {
		return &PodMetricsHistoryResult{Available: false, Reason: "no Prometheus service found in this cluster"}, nil
	}

	spec, rangeLabel := rangeSpecFor(rangeKey)
	end := time.Now()
	start := end.Add(-spec.window)
	// Rate window: wide enough to span several cAdvisor scrapes at any step.
	rateWin := spec.step * 4
	if rateWin < 120 {
		rateWin = 120
	}

	ns := strings.TrimSpace(namespace)
	pod := strings.TrimSpace(podName)

	// cAdvisor label names drifted across k8s versions: pod/container (≥1.16)
	// vs pod_name/container_name (older). Try modern labels first; if BOTH
	// queries come back empty, retry once with the legacy labels — an empty
	// chart on an old cluster is otherwise indistinguishable from "no data".
	labelSets := []struct{ pod, container string }{
		{"pod", "container"},
		{"pod_name", "container_name"},
	}

	var cpuByC, memByC map[string][]PromSeriesPoint
	for _, ls := range labelSets {
		sel := fmt.Sprintf(`%s="%s",%s="%s",%s!="",%s!="POD"`, ls.pod, promEscape(pod), "namespace", promEscape(ns), ls.container, ls.container)
		cpuQ := fmt.Sprintf(`sum by (%s) (rate(container_cpu_usage_seconds_total{%s}[%ds]))`, ls.container, sel, rateWin)
		memQ := fmt.Sprintf(`sum by (%s) (container_memory_working_set_bytes{%s})`, ls.container, sel)

		cpuRes, err := r.promQueryRange(ctx, p, tgt, cpuQ, start, end, spec.step, ls.container)
		if err != nil {
			return nil, err
		}
		memRes, err := r.promQueryRange(ctx, p, tgt, memQ, start, end, spec.step, ls.container)
		if err != nil {
			return nil, err
		}
		if len(cpuRes) > 0 || len(memRes) > 0 {
			// CPU rate is cores/sec → millicores to match the live snapshot unit.
			for c, pts := range cpuRes {
				for i := range pts {
					pts[i].Val *= 1000
				}
				cpuRes[c] = pts
			}
			cpuByC, memByC = cpuRes, memRes
			break
		}
	}

	res := &PodMetricsHistoryResult{
		Available: true,
		Source:    "prometheus",
		Service:   tgt.String(),
		Range:     rangeLabel,
		StepSec:   spec.step,
	}
	res.Containers = mergeContainerSeries(cpuByC, memByC)
	res.CPU = sumSeries(cpuByC)
	res.Mem = sumSeries(memByC)
	return res, nil
}

// resolveProm returns the Prometheus service for this connection+context, using
// the cache, then an explicit override, then auto-discovery. `found=false` (no
// error) means Prometheus is genuinely absent → the caller degrades silently.
// A non-nil error means a real failure (RBAC on services/proxy, or an explicit
// override that didn't answer) → the caller surfaces it.
func (r *Registry) resolveProm(ctx context.Context, conn models.K8sConnection, contextName string, p *pooled) (promTarget, bool, error) {
	key := conn.ID + "|" + strings.TrimSpace(contextName)
	if conn.ID != "" {
		r.promMu.Lock()
		if e, ok := r.promCache[key]; ok && time.Since(e.at) < promCacheTTL {
			r.promMu.Unlock()
			return e.target, e.found, nil
		}
		r.promMu.Unlock()
	}

	// Explicit override wins and skips discovery; if it can't answer, that's a
	// configuration error worth surfacing (don't cache a hard error).
	if ov := strings.TrimSpace(parseOptions(conn.OptionsJSON).PrometheusService); ov != "" {
		tgt, perr := parsePromService(ov)
		if perr != nil {
			return promTarget{}, false, perr
		}
		if err := r.promProbe(ctx, p, tgt); err != nil {
			return promTarget{}, false, fmt.Errorf("configured Prometheus service %s not reachable: %w", tgt, err)
		}
		r.cacheProm(key, tgt, true)
		return tgt, true, nil
	}

	// Primary discovery: LIST services in the well-known namespaces and
	// name-match (Helm release names are per-cluster, so exact-name guessing
	// misses the real service). A 403 anywhere is remembered — if nothing is
	// found and the only failures were RBAC denials, that's a real error.
	var forbidden error
	for _, ns := range promNamespaces {
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		svcs, err := p.clientset.CoreV1().Services(ns).List(lctx, metav1.ListOptions{})
		cancel()
		if err != nil {
			if apierrors.IsForbidden(err) {
				forbidden = err
			}
			continue // absent namespace / no list rights / transient → next
		}
		for _, tgt := range prometheusServiceCandidates(ns, svcs.Items) {
			perr := r.promProbe(ctx, p, tgt)
			if perr == nil {
				r.cacheProm(key, tgt, true)
				return tgt, true, nil
			}
			if apierrors.IsForbidden(perr) {
				forbidden = perr
			}
		}
	}

	// Last-ditch: blind-probe literal service names. Covers the rare kubeconfig
	// that can hit services/proxy but not list services.
	for _, tgt := range promCandidates {
		perr := r.promProbe(ctx, p, tgt)
		if perr == nil {
			r.cacheProm(key, tgt, true)
			return tgt, true, nil
		}
		if apierrors.IsForbidden(perr) {
			forbidden = perr
		}
	}

	if forbidden != nil {
		return promTarget{}, false, fmt.Errorf("cannot reach Prometheus via the API server proxy — the kubeconfig lacks services/proxy (or services list) permission: %w", forbidden)
	}
	r.cacheProm(key, promTarget{}, false) // negative cache
	return promTarget{}, false, nil
}

// prometheusServiceCandidates picks the likely query endpoints from a namespace's
// services: name contains "prometheus", is not an operator/exporter/sidecar, and
// exposes a usable port. Ordered best-first — a routable ClusterIP on port 9090
// beats a headless service, which beats anything else.
func prometheusServiceCandidates(ns string, svcs []corev1.Service) []promTarget {
	type scored struct {
		tgt   promTarget
		score int
	}
	var out []scored
	for i := range svcs {
		s := &svcs[i]
		name := strings.ToLower(s.Name)
		if !strings.Contains(name, "prometheus") {
			continue
		}
		if excluded := func() bool {
			for _, ex := range promNameExclude {
				if strings.Contains(name, ex) {
					return true
				}
			}
			return false
		}(); excluded {
			continue
		}
		port, prom9090 := pickPromPort(s.Spec.Ports)
		if port == "" {
			continue
		}
		score := 0
		if prom9090 {
			score += 4 // the Prometheus HTTP port
		}
		if ip := strings.TrimSpace(s.Spec.ClusterIP); ip != "" && ip != corev1.ClusterIPNone {
			score += 2 // routable beats headless
		}
		out = append(out, scored{promTarget{Namespace: ns, Service: s.Name, Port: port}, score})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	res := make([]promTarget, len(out))
	for i, sc := range out {
		res[i] = sc.tgt
	}
	return res
}

// pickPromPort chooses a service port to proxy to: prefer 9090 (Prometheus'
// HTTP port), then a web/http-named port, else the first TCP port. Returns the
// numeric port as a string plus whether it was the canonical 9090.
func pickPromPort(ports []corev1.ServicePort) (string, bool) {
	var web, first string
	for _, pt := range ports {
		if pt.Protocol != "" && pt.Protocol != corev1.ProtocolTCP {
			continue
		}
		if pt.Port == 9090 {
			return "9090", true
		}
		n := strings.ToLower(pt.Name)
		if web == "" && (n == "web" || n == "http" || n == "http-web") {
			web = strconv.Itoa(int(pt.Port))
		}
		if first == "" {
			first = strconv.Itoa(int(pt.Port))
		}
	}
	if web != "" {
		return web, false
	}
	return first, false
}

func (r *Registry) cacheProm(key string, tgt promTarget, found bool) {
	if strings.HasPrefix(key, "|") { // conn.ID empty → don't cache
		return
	}
	r.promMu.Lock()
	r.promCache[key] = promCacheEntry{target: tgt, found: found, at: time.Now()}
	r.promMu.Unlock()
}

// promProbe runs a cheap instant query (`vector(1)`) through the service-proxy to
// confirm the target is a live Prometheus. Returns nil on a successful Prometheus
// response; a typed apierror (NotFound/Forbidden/…) or a parse error otherwise.
func (r *Registry) promProbe(ctx context.Context, p *pooled, tgt promTarget) error {
	pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req := p.clientset.CoreV1().RESTClient().Get().
		AbsPath("api", "v1", "namespaces", tgt.Namespace, "services", tgt.Service+":"+tgt.Port, "proxy", "api", "v1", "query").
		Param("query", "vector(1)")
	raw, err := req.DoRaw(pctx)
	if err != nil {
		return err
	}
	var resp promResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("not a Prometheus endpoint")
	}
	if resp.Status != "success" {
		return fmt.Errorf("prometheus probe failed: %s", resp.Error)
	}
	return nil
}

// promQueryRange runs a PromQL range query and returns a per-series map keyed by
// the value of the grouping label (container / container_name).
func (r *Registry) promQueryRange(ctx context.Context, p *pooled, tgt promTarget, query string, start, end time.Time, step int, groupLabel string) (map[string][]PromSeriesPoint, error) {
	qctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req := p.clientset.CoreV1().RESTClient().Get().
		AbsPath("api", "v1", "namespaces", tgt.Namespace, "services", tgt.Service+":"+tgt.Port, "proxy", "api", "v1", "query_range").
		Param("query", query).
		Param("start", strconv.FormatInt(start.Unix(), 10)).
		Param("end", strconv.FormatInt(end.Unix(), 10)).
		Param("step", strconv.Itoa(step))
	raw, err := req.DoRaw(qctx)
	if err != nil {
		return nil, promHTTPErr(err)
	}
	var resp promResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode prometheus response: %w", err)
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("prometheus query error: %s", resp.Error)
	}
	out := make(map[string][]PromSeriesPoint, len(resp.Data.Result))
	for _, series := range resp.Data.Result {
		name := series.Metric[groupLabel]
		if name == "" {
			name = "container"
		}
		pts := make([]PromSeriesPoint, 0, len(series.Values))
		for _, v := range series.Values {
			pt, ok := parsePromValue(v)
			if ok {
				pts = append(pts, pt)
			}
		}
		if len(pts) > 0 {
			out[name] = append(out[name], pts...)
		}
	}
	return out, nil
}

// promResp mirrors just enough of the Prometheus HTTP API (matrix + vector).
type promResp struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string   `json:"metric"`
			Values [][]json.RawMessage `json:"values"` // range: [[ts, "val"], …]
			Value  []json.RawMessage   `json:"value"`  // instant: [ts, "val"]
		} `json:"result"`
	} `json:"data"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
}

// parsePromValue turns a Prometheus [ts, "val"] pair into a PromSeriesPoint
// (T in unix ms). NaN/Inf and unparseable values are dropped.
func parsePromValue(v []json.RawMessage) (PromSeriesPoint, bool) {
	if len(v) != 2 {
		return PromSeriesPoint{}, false
	}
	var tsSec float64
	if err := json.Unmarshal(v[0], &tsSec); err != nil {
		return PromSeriesPoint{}, false
	}
	var valStr string
	if err := json.Unmarshal(v[1], &valStr); err != nil {
		return PromSeriesPoint{}, false
	}
	val, err := strconv.ParseFloat(valStr, 64)
	if err != nil || math.IsNaN(val) || math.IsInf(val, 0) {
		return PromSeriesPoint{}, false
	}
	return PromSeriesPoint{T: int64(tsSec * 1000), Val: val}, true
}

// mergeContainerSeries zips the per-container CPU + memory maps into a sorted,
// stable slice (one entry per container that appears in either map).
func mergeContainerSeries(cpu, mem map[string][]PromSeriesPoint) []PromContainerSeries {
	names := map[string]struct{}{}
	for c := range cpu {
		names[c] = struct{}{}
	}
	for c := range mem {
		names[c] = struct{}{}
	}
	out := make([]PromContainerSeries, 0, len(names))
	for c := range names {
		cp := cpu[c]
		mp := mem[c]
		sort.Slice(cp, func(i, j int) bool { return cp[i].T < cp[j].T })
		sort.Slice(mp, func(i, j int) bool { return mp[i].T < mp[j].T })
		out = append(out, PromContainerSeries{Container: c, CPU: cp, Mem: mp})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Container < out[j].Container })
	return out
}

// sumSeries collapses per-container series into a pod-total series by summing all
// containers' values at each timestamp (query_range aligns every series to the
// same step grid, so timestamps match across containers).
func sumSeries(byC map[string][]PromSeriesPoint) []PromSeriesPoint {
	acc := map[int64]float64{}
	for _, pts := range byC {
		for _, pt := range pts {
			acc[pt.T] += pt.Val
		}
	}
	out := make([]PromSeriesPoint, 0, len(acc))
	for t, v := range acc {
		out = append(out, PromSeriesPoint{T: t, Val: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out
}

// promHTTPErr maps a client-go transport error to something the handler can act
// on. NotFound (service missing) and ServiceUnavailable (Prometheus down) are
// returned as-is so callers can treat them as "absent"; everything else too.
func promHTTPErr(err error) error {
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		code := int(status.Status().Code)
		return fmt.Errorf("prometheus proxy returned HTTP %d: %w", code, err)
	}
	return err
}

// parsePromService parses an OptionsJSON override "namespace/service:port".
func parsePromService(s string) (promTarget, error) {
	s = strings.TrimSpace(s)
	slash := strings.IndexByte(s, '/')
	colon := strings.LastIndexByte(s, ':')
	if slash <= 0 || colon <= slash+1 || colon >= len(s)-1 {
		return promTarget{}, fmt.Errorf(`invalid prometheusService %q — expected "namespace/service:port"`, s)
	}
	return promTarget{
		Namespace: s[:slash],
		Service:   s[slash+1 : colon],
		Port:      s[colon+1:],
	}, nil
}

// promEscape escapes a value for a PromQL double-quoted label matcher.
func promEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}
