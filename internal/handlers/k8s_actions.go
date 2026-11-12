package handlers

import (
	"context"
	"net/http"
	"time"

	"patchman-be/internal/k8sclient"

	"github.com/gin-gonic/gin"
)

// This file carries the v2 WRITE surface (apply / scale / rollout-restart /
// delete / cordon / drain / cronjob-trigger) plus the read-only can-i preflight.
// Every mutating endpoint calls access(c,true) (write grant required) and audits
// the specific action + target. Timestamps for rollout-restart / cronjob-trigger
// are passed IN by the FE — nothing here calls time.Now() for a stamp.

const actionTimeout = 30 * time.Second

// ── apply ───────────────────────────────────────────────────────────

type applyInput struct {
	Context string `json:"context"`
	YAML    string `json:"yaml" binding:"required"`
	DryRun  bool   `json:"dryRun"`
	Force   bool   `json:"force"`
}

// Apply performs a Server-Side Apply. POST /k8s/:id/apply
func (h *K8sOpsHandler) Apply(c *gin.Context) {
	conn, role, ok := h.access(c, true)
	if !ok {
		return
	}
	var in applyInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), actionTimeout)
	defer cancel()
	res, err := h.registry.Apply(ctx, *conn, in.Context, []byte(in.YAML), in.DryRun, in.Force)
	action := "apply"
	if in.DryRun {
		action = "apply-dryrun"
	}
	h.audit(c, conn, role, action, in.Context, "", "", err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

// ── scale ───────────────────────────────────────────────────────────

type scaleInput struct {
	Context   string `json:"context"`
	Kind      string `json:"kind" binding:"required"`
	Namespace string `json:"namespace"`
	Name      string `json:"name" binding:"required"`
	Replicas  int32  `json:"replicas"`
}

// Scale sets a workload's replica count. POST /k8s/:id/scale
func (h *K8sOpsHandler) Scale(c *gin.Context) {
	conn, role, ok := h.access(c, true)
	if !ok {
		return
	}
	var in scaleInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), actionTimeout)
	defer cancel()
	err := h.registry.Scale(ctx, *conn, in.Context, in.Kind, in.Namespace, in.Name, in.Replicas)
	h.audit(c, conn, role, "scale", in.Context, in.Namespace, in.Kind+"/"+in.Name, err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"scaled": true, "replicas": in.Replicas}})
}

// ── rollout restart ─────────────────────────────────────────────────

type rolloutRestartInput struct {
	Context   string `json:"context"`
	Kind      string `json:"kind" binding:"required"`
	Namespace string `json:"namespace"`
	Name      string `json:"name" binding:"required"`
	Stamp     string `json:"stamp" binding:"required"` // FE-supplied RFC3339 timestamp
}

// RolloutRestart triggers a rolling restart. POST /k8s/:id/rollout-restart
func (h *K8sOpsHandler) RolloutRestart(c *gin.Context) {
	conn, role, ok := h.access(c, true)
	if !ok {
		return
	}
	var in rolloutRestartInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), actionTimeout)
	defer cancel()
	err := h.registry.RolloutRestart(ctx, *conn, in.Context, in.Kind, in.Namespace, in.Name, in.Stamp)
	h.audit(c, conn, role, "rollout-restart", in.Context, in.Namespace, in.Kind+"/"+in.Name, err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"restarted": true}})
}

// ── delete ──────────────────────────────────────────────────────────

type deleteInput struct {
	Context   string `json:"context"`
	Kind      string `json:"kind" binding:"required"`
	Namespace string `json:"namespace"`
	Name      string `json:"name" binding:"required"`
}

// DeleteResource deletes any mapped resource. DELETE /k8s/:id/resource
func (h *K8sOpsHandler) DeleteResource(c *gin.Context) {
	conn, role, ok := h.access(c, true)
	if !ok {
		return
	}
	var in deleteInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), actionTimeout)
	defer cancel()
	err := h.registry.DeleteResource(ctx, *conn, in.Context, in.Kind, in.Namespace, in.Name)
	h.audit(c, conn, role, "delete", in.Context, in.Namespace, in.Kind+"/"+in.Name, err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"deleted": true}})
}

// ── cordon / uncordon ───────────────────────────────────────────────

type cordonInput struct {
	Context string `json:"context"`
	Node    string `json:"node" binding:"required"`
	On      bool   `json:"on"`
}

// Cordon toggles a node's schedulability. POST /k8s/:id/cordon
func (h *K8sOpsHandler) Cordon(c *gin.Context) {
	conn, role, ok := h.access(c, true)
	if !ok {
		return
	}
	var in cordonInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), actionTimeout)
	defer cancel()
	err := h.registry.Cordon(ctx, *conn, in.Context, in.Node, in.On)
	action := "cordon"
	if !in.On {
		action = "uncordon"
	}
	h.audit(c, conn, role, action, in.Context, "", "node/"+in.Node, err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"cordoned": in.On}})
}

// ── drain ───────────────────────────────────────────────────────────

type drainInput struct {
	Context string `json:"context"`
	Node    string `json:"node" binding:"required"`
}

// Drain cordons + evicts eligible pods off a node. POST /k8s/:id/drain
func (h *K8sOpsHandler) Drain(c *gin.Context) {
	conn, role, ok := h.access(c, true)
	if !ok {
		return
	}
	var in drainInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Eviction of many pods can outlast the default action timeout — give it room.
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Minute)
	defer cancel()
	evicted, err := h.registry.Drain(ctx, *conn, in.Context, in.Node)
	h.audit(c, conn, role, "drain", in.Context, "", "node/"+in.Node, err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"evicted": evicted}})
}

// ── cronjob trigger ─────────────────────────────────────────────────

type cronjobTriggerInput struct {
	Context   string `json:"context"`
	Namespace string `json:"namespace"`
	Name      string `json:"name" binding:"required"`
	Stamp     string `json:"stamp" binding:"required"` // FE-supplied job-name suffix
}

// CronjobTrigger launches a CronJob as a one-off Job. POST /k8s/:id/cronjob/trigger
func (h *K8sOpsHandler) CronjobTrigger(c *gin.Context) {
	conn, role, ok := h.access(c, true)
	if !ok {
		return
	}
	var in cronjobTriggerInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), actionTimeout)
	defer cancel()
	jobName, err := h.registry.CronjobTrigger(ctx, *conn, in.Context, in.Namespace, in.Name, in.Stamp)
	h.audit(c, conn, role, "cronjob-trigger", in.Context, in.Namespace, "cronjob/"+in.Name, err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"job": jobName}})
}

// ── can-i (read; RBAC preflight) ────────────────────────────────────

type canIInput struct {
	Context string                  `json:"context"`
	Checks  []k8sclient.AccessCheck `json:"checks"`
}

// CanI runs SelfSubjectAccessReviews to gate the UI. POST /k8s/:id/can-i
// This is a READ operation (it only asks "may I") — access(c,false).
func (h *K8sOpsHandler) CanI(c *gin.Context) {
	conn, role, ok := h.access(c, false)
	if !ok {
		return
	}
	var in canIInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(in.Checks) == 0 {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"results": []bool{}}})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), actionTimeout)
	defer cancel()
	results, err := h.registry.CanI(ctx, *conn, in.Context, in.Checks)
	h.audit(c, conn, role, "can-i", in.Context, "", "", err)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"results": results}})
}
