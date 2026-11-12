package handlers

import (
	"net/http"
	"strconv"
	"time"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const historyRetentionDays = 30

type HistoryHandler struct {
	db *gorm.DB
}

func NewHistoryHandler(db *gorm.DB) *HistoryHandler {
	return &HistoryHandler{db: db}
}

func (h *HistoryHandler) List(c *gin.Context) {
	userID := c.GetString("userID")

	// Cleanup entries older than 30 days
	cutoff := time.Now().AddDate(0, 0, -historyRetentionDays)
	h.db.Where("user_id = ? AND created_at < ?", userID, cutoff).Delete(&models.History{})

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))

	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 200 {
		limit = 50
	}

	offset := (page - 1) * limit

	var total int64
	h.db.Model(&models.History{}).Where("user_id = ?", userID).Count(&total)

	var histories []models.History
	if err := h.db.Where("user_id = ?", userID).
		Order("created_at DESC").
		Offset(offset).Limit(limit).
		Find(&histories).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch history"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"items":      histories,
			"total":      total,
			"page":       page,
			"limit":      limit,
			"totalPages": (total + int64(limit) - 1) / int64(limit),
		},
	})
}

func (h *HistoryHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var history models.History
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&history).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "History entry not found"})
		return
	}

	if err := h.db.Delete(&history).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete history entry"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "History entry deleted"}})
}

func (h *HistoryHandler) ClearAll(c *gin.Context) {
	userID := c.GetString("userID")

	if err := h.db.Where("user_id = ?", userID).Delete(&models.History{}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to clear history"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "History cleared"}})
}
