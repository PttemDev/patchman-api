package k8sclient

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/models"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// maskedSecretValue is what every Secret data value is replaced with — v1 never
// exposes secret contents through List or Get.
const maskedSecretValue = "••••••••"

// clusterScopedKinds are the kinds that ignore the namespace filter.
var clusterScopedKinds = map[string]bool{
	"nodes":      true,
	"namespaces": true,
}

// namespaced reports whether a kind is namespace-scoped.
func namespaced(kind string) bool { return !clusterScopedKinds[normalizeKind(kind)] }

// normalizeKind maps any reasonable kind spelling — the canonical Kind ("Pod"),
// the plural ("pods"), any case, or a kubectl short name ("po", "deploy",
// "svc") — to the internal lowercase-plural switch key. This is what lets an
// MCP client / LLM pass the natural "Pod" instead of only "pods" (the raw
// strings.ToLower left "Pod"→"pod", which matched no case). Unknown kinds pass
// through lowercased so the switch default still reports them unsupported.
func normalizeKind(kind string) string {
	k := strings.ToLower(strings.TrimSpace(kind))
	switch k {
	case "pod", "pods", "po":
		return "pods"
	case "deployment", "deployments", "deploy":
		return "deployments"
	case "statefulset", "statefulsets", "sts":
		return "statefulsets"
	case "daemonset", "daemonsets", "ds":
		return "daemonsets"
	case "job", "jobs":
		return "jobs"
	case "cronjob", "cronjobs", "cj":
		return "cronjobs"
	case "service", "services", "svc":
		return "services"
	case "configmap", "configmaps", "cm":
		return "configmaps"
	case "secret", "secrets":
		return "secrets"
	case "ingress", "ingresses", "ing":
		return "ingresses"
	case "node", "nodes", "no":
		return "nodes"
	case "namespace", "namespaces", "ns":
		return "namespaces"
	case "event", "events", "ev":
		return "events"
	}
	return k
}

// checkNamespace enforces the allowedNamespaces option for namespaced kinds.
func checkNamespace(conn models.K8sConnection, kind, namespace string) error {
	if !namespaced(kind) {
		return nil
	}
	opt := parseOptions(conn.OptionsJSON)
	if len(opt.AllowedNamespaces) == 0 {
		return nil
	}
	ns := strings.TrimSpace(namespace)
	if ns == "" {
		return fmt.Errorf("a namespace is required for this connection (allowed: %s)", strings.Join(opt.AllowedNamespaces, ", "))
	}
	for _, a := range opt.AllowedNamespaces {
		if strings.TrimSpace(a) == ns {
			return nil
		}
	}
	return fmt.Errorf("namespace %q is not permitted on this connection", ns)
}

// List returns normalized rows for a kind. namespace "" lists across all
// namespaces for namespaced kinds (subject to allowedNamespaces).
func (r *Registry) List(ctx context.Context, conn models.K8sConnection, contextName, namespace, kind string) ([]ResourceRow, error) {
	rows, _, err := r.ListWithVersion(ctx, conn, contextName, namespace, kind)
	return rows, err
}

