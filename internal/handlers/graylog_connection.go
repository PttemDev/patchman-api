package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/graylogquery"
	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// GraylogConnectionHandler covers the Graylog connection surface — CRUD, the
// Test probe and owner-only secret reveal. Mirrors the RabbitMQ one; search
// and browsing live in graylog_ops.go.
type GraylogConnectionHandler struct {
	db       *gorm.DB
	cipher   *services.TokenCipher
	registry *graylogquery.Registry
}

func NewGraylogConnectionHandler(db *gorm.DB, cipher *services.TokenCipher, reg *graylogquery.Registry) *GraylogConnectionHandler {
	return &GraylogConnectionHandler{db: db, cipher: cipher, registry: reg}
}

type createGraylogInput struct {
	ID       string `json:"id"`
	Name     string `json:"name" binding:"required"`
	URL      string `json:"url"`
	AuthKind string `json:"authKind"` // token (default) | basic
	Username string `json:"username"`
	// Access token (token) or password (basic).
	Secret       string         `json:"secret"`
	Options      map[string]any `json:"options"`
	WorkspaceID  string         `json:"workspaceId"`
	PrimaryScope string         `json:"primaryScope"`
}

type updateGraylogInput struct {
	Name         *string        `json:"name"`
	URL          *string        `json:"url"`
	AuthKind     *string        `json:"authKind"`
	Username     *string        `json:"username"`
	Secret       *string        `json:"secret"` // nil = keep
	Options      map[string]any `json:"options"`
	PrimaryScope *string        `json:"primaryScope"`
}

type graylogConnectionDTO struct {
	ID           string    `json:"id"`
	UserID       string    `json:"userId"`
	WorkspaceID  string    `json:"workspaceId,omitempty"`
	Name         string    `json:"name"`
	URL          string    `json:"url"`
	AuthKind     string    `json:"authKind"`
	Username     string    `json:"username,omitempty"`
	HasSecret    bool      `json:"hasSecret"`
	Options      any       `json:"options,omitempty"`
	OwnerName    string    `json:"ownerName,omitempty"`
	Shared       bool      `json:"shared"`
	GrantRole    string    `json:"grantRole,omitempty"`
	PrimaryScope string    `json:"primaryScope,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

func normalizeGraylogAuth(a string) string {
	if strings.EqualFold(strings.TrimSpace(a), graylogquery.AuthBasic) {
		return graylogquery.AuthBasic
	}
	return graylogquery.AuthToken
}

// graylogOptions keeps only the known option keys.
func graylogOptions(in map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"insecureSkipVerify", "caCert"} {
		if v, ok := in[k]; ok {
			out[k] = v
		}
	}
	return out
}

func (h *GraylogConnectionHandler) rowFromInput(in createGraylogInput) (*models.GraylogConnection, string) {
	base, err := graylogquery.BaseURL(in.URL)
	if err != nil {
		return nil, err.Error()
	}
	kind := normalizeGraylogAuth(in.AuthKind)
	if kind == graylogquery.AuthBasic && strings.TrimSpace(in.Username) == "" {
		return nil, "username is required"
	}
	if strings.TrimSpace(in.Secret) == "" {
		if kind == graylogquery.AuthBasic {
			return nil, "password is required"
		}
		return nil, "access token is required"
	}
	enc, err := h.cipher.Encrypt(strings.TrimSpace(in.Secret))
	if err != nil {
		return nil, "encrypt secret"
	}
	user := ""
	if kind == graylogquery.AuthBasic {
		user = strings.TrimSpace(in.Username)
	}
	opt, _ := json.Marshal(graylogOptions(or(in.Options, map[string]any{})))
	return &models.GraylogConnection{
		Name: in.Name, URL: base, AuthKind: kind, Username: user, SecretEnc: enc,
		OptionsJSON: string(opt), WorkspaceID: in.WorkspaceID, PrimaryScope: normalizeScope(in.PrimaryScope),
	}, ""
}

func (h *GraylogConnectionHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")
	var input createGraylogInput
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
	c.JSON(http.StatusCreated, gin.H{"data": toGraylogDTO(*row, "", false, "owner")})
}

func (h *GraylogConnectionHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	var own []models.GraylogConnection
	if err := h.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&own).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list connections"})
		return
	}
	var shared []models.GraylogConnection
	err := h.db.
		Where(`id IN (
			SELECT g.connection_id FROM graylog_connection_grants g
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
	out := make([]graylogConnectionDTO, 0, len(own)+len(shared))
	for _, cn := range own {
		out = append(out, toGraylogDTO(cn, "", false, "owner"))
	}
	for _, cn := range shared {
		out = append(out, toGraylogDTO(cn, owners[cn.UserID], true, h.grantRole(cn.ID, userID)))
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *GraylogConnectionHandler) Get(c *gin.Context) {
	userID := c.GetString("userID")
	conn, role, err := h.loadAccessible(c.Param("id"), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": toGraylogDTO(*conn, "", conn.UserID != userID, role)})
}

func (h *GraylogConnectionHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn models.GraylogConnection
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	var input updateGraylogInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updates := map[string]any{}
	if input.Name != nil {
		updates["name"] = *input.Name
	}
	if input.URL != nil {
		base, err := graylogquery.BaseURL(*input.URL)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		updates["url"] = base
	}
	kind := conn.AuthKind
	if input.AuthKind != nil {
		kind = normalizeGraylogAuth(*input.AuthKind)
		updates["auth_kind"] = kind
	}
	if input.Username != nil || (input.AuthKind != nil && kind == graylogquery.AuthToken) {
		user := ""
		if input.Username != nil && kind == graylogquery.AuthBasic {
			user = strings.TrimSpace(*input.Username)
		}
		updates["username"] = user
	}
	if input.Secret != nil && strings.TrimSpace(*input.Secret) != "" {
		e, err := h.cipher.Encrypt(strings.TrimSpace(*input.Secret))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt secret"})
			return
		}
		updates["secret_enc"] = e
	} else if input.AuthKind != nil && kind != conn.AuthKind {
		// A token is not a password (and the other way round): switching the
		// kind needs the new secret.
		c.JSON(http.StatusBadRequest, gin.H{"error": "changing the sign-in kind needs the new token or password"})
		return
	}
	if kind == graylogquery.AuthBasic {
		if u, ok := updates["username"].(string); (ok && u == "") || (!ok && conn.Username == "") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "username is required"})
			return
		}
	}
	if input.Options != nil {
		b, _ := json.Marshal(graylogOptions(input.Options))
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
	c.JSON(http.StatusOK, gin.H{"data": toGraylogDTO(conn, "", false, "owner")})
}

