package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// GitConnectionHandler covers the Git connection surface — CRUD, the one-shot
// Test probe, owner-only secret reveal, and delegated grants. It rides the same
// encrypted-secret spine as the other tools (DB/Kafka/S3/Redis/ES/K8s): the
// PAT/SSH secret is AES-GCM-encrypted with the shared *services.TokenCipher and
// never leaves the process except via RevealSecret (owner-only, audited).
//
// v1 providers: gitlab. OAuth connections carry no secret at rest — remote
// reads authenticate as the caller's linked GitLab account (see git_remote.go).
// PAT/SSH connections carry an encrypted credential.
type GitConnectionHandler struct {
	db     *gorm.DB
	cipher *services.TokenCipher
	http   *http.Client
}

func NewGitConnectionHandler(db *gorm.DB, cipher *services.TokenCipher) *GitConnectionHandler {
	return &GitConnectionHandler{
		db:     db,
		cipher: cipher,
		http:   &http.Client{Timeout: 15 * time.Second},
	}
}

type createGitInput struct {
	ID           string         `json:"id"`
	Name         string         `json:"name" binding:"required"`
	Provider     string         `json:"provider"`
	BaseURL      string         `json:"baseUrl"`
	AuthKind     string         `json:"authKind"`
	Credential   string         `json:"credential"` // PAT/token/SSH; required when authKind != oauth
	Options      map[string]any `json:"options"`
	PrimaryScope string         `json:"primaryScope"`
}

type updateGitInput struct {
	Name         *string        `json:"name"`
	Provider     *string        `json:"provider"`
	BaseURL      *string        `json:"baseUrl"`
	AuthKind     *string        `json:"authKind"`
	Credential   *string        `json:"credential"` // nil = keep existing
	Options      map[string]any `json:"options"`
	PrimaryScope *string        `json:"primaryScope"`
}

