package servicemap

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// ClusterInput is what the config layer reads from one cluster. Secrets is
// nil unless the connection opted in to host extraction from Secrets.
type ClusterInput struct {
	Deployments  []appsv1.Deployment
	StatefulSets []appsv1.StatefulSet
	DaemonSets   []appsv1.DaemonSet
	CronJobs     []batchv1.CronJob
	Services     []corev1.Service
	Ingresses    []networkingv1.Ingress
	Pods         []corev1.Pod                 // for the metric resolvers (pod / IP → workload)
	ConfigMaps   map[string]*corev1.ConfigMap // "ns/name"
	Secrets      map[string]*corev1.Secret    // "ns/name"; nil = not read
}

type workload struct {
	id, ns, kind, name string
	tmpl               corev1.PodTemplateSpec
	sel                labels.Selector
}

// cluster indexes services so an address can be resolved to its workloads.
type cluster struct {
	workloads []workload
	svcByKey  map[string][]string // "ns/svc" → workload ids, or one external node id
	svcByIP   map[string]string   // clusterIP → "ns/svc"
	svcPorts  map[string][]int32  // "ns/svc" → ports
}

// BuildConfig adds the config layer: workloads, Ingress → Service →
// workload, and every address a workload's configuration points at.
func BuildConfig(b *Builder, in ClusterInput, st *SourceStatus) {
	c := &cluster{svcByKey: map[string][]string{}, svcByIP: map[string]string{}, svcPorts: map[string][]int32{}}
	addW := func(ns, kind, name, replicas string, sel *metav1.LabelSelector, tmpl corev1.PodTemplateSpec) {
		id := WorkloadID(ns, kind, name)
		var s labels.Selector
		if sel != nil {
			s, _ = metav1.LabelSelectorAsSelector(sel)
		}
		if s == nil || s.Empty() {
			s = labels.SelectorFromSet(tmpl.Labels)
		}
		c.workloads = append(c.workloads, workload{id: id, ns: ns, kind: kind, name: name, tmpl: tmpl, sel: s})
		sub := classifyPod(tmpl.Spec)
		kindN := KindService
		if sub != "" && sub != "http" {
			kindN = KindOf(sub)
		}
		b.Upsert(&Node{ID: id, Kind: kindN, Subtype: sub, Namespace: ns, Name: name, Replicas: replicas,
			Images: images(tmpl.Spec), K8s: &K8sRef{Kind: kind, Namespace: ns, Name: name}}, "config")
	}
	for _, d := range in.Deployments {
		addW(d.Namespace, "Deployment", d.Name, replicas(d.Status.ReadyReplicas, d.Spec.Replicas), d.Spec.Selector, d.Spec.Template)
	}
	for _, s := range in.StatefulSets {
		addW(s.Namespace, "StatefulSet", s.Name, replicas(s.Status.ReadyReplicas, s.Spec.Replicas), s.Spec.Selector, s.Spec.Template)
	}
	for _, d := range in.DaemonSets {
		addW(d.Namespace, "DaemonSet", d.Name, fmt.Sprintf("%d/%d", d.Status.NumberReady, d.Status.DesiredNumberScheduled), d.Spec.Selector, d.Spec.Template)
	}
	for _, cj := range in.CronJobs {
		addW(cj.Namespace, "CronJob", cj.Name, "", nil, cj.Spec.JobTemplate.Spec.Template)
	}

	// Services → the workloads whose pod template their selector matches.
	for _, s := range in.Services {
		key := s.Namespace + "/" + s.Name
		for _, p := range s.Spec.Ports {
			c.svcPorts[key] = append(c.svcPorts[key], p.Port)
		}
		if ip := s.Spec.ClusterIP; ip != "" && ip != "None" {
			c.svcByIP[ip] = key
		}
		if s.Spec.Type == corev1.ServiceTypeExternalName && s.Spec.ExternalName != "" {
			port := 0
			if len(s.Spec.Ports) > 0 {
				port = int(s.Spec.Ports[0].Port)
			}
			id := externalNode(b, Endpoint{Host: strings.ToLower(s.Spec.ExternalName), Port: port})
			c.svcByKey[key] = []string{id}
			continue
		}
		if len(s.Spec.Selector) == 0 {
			continue
		}
		sel := labels.SelectorFromSet(s.Spec.Selector)
		for _, w := range c.workloads {
			if w.ns == s.Namespace && sel.Matches(labels.Set(w.tmpl.Labels)) {
				c.svcByKey[key] = append(c.svcByKey[key], w.id)
				if n := b.Node(w.id); n != nil {
					n.Services = addStr(n.Services, s.Name)
				}
			}
		}
	}

	// Pod and Service IPs → workloads, for the metric layers.
	for _, p := range in.Pods {
		for _, w := range c.workloads {
			if w.ns == p.Namespace && w.sel.Matches(labels.Set(p.Labels)) {
				b.pods[p.Namespace+"/"+p.Name] = w.id
				if ip := p.Status.PodIP; ip != "" && !p.Spec.HostNetwork {
					b.podIPs[ip] = w.id
				}
				break
			}
		}
	}
	for key, ids := range c.svcByKey {
		b.svcs[key] = ids
	}
	for ip, key := range c.svcByIP {
		if ids := c.svcByKey[key]; len(ids) > 0 {
			b.svcIPs[ip] = ids
		}
	}

	// Ingress → backend Service → workloads.
	for _, ing := range in.Ingresses {
		var hosts []string
		for _, r := range ing.Spec.Rules {
			if r.Host != "" {
				hosts = addStr(hosts, r.Host)
			}
		}
		name := ing.Name
		if len(hosts) > 0 {
			name = strings.Join(hosts, ", ")
		}
		id := "ingress:" + ing.Namespace + "/" + ing.Name
		b.Upsert(&Node{ID: id, Kind: KindIngress, Subtype: "http", Namespace: ing.Namespace, Name: name,
			K8s: &K8sRef{Kind: "Ingress", Namespace: ing.Namespace, Name: ing.Name}}, "config")
		ev := &Evidence{Source: "ingress", Name: ing.Name}
		link := func(svc string) {
			for _, to := range c.svcByKey[ing.Namespace+"/"+svc] {
				b.Link(id, to, "request", "http", "ingress", Stats{}, ev)
			}
		}
		if d := ing.Spec.DefaultBackend; d != nil && d.Service != nil {
			link(d.Service.Name)
		}
		for _, r := range ing.Spec.Rules {
			if r.HTTP == nil {
				continue
			}
			for _, p := range r.HTTP.Paths {
				if p.Backend.Service != nil {
					link(p.Backend.Service.Name)
				}
			}
		}
	}

	// Each workload's configuration → the addresses it talks to.
	secretsRead := in.Secrets != nil
	skippedSecrets := 0
	for _, w := range c.workloads {
		for _, src := range configSources(w, in, secretsRead, &skippedSecrets) {
			for _, ep := range src.endpoints {
				for _, to := range c.resolve(b, w.ns, ep) {
					if to == w.id {
						continue
					}
					sub := ""
					if n := b.Node(to); n != nil {
						sub = n.Subtype
					}
					if sub == "" {
						sub = Subtype(ep.Scheme, ep.Port)
					}
					ev := src.ev
					ev.Extracted = ep.String()
					b.Link(w.id, to, "request", ProtocolOf(sub), "config", Stats{}, &ev)
				}
			}
		}
	}
	if !secretsRead && skippedSecrets > 0 {
		st.Notes = append(st.Notes, fmt.Sprintf("secrets_skipped:%d", skippedSecrets))
	}
}

