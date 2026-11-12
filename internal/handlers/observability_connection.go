package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/apm"
	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ObservabilityConnectionHandler covers APM / observability connections
// (Coroot, Prometheus, Jaeger, Kiali, SigNoz, Datadog): CRUD, the Test
// probe, scopes, owner-only secret reveal and grants. Mirrors
// GrafanaConnectionHandler; the service map itself is in service_map.go.
type ObservabilityConnectionHandler struct {
	db       *gorm.DB
	cipher   *services.TokenCipher
	registry *apm.Registry
}

func NewObservabilityConnectionHandler(db *gorm.DB, cipher *services.TokenCipher, reg *apm.Registry) *ObservabilityConnectionHandler {
	return &ObservabilityConnectionHandler{db: db, cipher: cipher, registry: reg}
}

type obsSecretsInput struct {
	Password *string `json:"password"`
	Token    *string `json:"token"`
	APIKey   *string `json:"apiKey"`
	AppKey   *string `json:"appKey"`
	Session  *string `json:"session"`
}

type createObsInput struct {
	ID           string          `json:"id"`
	Name         string          `json:"name" binding:"required"`
	Provider     string          `json:"provider"`
	URL          string          `json:"url"`
	AuthKind     string          `json:"authKind"`
	Username     string          `json:"username"`
	Secrets      obsSecretsInput `json:"secrets"`
	Scope        string          `json:"scope"`
	Options      map[string]any  `json:"options"`
	WorkspaceID  string          `json:"workspaceId"`
	PrimaryScope string          `json:"primaryScope"`
}

type updateObsInput struct {
	Name         *string         `json:"name"`
	URL          *string         `json:"url"`
	AuthKind     *string         `json:"authKind"`
	Username     *string         `json:"username"`
	Secrets      obsSecretsInput `json:"secrets"` // nil fields = keep
	Scope        *string         `json:"scope"`
	Options      map[string]any  `json:"options"`
	PrimaryScope *string         `json:"primaryScope"`
}

