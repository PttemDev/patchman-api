package apm

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/servicemap"
)

func nowUTC() time.Time { return time.Now().UTC() }

// ── Jaeger ─────────────────────────────────────────────────────────────
// /api/dependencies gives parent → child call counts over a lookback; no
// errors or latency (edges are RateOnly). Tempo's service graph comes in
// through the Prometheus provider (traces_service_graph_*).
type jaegerProvider struct{ base }

func init() { register(jaegerProvider{}) }

func (jaegerProvider) Name() string        { return "jaeger" }
func (jaegerProvider) Title() string       { return "Jaeger" }
func (jaegerProvider) AuthKinds() []string { return []string{"none", "basic", "bearer"} }

func (jaegerProvider) Test(ctx context.Context, c *Client) (*TestResult, error) {
	var r struct {
		Data []string `json:"data"`
	}
	if err := c.Do(ctx, http.MethodGet, "/api/services", nil, nil, &r); err != nil {
		return nil, err
	}
	return &TestResult{OK: true, Notes: []string{fmt.Sprintf("services:%d", len(r.Data))}}, nil
}

func (jaegerProvider) Map(ctx context.Context, c *Client, req MapRequest, b *servicemap.Builder, st *servicemap.SourceStatus) error {
	end := req.End
	if end.IsZero() {
		end = nowUTC()
	}
	lookback := req.Range
	if lookback < time.Minute {
		lookback = 15 * time.Minute
	}
	var r struct {
		Data []struct {
			Parent    string  `json:"parent"`
			Child     string  `json:"child"`
			CallCount float64 `json:"callCount"`
		} `json:"data"`
	}
	q := url.Values{"endTs": {strconv.FormatInt(end.UnixMilli(), 10)}, "lookback": {strconv.FormatInt(lookback.Milliseconds(), 10)}}
	if err := c.Do(ctx, http.MethodGet, "/api/dependencies", q, nil, &r); err != nil {
		return err
	}
	for _, d := range r.Data {
		if d.Parent == "" || d.Child == "" || d.Parent == d.Child {
			continue
		}
		from, to := b.ServiceNode("", d.Parent, "jaeger"), b.ServiceNode("", d.Child, "jaeger")
		b.Link(from, to, "request", "http", "jaeger", servicemap.Stats{HasRED: true, RateOnly: true, RPS: d.CallCount / lookback.Seconds()}, nil)
	}
	servicemap.Rollup(b, "jaeger")
	st.Notes = append(st.Notes, "rates_only")
	return nil
}

// ── SigNoz ─────────────────────────────────────────────────────────────
// POST /api/v1/dependency_graph: per parent → child call rate, error % and
// latency quantiles in nanoseconds.
type signozProvider struct{ base }

func init() { register(signozProvider{}) }

func (signozProvider) Name() string         { return "signoz" }
func (signozProvider) Title() string        { return "SigNoz" }
func (signozProvider) AuthKinds() []string  { return []string{"apikey", "bearer", "none"} }
func (signozProvider) APIKeyHeader() string { return "SIGNOZ-API-KEY" }

func (signozProvider) Test(ctx context.Context, c *Client) (*TestResult, error) {
	var v map[string]any
	if err := c.Do(ctx, http.MethodGet, "/api/v1/version", nil, nil, &v); err != nil {
		return nil, err
	}
	res := &TestResult{OK: true}
	if s, ok := v["version"].(string); ok {
		res.Version = s
	}
	// The version endpoint is public: prove the key works too.
	if err := c.Do(ctx, http.MethodGet, "/api/v1/services/list", nil, nil, nil); err != nil {
		return nil, err
	}
	return res, nil
}

type signozDep struct {
	Parent    string  `json:"parent"`
	Child     string  `json:"child"`
	CallRate  float64 `json:"callRate"`
	ErrorRate float64 `json:"errorRate"` // percent
	P95       float64 `json:"p95"`       // ns
}

func (signozProvider) Map(ctx context.Context, c *Client, req MapRequest, b *servicemap.Builder, st *servicemap.SourceStatus) error {
	end := req.End
	if end.IsZero() {
		end = nowUTC()
	}
	rng := req.Range
	if rng < time.Minute {
		rng = 15 * time.Minute
	}
	body := map[string]any{
		"start": strconv.FormatInt(end.Add(-rng).UnixNano(), 10),
		"end":   strconv.FormatInt(end.UnixNano(), 10),
		"tags":  []any{},
	}
	raw, err := c.DoRaw(ctx, http.MethodPost, "/api/v1/dependency_graph", nil, body)
	if err != nil {
		return err
	}
	deps, err := decodeList[signozDep](raw)
	if err != nil {
		return &APIError{http.StatusBadGateway, "upstream", "SigNoz's dependency graph answer was not understood"}
	}
	for _, d := range deps {
		if d.Parent == "" || d.Child == "" || d.Parent == d.Child {
			continue
		}
		from, to := b.ServiceNode("", d.Parent, "signoz"), b.ServiceNode("", d.Child, "signoz")
		b.Link(from, to, "request", "http", "signoz", servicemap.Stats{HasRED: true, RPS: d.CallRate, ErrorRate: d.ErrorRate / 100, P95Ms: d.P95 / 1e6}, nil)
	}
	servicemap.Rollup(b, "signoz")
	return nil
}

