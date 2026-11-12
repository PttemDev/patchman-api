package servicemap

import (
	"context"
	"fmt"
	"math"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Sample is one series of an instant PromQL result.
type Sample struct {
	Metric map[string]string
	Value  float64
}

// PromQuerier runs an instant query. Coroot's Prometheus-compatible API and
// a cluster's own Prometheus both implement it, so the engine below does not
// care where the coroot-node-agent metrics come from.
type PromQuerier interface {
	Query(ctx context.Context, q string, at time.Time) ([]Sample, error)
}

// L7 protocols the coroot-node-agent reports, with their counter names.
var protocols = []struct{ proto, requests, duration string }{
	{"http", "container_http_requests_total", "container_http_requests_duration_seconds_total"},
	{"postgres", "container_postgres_queries_total", "container_postgres_queries_duration_seconds_total"},
	{"mysql", "container_mysql_queries_total", "container_mysql_queries_duration_seconds_total"},
	{"redis", "container_redis_queries_total", "container_redis_queries_duration_seconds_total"},
	{"mongo", "container_mongo_queries_total", "container_mongo_queries_duration_seconds_total"},
	{"memcached", "container_memcached_queries_total", "container_memcached_queries_duration_seconds_total"},
	{"kafka", "container_kafka_requests_total", "container_kafka_requests_duration_seconds_total"},
	{"cassandra", "container_cassandra_queries_total", "container_cassandra_queries_duration_seconds_total"},
	{"clickhouse", "container_clickhouse_queries_total", "container_clickhouse_queries_duration_seconds_total"},
	{"zookeeper", "container_zookeeper_requests_total", "container_zookeeper_requests_duration_seconds_total"},
	{"dubbo", "container_dubbo_requests_total", "container_dubbo_requests_duration_seconds_total"},
}

// MetricQueries are the instant queries the engine needs, each folded into
// one round-trip with `or` + a proto label. window is a PromQL duration.
func MetricQueries(window string) map[string]string {
	var req, lat []string
	for _, p := range protocols {
		req = append(req, fmt.Sprintf(`label_replace(sum by (container_id, destination, actual_destination, status) (rate(%s[%s])), "proto", "%s", "", "")`, p.requests, window, p.proto))
		// The agent's histograms are named …_duration_seconds_total_bucket.
		lat = append(lat, fmt.Sprintf(`label_replace(histogram_quantile(0.95, sum by (le, container_id, destination, actual_destination) (rate(%s_bucket[%s]))), "proto", "%s", "", "")`, p.duration, window, p.proto))
	}
	req = append(req, fmt.Sprintf(`label_replace(sum by (container_id, destination, actual_destination, method, status) (rate(container_rabbitmq_messages_total[%s])), "proto", "rabbitmq", "", "")`, window))
	req = append(req, fmt.Sprintf(`label_replace(sum by (container_id, destination, actual_destination, method) (rate(container_nats_messages_total[%s])), "proto", "nats", "", "")`, window))
	return map[string]string{
		"requests": strings.Join(req, " or "),
		"latency":  strings.Join(lat, " or "),
		"conns":    `sum by (container_id, destination, actual_destination) (container_net_tcp_active_connections)`,
		"podinfo":  `max by (namespace, pod, pod_ip, created_by_kind, created_by_name) (kube_pod_info)`,
		"podips":   `max by (namespace, pod, ip) (kube_pod_ips)`,
		"rsowner":  `max by (namespace, replicaset, owner_kind, owner_name) (kube_replicaset_owner)`,
		"jobowner": `max by (namespace, job_name, owner_kind, owner_name) (kube_job_owner)`,
		"fqdn":     `max by (ip, fqdn) (ip_to_fqdn)`,
	}
}

// Resolver maps containers and IPs to node ids. The config layer (a K8s
// connection) fills it exactly; kube_pod_info fills the rest; a name
// heuristic is the last resort.
type Resolver struct {
	PodWorkload map[string]string   // "ns/pod" → workload id
	IPWorkload  map[string]string   // pod IP → workload id
	ServiceIP   map[string][]string // ClusterIP → workload ids
	FQDN        map[string]string   // IP → name
}

func NewResolver() *Resolver {
	return &Resolver{PodWorkload: map[string]string{}, IPWorkload: map[string]string{}, ServiceIP: map[string][]string{}, FQDN: map[string]string{}}
}

// AddFromBuilder copies the pod / Service-IP index the config layer built
// (exact, from the K8s API) so metrics resolve without kube-state-metrics.
func (r *Resolver) AddFromBuilder(b *Builder) {
	for k, v := range b.pods {
		r.PodWorkload[k] = v
	}
	for k, v := range b.podIPs {
		r.IPWorkload[k] = v
	}
	for k, v := range b.svcIPs {
		r.ServiceIP[k] = v
	}
}

// AddKubeMetrics indexes kube_pod_info / kube_pod_ips / kube_*_owner /
// ip_to_fqdn samples.
func (r *Resolver) AddKubeMetrics(pods, podIPs, rsOwners, jobOwners, fqdn []Sample) {
	rs := map[string][2]string{}
	for _, s := range rsOwners {
		rs[s.Metric["namespace"]+"/"+s.Metric["replicaset"]] = [2]string{s.Metric["owner_kind"], s.Metric["owner_name"]}
	}
	jobs := map[string][2]string{}
	for _, s := range jobOwners {
		jobs[s.Metric["namespace"]+"/"+s.Metric["job_name"]] = [2]string{s.Metric["owner_kind"], s.Metric["owner_name"]}
	}
	for _, s := range pods {
		ns, pod := s.Metric["namespace"], s.Metric["pod"]
		kind, name := s.Metric["created_by_kind"], s.Metric["created_by_name"]
		switch kind {
		case "ReplicaSet":
			if o, ok := rs[ns+"/"+name]; ok && o[0] != "" && o[0] != "<none>" {
				kind, name = o[0], o[1]
			}
		case "Job":
			if o, ok := jobs[ns+"/"+name]; ok && o[0] != "" && o[0] != "<none>" {
				kind, name = o[0], o[1]
			}
		case "", "<none>":
			kind, name = "Pod", pod
		}
		id := WorkloadID(ns, kind, name)
		if _, ok := r.PodWorkload[ns+"/"+pod]; !ok {
			r.PodWorkload[ns+"/"+pod] = id
		}
		if ip := s.Metric["pod_ip"]; ip != "" {
			if _, ok := r.IPWorkload[ip]; !ok {
				r.IPWorkload[ip] = id
			}
		}
	}
	for _, s := range podIPs {
		if w, ok := r.PodWorkload[s.Metric["namespace"]+"/"+s.Metric["pod"]]; ok && s.Metric["ip"] != "" {
			if _, seen := r.IPWorkload[s.Metric["ip"]]; !seen {
				r.IPWorkload[s.Metric["ip"]] = w
			}
		}
	}
	for _, s := range fqdn {
		if s.Metric["ip"] != "" && s.Metric["fqdn"] != "" {
			r.FQDN[s.Metric["ip"]] = strings.TrimSuffix(s.Metric["fqdn"], ".")
		}
	}
}

var (
	deployPodRe = regexp.MustCompile(`^(.+)-[a-z0-9]{6,10}-[a-z0-9]{5}$`)
	stsPodRe    = regexp.MustCompile(`^(.+)-\d+$`)
	jobPodRe    = regexp.MustCompile(`^(.+)-\d{8,}-[a-z0-9]{5}$`)
	dsPodRe     = regexp.MustCompile(`^(.+)-[a-z0-9]{5}$`)
)

// containerNode maps a coroot container_id ("/k8s/ns/pod/container",
// "/docker/name", …) to a node id + namespace + name.
func (r *Resolver) containerNode(cid string) (id, ns, name, kind string, ok bool) {
	parts := strings.Split(strings.Trim(cid, "/"), "/")
	if len(parts) >= 3 && (parts[0] == "k8s" || parts[0] == "k8s-cronjob") {
		ns, pod := parts[1], parts[2]
		if w, ok := r.PodWorkload[ns+"/"+pod]; ok {
			k, n := splitWorkload(w)
			return w, ns, n, k, true
		}
		// Heuristic: strip the controller hashes off the pod name.
		switch {
		case parts[0] == "k8s-cronjob" || jobPodRe.MatchString(pod):
			if m := jobPodRe.FindStringSubmatch(pod); m != nil {
				return WorkloadID(ns, "CronJob", m[1]), ns, m[1], "CronJob", true
			}
		case deployPodRe.MatchString(pod):
			m := deployPodRe.FindStringSubmatch(pod)
			return WorkloadID(ns, "Deployment", m[1]), ns, m[1], "Deployment", true
		case stsPodRe.MatchString(pod):
			m := stsPodRe.FindStringSubmatch(pod)
			return WorkloadID(ns, "StatefulSet", m[1]), ns, m[1], "StatefulSet", true
		case dsPodRe.MatchString(pod):
			m := dsPodRe.FindStringSubmatch(pod)
			return WorkloadID(ns, "DaemonSet", m[1]), ns, m[1], "DaemonSet", true
		}
		return WorkloadID(ns, "Pod", pod), ns, pod, "Pod", true
	}
	if len(parts) >= 2 && (parts[0] == "docker" || parts[0] == "nerdctl" || parts[0] == "podman") {
		return "container:" + parts[1], "", parts[1], "Container", true
	}
	if len(parts) >= 2 && parts[0] == "system.slice" {
		n := strings.TrimSuffix(parts[1], ".service")
		return "systemd:" + n, "", n, "Systemd", true
	}
	return "", "", "", "", false
}

// ensureWorkload adds a node for a workload id ("ns/Kind/name") that only
// metrics know about (no K8s connection on the map).
func ensureWorkload(b *Builder, id, proto string) {
	if b.Node(id) != nil {
		return
	}
	p := strings.SplitN(id, "/", 3)
	if len(p) != 3 {
		return
	}
	sub, kind := "http", KindService
	if k := KindOf(proto); k != KindService {
		sub, kind = proto, k
	}
	b.Upsert(&Node{ID: id, Kind: kind, Subtype: sub, Namespace: p[0], Name: p[2], K8s: &K8sRef{Kind: p[1], Namespace: p[0], Name: p[2]}}, "metrics")
}

func splitWorkload(id string) (kind, name string) {
	p := strings.SplitN(id, "/", 3)
	if len(p) == 3 {
		return p[1], p[2]
	}
	return "", id
}

// destination resolves "ip:port" to node ids (creating an external node).
func (r *Resolver) destination(b *Builder, dest, proto string) []string {
	host, portS, err := net.SplitHostPort(dest)
	if err != nil {
		host, portS = dest, ""
	}
	port, _ := strconv.Atoi(portS)
	if w, ok := r.IPWorkload[host]; ok {
		ensureWorkload(b, w, proto)
		return []string{w}
	}
	if ws, ok := r.ServiceIP[host]; ok && len(ws) > 0 {
		return ws
	}
	name := host
	if f, ok := r.FQDN[host]; ok {
		name = f
	}
	sub := proto
	if sub == "" || sub == "tcp" {
		sub = subtypeByPort(port)
	}
	ep := Endpoint{Host: strings.ToLower(name), Port: port}
	id := "ext:" + ep.HostPort()
	n := b.Upsert(&Node{ID: id, Kind: externalKind(sub), Subtype: sub, Name: name, Address: ep.HostPort()}, "metrics")
	return []string{n.ID}
}

type edgeAcc struct {
	from, to, proto, flow   string
	rps, errRPS, p95, conns float64
	statuses                map[string]float64
}

// BuildMetrics adds edges observed by coroot-node-agent. source names the
// layer ("coroot" or "prometheus").
func BuildMetrics(b *Builder, r *Resolver, requests, latency, conns []Sample, source string) {
	acc := map[string]*edgeAcc{}
	get := func(cid, dest, proto, flow string) []*edgeAcc {
		fromID, ns, name, kind, ok := r.containerNode(cid)
		if !ok || dest == "" {
			return nil
		}
		if b.Node(fromID) == nil {
			var k8s *K8sRef
			if ns != "" {
				k8s = &K8sRef{Kind: kind, Namespace: ns, Name: name}
			}
			b.Upsert(&Node{ID: fromID, Kind: KindService, Subtype: "http", Namespace: ns, Name: name, K8s: k8s}, source)
		}
		var out []*edgeAcc
		for _, to := range r.destination(b, dest, proto) {
			key := fromID + "|" + to + "|" + proto + "|" + flow
			a, ok := acc[key]
			if !ok {
				a = &edgeAcc{from: fromID, to: to, proto: proto, flow: flow}
				acc[key] = a
			}
			out = append(out, a)
		}
		return out
	}
	dest := func(m map[string]string) string {
		if d := m["actual_destination"]; d != "" {
			return d
		}
		return m["destination"]
	}
	for _, s := range requests {
		if !finite(s.Value) || s.Value <= 0 {
			continue
		}
		proto := s.Metric["proto"]
		flow := "request"
		if m := s.Metric["method"]; proto == "rabbitmq" || proto == "nats" {
			switch strings.ToLower(m) {
			case "publish", "produce", "basic.publish", "pub":
				flow = "publish"
			case "deliver", "basic.deliver", "basic.get", "consume", "msg", "sub":
				flow = "consume"
			}
		}
		for _, a := range get(s.Metric["container_id"], dest(s.Metric), proto, flow) {
			a.rps += s.Value
			a.statuses = AddStatus(a.statuses, s.Metric["status"], s.Value)
			if isErrorStatus(s.Metric["status"]) {
				a.errRPS += s.Value
			}
		}
	}
	for _, s := range latency {
		if !finite(s.Value) {
			continue
		}
		for _, a := range get(s.Metric["container_id"], dest(s.Metric), s.Metric["proto"], "request") {
			if a.rps > 0 || a.p95 == 0 {
				a.p95 = math.Max(a.p95, s.Value*1000)
			}
		}
	}
	for _, s := range conns {
		if !finite(s.Value) || s.Value <= 0 {
			continue
		}
		cid, dst := s.Metric["container_id"], dest(s.Metric)
		_, portS, _ := net.SplitHostPort(dst)
		port, _ := strconv.Atoi(portS)
		proto := subtypeByPort(port)
		// Count connections onto the L7 edge when there is one.
		matched := false
		fromID, _, _, _, ok := r.containerNode(cid)
		if !ok {
			continue
		}
		for _, to := range r.destination(b, dst, proto) {
			for _, a := range acc {
				if a.from == fromID && a.to == to {
					a.conns += s.Value
					matched = true
					break
				}
			}
		}
		if !matched {
			p := proto
			if p == "" {
				p = "tcp"
			}
			for _, a := range get(cid, dst, p, "request") {
				a.conns += s.Value
			}
		}
	}
	for _, a := range acc {
		if a.rps == 0 && a.conns == 0 {
			continue
		}
		st := Stats{Conns: a.conns}
		if a.rps > 0 {
			st.HasRED, st.RPS, st.P95Ms, st.Statuses = true, a.rps, a.p95, a.statuses
			st.ErrorRate = a.errRPS / a.rps
		}
		// The callee's kind follows what is spoken to it.
		if n := b.Node(a.to); n != nil && a.proto != "http" && a.proto != "tcp" && a.proto != "" {
			if n.Kind == KindService || n.Kind == KindExternal {
				if k := KindOf(a.proto); k != KindService {
					n.Kind, n.Subtype = k, a.proto
				}
			}
		}
		b.Link(a.from, a.to, a.flow, ProtocolOf(a.proto), source, st, nil)
	}
	Rollup(b, source)
}

// Rollup gives every node the totals of its observed inbound edges
// (what it serves), and a health derived from the error rate when nothing
// better is known.
func Rollup(b *Builder, source string) {
	type tot struct {
		rps, err, p95 float64
		rateOnly      bool
		statuses      map[string]float64
	}
	in := map[string]*tot{}
	for _, e := range b.edges {
		if !e.Stats.HasRED || e.Flow == "consume" {
			continue
		}
		t := in[e.To]
		if t == nil {
			t = &tot{rateOnly: true}
			in[e.To] = t
		}
		t.rateOnly = t.rateOnly && e.Stats.RateOnly
		t.rps += e.Stats.RPS
		t.err += e.Stats.RPS * e.Stats.ErrorRate
		t.p95 = math.Max(t.p95, e.Stats.P95Ms)
		t.statuses = mergeStatuses(t.statuses, e.Stats.Statuses)
	}
	for id, t := range in {
		n := b.nodes[id]
		if n == nil || n.Stats.HasRED || t.rps == 0 {
			continue
		}
		n.Stats = Stats{HasRED: true, RPS: t.rps, ErrorRate: t.err / t.rps, P95Ms: t.p95, RateOnly: t.rateOnly, Statuses: t.statuses}
		if !t.rateOnly {
			n.Health = worse(n.Health, HealthFromRED(n.Stats))
		}
		n.Sources = addStr(n.Sources, source)
	}
}

// HealthFromRED: ≥5% errors critical, ≥1% warning.
func HealthFromRED(s Stats) string {
	switch {
	case !s.HasRED:
		return "unknown"
	case s.ErrorRate >= 0.05:
		return "critical"
	case s.ErrorRate >= 0.01:
		return "warning"
	}
	return "ok"
}

// isErrorStatus mirrors Coroot's IsRequestStatusFailed: only "failed", 5xx
// and non-OK gRPC codes are errors. Everything else — 4xx, and "unknown",
// which the agent reports when it cannot parse a response (common for
// MongoDB) — is not.
func isErrorStatus(status string) bool {
	s := strings.TrimSpace(status)
	switch {
	case s == "failed":
	case strings.HasPrefix(s, "5"):
	case strings.HasPrefix(s, "grpc:") && s != "grpc:OK":
	default:
		return false
	}
	return true
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
