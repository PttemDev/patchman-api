package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/rabbitquery"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// RabbitConnectionHandler covers the RabbitMQ connection surface — CRUD,
// the Test probe and owner-only password reveal. Mirrors the Grafana one;
// broker operations live in rabbit_ops.go.
type RabbitConnectionHandler struct {
	db       *gorm.DB
	cipher   *services.TokenCipher
	registry *rabbitquery.Registry
}

func NewRabbitConnectionHandler(db *gorm.DB, cipher *services.TokenCipher, reg *rabbitquery.Registry) *RabbitConnectionHandler {
	return &RabbitConnectionHandler{db: db, cipher: cipher, registry: reg}
}

type createRabbitInput struct {
	ID           string         `json:"id"`
	Name         string         `json:"name" binding:"required"`
	URL          string         `json:"url"`
	Username     string         `json:"username"`
	Password     string         `json:"password"`
	Options      map[string]any `json:"options"`
	WorkspaceID  string         `json:"workspaceId"`
	PrimaryScope string         `json:"primaryScope"`
}

type updateRabbitInput struct {
	Name         *string        `json:"name"`
	URL          *string        `json:"url"`
	Username     *string        `json:"username"`
	Password     *string        `json:"password"` // nil = keep
	Options      map[string]any `json:"options"`
	PrimaryScope *string        `json:"primaryScope"`
}

type rabbitConnectionDTO struct {
	ID           string    `json:"id"`
	UserID       string    `json:"userId"`
	WorkspaceID  string    `json:"workspaceId,omitempty"`
	Name         string    `json:"name"`
	URL          string    `json:"url"`
	Username     string    `json:"username,omitempty"`
	HasPassword  bool      `json:"hasPassword"`
	Options      any       `json:"options,omitempty"`
	OwnerName    string    `json:"ownerName,omitempty"`
	Shared       bool      `json:"shared"`
	GrantRole    string    `json:"grantRole,omitempty"`
	PrimaryScope string    `json:"primaryScope,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// rabbitOptions keeps only the known option keys.
func rabbitOptions(in map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"insecureSkipVerify", "caCert", "amqpHost", "amqpPort", "amqpTLS", "defaultVhost"} {
		if v, ok := in[k]; ok {
			out[k] = v
		}
	}
	return out
}

func (h *RabbitConnectionHandler) rowFromInput(in createRabbitInput) (*models.RabbitConnection, string) {
	if strings.TrimSpace(in.URL) == "" {
		return nil, "url is required"
	}
	if _, err := rabbitquery.BaseURL(models.RabbitConnection{URL: in.URL}); err != nil {
		return nil, err.Error()
	}
	if strings.TrimSpace(in.Username) == "" {
		return nil, "username is required"
	}
	passEnc := ""
	if in.Password != "" {
		enc, err := h.cipher.Encrypt(in.Password)
		if err != nil {
			return nil, "encrypt password"
		}
		passEnc = enc
	}
	opt, _ := json.Marshal(rabbitOptions(or(in.Options, map[string]any{})))
	return &models.RabbitConnection{
		Name: in.Name, URL: strings.TrimRight(strings.TrimSpace(in.URL), "/"), Username: strings.TrimSpace(in.Username),
		PasswordEnc: passEnc, OptionsJSON: string(opt), WorkspaceID: in.WorkspaceID, PrimaryScope: normalizeScope(in.PrimaryScope),
	}, ""
}

func (h *RabbitConnectionHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")
	var input createRabbitInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	row, msg := h.rowFromInput(input)
	if row == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	row.ID = strings.TrimSpace(input.ID)
	if row.ID == "" {
		row.ID = uuid.New().String()
	}
	row.UserID = userID
	if err := h.db.Create(row).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save connection"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": toRabbitDTO(*row, "", false, "owner")})
}

func (h *RabbitConnectionHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	var own []models.RabbitConnection
	if err := h.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&own).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list connections"})
		return
	}
	var shared []models.RabbitConnection
	err := h.db.
		Where(`id IN (
			SELECT g.connection_id FROM rabbit_connection_grants g
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
	out := make([]rabbitConnectionDTO, 0, len(own)+len(shared))
	for _, cn := range own {
		out = append(out, toRabbitDTO(cn, "", false, "owner"))
	}
	for _, cn := range shared {
		out = append(out, toRabbitDTO(cn, owners[cn.UserID], true, h.grantRole(cn.ID, userID)))
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *RabbitConnectionHandler) Get(c *gin.Context) {
	userID := c.GetString("userID")
	conn, role, err := h.loadAccessible(c.Param("id"), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": toRabbitDTO(*conn, "", conn.UserID != userID, role)})
}

func (h *RabbitConnectionHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn models.RabbitConnection
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	var input updateRabbitInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updates := map[string]any{}
	if input.Name != nil {
		updates["name"] = *input.Name
	}
	if input.URL != nil {
		if _, err := rabbitquery.BaseURL(models.RabbitConnection{URL: *input.URL}); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		updates["url"] = strings.TrimRight(strings.TrimSpace(*input.URL), "/")
	}
	if input.Username != nil {
		updates["username"] = strings.TrimSpace(*input.Username)
	}
	if input.Password != nil {
		enc := ""
		if *input.Password != "" {
			e, err := h.cipher.Encrypt(*input.Password)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt password"})
				return
			}
			enc = e
		}
		updates["password_enc"] = enc
	}
	if input.Options != nil {
		b, _ := json.Marshal(rabbitOptions(input.Options))
		updates["options_json"] = string(b)
	}
	if input.PrimaryScope != nil {
		updates["primary_scope"] = normalizeScope(*input.PrimaryScope)
	}
	if err := h.db.Model(&conn).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update connection"})
		return
	}
	h.registry.Close(id)
	h.db.First(&conn, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{"data": toRabbitDTO(conn, "", false, "owner")})
}

func (h *RabbitConnectionHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	res := h.db.Where("id = ? AND user_id = ?", id, userID).Delete(&models.RabbitConnection{})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete connection"})
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	h.db.Where("connection_id = ?", id).Delete(&models.RabbitConnectionGrant{})
	h.registry.Close(id)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "deleted"}})
}

