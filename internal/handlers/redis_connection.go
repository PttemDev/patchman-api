package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/redisquery"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// RedisConnectionHandler covers the Redis connection surface — CRUD, the
// one-shot Test probe, owner-only secret reveal. Mirrors
// KafkaConnectionHandler; key browsing + command console live in
// redis_keys.go / redis_command.go because they need per-op permission +
// guardrails.
type RedisConnectionHandler struct {
	db       *gorm.DB
	cipher   *services.TokenCipher
	registry *redisquery.Registry
}

func NewRedisConnectionHandler(db *gorm.DB, cipher *services.TokenCipher, reg *redisquery.Registry) *RedisConnectionHandler {
	return &RedisConnectionHandler{db: db, cipher: cipher, registry: reg}
}

type createRedisInput struct {
	ID               string         `json:"id"`
	Name             string         `json:"name" binding:"required"`
	Address          string         `json:"address" binding:"required"`
	Mode             string         `json:"mode"`
	SentinelMaster   string         `json:"sentinelMaster"`
	Username         string         `json:"username"`
	Password         string         `json:"password"`
	SentinelPassword string         `json:"sentinelPassword"`
	DBIndex          int            `json:"dbIndex"`
	TLS              bool           `json:"tls"`
	Options          map[string]any `json:"options"`
	WorkspaceID      string         `json:"workspaceId"`
	PrimaryScope     string         `json:"primaryScope"`
	AllowWrite       bool           `json:"allowWrite"`
}

type updateRedisInput struct {
	Name             *string        `json:"name"`
	Address          *string        `json:"address"`
	Mode             *string        `json:"mode"`
	SentinelMaster   *string        `json:"sentinelMaster"`
	Username         *string        `json:"username"`
	Password         *string        `json:"password"`         // nil = keep existing
	SentinelPassword *string        `json:"sentinelPassword"` // nil = keep existing
	DBIndex          *int           `json:"dbIndex"`
	TLS              *bool          `json:"tls"`
	Options          map[string]any `json:"options"`
	PrimaryScope     *string        `json:"primaryScope"`
	AllowWrite       *bool          `json:"allowWrite"` // read-write master switch; nil = unchanged
}

type redisConnectionDTO struct {
	ID                  string    `json:"id"`
	UserID              string    `json:"userId"`
	WorkspaceID         string    `json:"workspaceId,omitempty"`
	Name                string    `json:"name"`
	Address             string    `json:"address"`
	Mode                string    `json:"mode"`
	SentinelMaster      string    `json:"sentinelMaster,omitempty"`
	Username            string    `json:"username,omitempty"`
	HasPassword         bool      `json:"hasPassword"`
	HasSentinelPassword bool      `json:"hasSentinelPassword"`
	DBIndex             int       `json:"dbIndex"`
	TLS                 bool      `json:"tls"`
	Options             any       `json:"options,omitempty"`
	OwnerName           string    `json:"ownerName,omitempty"`
	Shared              bool      `json:"shared"`
	GrantRole           string    `json:"grantRole,omitempty"`
	PrimaryScope        string    `json:"primaryScope,omitempty"`
	AllowWrite          bool      `json:"allowWrite"`
	CreatedAt           time.Time `json:"createdAt"`
	UpdatedAt           time.Time `json:"updatedAt"`
}

func validRedisMode(m string) bool {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "standalone", "sentinel", "cluster", "":
		return true
	}
	return false
}

func (h *RedisConnectionHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")
	var input createRedisInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !validRedisMode(input.Mode) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "mode must be standalone, sentinel, or cluster"})
		return
	}
	mode := strings.ToLower(strings.TrimSpace(input.Mode))
	if mode == "" {
		mode = "standalone"
	}
	if mode == "sentinel" && strings.TrimSpace(input.SentinelMaster) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "sentinelMaster is required for sentinel mode"})
		return
	}
	passEnc, err := h.encOrEmpty(input.Password)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt password"})
		return
	}
	sentEnc, err := h.encOrEmpty(input.SentinelPassword)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt sentinel password"})
		return
	}
	optBytes, _ := json.Marshal(or(input.Options, map[string]any{}))
	id := strings.TrimSpace(input.ID)
	if id == "" {
		id = uuid.New().String()
	}
	row := models.RedisConnection{
		ID:                  id,
		UserID:              userID,
		WorkspaceID:         input.WorkspaceID,
		Name:                input.Name,
		Address:             input.Address,
		Mode:                mode,
		SentinelMaster:      input.SentinelMaster,
		Username:            input.Username,
		PasswordEnc:         passEnc,
		SentinelPasswordEnc: sentEnc,
		DBIndex:             input.DBIndex,
		TLS:                 input.TLS,
		AllowWrite:          input.AllowWrite,
		OptionsJSON:         string(optBytes),
		PrimaryScope:        normalizeScope(input.PrimaryScope),
	}
	if err := h.db.Create(&row).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save connection"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": toRedisDTO(row, "", false, "owner")})
}

