package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/realtime"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// GitLab webhook for a QA link (CR_QA_RFC P1): merge request, pipeline and
// note events drop the space's cached radars at once (instead of waiting for
// the 60 s cache), mark reviews of an old head stale and tell the space's
// members' open apps to refresh. Authenticated by a per-link secret in
// X-Gitlab-Token (stored as a SHA-256 hash, compared in constant time).
type QAWebhookHandler struct {
	db       *gorm.DB
	channels *QAChannels // may be nil
}

func NewQAWebhookHandler(db *gorm.DB) *QAWebhookHandler { return &QAWebhookHandler{db: db} }

// SetChannels wires the channel sender (it needs the cipher).
func (h *QAWebhookHandler) SetChannels(c *QAChannels) { h.channels = c }

// EventTypeQARadar tells a space's members to refresh Radar / Inbox.
const EventTypeQARadar = "qa.radar"

func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// GitLab — POST /api/public/qa/webhooks/gitlab/:linkId (no session auth).
func (h *QAWebhookHandler) GitLab(c *gin.Context) {
	var l models.QAProjectLink
	if err := h.db.Where("id = ?", c.Param("linkId")).First(&l).Error; err != nil || l.WebhookSecretHash == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	got := hashSecret(c.GetHeader("X-Gitlab-Token"))
	if subtle.ConstantTimeCompare([]byte(got), []byte(l.WebhookSecretHash)) != 1 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "bad token"})
		return
	}
	body, _ := io.ReadAll(io.LimitReader(c.Request.Body, 2<<20))
	var ev gitlabEvent
	_ = json.Unmarshal(body, &ev)
	h.handle(l, ev)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

type gitlabEvent struct {
	ObjectKind string `json:"object_kind"` // merge_request | pipeline | note
	User       struct {
		Username string `json:"username"`
	} `json:"user"`
	ObjectAttributes struct {
		IID        int64  `json:"iid"`
		Title      string `json:"title"`
		Action     string `json:"action"` // open | update | merge | close | approved …
		State      string `json:"state"`
		Status     string `json:"status"` // pipeline: success | failed …
		URL        string `json:"url"`
		LastCommit struct {
			ID string `json:"id"`
		} `json:"last_commit"`
		Ref string `json:"ref"`
	} `json:"object_attributes"`
	MergeRequest *struct {
		IID   int64  `json:"iid"`
		Title string `json:"title"`
		URL   string `json:"url"`
	} `json:"merge_request"`
	Project struct {
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
}

func (h *QAWebhookHandler) handle(l models.QAProjectLink, ev gitlabEvent) {
	now := time.Now()
	h.db.Model(&models.QAProjectLink{}).Where("id = ?", l.ID).Update("webhook_at", now)
	// A new head makes reviews of older commits stale.
	if ev.ObjectKind == "merge_request" && ev.ObjectAttributes.LastCommit.ID != "" {
		h.db.Model(&models.MRReview{}).
			Where("link_id = ? AND iid = ? AND head_sha <> ? AND status IN ?", l.ID, ev.ObjectAttributes.IID, ev.ObjectAttributes.LastCommit.ID, []string{"claimed", "draft"}).
			Update("status", "stale")
	}
	invalidateQACaches(l.UserID, l.WorkspaceID)
	recipients := qaSpaceMembers(h.db, l.UserID, l.WorkspaceID)
	realtime.Notify(h.db, realtime.Event{Type: EventTypeQARadar, Recipients: recipients})
	if h.channels != nil {
		h.channels.onGitLabEvent(l, ev)
	}
}

// qaSpaceMembers: who sees a QA space — the owner for a personal one; the
// workspace owner and its team's members for a workspace.
func qaSpaceMembers(db *gorm.DB, owner string, ws *string) []string {
	if ws == nil || *ws == "" {
		return []string{owner}
	}
	var w struct {
		OwnerID string
		TeamID  *string
	}
	db.Table("workspaces").Select("owner_id, team_id").Where("id = ?", *ws).Scan(&w)
	set := map[string]bool{owner: true}
	if w.OwnerID != "" {
		set[w.OwnerID] = true
	}
	if w.TeamID != nil && *w.TeamID != "" {
		var ids []string
		db.Table("team_members").Where("team_id = ?", *w.TeamID).Pluck("user_id", &ids)
		for _, id := range ids {
			set[id] = true
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	return out
}

// CreateSecret — POST /api/qa/links/:id/webhook (editors): makes a new
// secret, stores its hash and returns the secret once with the URL path.
func (h *QALinkHandler) CreateWebhookSecret(c *gin.Context) {
	userID := c.GetString("userID")
	var l models.QAProjectLink
	if !qaLoad(h.db, c, userID, c.Param("id"), &l, EditorRole, "Link not found") {
		return
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to make a secret"})
		return
	}
	secret := hex.EncodeToString(b)
	if err := h.db.Model(&models.QAProjectLink{}).Where("id = ?", l.ID).Update("webhook_secret_hash", hashSecret(secret)).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"path": "/api/public/qa/webhooks/gitlab/" + l.ID, "secret": secret}})
}

type installHookInput struct {
	URL    string `json:"url" binding:"required"` // full public webhook URL
	Secret string `json:"secret" binding:"required"`
}

// InstallWebhook — POST /api/qa/links/:id/webhook/install {url, secret}:
// creates the project hook in GitLab with the caller's credentials (their
// click; GitLab requires Maintainer). GitHub links are set up by hand.
func (h *QARadarHandler) InstallWebhook(c *gin.Context) {
	userID := c.GetString("userID")
	var in installHookInput
	if err := c.ShouldBindJSON(&in); err != nil || !(strings.HasPrefix(in.URL, "https://") || strings.HasPrefix(in.URL, "http://")) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "url (http/https) and secret are required"})
		return
	}
	var l models.QAProjectLink
	if !qaLoad(h.db, c, userID, c.Param("id"), &l, EditorRole, "Link not found") {
		return
	}
	if l.WebhookSecretHash == "" || hashSecret(in.Secret) != l.WebhookSecretHash {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Make a webhook secret first; the secret doesn't match"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	lc, ok := h.linkAccess(ctx, c, userID, l.ID)
	if !ok {
		return
	}
	if isGitHub(lc.conn) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "GitHub webhooks are not installed from here; GitHub links refresh by polling"})
		return
	}
	body := map[string]any{"url": in.URL, "token": in.Secret, "merge_requests_events": true, "pipeline_events": true,
		"note_events": true, "push_events": false, "enable_ssl_verification": strings.HasPrefix(in.URL, "https://")}
	b, st, err := lc.f.send(ctx, http.MethodPost, lc.f.glBase+projectPath(lc.link.GitProject)+"/hooks", body)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	if st/100 != 2 {
		c.JSON(http.StatusBadGateway, gin.H{"error": "GitLab " + http.StatusText(st) + ": " + remoteMessage(b)})
		return
	}
	h.git.conns.audit(userID, lc.conn.ID, "qa_webhook_install", true, gin.H{"project": lc.link.GitProject})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"installed": true}})
}