// ── Kiali ──────────────────────────────────────────────────────────────
// GET /api/namespaces/graph (workload graph): Istio traffic with rates,
// error counts and response time per edge.
type kialiProvider struct{ base }

func init() { register(kialiProvider{}) }

func (kialiProvider) Name() string        { return "kiali" }
func (kialiProvider) Title() string       { return "Kiali" }
func (kialiProvider) AuthKinds() []string { return []string{"none", "bearer", "basic"} }

func (kialiProvider) Test(ctx context.Context, c *Client) (*TestResult, error) {
	var s map[string]any
	if err := c.Do(ctx, http.MethodGet, "/api/status", nil, nil, &s); err != nil {
		return nil, err
	}
	res := &TestResult{OK: true}
	if st, ok := s["status"].(map[string]any); ok {
		if v, ok := st["Kiali version"].(string); ok {
			res.Version = v
		}
	}
	scopes, err := kialiProvider{}.Scopes(ctx, c)
	if err != nil {
		return nil, err
	}
	res.Scopes = scopes
	return res, nil
}

func (kialiProvider) Scopes(ctx context.Context, c *Client) ([]Scope, error) {
	var ns []struct {
		Name string `json:"name"`
	}
	if err := c.Do(ctx, http.MethodGet, "/api/namespaces", nil, nil, &ns); err != nil {
		return nil, err
	}
	out := make([]Scope, 0, len(ns))
	for _, n := range ns {
		out = append(out, Scope{ID: n.Name, Name: n.Name})
	}
	return out, nil
}

type kialiGraph struct {
	Elements struct {
		Nodes []struct {
			Data struct {
				ID        string `json:"id"`
				NodeType  string `json:"nodeType"`
				Namespace string `json:"namespace"`
				Workload  string `json:"workload"`
				App       string `json:"app"`
				Service   string `json:"service"`
				IsOutside bool   `json:"isOutside"`
				IsBox     string `json:"isBox"`
			} `json:"data"`
		} `json:"nodes"`
		Edges []struct {
			Data struct {
				Source       string `json:"source"`
				Target       string `json:"target"`
				ResponseTime string `json:"responseTime"`
				Traffic      struct {
					Protocol string            `json:"protocol"`
					Rates    map[string]string `json:"rates"`
				} `json:"traffic"`
			} `json:"data"`
		} `json:"edges"`
	} `json:"elements"`
}

func (k kialiProvider) Map(ctx context.Context, c *Client, req MapRequest, b *servicemap.Builder, st *servicemap.SourceStatus) error {
	namespaces := req.Namespaces
	if len(namespaces) == 0 {
		for _, n := range strings.Split(firstNonEmpty(req.Scope, c.Conn.Scope), ",") {
			if n = strings.TrimSpace(n); n != "" {
				namespaces = append(namespaces, n)
			}
		}
	}
	if len(namespaces) == 0 {
		scopes, err := k.Scopes(ctx, c)
		if err != nil {
			return err
		}
		for _, s := range scopes {
			if !strings.HasPrefix(s.Name, "kube-") && s.Name != "istio-system" {
				namespaces = append(namespaces, s.Name)
			}
			if len(namespaces) >= 20 {
				st.Notes = append(st.Notes, "namespaces_capped:20")
				break
			}
		}
	}
	dur := req.Range
	if dur < time.Minute {
		dur = 10 * time.Minute
	}
	q := url.Values{
		"namespaces": {strings.Join(namespaces, ",")}, "graphType": {"workload"},
		"duration": {strconv.Itoa(int(dur.Seconds())) + "s"}, "appenders": {"responseTime,deadNode"},
		"responseTime": {"95"}, "rateHttp": {"requests"}, "rateGrpc": {"requests"}, "rateTcp": {"sent"},
		"injectServiceNodes": {"false"},
	}
	var g kialiGraph
	if err := c.Do(ctx, http.MethodGet, "/api/namespaces/graph", q, nil, &g); err != nil {
		return err
	}
	ids := map[string]string{}
	for _, n := range g.Elements.Nodes {
		d := n.Data
		if d.IsBox != "" {
			continue
		}
		switch d.NodeType {
		case "workload":
			ids[d.ID] = b.ServiceNode(d.Namespace, d.Workload, "kiali")
		case "app":
			ids[d.ID] = b.ServiceNode(d.Namespace, d.App, "kiali")
		case "service":
			if d.IsOutside || strings.Contains(d.Service, ".") {
				ids[d.ID] = b.Upsert(&servicemap.Node{ID: "ext:" + strings.ToLower(d.Service), Kind: servicemap.KindExternal, Subtype: "http", Name: d.Service, Address: d.Service}, "kiali").ID
			} else {
				ids[d.ID] = b.ServiceNode(d.Namespace, d.Service, "kiali")
			}
		}
	}
	num := func(m map[string]string, k string) float64 {
		f, _ := strconv.ParseFloat(m[k], 64)
		return f
	}
	for _, e := range g.Elements.Edges {
		d := e.Data
		from, to := ids[d.Source], ids[d.Target]
		if from == "" || to == "" {
			continue
		}
		r := d.Traffic.Rates
		st := servicemap.Stats{}
		proto := d.Traffic.Protocol
		switch proto {
		case "http":
			st = servicemap.Stats{HasRED: true, RPS: num(r, "http")}
			if st.RPS > 0 {
				st.ErrorRate = (num(r, "http5xx") + num(r, "httpNoResponse")) / st.RPS
			}
		case "grpc":
			st = servicemap.Stats{HasRED: true, RPS: num(r, "grpc")}
			if st.RPS > 0 {
				st.ErrorRate = (num(r, "grpcErr") + num(r, "grpcNoResponse")) / st.RPS
			}
		default:
			proto = "tcp"
		}
		st.P95Ms, _ = strconv.ParseFloat(d.ResponseTime, 64)
		b.Link(from, to, "request", proto, "kiali", st, nil)
	}
	servicemap.Rollup(b, "kiali")
	return nil
}

