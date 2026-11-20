package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Service map: who calls whom (services, databases, queues) with request
// rate, error rate and p95 latency, merged from a Kubernetes connection's
// configuration and observability connections (Coroot, Prometheus, Jaeger,
// Kiali, SigNoz, Datadog). Read-only; never returns configuration values.

type serviceMapIn struct {
	K8s           string   `json:"k8s,omitempty" jsonschema:"Kubernetes connection id (see list_k8s_connections); adds workloads and the hosts their configuration points at"`
	Context       string   `json:"context,omitempty" jsonschema:"kubeconfig context (default: the connection's)"`
	Namespaces    []string `json:"namespaces,omitempty" jsonschema:"namespaces to focus on (default: all the connection allows)"`
	ClusterProm   bool     `json:"clusterPrometheus,omitempty" jsonschema:"also read traffic metrics from the cluster's own Prometheus"`
	Observability []string `json:"observability,omitempty" jsonschema:"observability connection ids (see list_observability_connections), optionally id@scope (Coroot project, Datadog env, Kiali namespaces)"`
	Range         string   `json:"range,omitempty" jsonschema:"5m | 15m (default) | 1h | 6h | 24h"`
}

func registerServiceMapTools(s *mcpsdk.Server, c *caller) {
	regCtx(s, "service_map", "Service map: which service calls which service, database or queue, with requests/s, error % and p95 latency per call, plus each node's health. Give a Kubernetes connection and/or observability connections. Read-only."+connNote,
		func(ctx context.Context, in serviceMapIn) (int, string) {
			v := url.Values{}
			if in.K8s != "" {
				v.Set("k8s", in.K8s)
			}
			if in.Context != "" {
				v.Set("context", in.Context)
			}
			if len(in.Namespaces) > 0 {
				v.Set("namespaces", strings.Join(in.Namespaces, ","))
			}
			if in.ClusterProm {
				v.Set("clusterProm", "1")
			}
			if len(in.Observability) > 0 {
				v.Set("apm", strings.Join(in.Observability, ","))
			}
			if in.Range != "" {
				v.Set("range", in.Range)
			}
			code, body := c.callCtx(ctx, "GET", withQuery("/api/service-map", v), nil)
			if code != 200 {
				return code, body
			}
			return code, compactServiceMap(body)
		})
	regCtx(s, "service_detail", "One service's APM page: throughput, error rate, average / p95 / p99 latency and Apdex over a range, its busiest endpoints, error classes, instances, health checks and alert incidents — from New Relic, Coroot or Prometheus (OTel service graph, Istio, Linkerd, Hubble). Read-only. Get the arguments from service_map's detail: hints."+connNote,
		func(ctx context.Context, in serviceDetailIn) (int, string) {
			v := url.Values{}
			for k, val := range map[string]string{"service": in.Service, "namespace": in.Namespace, "key": in.Key, "scope": in.Scope, "range": in.Range, "context": in.Context, "k8s": in.K8s} {
				if val != "" {
					v.Set(k, val)
				}
			}
			path := "/api/service-map/service-detail"
			if in.K8s == "" {
				if in.ConnectionID == "" {
					return 400, `{"error":"give connectionId (an observability connection) or k8s (a Kubernetes connection)"}`
				}
				path = "/api/observability-connections/" + url.PathEscape(in.ConnectionID) + "/service-detail"
			}
			code, body := c.callCtx(ctx, "GET", withQuery(path, v), nil)
			if code != 200 || in.Charts {
				return code, body
			}
			return code, dropDetailCharts(body)
		})
}

// dropDetailCharts keeps the time series out of an agent's context unless
// asked for (a week of charts is tens of KB): each chart becomes its id and
// series names.
func dropDetailCharts(body string) string {
	var resp map[string]map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil || resp["data"] == nil {
		return body
	}
	charts, _ := resp["data"]["charts"].([]any)
	out := make([]string, 0, len(charts))
	for _, ch := range charts {
		m, _ := ch.(map[string]any)
		name := fmt.Sprint(m["id"])
		if t, _ := m["title"].(string); t != "" {
			name = t
		}
		out = append(out, name)
	}
	delete(resp["data"], "charts")
	if len(out) > 0 {
		resp["data"]["chartsOmitted"] = strings.Join(out, ", ") + " (pass charts=true for the series)"
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return body
	}
	return string(raw)
}

