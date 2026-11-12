package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/authtoken"
	"patchman-be/internal/config"
	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

type AuthHandler struct {
	db        *gorm.DB
	cfg       *config.Config
	jwtSecret string
}

func NewAuthHandler(db *gorm.DB, cfg *config.Config, jwtSecret string) *AuthHandler {
	return &AuthHandler{db: db, cfg: cfg, jwtSecret: jwtSecret}
}

// emailDomainError returns the 403 message rendered when an email fails
// the tenant's allow-list. Pulled out so Register/Login/SSO produce
// identical wording regardless of which allow-list is configured.
func emailDomainError(cfg *config.Config) string {
	label := cfg.EmailDomainsLabel()
	if label == "" {
		// Should not be reached — EmailAllowed already returned true.
		return "Email address is not allowed"
	}
	return fmt.Sprintf("Only %s email addresses are allowed", label)
}

type registerInput struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required,min=6"`
	Name     string `json:"name" binding:"required"`
}

type loginInput struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required"`
}

func (h *AuthHandler) Register(c *gin.Context) {
	var input registerInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if !h.cfg.EmailAllowed(input.Email) {
		c.JSON(http.StatusForbidden, gin.H{"error": emailDomainError(h.cfg)})
		return
	}

	// Check if email already exists
	var existing models.User
	if err := h.db.Where("email = ?", input.Email).First(&existing).Error; err == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "Email already registered"})
		return
	}

	// Hash password
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to hash password"})
		return
	}

	user := models.User{
		ID:       uuid.New().String(),
		Email:    input.Email,
		Password: string(hashedPassword),
		Name:     input.Name,
	}

	if err := h.db.Create(&user).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create user"})
		return
	}

	token, err := h.generateToken(user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate token"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"data": gin.H{
			"token": token,
			"user":  user,
		},
	})
}

func (h *AuthHandler) Login(c *gin.Context) {
	var input loginInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if !h.cfg.EmailAllowed(input.Email) {
		c.JSON(http.StatusForbidden, gin.H{"error": emailDomainError(h.cfg)})
		return
	}

	var user models.User
	if err := h.db.Where("email = ?", input.Email).First(&user).Error; err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid email or password"})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(input.Password)); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid email or password"})
		return
	}

	token, err := h.generateToken(user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate token"})
		return
	}

	// Stamp last-login (and last-seen/version) for the admin dashboard.
	now := time.Now()
	updates := map[string]any{"last_login_at": now, "last_seen_at": now}
	if v := clientVersion(c); v != "" {
		updates["last_seen_version"] = v
	}
	h.db.Model(&models.User{}).Where("id = ?", user.ID).Updates(updates)
	user.LastLoginAt = &now
	user.LastSeenAt = &now

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"token": token,
			"user":  user,
		},
	})
}

func (h *AuthHandler) Me(c *gin.Context) {
	userID := c.GetString("userID")

	var user models.User
	if err := h.db.First(&user, "id = ?", userID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}

	// Opportunistically refresh last-seen + reported client version on each
	// /me poll — the cheapest seam that reaches the server without a
	// per-request write. Fire-and-forget; a failure here must not block auth.
	now := time.Now()
	updates := map[string]any{"last_seen_at": now}
	if v := clientVersion(c); v != "" {
		updates["last_seen_version"] = v
	}
	h.db.Model(&models.User{}).Where("id = ?", userID).Updates(updates)

	c.JSON(http.StatusOK, gin.H{"data": user})
}

// clientVersion reads and sanitizes the X-Client-Version header the FE stamps
// on every request (empty when an older client doesn't send it).
func clientVersion(c *gin.Context) string {
	v := strings.TrimSpace(c.GetHeader("X-Client-Version"))
	if len(v) > 32 {
		v = v[:32]
	}
	return v
}

func (h *AuthHandler) SearchUsers(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	if q == "" {
		c.JSON(http.StatusOK, gin.H{"data": []interface{}{}})
		return
	}

	var users []models.User
	search := "%" + strings.ToLower(q) + "%"
	h.db.Where("LOWER(name) LIKE ? OR LOWER(email) LIKE ?", search, search).
		Limit(10).
		Find(&users)

	type userResult struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}

	results := make([]userResult, len(users))
	for i, u := range users {
		results[i] = userResult{ID: u.ID, Name: u.Name, Email: u.Email}
	}

	c.JSON(http.StatusOK, gin.H{"data": results})
}

func (h *AuthHandler) generateToken(userID string) (string, error) {
	return authtoken.Mint(h.jwtSecret, userID, h.cfg.SessionTTL)
}
