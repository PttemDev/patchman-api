package servicemap

import (
	"net"
	"strings"
)

// SavedConn is a PatchMan connection (DB, Kafka, RabbitMQ, Redis, ES, S3)
// and the addresses it points at.
type SavedConn struct {
	Ref       ConnRef
	Subtype   string
	Endpoints []Endpoint
}

// AttachConnections marks the nodes a saved connection points at, so the
// map can open them in their tool. Matches ClusterIPs / pod IPs, in-cluster
// Service DNS names and external host:port.
func (b *Builder) AttachConnections(conns []SavedConn) {
	for _, sc := range conns {
		for _, ep := range sc.Endpoints {
			for _, id := range b.matchAddress(ep) {
				n := b.nodes[id]
				if n == nil || n.Conn != nil {
					continue
				}
				if n.Kind == KindService && n.Subtype != "" && n.Subtype != "http" && sc.Subtype != "" && n.Subtype != sc.Subtype {
					continue
				}
				ref := sc.Ref
				n.Conn = &ref
				if n.Kind == KindExternal || (n.Kind == KindService && n.K8s == nil) {
					if k := KindOf(sc.Subtype); k != KindService {
						n.Kind, n.Subtype = k, sc.Subtype
					}
				}
			}
		}
	}
}

func (b *Builder) matchAddress(ep Endpoint) []string {
	host := strings.ToLower(strings.TrimSuffix(ep.Host, "."))
	if ip := net.ParseIP(host); ip != nil {
		if id, ok := b.podIPs[host]; ok {
			return []string{id}
		}
		if ids, ok := b.svcIPs[host]; ok {
			return ids
		}
	} else {
		parts := strings.Split(host, ".")
		for i, p := range parts {
			if p == "svc" && i >= 1 {
				parts = parts[:i]
				break
			}
		}
		var key string
		switch len(parts) {
		case 2:
			key = parts[1] + "/" + parts[0]
		case 3:
			key = parts[2] + "/" + parts[1]
		}
		if ids, ok := b.svcs[key]; ok && key != "" {
			return ids
		}
	}
	var out []string
	label := strings.SplitN(host, ".", 2)[0]
	for id, n := range b.nodes {
		// APM services are known by name only: "orders-db" matches a saved
		// connection to orders-db[.namespace…]:5432.
		if n.Address == "" && n.K8s == nil && n.Name != "" && net.ParseIP(host) == nil &&
			(strings.EqualFold(n.Name, host) || strings.EqualFold(n.Name, label)) {
			out = append(out, id)
			continue
		}
		if n.Address == "" {
			continue
		}
		nh, np, err := net.SplitHostPort(n.Address)
		if err != nil {
			nh, np = n.Address, ""
		}
		if strings.EqualFold(nh, host) && (ep.Port == 0 || np == "" || np == itoa(ep.Port)) {
			out = append(out, id)
		}
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return ""
	}
	var b [12]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}

// FinishHealth gives nodes nothing observed a health from their replicas.
func (b *Builder) FinishHealth() {
	for _, n := range b.nodes {
		if (n.Health == "" || n.Health == "unknown") && n.Replicas != "" {
			n.Health = HealthFromReplicas(n.Replicas)
		}
		if n.Health == "" {
			n.Health = "unknown"
		}
	}
}
