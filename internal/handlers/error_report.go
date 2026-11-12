package handlers

import (
	"context"
	"net/http"
	"time"

	"patchman-be/internal/errorreport"
	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// ErrorReportHandler exposes three endpoints:
//
//   POST /api/errors/report           — auth'd users; FE pushes a crash here
//   GET  /api/admin/error-reporting/config  — admin reads the toggle state
//   PATCH /api/admin/error-reporting/config — admin updates the toggles
//   POST /api/admin/error-reporting/test    — admin sends a test payload
//
// The reporter is shared across requests; toggle state is read fresh
// from the DB on every report so admins flipping a switch take effect
// immediately without restarting the API.
type ErrorReportHandler struct {
	db       *gorm.DB
	reporter *errorreport.Reporter
}

func NewErrorReportHandler(db *gorm.DB, reporter *errorreport.Reporter) *ErrorReportHandler {
	return &ErrorReportHandler{db: db, reporter: reporter}
}

// reportRequest mirrors errorreport.Payload but unexports the
// server-filled fields so we don't bind UserID/UserEmail from the
// request body — they always come from the auth context.
type reportRequest struct {
	Source         string `json:"source" binding:"required"`
	Message        string `json:"message" binding:"required"`
	Stack          string `json:"stack"`
	ComponentStack string `json:"componentStack"`

	AppName     string `json:"appName"`
	AppVersion  string `json:"appVersion"`
	BuildEnv    string `json:"buildEnv"`
	BuildCommit string `json:"buildCommit"`
	Platform    string `json:"platform"`
	OSInfo      string `json:"osInfo"`

	Route   string `json:"route"`
	TabName string `json:"tabName"`

	WorkspaceID   string `json:"workspaceId"`
	WorkspaceName string `json:"workspaceName"`
}

// Report accepts a crash payload from any signed-in user, checks the
// admin toggle for the relevant source, and forwards to Slack when
// enabled. Always returns 204 — the FE doesn't need to know whether
// reporting is enabled or whether Slack accepted the payload.
func (h *ErrorReportHandler) Report(c *gin.Context) {
	var req reportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
		return
	}

	source := errorreport.Source(req.Source)
	if !validSource(source) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid source"})
		return
	}

	settings, err := loadSystemSetting(h.db)
	if err != nil {
		// Failing to read settings shouldn't break the FE — log-and-noop.
		c.Status(http.StatusNoContent)
		return
	}

	if !sourceEnabled(settings, source) {
		c.Status(http.StatusNoContent)
		return
	}

	payload := buildPayload(c, h.db, &req)
	// Fire-and-forget: the FE has no way to act on a Slack delivery
	// failure, and we don't want to keep the user's request open while
	// we wait for Slack. Errors are silently dropped — admins can
	// validate via the test endpoint.
	//
	// Detach from c.Request.Context() — Gin/net-http cancels the request
	// context the moment the handler returns and 204 lands on the wire,
	// which races the goroutine's outbound POST to Slack (req fires with
	// an already-cancelled context → client.Do returns context.Canceled,
	// nothing is delivered, and the user just sees a silent drop). A
	// fresh 10-second background context survives the handler return and
	// gives the reporter HTTP client enough room to finish its own 5s
	// Timeout.
	go func(p *errorreport.Payload) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = h.reporter.Post(ctx, p)
	}(payload)

	c.Status(http.StatusNoContent)
}

// configResponse is the shape of GET / PATCH config. Mirrors
// SystemSetting's JSON tags so a PATCH can be reused as the GET output.
type configResponse struct {
	BoundaryEnabled  bool `json:"boundaryEnabled"`
	RejectionEnabled bool `json:"rejectionEnabled"`
	WindowEnabled    bool `json:"windowEnabled"`
	// Whether the server has a Slack webhook URL configured. Lets the
	// admin UI surface "Webhook not configured — toggles have no effect"
	// without exposing the URL itself.
	SlackConfigured bool `json:"slackConfigured"`
}

func (h *ErrorReportHandler) GetConfig(c *gin.Context) {
	settings, err := loadSystemSetting(h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load settings"})
		return
	}
	c.JSON(http.StatusOK, configResponse{
		BoundaryEnabled:  settings.ErrorReportingBoundaryEnabled,
		RejectionEnabled: settings.ErrorReportingRejectionEnabled,
		WindowEnabled:    settings.ErrorReportingWindowEnabled,
		SlackConfigured:  h.reporter.Configured(),
	})
}

type patchConfigRequest struct {
	// Pointers so the caller can omit a field to leave it unchanged.
	BoundaryEnabled  *bool `json:"boundaryEnabled"`
	RejectionEnabled *bool `json:"rejectionEnabled"`
	WindowEnabled    *bool `json:"windowEnabled"`
}

