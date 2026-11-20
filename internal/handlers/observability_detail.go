package handlers

// Service detail (the APM summary page of one service): charts, endpoints,
// errors, instances and alerts from the connection the service came from.
// Read-only, so any grant is enough; every call is audited like a map read.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/apm"
	"patchman-be/internal/servicemap"

	"github.com/gin-gonic/gin"
)

func parseDetailRange(s string) time.Duration {
	switch s {
	case "30m":
		return 30 * time.Minute
	case "6h":
		return 6 * time.Hour
	case "24h":
		return 24 * time.Hour
	case "7d":
		return 7 * 24 * time.Hour
	}
	return time.Hour
}

func detailRef(c *gin.Context, source string) (servicemap.SourceRef, bool) {
	ref := servicemap.SourceRef{
		Source: source, Name: strings.TrimSpace(c.Query("service")), Namespace: strings.TrimSpace(c.Query("namespace")),
		Scope: strings.TrimSpace(c.Query("scope")), Key: strings.TrimSpace(c.Query("key")),
	}
	if ref.Name == "" && ref.Key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "service is required"})
		return ref, false
	}
	return ref, true
}

// detailFail answers a detail error with the provider's own status when it
// is a client-side one (unsupported → 501, bad input → 400).
func detailFail(c *gin.Context, err error) {
	if ae, ok := apm.AsAPIError(err); ok && (ae.Status == http.StatusNotImplemented || ae.Status == http.StatusBadRequest) {
		c.JSON(ae.Status, gin.H{"error": ae.Message, "code": ae.Code})
		return
	}
	obsFail(c, err)
}

// ServiceDetail — GET /observability-connections/:id/service-detail
// ?service=&namespace=&scope=&key=&range=30m|1h|6h|24h|7d
func (h *ObservabilityConnectionHandler) ServiceDetail(c *gin.Context) {
	conn, role, err := h.loadAccessible(c.Param("id"), c.GetString("userID"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	ref, ok := detailRef(c, conn.Provider)
	if !ok {
		return
	}
	cl, err := h.registry.Client(*conn)
	if err != nil {
		obsFail(c, err)
		return
	}
	det, ok := cl.Provider.(apm.ServiceDetailer)
	if !ok {
		detailFail(c, apm.ErrDetailUnsupported(cl.Provider.Title()))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 75*time.Second)
	defer cancel()
	d, err := det.ServiceDetail(ctx, cl, apm.DetailRequest{Ref: ref, Range: parseDetailRange(c.Query("range")), End: time.Now().UTC()})
	h.audit(c, conn, role, "service_detail", firstNonEmpty(ref.Name, ref.Key), err)
	if err != nil {
		detailFail(c, err)
		return
	}
	d.Normalize()
	c.JSON(http.StatusOK, gin.H{"data": d})
}

// ServiceDetail — GET /service-map/service-detail?k8s=&context=&service=&namespace=&range=
// reads a service from the cluster's own Prometheus.
func (h *ServiceMapHandler) ServiceDetail(c *gin.Context) {
	conn, _, err := h.k8sConns.loadAccessible(strings.TrimSpace(c.Query("k8s")), c.GetString("userID"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Kubernetes connection not found"})
		return
	}
	ref, ok := detailRef(c, "prometheus")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 75*time.Second)
	defer cancel()
	q, _, found, err := h.k8sReg.ClusterPrometheus(ctx, *conn, c.Query("context"))
	if err == nil && !found {
		err = errors.New("no Prometheus found in this cluster")
	}
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	rq, ok := q.(servicemap.PromRangeQuerier)
	if !ok {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "this Prometheus can't run range queries"})
		return
	}
	d, err := apm.PromServiceDetail(ctx, q, rq, apm.DetailRequest{Ref: ref, Range: parseDetailRange(c.Query("range")), End: time.Now().UTC()}, "prometheus")
	if err != nil {
		detailFail(c, err)
		return
	}
	d.Normalize()
	c.JSON(http.StatusOK, gin.H{"data": d})
}