type gitConnectionDTO struct {
	ID            string    `json:"id"`
	UserID        string    `json:"userId"`
	Name          string    `json:"name"`
	Provider      string    `json:"provider"`
	BaseURL       string    `json:"baseUrl"`
	AuthKind      string    `json:"authKind"`
	HasCredential bool      `json:"hasCredential"`
	Options       any       `json:"options,omitempty"`
	OwnerName     string    `json:"ownerName,omitempty"`
	Shared        bool      `json:"shared"`
	GrantRole     string    `json:"grantRole,omitempty"`
	PrimaryScope  string    `json:"primaryScope,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

func normalizeGitProvider(p string) string {
	v := strings.ToLower(strings.TrimSpace(p))
	if v == "" {
		return "gitlab"
	}
	return v
}

func normalizeGitAuthKind(a string) string {
	v := strings.ToLower(strings.TrimSpace(a))
	switch v {
	case "pat", "ssh", "oauth":
		return v
	default:
		return "oauth"
	}
}

func (h *GitConnectionHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")
	var input createGitInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	authKind := normalizeGitAuthKind(input.AuthKind)
	enc := ""
	if authKind != "oauth" {
		if strings.TrimSpace(input.Credential) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("credential is required for authKind %q", authKind)})
			return
		}
		e, err := h.cipher.Encrypt(input.Credential)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt credential"})
			return
		}
		enc = e
	}
	optBytes, _ := json.Marshal(or(input.Options, map[string]any{}))
	id := strings.TrimSpace(input.ID)
	if id == "" {
		id = uuid.New().String()
	}
	row := models.GitConnection{
		ID:            id,
		UserID:        userID,
		Name:          input.Name,
		Provider:      normalizeGitProvider(input.Provider),
		BaseURL:       strings.TrimSpace(input.BaseURL),
		AuthKind:      authKind,
		CredentialEnc: enc,
		OptionsJSON:   string(optBytes),
		PrimaryScope:  normalizeScope(input.PrimaryScope),
	}
	if err := h.db.Create(&row).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save connection"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": toGitDTO(row, "", false, "owner")})
}

func (h *GitConnectionHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	var own []models.GitConnection
	if err := h.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&own).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list connections"})
		return
	}
	var shared []models.GitConnection
	err := h.db.
		Where(`id IN (
			SELECT g.connection_id FROM git_connection_grants g
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
	out := make([]gitConnectionDTO, 0, len(own)+len(shared))
	for _, cn := range own {
		out = append(out, toGitDTO(cn, "", false, "owner"))
	}
	for _, cn := range shared {
		role := h.grantRole(cn.ID, userID)
		out = append(out, toGitDTO(cn, owners[cn.UserID], true, role))
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *GitConnectionHandler) Get(c *gin.Context) {
	userID := c.GetString("userID")
	conn, role, err := h.loadAccessible(c.Param("id"), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": toGitDTO(*conn, "", conn.UserID != userID, role)})
}

func (h *GitConnectionHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn models.GitConnection
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	var input updateGitInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updates := map[string]any{}
	if input.Name != nil {
		updates["name"] = *input.Name
	}
	if input.Provider != nil {
		updates["provider"] = normalizeGitProvider(*input.Provider)
	}
	if input.BaseURL != nil {
		updates["base_url"] = strings.TrimSpace(*input.BaseURL)
	}
	if input.AuthKind != nil {
		updates["auth_kind"] = normalizeGitAuthKind(*input.AuthKind)
	}
	if input.Credential != nil {
		if strings.TrimSpace(*input.Credential) == "" {
			// Explicit empty clears the stored secret (e.g. switching to oauth).
			updates["credential_enc"] = ""
		} else {
			enc, err := h.cipher.Encrypt(*input.Credential)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt credential"})
				return
			}
			updates["credential_enc"] = enc
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
	h.db.First(&conn, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{"data": toGitDTO(conn, "", false, "owner")})
}

func (h *GitConnectionHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	res := h.db.Where("id = ? AND user_id = ?", id, userID).Delete(&models.GitConnection{})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete connection"})
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	h.db.Where("connection_id = ?", id).Delete(&models.GitConnectionGrant{})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "deleted"}})
}

// Test probes reachability. Works for a saved connection (:id, owner-scoped) or
// an unsaved draft (POST /git/connections/test with a body). Mirrors the k8s
// Test contract: always 200; the payload carries {ok, reachable, ...}. Never a
// 4xx/5xx for an unreachable provider.
func (h *GitConnectionHandler) Test(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var (
		authKind string
		baseURL  string
		credent  string
		connID   string
	)
	if id != "" && id != "new" {
		var existing models.GitConnection
		if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&existing).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
			return
		}
		authKind = existing.AuthKind
		baseURL = existing.BaseURL
		connID = existing.ID
		if existing.CredentialEnc != "" {
			if p, err := h.cipher.Decrypt(existing.CredentialEnc); err == nil {
				credent = p
			}
		}
		// The dialog sends what is on screen, so an unsaved edit is what gets
		// tested. The stored token never goes to a different address: a new
		// Base URL needs its own token typed.
		var draft struct {
			BaseURL    string `json:"baseUrl"`
			AuthKind   string `json:"authKind"`
			Credential string `json:"credential"`
		}
		_ = c.ShouldBindJSON(&draft)
		if v := strings.TrimSpace(draft.AuthKind); v != "" {
			authKind = normalizeGitAuthKind(v)
		}
		if v := strings.TrimRight(strings.TrimSpace(draft.BaseURL), "/"); v != "" && v != strings.TrimRight(strings.TrimSpace(existing.BaseURL), "/") {
			baseURL = v
			credent = ""
		}
		if v := strings.TrimSpace(draft.Credential); v != "" {
			credent = v
		}
	} else {
		var input createGitInput
		if err := c.ShouldBindJSON(&input); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		authKind = normalizeGitAuthKind(input.AuthKind)
		baseURL = strings.TrimSpace(input.BaseURL)
		credent = input.Credential
	}

	switch authKind {
	case "pat":
		if strings.TrimSpace(baseURL) == "" {
			c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": false, "reachable": false, "error": "baseUrl is required to test a PAT connection"}})
			return
		}
		if strings.TrimSpace(credent) == "" {
			c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": false, "reachable": false, "error": "no credential stored to test"}})
			return
		}
		version, err := h.probeGitLabPAT(baseURL, credent)
		ok := err == nil
		if connID != "" {
			h.audit(userID, connID, "test", ok, gin.H{"authKind": authKind})
		}
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": false, "reachable": false, "error": err.Error()}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true, "reachable": true, "version": version}})
	case "oauth":
		// OAuth connections authenticate per-user via the caller's linked
		// GitLab account; there is no connection-level secret to probe here.
		// Reachability is verified implicitly the first time the user browses
		// (git_remote.go → validAccessToken). Report reachable so the FE can
		// save the connection.
		if connID != "" {
			h.audit(userID, connID, "test", true, gin.H{"authKind": authKind})
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{
			"ok":        true,
			"reachable": true,
			"message":   "OAuth connections authenticate with your linked GitLab account when you browse.",
		}})
	default: // ssh
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": false, "reachable": false, "error": "ssh connections are not testable in v1"}})
	}
}