func (h *RedisConnectionHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	var own []models.RedisConnection
	if err := h.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&own).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list connections"})
		return
	}
	var shared []models.RedisConnection
	err := h.db.
		Where(`id IN (
			SELECT g.connection_id FROM redis_connection_grants g
			WHERE (g.subject_kind = 'user' AND g.subject_id = ?)
			   OR (g.subject_kind = 'team' AND g.subject_id IN (
				   SELECT tm.team_id FROM team_members tm WHERE tm.user_id = ?
			   ))
		) AND user_id <> ?`, userID, userID, userID).
		Order("created_at DESC").Find(&shared).Error
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list shared connections"})
		return
	}
	owners := map[string]string{}
	if len(shared) > 0 {
		ids := make([]string, 0, len(shared))
		for _, s := range shared {
			ids = append(ids, s.UserID)
		}
		var rows []struct{ ID, Email, Name string }
		h.db.Table("users").Select("id, email, name").Where("id IN ?", ids).Scan(&rows)
		for _, r := range rows {
			if r.Name != "" {
				owners[r.ID] = r.Name
			} else {
				owners[r.ID] = r.Email
			}
		}
	}
	out := make([]redisConnectionDTO, 0, len(own)+len(shared))
	for _, cn := range own {
		out = append(out, toRedisDTO(cn, "", false, "owner"))
	}
	for _, cn := range shared {
		role := h.grantRole(cn.ID, userID)
		out = append(out, toRedisDTO(cn, owners[cn.UserID], true, role))
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *RedisConnectionHandler) Get(c *gin.Context) {
	userID := c.GetString("userID")
	conn, role, err := h.loadAccessible(c.Param("id"), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": toRedisDTO(*conn, "", conn.UserID != userID, role)})
}

func (h *RedisConnectionHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn models.RedisConnection
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	var input updateRedisInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updates := map[string]any{}
	if input.Name != nil {
		updates["name"] = *input.Name
	}
	if input.Address != nil {
		updates["address"] = *input.Address
	}
	if input.Mode != nil {
		if !validRedisMode(*input.Mode) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid mode"})
			return
		}
		updates["mode"] = strings.ToLower(strings.TrimSpace(*input.Mode))
	}
	if input.SentinelMaster != nil {
		updates["sentinel_master"] = *input.SentinelMaster
	}
	if input.Username != nil {
		updates["username"] = *input.Username
	}
	if input.Password != nil {
		if *input.Password == "" {
			updates["password_enc"] = ""
		} else {
			enc, err := h.cipher.Encrypt(*input.Password)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt password"})
				return
			}
			updates["password_enc"] = enc
		}
	}
	if input.SentinelPassword != nil {
		if *input.SentinelPassword == "" {
			updates["sentinel_password_enc"] = ""
		} else {
			enc, err := h.cipher.Encrypt(*input.SentinelPassword)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt sentinel password"})
				return
			}
			updates["sentinel_password_enc"] = enc
		}
	}
	if input.DBIndex != nil {
		updates["db_index"] = *input.DBIndex
	}
	if input.TLS != nil {
		updates["tls"] = *input.TLS
	}
	// A map update writes zero values, so switching back to read-only sticks.
	if input.AllowWrite != nil {
		updates["allow_write"] = *input.AllowWrite
	}
	if input.Options != nil {
		b, _ := json.Marshal(input.Options)
		updates["options_json"] = string(b)
	}
	if input.PrimaryScope != nil {
		updates["primary_scope"] = normalizeScope(*input.PrimaryScope)
	}
	if err := h.db.Model(&conn).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update connection"})
		return
	}
	h.registry.Close(id) // pooled clients may be stale after an edit
	h.db.First(&conn, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{"data": toRedisDTO(conn, "", false, "owner")})
}

