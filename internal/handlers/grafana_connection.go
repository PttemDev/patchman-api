package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/grafanaquery"
	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// GrafanaConnectionHandler covers the Grafana connection surface — CRUD, the
// Test probe, owner-only secret reveal — and implements
// grafanaquery.SessionStore so rotated session cookies land back in the row.
// Mirrors ESConnectionHandler; dashboards/queries live in grafana_ops.go.
type GrafanaConnectionHandler struct {
	db       *gorm.DB
	cipher   *services.TokenCipher
	registry *grafanaquery.Registry
}

func NewGrafanaConnectionHandler(db *gorm.DB, cipher *services.TokenCipher, reg *grafanaquery.Registry) *GrafanaConnectionHandler {
	h := &GrafanaConnectionHandler{db: db, cipher: cipher, registry: reg}
	reg.SetStore(h)
	return h
}

// SaveSession persists a rotated session cookie (grafanaquery.SessionStore).
func (h *GrafanaConnectionHandler) SaveSession(connID, cookie string) error {
	enc, err := h.cipher.Encrypt(cookie)
	if err != nil {
		return err
	}
	now := time.Now()
	return h.db.Model(&models.GrafanaConnection{}).Where("id = ?", connID).
		Updates(map[string]any{"session_enc": enc, "session_rotated_at": &now}).Error
}

type createGrafanaInput struct {
	ID           string         `json:"id"`
	Name         string         `json:"name" binding:"required"`
	URL          string         `json:"url"`
	AuthKind     string         `json:"authKind"`
	Username     string         `json:"username"`
	Password     string         `json:"password"`
	Token        string         `json:"token"`
	Session      string         `json:"session"`
	Options      map[string]any `json:"options"`
	WorkspaceID  string         `json:"workspaceId"`
	PrimaryScope string         `json:"primaryScope"`
}

type updateGrafanaInput struct {
	Name         *string        `json:"name"`
	URL          *string        `json:"url"`
	AuthKind     *string        `json:"authKind"`
	Username     *string        `json:"username"`
	Password     *string        `json:"password"` // nil = keep
	Token        *string        `json:"token"`    // nil = keep
	Session      *string        `json:"session"`  // nil = keep
	Options      map[string]any `json:"options"`
	PrimaryScope *string        `json:"primaryScope"`
}

type grafanaConnectionDTO struct {
	ID               string     `json:"id"`
	UserID           string     `json:"userId"`
	WorkspaceID      string     `json:"workspaceId,omitempty"`
	Name             string     `json:"name"`
	URL              string     `json:"url"`
	AuthKind         string     `json:"authKind"`
	Username         string     `json:"username,omitempty"`
	HasPassword      bool       `json:"hasPassword"`
	HasToken         bool       `json:"hasToken"`
	HasSession       bool       `json:"hasSession"`
	SessionRotatedAt *time.Time `json:"sessionRotatedAt,omitempty"`
	Options          any        `json:"options,omitempty"`
	OwnerName        string     `json:"ownerName,omitempty"`
	Shared           bool       `json:"shared"`
	GrantRole        string     `json:"grantRole,omitempty"`
	PrimaryScope     string     `json:"primaryScope,omitempty"`
	CreatedAt        time.Time  `json:"createdAt"`
	UpdatedAt        time.Time  `json:"updatedAt"`
}

func normalizeGrafanaAuthKind(a string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(a)) {
	case "", "token", "bearer", "serviceaccount":
		return "token", true
	case "basic", "password":
		return "basic", true
	case "session", "cookie":
		return "session", true
	case "none", "anonymous":
		return "none", true
	}
	return "", false
}

// grafanaOptions keeps only the known option keys.
func grafanaOptions(in map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"insecureSkipVerify", "caCert", "orgId", "cookieName"} {
		if v, ok := in[k]; ok {
			out[k] = v
		}
	}
	// Public dashboard links: [{token, title?}], deduped and capped.
	if list, ok := in["publicDashboards"].([]any); ok {
		seen := map[string]bool{}
		var refs []map[string]any
		for _, it := range list {
			m, _ := it.(map[string]any)
			tok, _ := m["token"].(string)
			tok = strings.TrimSpace(tok)
			if !grafanaquery.ValidPublicToken(tok) || seen[tok] {
				continue
			}
			seen[tok] = true
			ref := map[string]any{"token": tok}
			if title, _ := m["title"].(string); strings.TrimSpace(title) != "" {
				if r := []rune(strings.TrimSpace(title)); len(r) > 200 {
					title = string(r[:200])
				}
				ref["title"] = strings.TrimSpace(title)
			}
			refs = append(refs, ref)
			if len(refs) >= 50 {
				break
			}
		}
		if len(refs) > 0 {
			out["publicDashboards"] = refs
		}
	}
	return out
}