// ── Datadog ────────────────────────────────────────────────────────────
// GET /api/v1/service_dependencies?env=: which service calls which, for one
// env (the connection scope). Datadog's API has no per-edge rates, so edges
// carry no stats.
type datadogProvider struct{ base }

func init() { register(datadogProvider{}) }

func (datadogProvider) Name() string        { return "datadog" }
func (datadogProvider) Title() string       { return "Datadog" }
func (datadogProvider) AuthKinds() []string { return []string{"datadog", "bearer"} }

func (datadogProvider) Test(ctx context.Context, c *Client) (*TestResult, error) {
	// /api/v1/validate checks an API key; a personal / service access token
	// (bearer) is proven by the data call below.
	if c.Conn.AuthKind == "datadog" {
		var v struct {
			Valid bool `json:"valid"`
		}
		if err := c.Do(ctx, http.MethodGet, "/api/v1/validate", nil, nil, &v); err != nil {
			return nil, err
		}
		if !v.Valid {
			return nil, &APIError{http.StatusUnauthorized, "auth", "Datadog rejected the API key."}
		}
	}
	res := &TestResult{OK: true}
	if c.Conn.Scope == "" {
		res.Notes = append(res.Notes, "env_required")
		return res, nil
	}
	// The application key is only checked by data endpoints.
	end := nowUTC()
	q := url.Values{"env": {c.Conn.Scope}, "start": {strconv.FormatInt(end.Add(-time.Hour).Unix(), 10)}, "end": {strconv.FormatInt(end.Unix(), 10)}}
	if err := c.Do(ctx, http.MethodGet, "/api/v1/service_dependencies", q, nil, nil); err != nil {
		return nil, err
	}
	return res, nil
}

func (datadogProvider) Map(ctx context.Context, c *Client, req MapRequest, b *servicemap.Builder, st *servicemap.SourceStatus) error {
	env := firstNonEmpty(req.Scope, c.Conn.Scope)
	if env == "" {
		return &APIError{http.StatusBadRequest, "unsupported", "Set the Datadog env (e.g. prod) on the connection."}
	}
	end := req.End
	if end.IsZero() {
		end = nowUTC()
	}
	rng := req.Range
	if rng < time.Minute {
		rng = time.Hour
	}
	q := url.Values{"env": {env}, "start": {strconv.FormatInt(end.Add(-rng).Unix(), 10)}, "end": {strconv.FormatInt(end.Unix(), 10)}}
	var deps map[string]struct {
		Calls []string `json:"calls"`
	}
	if err := c.Do(ctx, http.MethodGet, "/api/v1/service_dependencies", q, nil, &deps); err != nil {
		return err
	}
	for svc, d := range deps {
		from := b.ServiceNode("", svc, "datadog")
		for _, callee := range d.Calls {
			if callee == svc {
				continue
			}
			b.Link(from, b.ServiceNode("", callee, "datadog"), "request", "http", "datadog", servicemap.Stats{}, nil)
		}
	}
	st.Notes = append(st.Notes, "no_rates")
	return nil
}