func (h *RedisConnectionHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	res := h.db.Where("id = ? AND user_id = ?", id, userID).Delete(&models.RedisConnection{})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete connection"})
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	h.db.Where("connection_id = ?", id).Delete(&models.RedisConnectionGrant{})
	h.registry.Close(id)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "deleted"}})
}

func (h *RedisConnectionHandler) Test(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn *models.RedisConnection
	if id != "" && id != "new" {
		var existing models.RedisConnection
		if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&existing).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
			return
		}
		conn = &existing
	} else {
		var input createRedisInput
		if err := c.ShouldBindJSON(&input); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if !validRedisMode(input.Mode) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid mode"})
			return
		}
		passEnc, err := h.encOrEmpty(input.Password)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt"})
			return
		}
		sentEnc, err := h.encOrEmpty(input.SentinelPassword)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt"})
			return
		}
		mode := strings.ToLower(strings.TrimSpace(input.Mode))
		if mode == "" {
			mode = "standalone"
		}
		optBytes, _ := json.Marshal(or(input.Options, map[string]any{}))
		conn = &models.RedisConnection{
			Address:             input.Address,
			Mode:                mode,
			SentinelMaster:      input.SentinelMaster,
			Username:            input.Username,
			PasswordEnc:         passEnc,
			SentinelPasswordEnc: sentEnc,
			DBIndex:             input.DBIndex,
			TLS:                 input.TLS,
			OptionsJSON:         string(optBytes),
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	if err := h.registry.Test(ctx, *conn); err != nil {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": false, "error": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

// RevealSecret returns a decrypted secret to the connection owner only. The
// body names which secret: {"which":"password"|"sentinelPassword"}.
func (h *RedisConnectionHandler) RevealSecret(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn models.RedisConnection
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	var body struct {
		Which string `json:"which"`
	}
	_ = c.ShouldBindJSON(&body)
	enc := conn.PasswordEnc
	if strings.EqualFold(body.Which, "sentinelPassword") {
		enc = conn.SentinelPasswordEnc
	}
	value := ""
	if enc != "" {
		p, err := h.cipher.Decrypt(enc)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "decrypt"})
			return
		}
		value = p
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"value": value}})
}

// ── helpers ─────────────────────────────────────────────────────────

func (h *RedisConnectionHandler) encOrEmpty(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	return h.cipher.Encrypt(plain)
}

// loadAccessible loads a connection if the user owns it or holds any grant.
// Returns the row + effective role ("owner" | "write" | "read").
func (h *RedisConnectionHandler) loadAccessible(id, userID string) (*models.RedisConnection, string, error) {
	var conn models.RedisConnection
	if err := h.db.First(&conn, "id = ?", id).Error; err != nil {
		return nil, "", err
	}
	if conn.UserID == userID {
		return &conn, "owner", nil
	}
	role := h.grantRole(id, userID)
	if role == "" {
		return nil, "", errors.New("forbidden")
	}
	return &conn, role, nil
}

// grantRole returns the highest role the user holds on a connection via a
// direct or team grant ("write" beats "read"), or "" if none.
func (h *RedisConnectionHandler) grantRole(connID, userID string) string {
	var grants []models.RedisConnectionGrant
	h.db.Where(`connection_id = ? AND (
		(subject_kind = 'user' AND subject_id = ?)
		OR (subject_kind = 'team' AND subject_id IN (
			SELECT tm.team_id FROM team_members tm WHERE tm.user_id = ?
		))
	)`, connID, userID, userID).Find(&grants)
	role := ""
	for _, g := range grants {
		if g.Role == "write" {
			return "write"
		}
		if g.Role == "read" {
			role = "read"
		}
	}
	return role
}

func toRedisDTO(c models.RedisConnection, ownerName string, shared bool, grantRole string) redisConnectionDTO {
	var opts any
	if c.OptionsJSON != "" {
		_ = json.Unmarshal([]byte(c.OptionsJSON), &opts)
	}
	return redisConnectionDTO{
		ID: c.ID, UserID: c.UserID, WorkspaceID: c.WorkspaceID, Name: c.Name,
		Address: c.Address, Mode: c.Mode, SentinelMaster: c.SentinelMaster,
		Username: c.Username, HasPassword: c.PasswordEnc != "",
		HasSentinelPassword: c.SentinelPasswordEnc != "", DBIndex: c.DBIndex,
		TLS: c.TLS, Options: opts, OwnerName: ownerName, Shared: shared,
		GrantRole: grantRole, PrimaryScope: c.PrimaryScope, AllowWrite: c.AllowWrite,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}