type obsConnectionDTO struct {
	ID           string    `json:"id"`
	UserID       string    `json:"userId"`
	WorkspaceID  string    `json:"workspaceId,omitempty"`
	Name         string    `json:"name"`
	Provider     string    `json:"provider"`
	URL          string    `json:"url"`
	AuthKind     string    `json:"authKind"`
	Username     string    `json:"username,omitempty"`
	HasSecrets   []string  `json:"hasSecrets"`
	Scope        string    `json:"scope,omitempty"`
	Options      any       `json:"options,omitempty"`
	OwnerName    string    `json:"ownerName,omitempty"`
	Shared       bool      `json:"shared"`
	GrantRole    string    `json:"grantRole,omitempty"`
	PrimaryScope string    `json:"primaryScope,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// obsOptions keeps only the known option keys.
func obsOptions(in map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"insecureSkipVerify", "caCert", "headerName"} {
		if v, ok := in[k]; ok {
			out[k] = v
		}
	}
	return out
}

func applySecrets(s *apm.Secrets, in obsSecretsInput) {
	set := func(dst *string, v *string, trim bool) {
		if v == nil {
			return
		}
		if trim {
			*dst = strings.TrimSpace(*v)
		} else {
			*dst = *v
		}
	}
	set(&s.Password, in.Password, false)
	set(&s.Token, in.Token, true)
	set(&s.APIKey, in.APIKey, true)
	set(&s.AppKey, in.AppKey, true)
	set(&s.Session, in.Session, true)
}

func validateObs(provider, authKind, url, username string) (apm.Provider, string, string) {
	p := apm.Get(strings.ToLower(strings.TrimSpace(provider)))
	if p == nil {
		return nil, "", "provider must be one of " + strings.Join(apm.Names(), ", ")
	}
	kind := strings.ToLower(strings.TrimSpace(authKind))
	if kind == "" {
		kind = p.AuthKinds()[0]
	}
	if !apm.AuthAllowed(p, kind) {
		return nil, "", p.Title() + " supports these sign-in methods: " + strings.Join(p.AuthKinds(), ", ")
	}
	if strings.TrimSpace(url) == "" {
		return nil, "", "url is required"
	}
	if _, err := apm.BaseURL(url); err != nil {
		return nil, "", err.Error()
	}
	if (kind == "basic" || kind == "login") && strings.TrimSpace(username) == "" {
		return nil, "", "username is required for this sign-in method"
	}
	return p, kind, ""
}

func (h *ObservabilityConnectionHandler) rowFromInput(in createObsInput) (*models.ObservabilityConnection, string) {
	p, kind, msg := validateObs(in.Provider, in.AuthKind, in.URL, in.Username)
	if p == nil {
		return nil, msg
	}
	var sec apm.Secrets
	applySecrets(&sec, in.Secrets)
	enc, err := apm.EncryptSecrets(h.cipher, sec)
	if err != nil {
		return nil, "encrypt credentials"
	}
	opt, _ := json.Marshal(obsOptions(or(in.Options, map[string]any{})))
	return &models.ObservabilityConnection{
		Name: strings.TrimSpace(in.Name), Provider: p.Name(), URL: strings.TrimRight(strings.TrimSpace(in.URL), "/"),
		AuthKind: kind, Username: strings.TrimSpace(in.Username), SecretsEnc: enc, Scope: strings.TrimSpace(in.Scope),
		OptionsJSON: string(opt), WorkspaceID: in.WorkspaceID, PrimaryScope: normalizeScope(in.PrimaryScope),
	}, ""
}

// Providers lists the supported backends and their sign-in methods.
func (h *ObservabilityConnectionHandler) Providers(c *gin.Context) {
	type dto struct {
		Name      string   `json:"name"`
		Title     string   `json:"title"`
		AuthKinds []string `json:"authKinds"`
	}
	var out []dto
	for _, n := range apm.Names() {
		p := apm.Get(n)
		out = append(out, dto{Name: n, Title: p.Title(), AuthKinds: p.AuthKinds()})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *ObservabilityConnectionHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")
	var input createObsInput
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
	c.JSON(http.StatusCreated, gin.H{"data": h.toDTO(*row, "", false, "owner")})
}

func (h *ObservabilityConnectionHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	var own []models.ObservabilityConnection
	if err := h.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&own).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list connections"})
		return
	}
	var shared []models.ObservabilityConnection
	err := h.db.
		Where(`id IN (
			SELECT g.connection_id FROM observability_connection_grants g
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
			owners[r.ID] = r.Name
			if r.Name == "" {
				owners[r.ID] = r.Email
			}
		}
	}
	out := make([]obsConnectionDTO, 0, len(own)+len(shared))
	for _, cn := range own {
		out = append(out, h.toDTO(cn, "", false, "owner"))
	}
	for _, cn := range shared {
		out = append(out, h.toDTO(cn, owners[cn.UserID], true, h.grantRole(cn.ID, userID)))
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *ObservabilityConnectionHandler) Get(c *gin.Context) {
	userID := c.GetString("userID")
	conn, role, err := h.loadAccessible(c.Param("id"), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": h.toDTO(*conn, "", conn.UserID != userID, role)})
}

func (h *ObservabilityConnectionHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn models.ObservabilityConnection
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	var input updateObsInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	next := conn
	if input.Name != nil {
		next.Name = strings.TrimSpace(*input.Name)
	}
	if input.URL != nil {
		next.URL = strings.TrimRight(strings.TrimSpace(*input.URL), "/")
	}
	if input.AuthKind != nil {
		next.AuthKind = *input.AuthKind
	}
	if input.Username != nil {
		next.Username = strings.TrimSpace(*input.Username)
	}
	if input.Scope != nil {
		next.Scope = strings.TrimSpace(*input.Scope)
	}
	p, kind, msg := validateObs(next.Provider, next.AuthKind, next.URL, next.Username)
	if p == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	next.AuthKind = kind
	sec, err := apm.DecryptSecrets(h.cipher, conn.SecretsEnc)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "decrypt credentials"})
		return
	}
	applySecrets(&sec, input.Secrets)
	if next.SecretsEnc, err = apm.EncryptSecrets(h.cipher, sec); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt credentials"})
		return
	}
	if input.Options != nil {
		b, _ := json.Marshal(obsOptions(input.Options))
		next.OptionsJSON = string(b)
	}
	if input.PrimaryScope != nil {
		next.PrimaryScope = normalizeScope(*input.PrimaryScope)
	}
	if err := h.db.Model(&conn).Updates(map[string]any{
		"name": next.Name, "url": next.URL, "auth_kind": next.AuthKind, "username": next.Username,
		"secrets_enc": next.SecretsEnc, "scope": next.Scope, "options_json": next.OptionsJSON, "primary_scope": next.PrimaryScope,
	}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update connection"})
		return
	}
	h.registry.Close(id)
	h.db.First(&conn, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{"data": h.toDTO(conn, "", false, "owner")})
}

