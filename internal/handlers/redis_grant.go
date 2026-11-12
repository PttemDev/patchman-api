package handlers

import (
	"net/http"
	"time"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// RedisGrantHandler manages delegated access on a Redis connection.
// role='read' → browse keys + read values + read commands + peek;
// role='write' → mutating commands + DEL + set-TTL. Owner-only mutations;
// anyone with access can list. Mirrors KafkaGrantHandler.
type RedisGrantHandler struct {
	db *gorm.DB
}

func NewRedisGrantHandler(db *gorm.DB) *RedisGrantHandler { return &RedisGrantHandler{db: db} }

type redisGrantInput struct {
	SubjectKind string `json:"subjectKind" binding:"required"`
	SubjectID   string `json:"subjectId" binding:"required"`
	Role        string `json:"role"` // read | write (default read)
}

func (h *RedisGrantHandler) Add(c *gin.Context) {
	userID := c.GetString("userID")
	connID := c.Param("id")
	var conn models.RedisConnection
	if err := h.db.Where("id = ? AND user_id = ?", connID, userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	var input redisGrantInput
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
	grant := models.RedisConnectionGrant{
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

func (h *RedisGrantHandler) List(c *gin.Context) {
	connID := c.Param("id")
	var grants []models.RedisConnectionGrant
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

func (h *RedisGrantHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	connID := c.Param("id")
	grantID := c.Param("grantId")
	var conn models.RedisConnection
	if err := h.db.Where("id = ? AND user_id = ?", connID, userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	res := h.db.Where("id = ? AND connection_id = ?", grantID, connID).Delete(&models.RedisConnectionGrant{})
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
