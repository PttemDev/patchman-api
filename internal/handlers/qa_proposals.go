package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Setup proposals: a CR + QA setup change an agent asked for over MCP
// when the client can't ask the user itself. The user applies or rejects it
// in the Hub; applying sends the stored request as the user (from their
// app), so nothing changes without their click.
type QAProposalHandler struct {
	db     *gorm.DB
	cipher *services.TokenCipher
}

func NewQAProposalHandler(db *gorm.DB, cipher *services.TokenCipher) *QAProposalHandler {
	return &QAProposalHandler{db: db, cipher: cipher}
}

// proposalRoutes: the only requests a proposal may carry (CR + QA setup).
var proposalRoutes = []struct {
	method string
	re     *regexp.Regexp
}{
	{"POST", regexp.MustCompile(`^/qa/links$`)},
	{"PUT", regexp.MustCompile(`^/qa/links/[A-Za-z0-9-]{1,36}$`)},
	{"DELETE", regexp.MustCompile(`^/qa/links/[A-Za-z0-9-]{1,36}$`)},
	{"PUT", regexp.MustCompile(`^/qa/links/[A-Za-z0-9-]{1,36}/mine$`)},
	{"POST", regexp.MustCompile(`^/qa/links/[A-Za-z0-9-]{1,36}/webhook/setup$`)},
	{"PUT", regexp.MustCompile(`^/qa/settings$`)},
	{"POST", regexp.MustCompile(`^/qa/schedules$`)},
	{"PUT", regexp.MustCompile(`^/qa/schedules/[A-Za-z0-9-]{1,36}$`)},
	{"DELETE", regexp.MustCompile(`^/qa/schedules/[A-Za-z0-9-]{1,36}$`)},
	{"POST", regexp.MustCompile(`^/qa/channels$`)},
	{"PUT", regexp.MustCompile(`^/qa/channels/[A-Za-z0-9-]{1,36}$`)},
	{"DELETE", regexp.MustCompile(`^/qa/channels/[A-Za-z0-9-]{1,36}$`)},
	{"POST", regexp.MustCompile(`^/jira-connections$`)},
}

func proposalAllowed(method, path string) bool {
	for _, r := range proposalRoutes {
		if r.method == method && r.re.MatchString(path) {
			return true
		}
	}
	return false
}

type proposalRequest struct {
	Method string            `json:"method"`
	Path   string            `json:"path"` // without /api
	Query  map[string]string `json:"query,omitempty"`
	Body   json.RawMessage   `json:"body,omitempty"`
}

type proposalInput struct {
	Tool    string          `json:"tool"`
	Summary string          `json:"summary" binding:"required"`
	Request proposalRequest `json:"request"`
}

// Create — POST /api/qa/proposals?workspaceId=.
func (h *QAProposalHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")
	var in proposalInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	in.Request.Method = strings.ToUpper(in.Request.Method)
	if !proposalAllowed(in.Request.Method, in.Request.Path) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Only CR + QA setup changes can be proposed"})
		return
	}
	var ws *string
	if v := qaWorkspaceParam(c); v != "" {
		if qaWorkspaceRole(h.db, v, userID) == NoRole {
			c.JSON(http.StatusNotFound, gin.H{"error": "Workspace not found"})
			return
		}
		ws = &v
	}
	raw, _ := json.Marshal(in.Request)
	enc, err := h.cipher.Encrypt(string(raw))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store the proposal"})
		return
	}
	summary, _ := capRunes(strings.TrimSpace(in.Summary), 2000)
	tool, _ := capRunes(in.Tool, 60)
	p := models.QASetupProposal{ID: uuid.NewString(), UserID: userID, WorkspaceID: ws, Agent: noteAgent(c), Tool: tool,
		Summary: summary, RequestEnc: enc, Status: "pending"}
	if err := h.db.Create(&p).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store the proposal"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": p})
}

// List — GET /api/qa/proposals?status=pending: the caller's own proposals
// (an agent works for the person whose token it uses).
func (h *QAProposalHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	q := h.db.Where("user_id = ?", userID)
	if s := c.Query("status"); s != "" {
		q = q.Where("status = ?", s)
	}
	rows := []models.QASetupProposal{}
	q.Order("created_at DESC").Limit(100).Find(&rows)
	c.JSON(http.StatusOK, gin.H{"data": rows})
}

func (h *QAProposalHandler) own(c *gin.Context, p *models.QASetupProposal) bool {
	// Applying or rejecting is the person's decision: never an agent's.
	if noteSource(c) == noteSourceMCP {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only you can apply or reject a proposal, in the CR + QA Hub"})
		return false
	}
	if err := h.db.Where("id = ? AND user_id = ?", c.Param("id"), c.GetString("userID")).First(p).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Proposal not found"})
		return false
	}
	return true
}

// Request — GET /api/qa/proposals/:id/request: the stored request, for the
// user's app to send when they apply it (owner only, pending only).
func (h *QAProposalHandler) Request(c *gin.Context) {
	var p models.QASetupProposal
	if !h.own(c, &p) {
		return
	}
	if p.Status != "pending" {
		c.JSON(http.StatusConflict, gin.H{"error": "This proposal is " + p.Status})
		return
	}
	raw, err := h.cipher.Decrypt(p.RequestEnc)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "The proposal can't be read"})
		return
	}
	var r proposalRequest
	_ = json.Unmarshal([]byte(raw), &r)
	if !proposalAllowed(r.Method, r.Path) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Not an allowed request"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": r})
}

