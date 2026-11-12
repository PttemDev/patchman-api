package k8sclient

import (
	"context"
	"io"

	"patchman-be/internal/models"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// ExecOptions bounds an interactive pod-exec request. resize carries terminal
// size changes from the WS bridge; nil is allowed for a non-tty stream.
type ExecOptions struct {
	Namespace string
	Pod       string
	Container string // "" = the pod's default container
	Command   []string
	TTY       bool

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Resize <-chan remotecommand.TerminalSize
}

// resizeQueue adapts a resize channel to the remotecommand.TerminalSizeQueue
// interface. Next blocks until a new size arrives (or the channel closes, in
// which case it returns nil to stop the size monitor).
type resizeQueue struct {
	ch <-chan remotecommand.TerminalSize
}

func (q *resizeQueue) Next() *remotecommand.TerminalSize {
	if q.ch == nil {
		return nil
	}
	size, ok := <-q.ch
	if !ok {
		return nil
	}
	return &size
}

// Exec opens an interactive exec stream into a pod container. It builds the
// exec subresource request, then uses a WebSocket-first executor with an SPDY
// fallback (k8s 1.31+ defaults to WebSocket; older API servers still need
// SPDY). StreamWithContext blocks until the remote process exits or ctx is
// cancelled (the WS bridge cancels on client disconnect). It respects the
// allowedNamespaces guard like every other op.
//
// exec-credential plugins are already stripped from p.rest (RCE guard in the
// registry), so this can never shell out to a credential helper.
func (r *Registry) Exec(ctx context.Context, conn models.K8sConnection, contextName string, o ExecOptions) error {
	if err := checkNamespace(conn, "pods", o.Namespace); err != nil {
		return err
	}
	p, err := r.resolve(conn, contextName)
	if err != nil {
		return err
	}

	req := p.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(o.Pod).
		Namespace(o.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: o.Container,
			Command:   o.Command,
			Stdin:     o.Stdin != nil,
			Stdout:    true,
			Stderr:    true,
			TTY:       o.TTY,
		}, scheme.ParameterCodec)

	wsExec, err := remotecommand.NewWebSocketExecutor(p.rest, "GET", req.URL().String())
	if err != nil {
		return err
	}
	spdyExec, err := remotecommand.NewSPDYExecutor(p.rest, "POST", req.URL())
	if err != nil {
		return err
	}
	exec, err := remotecommand.NewFallbackExecutor(wsExec, spdyExec, httpstream.IsUpgradeFailure)
	if err != nil {
		return err
	}

	var sizeQueue remotecommand.TerminalSizeQueue
	if o.TTY && o.Resize != nil {
		sizeQueue = &resizeQueue{ch: o.Resize}
	}

	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:             o.Stdin,
		Stdout:            o.Stdout,
		Stderr:            o.Stderr,
		Tty:               o.TTY,
		TerminalSizeQueue: sizeQueue,
	})
}
