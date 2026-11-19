package handlers

// New Relic data on an observability connection (provider "newrelic"):
// NRQL queries, entity search and alert incidents, all through NerdGraph
// with the connection's stored user key — the key never leaves the backend,
// and a grant (read or write) is enough since everything here only reads.

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/apm"
	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
)

// newRelicClient loads an accessible New Relic connection and its client.
func (h *ObservabilityConnectionHandler) newRelicClient(c *gin.Context) (*apm.Client, *models.ObservabilityConnection, string, bool) {
	conn, role, err := h.loadAccessible(c.Param("id"), c.GetString("userID"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return nil, nil, "", false
	}
	if conn.Provider != "newrelic" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "This is not a New Relic connection"})
		return nil, nil, "", false
	}
	cl, err := h.registry.Client(*conn)
	if err != nil {
		obsFail(c, err)
		return nil, nil, "", false
	}
	return cl, conn, role, true
}

// NewRelicNRQL — POST /observability-connections/:id/newrelic/nrql
// {query, accountId?}: the result rows of one NRQL query.
func (h *ObservabilityConnectionHandler) NewRelicNRQL(c *gin.Context) {
	cl, conn, role, ok := h.newRelicClient(c)
	if !ok {
		return
	}
	var in struct {
		Query     string `json:"query"`
		AccountID string `json:"accountId"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || strings.TrimSpace(in.Query) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query is required"})
		return
	}
	account, err := apm.NewRelicAccount(cl, in.AccountID)
	if err != nil {
		obsFail(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 75*time.Second)
	defer cancel()
	started := time.Now()
	rows, err := apm.NewRelicNRQL(ctx, cl, account, in.Query)
	h.audit(c, conn, role, "newrelic_nrql", in.Query, err)
	if err != nil {
		obsFail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"accountId": account, "rows": rows, "count": len(rows), "elapsedMs": time.Since(started).Milliseconds()}})
}

// NewRelicEntities — GET /observability-connections/:id/newrelic/entities?q=&max=
func (h *ObservabilityConnectionHandler) NewRelicEntities(c *gin.Context) {
	cl, _, _, ok := h.newRelicClient(c)
	if !ok {
		return
	}
	max, _ := strconv.Atoi(c.Query("max"))
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	list, err := apm.NewRelicEntities(ctx, cl, c.Query("q"), max)
	if err != nil {
		obsFail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": list})
}

// NewRelicAlerts — GET /observability-connections/:id/newrelic/alerts?accountId=&hours=&open=1
func (h *ObservabilityConnectionHandler) NewRelicAlerts(c *gin.Context) {
	cl, _, _, ok := h.newRelicClient(c)
	if !ok {
		return
	}
	account, err := apm.NewRelicAccount(cl, c.Query("accountId"))
	if err != nil {
		obsFail(c, err)
		return
	}
	hours, _ := strconv.Atoi(c.Query("hours"))
	if hours <= 0 || hours > 24*30 {
		hours = 24
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 75*time.Second)
	defer cancel()
	list, err := apm.NewRelicAlerts(ctx, cl, account, time.Duration(hours)*time.Hour, c.Query("open") == "1" || c.Query("open") == "true")
	if err != nil {
		obsFail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": list})
}