// Test probes a saved connection (by id) or an unsaved payload: broker
// version, cluster name, the user's tags, the vhosts and whether AMQP works.
func (h *RabbitConnectionHandler) Test(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn *models.RabbitConnection
	if id != "" && id != "new" {
		var existing models.RabbitConnection
		if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&existing).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
			return
		}
		conn = &existing
	} else {
		var input createRabbitInput
		if err := c.ShouldBindJSON(&input); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		row, msg := h.rowFromInput(input)
		if row == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": msg})
			return
		}
		conn = row
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()
	res, err := h.registry.Test(ctx, *conn)
	if err != nil {
		out := gin.H{"ok": false, "error": err.Error()}
		if ae, ok := rabbitquery.AsAPIError(err); ok {
			out["code"] = ae.Code
		}
		c.JSON(http.StatusOK, gin.H{"data": out})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"ok": true, "version": res.Version, "clusterName": res.ClusterName, "user": res.User, "tags": res.Tags,
		"vhosts": res.Vhosts, "amqpOk": res.AMQPOK, "amqpError": res.AMQPError,
	}})
}

// RevealSecret returns the decrypted password to the owner only.
func (h *RabbitConnectionHandler) RevealSecret(c *gin.Context) {
	userID := c.GetString("userID")
	var conn models.RabbitConnection
	if err := h.db.Where("id = ? AND user_id = ?", c.Param("id"), userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	value := ""
	if conn.PasswordEnc != "" {
		p, err := h.cipher.Decrypt(conn.PasswordEnc)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "decrypt"})
			return
		}
		value = p
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"value": value}})
}

// loadAccessible loads a connection the user owns or holds a grant on.
func (h *RabbitConnectionHandler) loadAccessible(id, userID string) (*models.RabbitConnection, string, error) {
	var conn models.RabbitConnection
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

func (h *RabbitConnectionHandler) grantRole(connID, userID string) string {
	var grants []models.RabbitConnectionGrant
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

func toRabbitDTO(c models.RabbitConnection, ownerName string, shared bool, grantRole string) rabbitConnectionDTO {
	var opts map[string]any
	if c.OptionsJSON != "" {
		_ = json.Unmarshal([]byte(c.OptionsJSON), &opts)
	}
	return rabbitConnectionDTO{
		ID: c.ID, UserID: c.UserID, WorkspaceID: c.WorkspaceID, Name: c.Name, URL: c.URL, Username: c.Username,
		HasPassword: c.PasswordEnc != "", Options: opts, OwnerName: ownerName, Shared: shared,
		GrantRole: grantRole, PrimaryScope: c.PrimaryScope, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}
