package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/k8sclient"
	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// K8sOpsHandler serves the v1 READ-ONLY cluster surface: kubeconfig context
// listing (no cluster call), the ServerVersion reachability probe, resource
// list / get, and the pod-log SSE stream. It reuses the connection handler's
// access check so owner + grant holders are gated consistently, and every op
// writes a fire-and-forget audit row. Mirrors ESOpsHandler.
//
// There is NO write/mutate/exec/apply surface in v1 — access() is always called
// with needWrite=false. The write gate is kept as a parameter only so the v2
// surface can reuse it unchanged.
type K8sOpsHandler struct {
	db        *gorm.DB
	registry  *k8sclient.Registry
	connsHand *K8sConnectionHandler
	// embedded is true when the BE runs bundled inside the desktop app
	// (config.ModeEmbedded). It gates the local-shell handler, which spawns a
	// login shell on the host and must NEVER be reachable on a hosted/shared
	// server BE. It also gates the port-forward handlers, which bind a local
	// listener on the host machine.
	embedded bool
	// pf holds the process-wide table of live port-forwards (embedded-only).
	pf *k8sclient.PFRegistry
}

func NewK8sOpsHandler(db *gorm.DB, reg *k8sclient.Registry, connsHand *K8sConnectionHandler, embedded bool, pf *k8sclient.PFRegistry) *K8sOpsHandler {
	return &K8sOpsHandler{db: db, registry: reg, connsHand: connsHand, embedded: embedded, pf: pf}
}

// access resolves the connection + enforces the required role. v1 reads accept
// any access. Returns the effective role so audit can flag delegated actions.
func (h *K8sOpsHandler) access(c *gin.Context, needWrite bool) (*models.K8sConnection, string, bool) {
	userID := c.GetString("userID")
	conn, role, err := h.connsHand.loadAccessible(c.Param("id"), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return nil, "", false
	}
	if needWrite && role != "owner" && role != "write" {
		c.JSON(http.StatusForbidden, gin.H{"error": "This action requires write access on the connection."})
		return nil, "", false
	}
	return conn, role, true
}