func (h *ErrorReportHandler) PatchConfig(c *gin.Context) {
	var req patchConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
		return
	}

	settings, err := loadSystemSetting(h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load settings"})
		return
	}

	if req.BoundaryEnabled != nil {
		settings.ErrorReportingBoundaryEnabled = *req.BoundaryEnabled
	}
	if req.RejectionEnabled != nil {
		settings.ErrorReportingRejectionEnabled = *req.RejectionEnabled
	}
	if req.WindowEnabled != nil {
		settings.ErrorReportingWindowEnabled = *req.WindowEnabled
	}

	if err := h.db.Save(&settings).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save settings"})
		return
	}

	c.JSON(http.StatusOK, configResponse{
		BoundaryEnabled:  settings.ErrorReportingBoundaryEnabled,
		RejectionEnabled: settings.ErrorReportingRejectionEnabled,
		WindowEnabled:    settings.ErrorReportingWindowEnabled,
		SlackConfigured:  h.reporter.Configured(),
	})
}

// Test fires a synthetic payload at Slack so the admin can validate the
// integration end-to-end. Bypasses the toggle and the dedupe window.
// Returns the actual upstream error message on failure so the admin
// can read it inline.
func (h *ErrorReportHandler) Test(c *gin.Context) {
	if !h.reporter.Configured() {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"ok":    false,
			"error": "Slack webhook URL is not configured on the server (SLACK_ERROR_WEBHOOK_URL).",
		})
		return
	}

	userEmail := c.GetString("userEmail")
	userID := c.GetString("userID")
	if userEmail == "" {
		// The admin handler doesn't set userEmail; look it up so the
		// test payload's "User" line is a real address.
		var u models.User
		if err := h.db.Select("email", "name").First(&u, "id = ?", userID).Error; err == nil {
			userEmail = u.Email
		}
	}

	payload := &errorreport.Payload{
		Source:     errorreport.SourceManual,
		Message:    "✅ Test from PatchMan admin — Slack integration is reachable.",
		Stack:      "(test payload — no stack)",
		AppName:    "patchman-ui",
		AppVersion: "test",
		BuildEnv:   "manual-test",
		Platform:   "admin",
		Route:      "/admin",
		UserID:     userID,
		UserEmail:  userEmail,
		ServerAt:   time.Now(),
	}

	status, err := h.reporter.PostRaw(c.Request.Context(), payload)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"ok":     false,
			"status": status,
			"error":  err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "status": status})
}

func validSource(s errorreport.Source) bool {
	switch s {
	case errorreport.SourceBoundary,
		errorreport.SourceRejection,
		errorreport.SourceWindow,
		errorreport.SourceManual:
		return true
	}
	return false
}

func sourceEnabled(s models.SystemSetting, src errorreport.Source) bool {
	switch src {
	case errorreport.SourceBoundary:
		return s.ErrorReportingBoundaryEnabled
	case errorreport.SourceRejection:
		return s.ErrorReportingRejectionEnabled
	case errorreport.SourceWindow:
		return s.ErrorReportingWindowEnabled
	case errorreport.SourceManual:
		// Manual = the admin Test button; never gated by the toggles.
		return true
	}
	return false
}

func loadSystemSetting(db *gorm.DB) (models.SystemSetting, error) {
	var s models.SystemSetting
	err := db.First(&s, models.SystemSettingSingletonID).Error
	if err == nil {
		return s, nil
	}
	// Defensive: if the seed row went missing, recreate it with defaults
	// instead of returning an error that would break every report.
	s = models.SystemSetting{ID: models.SystemSettingSingletonID}
	if cerr := db.Create(&s).Error; cerr != nil {
		return s, cerr
	}
	return s, nil
}

func buildPayload(c *gin.Context, db *gorm.DB, req *reportRequest) *errorreport.Payload {
	userID := c.GetString("userID")
	var (
		userEmail string
		userName  string
	)
	var u models.User
	if err := db.Select("email", "name").First(&u, "id = ?", userID).Error; err == nil {
		userEmail = u.Email
		userName = u.Name
	}

	return &errorreport.Payload{
		Source:         errorreport.Source(req.Source),
		Message:        req.Message,
		Stack:          req.Stack,
		ComponentStack: req.ComponentStack,
		AppName:        req.AppName,
		AppVersion:     req.AppVersion,
		BuildEnv:       req.BuildEnv,
		BuildCommit:    req.BuildCommit,
		Platform:       req.Platform,
		OSInfo:         req.OSInfo,
		Route:          req.Route,
		TabName:        req.TabName,
		WorkspaceID:    req.WorkspaceID,
		WorkspaceName:  req.WorkspaceName,
		UserID:         userID,
		UserEmail:      userEmail,
		UserName:       userName,
		ServerAt:       time.Now(),
	}
}
