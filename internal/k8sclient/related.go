package k8sclient

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"patchman-be/internal/models"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WorkloadRevision is one entry of a workload's rollout history: a ReplicaSet
// for a Deployment, a ControllerRevision for a StatefulSet / DaemonSet.
type WorkloadRevision struct {
	Revision    int64    `json:"revision"`
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`              // "ReplicaSet" | "ControllerRevision"
	Desired     *int32   `json:"desired,omitempty"` // ReplicaSet only
	Ready       *int32   `json:"ready,omitempty"`   // ReplicaSet only
	Images      []string `json:"images,omitempty"`
	ChangeCause string   `json:"changeCause,omitempty"`
	Age         string   `json:"age"`
	Current     bool     `json:"current"`
}

// WorkloadRelated is what a workload's detail view shows beside the object
// itself (Lens-style): the pods its selector matches and its revisions. Each
// part is best-effort — a forbidden ReplicaSet list doesn't hide the pods.
type WorkloadRelated struct {
	Pods              []ResourceRow      `json:"pods"`
	PodsErr           string             `json:"podsErr,omitempty"`
	SupportsRevisions bool               `json:"supportsRevisions"`
	Revisions         []WorkloadRevision `json:"revisions"`
	RevisionsErr      string             `json:"revisionsErr,omitempty"`
}

// RelatedKinds are the workload kinds Related understands.
var RelatedKinds = map[string]bool{"deployments": true, "statefulsets": true, "daemonsets": true, "jobs": true}

// Related returns the pods matched by a workload's selector plus, for
// Deployments / StatefulSets / DaemonSets, the revisions it owns.
func (r *Registry) Related(ctx context.Context, conn models.K8sConnection, contextName, namespace, kind, name string) (*WorkloadRelated, error) {
	k := normalizeKind(kind)
	if !RelatedKinds[k] {
		return nil, fmt.Errorf("related pods are not available for %q", kind)
	}
	if err := checkNamespace(conn, k, namespace); err != nil {
		return nil, err
	}
	p, err := r.resolve(conn, contextName)
	if err != nil {
		return nil, err
	}
	cs := p.clientset
	apps := cs.AppsV1()

	var (
		sel   *metav1.LabelSelector
		owner metav1.Object
		// statefulset: the revision name currently rolled out (status.updateRevision)
		currentRevisionName string
	)
	switch k {
	case "deployments":
		d, err := apps.Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		sel, owner = d.Spec.Selector, d
	case "statefulsets":
		s, err := apps.StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		sel, owner, currentRevisionName = s.Spec.Selector, s, s.Status.UpdateRevision
	case "daemonsets":
		ds, err := apps.DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		sel, owner = ds.Spec.Selector, ds
	case "jobs":
		j, err := cs.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		sel, owner = j.Spec.Selector, j
	}
	if sel == nil {
		return nil, fmt.Errorf("%s %q has no selector", kind, name)
	}
	selector, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return nil, fmt.Errorf("invalid selector: %w", err)
	}
	lo := metav1.ListOptions{LabelSelector: selector.String()}

	out := &WorkloadRelated{Pods: []ResourceRow{}, Revisions: []WorkloadRevision{}}

	// Pods: the selector is what the controller itself uses to adopt pods.
	if list, err := cs.CoreV1().Pods(namespace).List(ctx, lo); err != nil {
		out.PodsErr = err.Error()
	} else {
		for i := range list.Items {
			out.Pods = append(out.Pods, podRow(&list.Items[i]))
		}
		sort.Slice(out.Pods, func(i, j int) bool { return out.Pods[i].Name < out.Pods[j].Name })
	}

	switch k {
	case "deployments":
		out.SupportsRevisions = true
		out.Revisions, out.RevisionsErr = replicaSetRevisions(ctx, apps.ReplicaSets(namespace), lo, owner)
	case "statefulsets", "daemonsets":
		out.SupportsRevisions = true
		out.Revisions, out.RevisionsErr = controllerRevisions(ctx, apps.ControllerRevisions(namespace), lo, owner, currentRevisionName)
	}
	return out, nil
}

type rsLister interface {
	List(ctx context.Context, opts metav1.ListOptions) (*appsv1.ReplicaSetList, error)
}

type crLister interface {
	List(ctx context.Context, opts metav1.ListOptions) (*appsv1.ControllerRevisionList, error)
}

// replicaSetRevisions lists a Deployment's ReplicaSets (rollout history),
// newest first. The highest revision is the current one (kubectl rollout
// history semantics).
func replicaSetRevisions(ctx context.Context, rsl rsLister, lo metav1.ListOptions, owner metav1.Object) ([]WorkloadRevision, string) {
	list, err := rsl.List(ctx, lo)
	if err != nil {
		return []WorkloadRevision{}, err.Error()
	}
	revs := []WorkloadRevision{}
	for i := range list.Items {
		rs := &list.Items[i]
		if !metav1.IsControlledBy(rs, owner) {
			continue
		}
		rev, _ := strconv.ParseInt(rs.Annotations["deployment.kubernetes.io/revision"], 10, 64)
		desired := int32(0)
		if rs.Spec.Replicas != nil {
			desired = *rs.Spec.Replicas
		}
		ready := rs.Status.ReadyReplicas
		images := make([]string, 0, len(rs.Spec.Template.Spec.Containers))
		for _, c := range rs.Spec.Template.Spec.Containers {
			images = append(images, c.Image)
		}
		revs = append(revs, WorkloadRevision{
			Revision: rev, Name: rs.Name, Kind: "ReplicaSet",
			Desired: &desired, Ready: &ready, Images: images,
			ChangeCause: rs.Annotations["kubernetes.io/change-cause"],
			Age:         age(rs.CreationTimestamp.Time),
		})
	}
	markCurrentByMax(revs)
	return revs, ""
}

// controllerRevisions lists a StatefulSet's / DaemonSet's ControllerRevisions,
// newest first. currentName (StatefulSet status.updateRevision) marks the
// current one when known; otherwise the highest revision is current.
func controllerRevisions(ctx context.Context, crl crLister, lo metav1.ListOptions, owner metav1.Object, currentName string) ([]WorkloadRevision, string) {
	list, err := crl.List(ctx, lo)
	if err != nil {
		return []WorkloadRevision{}, err.Error()
	}
	revs := []WorkloadRevision{}
	for i := range list.Items {
		cr := &list.Items[i]
		if !metav1.IsControlledBy(cr, owner) {
			continue
		}
		revs = append(revs, WorkloadRevision{
			Revision: cr.Revision, Name: cr.Name, Kind: "ControllerRevision",
			Images:      templateImages(cr.Data.Raw),
			ChangeCause: cr.Annotations["kubernetes.io/change-cause"],
			Age:         age(cr.CreationTimestamp.Time),
			Current:     currentName != "" && cr.Name == currentName,
		})
	}
	if currentName == "" {
		markCurrentByMax(revs)
	} else {
		sortRevisions(revs)
	}
	return revs, ""
}

// templateImages pulls container images out of a ControllerRevision's data —
// a (partial) object whose spec.template.spec.containers carries the images.
func templateImages(raw []byte) []string {
	var d struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Image string `json:"image"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &d) != nil {
		return nil
	}
	var out []string
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Image != "" {
			out = append(out, c.Image)
		}
	}
	return out
}

func sortRevisions(revs []WorkloadRevision) {
	sort.Slice(revs, func(i, j int) bool { return revs[i].Revision > revs[j].Revision })
}

func markCurrentByMax(revs []WorkloadRevision) {
	sortRevisions(revs)
	if len(revs) > 0 {
		revs[0].Current = true
	}
}
