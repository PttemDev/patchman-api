package apm

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/servicemap"
)

// prometheusProvider reads the service map from any Prometheus-compatible
// API (Prometheus, VictoriaMetrics, Thanos, Mimir) by recognising which
// metric families are there: coroot-node-agent (eBPF), Istio, Linkerd,
// OpenTelemetry / Tempo service graph and Cilium Hubble.
type prometheusProvider struct{ base }

func init() { register(prometheusProvider{}) }

func (prometheusProvider) Name() string        { return "prometheus" }
func (prometheusProvider) Title() string       { return "Prometheus" }
func (prometheusProvider) AuthKinds() []string { return []string{"none", "basic", "bearer", "apikey"} }

func (p prometheusProvider) Test(ctx context.Context, c *Client) (*TestResult, error) {
	var bi struct {
		Data struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	res := &TestResult{OK: true}
	if err := c.Do(ctx, http.MethodGet, "/api/v1/status/buildinfo", nil, nil, &bi); err == nil {
		res.Version = bi.Data.Version
	}
	q := promQuerier{c: c, prefix: "/api/v1"}
	if _, err := q.Query(ctx, "vector(1)", time.Now()); err != nil {
		return nil, err
	}
	fams, err := DetectFamilies(ctx, q, time.Now())
	if err != nil {
		return nil, err
	}
	res.Notes = familyNotes(fams)
	return res, nil
}

func (p prometheusProvider) Map(ctx context.Context, c *Client, req MapRequest, b *servicemap.Builder, st *servicemap.SourceStatus) error {
	return MapFromPrometheus(ctx, promQuerier{c: c, prefix: "/api/v1"}, req, b, st, "prometheus")
}

// Metric families a Prometheus can hold.
const (
	FamCoroot  = "coroot"
	FamIstio   = "istio"
	FamLinkerd = "linkerd"
	FamOTel    = "otel"
	FamHubble  = "hubble"
)

var familyProbe = map[string]string{
	FamCoroot:  `container_http_requests_total|container_net_tcp_active_connections|container_postgres_queries_total`,
	FamIstio:   `istio_requests_total|istio_tcp_connections_opened_total`,
	FamLinkerd: `response_total`,
	FamOTel:    `traces_service_graph_request_total`,
	FamHubble:  `hubble_http_requests_total`,
}

// DetectFamilies reports which families have series right now.
func DetectFamilies(ctx context.Context, q servicemap.PromQuerier, at time.Time) (map[string]bool, error) {
	var names []string
	for _, n := range familyProbe {
		names = append(names, n)
	}
	rows, err := q.Query(ctx, fmt.Sprintf(`count by (__name__) ({__name__=~"%s"})`, strings.Join(names, "|")), at)
	if err != nil {
		return nil, err
	}
	has := map[string]bool{}
	for _, r := range rows {
		n := r.Metric["__name__"]
		for fam, alt := range familyProbe {
			for _, a := range strings.Split(alt, "|") {
				if a == n {
					has[fam] = true
				}
			}
		}
	}
	// response_total is a common name: it is Linkerd's only with dst_* labels.
	if has[FamLinkerd] {
		rows, err := q.Query(ctx, `count(response_total{direction="outbound", dst_namespace!=""})`, at)
		has[FamLinkerd] = err == nil && len(rows) > 0 && rows[0].Value > 0
	}
	return has, nil
}

func familyNotes(f map[string]bool) []string {
	var out []string
	for _, k := range []string{FamCoroot, FamIstio, FamLinkerd, FamOTel, FamHubble} {
		if f[k] {
			out = append(out, "family:"+k)
		}
	}
	if len(out) == 0 {
		out = append(out, "no_service_metrics")
	}
	return out
}

// MapFromPrometheus adds every recognised family's edges to b.
func MapFromPrometheus(ctx context.Context, q servicemap.PromQuerier, req MapRequest, b *servicemap.Builder, st *servicemap.SourceStatus, source string) error {
	at := req.End
	if at.IsZero() {
		at = time.Now()
	}
	fams, err := DetectFamilies(ctx, q, at)
	if err != nil {
		return err
	}
	st.Notes = append(st.Notes, familyNotes(fams)...)
	w := req.Window()
	run := func(fam string, f func() error) {
		if !fams[fam] {
			return
		}
		if err := f(); err != nil {
			st.Notes = append(st.Notes, fmt.Sprintf("family_error:%s:%s", fam, trimErr(err)))
		}
	}
	run(FamCoroot, func() error { return mapCorootAgent(ctx, q, at, w, b, source) })
	run(FamIstio, func() error { return mapIstio(ctx, q, at, w, b, source) })
	run(FamLinkerd, func() error { return mapLinkerd(ctx, q, at, w, b, source) })
	run(FamOTel, func() error { return mapOTel(ctx, q, at, w, b, source) })
	run(FamHubble, func() error { return mapHubble(ctx, q, at, w, b, source) })
	return nil
}

func trimErr(err error) string {
	s := err.Error()
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// mapCorootAgent: coroot-node-agent's per-container L7 + TCP metrics.
func mapCorootAgent(ctx context.Context, q servicemap.PromQuerier, at time.Time, w string, b *servicemap.Builder, source string) error {
	qs := servicemap.MetricQueries(w)
	get := func(k string) []servicemap.Sample {
		rows, _ := q.Query(ctx, qs[k], at) // optional series: a missing family is not an error
		return rows
	}
	requests, err := q.Query(ctx, qs["requests"], at)
	if err != nil {
		return err
	}
	r := servicemap.NewResolver()
	r.AddFromBuilder(b)
	r.AddKubeMetrics(get("podinfo"), get("podips"), get("rsowner"), get("jobowner"), get("fqdn"))
	servicemap.BuildMetrics(b, r, requests, get("latency"), get("conns"), source)
	return nil
}

type edgeKey struct{ from, to, flow, proto string }

type redAcc struct {
	rps, errRPS, p95 float64
	statuses         map[string]float64
}

func flush(b *servicemap.Builder, acc map[edgeKey]*redAcc, source string) {
	for k, a := range acc {
		if a.rps <= 0 && a.p95 <= 0 {
			continue
		}
		st := servicemap.Stats{HasRED: a.rps > 0, RPS: a.rps, P95Ms: a.p95, Statuses: a.statuses}
		if a.rps > 0 {
			st.ErrorRate = a.errRPS / a.rps
		}
		b.Link(k.from, k.to, k.flow, k.proto, source, st, nil)
	}
	servicemap.Rollup(b, source)
}

func accFor(acc map[edgeKey]*redAcc, k edgeKey) *redAcc {
	a := acc[k]
	if a == nil {
		a = &redAcc{}
		acc[k] = a
	}
	return a
}

func finiteF(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// mapIstio: istio_requests_total (server-reported, falling back to the
// client side for destinations without a sidecar) + TCP connections.
func mapIstio(ctx context.Context, q servicemap.PromQuerier, at time.Time, w string, b *servicemap.Builder, source string) error {
	const by = "source_workload, source_workload_namespace, destination_workload, destination_workload_namespace, destination_service_name, destination_service_namespace"
	node := func(m map[string]string, side string) string {
		wl, ns := m[side+"_workload"], m[side+"_workload_namespace"]
		if wl == "" || wl == "unknown" {
			if side == "source" {
				return ""
			}
			svc := m["destination_service_name"]
			if svc == "" || svc == "unknown" || svc == "PassthroughCluster" || svc == "BlackHoleCluster" {
				return ""
			}
			return b.ServiceNode(m["destination_service_namespace"], svc, source)
		}
		return b.ServiceNode(ns, wl, source)
	}
	acc := map[edgeKey]*redAcc{}
	seen := map[string]bool{}
	for _, rep := range []string{"destination", "source"} {
		rows, err := q.Query(ctx, fmt.Sprintf(`sum by (%s, response_code, grpc_response_status, request_protocol) (rate(istio_requests_total{reporter="%s"}[%s]))`, by, rep, w), at)
		if err != nil {
			return err
		}
		repSeen := map[string]bool{}
		for _, s := range rows {
			if !finiteF(s.Value) || s.Value <= 0 {
				continue
			}
			from, to := node(s.Metric, "source"), node(s.Metric, "destination")
			if from == "" || to == "" {
				continue
			}
			pair := from + "|" + to
			if rep == "source" && seen[pair] {
				continue // the server side already reported this pair
			}
			repSeen[pair] = true
			proto := "http"
			if p := strings.ToLower(s.Metric["request_protocol"]); p == "grpc" {
				proto = "grpc"
			}
			a := accFor(acc, edgeKey{from, to, "request", proto})
			a.rps += s.Value
			code := s.Metric["response_code"]
			status := code
			if gs := s.Metric["grpc_response_status"]; proto == "grpc" && gs != "" {
				status = "grpc:" + gs
			}
			a.statuses = servicemap.AddStatus(a.statuses, status, s.Value)
			if strings.HasPrefix(code, "5") || code == "0" || (proto == "grpc" && s.Metric["grpc_response_status"] != "" && s.Metric["grpc_response_status"] != "0") {
				a.errRPS += s.Value
			}
		}
		for k := range repSeen {
			seen[k] = true
		}
	}
	lat, _ := q.Query(ctx, fmt.Sprintf(`histogram_quantile(0.95, sum by (le, %s) (rate(istio_request_duration_milliseconds_bucket{reporter="destination"}[%s])))`, by, w), at)
	for _, s := range lat {
		from, to := node(s.Metric, "source"), node(s.Metric, "destination")
		if from == "" || to == "" || !finiteF(s.Value) {
			continue
		}
		for _, proto := range []string{"http", "grpc"} {
			if a, ok := acc[edgeKey{from, to, "request", proto}]; ok {
				a.p95 = math.Max(a.p95, s.Value)
			}
		}
	}
	tcp, _ := q.Query(ctx, fmt.Sprintf(`sum by (%s) (rate(istio_tcp_connections_opened_total{reporter="source"}[%s]))`, by, w), at)
	for _, s := range tcp {
		from, to := node(s.Metric, "source"), node(s.Metric, "destination")
		if from == "" || to == "" || !finiteF(s.Value) || s.Value <= 0 {
			continue
		}
		b.Link(from, to, "request", "tcp", source, servicemap.Stats{}, nil)
	}
	flush(b, acc, source)
	return nil
}

// mapLinkerd: outbound response_total / response_latency_ms.
func mapLinkerd(ctx context.Context, q servicemap.PromQuerier, at time.Time, w string, b *servicemap.Builder, source string) error {
	const by = "namespace, deployment, statefulset, dst_namespace, dst_deployment, dst_statefulset, dst_service"
	ends := func(m map[string]string) (string, string) {
		src := firstNonEmpty(m["deployment"], m["statefulset"])
		dst := firstNonEmpty(m["dst_deployment"], m["dst_statefulset"], m["dst_service"])
		if src == "" || dst == "" {
			return "", ""
		}
		return b.ServiceNode(m["namespace"], src, source), b.ServiceNode(m["dst_namespace"], dst, source)
	}
	rows, err := q.Query(ctx, fmt.Sprintf(`sum by (%s, classification) (rate(response_total{direction="outbound"}[%s]))`, by, w), at)
	if err != nil {
		return err
	}
	acc := map[edgeKey]*redAcc{}
	for _, s := range rows {
		if !finiteF(s.Value) || s.Value <= 0 {
			continue
		}
		from, to := ends(s.Metric)
		if from == "" {
			continue
		}
		a := accFor(acc, edgeKey{from, to, "request", "http"})
		a.rps += s.Value
		a.statuses = servicemap.AddStatus(a.statuses, s.Metric["classification"], s.Value)
		if s.Metric["classification"] == "failure" {
			a.errRPS += s.Value
		}
	}
	lat, _ := q.Query(ctx, fmt.Sprintf(`histogram_quantile(0.95, sum by (le, %s) (rate(response_latency_ms_bucket{direction="outbound"}[%s])))`, by, w), at)
	for _, s := range lat {
		from, to := ends(s.Metric)
		if a, ok := acc[edgeKey{from, to, "request", "http"}]; ok && finiteF(s.Value) {
			a.p95 = math.Max(a.p95, s.Value)
		}
	}
	flush(b, acc, source)
	return nil
}

// mapOTel: the OpenTelemetry Collector servicegraph connector / Tempo
// metrics-generator (traces_service_graph_*), by service name.
func mapOTel(ctx context.Context, q servicemap.PromQuerier, at time.Time, w string, b *servicemap.Builder, source string) error {
	total, err := q.Query(ctx, fmt.Sprintf(`sum by (client, server, connection_type) (rate(traces_service_graph_request_total[%s]))`, w), at)
	if err != nil {
		return err
	}
	failed, _ := q.Query(ctx, fmt.Sprintf(`sum by (client, server, connection_type) (rate(traces_service_graph_request_failed_total[%s]))`, w), at)
	lat, _ := q.Query(ctx, fmt.Sprintf(`histogram_quantile(0.95, sum by (le, client, server) (rate({__name__=~"traces_service_graph_request_server(_seconds)?_bucket"}[%s])))`, w), at)
	acc := map[edgeKey]*redAcc{}
	key := func(m map[string]string) (edgeKey, bool) {
		cl, sv := m["client"], m["server"]
		if cl == "" || sv == "" || cl == "user" {
			return edgeKey{}, false
		}
		from := b.ServiceNode("", cl, source)
		to := b.ServiceNode("", sv, source)
		proto, flow := "http", "request"
		switch m["connection_type"] {
		case "database":
			proto = "db"
			if n := b.Node(to); n != nil && n.Kind == servicemap.KindService {
				n.Kind, n.Subtype = servicemap.KindDatastore, dbSubtype(sv)
			}
		case "messaging_system":
			proto, flow = "messaging", "publish"
			if n := b.Node(to); n != nil && n.Kind == servicemap.KindService {
				n.Kind, n.Subtype = servicemap.KindMessaging, msgSubtype(sv)
			}
		}
		return edgeKey{from, to, flow, proto}, true
	}
	for _, s := range total {
		if k, ok := key(s.Metric); ok && finiteF(s.Value) && s.Value > 0 {
			accFor(acc, k).rps += s.Value
		}
	}
	for _, s := range failed {
		if k, ok := key(s.Metric); ok && finiteF(s.Value) {
			if a, ok := acc[k]; ok {
				a.errRPS += s.Value
				a.statuses = servicemap.AddStatus(a.statuses, "failed", s.Value)
			}
		}
	}
	for _, s := range lat {
		cl, sv := s.Metric["client"], s.Metric["server"]
		if cl == "" || sv == "" || !finiteF(s.Value) {
			continue
		}
		from, to := b.ServiceNode("", cl, source), b.ServiceNode("", sv, source)
		for k, a := range acc {
			if k.from == from && k.to == to {
				a.p95 = math.Max(a.p95, s.Value*1000)
			}
		}
	}
	flush(b, acc, source)
	return nil
}

// mapHubble: hubble_http_requests_total with source/destination context
// labels (workload names, or ns/pod).
func mapHubble(ctx context.Context, q servicemap.PromQuerier, at time.Time, w string, b *servicemap.Builder, source string) error {
	const by = "source, destination, source_workload, source_namespace, destination_workload, destination_namespace"
	ends := func(m map[string]string) (string, string) {
		side := func(p string) string {
			if wl := m[p+"_workload"]; wl != "" {
				return b.ServiceNode(m[p+"_namespace"], wl, source)
			}
			v := m[p]
			if v == "" {
				return ""
			}
			if ns, name, ok := strings.Cut(v, "/"); ok {
				return b.ServiceNode(ns, name, source)
			}
			return b.ServiceNode(m[p+"_namespace"], v, source)
		}
		return side("source"), side("destination")
	}
	rows, err := q.Query(ctx, fmt.Sprintf(`sum by (%s, status) (rate(hubble_http_requests_total[%s]))`, by, w), at)
	if err != nil {
		return err
	}
	acc := map[edgeKey]*redAcc{}
	for _, s := range rows {
		if !finiteF(s.Value) || s.Value <= 0 {
			continue
		}
		from, to := ends(s.Metric)
		if from == "" || to == "" {
			continue
		}
		a := accFor(acc, edgeKey{from, to, "request", "http"})
		a.rps += s.Value
		a.statuses = servicemap.AddStatus(a.statuses, s.Metric["status"], s.Value)
		if code, _ := strconv.Atoi(s.Metric["status"]); code >= 500 {
			a.errRPS += s.Value
		}
	}
	lat, _ := q.Query(ctx, fmt.Sprintf(`histogram_quantile(0.95, sum by (le, %s) (rate(hubble_http_request_duration_seconds_bucket[%s])))`, by, w), at)
	for _, s := range lat {
		from, to := ends(s.Metric)
		if a, ok := acc[edgeKey{from, to, "request", "http"}]; ok && finiteF(s.Value) {
			a.p95 = math.Max(a.p95, s.Value*1000)
		}
	}
	flush(b, acc, source)
	return nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func dbSubtype(name string) string {
	n := strings.ToLower(name)
	for _, m := range []struct{ needle, sub string }{
		{"postgres", "postgres"}, {"pg", "postgres"}, {"mysql", "mysql"}, {"maria", "mysql"}, {"mongo", "mongo"},
		{"redis", "redis"}, {"elastic", "elasticsearch"}, {"opensearch", "elasticsearch"}, {"mssql", "mssql"},
		{"sqlserver", "mssql"}, {"cassandra", "cassandra"}, {"clickhouse", "clickhouse"}, {"memcache", "memcached"},
	} {
		if strings.Contains(n, m.needle) {
			return m.sub
		}
	}
	return "db"
}

func msgSubtype(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "kafka"):
		return "kafka"
	case strings.Contains(n, "rabbit") || strings.Contains(n, "amqp"):
		return "rabbitmq"
	case strings.Contains(n, "nats"):
		return "nats"
	}
	return "queue"
}