type resolveInput struct {
	Status string `json:"status"` // applied | rejected
	Result string `json:"result"`
}

// Resolve — POST /api/qa/proposals/:id/resolve.
func (h *QAProposalHandler) Resolve(c *gin.Context) {
	var p models.QASetupProposal
	if !h.own(c, &p) {
		return
	}
	var in resolveInput
	if err := c.ShouldBindJSON(&in); err != nil || (in.Status != "applied" && in.Status != "rejected") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "status must be applied or rejected"})
		return
	}
	res, _ := capRunes(in.Result, 2000)
	p.Status, p.Result = in.Status, res
	if in.Status == "rejected" {
		p.RequestEnc = "" // nothing left to apply; drop any token it carried
	}
	h.db.Save(&p)
	c.JSON(http.StatusOK, gin.H{"data": p})
}

// pendingProposals counts the user's open proposals (Inbox banner).
func pendingProposals(db *gorm.DB, userID string) int {
	var n int64
	db.Model(&models.QASetupProposal{}).Where("user_id = ? AND status = ?", userID, "pending").Count(&n)
	return int(n)
}

type webhookSetupInput struct {
	BaseURL string `json:"baseUrl" binding:"required"` // public URL of this PatchMan server
}

// SetupWebhook — POST /api/qa/links/:id/webhook/setup {baseUrl}: makes a new
// secret and creates the GitLab project hook in one step with the caller's
// credentials; the secret never leaves the server.
func (h *QARadarHandler) SetupWebhook(c *gin.Context) {
	userID := c.GetString("userID")
	var in webhookSetupInput
	if err := c.ShouldBindJSON(&in); err != nil || !(strings.HasPrefix(in.BaseURL, "https://") || strings.HasPrefix(in.BaseURL, "http://")) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "baseUrl (the PatchMan server's public http/https URL) is required"})
		return
	}
	var l models.QAProjectLink
	if !qaLoad(h.db, c, userID, c.Param("id"), &l, EditorRole, "Link not found") {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	lc, ok := h.linkAccess(ctx, c, userID, l.ID)
	if !ok {
		return
	}
	if isGitHub(lc.conn) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "GitHub links refresh by polling; no webhook to install"})
		return
	}
	secret := uuid.NewString() + uuid.NewString()
	hookURL := strings.TrimRight(strings.TrimSuffix(strings.TrimRight(in.BaseURL, "/"), "/api"), "/") + "/api/public/qa/webhooks/gitlab/" + l.ID
	body := map[string]any{"url": hookURL, "token": secret, "merge_requests_events": true, "pipeline_events": true,
		"note_events": true, "push_events": false, "enable_ssl_verification": strings.HasPrefix(hookURL, "https://")}
	b, st, err := lc.f.send(ctx, http.MethodPost, lc.f.glBase+projectPath(lc.link.GitProject)+"/hooks", body)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	if st/100 != 2 {
		c.JSON(http.StatusBadGateway, gin.H{"error": "GitLab " + http.StatusText(st) + ": " + remoteMessage(b)})
		return
	}
	h.db.Model(&models.QAProjectLink{}).Where("id = ?", l.ID).Update("webhook_secret_hash", hashSecret(secret))
	h.git.conns.audit(userID, lc.conn.ID, "qa_webhook_install", true, gin.H{"project": lc.link.GitProject})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"installed": true, "url": hookURL}})
}

// Agent setup policy (Settings → MCP): whether an AI agent must get the
// user's OK before it changes CR + QA setup. Default on.
const qaAgentSetupKey = "qa_agent_setup"

type qaAgentSetupPolicy struct {
	Ask bool `json:"ask"`
}

func loadAgentSetupPolicy(db *gorm.DB, userID string) qaAgentSetupPolicy {
	p := qaAgentSetupPolicy{Ask: true}
	var row models.UserSetting
	if r := db.Where("user_id = ? AND key = ?", userID, qaAgentSetupKey).Limit(1).Find(&row); r.RowsAffected > 0 {
		_ = json.Unmarshal([]byte(row.Value), &p)
	}
	return p
}

// GetAgentPolicy — GET /api/qa/agent-setup-policy.
func (h *QAProposalHandler) GetAgentPolicy(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": loadAgentSetupPolicy(h.db, c.GetString("userID"))})
}

// PutAgentPolicy — PUT /api/qa/agent-setup-policy {ask}. Only the person
// (not an agent over MCP) can turn the question off.
func (h *QAProposalHandler) PutAgentPolicy(c *gin.Context) {
	if noteSource(c) == noteSourceMCP {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only you can change this, in Settings → MCP"})
		return
	}
	var in qaAgentSetupPolicy
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	b, _ := json.Marshal(in)
	row := models.UserSetting{UserID: c.GetString("userID"), Key: qaAgentSetupKey, Value: string(b), UpdatedAt: time.Now()}
	if err := h.db.Save(&row).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": in})
}
