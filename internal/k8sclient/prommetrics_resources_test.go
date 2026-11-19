package k8sclient

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func ctr(req, lim corev1.ResourceList) corev1.Container {
	return corev1.Container{Resources: corev1.ResourceRequirements{Requests: req, Limits: lim}}
}

func rl(cpu, mem string) corev1.ResourceList {
	l := corev1.ResourceList{}
	if cpu != "" {
		l[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if mem != "" {
		l[corev1.ResourceMemory] = resource.MustParse(mem)
	}
	return l
}

func TestPodResources(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
		ctr(rl("250m", "64Mi"), rl("500m", "128Mi")),
		ctr(rl("100m", ""), rl("", "64Mi")),
	}}}
	r := podResources(pod)
	if *r.CPURequest != 350 || *r.MemRequest != 64*1024*1024 {
		t.Fatalf("requests: %v %v", *r.CPURequest, *r.MemRequest)
	}
	if r.CPULimit != nil {
		t.Fatal("cpu limit must be nil when a container is unbounded")
	}
	if *r.MemLimit != 192*1024*1024 {
		t.Fatalf("mem limit %v", *r.MemLimit)
	}
	if podResources(&corev1.Pod{}) != nil {
		t.Fatal("no containers → nil")
	}
}