// ListWithVersion is List plus the list's resourceVersion — the consistent point
// a Watch resumes from so no events slip through the gap between the initial
// list and the watch. Every row is built by the SAME per-kind builder Watch
// uses (podRow, deploymentRow, …), so a synced row and a watched row for the
// same object are byte-identical.
func (r *Registry) ListWithVersion(ctx context.Context, conn models.K8sConnection, contextName, namespace, kind string) ([]ResourceRow, string, error) {
	k := normalizeKind(kind)
	if err := checkNamespace(conn, k, namespace); err != nil {
		return nil, "", err
	}
	p, err := r.resolve(conn, contextName)
	if err != nil {
		return nil, "", err
	}
	cs := p.clientset
	lo := metav1.ListOptions{}

	switch k {
	case "pods":
		list, err := cs.CoreV1().Pods(namespace).List(ctx, lo)
		if err != nil {
			return nil, "", err
		}
		rows := make([]ResourceRow, 0, len(list.Items))
		for i := range list.Items {
			rows = append(rows, podRow(&list.Items[i]))
		}
		return rows, list.ResourceVersion, nil
	case "deployments":
		list, err := cs.AppsV1().Deployments(namespace).List(ctx, lo)
		if err != nil {
			return nil, "", err
		}
		rows := make([]ResourceRow, 0, len(list.Items))
		for i := range list.Items {
			rows = append(rows, deploymentRow(&list.Items[i]))
		}
		return rows, list.ResourceVersion, nil
	case "statefulsets":
		list, err := cs.AppsV1().StatefulSets(namespace).List(ctx, lo)
		if err != nil {
			return nil, "", err
		}
		rows := make([]ResourceRow, 0, len(list.Items))
		for i := range list.Items {
			rows = append(rows, statefulSetRow(&list.Items[i]))
		}
		return rows, list.ResourceVersion, nil
	case "daemonsets":
		list, err := cs.AppsV1().DaemonSets(namespace).List(ctx, lo)
		if err != nil {
			return nil, "", err
		}
		rows := make([]ResourceRow, 0, len(list.Items))
		for i := range list.Items {
			rows = append(rows, daemonSetRow(&list.Items[i]))
		}
		return rows, list.ResourceVersion, nil
	case "jobs":
		list, err := cs.BatchV1().Jobs(namespace).List(ctx, lo)
		if err != nil {
			return nil, "", err
		}
		rows := make([]ResourceRow, 0, len(list.Items))
		for i := range list.Items {
			rows = append(rows, jobRow(&list.Items[i]))
		}
		return rows, list.ResourceVersion, nil
	case "cronjobs":
		list, err := cs.BatchV1().CronJobs(namespace).List(ctx, lo)
		if err != nil {
			return nil, "", err
		}
		rows := make([]ResourceRow, 0, len(list.Items))
		for i := range list.Items {
			rows = append(rows, cronJobRow(&list.Items[i]))
		}
		return rows, list.ResourceVersion, nil
	case "services":
		list, err := cs.CoreV1().Services(namespace).List(ctx, lo)
		if err != nil {
			return nil, "", err
		}
		rows := make([]ResourceRow, 0, len(list.Items))
		for i := range list.Items {
			rows = append(rows, serviceRow(&list.Items[i]))
		}
		return rows, list.ResourceVersion, nil
	case "configmaps":
		list, err := cs.CoreV1().ConfigMaps(namespace).List(ctx, lo)
		if err != nil {
			return nil, "", err
		}
		rows := make([]ResourceRow, 0, len(list.Items))
		for i := range list.Items {
			rows = append(rows, configMapRow(&list.Items[i]))
		}
		return rows, list.ResourceVersion, nil
	case "secrets":
		list, err := cs.CoreV1().Secrets(namespace).List(ctx, lo)
		if err != nil {
			return nil, "", err
		}
		rows := make([]ResourceRow, 0, len(list.Items))
		for i := range list.Items {
			rows = append(rows, secretRow(&list.Items[i]))
		}
		return rows, list.ResourceVersion, nil
	case "ingresses":
		list, err := cs.NetworkingV1().Ingresses(namespace).List(ctx, lo)
		if err != nil {
			return nil, "", err
		}
		rows := make([]ResourceRow, 0, len(list.Items))
		for i := range list.Items {
			rows = append(rows, ingressRow(&list.Items[i]))
		}
		return rows, list.ResourceVersion, nil
	case "nodes":
		list, err := cs.CoreV1().Nodes().List(ctx, lo)
		if err != nil {
			return nil, "", err
		}
		rows := make([]ResourceRow, 0, len(list.Items))
		for i := range list.Items {
			rows = append(rows, nodeRow(&list.Items[i]))
		}
		return rows, list.ResourceVersion, nil
	case "namespaces":
		list, err := cs.CoreV1().Namespaces().List(ctx, lo)
		if err != nil {
			return nil, "", err
		}
		rows := make([]ResourceRow, 0, len(list.Items))
		for i := range list.Items {
			rows = append(rows, namespaceRow(&list.Items[i]))
		}
		return rows, list.ResourceVersion, nil
	case "events":
		list, err := cs.CoreV1().Events(namespace).List(ctx, lo)
		if err != nil {
			return nil, "", err
		}
		rows := make([]ResourceRow, 0, len(list.Items))
		for i := range list.Items {
			rows = append(rows, eventRow(&list.Items[i]))
		}
		// Events read most-recent-first.
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
		return rows, list.ResourceVersion, nil
	default:
		return nil, "", fmt.Errorf("unsupported kind %q", kind)
	}
}

