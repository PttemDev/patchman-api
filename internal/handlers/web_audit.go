package handlers

import (
	"errors"
	"net/http"
	"strings"

	"patchman-be/internal/webaudit"

	"github.com/gin-gonic/gin"
)

// WebAuditHandler backs the Dev Tools "Web Audit" utility. Like the DNS handler
// it is stateless and dependency-free — no DB, no cipher, no connection
// registry — so it is registered with the plain utility handlers and works on
// every BE (embedded + remote) regardless of configuration.
type WebAuditHandler struct{}

func NewWebAuditHandler() *WebAuditHandler {
	return &WebAuditHandler{}
}

// webAuditRequest is the wire body: the URL to audit.
type webAuditRequest struct {
	URL string `json:"url"`
	// FollowRedirects: whether to follow 3xx redirects (default true when omitted).
	// When false, the audit reports the first response as-is (e.g. the 301/302).
	FollowRedirects *bool `json:"followRedirects"`
}

// Audit fetches and analyzes a URL, returning a structured SEO/quality report.
// POST /api/devtools/web-audit  { url } → { data: WebAuditReport }
//
// It uses the request context so a client abort cancels the outbound fetch. A
// bad/non-http(s) URL is a 400; an unfetchable one (DNS/connection/timeout) is
// a 502 — both as { error: … }.
func (h *WebAuditHandler) Audit(c *gin.Context) {
	var req webAuditRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	rawURL := strings.TrimSpace(req.URL)
	if rawURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "url is required"})
		return
	}

	follow := req.FollowRedirects == nil || *req.FollowRedirects
	report, err := webaudit.Audit(c.Request.Context(), rawURL, follow)
	if err != nil {
		if errors.Is(err, webaudit.ErrInvalidURL) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": report})
}
