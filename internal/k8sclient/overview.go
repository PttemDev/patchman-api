package k8sclient

import (
	"context"
	"sort"
	"strings"
	"sync"

	"patchman-be/internal/models"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// nodeMetricsGVR is the metrics-server node metrics resource. It is queried
// through the DYNAMIC client (no metrics client dependency): when the group is
// not registered / metrics-server is absent the List simply errors and metrics
// are reported unavailable.
var nodeMetricsGVR = schema.GroupVersionResource{
	Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes",
}

// nodeUsage is one node's measured CPU (cores) + memory (bytes) usage, parsed
// from the metrics API.
type nodeUsage struct {
	cpu float64
	mem int64
}

// Overview aggregates a Lens-style cluster dashboard: node/pod/workload/
// networking counts, namespace + event counts, the server version, and (when
// metrics-server is present) cluster + per-node CPU/memory usage vs capacity.
//
// Every section is fetched by its own goroutine and merged under a mutex, so a
// slow or failing section never blocks or fails the whole overview — a section
// that errors just stays at its zero value (partial-failure tolerant).
//
// Namespace scoping mirrors List: cluster-scoped kinds (nodes, namespaces, node
// metrics, version) are always cluster-wide; namespaced kinds list across all
// namespaces (namespace "") UNLESS the connection restricts allowedNamespaces,
// in which case each namespaced count sums only the allowed namespaces — so a
// scoped connection can never read cluster-wide counts through the overview.
func (r *Registry) Overview(ctx context.Context, conn models.K8sConnection, contextName string) (*ClusterOverview, error) {
	p, err := r.resolve(conn, contextName)
	if err != nil {
		return nil, err
	}
	cs := p.clientset
	lo := metav1.ListOptions{}

	// nsList is the set of namespaces a namespaced List fans out over. Empty
	// string = cluster-wide (all namespaces); a restricted connection lists only
	// its allowed namespaces.
	nsList := []string{""}
	if opt := parseOptions(conn.OptionsJSON); len(opt.AllowedNamespaces) > 0 {
		nsList = nsList[:0]
		for _, ns := range opt.AllowedNamespaces {
			if ns = strings.TrimSpace(ns); ns != "" {
				nsList = append(nsList, ns)
			}
		}
	}

	var (
		ov  ClusterOverview
		mu  sync.Mutex
		wg  sync.WaitGroup
		grp overviewShared
	)

	// countAcross runs a namespaced List once per target namespace and sums the
	// item counts, tolerating per-namespace failures.
	countAcross := func(list func(ns string) (int, error)) int {
		total := 0
		for _, ns := range nsList {
			n, err := list(ns)
			if err != nil {
				continue
			}
			total += n
		}
		return total
	}

	// spawn wraps the WaitGroup bookkeeping for one section.
	spawn := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn()
		}()
	}

	// ── version (cluster-scoped) — reuse the bounded ServerVersion probe.
	spawn(func() {
		v, err := r.Test(ctx, conn, contextName)
		if err != nil || v == nil {
			return
		}
		mu.Lock()
		ov.Version = *v
		mu.Unlock()
	})

	// ── nodes (cluster-scoped). The NodeList is also kept for the metrics join.
	spawn(func() {
		l, err := cs.CoreV1().Nodes().List(ctx, lo)
		if err != nil {
			return
		}
		var n OverviewNodes
		for i := range l.Items {
			n.Total++
			if nodeStatus(&l.Items[i]) == "Ready" {
				n.Ready++
			} else {
				n.NotReady++
			}
		}
		mu.Lock()
		ov.Nodes = n
		grp.nodeList = l
		mu.Unlock()
	})

	// ── namespaces (cluster-scoped).
	spawn(func() {
		l, err := cs.CoreV1().Namespaces().List(ctx, lo)
		if err != nil {
			return
		}
		mu.Lock()
		ov.Namespaces = len(l.Items)
		mu.Unlock()
	})

	// ── pods + phase breakdown (namespaced).
	spawn(func() {
		var pods OverviewPods
		for _, ns := range nsList {
			l, err := cs.CoreV1().Pods(ns).List(ctx, lo)
			if err != nil {
				continue
			}
			for i := range l.Items {
				pods.Total++
				switch l.Items[i].Status.Phase {
				case corev1.PodRunning:
					pods.Running++
				case corev1.PodPending:
					pods.Pending++
				case corev1.PodSucceeded:
					pods.Succeeded++
				case corev1.PodFailed:
					pods.Failed++
				default: // PodUnknown or an empty phase
					pods.Unknown++
				}
			}
		}
		mu.Lock()
		ov.Pods = pods
		mu.Unlock()
	})

	// ── events + warning count (namespaced).
	spawn(func() {
		var ev OverviewEvents
		for _, ns := range nsList {
			l, err := cs.CoreV1().Events(ns).List(ctx, lo)
			if err != nil {
				continue
			}
			for i := range l.Items {
				ev.Total++
				if l.Items[i].Type == corev1.EventTypeWarning {
					ev.Warnings++
				}
			}
		}
		mu.Lock()
		ov.Events = ev
		mu.Unlock()
	})

	// ── workloads (namespaced, count-only).
	spawn(func() {
		c := countAcross(func(ns string) (int, error) {
			l, err := cs.AppsV1().Deployments(ns).List(ctx, lo)
			if err != nil {
				return 0, err
			}
			return len(l.Items), nil
		})
		mu.Lock()
		ov.Workloads.Deployments = c
		mu.Unlock()
	})
	spawn(func() {
		c := countAcross(func(ns string) (int, error) {
			l, err := cs.AppsV1().StatefulSets(ns).List(ctx, lo)
			if err != nil {
				return 0, err
			}
			return len(l.Items), nil
		})
		mu.Lock()
		ov.Workloads.StatefulSets = c
		mu.Unlock()
	})
	spawn(func() {
		c := countAcross(func(ns string) (int, error) {
			l, err := cs.AppsV1().DaemonSets(ns).List(ctx, lo)
			if err != nil {
				return 0, err
			}
			return len(l.Items), nil
		})
		mu.Lock()
		ov.Workloads.DaemonSets = c
		mu.Unlock()
	})
	spawn(func() {
		c := countAcross(func(ns string) (int, error) {
			l, err := cs.BatchV1().Jobs(ns).List(ctx, lo)
			if err != nil {
				return 0, err
			}
			return len(l.Items), nil
		})
		mu.Lock()
		ov.Workloads.Jobs = c
		mu.Unlock()
	})
	spawn(func() {
		c := countAcross(func(ns string) (int, error) {
			l, err := cs.BatchV1().CronJobs(ns).List(ctx, lo)
			if err != nil {
				return 0, err
			}
			return len(l.Items), nil
		})
		mu.Lock()
		ov.Workloads.CronJobs = c
		mu.Unlock()
	})
	spawn(func() {
		c := countAcross(func(ns string) (int, error) {
			l, err := cs.AppsV1().ReplicaSets(ns).List(ctx, lo)
			if err != nil {
				return 0, err
			}
			return len(l.Items), nil
		})
		mu.Lock()
		ov.Workloads.ReplicaSets = c
		mu.Unlock()
	})

	// ── networking (namespaced, count-only).
	spawn(func() {
		c := countAcross(func(ns string) (int, error) {
			l, err := cs.CoreV1().Services(ns).List(ctx, lo)
			if err != nil {
				return 0, err
			}
			return len(l.Items), nil
		})
		mu.Lock()
		ov.Networking.Services = c
		mu.Unlock()
	})
	spawn(func() {
		c := countAcross(func(ns string) (int, error) {
			l, err := cs.NetworkingV1().Ingresses(ns).List(ctx, lo)
			if err != nil {
				return 0, err
			}
			return len(l.Items), nil
		})
		mu.Lock()
		ov.Networking.Ingresses = c
		mu.Unlock()
	})
	spawn(func() {
		c := countAcross(func(ns string) (int, error) {
			l, err := cs.CoreV1().ConfigMaps(ns).List(ctx, lo)
			if err != nil {
				return 0, err
			}
			return len(l.Items), nil
		})
		mu.Lock()
		ov.Networking.ConfigMaps = c
		mu.Unlock()
	})
	spawn(func() {
		c := countAcross(func(ns string) (int, error) {
			l, err := cs.CoreV1().Secrets(ns).List(ctx, lo)
			if err != nil {
				return 0, err
			}
			return len(l.Items), nil
		})
		mu.Lock()
		ov.Networking.Secrets = c
		mu.Unlock()
	})

	// ── metrics (cluster-scoped, via the dynamic client). Usage is stored as a
	// name→usage map and joined with node capacity AFTER the wait so it never
	// depends on the nodes goroutine's ordering.
	spawn(func() {
		dyn, _, derr := p.dynamicClient()
		if derr != nil {
			return // dynamic client unavailable → metrics unavailable
		}
		l, err := dyn.Resource(nodeMetricsGVR).List(ctx, lo)
		if err != nil {
			return // group not registered / metrics-server absent → unavailable
		}
		usage := make(map[string]nodeUsage, len(l.Items))
		for i := range l.Items {
			it := &l.Items[i]
			raw, found, _ := unstructured.NestedStringMap(it.Object, "usage")
			if !found {
				continue
			}
			var u nodeUsage
			if q, e := resource.ParseQuantity(raw["cpu"]); e == nil {
				u.cpu = q.AsApproximateFloat64()
			}
			if q, e := resource.ParseQuantity(raw["memory"]); e == nil {
				u.mem = q.Value()
			}
			usage[it.GetName()] = u
		}
		mu.Lock()
		grp.usage = usage
		grp.metricsAvailable = true
		mu.Unlock()
	})

	wg.Wait()

	// Join node usage (metrics) with node capacity (Node list) into the metrics
	// block. Only populated when metrics-server answered; otherwise the block
	// stays {available:false} with zeroed numbers.
	if grp.metricsAvailable {
		ov.Metrics.Available = true
		out := make([]OverviewNodeMetrics, 0, len(grp.usage))
		seen := map[string]bool{}
		if grp.nodeList != nil {
			for i := range grp.nodeList.Items {
				n := &grp.nodeList.Items[i]
				nm := OverviewNodeMetrics{
					Name:             n.Name,
					CPUUsedCores:     grp.usage[n.Name].cpu,
					CPUCapacityCores: n.Status.Capacity.Cpu().AsApproximateFloat64(),
					MemUsedBytes:     grp.usage[n.Name].mem,
					MemCapacityBytes: n.Status.Capacity.Memory().Value(),
				}
				out = append(out, nm)
				seen[n.Name] = true
			}
		}
		// A node the metrics API reports but the Node list didn't (e.g. RBAC on
		// nodes) still contributes its usage, with unknown (zero) capacity.
		for name, u := range grp.usage {
			if seen[name] {
				continue
			}
			out = append(out, OverviewNodeMetrics{Name: name, CPUUsedCores: u.cpu, MemUsedBytes: u.mem})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		for _, nm := range out {
			ov.Metrics.CPUUsedCores += nm.CPUUsedCores
			ov.Metrics.CPUCapacityCores += nm.CPUCapacityCores
			ov.Metrics.MemUsedBytes += nm.MemUsedBytes
			ov.Metrics.MemCapacityBytes += nm.MemCapacityBytes
		}
		ov.Metrics.Nodes = out
	}

	return &ov, nil
}

// overviewShared holds the cross-goroutine intermediate values the metrics join
// needs after the wait: the raw Node list (for capacity) and the parsed usage
// map, plus whether the metrics API answered at all. Guarded by the Overview
// mutex while goroutines run; read single-threaded after wg.Wait.
type overviewShared struct {
	nodeList         *corev1.NodeList
	usage            map[string]nodeUsage
	metricsAvailable bool
}