func (h *ObservabilityConnectionHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	res := h.db.Where("id = ? AND user_id = ?", id, userID).Delete(&models.ObservabilityConnection{})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete connection"})
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	h.db.Where("connection_id = ?", id).Delete(&models.ObservabilityConnectionGrant{})
	h.registry.Close(id)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "deleted"}})
}

// Test probes a saved connection (by id) or an unsaved payload. For an
// existing connection the payload may carry edits (unchanged secrets stay).
func (h *ObservabilityConnectionHandler) Test(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn *models.ObservabilityConnection
	if id != "" && id != "new" {
		var existing models.ObservabilityConnection
		if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&existing).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
			return
		}
		existing.ID = "" // a throwaway client: never reuse the pooled session
		conn = &existing
	} else {
		var input createObsInput
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
	cl, err := h.registry.Client(*conn)
	var res *apm.TestResult
	if err == nil {
		res, err = cl.Provider.Test(ctx, cl)
	}
	if err != nil {
		out := gin.H{"ok": false, "error": err.Error()}
		if ae, ok := apm.AsAPIError(err); ok {
			out["code"] = ae.Code
		}
		c.JSON(http.StatusOK, gin.H{"data": out})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

// Scopes lists Coroot projects / Kiali namespaces for a connection.
func (h *ObservabilityConnectionHandler) Scopes(c *gin.Context) {
	conn, _, err := h.loadAccessible(c.Param("id"), c.GetString("userID"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	cl, err := h.registry.Client(*conn)
	var scopes []apm.Scope
	if err == nil {
		scopes, err = cl.Provider.Scopes(ctx, cl)
	}
	if err != nil {
		obsFail(c, err)
		return
	}
	if scopes == nil {
		scopes = []apm.Scope{}
	}
	c.JSON(http.StatusOK, gin.H{"data": scopes})
}

func obsFail(c *gin.Context, err error) {
	if ae, ok := apm.AsAPIError(err); ok {
		status := http.StatusBadGateway
		if ae.Code == "not_found" {
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"error": ae.Message, "code": ae.Code})
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "the backend did not answer in time"})
		return
	}
	c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
}

// RevealSecret returns one decrypted secret to the owner only:
// {"which":"password"|"token"|"apiKey"|"appKey"|"session"}.
func (h *ObservabilityConnectionHandler) RevealSecret(c *gin.Context) {
	userID := c.GetString("userID")
	var conn models.ObservabilityConnection
	if err := h.db.Where("id = ? AND user_id = ?", c.Param("id"), userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	var body struct {
		Which string `json:"which"`
	}
	_ = c.ShouldBindJSON(&body)
	sec, err := apm.DecryptSecrets(h.cipher, conn.SecretsEnc)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "decrypt"})
		return
	}
	value := map[string]string{"password": sec.Password, "token": sec.Token, "apiKey": sec.APIKey, "appKey": sec.AppKey, "session": sec.Session}[strings.TrimSpace(body.Which)]
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"value": value}})
}