type serviceDetailIn struct {
	ConnectionID string `json:"connectionId,omitempty" jsonschema:"observability connection the service came from (New Relic, Coroot, Prometheus); service_map prints it per node as detail: service_detail(…)"`
	K8s          string `json:"k8s,omitempty" jsonschema:"instead of connectionId: a Kubernetes connection id, to read the cluster's own Prometheus"`
	Context      string `json:"context,omitempty" jsonschema:"kubeconfig context with k8s (default: the connection's)"`
	Service      string `json:"service" jsonschema:"the service name as that backend knows it (New Relic appName, OTel service name, workload name)"`
	Namespace    string `json:"namespace,omitempty" jsonschema:"the service's namespace, when the backend has one"`
	Key          string `json:"key,omitempty" jsonschema:"the backend's own id (required for Coroot: the application id)"`
	Scope        string `json:"scope,omitempty" jsonschema:"provider scope (New Relic account id, Coroot project); default: the connection's"`
	Range        string `json:"range,omitempty" jsonschema:"30m | 1h (default) | 6h | 24h | 7d"`
	Charts       bool   `json:"charts,omitempty" jsonschema:"include the time series (large); default: summary, endpoints, errors, instances, checks and alerts only"`
}

type newRelicNRQLIn struct {
	ConnectionID string `json:"connectionId" jsonschema:"a New Relic observability connection id (see list_observability_connections, provider newrelic)"`
	Query        string `json:"query" jsonschema:"one NRQL query, e.g. SELECT count(*) FROM Transaction WHERE appName = 'checkout' FACET name SINCE 1 hour ago. NRQL only reads"`
	AccountID    string `json:"accountId,omitempty" jsonschema:"New Relic account id (default: the connection's)"`
}

type newRelicEntitiesIn struct {
	ConnectionID string `json:"connectionId" jsonschema:"a New Relic observability connection id"`
	Query        string `json:"query,omitempty" jsonschema:"New Relic entity search, e.g. domainType IN ('APM-APPLICATION') or name LIKE 'checkout'; a plain word searches names (default: all APM applications)"`
	Max          int    `json:"max,omitempty" jsonschema:"at most this many entities (default 200)"`
}

type newRelicAlertsIn struct {
	ConnectionID string `json:"connectionId" jsonschema:"a New Relic observability connection id"`
	AccountID    string `json:"accountId,omitempty" jsonschema:"New Relic account id (default: the connection's)"`
	Hours        int    `json:"hours,omitempty" jsonschema:"look back this many hours (default 24)"`
	OpenOnly     bool   `json:"openOnly,omitempty" jsonschema:"only incidents still open"`
}

// registerNewRelicTools: NRQL, entity search and alert incidents on a New
// Relic connection (read-only; the stored user key stays in PatchMan).
func registerNewRelicTools(s *mcpsdk.Server, c *caller) {
	regCtx(s, "newrelic_nrql", "Run one NRQL query on a New Relic connection and get its result rows (Transaction, Span, Log, Metric, SystemSample, NrAiIncident…). Read-only. Add SINCE / LIMIT to keep it small."+connNote,
		func(ctx context.Context, in newRelicNRQLIn) (int, string) {
			return c.callCtx(ctx, "POST", "/api/observability-connections/"+url.PathEscape(in.ConnectionID)+"/newrelic/nrql", map[string]string{"query": in.Query, "accountId": in.AccountID})
		})
	regCtx(s, "newrelic_entities", "Search New Relic entities (APM applications, hosts, browser apps, …) with their guid, type, account, whether they report and their alert severity. Read-only."+connNote,
		func(ctx context.Context, in newRelicEntitiesIn) (int, string) {
			v := url.Values{}
			if in.Query != "" {
				v.Set("q", in.Query)
			}
			if in.Max > 0 {
				v.Set("max", fmt.Sprint(in.Max))
			}
			return c.callCtx(ctx, "GET", withQuery("/api/observability-connections/"+url.PathEscape(in.ConnectionID)+"/newrelic/entities", v), nil)
		})
	regCtx(s, "newrelic_alerts", "New Relic alert incidents of the last hours (title, priority, open / closed, entity, condition, policy), open ones first. Read-only."+connNote,
		func(ctx context.Context, in newRelicAlertsIn) (int, string) {
			v := url.Values{}
			if in.AccountID != "" {
				v.Set("accountId", in.AccountID)
			}
			if in.Hours > 0 {
				v.Set("hours", fmt.Sprint(in.Hours))
			}
			if in.OpenOnly {
				v.Set("open", "1")
			}
			return c.callCtx(ctx, "GET", withQuery("/api/observability-connections/"+url.PathEscape(in.ConnectionID)+"/newrelic/alerts", v), nil)
		})
}

type smNode struct {
	ID, Kind, Subtype, Namespace, Name, Health string
	Stats                                      smStats
	Connection                                 *struct{ Tool, Name string }
}