type configSource struct {
	ev        Evidence
	endpoints []Endpoint
}

// configSources scans a pod template's env, envFrom and mounted ConfigMaps
// (and Secrets when read). Values are parsed here and dropped: only the
// evidence names and extracted addresses leave this function.
func configSources(w workload, in ClusterInput, secretsRead bool, skipped *int) []configSource {
	var out []configSource
	spec := w.tmpl.Spec
	containers := append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...)
	scan := func(source, name string, kv map[string]string) {
		port := func(prefix string) int {
			for _, k := range []string{prefix + "_PORT", prefix + "PORT"} {
				if v, ok := kv[k]; ok {
					if p, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
						return p
					}
				}
			}
			return 0
		}
		keys := make([]string, 0, len(kv))
		for k := range kv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if eps := ExtractEndpoints(k, kv[k], port); len(eps) > 0 {
				out = append(out, configSource{ev: Evidence{Source: source, Workload: w.name, Name: name, Key: k}, endpoints: eps})
			}
		}
	}
	cm := func(name string) map[string]string {
		if m := in.ConfigMaps[w.ns+"/"+name]; m != nil {
			return m.Data
		}
		return nil
	}
	sec := func(name string) map[string]string {
		if !secretsRead {
			*skipped++
			return nil
		}
		s := in.Secrets[w.ns+"/"+name]
		if s == nil {
			return nil
		}
		m := make(map[string]string, len(s.Data)+len(s.StringData))
		for k, v := range s.Data {
			m[k] = string(v)
		}
		for k, v := range s.StringData {
			m[k] = v
		}
		return m
	}
	for _, ctr := range containers {
		plain := map[string]string{}
		for _, e := range ctr.Env {
			switch {
			case e.ValueFrom == nil:
				plain[e.Name] = e.Value
			case e.ValueFrom.ConfigMapKeyRef != nil:
				r := e.ValueFrom.ConfigMapKeyRef
				if v, ok := cm(r.Name)[r.Key]; ok {
					scan("configMap", r.Name, map[string]string{e.Name: v})
				}
			case e.ValueFrom.SecretKeyRef != nil:
				r := e.ValueFrom.SecretKeyRef
				if v, ok := sec(r.Name)[r.Key]; ok {
					scan("secret", r.Name, map[string]string{e.Name: v})
				}
			}
		}
		scan("env", "", plain)
		for _, ef := range ctr.EnvFrom {
			if ef.ConfigMapRef != nil {
				scan("configMap", ef.ConfigMapRef.Name, cm(ef.ConfigMapRef.Name))
			}
			if ef.SecretRef != nil {
				scan("secret", ef.SecretRef.Name, sec(ef.SecretRef.Name))
			}
		}
	}
	// Mounted files: configuration only — a ConfigMap carrying source code
	// would make every workload that mounts it look like a client of every
	// host the code mentions.
	files := func(m map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range m {
			if configFile(k) {
				out[k] = v
			}
		}
		return out
	}
	for _, v := range spec.Volumes {
		switch {
		case v.ConfigMap != nil:
			scan("configMap", v.ConfigMap.Name, files(cm(v.ConfigMap.Name)))
		case v.Secret != nil:
			scan("secret", v.Secret.SecretName, files(sec(v.Secret.SecretName)))
		case v.Projected != nil:
			for _, ps := range v.Projected.Sources {
				if ps.ConfigMap != nil {
					scan("configMap", ps.ConfigMap.Name, files(cm(ps.ConfigMap.Name)))
				}
				if ps.Secret != nil {
					scan("secret", ps.Secret.Name, files(sec(ps.Secret.Name)))
				}
			}
		}
	}
	return out
}

