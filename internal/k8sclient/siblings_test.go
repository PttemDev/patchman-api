package k8sclient

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func sibPod(name string, lbl map[string]string, owner []metav1.OwnerReference, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: lbl, OwnerReferences: owner},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}, {Name: "sidecar"}}, InitContainers: []corev1.Container{{Name: "init"}}},
		Status:     corev1.PodStatus{Phase: phase},
	}
}

func sibNames(s *PodSiblings) []string {
	out := []string{}
	for _, p := range s.Pods {
		out = append(out, p.Name)
	}
	return out
}

func TestPodSiblings_DeploymentSpansReplicaSets(t *testing.T) {
	sel := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "ns", UID: "dep"}, Spec: appsv1.DeploymentSpec{Selector: sel}}
	rs := func(name, uid, owner string) *appsv1.ReplicaSet {
		return &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: ktypes.UID(uid), Labels: map[string]string{"app": "api"}, OwnerReferences: ctrlRef("Deployment", "api", owner)},
			Spec:       appsv1.ReplicaSetSpec{Selector: sel},
		}
	}
	lbl := map[string]string{"app": "api"}
	cs := fake.NewSimpleClientset(dep,
		rs("api-new", "rs-new", "dep"), rs("api-old", "rs-old", "dep"), rs("stray", "rs-stray", "other"),
		sibPod("api-new-1", lbl, ctrlRef("ReplicaSet", "api-new", "rs-new"), corev1.PodRunning),
		sibPod("api-new-2", lbl, ctrlRef("ReplicaSet", "api-new", "rs-new"), corev1.PodPending),
		sibPod("api-old-1", lbl, ctrlRef("ReplicaSet", "api-old", "rs-old"), corev1.PodRunning),
		sibPod("stray-1", lbl, ctrlRef("ReplicaSet", "stray", "rs-stray"), corev1.PodRunning), // same labels, other owner
		sibPod("bare", lbl, nil, corev1.PodRunning),                                           // same labels, no owner
	)
	s, err := podSiblings(context.Background(), cs, "ns", "api-new-2")
	if err != nil {
		t.Fatal(err)
	}
	if s.MatchedBy != "owner" || s.Owner == nil || s.Owner.Kind != "Deployment" || s.Owner.Name != "api" {
		t.Fatalf("owner: %+v %+v", s.MatchedBy, s.Owner)
	}
	got := sibNames(s)
	if len(got) != 3 || got[0] != "api-new-1" || got[1] != "api-new-2" || got[2] != "api-old-1" {
		t.Fatalf("pods %v", got)
	}
	cur := 0
	for _, p := range s.Pods {
		if p.Current {
			cur++
			if p.Name != "api-new-2" || p.Status != "Pending" {
				t.Fatalf("current row wrong: %+v", p)
			}
		}
	}
	if cur != 1 {
		t.Fatalf("want exactly one current, got %d", cur)
	}
	if c := s.Pods[0].Containers; len(c) != 3 || c[0] != "app" || c[2] != "init" {
		t.Fatalf("containers %v", c)
	}
}

func TestPodSiblings_StatefulSet(t *testing.T) {
	sel := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns", UID: "sts"}, Spec: appsv1.StatefulSetSpec{Selector: sel}}
	lbl := map[string]string{"app": "db"}
	ref := func(uid string) []metav1.OwnerReference { return ctrlRef("StatefulSet", "db", uid) }
	cs := fake.NewSimpleClientset(sts,
		sibPod("db-0", lbl, ref("sts"), corev1.PodRunning),
		sibPod("db-1", lbl, ref("sts"), corev1.PodRunning),
		sibPod("db-x", lbl, ref("old-sts"), corev1.PodRunning), // a recreated StatefulSet's leftover
	)
	s, err := podSiblings(context.Background(), cs, "ns", "db-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := sibNames(s); len(got) != 2 || got[0] != "db-0" || got[1] != "db-1" || s.Owner.Kind != "StatefulSet" {
		t.Fatalf("pods %v owner %+v", got, s.Owner)
	}
}

// An unknown controller kind (e.g. a CRD operator) has no selector to read:
// the namespace is listed and the controller UID decides.
func TestPodSiblings_UnknownControllerByUID(t *testing.T) {
	ref := ctrlRef("Rollout", "web", "ro")
	cs := fake.NewSimpleClientset(
		sibPod("web-a", nil, ref, corev1.PodRunning),
		sibPod("web-b", nil, ref, corev1.PodRunning),
		sibPod("other", nil, ctrlRef("Rollout", "x", "ro2"), corev1.PodRunning),
	)
	s, err := podSiblings(context.Background(), cs, "ns", "web-a")
	if err != nil {
		t.Fatal(err)
	}
	if got := sibNames(s); len(got) != 2 || s.Owner.Kind != "Rollout" {
		t.Fatalf("pods %v owner %+v", got, s.Owner)
	}
}

func TestPodSiblings_LabelFallbackAndNone(t *testing.T) {
	cs := fake.NewSimpleClientset(
		sibPod("a", map[string]string{"app.kubernetes.io/name": "tool", "app": "x"}, nil, corev1.PodRunning),
		sibPod("b", map[string]string{"app.kubernetes.io/name": "tool"}, nil, corev1.PodRunning),
		sibPod("c", map[string]string{"app": "x"}, nil, corev1.PodRunning),
		sibPod("lonely", nil, nil, corev1.PodRunning),
	)
	s, err := podSiblings(context.Background(), cs, "ns", "a")
	if err != nil {
		t.Fatal(err)
	}
	if got := sibNames(s); s.MatchedBy != "labels" || s.Owner != nil || len(got) != 2 || got[1] != "b" {
		t.Fatalf("labels: %s %v", s.MatchedBy, got)
	}
	s, err = podSiblings(context.Background(), cs, "ns", "lonely")
	if err != nil {
		t.Fatal(err)
	}
	if s.MatchedBy != "none" || len(s.Pods) != 1 || !s.Pods[0].Current {
		t.Fatalf("none: %+v", s)
	}
	if _, err := podSiblings(context.Background(), cs, "ns", "missing"); err == nil {
		t.Fatal("missing pod: want error")
	}
}