type smStats struct {
	RPS       float64 `json:"rps"`
	ErrorRate float64 `json:"errorRate"`
	P95Ms     float64 `json:"p95Ms"`
	Conns     float64 `json:"conns"`
	HasRED    bool    `json:"hasRed"`
	RateOnly  bool    `json:"rateOnly"`
}

func compactServiceMap(body string) string {
	var resp struct {
		Data struct {
			Nodes []struct {
				ID         string  `json:"id"`
				Kind       string  `json:"kind"`
				Subtype    string  `json:"subtype"`
				Namespace  string  `json:"namespace"`
				Name       string  `json:"name"`
				Health     string  `json:"health"`
				Stats      smStats `json:"stats"`
				Connection *struct {
					Tool string `json:"tool"`
					Name string `json:"name"`
				} `json:"connection"`
				Refs []struct {
					Conn      string `json:"conn"`
					Scope     string `json:"scope"`
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
					Key       string `json:"key"`
				} `json:"refs"`
			} `json:"nodes"`
			Edges []struct {
				From     string   `json:"from"`
				To       string   `json:"to"`
				Protocol string   `json:"protocol"`
				Flow     string   `json:"flow"`
				Sources  []string `json:"sources"`
				Stats    smStats  `json:"stats"`
			} `json:"edges"`
			Sources map[string]struct {
				OK    bool     `json:"ok"`
				Error string   `json:"error"`
				Notes []string `json:"notes"`
			} `json:"sources"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return body
	}
	d := resp.Data
	label := map[string]string{}
	var b strings.Builder
	fmt.Fprintf(&b, "%d nodes, %d edges\n\nSources:\n", len(d.Nodes), len(d.Edges))
	keys := make([]string, 0, len(d.Sources))
	for k := range d.Sources {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := d.Sources[k]
		state := "ok"
		if !s.OK {
			state = "error: " + s.Error
		}
		fmt.Fprintf(&b, "- %s: %s", k, state)
		if len(s.Notes) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(s.Notes, ", "))
		}
		b.WriteString("\n")
	}
	b.WriteString("\nNodes (kind/subtype, health, inbound rps · err% · p95):\n")
	for _, n := range d.Nodes {
		name := n.Name
		if n.Namespace != "" {
			name = n.Namespace + "/" + n.Name
		}
		label[n.ID] = name
		fmt.Fprintf(&b, "- %s [%s/%s] %s", name, n.Kind, n.Subtype, n.Health)
		if n.Stats.HasRED {
			fmt.Fprintf(&b, " · %s", fmtRED(n.Stats))
		}
		if n.Connection != nil {
			fmt.Fprintf(&b, " · PatchMan %s connection %q", n.Connection.Tool, n.Connection.Name)
		}
		for i, r := range n.Refs {
			if n.Kind != "service" || i > 0 {
				break // one hint per service; datastores / externals have no page
			}
			if strings.HasPrefix(r.Conn, "k8s:") {
				fmt.Fprintf(&b, " · detail: service_detail(k8s=%s, service=%q", strings.TrimPrefix(r.Conn, "k8s:"), r.Name)
			} else {
				fmt.Fprintf(&b, " · detail: service_detail(connectionId=%s, service=%q", r.Conn, r.Name)
			}
			if r.Namespace != "" {
				fmt.Fprintf(&b, ", namespace=%q", r.Namespace)
			}
			if r.Key != "" {
				fmt.Fprintf(&b, ", key=%q", r.Key)
			}
			if r.Scope != "" {
				fmt.Fprintf(&b, ", scope=%q", r.Scope)
			}
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
	b.WriteString("\nCalls (from -> to [protocol, flow] rps · err% · p95; sources):\n")
	for _, e := range d.Edges {
		fmt.Fprintf(&b, "- %s -> %s [%s, %s]", or(label[e.From], e.From), or(label[e.To], e.To), e.Protocol, e.Flow)
		switch {
		case e.Stats.HasRED:
			fmt.Fprintf(&b, " %s", fmtRED(e.Stats))
		case e.Stats.Conns > 0:
			fmt.Fprintf(&b, " %.0f open connections", e.Stats.Conns)
		default:
			b.WriteString(" (declared, no traffic data)")
		}
		fmt.Fprintf(&b, "; %s\n", strings.Join(e.Sources, ","))
	}
	return b.String()
}

func fmtRED(s smStats) string {
	if s.RateOnly {
		return fmt.Sprintf("%.2f rps", s.RPS)
	}
	out := fmt.Sprintf("%.2f rps · %.1f%% err", s.RPS, s.ErrorRate*100)
	if s.P95Ms > 0 {
		out += fmt.Sprintf(" · p95 %.0fms", s.P95Ms)
	}
	return out
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