// Get returns a single object as JSON + YAML. Secret data values are masked.
func (r *Registry) Get(ctx context.Context, conn models.K8sConnection, contextName, namespace, kind, name string) (*ResourceDetail, error) {
	k := normalizeKind(kind)
	if err := checkNamespace(conn, k, namespace); err != nil {
		return nil, err
	}
	p, err := r.resolve(conn, contextName)
	if err != nil {
		return nil, err
	}
	cs := p.clientset
	go1 := metav1.GetOptions{}

	var obj any
	var gvk schema.GroupVersionKind
	switch k {
	case "pods":
		obj, err = cs.CoreV1().Pods(namespace).Get(ctx, name, go1)
		gvk = schema.GroupVersionKind{Version: "v1", Kind: "Pod"}
	case "deployments":
		obj, err = cs.AppsV1().Deployments(namespace).Get(ctx, name, go1)
		gvk = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	case "statefulsets":
		obj, err = cs.AppsV1().StatefulSets(namespace).Get(ctx, name, go1)
		gvk = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"}
	case "daemonsets":
		obj, err = cs.AppsV1().DaemonSets(namespace).Get(ctx, name, go1)
		gvk = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "DaemonSet"}
	case "jobs":
		obj, err = cs.BatchV1().Jobs(namespace).Get(ctx, name, go1)
		gvk = schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"}
	case "cronjobs":
		obj, err = cs.BatchV1().CronJobs(namespace).Get(ctx, name, go1)
		gvk = schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "CronJob"}
	case "services":
		obj, err = cs.CoreV1().Services(namespace).Get(ctx, name, go1)
		gvk = schema.GroupVersionKind{Version: "v1", Kind: "Service"}
	case "configmaps":
		obj, err = cs.CoreV1().ConfigMaps(namespace).Get(ctx, name, go1)
		gvk = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	case "secrets":
		s, e := cs.CoreV1().Secrets(namespace).Get(ctx, name, go1)
		if e == nil && s != nil {
			maskSecret(s)
		}
		obj, err = s, e
		gvk = schema.GroupVersionKind{Version: "v1", Kind: "Secret"}
	case "ingresses":
		obj, err = cs.NetworkingV1().Ingresses(namespace).Get(ctx, name, go1)
		gvk = schema.GroupVersionKind{Group: "networking.k8s.io", Version: "v1", Kind: "Ingress"}
	case "nodes":
		obj, err = cs.CoreV1().Nodes().Get(ctx, name, go1)
		gvk = schema.GroupVersionKind{Version: "v1", Kind: "Node"}
	case "namespaces":
		obj, err = cs.CoreV1().Namespaces().Get(ctx, name, go1)
		gvk = schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}
	case "events":
		obj, err = cs.CoreV1().Events(namespace).Get(ctx, name, go1)
		gvk = schema.GroupVersionKind{Version: "v1", Kind: "Event"}
	default:
		return nil, fmt.Errorf("unsupported kind %q", kind)
	}
	if err != nil {
		return nil, err
	}

	rawJSON, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal object: %w", err)
	}
	// Typed clients leave TypeMeta empty; inject the GVK so JSON/YAML carry
	// apiVersion + kind like a kubectl get -o yaml would.
	rawJSON = injectTypeMeta(rawJSON, gvk)

	yml, err := yaml.JSONToYAML(rawJSON)
	if err != nil {
		return nil, fmt.Errorf("marshal yaml: %w", err)
	}
	return &ResourceDetail{
		Kind: gvk.Kind, Namespace: namespace, Name: name,
		JSON: json.RawMessage(rawJSON), YAML: string(yml),
	}, nil
}

