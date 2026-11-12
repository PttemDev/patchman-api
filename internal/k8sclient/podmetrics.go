package k8sclient

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"patchman-be/internal/models"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
)

// metricsUnavailableReason is the typed signal the FE renders as an install
// hint when metrics-server is absent / not ready (a 404 or 503 from the
// aggregated metrics API), as opposed to a genuine error (RBAC, timeout, TLS).
const metricsUnavailableReason = "metrics-server is not installed or not ready in this cluster"

// podMetricsList mirrors the metrics.k8s.io/v1beta1 PodMetricsList JSON just
// enough to read per-container usage. Declaring it locally is deliberate: it
// lets us hit the raw REST path without vendoring the k8s.io/metrics clientset
// (no new module dependency). `usage.cpu` is a resource.Quantity like "12m" or
// "1"; `usage.memory` like "64Mi".
type podMetricsList struct {
	Items []podMetricsItem `json:"items"`
}

type podMetricsItem struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Timestamp  string `json:"timestamp"`
	Window     string `json:"window"`
	Containers []struct {
		Name  string `json:"name"`
		Usage struct {
			CPU    string `json:"cpu"`
			Memory string `json:"memory"`
		} `json:"usage"`
	} `json:"containers"`
}

// PodMetrics fetches live per-pod CPU + memory usage from the cluster's
// metrics-server via the RAW REST path (GET on the aggregated metrics API) —
// namespaced when a namespace is given, cluster-wide otherwise. It reuses the
// pooled, hardened rest.Config (same exec-plugin stripping / TLS options as
// every other read) and adds NO client dependency.
//
// Graceful degrade: a NotFound (metrics APIService not registered) or
// ServiceUnavailable (metrics-server not ready) is NOT an error — it returns
// {Available:false, Reason:…} so the FE shows an install hint. Any other error
// (RBAC Forbidden, timeout, TLS) propagates so the handler can surface it.
//
// CPU is normalized to millicores (int64, e.g. "12m"→12, "1"→1000) via
// Quantity.MilliValue(); memory to bytes (int64) via Quantity.Value(). Per-pod
// totals sum the containers; per-container usage is returned too.
func (r *Registry) PodMetrics(ctx context.Context, conn models.K8sConnection, contextName, namespace string) (*PodMetricsResult, error) {
	// Enforce the same namespace scoping as List (a restricted connection with
	// an empty namespace is rejected identically).
	if err := checkNamespace(conn, "pods", namespace); err != nil {
		return nil, err
	}
	p, err := r.resolve(conn, contextName)
	if err != nil {
		return nil, err
	}

	// AbsPath is variadic and path-joins its segments, so no string building.
	req := p.clientset.Discovery().RESTClient().Get()
	ns := strings.TrimSpace(namespace)
	if ns != "" {
		req = req.AbsPath("apis", "metrics.k8s.io", "v1beta1", "namespaces", ns, "pods")
	} else {
		req = req.AbsPath("apis", "metrics.k8s.io", "v1beta1", "pods")
	}
	raw, err := req.DoRaw(ctx)
	if err != nil {
		// 404 (APIService absent) / 503 (metrics-server not ready) → unavailable,
		// not an error. DoRaw returns a typed StatusError for both.
		if apierrors.IsNotFound(err) || apierrors.IsServiceUnavailable(err) {
			return &PodMetricsResult{Available: false, Reason: metricsUnavailableReason}, nil
		}
		return nil, err
	}

	var list podMetricsList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}

	pods := make([]PodMetrics, 0, len(list.Items))
	for i := range list.Items {
		it := &list.Items[i]
		pm := PodMetrics{
			Namespace:  it.Metadata.Namespace,
			Name:       it.Metadata.Name,
			Window:     it.Window,
			Containers: make([]ContainerMetrics, 0, len(it.Containers)),
		}
		for _, c := range it.Containers {
			cm := ContainerMetrics{Name: c.Name}
			// ParseQuantity (not MustParse) so an odd value skips instead of
			// panicking the whole request.
			if q, e := resource.ParseQuantity(c.Usage.CPU); e == nil {
				cm.CPUMilli = q.MilliValue()
			}
			if q, e := resource.ParseQuantity(c.Usage.Memory); e == nil {
				cm.MemBytes = q.Value()
			}
			pm.CPUMilli += cm.CPUMilli
			pm.MemBytes += cm.MemBytes
			pm.Containers = append(pm.Containers, cm)
		}
		pods = append(pods, pm)
	}
	sort.Slice(pods, func(i, j int) bool {
		if pods[i].Namespace != pods[j].Namespace {
			return pods[i].Namespace < pods[j].Namespace
		}
		return pods[i].Name < pods[j].Name
	})

	return &PodMetricsResult{Available: true, Pods: pods}, nil
}
