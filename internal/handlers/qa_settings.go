package handlers

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/qalink"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// QASettingsHandler: the MR Radar thresholds of a QA space and the Inbox
// snoozes (CR_QA_P0_PLAN P0.c).
type QASettingsHandler struct{ db *gorm.DB }

func NewQASettingsHandler(db *gorm.DB) *QASettingsHandler { return &QASettingsHandler{db: db} }

// loadQASettings returns the space's settings, or empty ones (every
// threshold off) when none were saved.
func loadQASettings(db *gorm.DB, userID string, ws *string) models.QASettings {
	var s models.QASettings
	q := db.Model(&models.QASettings{})
	if ws != nil && *ws != "" {
		q = q.Where("workspace_id = ?", *ws)
	} else {
		q = q.Where("user_id = ? AND (workspace_id IS NULL OR workspace_id = '')", userID)
	}
	if r := q.Order("updated_at DESC").Limit(1).Find(&s); r.Error != nil || r.RowsAffected == 0 {
		return models.QASettings{UserID: userID, WorkspaceID: ws}
	}
	return s
}

// radarSettings converts the stored row into the pure signal settings.
func radarSettings(s models.QASettings) qalink.Settings {
	dis := map[string]bool{}
	for _, k := range qalink.SplitPatterns(s.DisabledSignals) {
		dis[k] = true
	}
	return qalink.Settings{
		SLAHours:       s.SLAHours,
		LargeFiles:     s.LargeFiles,
		SourcePatterns: qalink.SplitPatterns(s.SourcePatterns),
		TestPatterns:   qalink.SplitPatterns(s.TestPatterns),
		Disabled:       dis,
	}
}

// Get — GET /api/qa/settings?workspaceId=.
func (h *QASettingsHandler) Get(c *gin.Context) {
	userID := c.GetString("userID")
	ws := qaWorkspaceParam(c)
	if ws != "" && qaWorkspaceRole(h.db, ws, userID) == NoRole {
		c.JSON(http.StatusNotFound, gin.H{"error": "Workspace not found"})
		return
	}
	var wsp *string
	if ws != "" {
		wsp = &ws
	}
	c.JSON(http.StatusOK, gin.H{"data": loadQASettings(h.db, userID, wsp)})
}

type qaSettingsInput struct {
	SLAHours        *int    `json:"slaHours"`
	LargeFiles      *int    `json:"largeFiles"`
	SourcePatterns  *string `json:"sourcePatterns"`
	TestPatterns    *string `json:"testPatterns"`
	DisabledSignals *string `json:"disabledSignals"`
	TestStatuses    *string `json:"testStatuses"`
	MergedDays      *int    `json:"mergedDays"`
	DigestHour      *int    `json:"digestHour"`
	ClearDigest     bool    `json:"clearDigest"`
	Timezone        *string `json:"timezone"`
	ReviewGuide     *string `json:"reviewGuide"`
}

// Put — PUT /api/qa/settings?workspaceId= (workspace editors).
func (h *QASettingsHandler) Put(c *gin.Context) {
	userID := c.GetString("userID")
	var in qaSettingsInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ws, ok := qaCreateWorkspace(h.db, c, userID)
	if !ok {
		return
	}
	s := loadQASettings(h.db, userID, ws)
	clampInt := func(dst *int, v *int, max int) {
		if v != nil {
			n := *v
			if n < 0 {
				n = 0
			}
			if n > max {
				n = max
			}
			*dst = n
		}
	}
	clampInt(&s.SLAHours, in.SLAHours, 24*30)
	clampInt(&s.LargeFiles, in.LargeFiles, 100000)
	clampInt(&s.MergedDays, in.MergedDays, 90)
	setText := func(dst *string, v *string, max int) bool {
		if v == nil {
			return true
		}
		t := strings.TrimSpace(*v)
		if len(t) > max {
			return false
		}
		*dst = t
		return true
	}
	if !setText(&s.ReviewGuide, in.ReviewGuide, 8000) || !setText(&s.SourcePatterns, in.SourcePatterns, 4000) || !setText(&s.TestPatterns, in.TestPatterns, 4000) ||
		!setText(&s.DisabledSignals, in.DisabledSignals, 500) || !setText(&s.TestStatuses, in.TestStatuses, 500) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "A setting is too long"})
		return
	}
	if in.Timezone != nil {
		if _, err := time.LoadLocation(*in.Timezone); err != nil || len(*in.Timezone) > 64 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "timezone must be an IANA name, e.g. Europe/Istanbul"})
			return
		}
		s.Timezone = *in.Timezone
	}
	if in.ClearDigest {
		s.DigestHour = nil
	} else if in.DigestHour != nil {
		if *in.DigestHour < 0 || *in.DigestHour > 23 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "digestHour must be 0–23"})
			return
		}
		v := *in.DigestHour
		s.DigestHour = &v
	}
	if s.ID == "" {
		s.ID = uuid.NewString()
		s.UserID = userID
		s.WorkspaceID = ws
	}
	if err := h.db.Save(&s).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save settings"})
		return
	}
	invalidateQACaches(userID, ws) // new thresholds apply at once
	c.JSON(http.StatusOK, gin.H{"data": s})
}

