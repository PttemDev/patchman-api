package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// portForwardRequest is the POST body for opening a forward. localPort omitted
// (0) means "let the OS pick a free port".
type portForwardRequest struct {
	Context    string `json:"context"`
	Namespace  string `json:"namespace"`
	Kind       string `json:"kind"` // "pod" | "service"
	Name       string `json:"name"`
	TargetPort int    `json:"targetPort"`
	LocalPort  int    `json:"localPort"`
}

// PortForwardStart opens a local 127.0.0.1 listener bridged to a pod/service
// port on the user's OWN machine (kubectl port-forward equivalent).
//
// POST /k8s/:id/port-forward
//
// SECURITY GATE (critical, identical to LocalShell): binding a local listener
// is only meaningful — and only safe — on the desktop app. A hosted/shared BE
// would bind the listener on the server, so the handler refuses with 403 unless
// the process runs embedded. The write grant (access(c,true)) is required on
// top, matching Exec/LocalShell.
func (h *K8sOpsHandler) PortForwardStart(c *gin.Context) {
	// Gate 1: embedded mode only.
	if !h.embedded {
		c.JSON(http.StatusForbidden, gin.H{"error": "port-forward is only available on the desktop app"})
		return
	}
	// Gate 2: write grant.
	conn, role, ok := h.access(c, true)
	if !ok {
		return
	}

	var req portForwardRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	if strings.TrimSpace(req.Namespace) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "namespace is required"})
		return
	}
	if req.TargetPort <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "targetPort is required"})
		return
	}

	// Resolve the target (service → Ready backing pod + container port when
	// needed) with a bounded context; the forward itself outlives this request.
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	target := req.Kind + "/" + req.Name
	restCfg, podName, resolvedPort, err := h.registry.PortForwardTarget(ctx, *conn, req.Context, req.Namespace, req.Kind, req.Name, req.TargetPort)
	if err != nil {
		h.audit(c, conn, role, "port_forward", req.Context, req.Namespace, target, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	af, err := h.pf.Start(restCfg, conn.ID, req.Namespace, podName, resolvedPort, req.LocalPort)
	h.audit(c, conn, role, "port_forward", req.Context, req.Namespace, target, err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"id":         af.ID,
		"localPort":  af.LocalPort,
		"targetPort": af.TargetPort,
		"namespace":  af.Namespace,
		"name":       req.Name,
		"pod":        af.PodName,
		"address":    fmt.Sprintf("127.0.0.1:%d", af.LocalPort),
	}})
}

// PortForwardList returns the active forwards for this connection.
// GET /k8s/:id/port-forwards
func (h *K8sOpsHandler) PortForwardList(c *gin.Context) {
	if !h.embedded {
		c.JSON(http.StatusForbidden, gin.H{"error": "port-forward is only available on the desktop app"})
		return
	}
	conn, _, ok := h.access(c, true)
	if !ok {
		return
	}
	forwards := h.pf.List(conn.ID)
	out := make([]gin.H, 0, len(forwards))
	for _, af := range forwards {
		out = append(out, gin.H{
			"id":         af.ID,
			"namespace":  af.Namespace,
			"name":       af.PodName,
			"pod":        af.PodName,
			"localPort":  af.LocalPort,
			"targetPort": af.TargetPort,
			"address":    fmt.Sprintf("127.0.0.1:%d", af.LocalPort),
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// PortForwardStop tears down a forward. The forward must belong to this
// connection (owner check) or it reads as not-found.
// DELETE /k8s/:id/port-forward/:pfId
func (h *K8sOpsHandler) PortForwardStop(c *gin.Context) {
	if !h.embedded {
		c.JSON(http.StatusForbidden, gin.H{"error": "port-forward is only available on the desktop app"})
		return
	}
	conn, role, ok := h.access(c, true)
	if !ok {
		return
	}
	pfID := c.Param("pfId")
	af, found := h.pf.Get(pfID)
	if !found || af.ConnID != conn.ID {
		c.JSON(http.StatusNotFound, gin.H{"error": "port-forward not found"})
		return
	}
	stopped := h.pf.Stop(pfID)
	h.audit(c, conn, role, "port_forward_stop", "", af.Namespace, "pod/"+af.PodName, nil)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": pfID, "stopped": stopped}})
}