var codeExt = map[string]bool{
	".py": true, ".js": true, ".mjs": true, ".cjs": true, ".ts": true, ".go": true, ".java": true, ".kt": true, ".rb": true,
	".php": true, ".sh": true, ".bash": true, ".ps1": true, ".lua": true, ".rs": true, ".c": true, ".cpp": true, ".cs": true,
	".scala": true, ".pl": true, ".r": true, ".sql": true, ".html": true, ".css": true, ".md": true, ".txt": true, ".crt": true, ".pem": true, ".key": true,
}

// configFile reports whether a mounted file name looks like configuration
// (yaml, properties, json, env, ini, toml, conf, or no extension).
func configFile(name string) bool {
	i := strings.LastIndex(name, ".")
	if i <= 0 {
		return true
	}
	return !codeExt[strings.ToLower(name[i:])]
}

// resolve maps an address seen in namespace ns to node ids: the workloads
// behind an in-cluster Service, or an external node.
func (c *cluster) resolve(b *Builder, ns string, ep Endpoint) []string {
	if key := c.serviceKey(ns, ep.Host); key != "" {
		if ids := c.svcByKey[key]; len(ids) > 0 {
			return ids
		}
		// A Service with no matching workload (selector-less, other kinds):
		// draw the Service itself.
		parts := strings.SplitN(key, "/", 2)
		port := ep.Port
		if port == 0 && len(c.svcPorts[key]) > 0 {
			port = int(c.svcPorts[key][0])
		}
		sub := Subtype(ep.Scheme, port)
		id := "svc:" + key
		b.Upsert(&Node{ID: id, Kind: kindOrService(sub), Subtype: sub, Namespace: parts[0], Name: parts[1],
			Address: ep.HostPort(), K8s: &K8sRef{Kind: "Service", Namespace: parts[0], Name: parts[1]}}, "config")
		return []string{id}
	}
	if ep.Port == 0 && ep.Scheme == "" && !strings.Contains(ep.Host, ".") && net.ParseIP(ep.Host) == nil {
		return nil // a bare word that is no Service here: not an address
	}
	return []string{externalNode(b, ep)}
}

