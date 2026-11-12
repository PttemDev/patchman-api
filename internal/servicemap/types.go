// Package servicemap builds the directed service map PatchMan draws: which
// workload talks to which service, database or queue, and how much. Sources
// are merged into one graph:
//
//   - config:  hosts found in a workload's env / ConfigMaps / (opt-in)
//     Secrets plus Ingress → Service → workload. Declared, not observed.
//   - coroot:  Coroot's eBPF-observed application map and per-link metrics.
//
// Invariant: nothing here ever carries a raw env value, ConfigMap body or
// Secret value — evidence holds key names and the extracted scheme://host:port.
package servicemap

import (
	"sort"
	"strings"
)

// Node kinds.
const (
	KindService   = "service"
	KindDatastore = "datastore"
	KindMessaging = "messaging"
	KindExternal  = "external"
	KindIngress   = "ingress"
)

type Stats struct {
	RPS       float64 `json:"rps"`
	ErrorRate float64 `json:"errorRate"`        // 0..1
	P95Ms     float64 `json:"p95Ms,omitempty"`  // milliseconds
	Conns     float64 `json:"conns,omitempty"`  // active TCP connections
	HasRED    bool    `json:"hasRed,omitempty"` // rps/error/latency are observed
	// RateOnly: the backend reports call rates but no errors or latency
	// (Jaeger dependencies), so ErrorRate / P95Ms are unknown, not zero.
	RateOnly bool `json:"rateOnly,omitempty"`
	// Statuses: requests/s per response status as the backend labels it
	// ("2xx", "5xx", "failed", "unknown", "grpc:UNAVAILABLE", …), so the UI
	// can say what the errors are.
	Statuses map[string]float64 `json:"statuses,omitempty"`
}

// AddStatus adds rps to one status of a breakdown (empty status = skipped).
func AddStatus(m map[string]float64, status string, rps float64) map[string]float64 {
	if status == "" || rps <= 0 {
		return m
	}
	if m == nil {
		m = map[string]float64{}
	}
	m[status] += rps
	return m
}

func mergeStatuses(a, b map[string]float64) map[string]float64 {
	if len(b) == 0 {
		return a
	}
	out := make(map[string]float64, len(a)+len(b))
	for k, v := range a {
		out[k] += v
	}
	for k, v := range b {
		out[k] += v
	}
	return out
}

type ConnRef struct {
	Tool string `json:"tool"` // db | kafka | rabbit | redis | es | s3
	ID   string `json:"id"`
	Name string `json:"name"`
}

type K8sRef struct {
	Kind      string `json:"kind"` // Deployment | StatefulSet | DaemonSet | CronJob | Service | Ingress
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type CorootRef struct {
	Project string `json:"project"`
	AppID   string `json:"appId"`
}

type Node struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Subtype   string   `json:"subtype,omitempty"` // http | postgres | mysql | redis | kafka | rabbitmq | ...
	Namespace string   `json:"namespace,omitempty"`
	Name      string   `json:"name"`
	Address   string   `json:"address,omitempty"`  // host:port for external / datastore nodes
	Replicas  string   `json:"replicas,omitempty"` // "ready/desired"
	Images    []string `json:"images,omitempty"`
	Services  []string `json:"services,omitempty"`
	Health    string   `json:"health"` // ok | warning | critical | unknown
	// Category from the backend (Coroot: application | monitoring |
	// control-plane) so the map can hide system components.
	Category string     `json:"category,omitempty"`
	Stats    Stats      `json:"stats"`
	Conn     *ConnRef   `json:"connection,omitempty"`
	K8s      *K8sRef    `json:"k8s,omitempty"`
	Coroot   *CorootRef `json:"coroot,omitempty"`
	Sources  []string   `json:"sources"`
}

// Evidence explains an edge. Never a value: names + the extracted address.
type Evidence struct {
	Source    string `json:"source"` // env | configMap | secret | ingress | coroot
	Workload  string `json:"workload,omitempty"`
	Name      string `json:"name,omitempty"` // env var / ConfigMap / Secret name
	Key       string `json:"key,omitempty"`
	Extracted string `json:"extracted,omitempty"` // scheme://host:port
}