func (h *GrafanaConnectionHandler) encOrEmpty(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	return h.cipher.Encrypt(plain)
}

func (h *GrafanaConnectionHandler) rowFromInput(in createGrafanaInput) (*models.GrafanaConnection, string) {
	kind, ok := normalizeGrafanaAuthKind(in.AuthKind)
	if !ok {
		return nil, "authKind must be token, basic, session or none"
	}
	if strings.TrimSpace(in.URL) == "" {
		return nil, "url is required"
	}
	if _, err := grafanaquery.BaseURL(models.GrafanaConnection{URL: in.URL}); err != nil {
		return nil, err.Error()
	}
	if kind == "basic" && strings.TrimSpace(in.Username) == "" {
		return nil, "username is required for username/password auth"
	}
	tokEnc, err := h.encOrEmpty(strings.TrimSpace(in.Token))
	if err != nil {
		return nil, "encrypt token"
	}
	passEnc, err := h.encOrEmpty(in.Password)
	if err != nil {
		return nil, "encrypt password"
	}
	sessEnc, err := h.encOrEmpty(strings.TrimSpace(in.Session))
	if err != nil {
		return nil, "encrypt session"
	}
	opt, _ := json.Marshal(grafanaOptions(or(in.Options, map[string]any{})))
	return &models.GrafanaConnection{
		Name: in.Name, URL: strings.TrimRight(strings.TrimSpace(in.URL), "/"), AuthKind: kind,
		Username: strings.TrimSpace(in.Username), TokenEnc: tokEnc, PasswordEnc: passEnc, SessionEnc: sessEnc,
		OptionsJSON: string(opt), WorkspaceID: in.WorkspaceID, PrimaryScope: normalizeScope(in.PrimaryScope),
	}, ""
}

func (h *GrafanaConnectionHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")
	var input createGrafanaInput
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
	if row.SessionEnc != "" {
		now := time.Now()
		row.SessionRotatedAt = &now
	}
	if err := h.db.Create(row).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save connection"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": toGrafanaDTO(*row, "", false, "owner")})
}