// RevealSecret returns the decrypted credential to the connection owner only.
// GET /git/connections/:id/secret -> {credential, authKind}. Audited in-handler
// because the audit middleware skips GET requests.
func (h *GitConnectionHandler) RevealSecret(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn models.GitConnection
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&conn).Error; err != nil {
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
	h.audit(userID, conn.ID, "reveal_secret", true, gin.H{"authKind": conn.AuthKind})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"credential": value, "authKind": conn.AuthKind}})
}

// ── grants ──────────────────────────────────────────────────────────

type gitGrantInput struct {
	SubjectKind string `json:"subjectKind" binding:"required"`
	SubjectID   string `json:"subjectId" binding:"required"`
	Role        string `json:"role"` // read | write (default read)
}

type gitGrantDeleteInput struct {
	SubjectKind string `json:"subjectKind" binding:"required"`
	SubjectID   string `json:"subjectId" binding:"required"`
}

// ListGrants lists the grants on a connection. Any user with access (owner or
// grantee) can read them; the FE uses this to render the share panel.
func (h *GitConnectionHandler) ListGrants(c *gin.Context) {
	userID := c.GetString("userID")
	connID := c.Param("id")
	if _, _, err := h.loadAccessible(connID, userID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	var grants []models.GitConnectionGrant
	if err := h.db.Where("connection_id = ?", connID).Order("subject_kind, subject_id").Find(&grants).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list grants"})
		return
	}
	type dto struct {
		ConnectionID string `json:"connectionId"`
		SubjectKind  string `json:"subjectKind"`
		SubjectID    string `json:"subjectId"`
		Role         string `json:"role"`
		Label        string `json:"label"`
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
		out = append(out, dto{
			ConnectionID: g.ConnectionID, SubjectKind: g.SubjectKind,
			SubjectID: g.SubjectID, Role: g.Role, Label: label,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// AddGrant grants (or re-grants) access. Owner-only. The (connection, subject)
// triple is the primary key, so a repeat grant upserts the role.
func (h *GitConnectionHandler) AddGrant(c *gin.Context) {
	userID := c.GetString("userID")
	connID := c.Param("id")
	var conn models.GitConnection
	if err := h.db.Where("id = ? AND user_id = ?", connID, userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	var input gitGrantInput
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
	grant := models.GitConnectionGrant{
		ConnectionID: connID,
		SubjectKind:  input.SubjectKind,
		SubjectID:    input.SubjectID,
		Role:         role,
	}
	if err := h.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "connection_id"}, {Name: "subject_kind"}, {Name: "subject_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"role"}),
	}).Create(&grant).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save grant"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": grant})
}

// DeleteGrant revokes access. Owner-only. The subject is named in the body
// ({subjectKind, subjectId}) rather than a path id because the grant has a
// composite key, not a surrogate id.
func (h *GitConnectionHandler) DeleteGrant(c *gin.Context) {
	userID := c.GetString("userID")
	connID := c.Param("id")
	var conn models.GitConnection
	if err := h.db.Where("id = ? AND user_id = ?", connID, userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	var input gitGrantDeleteInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	res := h.db.Where("connection_id = ? AND subject_kind = ? AND subject_id = ?",
		connID, input.SubjectKind, input.SubjectID).Delete(&models.GitConnectionGrant{})
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

// ── access helpers (mirror k8s) ─────────────────────────────────────

// loadAccessible loads a connection if the user owns it or holds any grant.
func (h *GitConnectionHandler) loadAccessible(id, userID string) (*models.GitConnection, string, error) {
	var conn models.GitConnection
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

// access resolves the connection for the current request + enforces the
// required role. Kept for the write surface (Wave 2 push/commit) — v1 reads
// call it with needWrite=false.
func (h *GitConnectionHandler) access(c *gin.Context, needWrite bool) (*models.GitConnection, string, bool) {
	userID := c.GetString("userID")
	conn, role, err := h.loadAccessible(c.Param("id"), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return nil, "", false
	}
	if needWrite && role != "owner" && role != "write" {
		c.JSON(http.StatusForbidden, gin.H{"error": "This action requires write access on the connection."})
		return nil, "", false
	}
	return conn, role, true
}

// grantRole returns the highest role the user holds on a connection ("write"
// beats "read"), or "" if none — owner or team-via-team_members EXISTS.
func (h *GitConnectionHandler) grantRole(connID, userID string) string {
	var grants []models.GitConnectionGrant
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

// audit writes a best-effort GitActivityHistory row. Used by the paths the
// audit middleware can't cover on its own (GET reveal/test) and reused by the
// remote-read handler.
func (h *GitConnectionHandler) audit(userID, connID, op string, ok bool, detail any) {
	auditGit(h.db, userID, connID, op, ok, detail)
}

// probeGitLabPAT verifies a personal access token against a GitLab instance by
// calling GET <baseURL>/api/v4/version with the PRIVATE-TOKEN header. Returns
// the reported version string on success.
func (h *GitConnectionHandler) probeGitLabPAT(baseURL, token string) (string, error) {
	endpoint := strings.TrimRight(baseURL, "/") + "/api/v4/version"
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("PRIVATE-TOKEN", token)
	req.Header.Set("Accept", "application/json")
	resp, err := h.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("gitlab /version %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var v struct {
		Version string `json:"version"`
	}
	_ = json.Unmarshal(body, &v)
	return v.Version, nil
}

// auditGit writes a best-effort GitActivityHistory row (fire-and-forget). It is
// the single audit sink for the Git tool — the connection handler, the
// remote-read handler, and (Wave 2) the local-ops handler all funnel through it.
func auditGit(db *gorm.DB, userID, connID, op string, ok bool, detail any) {
	b, _ := json.Marshal(detail)
	row := models.GitActivityHistory{
		ID:           uuid.New().String(),
		ConnectionID: connID,
		UserID:       userID,
		Op:           op,
		DetailJSON:   string(b),
		OK:           ok,
		CreatedAt:    time.Now(),
	}
	go func() { _ = db.Create(&row).Error }()
}

func toGitDTO(c models.GitConnection, ownerName string, shared bool, grantRole string) gitConnectionDTO {
	var opts any
	if strings.TrimSpace(c.OptionsJSON) != "" {
		_ = json.Unmarshal([]byte(c.OptionsJSON), &opts)
	}
	return gitConnectionDTO{
		ID: c.ID, UserID: c.UserID, Name: c.Name, Provider: c.Provider,
		BaseURL: c.BaseURL, AuthKind: c.AuthKind, HasCredential: c.CredentialEnc != "",
		Options: opts, OwnerName: ownerName, Shared: shared, GrantRole: grantRole,
		PrimaryScope: c.PrimaryScope, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}