// injectTypeMeta sets apiVersion + kind on a marshaled object's top level when
// the typed client omitted them.
func injectTypeMeta(raw []byte, gvk schema.GroupVersionKind) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	if _, ok := m["kind"]; !ok || string(m["kind"]) == `""` {
		kb, _ := json.Marshal(gvk.Kind)
		m["kind"] = kb
	}
	if _, ok := m["apiVersion"]; !ok || string(m["apiVersion"]) == `""` {
		av := gvk.Version
		if gvk.Group != "" {
			av = gvk.Group + "/" + gvk.Version
		}
		ab, _ := json.Marshal(av)
		m["apiVersion"] = ab
	}
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// maskSecret replaces every Secret data value with the mask sentinel.
func maskSecret(s *corev1.Secret) {
	if s.Data != nil {
		for k := range s.Data {
			s.Data[k] = []byte(maskedSecretValue)
		}
	}
	if s.StringData != nil {
		for k := range s.StringData {
			s.StringData[k] = maskedSecretValue
		}
	}
}

// ── row helpers ─────────────────────────────────────────────────────

func podRow(pod *corev1.Pod) ResourceRow {
	total := len(pod.Spec.Containers)
	ready := 0
	restarts := 0
	node := pod.Spec.NodeName
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Ready {
			ready++
		}
		restarts += int(cs.RestartCount)
	}
	status := string(pod.Status.Phase)
	if pod.Status.Reason != "" {
		status = pod.Status.Reason
	}
	// A waiting/terminated container reason is more informative than the phase.
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			status = cs.State.Waiting.Reason
			break
		}
		if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" {
			status = cs.State.Terminated.Reason
			break
		}
	}
	row := ResourceRow{
		Kind: "Pod", Namespace: pod.Namespace, Name: pod.Name,
		Status: status, Ready: fmt.Sprintf("%d/%d", ready, total),
		Restarts: restarts, Node: node, Age: age(pod.CreationTimestamp.Time),
	}
	if lt := lastTermination(pod); lt != "" {
		row.Extra = map[string]string{"lastTermination": lt}
	}
	return row
}

// lastTermination describes the most recent container termination in the pod
// — "OOMKilled (exit 137) · 5m ago", prefixed with the container's name when
// the pod has several. A container that was OOM-killed and restarted shows
// Running again, so its lastState is the only place the reason survives.
func lastTermination(pod *corev1.Pod) string {
	var best *corev1.ContainerStateTerminated
	bestName := ""
	consider := func(name string, t *corev1.ContainerStateTerminated) {
		if t == nil || (t.Reason == "Completed" && t.ExitCode == 0) {
			return
		}
		if best == nil || t.FinishedAt.After(best.FinishedAt.Time) {
			best, bestName = t, name
		}
	}
	statuses := append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
	for i := range statuses {
		consider(statuses[i].Name, statuses[i].State.Terminated)
		consider(statuses[i].Name, statuses[i].LastTerminationState.Terminated)
	}
	if best == nil {
		return ""
	}
	reason := best.Reason
	if reason == "" {
		reason = "Terminated"
	}
	out := fmt.Sprintf("%s (exit %d)", reason, best.ExitCode)
	if len(statuses) > 1 {
		out = bestName + ": " + out
	}
	if !best.FinishedAt.IsZero() {
		out += " · " + age(best.FinishedAt.Time) + " ago"
	}
	return out
}

func deploymentRow(d *appsv1.Deployment) ResourceRow {
	return ResourceRow{
		Kind: "Deployment", Namespace: d.Namespace, Name: d.Name,
		Ready:  fmt.Sprintf("%d/%d", d.Status.ReadyReplicas, d.Status.Replicas),
		Status: deployStatus(d.Status.ReadyReplicas, d.Status.Replicas),
		Age:    age(d.CreationTimestamp.Time),
		Extra: map[string]string{
			"upToDate":  strconv.Itoa(int(d.Status.UpdatedReplicas)),
			"available": strconv.Itoa(int(d.Status.AvailableReplicas)),
		},
	}
}

