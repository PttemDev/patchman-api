package k8sclient

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func ctrlRef(kind, name string, uid string) []metav1.OwnerReference {
	t := true
	return []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: kind, Name: name, UID: ktypes.UID(uid), Controller: &t}}
}

func TestReplicaSetRevisions(t *testing.T) {
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "ns", UID: "dep-uid"}}
	labels := map[string]string{"app": "api"}
	rs := func(name, rev, image string, replicas int32, owner string) *appsv1.ReplicaSet {
		return &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "ns", Labels: labels,
				Annotations:     map[string]string{"deployment.kubernetes.io/revision": rev},
				OwnerReferences: ctrlRef("Deployment", "api", owner),
			},
			Spec: appsv1.ReplicaSetSpec{
				Replicas: &replicas,
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: image}}}},
			},
		}
	}
	cs := fake.NewSimpleClientset(
		rs("api-aaa", "1", "api:1.0", 0, "dep-uid"),
		rs("api-bbb", "3", "api:1.2", 2, "dep-uid"),
		rs("api-ccc", "2", "api:1.1", 0, "dep-uid"),
		rs("other-x", "9", "other:9", 1, "someone-else"), // same labels, different owner
	)
	revs, errStr := replicaSetRevisions(context.Background(), cs.AppsV1().ReplicaSets("ns"), metav1.ListOptions{LabelSelector: "app=api"}, dep)
	if errStr != "" {
		t.Fatal(errStr)
	}
	if len(revs) != 3 {
		t.Fatalf("want 3 owned revisions, got %d (%+v)", len(revs), revs)
	}
	if revs[0].Revision != 3 || !revs[0].Current || revs[0].Images[0] != "api:1.2" || *revs[0].Desired != 2 {
		t.Fatalf("newest/current wrong: %+v", revs[0])
	}
	if revs[1].Revision != 2 || revs[1].Current || revs[2].Revision != 1 {
		t.Fatalf("order wrong: %d,%d", revs[1].Revision, revs[2].Revision)
	}
}

func TestControllerRevisions(t *testing.T) {
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns", UID: "sts-uid"}}
	cr := func(name string, rev int64, image string) *appsv1.ControllerRevision {
		return &appsv1.ControllerRevision{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{"app": "db"}, OwnerReferences: ctrlRef("StatefulSet", "db", "sts-uid")},
			Revision:   rev,
			Data:       runtime.RawExtension{Raw: []byte(`{"spec":{"template":{"spec":{"containers":[{"name":"c","image":"` + image + `"}]}}}}`)},
		}
	}
	cs := fake.NewSimpleClientset(cr("db-1", 1, "pg:15"), cr("db-2", 2, "pg:16"))
	// status.updateRevision points at db-1 (e.g. a rollback): it, not the max, is current.
	revs, errStr := controllerRevisions(context.Background(), cs.AppsV1().ControllerRevisions("ns"), metav1.ListOptions{LabelSelector: "app=db"}, sts, "db-1")
	if errStr != "" {
		t.Fatal(errStr)
	}
	if len(revs) != 2 || revs[0].Revision != 2 || revs[0].Current || !revs[1].Current || revs[1].Images[0] != "pg:15" {
		t.Fatalf("unexpected: %+v", revs)
	}
}
