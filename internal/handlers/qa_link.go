package handlers

import (
	"net/http"
	"regexp"
	"strings"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// QALinkHandler manages Git project ↔ Jira project links for the CR + QA
// Hub (CR_QA_P0_PLAN P0.b). Access follows the QA rules (qa_access.go):
// personal links are the owner's, workspace links are read by the workspace
// and edited by its editors.
type QALinkHandler struct{ db *gorm.DB }

func NewQALinkHandler(db *gorm.DB) *QALinkHandler { return &QALinkHandler{db: db} }

var jiraProjectKeyRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,49}$`)

type qaLinkInput struct {
	ID                string  `json:"id"`
	GitConnectionID   *string `json:"gitConnectionId"`
	GitProject        *string `json:"gitProject"`
	JiraConnectionID  *string `json:"jiraConnectionId"`
	JiraProjectKey    *string `json:"jiraProjectKey"`
	KeyPattern        *string `json:"keyPattern"`
	TestEnvironmentID *string `json:"testEnvironmentId"`
}

func (in qaLinkInput) apply(l *models.QAProjectLink) string {
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = strings.TrimSpace(*v)
		}
	}
	set(&l.GitConnectionID, in.GitConnectionID)
	set(&l.GitProject, in.GitProject)
	set(&l.JiraConnectionID, in.JiraConnectionID)
	set(&l.JiraProjectKey, in.JiraProjectKey)
	set(&l.KeyPattern, in.KeyPattern)
	set(&l.TestEnvironment, in.TestEnvironmentID)
	l.JiraProjectKey = strings.ToUpper(l.JiraProjectKey)
	switch {
	case l.GitConnectionID == "" || l.GitProject == "":
		return "gitConnectionId and gitProject are required"
	case l.JiraProjectKey != "" && !jiraProjectKeyRE.MatchString(l.JiraProjectKey):
		return "jiraProjectKey must look like MER (letters, digits, _)"
	case l.JiraProjectKey != "" && l.JiraConnectionID == "":
		return "jiraConnectionId is required with jiraProjectKey"
	case len(l.KeyPattern) > 200:
		return "keyPattern is too long"
	}
	if l.KeyPattern != "" {
		if _, err := regexp.Compile(l.KeyPattern); err != nil {
			return "keyPattern is not a valid regular expression"
		}
	}
	return ""
}

// List — GET /api/qa/links?workspaceId=.
func (h *QALinkHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	scope, ok := qaListScope(h.db, c, userID)
	if !ok {
		return
	}
	links := []models.QAProjectLink{}
	if err := scope.Order("git_project ASC").Find(&links).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list links"})
		return
	}
	maps := qaLinkMaps(h.db, userID, links)
	for i := range links {
		links[i].HasWebhook = links[i].WebhookSecretHash != ""
		if m, ok := maps[links[i].ID]; ok {
			links[i].MyGitConnectionID, links[i].MyJiraConnectionID = m.GitConnectionID, m.JiraConnectionID
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": links})
}

// Create — POST /api/qa/links?workspaceId=.
func (h *QALinkHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")
	var in qaLinkInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ws, ok := qaCreateWorkspace(h.db, c, userID)
	if !ok {
		return
	}
	id, ok := pickID(in.ID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid id format"})
		return
	}
	l := models.QAProjectLink{ID: id, UserID: userID, WorkspaceID: ws}
	if msg := in.apply(&l); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	if err := h.db.Create(&l).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create link"})
		return
	}
	invalidateQACaches(userID, l.WorkspaceID)
	c.JSON(http.StatusCreated, gin.H{"data": l})
}

// Update — PUT /api/qa/links/:id.
func (h *QALinkHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	var l models.QAProjectLink
	if !qaLoad(h.db, c, userID, c.Param("id"), &l, EditorRole, "Link not found") {
		return
	}
	var in qaLinkInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if msg := in.apply(&l); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	if err := h.db.Save(&l).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update link"})
		return
	}
	invalidateQACaches(userID, l.WorkspaceID)
	c.JSON(http.StatusOK, gin.H{"data": l})
}

// Delete — DELETE /api/qa/links/:id (its creator or a workspace admin).
func (h *QALinkHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	var l models.QAProjectLink
	if !qaLoad(h.db, c, userID, c.Param("id"), &l, OwnerRole, "Link not found") {
		return
	}
	if err := h.db.Delete(&l).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete link"})
		return
	}
	h.db.Where("link_id = ?", l.ID).Delete(&models.QALinkConnectionMap{})
	invalidateQACaches(userID, l.WorkspaceID)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"deleted": true}})
}

// qaLinkMaps: the user's own-connection mappings for these links, by link.
func qaLinkMaps(db *gorm.DB, userID string, links []models.QAProjectLink) map[string]models.QALinkConnectionMap {
	out := map[string]models.QALinkConnectionMap{}
	if len(links) == 0 {
		return out
	}
	ids := make([]string, 0, len(links))
	for _, l := range links {
		ids = append(ids, l.ID)
	}
	var rows []models.QALinkConnectionMap
	db.Where("user_id = ? AND link_id IN ?", userID, ids).Find(&rows)
	for _, r := range rows {
		out[r.LinkID] = r
	}
	return out
}