func statefulSetRow(s *appsv1.StatefulSet) ResourceRow {
	return ResourceRow{
		Kind: "StatefulSet", Namespace: s.Namespace, Name: s.Name,
		Ready:  fmt.Sprintf("%d/%d", s.Status.ReadyReplicas, s.Status.Replicas),
		Status: deployStatus(s.Status.ReadyReplicas, s.Status.Replicas),
		Age:    age(s.CreationTimestamp.Time),
	}
}

func daemonSetRow(d *appsv1.DaemonSet) ResourceRow {
	return ResourceRow{
		Kind: "DaemonSet", Namespace: d.Namespace, Name: d.Name,
		Ready:  fmt.Sprintf("%d/%d", d.Status.NumberReady, d.Status.DesiredNumberScheduled),
		Status: deployStatus(d.Status.NumberReady, d.Status.DesiredNumberScheduled),
		Age:    age(d.CreationTimestamp.Time),
	}
}

func jobRow(j *batchv1.Job) ResourceRow {
	status := "Running"
	if j.Status.Succeeded > 0 {
		status = "Complete"
	} else if j.Status.Failed > 0 {
		status = "Failed"
	}
	comp := int32(1)
	if j.Spec.Completions != nil {
		comp = *j.Spec.Completions
	}
	return ResourceRow{
		Kind: "Job", Namespace: j.Namespace, Name: j.Name, Status: status,
		Ready: fmt.Sprintf("%d/%d", j.Status.Succeeded, comp),
		Age:   age(j.CreationTimestamp.Time),
	}
}

func cronJobRow(cj *batchv1.CronJob) ResourceRow {
	suspended := cj.Spec.Suspend != nil && *cj.Spec.Suspend
	last := ""
	if cj.Status.LastScheduleTime != nil {
		last = age(cj.Status.LastScheduleTime.Time)
	}
	status := "Active"
	if suspended {
		status = "Suspended"
	}
	return ResourceRow{
		Kind: "CronJob", Namespace: cj.Namespace, Name: cj.Name, Status: status,
		Age: age(cj.CreationTimestamp.Time),
		Extra: map[string]string{
			"schedule":     cj.Spec.Schedule,
			"lastSchedule": last,
			"active":       strconv.Itoa(len(cj.Status.Active)),
		},
	}
}

// serviceRow mirrors `kubectl get svc`: ports as port[:nodePort]/protocol
// (80:30080/TCP) and the external IPs — load balancer ingress plus
// spec.externalIPs.
func serviceRow(s *corev1.Service) ResourceRow {
	ports := make([]string, 0, len(s.Spec.Ports))
	for _, pt := range s.Spec.Ports {
		if pt.NodePort != 0 {
			ports = append(ports, fmt.Sprintf("%d:%d/%s", pt.Port, pt.NodePort, pt.Protocol))
		} else {
			ports = append(ports, fmt.Sprintf("%d/%s", pt.Port, pt.Protocol))
		}
	}
	var external []string
	for _, in := range s.Status.LoadBalancer.Ingress {
		if in.IP != "" {
			external = append(external, in.IP)
		} else if in.Hostname != "" {
			external = append(external, in.Hostname)
		}
	}
	external = append(external, s.Spec.ExternalIPs...)
	if s.Spec.Type == corev1.ServiceTypeExternalName && s.Spec.ExternalName != "" {
		external = append(external, s.Spec.ExternalName)
	}
	extra := map[string]string{
		"type":      string(s.Spec.Type),
		"clusterIP": s.Spec.ClusterIP,
		"ports":     strings.Join(ports, ","),
	}
	if len(external) > 0 {
		extra["externalIP"] = strings.Join(external, ",")
	}
	return ResourceRow{
		Kind: "Service", Namespace: s.Namespace, Name: s.Name,
		Status: string(s.Spec.Type), Age: age(s.CreationTimestamp.Time),
		Extra: extra,
	}
}

