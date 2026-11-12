package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/k8sclient"

	"github.com/gin-gonic/gin"
)

// watchMaxDuration caps a single watch stream. Client-go watches are torn down
// by the apiserver anyway (idle/timeout), and long-lived rows drift stale, so we
// proactively end the stream with a `reconnect` hint the FE acts on by re-listing
// + re-watching. Keeps the row set consistent without an unbounded connection.
const watchMaxDuration = 10 * time.Minute

// Watch streams live resource changes as Server-Set-Events. It mirrors the
// logs SSE contract in k8s_ops.go: chunked c.Writer + Flush, and clientGone
// (request-context cancellation) tears down the upstream k8s watch so nothing
// leaks when the FE disconnects.
//
// Frames:
//
//	event: sync      data: {"rows":[…ResourceRow],"resourceVersion":"123"}
//	event: added     data: {…ResourceRow}
//	event: modified  data: {…ResourceRow}
//	event: deleted   data: {…ResourceRow}
//	event: reconnect data: {"message":"…"}   ← stream ended; FE re-subscribes
//	event: error     data: {"message":"…"}
//
// GET /k8s/:id/watch?context=&namespace=&kind=
func (h *K8sOpsHandler) Watch(c *gin.Context) {
	conn, role, ok := h.access(c, false)
	if !ok {
		return
	}
	contextName := c.Query("context")
	namespace := c.Query("namespace")
	kind := c.Query("kind")
	if strings.TrimSpace(kind) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind is required"})
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	// Initial list → the `sync` frame (full rows + the resourceVersion the watch
	// resumes from). A short timeout bounds only the list, not the watch.
	listCtx, cancelList := context.WithTimeout(c.Request.Context(), 30*time.Second)
	rows, rv, err := h.registry.ListWithVersion(listCtx, *conn, contextName, namespace, kind)
	cancelList()
	h.audit(c, conn, role, "watch", contextName, namespace, kind, err)
	if err != nil {
		writeSSEEvent(c.Writer, "error", fmt.Sprintf(`{"message":%q}`, err.Error()))
		c.Writer.Flush()
		return
	}
	if rows == nil {
		rows = []k8sclient.ResourceRow{}
	}
	syncPayload, _ := json.Marshal(gin.H{"rows": rows, "resourceVersion": rv})
	writeSSEEvent(c.Writer, "sync", string(syncPayload))
	c.Writer.Flush()

	// Cap the watch; the parent (request) context still cancels first on
	// clientGone, so both teardown paths are honored.
	watchCtx, cancel := context.WithTimeout(c.Request.Context(), watchMaxDuration)
	defer cancel()

	emit := func(evType string, row k8sclient.ResourceRow) {
		b, mErr := json.Marshal(row)
		if mErr != nil {
			return
		}
		writeSSEEvent(c.Writer, evType, string(b))
		c.Writer.Flush()
	}

	werr := h.registry.Watch(watchCtx, *conn, contextName, namespace, kind, rv, emit)

	// If the client is gone, don't bother writing — the socket is dead.
	if c.Request.Context().Err() != nil {
		return
	}
	if werr != nil {
		writeSSEEvent(c.Writer, "error", fmt.Sprintf(`{"message":%q}`, werr.Error()))
		c.Writer.Flush()
	}
	// The watch ended (cap reached, apiserver closed it, or a watch.Error such as
	// 410 Gone). Tell the FE to re-list + re-watch.
	writeSSEEvent(c.Writer, "reconnect", `{"message":"watch ended; re-subscribe"}`)
	c.Writer.Flush()
}
