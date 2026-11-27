package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"patchman-be/internal/jira"
	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// JiraConnectionHandler covers the Jira connection surface — CRUD, the Test
// probe (Myself + field list for the acceptance-criteria picker) and
// owner-only secret reveal. Mirrors the Graylog one; reads (search, issue,
// fields) live in jira_ops.go. CR_QA_P0_PLAN.md P0.b / O1.
type JiraConnectionHandler struct {
	db     *gorm.DB
	cipher *services.TokenCipher
	// httpClient overrides the Jira HTTP client (tests); nil = default.
	httpClient *http.Client

	sprintMu    sync.Mutex
	sprintCache map[string]sprintEntry
}

type sprintEntry struct {
	fieldID string
	stamp   time.Time
	at      time.Time
}

func NewJiraConnectionHandler(db *gorm.DB, cipher *services.TokenCipher) *JiraConnectionHandler {
	return &JiraConnectionHandler{db: db, cipher: cipher, sprintCache: map[string]sprintEntry{}}
}

type createJiraInput struct {
	ID      string `json:"id"`
	Name    string `json:"name" binding:"required"`
	BaseURL string `json:"baseUrl"`
	Flavor  string `json:"flavor"` // cloud (default) | datacenter
	Email   string `json:"email"`
	// API token (Cloud) or personal access token (Data Center).
	Secret       string         `json:"secret"`
	Options      map[string]any `json:"options"`
	WorkspaceID  string         `json:"workspaceId"`
	PrimaryScope string         `json:"primaryScope"`
}

type updateJiraInput struct {
	Name         *string        `json:"name"`
	BaseURL      *string        `json:"baseUrl"`
	Flavor       *string        `json:"flavor"`
	Email        *string        `json:"email"`
	Secret       *string        `json:"secret"` // nil / "" = keep
	Options      map[string]any `json:"options"`
	PrimaryScope *string        `json:"primaryScope"`
}

type jiraConnectionDTO struct {
	ID           string         `json:"id"`
	UserID       string         `json:"userId"`
	WorkspaceID  string         `json:"workspaceId,omitempty"`
	Name         string         `json:"name"`
	BaseURL      string         `json:"baseUrl"`
	Flavor       string         `json:"flavor"`
	AuthKind     string         `json:"authKind"`
	Email        string         `json:"email,omitempty"`
	HasSecret    bool           `json:"hasSecret"`
	Options      map[string]any `json:"options"`
	OwnerName    string         `json:"ownerName,omitempty"`
	Shared       bool           `json:"shared"`
	GrantRole    string         `json:"grantRole,omitempty"`
	PrimaryScope string         `json:"primaryScope,omitempty"`
	CreatedAt    time.Time      `json:"createdAt"`
	UpdatedAt    time.Time      `json:"updatedAt"`
}

// jiraOptions keeps only the known option keys, validated.
func jiraOptions(in map[string]any) (map[string]any, string) {
	out := map[string]any{}
	if v, ok := in["acFieldId"].(string); ok && strings.TrimSpace(v) != "" {
		v = strings.TrimSpace(v)
		if !jira.ValidFieldID(v) {
			return nil, "acFieldId is not a Jira field id (e.g. customfield_10042)"
		}
		out["acFieldId"] = v
	}
	if v, ok := in["defaultJql"].(string); ok && strings.TrimSpace(v) != "" {
		v = strings.TrimSpace(v)
		if len(v) > 2000 {
			return nil, "defaultJql is too long (max 2000 characters)"
		}
		out["defaultJql"] = v
	}
	return out, ""
}

// JiraOptionsOf parses a connection's OptionsJSON.
func JiraOptionsOf(conn models.JiraConnection) map[string]any {
	out := map[string]any{}
	if strings.TrimSpace(conn.OptionsJSON) != "" {
		_ = json.Unmarshal([]byte(conn.OptionsJSON), &out)
	}
	return out
}

func jiraOpt(conn models.JiraConnection, key string) string {
	s, _ := JiraOptionsOf(conn)[key].(string)
	return s
}

