package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Notification channels of a QA space (O4): Slack and Microsoft Teams
// incoming webhooks. Messages come from GitLab webhook events (no stored Git
// credentials needed) and the daily digest (counts from the space's radar
// snapshot). Webhook URLs are secrets: encrypted, never echoed, and limited
// to the providers' own hosts (no SSRF to internal addresses).
type QAChannels struct {
	db     *gorm.DB
	cipher *services.TokenCipher
	http   *http.Client
}

func NewQAChannels(db *gorm.DB, cipher *services.TokenCipher) *QAChannels {
	return &QAChannels{db: db, cipher: cipher, http: &http.Client{Timeout: 10 * time.Second}}
}

var qaChannelEvents = map[string]bool{"mr_opened": true, "pipeline_failed": true, "mr_approved": true, "mr_merged": true, "digest": true}

// channelURLAllowed is the send-time check (tests point it at a local server).
var channelURLAllowed = validChannelURL

// validChannelURL accepts only the providers' incoming-webhook hosts.
func validChannelURL(kind, raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	switch kind {
	case "slack":
		return host == "hooks.slack.com"
	case "teams":
		return strings.HasSuffix(host, ".webhook.office.com") || strings.HasSuffix(host, ".logic.azure.com") ||
			strings.HasSuffix(host, ".environment.api.powerplatform.com")
	}
	return false
}

type channelInput struct {
	Kind    *string `json:"kind"`
	Name    *string `json:"name"`
	URL     *string `json:"url"` // write-only
	Events  *string `json:"events"`
	Enabled *bool   `json:"enabled"`
}

func (q *QAChannels) apply(in channelInput, ch *models.QAChannel) string {
	if in.Kind != nil {
		ch.Kind = strings.TrimSpace(*in.Kind)
	}
	if ch.Kind != "slack" && ch.Kind != "teams" {
		return "kind must be slack or teams"
	}
	if in.Name != nil {
		ch.Name, _ = capRunes(strings.TrimSpace(*in.Name), 200)
	}
	if in.URL != nil && strings.TrimSpace(*in.URL) != "" {
		if !channelURLAllowed(ch.Kind, *in.URL) {
			return "url must be the provider's https incoming-webhook URL (Slack: hooks.slack.com; Teams: *.webhook.office.com or a Power Automate workflow URL)"
		}
		enc, err := q.cipher.Encrypt(strings.TrimSpace(*in.URL))
		if err != nil {
			return "could not store the URL"
		}
		ch.URLEnc = enc
	}
	if ch.URLEnc == "" {
		return "url is required"
	}
	if in.Events != nil {
		evs := []string{}
		for _, e := range strings.Split(*in.Events, ",") {
			if e = strings.TrimSpace(e); qaChannelEvents[e] {
				evs = append(evs, e)
			}
		}
		ch.Events = strings.Join(evs, ",")
	}
	if in.Enabled != nil {
		ch.Enabled = *in.Enabled
	}
	return ""
}

// List — GET /api/qa/channels?workspaceId=.
func (q *QAChannels) List(c *gin.Context) {
	userID := c.GetString("userID")
	scope, ok := qaListScope(q.db, c, userID)
	if !ok {
		return
	}
	rows := []models.QAChannel{}
	scope.Order("created_at ASC").Find(&rows)
	c.JSON(http.StatusOK, gin.H{"data": rows})
}

// Create — POST /api/qa/channels?workspaceId=.
func (q *QAChannels) Create(c *gin.Context) {
	userID := c.GetString("userID")
	var in channelInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ws, ok := qaCreateWorkspace(q.db, c, userID)
	if !ok {
		return
	}
	ch := models.QAChannel{ID: uuid.NewString(), UserID: userID, WorkspaceID: ws, Enabled: true, Events: "mr_opened,pipeline_failed,mr_merged,digest"}
	if msg := q.apply(in, &ch); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	if err := q.db.Create(&ch).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": ch})
}

// Update — PUT /api/qa/channels/:id (editors).
func (q *QAChannels) Update(c *gin.Context) {
	userID := c.GetString("userID")
	var ch models.QAChannel
	if !qaLoad(q.db, c, userID, c.Param("id"), &ch, EditorRole, "Channel not found") {
		return
	}
	var in channelInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if msg := q.apply(in, &ch); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	q.db.Save(&ch)
	c.JSON(http.StatusOK, gin.H{"data": ch})
}

// Delete — DELETE /api/qa/channels/:id.
func (q *QAChannels) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	var ch models.QAChannel
	if !qaLoad(q.db, c, userID, c.Param("id"), &ch, OwnerRole, "Channel not found") {
		return
	}
	q.db.Delete(&ch)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"deleted": true}})
}

// Test — POST /api/qa/channels/:id/test: sends a test message.
func (q *QAChannels) Test(c *gin.Context) {
	userID := c.GetString("userID")
	var ch models.QAChannel
	if !qaLoad(q.db, c, userID, c.Param("id"), &ch, EditorRole, "Channel not found") {
		return
	}
	if err := q.send(c.Request.Context(), &ch, "PatchMan CR + QA Hub: this channel is connected ✅"); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"sent": true}})
}