type Edge struct {
	ID       string   `json:"id"`
	From     string   `json:"from"`
	To       string   `json:"to"`
	Sources  []string `json:"sources"`            // config | coroot | ingress
	Protocol string   `json:"protocol,omitempty"` // http | postgres | redis | amqp | kafka | tcp ...
	Flow     string   `json:"flow"`               // request | publish | consume
	// Issue is a backend's verdict on the link itself, e.g. Coroot's
	// "failed connections".
	Issue    string     `json:"issue,omitempty"`
	Stats    Stats      `json:"stats"`
	Evidence []Evidence `json:"evidence,omitempty"`
}

type SourceStatus struct {
	OK    bool     `json:"ok"`
	Error string   `json:"error,omitempty"`
	Notes []string `json:"notes,omitempty"`
}

type Graph struct {
	Nodes   []*Node                  `json:"nodes"`
	Edges   []*Edge                  `json:"edges"`
	Sources map[string]*SourceStatus `json:"sources"`
}

// Builder merges nodes and edges from several sources by id.
type Builder struct {
	nodes map[string]*Node
	edges map[string]*Edge
	alias map[string]string // alternate id → canonical node id
	named map[string]string // ServiceNode cache: "ns/name" → id

	// Filled by the config layer for the metric resolvers.
	pods   map[string]string   // "ns/pod" → workload id
	podIPs map[string]string   // pod IP → workload id
	svcIPs map[string][]string // ClusterIP → workload ids
	svcs   map[string][]string // "ns/svc" → node ids
}

func NewBuilder() *Builder {
	return &Builder{nodes: map[string]*Node{}, edges: map[string]*Edge{}, alias: map[string]string{}, named: map[string]string{},
		pods: map[string]string{}, podIPs: map[string]string{}, svcIPs: map[string][]string{}, svcs: map[string][]string{}}
}

// ServiceNode finds the node an APM service name refers to — a workload of
// that name, or the workload behind a K8s Service of that name — in ns
// (any namespace when ns is ""), creating an "apm:" service node when none
// matches.
func (b *Builder) ServiceNode(ns, name, source string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	key := ns + "/" + name
	if id, ok := b.named[key]; ok {
		return id
	}
	var found string
	ids := make([]string, 0, len(b.nodes))
	for id := range b.nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for pass := 0; pass < 2 && found == ""; pass++ {
		for _, id := range ids {
			n := b.nodes[id]
			if n.K8s == nil || n.Kind == KindIngress || (ns != "" && n.Namespace != ns) {
				continue
			}
			if pass == 0 && n.K8s.Name == name && n.K8s.Kind != "Service" {
				found = id
				break
			}
			if pass == 1 {
				for _, s := range n.Services {
					if s == name {
						found = id
						break
					}
				}
				if found != "" {
					break
				}
			}
		}
	}
	if found == "" {
		found = "apm:" + key
		if ns == "" {
			found = "apm:" + name
		}
		b.Upsert(&Node{ID: found, Kind: KindService, Subtype: "http", Namespace: ns, Name: name}, source)
	}
	b.named[key] = found
	return found
}

// WorkloadID is the canonical id of a K8s workload (also Coroot's app id).
func WorkloadID(ns, kind, name string) string { return ns + "/" + kind + "/" + name }

func (b *Builder) resolve(id string) string {
	for i := 0; i < 4; i++ {
		a, ok := b.alias[id]
		if !ok {
			break
		}
		id = a
	}
	return id
}

// Alias makes alt resolve to id (e.g. a datastore address → its workload).
func (b *Builder) Alias(alt, id string) {
	if alt != "" && alt != id {
		b.alias[alt] = id
	}
}

func (b *Builder) Node(id string) *Node { return b.nodes[b.resolve(id)] }

