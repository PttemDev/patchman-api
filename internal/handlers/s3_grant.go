package handlers

import (
	"net/http"
	"time"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// S3GrantHandler manages delegated access on an S3 connection.
// read='browse+get', write='put/delete/copy'. Owner-only mutations.
type S3GrantHandler struct {
	db *gorm.DB
}

func NewS3GrantHandler(db *gorm.DB) *S3GrantHandler { return &S3GrantHandler{db: db} }

type s3GrantInput struct {
	SubjectKind string `json:"subjectKind" binding:"required"`
	SubjectID   string `json:"subjectId" binding:"required"`
	Role        string `json:"role"`
}

func (h *S3GrantHandler) Add(c *gin.Context) {
	userID := c.GetString("userID")
	connID := c.Param("id")
	var conn models.S3Connection
	if err := h.db.Where("id = ? AND user_id = ?", connID, userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	var input s3GrantInput
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
	grant := models.S3ConnectionGrant{
		ID: uuid.New().String(), ConnectionID: connID,
		SubjectKind: input.SubjectKind, SubjectID: input.SubjectID,
		Role: role, GrantedBy: userID, CreatedAt: time.Now(),
	}
	if err := h.db.Create(&grant).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save grant"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": grant})
}

func (h *S3GrantHandler) List(c *gin.Context) {
	connID := c.Param("id")
	var grants []models.S3ConnectionGrant
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

func (h *S3GrantHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	connID := c.Param("id")
	grantID := c.Param("grantId")
	var conn models.S3Connection
	if err := h.db.Where("id = ? AND user_id = ?", connID, userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	res := h.db.Where("id = ? AND connection_id = ?", grantID, connID).Delete(&models.S3ConnectionGrant{})
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