// send posts a plain-text message (Slack and Teams both accept {"text"};
// Teams Workflows URLs expect an Adaptive Card — doğrulanmadı — so a card
// carrying the same text is sent there).
func (q *QAChannels) send(ctx context.Context, ch *models.QAChannel, text string) error {
	raw, err := q.cipher.Decrypt(ch.URLEnc)
	if err != nil {
		return fmt.Errorf("the channel URL can't be read; edit the channel")
	}
	if !channelURLAllowed(ch.Kind, raw) {
		return fmt.Errorf("the channel URL is not allowed")
	}
	var body any = map[string]any{"text": text}
	if ch.Kind == "teams" && !strings.Contains(raw, ".webhook.office.com") {
		body = map[string]any{"type": "message", "attachments": []any{map[string]any{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"content": map[string]any{"type": "AdaptiveCard", "version": "1.4", "$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
				"body": []any{map[string]any{"type": "TextBlock", "text": text, "wrap": true}}},
		}}}
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, raw, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := q.http.Do(req)
	now := time.Now()
	if err != nil {
		q.db.Model(&models.QAChannel{}).Where("id = ?", ch.ID).Update("last_error", "unreachable")
		return fmt.Errorf("the channel is not reachable")
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
	if resp.StatusCode/100 != 2 {
		e := fmt.Sprintf("%d %s", resp.StatusCode, strings.TrimSpace(string(msg)))
		q.db.Model(&models.QAChannel{}).Where("id = ?", ch.ID).Update("last_error", e)
		return fmt.Errorf("the channel answered %s", e)
	}
	q.db.Model(&models.QAChannel{}).Where("id = ?", ch.ID).Updates(map[string]any{"last_error": "", "last_sent_at": &now})
	return nil
}

// spaceChannels: enabled channels of a link's space subscribed to event.
func (q *QAChannels) spaceChannels(owner string, ws *string, event string) []models.QAChannel {
	var rows []models.QAChannel
	qq := q.db.Where("enabled = ?", true)
	if ws != nil && *ws != "" {
		qq = qq.Where("workspace_id = ?", *ws)
	} else {
		qq = qq.Where("user_id = ? AND (workspace_id IS NULL OR workspace_id = '')", owner)
	}
	qq.Find(&rows)
	out := []models.QAChannel{}
	for _, r := range rows {
		for _, e := range strings.Split(r.Events, ",") {
			if strings.TrimSpace(e) == event {
				out = append(out, r)
			}
		}
	}
	return out
}

// eventMessage maps a GitLab event to a channel event and its text ("" =
// not posted).
func eventMessage(l models.QAProjectLink, ev gitlabEvent) (string, string) {
	a := ev.ObjectAttributes
	ref := func(iid int64, title, link string) string {
		s := fmt.Sprintf("%s!%d %s", l.GitProject, iid, title)
		if link != "" {
			s += " — " + link
		}
		return s
	}
	switch ev.ObjectKind {
	case "merge_request":
		switch a.Action {
		case "open", "reopen":
			return "mr_opened", "🆕 MR opened: " + ref(a.IID, a.Title, a.URL)
		case "approved":
			return "mr_approved", "👍 MR approved: " + ref(a.IID, a.Title, a.URL)
		case "merge":
			return "mr_merged", "✅ MR merged: " + ref(a.IID, a.Title, a.URL)
		}
	case "pipeline":
		if a.Status == "failed" {
			if ev.MergeRequest != nil {
				return "pipeline_failed", "❌ Pipeline failed on " + ref(ev.MergeRequest.IID, ev.MergeRequest.Title, ev.MergeRequest.URL)
			}
			return "pipeline_failed", fmt.Sprintf("❌ Pipeline failed on %s (%s)", l.GitProject, a.Ref)
		}
	}
	return "", ""
}

func (q *QAChannels) onGitLabEvent(l models.QAProjectLink, ev gitlabEvent) {
	event, text := eventMessage(l, ev)
	if event == "" {
		return
	}
	for _, ch := range q.spaceChannels(l.UserID, l.WorkspaceID, event) {
		ch := ch
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_ = q.send(ctx, &ch, text)
		}()
	}
}

// digestTick sends each space's daily digest once, at its digest hour.
func (q *QAChannels) digestTick(now time.Time) {
	var settings []models.QASettings
	q.db.Where("digest_hour IS NOT NULL").Find(&settings)
	for _, s := range settings {
		loc, err := time.LoadLocation(s.Timezone)
		if err != nil || s.Timezone == "" {
			loc = time.UTC
		}
		n := now.In(loc)
		day := n.Format("2006-01-02")
		if s.DigestHour == nil || n.Hour() != *s.DigestHour || s.DigestSentDay == day {
			continue
		}
		channels := q.spaceChannels(s.UserID, s.WorkspaceID, "digest")
		if len(channels) == 0 {
			continue
		}
		claim := q.db.Model(&models.QASettings{}).Where("id = ? AND (digest_sent_day IS NULL OR digest_sent_day <> ?)", s.ID, day).Update("digest_sent_day", day)
		if claim.Error != nil || claim.RowsAffected != 1 {
			continue
		}
		text := q.digestText(s)
		for _, ch := range channels {
			ch := ch
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				_ = q.send(ctx, &ch, text)
			}()
		}
	}
}

// digestText: team-level counts from the latest radar snapshot.
func (q *QAChannels) digestText(s models.QASettings) string {
	var snap models.QARadarSnapshot
	if r := q.db.Where("space_key = ?", qaSpaceKey(s.UserID, s.WorkspaceID)).Order("day DESC").Limit(1).Find(&snap); r.RowsAffected == 0 {
		return "PatchMan CR + QA daily digest: no radar data yet — open the CR + QA Hub once to collect it."
	}
	sig := map[string]int{}
	_ = json.Unmarshal([]byte(snap.SignalsJSON), &sig)
	text := fmt.Sprintf("📊 PatchMan CR + QA digest (%s): %d open MRs · %d stuck · %d awaiting review · %d without Jira · %d merged in the window",
		snap.Day, snap.OpenMRs, sig["stuck"], sig["awaitingReview"], sig["noJira"], snap.Merged)
	if snap.LeadMedianH != nil {
		text += fmt.Sprintf(" · lead time median %.1f h", *snap.LeadMedianH)
	}
	return text
}