// Upsert adds n or merges it into the node with the same id; the first
// non-empty value wins, sources are unioned, health takes the worst.
func (b *Builder) Upsert(n *Node, source string) *Node {
	n.ID = b.resolve(n.ID)
	cur, ok := b.nodes[n.ID]
	if !ok {
		if n.Health == "" {
			n.Health = "unknown"
		}
		n.Sources = addStr(n.Sources, source)
		b.nodes[n.ID] = n
		return n
	}
	if cur.Kind == KindExternal && n.Kind != KindExternal && n.Kind != "" {
		cur.Kind = n.Kind
	}
	if cur.Subtype == "" || cur.Subtype == "http" && n.Subtype != "" && n.Subtype != "http" {
		if n.Subtype != "" {
			cur.Subtype = n.Subtype
		}
	}
	fill(&cur.Namespace, n.Namespace)
	fill(&cur.Category, n.Category)
	fill(&cur.Address, n.Address)
	fill(&cur.Replicas, n.Replicas)
	if len(cur.Images) == 0 {
		cur.Images = n.Images
	}
	for _, s := range n.Services {
		cur.Services = addStr(cur.Services, s)
	}
	if cur.Conn == nil {
		cur.Conn = n.Conn
	}
	if cur.K8s == nil {
		cur.K8s = n.K8s
	}
	if cur.Coroot == nil {
		cur.Coroot = n.Coroot
	}
	if n.Stats.HasRED {
		cur.Stats = n.Stats
	}
	cur.Health = worse(cur.Health, n.Health)
	cur.Sources = addStr(cur.Sources, source)
	return cur
}

// Link adds or merges a directed edge. Missing endpoints are ignored.
func (b *Builder) Link(from, to, flow, protocol, source string, st Stats, ev *Evidence) *Edge {
	from, to = b.resolve(from), b.resolve(to)
	if from == to || b.nodes[from] == nil || b.nodes[to] == nil {
		return nil
	}
	if flow == "" {
		flow = "request"
	}
	id := from + "→" + to + "|" + flow
	e, ok := b.edges[id]
	if !ok {
		e = &Edge{ID: id, From: from, To: to, Flow: flow}
		b.edges[id] = e
	}
	if e.Protocol == "" || e.Protocol == "tcp" {
		if protocol != "" {
			e.Protocol = protocol
		}
	}
	e.Sources = addStr(e.Sources, source)
	if st.HasRED || st.Conns > 0 {
		if !e.Stats.HasRED || st.HasRED {
			e.Stats = mergeStats(e.Stats, st)
		}
	}
	if ev != nil && len(e.Evidence) < 12 {
		dup := false
		for _, x := range e.Evidence {
			if x == *ev {
				dup = true
				break
			}
		}
		if !dup {
			e.Evidence = append(e.Evidence, *ev)
		}
	}
	return e
}

func mergeStats(a, b Stats) Stats {
	if !a.HasRED {
		if b.Conns == 0 {
			b.Conns = a.Conns
		}
		return b
	}
	if !b.HasRED {
		a.Conns += b.Conns
		return a
	}
	// Two observed series on one edge (e.g. two protocols): add rates,
	// weight errors, keep the worst latency.
	total := a.RPS + b.RPS
	out := Stats{RPS: total, HasRED: true, Conns: a.Conns + b.Conns, P95Ms: max(a.P95Ms, b.P95Ms), Statuses: mergeStatuses(a.Statuses, b.Statuses)}
	if total > 0 {
		out.ErrorRate = (a.ErrorRate*a.RPS + b.ErrorRate*b.RPS) / total
	}
	return out
}

// EdgeBetween returns any edge from → to (whatever its flow), or nil.
func (b *Builder) EdgeBetween(from, to string) *Edge {
	from, to = b.resolve(from), b.resolve(to)
	for _, e := range b.edges {
		if e.From == from && e.To == to {
			return e
		}
	}
	return nil
}

// Graph returns the merged graph, sorted for stable output.
func (b *Builder) Graph(sources map[string]*SourceStatus) *Graph {
	g := &Graph{Nodes: make([]*Node, 0, len(b.nodes)), Edges: make([]*Edge, 0, len(b.edges)), Sources: sources}
	for _, n := range b.nodes {
		g.Nodes = append(g.Nodes, n)
	}
	for _, e := range b.edges {
		g.Edges = append(g.Edges, e)
	}
	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].ID < g.Nodes[j].ID })
	sort.Slice(g.Edges, func(i, j int) bool { return g.Edges[i].ID < g.Edges[j].ID })
	return g
}

func fill(dst *string, v string) {
	if *dst == "" {
		*dst = v
	}
}

func addStr(list []string, s string) []string {
	if s == "" {
		return list
	}
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

var healthRank = map[string]int{"": 0, "unknown": 0, "ok": 1, "warning": 2, "critical": 3}

func worse(a, b string) string {
	if healthRank[strings.ToLower(b)] > healthRank[strings.ToLower(a)] {
		return strings.ToLower(b)
	}
	if a == "" {
		return "unknown"
	}
	return a
}