func (h *GraylogConnectionHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	res := h.db.Where("id = ? AND user_id = ?", id, userID).Delete(&models.GraylogConnection{})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete connection"})
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	h.db.Where("connection_id = ?", id).Delete(&models.GraylogConnectionGrant{})
	h.registry.Close(id)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "deleted"}})
}

// Test probes a saved connection (by id) or an unsaved payload: Graylog
// version, the signed-in user and how many streams it can read. A saved
// connection may take on-screen overrides (URL, kind, username, a new
// secret) so the edit dialog tests what it shows; the stored secret is never
// sent to a changed URL.
func (h *GraylogConnectionHandler) Test(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn *models.GraylogConnection
	if id != "" && id != "new" {
		var existing models.GraylogConnection
		if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&existing).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
			return
		}
		var draft createGraylogInput
		_ = c.ShouldBindJSON(&draft)
		if draft.URL != "" || draft.Secret != "" || draft.AuthKind != "" || draft.Username != "" {
			if draft.Secret == "" {
				base, err := graylogquery.BaseURL(draft.URL)
				if draft.URL != "" && (err != nil || base != existing.URL) {
					c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": false, "error": "Enter the token or password again to test a changed URL."}})
					return
				}
				if draft.AuthKind != "" && normalizeGraylogAuth(draft.AuthKind) != existing.AuthKind {
					c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": false, "error": "Enter the new token or password to test another sign-in kind."}})
					return
				}
				if s, err := h.cipher.Decrypt(existing.SecretEnc); err == nil {
					draft.Secret = s
				}
			}
			if draft.URL == "" {
				draft.URL = existing.URL
			}
			if draft.AuthKind == "" {
				draft.AuthKind = existing.AuthKind
			}
			if draft.Username == "" {
				draft.Username = existing.Username
			}
			if draft.Options == nil {
				draft.Options = graylogquery.Options(existing)
			}
			draft.Name = existing.Name
			row, msg := h.rowFromInput(draft)
			if row == nil {
				c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": false, "error": msg}})
				return
			}
			conn = row
		} else {
			conn = &existing
		}
	} else {
		var input createGraylogInput
		if err := c.ShouldBindJSON(&input); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		row, msg := h.rowFromInput(input)
		if row == nil {
			c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": false, "error": msg}})
			return
		}
		conn = row
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()
	probe := *conn
	probe.ID = "" // never cache a draft's client under the saved id
	res, err := h.registry.Test(ctx, probe)
	if err != nil {
		out := gin.H{"ok": false, "error": err.Error()}
		if ae, ok := graylogquery.AsAPIError(err); ok {
			out["code"] = ae.Code
		}
		c.JSON(http.StatusOK, gin.H{"data": out})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"ok": true, "version": res.Version, "hostname": res.Hostname, "username": res.Username, "streams": res.Streams,
		"resolvedUrl": conn.URL,
	}})
}

// RevealSecret returns the decrypted token / password to the owner only.
func (h *GraylogConnectionHandler) RevealSecret(c *gin.Context) {
	userID := c.GetString("userID")
	var conn models.GraylogConnection
	if err := h.db.Where("id = ? AND user_id = ?", c.Param("id"), userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	value := ""
	if conn.SecretEnc != "" {
		p, err := h.cipher.Decrypt(conn.SecretEnc)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "decrypt"})
			return
		}
		value = p
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"value": value}})
}

// loadAccessible loads a connection the user owns or holds a grant on.
func (h *GraylogConnectionHandler) loadAccessible(id, userID string) (*models.GraylogConnection, string, error) {
	var conn models.GraylogConnection
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

func (h *GraylogConnectionHandler) grantRole(connID, userID string) string {
	var grants []models.GraylogConnectionGrant
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

func toGraylogDTO(c models.GraylogConnection, ownerName string, shared bool, grantRole string) graylogConnectionDTO {
	var opts map[string]any
	if c.OptionsJSON != "" {
		_ = json.Unmarshal([]byte(c.OptionsJSON), &opts)
	}
	return graylogConnectionDTO{
		ID: c.ID, UserID: c.UserID, WorkspaceID: c.WorkspaceID, Name: c.Name, URL: c.URL, AuthKind: c.AuthKind,
		Username: c.Username, HasSecret: c.SecretEnc != "", Options: opts, OwnerName: ownerName, Shared: shared,
		GrantRole: grantRole, PrimaryScope: c.PrimaryScope, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}
