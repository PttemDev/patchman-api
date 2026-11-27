package k8sclient

import (
	"context"
	"sort"

	"patchman-be/internal/models"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// PodSibling is one pod of the same owner as the pod whose logs are open: the
// usual pod row plus its container names (so the log viewer can keep the
// picked container when the sibling has it) and whether it is that pod.
type PodSibling struct {
	ResourceRow
	Containers []string `json:"containers"`
	Current    bool     `json:"current"`
}

// PodOwner names the workload the siblings share.
type PodOwner struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// PodSiblings is the log viewer's pod switcher: the pods sharing the open
// pod's owner. MatchedBy says how they were found:
//   - "owner": the same controller (ownerReferences). A ReplicaSet owned by a
//     Deployment widens to every ReplicaSet of that Deployment, so pods of an
//     in-progress rollout show up too; Owner is then the Deployment.
//   - "labels": the pod has no controller, so pods with the same
//     app.kubernetes.io/name (or app) label.
//   - "none": neither — only the pod itself.
//
// Pods always contains the open pod.
type PodSiblings struct {
	Owner     *PodOwner    `json:"owner"`
	MatchedBy string       `json:"matchedBy"`
	Pods      []PodSibling `json:"pods"`
}

// siblingLabelKeys are tried in order when a pod has no controller.
var siblingLabelKeys = []string{"app.kubernetes.io/name", "app"}

// PodSiblings returns the pods that share pod's owner (see PodSiblings type).
func (r *Registry) PodSiblings(ctx context.Context, conn models.K8sConnection, contextName, namespace, pod string) (*PodSiblings, error) {
	if err := checkNamespace(conn, "pods", namespace); err != nil {
		return nil, err
	}
	p, err := r.resolve(conn, contextName)
	if err != nil {
		return nil, err
	}
	return podSiblings(ctx, p.clientset, namespace, pod)
}

func podSiblings(ctx context.Context, cs kubernetes.Interface, namespace, name string) (*PodSiblings, error) {
	pod, err := cs.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	out := &PodSiblings{MatchedBy: "none", Pods: []PodSibling{}}
	var candidates []corev1.Pod
	keep := func(*corev1.Pod) bool { return true }

	if ctrl := metav1.GetControllerOf(pod); ctrl != nil {
		out.MatchedBy = "owner"
		out.Owner = &PodOwner{Kind: ctrl.Kind, Name: ctrl.Name}
		owners := map[ktypes.UID]bool{ctrl.UID: true}
		sel := ownerSelector(ctx, cs, namespace, ctrl, owners, out)
		lo := metav1.ListOptions{}
		if sel != nil {
			if s, err := metav1.LabelSelectorAsSelector(sel); err == nil {
				lo.LabelSelector = s.String()
			}
		}
		// Without a selector (an unknown controller kind, or the owner could
		// not be read) this lists the namespace; the UID filter keeps it exact.
		list, err := cs.CoreV1().Pods(namespace).List(ctx, lo)
		if err != nil {
			return nil, err
		}
		candidates = list.Items
		keep = func(p *corev1.Pod) bool {
			c := metav1.GetControllerOf(p)
			return c != nil && owners[c.UID]
		}
	} else {
		for _, k := range siblingLabelKeys {
			v := pod.Labels[k]
			if v == "" {
				continue
			}
			list, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: labels.SelectorFromSet(labels.Set{k: v}).String(),
			})
			if err != nil {
				return nil, err
			}
			out.MatchedBy = "labels"
			candidates = list.Items
			break
		}
	}

	seenSelf := false
	for i := range candidates {
		p := &candidates[i]
		if !keep(p) {
			continue
		}
		cur := p.Name == pod.Name
		seenSelf = seenSelf || cur
		out.Pods = append(out.Pods, siblingRow(p, cur))
	}
	if !seenSelf {
		out.Pods = append(out.Pods, siblingRow(pod, true))
	}
	sort.Slice(out.Pods, func(i, j int) bool { return out.Pods[i].Name < out.Pods[j].Name })
	return out, nil
}

// ownerSelector returns the label selector of the pod's controller (narrowing
// the pod list) and, for a Deployment's ReplicaSet, adds the Deployment's other
// ReplicaSets to owners and reports the Deployment as the owner. Best effort:
// any read error falls back to the direct controller (nil selector = list the
// namespace and filter by owner UID).
func ownerSelector(ctx context.Context, cs kubernetes.Interface, namespace string, ctrl *metav1.OwnerReference, owners map[ktypes.UID]bool, out *PodSiblings) *metav1.LabelSelector {
	apps := cs.AppsV1()
	switch ctrl.Kind {
	case "ReplicaSet":
		rs, err := apps.ReplicaSets(namespace).Get(ctx, ctrl.Name, metav1.GetOptions{})
		if err != nil {
			return nil
		}
		dref := metav1.GetControllerOf(rs)
		if dref == nil || dref.Kind != "Deployment" {
			return rs.Spec.Selector
		}
		dep, err := apps.Deployments(namespace).Get(ctx, dref.Name, metav1.GetOptions{})
		if err != nil || dep.Spec.Selector == nil {
			return rs.Spec.Selector
		}
		s, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
		if err != nil {
			return rs.Spec.Selector
		}
		rsl, err := apps.ReplicaSets(namespace).List(ctx, metav1.ListOptions{LabelSelector: s.String()})
		if err != nil {
			return rs.Spec.Selector
		}
		for i := range rsl.Items {
			if metav1.IsControlledBy(&rsl.Items[i], dep) {
				owners[rsl.Items[i].UID] = true
			}
		}
		out.Owner = &PodOwner{Kind: "Deployment", Name: dep.Name}
		return dep.Spec.Selector
	case "StatefulSet":
		if s, err := apps.StatefulSets(namespace).Get(ctx, ctrl.Name, metav1.GetOptions{}); err == nil {
			return s.Spec.Selector
		}
	case "DaemonSet":
		if d, err := apps.DaemonSets(namespace).Get(ctx, ctrl.Name, metav1.GetOptions{}); err == nil {
			return d.Spec.Selector
		}
	case "Job":
		if j, err := cs.BatchV1().Jobs(namespace).Get(ctx, ctrl.Name, metav1.GetOptions{}); err == nil {
			return j.Spec.Selector
		}
	}
	return nil
}

func siblingRow(p *corev1.Pod, current bool) PodSibling {
	names := make([]string, 0, len(p.Spec.Containers)+len(p.Spec.InitContainers))
	for _, c := range p.Spec.Containers {
		names = append(names, c.Name)
	}
	for _, c := range p.Spec.InitContainers {
		names = append(names, c.Name)
	}
	return PodSibling{ResourceRow: podRow(p), Containers: names, Current: current}
}