func (h *GrafanaConnectionHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	var own []models.GrafanaConnection
	if err := h.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&own).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list connections"})
		return
	}
	var shared []models.GrafanaConnection
	err := h.db.
		Where(`id IN (
			SELECT g.connection_id FROM grafana_connection_grants g
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
	out := make([]grafanaConnectionDTO, 0, len(own)+len(shared))
	for _, cn := range own {
		out = append(out, toGrafanaDTO(cn, "", false, "owner"))
	}
	for _, cn := range shared {
		out = append(out, toGrafanaDTO(cn, owners[cn.UserID], true, h.grantRole(cn.ID, userID)))
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *GrafanaConnectionHandler) Get(c *gin.Context) {
	userID := c.GetString("userID")
	conn, role, err := h.loadAccessible(c.Param("id"), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": toGrafanaDTO(*conn, "", conn.UserID != userID, role)})
}

func (h *GrafanaConnectionHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn models.GrafanaConnection
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	var input updateGrafanaInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updates := map[string]any{}
	if input.Name != nil {
		updates["name"] = *input.Name
	}
	if input.URL != nil {
		if _, err := grafanaquery.BaseURL(models.GrafanaConnection{URL: *input.URL}); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		updates["url"] = strings.TrimRight(strings.TrimSpace(*input.URL), "/")
	}
	if input.AuthKind != nil {
		kind, ok := normalizeGrafanaAuthKind(*input.AuthKind)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "authKind must be token, basic, session or none"})
			return
		}
		updates["auth_kind"] = kind
	}
	if input.Username != nil {
		updates["username"] = strings.TrimSpace(*input.Username)
	}
	for _, s := range []struct {
		v   *string
		col string
	}{{input.Password, "password_enc"}, {input.Token, "token_enc"}, {input.Session, "session_enc"}} {
		if s.v == nil {
			continue
		}
		v := *s.v
		if s.col != "password_enc" {
			v = strings.TrimSpace(v)
		}
		enc, err := h.encOrEmpty(v)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt secret"})
			return
		}
		updates[s.col] = enc
		if s.col == "session_enc" {
			now := time.Now()
			updates["session_rotated_at"] = &now
		}
	}
	if input.Options != nil {
		b, _ := json.Marshal(grafanaOptions(input.Options))
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
	c.JSON(http.StatusOK, gin.H{"data": toGrafanaDTO(conn, "", false, "owner")})
}

func (h *GrafanaConnectionHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	res := h.db.Where("id = ? AND user_id = ?", id, userID).Delete(&models.GrafanaConnection{})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete connection"})
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	h.db.Where("connection_id = ?", id).Delete(&models.GrafanaConnectionGrant{})
	h.registry.Close(id)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "deleted"}})
}

// Test probes a saved connection (by id) or an unsaved payload. It reports
// the Grafana version, the signed-in user and the datasource count.
func (h *GrafanaConnectionHandler) Test(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn *models.GrafanaConnection
	if id != "" && id != "new" {
		var existing models.GrafanaConnection
		if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&existing).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
			return
		}
		conn = &existing
	} else {
		var input createGrafanaInput
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
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	info, rotated, err := h.registry.Test(ctx, *conn)
	refs := grafanaquery.PublicDashboards(*conn)
	if err != nil && conn.AuthKind == "none" && len(refs) > 0 {
		// Login required for everything else, but the public links work:
		// a public-dashboards-only connection.
		if ae, ok := grafanaquery.AsAPIError(err); ok && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden) {
			titled, perr := h.registry.TestPublic(ctx, *conn, refs)
			if perr == nil {
				c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true, "publicOnly": true, "publicDashboards": titled}})
				return
			}
			err = perr
		}
	}
	if err == nil && len(refs) > 0 {
		titled, perr := h.registry.TestPublic(ctx, *conn, refs)
		if perr != nil {
			err = perr
		} else {
			refs = titled
		}
	}
	if err != nil {
		out := gin.H{"ok": false, "error": err.Error()}
		if ae, ok := grafanaquery.AsAPIError(err); ok {
			out["code"] = ae.Code
		}
		c.JSON(http.StatusOK, gin.H{"data": out})
		return
	}
	out := gin.H{"ok": true, "version": info.Version, "datasources": len(info.Datasources)}
	if len(refs) > 0 {
		out["publicDashboards"] = refs
	}
	if info.User != nil {
		out["user"] = info.User
	}
	// Grafana answered under a sub path of the given URL (Percona PMM: /graph).
	if info.ResolvedURL != "" {
		out["resolvedUrl"] = info.ResolvedURL
	}
	// The probe rotated the pasted session: hand the live value back so the
	// dialog saves it (the caller just sent us the old one — nothing new leaks).
	if rotated != "" && (id == "" || id == "new") {
		out["session"] = rotated
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// RevealSecret returns a decrypted secret to the owner only:
// {"which":"token"|"password"|"session"}.
func (h *GrafanaConnectionHandler) RevealSecret(c *gin.Context) {
	userID := c.GetString("userID")
	var conn models.GrafanaConnection
	if err := h.db.Where("id = ? AND user_id = ?", c.Param("id"), userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	var body struct {
		Which string `json:"which"`
	}
	_ = c.ShouldBindJSON(&body)
	enc := conn.TokenEnc
	switch strings.ToLower(strings.TrimSpace(body.Which)) {
	case "password":
		enc = conn.PasswordEnc
	case "session":
		enc = conn.SessionEnc
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

// loadAccessible loads a connection the user owns or holds a grant on.
func (h *GrafanaConnectionHandler) loadAccessible(id, userID string) (*models.GrafanaConnection, string, error) {
	var conn models.GrafanaConnection
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

func (h *GrafanaConnectionHandler) grantRole(connID, userID string) string {
	var grants []models.GrafanaConnectionGrant
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

func toGrafanaDTO(c models.GrafanaConnection, ownerName string, shared bool, grantRole string) grafanaConnectionDTO {
	var opts map[string]any
	if c.OptionsJSON != "" {
		_ = json.Unmarshal([]byte(c.OptionsJSON), &opts)
	}
	return grafanaConnectionDTO{
		ID: c.ID, UserID: c.UserID, WorkspaceID: c.WorkspaceID, Name: c.Name, URL: c.URL,
		AuthKind: c.AuthKind, Username: c.Username,
		HasPassword: c.PasswordEnc != "", HasToken: c.TokenEnc != "", HasSession: c.SessionEnc != "",
		SessionRotatedAt: c.SessionRotatedAt, Options: opts, OwnerName: ownerName, Shared: shared,
		GrantRole: grantRole, PrimaryScope: c.PrimaryScope, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}
