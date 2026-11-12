package k8sclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"patchman-be/internal/models"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// pfReadyTimeout bounds how long Start waits for the SPDY tunnel + local
// listener to come up before giving up.
const pfReadyTimeout = 15 * time.Second

// activeForward is one live port-forward: a local 127.0.0.1 listener bridged
// over an SPDY tunnel to a pod port. It lives until Stop closes stopCh (or the
// forwarding goroutine dies on its own, e.g. the pod is deleted).
type activeForward struct {
	ID         string
	ConnID     string
	Namespace  string
	PodName    string
	TargetPort int
	LocalPort  int
	stopCh     chan struct{}
}

// ID accessors are exported struct fields, so the handler reads them directly.

// PFRegistry is a process-wide table of active port-forwards. It is
// embedded/desktop-local ONLY — the listeners bind on 127.0.0.1 of whatever
// machine the BE runs on, so this must never be reachable on a hosted BE (the
// handler enforces that gate). Forwards persist until stopped or the BE exits.
type PFRegistry struct {
	mu       sync.Mutex
	forwards map[string]*activeForward
}

// NewPFRegistry builds an empty registry.
func NewPFRegistry() *PFRegistry {
	return &PFRegistry{forwards: map[string]*activeForward{}}
}

// Start opens a port-forward from a local 127.0.0.1 port to targetPort on the
// given pod. localPort == 0 lets the OS auto-assign — the actual bound port is
// read back from fw.GetPorts() after the tunnel is ready. It builds the SPDY
// dialer against the pods/<pod>/portforward subresource, runs ForwardPorts in a
// goroutine, registers the forward on success, and deregisters when that
// goroutine exits (Stop, pod death, connection drop). The rest.Config already
// has ExecProvider forced nil (RCE guard) upstream in the registry.
func (pf *PFRegistry) Start(restCfg *rest.Config, connID, namespace, pod string, targetPort, localPort int) (*activeForward, error) {
	if strings.TrimSpace(namespace) == "" {
		return nil, fmt.Errorf("a namespace is required for port-forward")
	}
	if strings.TrimSpace(pod) == "" {
		return nil, fmt.Errorf("a target pod is required for port-forward")
	}
	if targetPort <= 0 {
		return nil, fmt.Errorf("a positive target port is required")
	}

	// A clientset gives us the correctly-versioned portforward subresource URL
	// (host + APIPath + auth transport) without hand-building it.
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("build clientset: %w", err)
	}
	transport, upgrader, err := spdy.RoundTripperFor(restCfg)
	if err != nil {
		return nil, fmt.Errorf("build spdy transport: %w", err)
	}
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(namespace).
		Name(pod).
		SubResource("portforward")
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", req.URL())

	stopCh := make(chan struct{})
	readyCh := make(chan struct{})
	ports := []string{fmt.Sprintf("%d:%d", localPort, targetPort)}
	fw, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, ports, stopCh, readyCh, io.Discard, io.Discard)
	if err != nil {
		return nil, fmt.Errorf("build port forwarder: %w", err)
	}

	// ForwardPorts blocks until stopCh closes (or it errors); the buffered errCh
	// captures the terminal error whether it fires before or after ready.
	errCh := make(chan error, 1)
	go func() { errCh <- fw.ForwardPorts() }()

	select {
	case <-readyCh:
		// tunnel up + listener bound
	case err := <-errCh:
		if err == nil {
			err = fmt.Errorf("port-forward exited before it became ready")
		}
		return nil, err
	case <-time.After(pfReadyTimeout):
		close(stopCh)
		return nil, fmt.Errorf("timed out waiting for port-forward to become ready")
	}

	fwPorts, err := fw.GetPorts()
	if err != nil {
		close(stopCh)
		return nil, fmt.Errorf("read forwarded ports: %w", err)
	}
	if len(fwPorts) == 0 {
		close(stopCh)
		return nil, fmt.Errorf("port-forward reported no bound ports")
	}
	actualLocal := int(fwPorts[0].Local)

	af := &activeForward{
		ID:         uuid.NewString(),
		ConnID:     connID,
		Namespace:  namespace,
		PodName:    pod,
		TargetPort: targetPort,
		LocalPort:  actualLocal,
		stopCh:     stopCh,
	}
	pf.mu.Lock()
	pf.forwards[af.ID] = af
	pf.mu.Unlock()

	// Deregister once the forwarding goroutine returns (Stop closed stopCh, the
	// pod went away, or the connection dropped). delete is idempotent w.r.t. an
	// explicit Stop that already removed the entry.
	go func() {
		<-errCh
		pf.mu.Lock()
		delete(pf.forwards, af.ID)
		pf.mu.Unlock()
	}()

	return af, nil
}

// Stop tears down the forward with the given id (closing its stopCh, which
// unblocks ForwardPorts and triggers deregistration). Returns false if no such
// forward is live. stopCh is closed exactly once because the id is removed from
// the map under the lock before the close, so a second Stop finds nothing.
func (pf *PFRegistry) Stop(id string) bool {
	pf.mu.Lock()
	af, ok := pf.forwards[id]
	if ok {
		delete(pf.forwards, id)
	}
	pf.mu.Unlock()
	if !ok {
		return false
	}
	close(af.stopCh)
	return true
}