type qaSnoozeInput struct {
	ItemRef string `json:"itemRef" binding:"required"`
	Hours   int    `json:"hours"` // 0 = until tomorrow 09:00 server time
}

// Snooze — POST /api/qa/inbox/snooze {itemRef, hours}.
func (h *QASettingsHandler) Snooze(c *gin.Context) {
	userID := c.GetString("userID")
	var in qaSnoozeInput
	if err := c.ShouldBindJSON(&in); err != nil || len(in.ItemRef) > 300 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "itemRef is required (max 300 characters)"})
		return
	}
	now := time.Now()
	until := time.Date(now.Year(), now.Month(), now.Day()+1, 9, 0, 0, 0, now.Location())
	if in.Hours > 0 {
		if in.Hours > 24*30 {
			in.Hours = 24 * 30
		}
		until = now.Add(time.Duration(in.Hours) * time.Hour)
	}
	var row models.QAInboxSnooze
	if r := h.db.Where("user_id = ? AND item_ref = ?", userID, in.ItemRef).Limit(1).Find(&row); r.Error != nil || r.RowsAffected == 0 {
		row = models.QAInboxSnooze{ID: uuid.NewString(), UserID: userID, ItemRef: in.ItemRef}
	}
	row.Until = until
	if err := h.db.Save(&row).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to snooze"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": row})
}

// Unsnooze — DELETE /api/qa/inbox/snooze?itemRef=.
func (h *QASettingsHandler) Unsnooze(c *gin.Context) {
	userID := c.GetString("userID")
	h.db.Where("user_id = ? AND item_ref = ?", userID, c.Query("itemRef")).Delete(&models.QAInboxSnooze{})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"deleted": true}})
}

// activeSnoozes: itemRef → until, for the user's unexpired snoozes.
func activeSnoozes(db *gorm.DB, userID string, now time.Time) map[string]time.Time {
	var rows []models.QAInboxSnooze
	db.Where("user_id = ? AND until > ?", userID, now).Find(&rows)
	out := make(map[string]time.Time, len(rows))
	for _, r := range rows {
		out[r.ItemRef] = r.Until
	}
	return out
}

// invalidateQACaches drops the cached radars / Jira test queues of a QA
// space after its links or settings change: every member's for a workspace,
// the caller's own for a personal space.
func invalidateQACaches(userID string, ws *string) {
	suffix := "|"
	if ws != nil {
		suffix += *ws
	}
	drop := func(m *sync.Map) {
		m.Range(func(k, _ any) bool {
			key, _ := k.(string)
			if strings.HasSuffix(key, suffix) && (ws != nil && *ws != "" || strings.HasPrefix(key, userID+"|")) {
				m.Delete(k)
			}
			return true
		})
	}
	drop(&radarResults)
	drop(&inboxJiraCache)
}

// resetQACaches empties every QA radar cache (tests: each test has its own
// database but the caches are process-wide).
func resetQACaches() {
	for _, m := range []*sync.Map{&radarResults, &inboxJiraCache, &detailCache, &radarAuditAt} {
		m.Range(func(k, _ any) bool { m.Delete(k); return true })
	}
}