// loadAccessible loads a connection the user owns or holds a grant on.
func (h *ObservabilityConnectionHandler) loadAccessible(id, userID string) (*models.ObservabilityConnection, string, error) {
	var conn models.ObservabilityConnection
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

func (h *ObservabilityConnectionHandler) grantRole(connID, userID string) string {
	var grants []models.ObservabilityConnectionGrant
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

func (h *ObservabilityConnectionHandler) toDTO(c models.ObservabilityConnection, ownerName string, shared bool, grantRole string) obsConnectionDTO {
	var opts map[string]any
	if c.OptionsJSON != "" {
		_ = json.Unmarshal([]byte(c.OptionsJSON), &opts)
	}
	has := []string{}
	if sec, err := apm.DecryptSecrets(h.cipher, c.SecretsEnc); err == nil {
		has = append(has, sec.Has()...)
	}
	return obsConnectionDTO{
		ID: c.ID, UserID: c.UserID, WorkspaceID: c.WorkspaceID, Name: c.Name, Provider: c.Provider, URL: c.URL,
		AuthKind: c.AuthKind, Username: c.Username, HasSecrets: has, Scope: c.Scope, Options: opts,
		OwnerName: ownerName, Shared: shared, GrantRole: grantRole, PrimaryScope: c.PrimaryScope,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

// ── Grants ──────────────────────────────────────────────────────────────

func (h *ObservabilityConnectionHandler) AddGrant(c *gin.Context) {
	userID := c.GetString("userID")
	connID := c.Param("id")
	var conn models.ObservabilityConnection
	if err := h.db.Where("id = ? AND user_id = ?", connID, userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	var input struct {
		SubjectKind string `json:"subjectKind" binding:"required"`
		SubjectID   string `json:"subjectId" binding:"required"`
		Role        string `json:"role"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if input.SubjectKind != "user" && input.SubjectKind != "team" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "subjectKind must be 'user' or 'team'"})
		return
	}
	role := input.Role
	if role == "" {
		role = "read"
	}
	if role != "read" && role != "write" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "role must be 'read' or 'write'"})
		return
	}
	grant := models.ObservabilityConnectionGrant{
		ID: uuid.New().String(), ConnectionID: connID, SubjectKind: input.SubjectKind, SubjectID: input.SubjectID,
		Role: role, GrantedBy: userID, CreatedAt: time.Now(),
	}
	if err := h.db.Create(&grant).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save grant"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": grant})
}

func (h *ObservabilityConnectionHandler) ListGrants(c *gin.Context) {
	connID := c.Param("id")
	if _, _, err := h.loadAccessible(connID, c.GetString("userID")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	var grants []models.ObservabilityConnectionGrant
	if err := h.db.Where("connection_id = ?", connID).Order("created_at DESC").Find(&grants).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list grants"})
		return
	}
	type dto struct {
		ID          string `json:"id"`
		SubjectKind string `json:"subjectKind"`
		SubjectID   string `json:"subjectId"`
		Role        string `json:"role"`
		Label       string `json:"label"`
		GrantedBy   string `json:"grantedBy"`
		CreatedAt   string `json:"createdAt"`
	}
	out := make([]dto, 0, len(grants))
	for _, g := range grants {
		label := g.SubjectID
		if g.SubjectKind == "user" {
			var u struct{ ID, Email, Name string }
			h.db.Table("users").Select("id, email, name").Where("id = ?", g.SubjectID).Scan(&u)
			if u.Name != "" {
				label = u.Name
			} else if u.Email != "" {
				label = u.Email
			}
		} else if g.SubjectKind == "team" {
			var t struct{ ID, Name string }
			h.db.Table("teams").Select("id, name").Where("id = ? AND deleted_at IS NULL", g.SubjectID).Scan(&t)
			if t.Name != "" {
				label = t.Name
			}
		}
		out = append(out, dto{ID: g.ID, SubjectKind: g.SubjectKind, SubjectID: g.SubjectID, Role: g.Role,
			Label: label, GrantedBy: g.GrantedBy, CreatedAt: g.CreatedAt.Format(time.RFC3339)})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *ObservabilityConnectionHandler) DeleteGrant(c *gin.Context) {
	userID := c.GetString("userID")
	connID := c.Param("id")
	var conn models.ObservabilityConnection
	if err := h.db.Where("id = ? AND user_id = ?", connID, userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	res := h.db.Where("id = ? AND connection_id = ?", c.Param("grantId"), connID).Delete(&models.ObservabilityConnectionGrant{})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete grant"})
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Grant not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"deleted": true}})
}

func (h *ObservabilityConnectionHandler) audit(c *gin.Context, conn *models.ObservabilityConnection, role, action, target string, actErr error) {
	var delegated *string
	if role != "owner" {
		owner := conn.UserID
		delegated = &owner
	}
	errText := ""
	if actErr != nil {
		errText = actErr.Error()
	}
	if len(target) > 1024 {
		target = target[:1024]
	}
	row := models.ObservabilityActivityHistory{
		ID: uuid.New().String(), UserID: c.GetString("userID"), DelegatedOwner: delegated,
		ConnectionID: conn.ID, Action: action, Target: target, ErrorText: errText, ExecutedAt: time.Now(),
	}
	go func() { _ = h.db.Create(&row).Error }()
}
