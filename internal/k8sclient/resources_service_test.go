package k8sclient

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestServiceRowPortsAndExternalIPs(t *testing.T) {
	svc := &corev1.Service{
		Spec: corev1.ServiceSpec{
			Type:        corev1.ServiceTypeLoadBalancer,
			ClusterIP:   "10.0.0.7",
			ExternalIPs: []string{"192.168.1.9"},
			Ports: []corev1.ServicePort{
				{Port: 80, NodePort: 30080, Protocol: corev1.ProtocolTCP},
				{Port: 9090, Protocol: corev1.ProtocolUDP},
			},
		},
		Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{
			Ingress: []corev1.LoadBalancerIngress{{IP: "34.1.2.3"}, {Hostname: "lb.example.com"}},
		}},
	}
	row := serviceRow(svc)
	if got, want := row.Extra["ports"], "80:30080/TCP,9090/UDP"; got != want {
		t.Errorf("ports = %q, want %q", got, want)
	}
	if got, want := row.Extra["externalIP"], "34.1.2.3,lb.example.com,192.168.1.9"; got != want {
		t.Errorf("externalIP = %q, want %q", got, want)
	}
	plain := serviceRow(&corev1.Service{Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Ports: []corev1.ServicePort{{Port: 5432, Protocol: corev1.ProtocolTCP}}}})
	if _, ok := plain.Extra["externalIP"]; ok || plain.Extra["ports"] != "5432/TCP" {
		t.Errorf("cluster-ip service extra = %v", plain.Extra)
	}
}

func TestPodRowLastTermination(t *testing.T) {
	finished := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "app", Ready: true, RestartCount: 3,
			State:                corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137, FinishedAt: finished}},
		}}},
	}
	row := podRow(pod)
	if row.Status != "Running" || row.Restarts != 3 {
		t.Errorf("status=%s restarts=%d", row.Status, row.Restarts)
	}
	if got := row.Extra["lastTermination"]; got != "OOMKilled (exit 137) · 5m ago" {
		t.Errorf("lastTermination = %q", got)
	}
	// Several containers: the newest termination wins and names its container;
	// a clean Completed exit doesn't count.
	older := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{
		Name:                 "sidecar",
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1, FinishedAt: older}},
	})
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "init", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed", FinishedAt: metav1.Now()}}}}
	if got := podRow(pod).Extra["lastTermination"]; got != "app: OOMKilled (exit 137) · 5m ago" {
		t.Errorf("multi-container lastTermination = %q", got)
	}
	if _, ok := podRow(&corev1.Pod{}).Extra["lastTermination"]; ok {
		t.Error("a pod without terminations has no lastTermination")
	}
}
