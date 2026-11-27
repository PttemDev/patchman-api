package handlers

import (
	"net/http"
	"strings"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Own-connection mapping for a QA link (QALinkConnectionMap). Anyone who
// can see the link may map it, but only to connections they can use
// themselves; it changes only what they see.

type qaLinkMapInput struct {
	GitConnectionID  string `json:"gitConnectionId"`
	JiraConnectionID string `json:"jiraConnectionId"`
}

// PutMine — PUT /api/qa/links/:id/mine {gitConnectionId, jiraConnectionId}
// ("" = the link's connection; both "" removes the mapping).
func (h *QARadarHandler) PutMine(c *gin.Context) {
	userID := c.GetString("userID")
	var l models.QAProjectLink
	if !qaLoad(h.db, c, userID, c.Param("id"), &l, ViewerRole, "Link not found") {
		return
	}
	var in qaLinkMapInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	in.GitConnectionID, in.JiraConnectionID = strings.TrimSpace(in.GitConnectionID), strings.TrimSpace(in.JiraConnectionID)
	if in.GitConnectionID != "" {
		if _, _, err := h.git.conns.loadAccessible(in.GitConnectionID, userID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "You can't use that Git connection"})
			return
		}
	}
	if in.JiraConnectionID != "" {
		if _, _, err := h.jira.loadAccessible(in.JiraConnectionID, userID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "You can't use that Jira connection"})
			return
		}
	}
	var row models.QALinkConnectionMap
	found := false
	if r := h.db.Where("link_id = ? AND user_id = ?", l.ID, userID).Limit(1).Find(&row); r.Error == nil && r.RowsAffected > 0 {
		found = true
	}
	if in.GitConnectionID == "" && in.JiraConnectionID == "" {
		if found {
			h.db.Delete(&row)
		}
		invalidateQACaches(userID, l.WorkspaceID)
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"deleted": true}})
		return
	}
	if !found {
		row = models.QALinkConnectionMap{ID: uuid.NewString(), LinkID: l.ID, UserID: userID}
	}
	row.GitConnectionID, row.JiraConnectionID = in.GitConnectionID, in.JiraConnectionID
	if err := h.db.Save(&row).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save"})
		return
	}
	invalidateQACaches(userID, l.WorkspaceID)
	c.JSON(http.StatusOK, gin.H{"data": row})
}
