package handlers

import (
	"net/http"

	"patchman-be/internal/cloudsession"

	"github.com/gin-gonic/gin"
)

// CloudSessionHandler receives the signed-in user's cloud session from the
// desktop FE so the embedded backend's whiteboard MCP tools can act on the
// hosted BE as the user (see WHITEBOARD_UNIFIED_LOCAL_RFC.md). Mounted ONLY in
// embedded mode, on the loopback-bound embedded server. The token is held in
// RAM only (cloudsession) and never persisted.
type CloudSessionHandler struct{}

func NewCloudSessionHandler() *CloudSessionHandler { return &CloudSessionHandler{} }

type cloudSessionInput struct {
	BaseURL string `json:"baseUrl"`
	Token   string `json:"token"`
}

// The only legitimate caller is the desktop's own webview. Tauri webviews send
// a fixed Origin; a malicious web page in the user's normal browser (which
// could otherwise scan loopback ports and POST its own token to hijack which
// account the AI writes to) sends its own site's Origin and is rejected. A
// request with NO Origin is a non-browser local process — already able to run
// arbitrary code on this machine, so out of scope, and allowed (curl/tests).
var allowedCloudSessionOrigins = map[string]bool{
	"tauri://localhost":       true, // macOS/Linux webview
	"https://tauri.localhost": true, // Windows webview
}

func cloudSessionOriginAllowed(c *gin.Context) bool {
	o := c.GetHeader("Origin")
	return o == "" || allowedCloudSessionOrigins[o]
}

// Set installs (or, with empty fields, clears) the cloud session.
func (h *CloudSessionHandler) Set(c *gin.Context) {
	if !cloudSessionOriginAllowed(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden origin"})
		return
	}
	var in cloudSessionInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	cloudsession.Set(in.BaseURL, in.Token)
	_, ok := cloudsession.Get()
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"active": ok}})
}

// Clear drops the cloud session (logout / going local).
func (h *CloudSessionHandler) Clear(c *gin.Context) {
	if !cloudSessionOriginAllowed(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden origin"})
		return
	}
	cloudsession.Clear()
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"active": false}})
}
