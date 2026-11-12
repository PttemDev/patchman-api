package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/kafkaquery"
	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// KafkaConnectionHandler covers the Kafka connection surface — CRUD, the
// one-shot Test probe, owner-only SASL secret reveal. Mirrors
// DBConnectionHandler; admin + consume/produce live in kafka_admin.go /
// kafka_messages.go because they need per-topic permission + guardrails.
type KafkaConnectionHandler struct {
	db       *gorm.DB
	cipher   *services.TokenCipher
	registry *kafkaquery.Registry
}

func NewKafkaConnectionHandler(db *gorm.DB, cipher *services.TokenCipher, reg *kafkaquery.Registry) *KafkaConnectionHandler {
	return &KafkaConnectionHandler{db: db, cipher: cipher, registry: reg}
}

type createKafkaInput struct {
	ID               string         `json:"id"`
	Name             string         `json:"name" binding:"required"`
	BootstrapServers string         `json:"bootstrapServers" binding:"required"`
	SecurityProtocol string         `json:"securityProtocol"`
	SASLMechanism    string         `json:"saslMechanism"`
	SASLUsername     string         `json:"saslUsername"`
	SASLPassword     string         `json:"saslPassword"`
	Options          map[string]any `json:"options"`
	WorkspaceID      string         `json:"workspaceId"`
	PrimaryScope     string         `json:"primaryScope"`
}

type updateKafkaInput struct {
	Name             *string        `json:"name"`
	BootstrapServers *string        `json:"bootstrapServers"`
	SecurityProtocol *string        `json:"securityProtocol"`
	SASLMechanism    *string        `json:"saslMechanism"`
	SASLUsername     *string        `json:"saslUsername"`
	SASLPassword     *string        `json:"saslPassword"` // nil = keep existing
	Options          map[string]any `json:"options"`
	PrimaryScope     *string        `json:"primaryScope"`
}

type kafkaConnectionDTO struct {
	ID               string    `json:"id"`
	UserID           string    `json:"userId"`
	WorkspaceID      string    `json:"workspaceId,omitempty"`
	Name             string    `json:"name"`
	BootstrapServers string    `json:"bootstrapServers"`
	SecurityProtocol string    `json:"securityProtocol"`
	SASLMechanism    string    `json:"saslMechanism,omitempty"`
	SASLUsername     string    `json:"saslUsername,omitempty"`
	HasSASLPassword  bool      `json:"hasSaslPassword"`
	Options          any       `json:"options,omitempty"`
	OwnerName        string    `json:"ownerName,omitempty"`
	Shared           bool      `json:"shared"`
	GrantRole        string    `json:"grantRole,omitempty"`
	PrimaryScope     string    `json:"primaryScope,omitempty"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

func validSecurityProtocol(p string) bool {
	switch strings.ToUpper(strings.TrimSpace(p)) {
	case "PLAINTEXT", "SSL", "SASL_PLAINTEXT", "SASL_SSL", "":
		return true
	}
	return false
}

func (h *KafkaConnectionHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")
	var input createKafkaInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !validSecurityProtocol(input.SecurityProtocol) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "securityProtocol must be PLAINTEXT, SSL, SASL_PLAINTEXT, or SASL_SSL"})
		return
	}
	proto := strings.ToUpper(strings.TrimSpace(input.SecurityProtocol))
	if proto == "" {
		proto = "PLAINTEXT"
	}
	passEnc := ""
	if input.SASLPassword != "" {
		enc, err := h.cipher.Encrypt(input.SASLPassword)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt sasl password"})
			return
		}
		passEnc = enc
	}
	optBytes, _ := json.Marshal(or(input.Options, map[string]any{}))
	id := strings.TrimSpace(input.ID)
	if id == "" {
		id = uuid.New().String()
	}
	row := models.KafkaConnection{
		ID:               id,
		UserID:           userID,
		WorkspaceID:      input.WorkspaceID,
		Name:             input.Name,
		BootstrapServers: input.BootstrapServers,
		SecurityProtocol: proto,
		SASLMechanism:    strings.ToUpper(strings.TrimSpace(input.SASLMechanism)),
		SASLUsername:     input.SASLUsername,
		SASLPasswordEnc:  passEnc,
		OptionsJSON:      string(optBytes),
		PrimaryScope:     normalizeScope(input.PrimaryScope),
	}
	if err := h.db.Create(&row).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save connection"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": toKafkaDTO(row, "", false, "owner")})
}

func (h *KafkaConnectionHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	var own []models.KafkaConnection
	if err := h.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&own).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list connections"})
		return
	}
	var shared []models.KafkaConnection
	err := h.db.
		Where(`id IN (
			SELECT g.connection_id FROM kafka_connection_grants g
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
	out := make([]kafkaConnectionDTO, 0, len(own)+len(shared))
	for _, cn := range own {
		out = append(out, toKafkaDTO(cn, "", false, "owner"))
	}
	for _, cn := range shared {
		role := h.grantRole(cn.ID, userID)
		out = append(out, toKafkaDTO(cn, owners[cn.UserID], true, role))
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *KafkaConnectionHandler) Get(c *gin.Context) {
	userID := c.GetString("userID")
	conn, role, err := h.loadAccessible(c.Param("id"), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": toKafkaDTO(*conn, "", conn.UserID != userID, role)})
}

func (h *KafkaConnectionHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn models.KafkaConnection
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	var input updateKafkaInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updates := map[string]any{}
	if input.Name != nil {
		updates["name"] = *input.Name
	}
	if input.BootstrapServers != nil {
		updates["bootstrap_servers"] = *input.BootstrapServers
	}
	if input.SecurityProtocol != nil {
		if !validSecurityProtocol(*input.SecurityProtocol) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid securityProtocol"})
			return
		}
		updates["security_protocol"] = strings.ToUpper(strings.TrimSpace(*input.SecurityProtocol))
	}
	if input.SASLMechanism != nil {
		updates["sasl_mechanism"] = strings.ToUpper(strings.TrimSpace(*input.SASLMechanism))
	}
	if input.SASLUsername != nil {
		updates["sasl_username"] = *input.SASLUsername
	}
	if input.SASLPassword != nil {
		if *input.SASLPassword == "" {
			updates["sasl_password_enc"] = ""
		} else {
			enc, err := h.cipher.Encrypt(*input.SASLPassword)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt sasl password"})
				return
			}
			updates["sasl_password_enc"] = enc
		}
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
	h.registry.Close(id) // pooled client may be stale after an edit
	h.db.First(&conn, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{"data": toKafkaDTO(conn, "", false, "owner")})
}

func (h *KafkaConnectionHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	res := h.db.Where("id = ? AND user_id = ?", id, userID).Delete(&models.KafkaConnection{})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete connection"})
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	h.db.Where("connection_id = ?", id).Delete(&models.KafkaConnectionGrant{})
	h.registry.Close(id)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "deleted"}})
}

