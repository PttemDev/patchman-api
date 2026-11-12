package handlers

import (
	"net/http"
	"time"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// RabbitGrantHandler manages delegated access on a RabbitMQ connection.
// role='read' → view, peek (requeue) and tail; role='write' → also publish,
// purge, move, create/delete and close connections. Owner-only mutations;
// anyone with access can list. Mirrors GrafanaGrantHandler.
type RabbitGrantHandler struct {
	db    *gorm.DB
	conns *RabbitConnectionHandler
}

func NewRabbitGrantHandler(db *gorm.DB, conns *RabbitConnectionHandler) *RabbitGrantHandler {
	return &RabbitGrantHandler{db: db, conns: conns}
}

type rabbitGrantInput struct {
	SubjectKind string `json:"subjectKind" binding:"required"`
	SubjectID   string `json:"subjectId" binding:"required"`
	Role        string `json:"role"` // read | write (default read)
}

func (h *RabbitGrantHandler) Add(c *gin.Context) {
	userID := c.GetString("userID")
	connID := c.Param("id")
	var conn models.RabbitConnection
	if err := h.db.Where("id = ? AND user_id = ?", connID, userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	var input rabbitGrantInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if input.SubjectKind != "user" && input.SubjectKind != "team" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "subjectKind must be 'user' or 'team'"})
		return
	}
	role := input.Role
	if role == "" {
		role = "read"
	}
	if role != "read" && role != "write" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "role must be 'read' or 'write'"})
		return
	}
	grant := models.RabbitConnectionGrant{
		ID:           uuid.New().String(),
		ConnectionID: connID,
		SubjectKind:  input.SubjectKind,
		SubjectID:    input.SubjectID,
		Role:         role,
		GrantedBy:    userID,
		CreatedAt:    time.Now(),
	}
	if err := h.db.Create(&grant).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save grant"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": grant})
}

func (h *RabbitGrantHandler) List(c *gin.Context) {
	connID := c.Param("id")
	if _, _, err := h.conns.loadAccessible(connID, c.GetString("userID")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	var grants []models.RabbitConnectionGrant
	if err := h.db.Where("connection_id = ?", connID).Order("created_at DESC").Find(&grants).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list grants"})
		return
	}
	type dto struct {
		ID          string `json:"id"`
		SubjectKind string `json:"subjectKind"`
		SubjectID   string `json:"subjectId"`
		Role        string `json:"role"`
		Label       string `json:"label"`
		GrantedBy   string `json:"grantedBy"`
		CreatedAt   string `json:"createdAt"`
	}
	out := make([]dto, 0, len(grants))
	for _, g := range grants {
		label := g.SubjectID
		if g.SubjectKind == "user" {
			var u struct{ ID, Email, Name string }
			h.db.Table("users").Select("id, email, name").Where("id = ?", g.SubjectID).Scan(&u)
			if u.Name != "" {
				label = u.Name
			} else if u.Email != "" {
				label = u.Email
			}
		} else if g.SubjectKind == "team" {
			var t struct{ ID, Name string }
			h.db.Table("teams").Select("id, name").Where("id = ? AND deleted_at IS NULL", g.SubjectID).Scan(&t)
			if t.Name != "" {
				label = t.Name
			}
		}
		out = append(out, dto{
			ID: g.ID, SubjectKind: g.SubjectKind, SubjectID: g.SubjectID,
			Role: g.Role, Label: label, GrantedBy: g.GrantedBy,
			CreatedAt: g.CreatedAt.Format(time.RFC3339),
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *RabbitGrantHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	connID := c.Param("id")
	grantID := c.Param("grantId")
	var conn models.RabbitConnection
	if err := h.db.Where("id = ? AND user_id = ?", connID, userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	res := h.db.Where("id = ? AND connection_id = ?", grantID, connID).Delete(&models.RabbitConnectionGrant{})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete grant"})
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Grant not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"deleted": true}})
}