// Contexts lists the kubeconfig contexts — no cluster call.
func (h *K8sOpsHandler) Contexts(c *gin.Context) {
	conn, role, ok := h.access(c, false)
	if !ok {
		return
	}
	list, err := h.registry.Contexts(*conn)
	h.audit(c, conn, role, "contexts", "", "", "", err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": list})
}

// Version probes the cluster ServerVersion (also the reachability check).
func (h *K8sOpsHandler) Version(c *gin.Context) {
	conn, role, ok := h.access(c, false)
	if !ok {
		return
	}
	contextName := c.Query("context")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	res, err := h.registry.Test(ctx, *conn, contextName)
	h.audit(c, conn, role, "version", contextName, "", "", err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

// Overview aggregates the Lens-style cluster dashboard: node/pod/workload/
// networking counts, namespaces, events, version, and (when metrics-server is
// present) cluster + per-node CPU/memory usage vs capacity. Cluster-wide read.
// GET /k8s/:id/overview?context=
func (h *K8sOpsHandler) Overview(c *gin.Context) {
	conn, role, ok := h.access(c, false)
	if !ok {
		return
	}
	contextName := c.Query("context")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	overview, err := h.registry.Overview(ctx, *conn, contextName)
	h.audit(c, conn, role, "overview", contextName, "", "", err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": overview})
}

// Resources lists rows for a kind. ?context=&namespace=&kind=
func (h *K8sOpsHandler) Resources(c *gin.Context) {
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
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	rows, err := h.registry.List(ctx, *conn, contextName, namespace, kind)
	h.audit(c, conn, role, "resources", contextName, namespace, kind, err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rows})
}

// PodMetrics returns live per-pod CPU (millicores) + memory (bytes) usage from
// the cluster's metrics-server. Namespaced when ?namespace= is set, cluster-wide
// otherwise. Degrades to {available:false, reason} (HTTP 200) when metrics-server
// isn't installed/ready instead of 502. Read access is enough.
// GET /k8s/:id/pod-metrics?context=&namespace=
func (h *K8sOpsHandler) PodMetrics(c *gin.Context) {
	conn, role, ok := h.access(c, false)
	if !ok {
		return
	}
	contextName := c.Query("context")
	namespace := c.Query("namespace")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	res, err := h.registry.PodMetrics(ctx, *conn, contextName, namespace)
	h.audit(c, conn, role, "pod-metrics", contextName, namespace, "", err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

// PodMetricsHistory returns HISTORICAL per-container CPU (millicores) + memory
// (bytes) for one pod from an in-cluster Prometheus, reached through the
// kube-apiserver service-proxy (no ingress / port-forward). Degrades to
// {available:false, reason} (HTTP 200) when no Prometheus is discoverable so the
// FE falls back to the metrics-server live snapshot; errors (RBAC on
// services/proxy, timeout, TLS, a bad explicit override) surface as 502. Read
// access is enough. GET /k8s/:id/pod-metrics-history?context=&namespace=&pod=&range=
func (h *K8sOpsHandler) PodMetricsHistory(c *gin.Context) {
	conn, role, ok := h.access(c, false)
	if !ok {
		return
	}
	contextName := c.Query("context")
	namespace := c.Query("namespace")
	pod := c.Query("pod")
	rangeKey := c.Query("range")
	if strings.TrimSpace(namespace) == "" || strings.TrimSpace(pod) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "namespace and pod are required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 45*time.Second)
	defer cancel()
	res, err := h.registry.PodMetricsHistory(ctx, *conn, contextName, namespace, pod, rangeKey)
	h.audit(c, conn, role, "pod-metrics-history", contextName, namespace, pod, err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

// Resource returns a single object as JSON + YAML. ?context=&namespace=&kind=&name=
func (h *K8sOpsHandler) Resource(c *gin.Context) {
	conn, role, ok := h.access(c, false)
	if !ok {
		return
	}
	contextName := c.Query("context")
	namespace := c.Query("namespace")
	kind := c.Query("kind")
	name := c.Query("name")
	if strings.TrimSpace(kind) == "" || strings.TrimSpace(name) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind and name are required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	detail, err := h.registry.Get(ctx, *conn, contextName, namespace, kind, name)
	h.audit(c, conn, role, "resource", contextName, namespace, kind+"/"+name, err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": detail})
}

// Related returns a workload's matched pods + revisions for its detail view.
// GET /k8s/:id/related?context=&namespace=&kind=&name=
func (h *K8sOpsHandler) Related(c *gin.Context) {
	conn, role, ok := h.access(c, false)
	if !ok {
		return
	}
	contextName := c.Query("context")
	namespace := c.Query("namespace")
	kind := c.Query("kind")
	name := c.Query("name")
	if strings.TrimSpace(kind) == "" || strings.TrimSpace(name) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind and name are required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	rel, err := h.registry.Related(ctx, *conn, contextName, namespace, kind, name)
	h.audit(c, conn, role, "related", contextName, namespace, kind+"/"+name, err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rel})
}

// Logs streams pod logs as Server-Sent Events. It mirrors sse.go: chunked
// c.Writer + Flush, and clientGone (request-context cancellation) tears down the
// upstream k8s stream so nothing leaks when the FE disconnects.
// GET /k8s/:id/logs?context=&namespace=&pod=&container=&follow=&tailLines=&timestamps=&previous=&sinceTime=
// tailLines=all reads the whole log (capped at fullLogLimitBytes) — the FE's
// "download full log"; previous=true reads the last terminated container;
// sinceTime (RFC3339 / RFC3339Nano) resumes a dropped follow stream.
// fullLogLimitBytes caps a tailLines=all read so a chatty pod can't stream an
// unbounded log through the BE into the browser.
const fullLogLimitBytes = 50 << 20

// logOptionsFromQuery maps the /logs query string onto LogStreamOptions.
func logOptionsFromQuery(q url.Values) (k8sclient.LogStreamOptions, error) {
	flag := func(k string) bool { return q.Get(k) == "true" || q.Get(k) == "1" }
	opts := k8sclient.LogStreamOptions{
		Namespace:  q.Get("namespace"),
		Pod:        q.Get("pod"),
		Container:  q.Get("container"),
		Follow:     flag("follow"),
		Timestamps: flag("timestamps"),
		Previous:   flag("previous"),
	}
	if strings.TrimSpace(opts.Pod) == "" {
		return opts, fmt.Errorf("pod is required")
	}
	if q.Get("tailLines") == "all" {
		opts.TailLines = 0                  // no tail limit…
		opts.LimitBytes = fullLogLimitBytes // …but never an unbounded read
	} else if tl, err := strconv.ParseInt(q.Get("tailLines"), 10, 64); err == nil && tl > 0 {
		opts.TailLines = tl
	} else {
		opts.TailLines = 500 // sane default when unspecified
	}
	if s := strings.TrimSpace(q.Get("sinceTime")); s != "" {
		st, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return opts, fmt.Errorf("sinceTime must be RFC3339: %w", err)
		}
		opts.SinceTime = &st
	}
	return opts, nil
}

func (h *K8sOpsHandler) Logs(c *gin.Context) {
	conn, role, ok := h.access(c, false)
	if !ok {
		return
	}
	opts, err := logOptionsFromQuery(c.Request.URL.Query())
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	namespace, pod := opts.Namespace, opts.Pod
	contextName := c.Query("context")

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	writeSSEEvent(c.Writer, "connected", fmt.Sprintf(`{"pod":"%s","namespace":"%s"}`, pod, namespace))
	c.Writer.Flush()

	// No overall timeout — a follow stream lives until the client disconnects
	// (request context cancels) or the pod stops producing output.
	err = h.registry.StreamLogs(c.Request.Context(), *conn, contextName, opts, c.Writer, c.Writer.Flush)
	h.audit(c, conn, role, "logs", contextName, namespace, "pod/"+pod, err)
	if err != nil {
		writeSSEEvent(c.Writer, "error", fmt.Sprintf(`{"message":%q}`, err.Error()))
		c.Writer.Flush()
		return
	}
	writeSSEEvent(c.Writer, "done", `{"message":"Stream ended"}`)
	c.Writer.Flush()
}

// audit writes a best-effort activity row. DelegatedOwner is set to the
// connection owner when the actor is a grant holder (role != owner).
func (h *K8sOpsHandler) audit(c *gin.Context, conn *models.K8sConnection, role, action, context, namespace, name string, actErr error) {
	userID := c.GetString("userID")
	var delegated *string
	if role != "owner" {
		owner := conn.UserID
		delegated = &owner
	}
	errText := ""
	if actErr != nil {
		errText = actErr.Error()
	}
	kind := ""
	if i := strings.Index(name, "/"); i >= 0 {
		kind = name[:i]
	}
	row := models.K8sActivityHistory{
		ID: uuid.New().String(), UserID: userID, DelegatedOwner: delegated,
		ConnectionID: conn.ID, Action: action, Context: context,
		Namespace: namespace, Kind: kind, Name: name, ErrorText: errText,
		ExecutedAt: time.Now(),
	}
	go func() { _ = h.db.Create(&row).Error }()
}
