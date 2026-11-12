package k8sclient

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"time"

	"patchman-be/internal/models"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LogStreamOptions bounds a pod-log stream request.
type LogStreamOptions struct {
	Namespace  string
	Pod        string
	Container  string // "" = the pod's default container
	Follow     bool
	TailLines  int64 // <=0 means no tail limit
	Timestamps bool  // prepend the RFC3339Nano timestamp to each line (kubectl --timestamps)
	Previous   bool  // the previous (terminated) container instance — kubectl logs -p
	LimitBytes int64 // >0 caps the bytes read from the log (full-log dumps)
	// SinceTime resumes a stream: only lines at/after this instant. The k8s API
	// takes whole seconds, so it is truncated DOWN — the resumed stream overlaps
	// the previous one by up to a second (never a gap); the caller dedupes on
	// the line timestamps.
	SinceTime *time.Time
}

const (
	// logHeartbeatEvery is how often an idle follow stream gets an SSE comment
	// frame, so an idle-timeout hop (ingress / LB / proxy, often 60s) doesn't
	// silently cut a quiet stream. Comment frames carry no data — every SSE
	// consumer ignores them.
	logHeartbeatEvery = 15 * time.Second
	// maxLogLineBytes caps one emitted line. A longer line is emitted in pieces
	// (in order) instead of failing the whole stream the way a bufio.Scanner
	// token-too-long error did.
	maxLogLineBytes = 1 << 20
)

// StreamLogs opens a pod-log stream and copies it line-by-line to w as SSE
// `data:` events (see copyLogLines). It honors ctx cancellation (clientGone)
// so the upstream k8s stream is torn down the moment the FE disconnects — no
// goroutine leak. Mirrors the sse.go teardown contract.
//
// flush is the response flusher (e.g. gin's c.Writer.Flush); it may be nil.
func (r *Registry) StreamLogs(ctx context.Context, conn models.K8sConnection, contextName string, o LogStreamOptions, w io.Writer, flush func()) error {
	if err := checkNamespace(conn, "pods", o.Namespace); err != nil {
		return err
	}
	p, err := r.resolve(conn, contextName)
	if err != nil {
		return err
	}
	logOpts := &corev1.PodLogOptions{
		Container:  o.Container,
		Follow:     o.Follow,
		Timestamps: o.Timestamps,
		Previous:   o.Previous,
	}
	if o.LimitBytes > 0 {
		lb := o.LimitBytes
		logOpts.LimitBytes = &lb
	}
	if o.TailLines > 0 {
		tl := o.TailLines
		logOpts.TailLines = &tl
	}
	if o.SinceTime != nil && !o.SinceTime.IsZero() {
		st := metav1.NewTime(o.SinceTime.Truncate(time.Second))
		logOpts.SinceTime = &st
	}
	req := p.clientset.CoreV1().Pods(o.Namespace).GetLogs(o.Pod, logOpts)
	stream, err := req.Stream(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()

	var heartbeat time.Duration
	if o.Follow {
		heartbeat = logHeartbeatEvery
	}
	return copyLogLines(ctx, stream, w, flush, heartbeat)
}

// copyLogLines copies newline-delimited log text from src to w, one SSE
// `data:` frame per line, strictly in arrival order. A line split across reads
// is held until its newline (or EOF); a trailing "\r" is dropped; a line longer
// than maxLogLineBytes is emitted in consecutive pieces rather than lost.
//
// One goroutine reads, this one writes: w (gin's ResponseWriter) is never
// written concurrently, which is what lets the heartbeat (heartbeat > 0: one
// comment frame up front, then one per interval) share it. Flushes happen once the queue of ready lines is drained, so a
// 500-line tail burst is not 500 flushes.
//
// src is closed when ctx ends so a blocked follow read unblocks; the return is
// nil on a clean EOF or a cancelled ctx, the read error otherwise.
func copyLogLines(ctx context.Context, src io.ReadCloser, w io.Writer, flush func(), heartbeat time.Duration) error {
	lines := make(chan string, 256)
	readErr := make(chan error, 1)
	done := make(chan struct{})
	defer close(done)

	// Unblock a pending follow read the moment the client goes away.
	go func() {
		select {
		case <-ctx.Done():
			_ = src.Close()
		case <-done:
		}
	}()

	go func() {
		defer close(lines)
		br := bufio.NewReaderSize(src, 64*1024)
		var acc []byte
		emit := func() bool {
			s := string(acc)
			acc = acc[:0]
			select {
			case lines <- s:
				return true
			case <-done:
				return false
			}
		}
		for {
			frag, isPrefix, err := br.ReadLine()
			if err != nil {
				if len(acc) > 0 {
					emit()
				}
				if err != io.EOF {
					readErr <- err
				}
				return
			}
			acc = append(acc, frag...)
			if isPrefix && len(acc) < maxLogLineBytes {
				continue
			}
			if !emit() {
				return
			}
		}
	}()

	doFlush := func() {
		if flush != nil {
			flush()
		}
	}
	var tick <-chan time.Time
	if heartbeat > 0 {
		t := time.NewTicker(heartbeat)
		defer t.Stop()
		tick = t.C
		// One up front: tells the consumer this stream heartbeats, so it can
		// arm a stall watchdog right away (an older BE never sends any).
		fmt.Fprint(w, ": keepalive\n\n")
		doFlush()
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case l, ok := <-lines:
			if !ok {
				select {
				case err := <-readErr:
					if ctx.Err() == nil {
						return err
					}
				default:
				}
				return nil
			}
			fmt.Fprintf(w, "data: %s\n\n", l)
			if len(lines) == 0 {
				doFlush()
			}
		case <-tick:
			fmt.Fprint(w, ": keepalive\n\n")
			doFlush()
		}
	}
}
