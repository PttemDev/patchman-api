package k8sclient

import "encoding/json"

// ContextInfo is one kubeconfig context, parsed WITHOUT any cluster call. JSON
// field names are the FE wire contract.
type ContextInfo struct {
	Name      string `json:"name"`
	Cluster   string `json:"cluster"`
	Server    string `json:"server"`
	Namespace string `json:"namespace"`
}

// ClusterVersion is the Discovery ServerVersion probe result + reachability.
type ClusterVersion struct {
	Version   string `json:"version"`
	Platform  string `json:"platform"`
	Reachable bool   `json:"reachable"`
}

// ResourceRow is one normalized row of a List call. The common columns cover
// most kinds; `extra` carries kind-specific columns (e.g. service type/clusterIP,
// deployment replicas). Secret values are always masked — never present here.
type ResourceRow struct {
	Kind      string            `json:"kind"`
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Status    string            `json:"status"`
	Ready     string            `json:"ready"`
	Restarts  int               `json:"restarts"`
	Age       string            `json:"age"`
	Node      string            `json:"node"`
	Extra     map[string]string `json:"extra,omitempty"`
}

// ResourceDetail is a single object's full representation: canonical JSON plus
// pretty YAML (sigs.k8s.io/yaml). Secret `data` values are masked before both
// are produced.
type ResourceDetail struct {
	Kind      string          `json:"kind"`
	Namespace string          `json:"namespace"`
	Name      string          `json:"name"`
	JSON      json.RawMessage `json:"json"`
	YAML      string          `json:"yaml"`
}

// ApplyResult is the outcome of a Server-Side Apply (v2). `applied` is the
// object the server returned (nil-ish on a pure conflict), `diff` is a readable
// field-level current→applied diff, `conflicts` is the parsed list of
// field-manager conflicts from a 409 (only when force=false), and `dryRun`
// echoes whether the apply actually mutated the cluster.
type ApplyResult struct {
	Applied   json.RawMessage `json:"applied"`
	Diff      string          `json:"diff"`
	Conflicts []string        `json:"conflicts"`
	DryRun    bool            `json:"dryRun"`
}

// AccessCheck is one SelfSubjectAccessReview request — the tuple the FE wants to
// preflight before showing a write action. Empty group means the core API group.
type AccessCheck struct {
	Verb      string `json:"verb"`
	Group     string `json:"group"`
	Resource  string `json:"resource"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// ClusterOverview is the aggregated Lens-style dashboard payload for a cluster.
// It is assembled by fanning out one typed List per kind (plus the metrics API
// via the dynamic client) and tolerating partial failures — any section that
// fails its list simply stays at its zero value. Every json tag here is the FE
// wire contract.
type ClusterOverview struct {
	Version    ClusterVersion     `json:"version"`
	Nodes      OverviewNodes      `json:"nodes"`
	Pods       OverviewPods       `json:"pods"`
	Workloads  OverviewWorkloads  `json:"workloads"`
	Networking OverviewNetworking `json:"networking"`
	Namespaces int                `json:"namespaces"`
	Events     OverviewEvents     `json:"events"`
	Metrics    OverviewMetrics    `json:"metrics"`
}

// OverviewNodes counts nodes by readiness (from Node Ready conditions).
type OverviewNodes struct {
	Total    int `json:"total"`
	Ready    int `json:"ready"`
	NotReady int `json:"notReady"`
}

// OverviewPods is the pod total plus a breakdown by phase.
type OverviewPods struct {
	Total     int `json:"total"`
	Running   int `json:"running"`
	Pending   int `json:"pending"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Unknown   int `json:"unknown"`
}

// OverviewWorkloads counts each workload kind.
type OverviewWorkloads struct {
	Deployments  int `json:"deployments"`
	StatefulSets int `json:"statefulSets"`
	DaemonSets   int `json:"daemonSets"`
	Jobs         int `json:"jobs"`
	CronJobs     int `json:"cronJobs"`
	ReplicaSets  int `json:"replicaSets"`
}

// OverviewNetworking counts the networking + config kinds.
type OverviewNetworking struct {
	Services   int `json:"services"`
	Ingresses  int `json:"ingresses"`
	ConfigMaps int `json:"configMaps"`
	Secrets    int `json:"secrets"`
}

// OverviewEvents is the event total plus the count of Warning-type events.
type OverviewEvents struct {
	Total    int `json:"total"`
	Warnings int `json:"warnings"`
}

// OverviewMetrics is the cluster resource-usage summary from the metrics API
// (metrics.k8s.io/v1beta1 nodes) joined with node capacity from the Node list.
// Available is false when metrics-server is absent / the group is unregistered;
// callers gate on Available and ignore the (zero) numbers when it is false.
type OverviewMetrics struct {
	Available        bool                  `json:"available"`
	CPUUsedCores     float64               `json:"cpuUsedCores"`
	CPUCapacityCores float64               `json:"cpuCapacityCores"`
	MemUsedBytes     int64                 `json:"memUsedBytes"`
	MemCapacityBytes int64                 `json:"memCapacityBytes"`
	Nodes            []OverviewNodeMetrics `json:"nodes"`
}

// OverviewNodeMetrics is one node's usage (from the metrics API) joined with its
// capacity (from the Node list).
type OverviewNodeMetrics struct {
	Name             string  `json:"name"`
	CPUUsedCores     float64 `json:"cpuUsedCores"`
	CPUCapacityCores float64 `json:"cpuCapacityCores"`
	MemUsedBytes     int64   `json:"memUsedBytes"`
	MemCapacityBytes int64   `json:"memCapacityBytes"`
}

// PodMetricsResult is the live pod resource-usage snapshot from the metrics API
// (metrics.k8s.io/v1beta1 pods). Available is false when metrics-server is
// absent / not ready; callers gate on Available and show an install hint
// (Reason) instead of the (empty) Pods list. Every json tag is the FE wire
// contract.
type PodMetricsResult struct {
	Available bool         `json:"available"`
	Reason    string       `json:"reason,omitempty"`
	Pods      []PodMetrics `json:"pods"`
}

// PodMetrics is one pod's measured usage: the per-pod total (sum of containers)
// plus the per-container breakdown. CPU is millicores (e.g. 12), memory is
// bytes.
type PodMetrics struct {
	Namespace  string             `json:"namespace"`
	Name       string             `json:"name"`
	CPUMilli   int64              `json:"cpuMilli"`
	MemBytes   int64              `json:"memBytes"`
	Window     string             `json:"window,omitempty"`
	Containers []ContainerMetrics `json:"containers,omitempty"`
}

// ContainerMetrics is one container's measured CPU (millicores) + memory (bytes)
// usage.
type ContainerMetrics struct {
	Name     string `json:"name"`
	CPUMilli int64  `json:"cpuMilli"`
	MemBytes int64  `json:"memBytes"`
}
