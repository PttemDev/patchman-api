package handlers

import (
	"net/http"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type CommentHandler struct {
	db *gorm.DB
}

func NewCommentHandler(db *gorm.DB) *CommentHandler {
	return &CommentHandler{db: db}
}

type createCommentInput struct {
	TargetType string  `json:"targetType" binding:"required"`
	TargetID   string  `json:"targetId" binding:"required"`
	Content    string  `json:"content" binding:"required"`
	ParentID   *string `json:"parentId"`
}

type updateCommentInput struct {
	Content string `json:"content" binding:"required"`
}

func (h *CommentHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")

	var input createCommentInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	comment := models.Comment{
		ID:         uuid.New().String(),
		UserID:     userID,
		TargetType: input.TargetType,
		TargetID:   input.TargetID,
		Content:    input.Content,
		ParentID:   input.ParentID,
	}

	if err := h.db.Create(&comment).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create comment"})
		return
	}

	// Reload with User relation
	h.db.Preload("User").First(&comment, "id = ?", comment.ID)

	c.JSON(http.StatusCreated, gin.H{"data": comment})
}

func (h *CommentHandler) List(c *gin.Context) {
	targetType := c.Query("targetType")
	targetID := c.Query("targetId")

	if targetType == "" || targetID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "targetType and targetId are required"})
		return
	}

	// Fetch top-level comments (no parent)
	var comments []models.Comment
	if err := h.db.Preload("User").
		Where("target_type = ? AND target_id = ? AND parent_id IS NULL", targetType, targetID).
		Order("created_at ASC").
		Find(&comments).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch comments"})
		return
	}

	// Build threaded response
	type commentWithReplies struct {
		models.Comment
		Replies []models.Comment `json:"replies"`
	}

	var threaded []commentWithReplies
	for _, comment := range comments {
		var replies []models.Comment
		h.db.Preload("User").
			Where("parent_id = ?", comment.ID).
			Order("created_at ASC").
			Find(&replies)

		threaded = append(threaded, commentWithReplies{
			Comment: comment,
			Replies: replies,
		})
	}

	c.JSON(http.StatusOK, gin.H{"data": threaded})
}

func (h *CommentHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var comment models.Comment
	if err := h.db.Where("id = ?", id).First(&comment).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Comment not found"})
		return
	}

	// Only own comments
	if comment.UserID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "You can only edit your own comments"})
		return
	}

	var input updateCommentInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.db.Model(&comment).Update("content", input.Content).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update comment"})
		return
	}

	h.db.Preload("User").First(&comment, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{"data": comment})
}

func (h *CommentHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var comment models.Comment
	if err := h.db.Where("id = ?", id).First(&comment).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Comment not found"})
		return
	}

	// Only own comments
	if comment.UserID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "You can only delete your own comments"})
		return
	}

	// Also delete replies to this comment
	h.db.Where("parent_id = ?", id).Delete(&models.Comment{})

	if err := h.db.Delete(&comment).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete comment"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Comment deleted"}})
}