func (h *JiraConnectionHandler) rowFromInput(in createJiraInput) (*models.JiraConnection, string) {
	base, err := jira.BaseURL(in.BaseURL)
	if err != nil {
		return nil, err.Error()
	}
	flavor := jira.NormalizeFlavor(in.Flavor)
	email := strings.TrimSpace(in.Email)
	if flavor == jira.FlavorCloud && email == "" {
		return nil, "email is required for Jira Cloud"
	}
	if flavor == jira.FlavorDataCenter {
		email = ""
	}
	if strings.TrimSpace(in.Secret) == "" {
		if flavor == jira.FlavorCloud {
			return nil, "API token is required"
		}
		return nil, "personal access token is required"
	}
	enc, err := h.cipher.Encrypt(strings.TrimSpace(in.Secret))
	if err != nil {
		return nil, "encrypt secret"
	}
	opts, msg := jiraOptions(or(in.Options, map[string]any{}))
	if opts == nil {
		return nil, msg
	}
	ob, _ := json.Marshal(opts)
	return &models.JiraConnection{
		Name: strings.TrimSpace(in.Name), BaseURL: base, Flavor: flavor, AuthKind: jira.AuthKindFor(flavor), Email: email,
		CredentialEnc: enc, OptionsJSON: string(ob), WorkspaceID: in.WorkspaceID, PrimaryScope: normalizeScope(in.PrimaryScope),
	}, ""
}

// client builds a Jira client for a connection (decrypting its secret).
func (h *JiraConnectionHandler) client(conn models.JiraConnection) (*jira.Client, error) {
	secret := ""
	if conn.CredentialEnc != "" {
		s, err := h.cipher.Decrypt(conn.CredentialEnc)
		if err != nil {
			return nil, &jira.APIError{Code: "auth", Message: "The stored token could not be read; enter it again."}
		}
		secret = s
	}
	return jira.New(jira.Config{BaseURL: conn.BaseURL, Flavor: conn.Flavor, Email: conn.Email, Secret: secret, HTTP: h.httpClient})
}

func (h *JiraConnectionHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")
	var input createJiraInput
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
	c.JSON(http.StatusCreated, gin.H{"data": toJiraDTO(*row, "", false, "owner")})
}