// serviceKey resolves svc, svc.ns, svc.ns.svc[.cluster.local], headless
// pod-0.svc.ns.svc… and ClusterIPs to "ns/svc" when that Service exists.
func (c *cluster) serviceKey(ns, host string) string {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if key, ok := c.svcByIP[host]; ok {
		return key
	}
	if net.ParseIP(host) != nil {
		return ""
	}
	parts := strings.Split(host, ".")
	// Strip a cluster domain: …svc.<domain>.
	for i, p := range parts {
		if p == "svc" && i >= 1 {
			parts = parts[:i]
			break
		}
	}
	try := func(n, s string) string {
		k := n + "/" + s
		if _, ok := c.svcPorts[k]; ok {
			return k
		}
		return ""
	}
	switch len(parts) {
	case 1:
		return try(ns, parts[0])
	case 2:
		if k := try(parts[1], parts[0]); k != "" {
			return k
		}
		return ""
	case 3: // pod-0.svc.ns (headless)
		return try(parts[2], parts[1])
	}
	return ""
}

func externalNode(b *Builder, ep Endpoint) string {
	sub := Subtype(ep.Scheme, ep.Port)
	id := "ext:" + ep.HostPort()
	b.Upsert(&Node{ID: id, Kind: externalKind(sub), Subtype: sub, Name: ep.Host, Address: ep.HostPort()}, "config")
	return id
}

func externalKind(sub string) string {
	if k := KindOf(sub); k != KindService {
		return k
	}
	return KindExternal
}

func kindOrService(sub string) string {
	if k := KindOf(sub); k != KindService {
		return k
	}
	return KindService
}

var imageSubtypes = []struct{ needle, sub string }{
	{"postgres", "postgres"}, {"postgis", "postgres"}, {"timescale", "postgres"}, {"pgbouncer", "postgres"},
	{"mysql", "mysql"}, {"mariadb", "mysql"}, {"percona-server", "mysql"},
	{"mssql", "mssql"}, {"mongo", "mongo"},
	{"redis", "redis"}, {"valkey", "redis"}, {"keydb", "redis"}, {"dragonfly", "redis"},
	{"rabbitmq", "rabbitmq"}, {"kafka", "kafka"}, {"redpanda", "kafka"}, {"nats", "nats"},
	{"elasticsearch", "elasticsearch"}, {"opensearch", "elasticsearch"},
	{"minio", "s3"}, {"memcached", "memcached"}, {"cassandra", "cassandra"},
	{"clickhouse", "clickhouse"}, {"zookeeper", "zookeeper"},
}

// classifyPod guesses what a workload is from its images, then its ports.
func classifyPod(spec corev1.PodSpec) string {
	for _, ctr := range spec.Containers {
		img := strings.ToLower(ctr.Image)
		if i := strings.LastIndex(img, "/"); i >= 0 {
			img = img[i+1:]
		}
		for _, m := range imageSubtypes {
			if strings.Contains(img, m.needle) && !strings.Contains(img, "exporter") {
				return m.sub
			}
		}
	}
	for _, ctr := range spec.Containers {
		for _, p := range ctr.Ports {
			if s := subtypeByPort(int(p.ContainerPort)); s != "" && s != "http" {
				return s
			}
		}
	}
	return "http"
}

func images(spec corev1.PodSpec) []string {
	var out []string
	for _, c := range spec.Containers {
		out = addStr(out, c.Image)
	}
	return out
}

func replicas(ready int32, desired *int32) string {
	d := int32(1)
	if desired != nil {
		d = *desired
	}
	return fmt.Sprintf("%d/%d", ready, d)
}

// HealthFromReplicas is the fallback health when nothing observed it.
func HealthFromReplicas(r string) string {
	var ready, desired int
	if _, err := fmt.Sscanf(r, "%d/%d", &ready, &desired); err != nil {
		return "unknown"
	}
	switch {
	case desired == 0:
		return "unknown"
	case ready == 0:
		return "critical"
	case ready < desired:
		return "warning"
	}
	return "ok"
}
