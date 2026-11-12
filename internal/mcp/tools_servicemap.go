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
