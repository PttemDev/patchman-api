package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"patchman-be/internal/models"
	"patchman-be/internal/sessions"
)

// SessionHandler lists and revokes signed-in devices (DESKTOP_SECURITY_RFC
// L8): a user can sign out a lost laptop, an admin can sign out all of a
// user's devices. Every revocation is audited (L9).
type SessionHandler struct {
	db *gorm.DB
}

func NewSessionHandler(db *gorm.DB) *SessionHandler { return &SessionHandler{db: db} }

type sessionDTO struct {
	models.UserSession
	Current bool `json:"current"`
}

func (h *SessionHandler) store(c *gin.Context) *sessions.Store {
	if sessions.Default == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "sessions are not tracked on this backend"})
		return nil
	}
	return sessions.Default
}

func (h *SessionHandler) list(c *gin.Context, userID, current string) {
	st := h.store(c)
	if st == nil {
		return
	}
	rows, err := st.List(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list sessions"})
		return
	}
	out := make([]sessionDTO, len(rows))
	for i, r := range rows {
		out[i] = sessionDTO{UserSession: r, Current: r.ID == current}
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *SessionHandler) audit(c *gin.Context, actor, targetUser string, ids []string, n int64, scope string) {
	details, _ := json.Marshal(gin.H{"targetUserId": targetUser, "sessionIds": ids, "revoked": n, "scope": scope})
	h.db.Create(&models.AuditLog{
		ID: uuid.New().String(), UserID: actor, Action: "revoke", Resource: "session",
		ResourceID: firstOr(ids, targetUser), Details: string(details),
		IPAddress: c.ClientIP(), UserAgent: c.Request.UserAgent(), CreatedAt: time.Now(),
	})
}

func firstOr(ids []string, fallback string) string {
	if len(ids) == 1 {
		return ids[0]
	}
	return fallback
}

// List — GET /api/auth/sessions
func (h *SessionHandler) List(c *gin.Context) {
	h.list(c, c.GetString("userID"), c.GetString("sessionID"))
}

// Revoke — DELETE /api/auth/sessions/:id
func (h *SessionHandler) Revoke(c *gin.Context) {
	st := h.store(c)
	if st == nil {
		return
	}
	uid, id := c.GetString("userID"), c.Param("id")
	n, err := st.Revoke(uid, []string{id}, "")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "revoke session"})
		return
	}
	if n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	h.audit(c, uid, uid, []string{id}, n, "one")
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"revoked": n}})
}

// RevokeOthers — POST /api/auth/sessions/revoke-others
func (h *SessionHandler) RevokeOthers(c *gin.Context) {
	st := h.store(c)
	if st == nil {
		return
	}
	uid, cur := c.GetString("userID"), c.GetString("sessionID")
	n, err := st.Revoke(uid, nil, cur)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "revoke sessions"})
		return
	}
	h.audit(c, uid, uid, nil, n, "others")
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"revoked": n}})
}

// Logout — POST /api/auth/logout: ends this device's session server-side.
func (h *SessionHandler) Logout(c *gin.Context) {
	uid, cur := c.GetString("userID"), c.GetString("sessionID")
	if sessions.Default != nil && cur != "" {
		if n, err := sessions.Default.Revoke(uid, []string{cur}, ""); err == nil && n > 0 {
			h.audit(c, uid, uid, []string{cur}, n, "logout")
		}
	}
	c.Status(http.StatusNoContent)
}

// AdminList — GET /api/admin/users/:id/sessions
func (h *SessionHandler) AdminList(c *gin.Context) {
	h.list(c, c.Param("id"), "")
}

// AdminRevokeAll — POST /api/admin/users/:id/sessions/revoke
func (h *SessionHandler) AdminRevokeAll(c *gin.Context) {
	st := h.store(c)
	if st == nil {
		return
	}
	target := c.Param("id")
	n, err := st.Revoke(target, nil, "")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "revoke sessions"})
		return
	}
	h.audit(c, c.GetString("userID"), target, nil, n, "admin-all")
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"revoked": n}})
}