func (h *JiraConnectionHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	var own []models.JiraConnection
	if err := h.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&own).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list connections"})
		return
	}
	var shared []models.JiraConnection
	err := h.db.
		Where(`id IN (
			SELECT g.connection_id FROM jira_connection_grants g
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
	out := make([]jiraConnectionDTO, 0, len(own)+len(shared))
	for _, cn := range own {
		out = append(out, toJiraDTO(cn, "", false, "owner"))
	}
	for _, cn := range shared {
		out = append(out, toJiraDTO(cn, owners[cn.UserID], true, h.grantRole(cn.ID, userID)))
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *JiraConnectionHandler) Get(c *gin.Context) {
	userID := c.GetString("userID")
	conn, role, err := h.loadAccessible(c.Param("id"), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": toJiraDTO(*conn, "", conn.UserID != userID, role)})
}

func (h *JiraConnectionHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn models.JiraConnection
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	var input updateJiraInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updates := map[string]any{}
	if input.Name != nil {
		if strings.TrimSpace(*input.Name) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
			return
		}
		updates["name"] = strings.TrimSpace(*input.Name)
	}
	if input.BaseURL != nil {
		base, err := jira.BaseURL(*input.BaseURL)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		updates["base_url"] = base
	}
	flavor := conn.Flavor
	if input.Flavor != nil {
		flavor = jira.NormalizeFlavor(*input.Flavor)
		updates["flavor"] = flavor
		updates["auth_kind"] = jira.AuthKindFor(flavor)
	}
	email := conn.Email
	if input.Email != nil {
		email = strings.TrimSpace(*input.Email)
	}
	if flavor == jira.FlavorDataCenter {
		email = ""
	}
	if email != conn.Email {
		updates["email"] = email
	}
	if flavor == jira.FlavorCloud && email == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "email is required for Jira Cloud"})
		return
	}
	if input.Secret != nil && strings.TrimSpace(*input.Secret) != "" {
		e, err := h.cipher.Encrypt(strings.TrimSpace(*input.Secret))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt secret"})
			return
		}
		updates["credential_enc"] = e
	} else if flavor != conn.Flavor {
		// An API token is not a PAT (and the other way round): switching the
		// flavor needs the new credential.
		c.JSON(http.StatusBadRequest, gin.H{"error": "changing Cloud / Data Center needs the new token"})
		return
	}
	if input.Options != nil {
		opts, msg := jiraOptions(input.Options)
		if opts == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": msg})
			return
		}
		b, _ := json.Marshal(opts)
		updates["options_json"] = string(b)
	}
	if input.PrimaryScope != nil {
		updates["primary_scope"] = normalizeScope(*input.PrimaryScope)
	}
	if len(updates) > 0 {
		if err := h.db.Model(&conn).Updates(updates).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update connection"})
			return
		}
	}
	h.forgetSprint(id)
	h.db.First(&conn, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{"data": toJiraDTO(conn, "", false, "owner")})
}

func (h *JiraConnectionHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	res := h.db.Where("id = ? AND user_id = ?", id, userID).Delete(&models.JiraConnection{})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete connection"})
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	h.db.Where("connection_id = ?", id).Delete(&models.JiraConnectionGrant{})
	h.forgetSprint(id)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "deleted"}})
}

// Test probes a saved connection (by id) or an unsaved payload: who the
// credential signs in as, the site's deployment type and version, and the
// custom fields (for the acceptance-criteria picker, which runs right after
// Test, before the connection is saved). A saved connection may take
// on-screen overrides; the stored secret is never sent to a changed URL or
// used for another flavor.
func (h *JiraConnectionHandler) Test(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn *models.JiraConnection
	fail := func(msg string) { c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": false, "error": msg}}) }
	if id != "" && id != "new" {
		var existing models.JiraConnection
		if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&existing).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
			return
		}
		var draft createJiraInput
		_ = c.ShouldBindJSON(&draft)
		if draft.BaseURL != "" || draft.Secret != "" || draft.Flavor != "" || draft.Email != "" {
			if draft.Secret == "" {
				if draft.BaseURL != "" {
					if base, err := jira.BaseURL(draft.BaseURL); err != nil || base != existing.BaseURL {
						fail("Enter the token again to test a changed URL.")
						return
					}
				}
				if draft.Flavor != "" && jira.NormalizeFlavor(draft.Flavor) != existing.Flavor {
					fail("Enter the new token to test another Jira flavor.")
					return
				}
				if draft.Email != "" && strings.TrimSpace(draft.Email) != existing.Email {
					fail("Enter the API token again to test another email.")
					return
				}
				s, err := h.cipher.Decrypt(existing.CredentialEnc)
				if err != nil {
					fail("The stored token could not be read; enter it again.")
					return
				}
				draft.Secret = s
			}
			if draft.BaseURL == "" {
				draft.BaseURL = existing.BaseURL
			}
			if draft.Flavor == "" {
				draft.Flavor = existing.Flavor
			}
			if draft.Email == "" {
				draft.Email = existing.Email
			}
			draft.Name = existing.Name
			draft.Options = nil
			row, msg := h.rowFromInput(draft)
			if row == nil {
				fail(msg)
				return
			}
			conn = row
		} else {
			conn = &existing
		}
	} else {
		// Decoded without the create binding: Test runs before the
		// connection has a name (Test, then pick the AC field, then save).
		var input createJiraInput
		if err := json.NewDecoder(c.Request.Body).Decode(&input); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
			return
		}
		input.Options = nil
		row, msg := h.rowFromInput(input)
		if row == nil {
			fail(msg)
			return
		}
		conn = row
	}
	cl, err := h.client(*conn)
	if err != nil {
		fail(err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()
	me, err := cl.Myself(ctx)
	if err != nil {
		out := gin.H{"ok": false, "error": err.Error()}
		if ae, ok := jira.AsAPIError(err); ok {
			out["code"] = ae.Code
		}
		c.JSON(http.StatusOK, gin.H{"data": out})
		return
	}
	// Server info and fields in parallel; neither failing fails the Test.
	var (
		wg      sync.WaitGroup
		info    *jira.ServerInfo
		fields  []jira.Field
		fErr    error
		infoErr error
	)
	wg.Add(2)
	go func() { defer wg.Done(); info, infoErr = cl.ServerInfo(ctx) }()
	go func() { defer wg.Done(); fields, fErr = cl.Fields(ctx) }()
	wg.Wait()
	out := gin.H{
		"ok": true, "displayName": me.DisplayName, "accountId": me.AccountID, "username": me.Name,
		"resolvedUrl": cl.Base(), "fields": customFields(fields),
	}
	if infoErr == nil && info != nil {
		out["version"] = info.Version
		out["deploymentType"] = info.DeploymentType
		isCloud := strings.EqualFold(info.DeploymentType, "Cloud")
		if conn.Flavor == jira.FlavorCloud && info.DeploymentType != "" && !isCloud {
			out["warning"] = "This site reports itself as " + info.DeploymentType + ", not Cloud: choose Data Center."
		} else if conn.Flavor == jira.FlavorDataCenter && isCloud {
			out["warning"] = "This site is Jira Cloud: choose Cloud (email + API token)."
		}
	}
	if fErr != nil {
		out["fieldsError"] = fErr.Error()
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// customFields keeps the custom fields (the AC field is always one), sorted
// as Jira sent them; a nil list becomes [].
func customFields(all []jira.Field) []jira.Field {
	out := make([]jira.Field, 0, len(all))
	for _, f := range all {
		if f.Custom {
			out = append(out, f)
		}
	}
	return out
}

// RevealSecret returns the decrypted token / PAT to the owner only.
func (h *JiraConnectionHandler) RevealSecret(c *gin.Context) {
	userID := c.GetString("userID")
	var conn models.JiraConnection
	if err := h.db.Where("id = ? AND user_id = ?", c.Param("id"), userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	value := ""
	if conn.CredentialEnc != "" {
		p, err := h.cipher.Decrypt(conn.CredentialEnc)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "decrypt"})
			return
		}
		value = p
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"value": value}})
}

// loadAccessible loads a connection the user owns or holds a grant on.
func (h *JiraConnectionHandler) loadAccessible(id, userID string) (*models.JiraConnection, string, error) {
	var conn models.JiraConnection
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

func (h *JiraConnectionHandler) grantRole(connID, userID string) string {
	var grants []models.JiraConnectionGrant
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

func toJiraDTO(c models.JiraConnection, ownerName string, shared bool, grantRole string) jiraConnectionDTO {
	return jiraConnectionDTO{
		ID: c.ID, UserID: c.UserID, WorkspaceID: c.WorkspaceID, Name: c.Name, BaseURL: c.BaseURL, Flavor: c.Flavor,
		AuthKind: c.AuthKind, Email: c.Email, HasSecret: c.CredentialEnc != "", Options: JiraOptionsOf(c),
		OwnerName: ownerName, Shared: shared, GrantRole: grantRole, PrimaryScope: c.PrimaryScope,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

// ── sprint field cache ──────────────────────────────────────────────

const sprintTTL = 30 * time.Minute

// sprintField resolves (and caches per connection) the sprint field id from
// the field list. "" when the site has no Jira Software sprint field or the
// lookup failed — the issue is still returned, without sprints.
func (h *JiraConnectionHandler) sprintField(ctx context.Context, cl *jira.Client, conn models.JiraConnection) string {
	h.sprintMu.Lock()
	e, ok := h.sprintCache[conn.ID]
	h.sprintMu.Unlock()
	if ok && e.stamp.Equal(conn.UpdatedAt) && time.Since(e.at) < sprintTTL {
		return e.fieldID
	}
	fields, err := cl.Fields(ctx)
	if err != nil {
		return ""
	}
	id := jira.SprintFieldID(fields)
	h.sprintMu.Lock()
	h.sprintCache[conn.ID] = sprintEntry{fieldID: id, stamp: conn.UpdatedAt, at: time.Now()}
	h.sprintMu.Unlock()
	return id
}

func (h *JiraConnectionHandler) forgetSprint(id string) {
	h.sprintMu.Lock()
	delete(h.sprintCache, id)
	h.sprintMu.Unlock()
}
