package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/mcpscope"
	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// MCPTokenHandler manages personal access tokens used by external MCP
// clients to reach the /api/mcp Streamable-HTTP endpoint. These routes
// are mounted under the regular JWT-protected /api group, so a token is
// always minted on behalf of the signed-in user.
type MCPTokenHandler struct {
	db *gorm.DB
}

func NewMCPTokenHandler(db *gorm.DB) *MCPTokenHandler {
	return &MCPTokenHandler{db: db}
}

// hashToken returns the hex SHA-256 of a raw token. Used both at create
// time (to persist) and at auth time (to look up).
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// ResolveMCPToken maps a raw MCP token to its owning user id and scopes,
// bumping last_used_at. Returns ("", nil, false) for unknown/empty tokens. The
// returned scopes pointer is the token's persisted scope list (nil = all tool
// groups). Lives here so the /api/mcp transport can reuse the same hashing.
func ResolveMCPToken(db *gorm.DB, raw string) (string, *string, bool) {
	tok, ok := ResolveMCPTokenInfo(db, raw)
	return tok.UserID, tok.Scopes, ok
}

// ResolveMCPTokenInfo is ResolveMCPToken returning the whole token row (its
// id and the name of the agent that last connected with it, too).
func ResolveMCPTokenInfo(db *gorm.DB, raw string) (models.MCPToken, bool) {
	var tok models.MCPToken
	if raw == "" {
		return tok, false
	}
	if err := db.Where("token_hash = ?", hashToken(raw)).First(&tok).Error; err != nil {
		return models.MCPToken{}, false
	}
	// Best-effort "last used" bookkeeping. This runs on every MCP request,
	// and an active session fires many tool calls a second — issuing an
	// UPDATE for each one needlessly hammers the write path. A minute of
	// granularity is plenty for a "last used" indicator, so throttle the
	// write and keep token auth read-mostly.
	now := time.Now()
	if tok.LastUsedAt == nil || now.Sub(*tok.LastUsedAt) > time.Minute {
		db.Model(&models.MCPToken{}).Where("id = ?", tok.ID).Update("last_used_at", &now)
	}
	return tok, true
}

// RecordMCPClient remembers which agent connected with a token (from its MCP
// initialize). MCP runs stateless here, so later tool calls only know the
// token — this is how they know the agent.
func RecordMCPClient(db *gorm.DB, tokenID, agent string) {
	if tokenID == "" || agent == "" {
		return
	}
	db.Model(&models.MCPToken{}).Where("id = ? AND (client_name IS NULL OR client_name <> ?)", tokenID, agent).Update("client_name", agent)
}

// EnsureLocalMCPToken makes the mcp_tokens row for the embedded local user
// match the plaintext held in <db>.mcp.token. It exists because hashToken is
// unexported, so the embedded startup can't derive the hash itself.
// Idempotent: creates the row when absent and updates the hash/prefix in
// place when the file was regenerated — keyed by the fixed marker name so
// regeneration updates the same row instead of orphaning old ones.
func EnsureLocalMCPToken(db *gorm.DB, userID, raw, name string) error {
	hash := hashToken(raw)
	prefix := raw
	if len(prefix) > 13 {
		prefix = prefix[:13] // "pmcp_" + 8 hex chars
	}
	var existing models.MCPToken
	err := db.Where("user_id = ? AND name = ?", userID, name).First(&existing).Error
	switch {
	case err == nil:
		if existing.TokenHash != hash {
			return db.Model(&models.MCPToken{}).Where("id = ?", existing.ID).
				Updates(map[string]any{"token_hash": hash, "prefix": prefix}).Error
		}
		return nil
	case errors.Is(err, gorm.ErrRecordNotFound):
		return db.Create(&models.MCPToken{
			ID:        uuid.New().String(),
			UserID:    userID,
			Name:      name,
			TokenHash: hash,
			Prefix:    prefix,
		}).Error
	default:
		return err
	}
}

type createMCPTokenInput struct {
	Name string `json:"name"`
}

// Create issues a new token. The raw value is returned exactly once.
func (h *MCPTokenHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authorization required"})
		return
	}
	// Body is optional — callers may POST with no payload to mint a
	// token with the default name. But if a body IS sent and it's
	// malformed JSON, surface that as a 400 instead of silently falling
	// back to the default name.
	var in createMCPTokenInput
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&in); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON body"})
			return
		}
	}
	if in.Name == "" {
		in.Name = "MCP token"
	}

	// 32 random bytes → 64 hex chars, prefixed so it's recognisable.
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate token"})
		return
	}
	raw := "pmcp_" + hex.EncodeToString(buf)

	tok := models.MCPToken{
		ID:        uuid.New().String(),
		UserID:    userID,
		Name:      in.Name,
		TokenHash: hashToken(raw),
		Prefix:    raw[:13], // "pmcp_" + 8 hex chars
	}
	if err := h.db.Create(&tok).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store token"})
		return
	}

	// Return the raw token ONCE alongside the stored record.
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{
		"token":  raw,
		"id":     tok.ID,
		"name":   tok.Name,
		"prefix": tok.Prefix,
	}})
}

// List returns the caller's tokens (never the raw value).
func (h *MCPTokenHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	var toks []models.MCPToken
	if err := h.db.Where("user_id = ?", userID).Order("created_at desc").Find(&toks).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list tokens"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": toks})
}

type updateMCPScopesInput struct {
	// Pointer distinguishes an absent/null value (→ all tool groups) from an
	// explicit [] (→ no tool groups). Non-nil is the exact enabled-group list.
	Scopes *[]string `json:"scopes"`
}

// UpdateScopes sets which tool groups a token may use. Body `{"scopes": null}`
// (or absent) grants all groups; `{"scopes": ["db","http"]}` restricts to those;
// `{"scopes": []}` disables every tool. Ownership-scoped: a user can only edit
// their own tokens.
func (h *MCPTokenHandler) UpdateScopes(c *gin.Context) {
	userID := c.GetString("userID")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authorization required"})
		return
	}
	id := c.Param("id")

	var in updateMCPScopesInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON body"})
		return
	}

	var scopes *string
	if in.Scopes != nil {
		// Validate + dedupe every group key so a typo ("dbb") is a 400 rather
		// than a silently-dropped permission.
		seen := map[string]bool{}
		clean := make([]string, 0, len(*in.Scopes))
		for _, g := range *in.Scopes {
			g = strings.TrimSpace(g)
			if g == "" {
				continue
			}
			if !mcpscope.IsValidGroup(g) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Unknown tool group: " + g})
				return
			}
			if !seen[g] {
				seen[g] = true
				clean = append(clean, g)
			}
		}
		joined := strings.Join(clean, ",")
		scopes = &joined // may be "" → no tool groups
	}

	// Ownership-scoped update; a map is used so a nil *string writes SQL NULL
	// (GORM would otherwise skip a zero-value field).
	res := h.db.Model(&models.MCPToken{}).
		Where("id = ? AND user_id = ?", id, userID).
		Updates(map[string]any{"scopes": scopes})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update token"})
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Token not found"})
		return
	}

	var tok models.MCPToken
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&tok).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load token"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": tok})
}

// Delete revokes a token by id (must belong to the caller).
func (h *MCPTokenHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	res := h.db.Where("id = ? AND user_id = ?", id, userID).Delete(&models.MCPToken{})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to revoke token"})
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Token not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"revoked": true}})
}
