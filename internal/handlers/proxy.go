package handlers

import (
	"encoding/json"
	"net/http"

	"patchman-be/internal/forwarder"
	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type ProxyHandler struct {
	db  *gorm.DB
	fwd *forwarder.Client
}

func NewProxyHandler(db *gorm.DB) *ProxyHandler {
	return &ProxyHandler{db: db}
}

// WithForwarder attaches an upstream forwarder so history writes are mirrored
// to a remote BE. Called by main.go when the embedded BE is configured with
// --forward-target/--forward-token. Separate from the constructor to keep the
// call sites that build a ProxyHandler with nil deps unchanged.
func (h *ProxyHandler) WithForwarder(fwd *forwarder.Client) *ProxyHandler {
	h.fwd = fwd
	return h
}

func (h *ProxyHandler) Send(c *gin.Context) {
	userID := c.GetString("userID")

	var req services.ProxyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.URL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "URL is required"})
		return
	}

	if req.Method == "" {
		req.Method = "GET"
	}

	// Execute the request, tied to the client's context so aborting the send
	// (or the client disconnecting) cancels the in-flight upstream request.
	resp, err := services.ExecuteRequestCtx(c.Request.Context(), &req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	// Save to history only for authenticated users (anonymous callers keep history in the browser)
	if userID != "" {
		reqHeadersJSON, _ := json.Marshal(req.Headers)
		respHeadersJSON, _ := json.Marshal(resp.Headers)

		var reqBody string
		if req.Body != nil {
			reqBody = req.Body.Content
		}

		history := models.History{
			ID:              uuid.New().String(),
			UserID:          userID,
			Method:          req.Method,
			URL:             req.URL,
			StatusCode:      resp.StatusCode,
			Duration:        resp.Duration,
			RequestHeaders:  string(reqHeadersJSON),
			RequestBody:     reqBody,
			ResponseHeaders: string(respHeadersJSON),
			ResponseBody:    resp.Body,
			ResponseSize:    resp.Size,
		}

		h.db.Create(&history)

		if h.fwd != nil {
			h.fwd.ForwardHistory(history)
		}
	}

	c.JSON(http.StatusOK, gin.H{"data": resp})
}