func configMapRow(cm *corev1.ConfigMap) ResourceRow {
	return ResourceRow{
		Kind: "ConfigMap", Namespace: cm.Namespace, Name: cm.Name,
		Age:   age(cm.CreationTimestamp.Time),
		Extra: map[string]string{"keys": strconv.Itoa(len(cm.Data))},
	}
}

func secretRow(s *corev1.Secret) ResourceRow {
	// Values are never surfaced — only the key count + type.
	return ResourceRow{
		Kind: "Secret", Namespace: s.Namespace, Name: s.Name,
		Status: string(s.Type), Age: age(s.CreationTimestamp.Time),
		Extra: map[string]string{
			"type": string(s.Type),
			"keys": strconv.Itoa(len(s.Data)),
		},
	}
}

func ingressRow(ing *networkingv1.Ingress) ResourceRow {
	hosts := make([]string, 0, len(ing.Spec.Rules))
	for _, ru := range ing.Spec.Rules {
		if ru.Host != "" {
			hosts = append(hosts, ru.Host)
		}
	}
	cls := ""
	if ing.Spec.IngressClassName != nil {
		cls = *ing.Spec.IngressClassName
	}
	return ResourceRow{
		Kind: "Ingress", Namespace: ing.Namespace, Name: ing.Name,
		Age: age(ing.CreationTimestamp.Time),
		Extra: map[string]string{
			"hosts": strings.Join(hosts, ","),
			"class": cls,
		},
	}
}

func nodeRow(n *corev1.Node) ResourceRow {
	return ResourceRow{
		Kind: "Node", Name: n.Name, Status: nodeStatus(n),
		Node: n.Name, Age: age(n.CreationTimestamp.Time),
		Extra: map[string]string{
			"roles":      nodeRoles(n),
			"version":    n.Status.NodeInfo.KubeletVersion,
			"os":         n.Status.NodeInfo.OSImage,
			"internalIP": nodeInternalIP(n),
		},
	}
}

func namespaceRow(ns *corev1.Namespace) ResourceRow {
	return ResourceRow{
		Kind: "Namespace", Name: ns.Name, Status: string(ns.Status.Phase),
		Age: age(ns.CreationTimestamp.Time),
	}
}

func eventRow(e *corev1.Event) ResourceRow {
	ts := e.LastTimestamp.Time
	if ts.IsZero() {
		ts = e.CreationTimestamp.Time
	}
	return ResourceRow{
		Kind: "Event", Namespace: e.Namespace, Name: e.Name,
		Status: e.Type, Age: age(ts),
		Extra: map[string]string{
			"reason":  e.Reason,
			"object":  e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name,
			"message": e.Message,
			"count":   strconv.Itoa(int(e.Count)),
		},
	}
}

func deployStatus(ready, desired int32) string {
	if desired == 0 {
		return "Scaled down"
	}
	if ready >= desired {
		return "Ready"
	}
	return "Progressing"
}

func nodeStatus(n *corev1.Node) string {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			if c.Status == corev1.ConditionTrue {
				return "Ready"
			}
			return "NotReady"
		}
	}
	return "Unknown"
}

func nodeRoles(n *corev1.Node) string {
	roles := []string{}
	for label := range n.Labels {
		const prefix = "node-role.kubernetes.io/"
		if strings.HasPrefix(label, prefix) {
			r := strings.TrimPrefix(label, prefix)
			if r == "" {
				r = "master"
			}
			roles = append(roles, r)
		}
	}
	sort.Strings(roles)
	if len(roles) == 0 {
		return "<none>"
	}
	return strings.Join(roles, ",")
}

func nodeInternalIP(n *corev1.Node) string {
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			return a.Address
		}
	}
	return ""
}

// age renders a compact human duration like kubectl's AGE column.
func age(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	}
}