func (h *KafkaConnectionHandler) Test(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn *models.KafkaConnection
	if id != "" && id != "new" {
		var existing models.KafkaConnection
		if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&existing).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
			return
		}
		conn = &existing
	} else {
		var input createKafkaInput
		if err := c.ShouldBindJSON(&input); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if !validSecurityProtocol(input.SecurityProtocol) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid securityProtocol"})
			return
		}
		enc := ""
		if input.SASLPassword != "" {
			e, err := h.cipher.Encrypt(input.SASLPassword)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt"})
				return
			}
			enc = e
		}
		proto := strings.ToUpper(strings.TrimSpace(input.SecurityProtocol))
		if proto == "" {
			proto = "PLAINTEXT"
		}
		optBytes, _ := json.Marshal(or(input.Options, map[string]any{}))
		conn = &models.KafkaConnection{
			BootstrapServers: input.BootstrapServers,
			SecurityProtocol: proto,
			SASLMechanism:    strings.ToUpper(strings.TrimSpace(input.SASLMechanism)),
			SASLUsername:     input.SASLUsername,
			SASLPasswordEnc:  enc,
			OptionsJSON:      string(optBytes),
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

func (h *KafkaConnectionHandler) RevealSecret(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn models.KafkaConnection
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	pw := ""
	if conn.SASLPasswordEnc != "" {
		p, err := h.cipher.Decrypt(conn.SASLPasswordEnc)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "decrypt"})
			return
		}
		pw = p
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"saslPassword": pw}})
}

// ── helpers ─────────────────────────────────────────────────────────

// loadAccessible loads a connection if the user owns it or holds any
// grant. Returns the row + effective role ("owner" | "write" | "read").
func (h *KafkaConnectionHandler) loadAccessible(id, userID string) (*models.KafkaConnection, string, error) {
	var conn models.KafkaConnection
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
func (h *KafkaConnectionHandler) grantRole(connID, userID string) string {
	var grants []models.KafkaConnectionGrant
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

func toKafkaDTO(c models.KafkaConnection, ownerName string, shared bool, grantRole string) kafkaConnectionDTO {
	var opts any
	if c.OptionsJSON != "" {
		_ = json.Unmarshal([]byte(c.OptionsJSON), &opts)
	}
	return kafkaConnectionDTO{
		ID: c.ID, UserID: c.UserID, WorkspaceID: c.WorkspaceID, Name: c.Name,
		BootstrapServers: c.BootstrapServers, SecurityProtocol: c.SecurityProtocol,
		SASLMechanism: c.SASLMechanism, SASLUsername: c.SASLUsername,
		HasSASLPassword: c.SASLPasswordEnc != "", Options: opts,
		OwnerName: ownerName, Shared: shared, GrantRole: grantRole,
		PrimaryScope: c.PrimaryScope, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}
