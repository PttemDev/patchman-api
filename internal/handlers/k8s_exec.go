package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"patchman-be/internal/k8sclient"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"k8s.io/client-go/tools/remotecommand"
)

// k8sExecUpgrader upgrades the exec request to a WebSocket. Auth already ran via
// the JWT middleware (with the ?token= query fallback the browser WebSocket API
// needs), and access(c,true) gates the write grant below — so CheckOrigin can be
// permissive here (the app connects same-origin through the reachable BE).
var k8sExecUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// exec keepalive/deadline tuning. The stream traverses the sidecar proxy, so we
// keep a generous read deadline refreshed by pong replies and ping on a ticker.
// An idle shell must survive — do NOT let a short inbound timeout kill it.
const (
	execWriteWait  = 10 * time.Second
	execPongWait   = 90 * time.Second
	execPingPeriod = 30 * time.Second
	execStdinBuf   = 32
)

// execInbound is a frame the FE sends us: stdin data or a terminal resize.
type execInbound struct {
	Type string `json:"type"` // "stdin" | "resize"
	Data string `json:"data"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// execOutbound is a frame we send the FE: stdout/stderr bytes or a terminal exit.
type execOutbound struct {
	Type  string `json:"type"` // "stdout" | "stderr" | "exit"
	Data  string `json:"data,omitempty"`
	Code  int    `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

// wsWriter serializes all writes to the WebSocket behind a mutex — gorilla
// forbids concurrent writers, and stdout, stderr, ping and the exit frame all
// race to write.
type wsWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (w *wsWriter) writeJSON(v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(execWriteWait))
	return w.conn.WriteJSON(v)
}

func (w *wsWriter) ping() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(execWriteWait))
	return w.conn.WriteMessage(websocket.PingMessage, nil)
}

// streamWriter adapts a stream type ("stdout"/"stderr") to an io.Writer that
// marshals each chunk into an outbound frame.
type streamWriter struct {
	w      *wsWriter
	stream string
}

func (s *streamWriter) Write(p []byte) (int, error) {
	if err := s.w.writeJSON(execOutbound{Type: s.stream, Data: string(p)}); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Exec bridges a browser WebSocket to an interactive pod-exec stream.
// GET /k8s/:id/exec?context=&namespace=&pod=&container=&tty=&command=...
//
// It requires the write grant (access(c,true)); exec is a mutating capability.
// Framing is OUR protocol (see execInbound/execOutbound), not raw
// channel.k8s.io — the FE speaks {type:stdin|resize} and hears
// {type:stdout|stderr|exit}. The pod stream is torn down when the socket closes
// (ctx cancel) so nothing leaks.
func (h *K8sOpsHandler) Exec(c *gin.Context) {
	conn, role, ok := h.access(c, true)
	if !ok {
		return
	}
	namespace := c.Query("namespace")
	pod := c.Query("pod")
	if strings.TrimSpace(pod) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "pod is required"})
		return
	}
	contextName := c.Query("context")
	container := c.Query("container")
	tty := c.Query("tty") == "true" || c.Query("tty") == "1"

	// command may arrive as repeated ?command= params or fall back to a shell.
	command := c.QueryArray("command")
	if len(command) == 0 {
		command = []string{"/bin/sh"}
	}

	// Audit the exec START (before the long-lived stream). Errors during the
	// stream are surfaced to the client via the exit frame, not re-audited.
	h.audit(c, conn, role, "exec", contextName, namespace, "pod/"+pod, nil)

	wsConn, err := k8sExecUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		// gorilla already wrote the HTTP error.
		return
	}
	defer wsConn.Close()

	writer := &wsWriter{conn: wsConn}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// stdin pipe fed by the reader goroutine; resize channel drives the TSQ.
	stdinR, stdinW := io.Pipe()
	resizeCh := make(chan remotecommand.TerminalSize, execStdinBuf)

	// Read deadline + pong handler keep the idle shell alive across the proxy.
	_ = wsConn.SetReadDeadline(time.Now().Add(execPongWait))
	wsConn.SetPongHandler(func(string) error {
		return wsConn.SetReadDeadline(time.Now().Add(execPongWait))
	})

	// Ping ticker (single-writer via wsWriter mutex).
	pingDone := make(chan struct{})
	go func() {
		t := time.NewTicker(execPingPeriod)
		defer t.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if err := writer.ping(); err != nil {
					return
				}
			}
		}
	}()

	// Reader goroutine: WS frames → stdin pipe / resize channel. On any read
	// error (client closed, timeout) it cancels the stream context.
	go func() {
		defer func() {
			cancel()
			_ = stdinW.Close()
		}()
		for {
			_, msg, err := wsConn.ReadMessage()
			if err != nil {
				return
			}
			var in execInbound
			if json.Unmarshal(msg, &in) != nil {
				continue
			}
			switch in.Type {
			case "stdin":
				if _, werr := stdinW.Write([]byte(in.Data)); werr != nil {
					return
				}
			case "resize":
				select {
				case resizeCh <- remotecommand.TerminalSize{Width: in.Cols, Height: in.Rows}:
				default:
				}
			}
		}
	}()

	// Run the exec stream. Blocks until the process exits or ctx is cancelled.
	execErr := h.registry.Exec(ctx, *conn, contextName, k8sclient.ExecOptions{
		Namespace: namespace,
		Pod:       pod,
		Container: container,
		Command:   command,
		TTY:       tty,
		Stdin:     stdinR,
		Stdout:    &streamWriter{w: writer, stream: "stdout"},
		Stderr:    &streamWriter{w: writer, stream: "stderr"},
		Resize:    resizeCh,
	})

	close(pingDone)
	cancel()

	// Report the exit to the FE. A CodeExitError carries the process exit code.
	out := execOutbound{Type: "exit", Code: 0}
	if execErr != nil {
		out.Error = execErr.Error()
		if code, okc := exitCode(execErr); okc {
			out.Code = code
		} else {
			out.Code = 1
		}
	}
	_ = writer.writeJSON(out)
}

// exitCode extracts a shell exit code from a remotecommand exit error. The
// non-zero-exit case returns a k8s.io/client-go/util/exec.CodeExitError, which
// satisfies this ExitStatus() interface.
func exitCode(err error) (int, bool) {
	type coder interface{ ExitStatus() int }
	if ce, ok := err.(coder); ok {
		return ce.ExitStatus(), true
	}
	return 0, false
}
