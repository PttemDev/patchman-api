package handlers

import (
	"net/http"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type CookieHandler struct {
	db *gorm.DB
}

func NewCookieHandler(db *gorm.DB) *CookieHandler {
	return &CookieHandler{db: db}
}

type setCookieInput struct {
	ID       string  `json:"id"`
	Domain   string  `json:"domain" binding:"required"`
	Name     string  `json:"name" binding:"required"`
	Value    string  `json:"value"`
	Path     string  `json:"path"`
	Secure   bool    `json:"secure"`
	HttpOnly bool    `json:"httpOnly"`
	SameSite string  `json:"sameSite"`
	ExpiresAt *string `json:"expiresAt"` // ISO 8601 string, parsed on frontend
}

func (h *CookieHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	domain := c.Query("domain")

	query := h.db.Where("user_id = ?", userID)
	if domain != "" {
		query = query.Where("domain = ?", domain)
	}

	var cookies []models.CookieJar
	if err := query.Order("domain ASC, name ASC").Find(&cookies).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch cookies"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": cookies})
}

func (h *CookieHandler) Set(c *gin.Context) {
	userID := c.GetString("userID")

	var input setCookieInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	path := input.Path
	if path == "" {
		path = "/"
	}

	// Check if a cookie with same domain+name+path already exists for this user
	var existing models.CookieJar
	err := h.db.Where("user_id = ? AND domain = ? AND name = ? AND path = ?",
		userID, input.Domain, input.Name, path).First(&existing).Error

	if err == nil {
		// Update existing cookie
		updates := map[string]interface{}{
			"value":     input.Value,
			"secure":    input.Secure,
			"http_only": input.HttpOnly,
			"same_site": input.SameSite,
		}

		if err := h.db.Model(&existing).Updates(updates).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update cookie"})
			return
		}

		h.db.First(&existing, "id = ?", existing.ID)
		c.JSON(http.StatusOK, gin.H{"data": existing})
		return
	}

	// Create new cookie
	newID, ok := pickID(input.ID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid id format"})
		return
	}

	cookie := models.CookieJar{
		ID:       newID,
		UserID:   userID,
		Domain:   input.Domain,
		Name:     input.Name,
		Value:    input.Value,
		Path:     path,
		Secure:   input.Secure,
		HttpOnly: input.HttpOnly,
		SameSite: input.SameSite,
	}

	if err := h.db.Create(&cookie).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create cookie"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"data": cookie})
}

func (h *CookieHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var cookie models.CookieJar
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&cookie).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Cookie not found"})
		return
	}

	if err := h.db.Delete(&cookie).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete cookie"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Cookie deleted"}})
}

func (h *CookieHandler) Clear(c *gin.Context) {
	userID := c.GetString("userID")
	domain := c.Query("domain")

	query := h.db.Where("user_id = ?", userID)
	if domain != "" {
		query = query.Where("domain = ?", domain)
	}

	if err := query.Delete(&models.CookieJar{}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to clear cookies"})
		return
	}

	msg := "All cookies cleared"
	if domain != "" {
		msg = "Cookies cleared for domain: " + domain
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": msg}})
}