// Get returns the forward for id (for the DELETE owner check) without stopping.
func (pf *PFRegistry) Get(id string) (*activeForward, bool) {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	af, ok := pf.forwards[id]
	return af, ok
}

// List returns the active forwards for one connection.
func (pf *PFRegistry) List(connID string) []*activeForward {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	out := make([]*activeForward, 0)
	for _, af := range pf.forwards {
		if af.ConnID == connID {
			out = append(out, af)
		}
	}
	return out
}

// StopAll closes every live forward. Best-effort shutdown hook.
func (pf *PFRegistry) StopAll() {
	pf.mu.Lock()
	forwards := pf.forwards
	pf.forwards = map[string]*activeForward{}
	pf.mu.Unlock()
	for _, af := range forwards {
		close(af.stopCh)
	}
}

// PortForwardTarget resolves a {kind,name,targetPort} request to the concrete
// {rest.Config, pod, containerPort} the forwarder needs. For kind=pod it passes
// the pod + port straight through; for kind=service it looks up the Service,
// picks the matching ServicePort, resolves its targetPort (int or named) to a
// container port, and selects one Ready backing pod (mirroring kubectl). The
// namespace is subject to the connection's allowedNamespaces guard.
func (r *Registry) PortForwardTarget(ctx context.Context, conn models.K8sConnection, contextName, namespace, kind, name string, targetPort int) (*rest.Config, string, int, error) {
	if err := checkNamespace(conn, "pods", namespace); err != nil {
		return nil, "", 0, err
	}
	p, err := r.resolve(conn, contextName)
	if err != nil {
		return nil, "", 0, err
	}
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "", "pod", "pods":
		if targetPort <= 0 {
			return nil, "", 0, fmt.Errorf("targetPort is required for a pod port-forward")
		}
		return p.rest, name, targetPort, nil
	case "service", "services", "svc":
		pod, port, err := r.resolveServicePod(ctx, p, namespace, name, targetPort)
		if err != nil {
			return nil, "", 0, err
		}
		return p.rest, pod, port, nil
	default:
		return nil, "", 0, fmt.Errorf("unsupported kind %q for port-forward (use pod or service)", kind)
	}
}

// resolveServicePod maps a Service + requested port to a Ready backing pod and
// the container port to forward to. requestedPort matches against the Service's
// .spec.ports[].port; 0 selects the first port. A Service with no selector
// (headless/ExternalName) can't be forwarded — the caller must target a pod.
func (r *Registry) resolveServicePod(ctx context.Context, p *pooled, namespace, serviceName string, requestedPort int) (string, int, error) {
	if strings.TrimSpace(serviceName) == "" {
		return "", 0, fmt.Errorf("a service name is required")
	}
	svc, err := p.clientset.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("get service: %w", err)
	}
	if len(svc.Spec.Selector) == 0 {
		return "", 0, fmt.Errorf("service %q has no selector (headless/ExternalName); port-forward a pod directly", serviceName)
	}

	var chosen *corev1.ServicePort
	for i := range svc.Spec.Ports {
		if requestedPort == 0 || int(svc.Spec.Ports[i].Port) == requestedPort {
			chosen = &svc.Spec.Ports[i]
			break
		}
	}
	if chosen == nil {
		return "", 0, fmt.Errorf("service %q has no port %d", serviceName, requestedPort)
	}

	sel := labels.SelectorFromSet(svc.Spec.Selector).String()
	pods, err := p.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return "", 0, fmt.Errorf("list backing pods: %w", err)
	}
	pod := pickReadyPod(pods.Items)
	if pod == nil {
		return "", 0, fmt.Errorf("no Ready pod backing service %q", serviceName)
	}

	containerPort, err := resolveTargetPort(pod, chosen.TargetPort, int(chosen.Port))
	if err != nil {
		return "", 0, err
	}
	return pod.Name, containerPort, nil
}

// pickReadyPod returns the first Running, non-terminating, Ready pod.
func pickReadyPod(pods []corev1.Pod) *corev1.Pod {
	for i := range pods {
		pd := &pods[i]
		if pd.Status.Phase != corev1.PodRunning || pd.DeletionTimestamp != nil {
			continue
		}
		for _, cond := range pd.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				return pd
			}
		}
	}
	return nil
}

// resolveTargetPort turns a ServicePort.TargetPort (int or named) into the pod
// container port. A named port is resolved against the pod's container ports;
// an unset target falls back to the service port.
func resolveTargetPort(pod *corev1.Pod, tp intstr.IntOrString, servicePort int) (int, error) {
	switch tp.Type {
	case intstr.Int:
		if tp.IntValue() != 0 {
			return tp.IntValue(), nil
		}
		return servicePort, nil
	case intstr.String:
		for _, c := range pod.Spec.Containers {
			for _, port := range c.Ports {
				if port.Name == tp.StrVal {
					return int(port.ContainerPort), nil
				}
			}
		}
		return 0, fmt.Errorf("named target port %q not found on backing pod %q", tp.StrVal, pod.Name)
	default:
		return servicePort, nil
	}
}
