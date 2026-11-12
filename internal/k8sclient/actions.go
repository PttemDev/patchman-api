package k8sclient

import (
	"context"
	"fmt"
	"strings"

	"patchman-be/internal/models"

	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// resourceClient returns the dynamic ResourceInterface for a GVR, scoped to a
// namespace when the kind is namespaced. Shared by apply + the quick actions.
func resourceClient(dyn dynamic.Interface, gvr schema.GroupVersionResource, namespaced bool, ns string) dynamic.ResourceInterface {
	if namespaced {
		return dyn.Resource(gvr).Namespace(ns)
	}
	return dyn.Resource(gvr)
}

// scaleTarget maps a workload kind to its apps/v1 GVR. Scale is only meaningful
// for these three.
func scaleGVR(kind string) (schema.GroupVersionResource, error) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "deployment", "deployments":
		return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, nil
	case "statefulset", "statefulsets":
		return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}, nil
	case "replicaset", "replicasets":
		return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}, nil
	default:
		return schema.GroupVersionResource{}, fmt.Errorf("kind %q is not scalable", kind)
	}
}

// Scale sets the replica count on a Deployment / StatefulSet / ReplicaSet via
// its typed /scale subresource.
func (r *Registry) Scale(ctx context.Context, conn models.K8sConnection, contextName, kind, namespace, name string, replicas int32) error {
	if err := checkNamespace(conn, "generic", namespace); err != nil {
		return err
	}
	p, err := r.resolve(conn, contextName)
	if err != nil {
		return err
	}
	cs := p.clientset
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "deployment", "deployments":
		sc, err := cs.AppsV1().Deployments(namespace).GetScale(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		sc.Spec.Replicas = replicas
		_, err = cs.AppsV1().Deployments(namespace).UpdateScale(ctx, name, sc, metav1.UpdateOptions{})
		return err
	case "statefulset", "statefulsets":
		sc, err := cs.AppsV1().StatefulSets(namespace).GetScale(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		sc.Spec.Replicas = replicas
		_, err = cs.AppsV1().StatefulSets(namespace).UpdateScale(ctx, name, sc, metav1.UpdateOptions{})
		return err
	case "replicaset", "replicasets":
		sc, err := cs.AppsV1().ReplicaSets(namespace).GetScale(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		sc.Spec.Replicas = replicas
		_, err = cs.AppsV1().ReplicaSets(namespace).UpdateScale(ctx, name, sc, metav1.UpdateOptions{})
		return err
	default:
		return fmt.Errorf("kind %q is not scalable", kind)
	}
}

// RolloutRestart stamps the pod template's restartedAt annotation, which forces
// a rolling restart of the workload's pods (the kubectl rollout restart trick).
// The timestamp is passed in by the caller — scripts here never call time.Now().
func (r *Registry) RolloutRestart(ctx context.Context, conn models.K8sConnection, contextName, kind, namespace, name, stamp string) error {
	if err := checkNamespace(conn, "generic", namespace); err != nil {
		return err
	}
	gvr, err := scaleGVR(kind) // same three workload kinds carry a pod template
	if err != nil {
		// DaemonSets also support rollout restart; handle separately.
		if k := strings.ToLower(strings.TrimSpace(kind)); k == "daemonset" || k == "daemonsets" {
			gvr = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}
		} else {
			return err
		}
	}
	if strings.TrimSpace(stamp) == "" {
		return fmt.Errorf("a restart timestamp is required")
	}
	_, dyn, _, err := r.resolveDynamic(conn, contextName)
	if err != nil {
		return err
	}
	patch := fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`,
		stamp,
	)
	_, err = dyn.Resource(gvr).Namespace(namespace).Patch(
		ctx, name, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{FieldManager: fieldManager},
	)
	return err
}

// DeleteResource deletes any mapped resource by kind/name via the dynamic
// client + REST mapper (so it covers CRDs too, not just the built-in kinds).
func (r *Registry) DeleteResource(ctx context.Context, conn models.K8sConnection, contextName, kind, namespace, name string) error {
	_, dyn, mapper, err := r.resolveDynamic(conn, contextName)
	if err != nil {
		return err
	}
	mapping, err := mapper.RESTMapping(schema.GroupKind{Kind: singularKind(kind)})
	if err != nil {
		// Fall back to a plural-name lookup for the common built-in kinds.
		gvr, gerr := builtinGVR(kind)
		if gerr != nil {
			return fmt.Errorf("map %q: %w", kind, err)
		}
		namespaced := !clusterScopedKinds[strings.ToLower(kind)]
		if namespaced {
			if err := checkNamespace(conn, "generic", namespace); err != nil {
				return err
			}
		}
		return resourceClient(dyn, gvr, namespaced, namespace).Delete(ctx, name, metav1.DeleteOptions{})
	}
	namespaced := mapping.Scope.Name() == "namespace"
	if namespaced {
		if err := checkNamespace(conn, "generic", namespace); err != nil {
			return err
		}
	}
	return resourceClient(dyn, mapping.Resource, namespaced, namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

// Cordon toggles a node's spec.unschedulable. on=true cordons, on=false
// uncordons.
func (r *Registry) Cordon(ctx context.Context, conn models.K8sConnection, contextName, node string, on bool) error {
	p, err := r.resolve(conn, contextName)
	if err != nil {
		return err
	}
	patch := fmt.Sprintf(`{"spec":{"unschedulable":%t}}`, on)
	_, err = p.clientset.CoreV1().Nodes().Patch(
		ctx, node, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{FieldManager: fieldManager},
	)
	return err
}

// Drain cordons a node, then evicts every eligible pod on it via the policy/v1
// Eviction API. DaemonSet-owned pods and mirror (static) pods are skipped, as
// kubectl drain does. Returns the "namespace/name" of each pod an eviction was
// requested for.
func (r *Registry) Drain(ctx context.Context, conn models.K8sConnection, contextName, node string) ([]string, error) {
	if err := r.Cordon(ctx, conn, contextName, node, true); err != nil {
		return nil, err
	}
	p, err := r.resolve(conn, contextName)
	if err != nil {
		return nil, err
	}
	cs := p.clientset
	pods, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + node,
	})
	if err != nil {
		return nil, err
	}
	evicted := make([]string, 0, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		if isDaemonSetPod(pod) || isMirrorPod(pod) {
			continue
		}
		ev := &policyv1.Eviction{
			ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
		}
		if err := cs.PolicyV1().Evictions(pod.Namespace).Evict(ctx, ev); err != nil {
			// A PDB block (429/TooManyRequests) or transient error shouldn't abort
			// the whole drain; report what we could evict and surface the error
			// only if nothing was evicted.
			if apierrors.IsNotFound(err) {
				continue
			}
			if len(evicted) == 0 {
				return nil, fmt.Errorf("evict %s/%s: %w", pod.Namespace, pod.Name, err)
			}
			continue
		}
		evicted = append(evicted, pod.Namespace+"/"+pod.Name)
	}
	return evicted, nil
}

// CronjobTrigger manually launches a CronJob's jobTemplate as a one-off Job,
// mirroring `kubectl create job --from=cronjob/…`. The new Job's name is the
// cronjob name suffixed with the passed stamp (scripts never call time.Now()).
func (r *Registry) CronjobTrigger(ctx context.Context, conn models.K8sConnection, contextName, namespace, name, stamp string) (string, error) {
	if err := checkNamespace(conn, "generic", namespace); err != nil {
		return "", err
	}
	if strings.TrimSpace(stamp) == "" {
		return "", fmt.Errorf("a job-name stamp is required")
	}
	p, err := r.resolve(conn, contextName)
	if err != nil {
		return "", err
	}
	cs := p.clientset
	cj, err := cs.BatchV1().CronJobs(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	jobName := name + "-" + stamp
	if len(jobName) > 63 {
		jobName = jobName[:63]
	}
	tmpl := cj.Spec.JobTemplate
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        jobName,
			Namespace:   namespace,
			Labels:      tmpl.Labels,
			Annotations: tmpl.Annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1",
				Kind:       "CronJob",
				Name:       cj.Name,
				UID:        cj.UID,
			}},
		},
		Spec: tmpl.Spec,
	}
	created, err := cs.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{FieldManager: fieldManager})
	if err != nil {
		return "", err
	}
	return created.Name, nil
}

// CanI runs one SelfSubjectAccessReview per check and returns the allowed flags
// in the same order. A review error yields false for that entry (fail-closed)
// so the UI hides an action it can't confirm.
func (r *Registry) CanI(ctx context.Context, conn models.K8sConnection, contextName string, checks []AccessCheck) ([]bool, error) {
	p, err := r.resolve(conn, contextName)
	if err != nil {
		return nil, err
	}
	cs := p.clientset
	out := make([]bool, len(checks))
	for i, chk := range checks {
		ssar := &authorizationv1.SelfSubjectAccessReview{
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Verb:      chk.Verb,
					Group:     chk.Group,
					Resource:  chk.Resource,
					Namespace: chk.Namespace,
					Name:      chk.Name,
				},
			},
		}
		res, err := cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, ssar, metav1.CreateOptions{})
		if err != nil {
			out[i] = false
			continue
		}
		out[i] = res.Status.Allowed
	}
	return out, nil
}

// ── helpers ─────────────────────────────────────────────────────────

func isDaemonSetPod(pod *corev1.Pod) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}

func isMirrorPod(pod *corev1.Pod) bool {
	_, ok := pod.Annotations[corev1.MirrorPodAnnotationKey]
	return ok
}

// singularKind lowercases + de-pluralizes a kind hint so the REST mapper's
// GroupKind lookup (which wants the singular Kind, e.g. "Deployment") gets a
// reasonable input. The mapper itself is case/plural tolerant via RESTMapping.
func singularKind(kind string) string {
	k := strings.TrimSpace(kind)
	// Map the lowercase plural forms the FE sends to canonical singular Kinds.
	switch strings.ToLower(k) {
	case "pods", "pod":
		return "Pod"
	case "deployments", "deployment":
		return "Deployment"
	case "statefulsets", "statefulset":
		return "StatefulSet"
	case "daemonsets", "daemonset":
		return "DaemonSet"
	case "replicasets", "replicaset":
		return "ReplicaSet"
	case "jobs", "job":
		return "Job"
	case "cronjobs", "cronjob":
		return "CronJob"
	case "services", "service":
		return "Service"
	case "configmaps", "configmap":
		return "ConfigMap"
	case "secrets", "secret":
		return "Secret"
	case "ingresses", "ingress":
		return "Ingress"
	case "nodes", "node":
		return "Node"
	case "namespaces", "namespace":
		return "Namespace"
	case "events", "event":
		return "Event"
	}
	return k
}

// builtinGVR maps the FE's lowercase plural kind names to their GVR for the
// fallback delete path when the discovery mapper is unavailable.
func builtinGVR(kind string) (schema.GroupVersionResource, error) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "pods", "pod":
		return schema.GroupVersionResource{Version: "v1", Resource: "pods"}, nil
	case "deployments", "deployment":
		return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, nil
	case "statefulsets", "statefulset":
		return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}, nil
	case "daemonsets", "daemonset":
		return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}, nil
	case "replicasets", "replicaset":
		return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}, nil
	case "jobs", "job":
		return schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}, nil
	case "cronjobs", "cronjob":
		return schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}, nil
	case "services", "service":
		return schema.GroupVersionResource{Version: "v1", Resource: "services"}, nil
	case "configmaps", "configmap":
		return schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, nil
	case "secrets", "secret":
		return schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, nil
	case "ingresses", "ingress":
		return schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}, nil
	case "nodes", "node":
		return schema.GroupVersionResource{Version: "v1", Resource: "nodes"}, nil
	case "namespaces", "namespace":
		return schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}, nil
	default:
		return schema.GroupVersionResource{}, fmt.Errorf("unknown kind %q", kind)
	}
}
