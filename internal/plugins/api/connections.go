package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"

	"patchman-be/internal/middleware"
	"patchman-be/internal/models"
	"patchman-be/internal/plugins/manifest"
	"patchman-be/internal/plugins/runtime"
)

type connDTO struct {
	ID           string         `json:"id"`
	PluginID     string         `json:"pluginId"`
	UserID       string         `json:"userId"`
	WorkspaceID  string         `json:"workspaceId,omitempty"`
	Name         string         `json:"name"`
	Config       map[string]any `json:"config"`
	SecretsSet   []string       `json:"secretsSet"` // which secret fields hold a value
	AllowWrite   bool           `json:"allowWrite"`
	PrimaryScope string         `json:"primaryScope,omitempty"`
	OwnerName    string         `json:"ownerName,omitempty"`
	Shared       bool           `json:"shared"`
	GrantRole    string         `json:"grantRole,omitempty"`
	CreatedAt    time.Time      `json:"createdAt"`
	UpdatedAt    time.Time      `json:"updatedAt"`
}

func (s *Service) toConnDTO(cn models.PluginConnection, owner string, shared bool, role string) connDTO {
	cfg := map[string]any{}
	_ = json.Unmarshal([]byte(cn.ConfigJSON), &cfg)
	set := []string{}
	if secrets, err := s.secrets(cn); err == nil {
		for k := range secrets {
			set = append(set, k)
		}
		sort.Strings(set)
	}
	return connDTO{ID: cn.ID, PluginID: cn.PluginID, UserID: cn.UserID, WorkspaceID: cn.WorkspaceID, Name: cn.Name,
		Config: cfg, SecretsSet: set, AllowWrite: cn.AllowWrite, PrimaryScope: cn.PrimaryScope,
		OwnerName: owner, Shared: shared, GrantRole: role, CreatedAt: cn.CreatedAt, UpdatedAt: cn.UpdatedAt}
}

func normalizeScope(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "local":
		return "local"
	case "remote":
		return "remote"
	}
	return ""
}

func (s *Service) secrets(cn models.PluginConnection) (map[string]string, error) {
	out := map[string]string{}
	if cn.SecretsEnc == "" {
		return out, nil
	}
	plain, err := s.cipher.Decrypt(cn.SecretsEnc)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal([]byte(plain), &out)
	return out, err
}

func (s *Service) encSecrets(m map[string]string) (string, error) {
	if len(m) == 0 {
		return "", nil
	}
	b, _ := json.Marshal(m)
	return s.cipher.Encrypt(string(b))
}

// runtimeConn is what the runtime sees: config stringified, secrets
// decrypted, and a version that changes with every edit.
func (s *Service) runtimeConn(cn models.PluginConnection) (*runtime.Connection, error) {
	cfg := map[string]any{}
	if err := json.Unmarshal([]byte(cn.ConfigJSON), &cfg); err != nil {
		return nil, err
	}
	sec, err := s.secrets(cn)
	if err != nil {
		return nil, errors.New("decrypt connection secrets")
	}
	str := map[string]string{}
	for k, v := range cfg {
		switch t := v.(type) {
		case string:
			str[k] = t
		case nil:
		default:
			b, _ := json.Marshal(t)
			str[k] = string(b)
		}
	}
	return &runtime.Connection{ID: cn.ID, Version: strconv.FormatInt(cn.UpdatedAt.UnixNano(), 10), Config: str, Secrets: sec}, nil
}

// configSchema is the connection schema without the secret fields: config is
// validated against it, secrets travel separately.
func configSchema(m *manifest.Manifest) (*jsonschema.Resolved, error) {
	if len(m.Connection.Schema) == 0 {
		return nil, nil
	}
	var raw map[string]any
	if err := json.Unmarshal(m.Connection.Schema, &raw); err != nil {
		return nil, err
	}
	if props, ok := raw["properties"].(map[string]any); ok {
		for _, f := range m.Connection.SecretFields {
			delete(props, f)
		}
	}
	if req, ok := raw["required"].([]any); ok {
		keep := req[:0]
		for _, r := range req {
			if name, _ := r.(string); !m.SecretField(name) {
				keep = append(keep, r)
			}
		}
		raw["required"] = keep
	}
	b, _ := json.Marshal(raw)
	var sch jsonschema.Schema
	if err := json.Unmarshal(b, &sch); err != nil {
		return nil, err
	}
	return sch.Resolve(nil)
}

// checkConfig enforces the config/secret split and the schema, and that
// every host a net rule takes from the config is a plain host.
func checkConfig(m *manifest.Manifest, cfg map[string]any, secrets map[string]string) error {
	for k := range cfg {
		if m.SecretField(k) {
			return fmt.Errorf("%q is a secret field: send it in secrets, not config", k)
		}
	}
	for k := range secrets {
		if !m.SecretField(k) {
			return fmt.Errorf("%q is not a declared secret field", k)
		}
	}
	rs, err := configSchema(m)
	if err != nil {
		return fmt.Errorf("connection schema: %w", err)
	}
	if rs != nil {
		if err := rs.Validate(cfg); err != nil {
			return fmt.Errorf("config: %w", err)
		}
	}
	for _, r := range m.Permissions.Net {
		if r.From != "" {
			if v, ok := cfg[manifest.RefField(r.From)].(string); ok && v != "" {
				if _, err := manifest.NormalizeHost(v, false); err != nil {
					return fmt.Errorf("config %s: %w", manifest.RefField(r.From), err)
				}
			}
		}
		if r.PortsFrom != "" {
			if v, ok := cfg[manifest.RefField(r.PortsFrom)]; ok {
				if _, err := manifest.ParsePort(fmt.Sprint(v)); err != nil {
					return fmt.Errorf("config %s: %w", manifest.RefField(r.PortsFrom), err)
				}
			}
		}
	}
	return nil
}

// grantRole: highest role via a direct or team grant ("write" beats "read").
func (s *Service) grantRole(connID, userID string) string {
	var grants []models.PluginConnectionGrant
	s.db.Where(`connection_id = ? AND (
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

// loadAccessible is the single access resolver for plugin connections: the
// row must belong to pluginID (a connection of plugin B never runs under
// plugin A) and the user must own it or hold a grant.
func (s *Service) loadAccessible(pluginID, connID, userID string) (*models.PluginConnection, string, error) {
	var cn models.PluginConnection
	if err := s.db.First(&cn, "id = ? AND plugin_id = ?", connID, pluginID).Error; err != nil {
		return nil, "", err
	}
	if cn.UserID == userID {
		return &cn, "owner", nil
	}
	if role := s.grantRole(cn.ID, userID); role != "" {
		return &cn, role, nil
	}
	return nil, "", errors.New("forbidden")
}

func (s *Service) loadOwned(pluginID, connID, userID string) (*models.PluginConnection, error) {
	var cn models.PluginConnection
	err := s.db.First(&cn, "id = ? AND plugin_id = ? AND user_id = ?", connID, pluginID, userID).Error
	return &cn, err
}

type connInput struct {
	ID           string            `json:"id"`
	Name         *string           `json:"name"`
	Config       map[string]any    `json:"config"`
	Secrets      map[string]string `json:"secrets"` // update: "" clears a field, absent keeps it
	AllowWrite   *bool             `json:"allowWrite"`
	WorkspaceID  string            `json:"workspaceId"`
	PrimaryScope *string           `json:"primaryScope"`
}

func (s *Service) createConn(c *gin.Context) {
	p, ok := s.lookup(c.Param("pluginId"), false)
	if !ok {
		errf(c, http.StatusNotFound, "Plugin not found")
		return
	}
	var in connInput
	if err := c.ShouldBindJSON(&in); err != nil {
		errf(c, http.StatusBadRequest, "%v", err)
		return
	}
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		errf(c, http.StatusBadRequest, "name is required")
		return
	}
	if in.Config == nil {
		in.Config = map[string]any{}
	}
	for k, v := range in.Secrets {
		if v == "" {
			delete(in.Secrets, k)
		}
	}
	if err := checkConfig(p.m, in.Config, in.Secrets); err != nil {
		errf(c, http.StatusBadRequest, "%v", err)
		return
	}
	enc, err := s.encSecrets(in.Secrets)
	if err != nil {
		errf(c, http.StatusInternalServerError, "encrypt secrets")
		return
	}
	id := strings.TrimSpace(in.ID)
	if id == "" {
		id = uuid.New().String()
	} else if _, err := uuid.Parse(id); err != nil {
		errf(c, http.StatusBadRequest, "id must be a UUID")
		return
	}
	cfg, _ := json.Marshal(in.Config)
	row := models.PluginConnection{ID: id, PluginID: p.row.ID, UserID: c.GetString("userID"), WorkspaceID: in.WorkspaceID,
		Name: strings.TrimSpace(*in.Name), ConfigJSON: string(cfg), SecretsEnc: enc}
	if in.AllowWrite != nil {
		row.AllowWrite = *in.AllowWrite
	}
	if in.PrimaryScope != nil {
		row.PrimaryScope = normalizeScope(*in.PrimaryScope)
	}
	if err := s.db.Create(&row).Error; err != nil {
		errf(c, http.StatusInternalServerError, "save connection")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": s.toConnDTO(row, "", false, "owner")})
}

func (s *Service) listConns(c *gin.Context) {
	pluginID, userID := c.Param("pluginId"), c.GetString("userID")
	var own, shared []models.PluginConnection
	if err := s.db.Where("plugin_id = ? AND user_id = ?", pluginID, userID).Order("created_at DESC").Find(&own).Error; err != nil {
		errf(c, http.StatusInternalServerError, "list connections")
		return
	}
	err := s.db.Where(`plugin_id = ? AND user_id <> ? AND id IN (
			SELECT g.connection_id FROM plugin_connection_grants g
			WHERE (g.subject_kind = 'user' AND g.subject_id = ?)
			   OR (g.subject_kind = 'team' AND g.subject_id IN (
				   SELECT tm.team_id FROM team_members tm WHERE tm.user_id = ?)))`,
		pluginID, userID, userID, userID).Order("created_at DESC").Find(&shared).Error
	if err != nil {
		errf(c, http.StatusInternalServerError, "list shared connections")
		return
	}
	owners := map[string]string{}
	if len(shared) > 0 {
		ids := make([]string, 0, len(shared))
		for _, cn := range shared {
			ids = append(ids, cn.UserID)
		}
		var rows []struct{ ID, Email, Name string }
		s.db.Table("users").Select("id, email, name").Where("id IN ?", ids).Scan(&rows)
		for _, r := range rows {
			owners[r.ID] = r.Name
			if r.Name == "" {
				owners[r.ID] = r.Email
			}
		}
	}
	out := make([]connDTO, 0, len(own)+len(shared))
	for _, cn := range own {
		out = append(out, s.toConnDTO(cn, "", false, "owner"))
	}
	for _, cn := range shared {
		out = append(out, s.toConnDTO(cn, owners[cn.UserID], true, s.grantRole(cn.ID, userID)))
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (s *Service) getConn(c *gin.Context) {
	userID := c.GetString("userID")
	cn, role, err := s.loadAccessible(c.Param("pluginId"), c.Param("id"), userID)
	if err != nil {
		errf(c, http.StatusNotFound, "Connection not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": s.toConnDTO(*cn, "", cn.UserID != userID, role)})
}

func (s *Service) updateConn(c *gin.Context) {
	pluginID := c.Param("pluginId")
	p, ok := s.lookup(pluginID, false)
	if !ok {
		errf(c, http.StatusNotFound, "Plugin not found")
		return
	}
	cn, err := s.loadOwned(pluginID, c.Param("id"), c.GetString("userID"))
	if err != nil {
		errf(c, http.StatusNotFound, "Connection not found")
		return
	}
	var in connInput
	if err := c.ShouldBindJSON(&in); err != nil {
		errf(c, http.StatusBadRequest, "%v", err)
		return
	}
	cfg := map[string]any{}
	_ = json.Unmarshal([]byte(cn.ConfigJSON), &cfg)
	if in.Config != nil {
		cfg = in.Config // full replace
	}
	sec, err := s.secrets(*cn)
	if err != nil {
		errf(c, http.StatusInternalServerError, "decrypt secrets")
		return
	}
	for k, v := range in.Secrets {
		if !p.m.SecretField(k) {
			errf(c, http.StatusBadRequest, "%q is not a declared secret field", k)
			return
		}
		if v == "" {
			delete(sec, k)
		} else {
			sec[k] = v
		}
	}
	if err := checkConfig(p.m, cfg, sec); err != nil {
		errf(c, http.StatusBadRequest, "%v", err)
		return
	}
	enc, err := s.encSecrets(sec)
	if err != nil {
		errf(c, http.StatusInternalServerError, "encrypt secrets")
		return
	}
	cfgJSON, _ := json.Marshal(cfg)
	updates := map[string]any{"config_json": string(cfgJSON), "secrets_enc": enc, "updated_at": time.Now()}
	if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
		updates["name"] = strings.TrimSpace(*in.Name)
	}
	if in.AllowWrite != nil {
		updates["allow_write"] = *in.AllowWrite
	}
	if in.PrimaryScope != nil {
		updates["primary_scope"] = normalizeScope(*in.PrimaryScope)
	}
	if err := s.db.Model(cn).Updates(updates).Error; err != nil {
		errf(c, http.StatusInternalServerError, "update connection")
		return
	}
	s.rt.DropConnection(pluginID, cn.ID)
	s.db.First(cn, "id = ?", cn.ID)
	c.JSON(http.StatusOK, gin.H{"data": s.toConnDTO(*cn, "", false, "owner")})
}

func (s *Service) deleteConn(c *gin.Context) {
	pluginID, id := c.Param("pluginId"), c.Param("id")
	res := s.db.Where("id = ? AND plugin_id = ? AND user_id = ?", id, pluginID, c.GetString("userID")).Delete(&models.PluginConnection{})
	if res.Error != nil {
		errf(c, http.StatusInternalServerError, "delete connection")
		return
	}
	if res.RowsAffected == 0 {
		errf(c, http.StatusNotFound, "Connection not found")
		return
	}
	s.db.Where("connection_id = ?", id).Delete(&models.PluginConnectionGrant{})
	s.rt.DropConnection(pluginID, id)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "deleted"}})
}

// testConn runs the plugin's "test" op against a saved connection (owner)
// or an unsaved one from the body.
func (s *Service) testConn(c *gin.Context) {
	pluginID := c.Param("pluginId")
	p, ok := s.lookup(pluginID, true)
	if !ok {
		errf(c, http.StatusNotFound, "Plugin not found or disabled")
		return
	}
	if _, ok := p.m.Ops["test"]; !ok {
		errf(c, http.StatusBadRequest, "This plugin has no test op")
		return
	}
	var rc *runtime.Connection
	if id := c.Param("id"); id != "" {
		cn, err := s.loadOwned(pluginID, id, c.GetString("userID"))
		if err != nil {
			errf(c, http.StatusNotFound, "Connection not found")
			return
		}
		if rc, err = s.runtimeConn(*cn); err != nil {
			errf(c, http.StatusInternalServerError, "%v", err)
			return
		}
	} else {
		var in connInput
		if err := c.ShouldBindJSON(&in); err != nil {
			errf(c, http.StatusBadRequest, "%v", err)
			return
		}
		if in.Config == nil {
			in.Config = map[string]any{}
		}
		if err := checkConfig(p.m, in.Config, in.Secrets); err != nil {
			errf(c, http.StatusBadRequest, "%v", err)
			return
		}
		cfg, _ := json.Marshal(in.Config)
		tmp := models.PluginConnection{ID: "test-" + uuid.New().String(), ConfigJSON: string(cfg), UpdatedAt: time.Now()}
		var err error
		if rc, err = s.runtimeConn(tmp); err != nil {
			errf(c, http.StatusInternalServerError, "%v", err)
			return
		}
		rc.Secrets = in.Secrets
		defer s.rt.DropConnection(pluginID, tmp.ID)
	}
	out, _, err := s.rt.Call(c.Request.Context(), pluginID, rc, "test", []byte("{}"))
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": false, "error": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true, "result": rawOrText(out)}})
}

// revealSecret returns one decrypted secret field to the owner. Step-up
// gated (registered in Mount); never for an in-process (agent) request.
func (s *Service) revealSecret(c *gin.Context) {
	if middleware.IsInProcessRequest(c.Request.Context()) {
		errf(c, http.StatusForbidden, "Secrets are revealed to people only")
		return
	}
	pluginID := c.Param("pluginId")
	p, ok := s.lookup(pluginID, false)
	if !ok {
		errf(c, http.StatusNotFound, "Plugin not found")
		return
	}
	cn, err := s.loadOwned(pluginID, c.Param("id"), c.GetString("userID"))
	if err != nil {
		errf(c, http.StatusNotFound, "Connection not found")
		return
	}
	var in struct {
		Which string `json:"which"`
	}
	_ = c.ShouldBindJSON(&in)
	if !p.m.SecretField(in.Which) {
		errf(c, http.StatusBadRequest, "which must be a declared secret field")
		return
	}
	sec, err := s.secrets(*cn)
	if err != nil {
		errf(c, http.StatusInternalServerError, "decrypt")
		return
	}
	s.audit(c, models.PluginActivity{PluginID: pluginID, PluginVersion: p.row.Version, ConnectionID: cn.ID,
		Action: "reveal", DetailJSON: detail(gin.H{"field": in.Which})})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"value": sec[in.Which]}})
}

// ── grants ──────────────────────────────────────────────────────────

func (s *Service) addGrant(c *gin.Context) {
	userID := c.GetString("userID")
	cn, err := s.loadOwned(c.Param("pluginId"), c.Param("id"), userID)
	if err != nil {
		errf(c, http.StatusNotFound, "Connection not found")
		return
	}
	var in struct {
		SubjectKind string `json:"subjectKind" binding:"required"`
		SubjectID   string `json:"subjectId" binding:"required"`
		Role        string `json:"role"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		errf(c, http.StatusBadRequest, "%v", err)
		return
	}
	if in.SubjectKind != "user" && in.SubjectKind != "team" {
		errf(c, http.StatusBadRequest, "subjectKind must be 'user' or 'team'")
		return
	}
	if in.Role == "" {
		in.Role = "read"
	}
	if in.Role != "read" && in.Role != "write" {
		errf(c, http.StatusBadRequest, "role must be 'read' or 'write'")
		return
	}
	g := models.PluginConnectionGrant{ID: uuid.New().String(), ConnectionID: cn.ID, SubjectKind: in.SubjectKind,
		SubjectID: in.SubjectID, Role: in.Role, GrantedBy: userID, CreatedAt: time.Now()}
	if err := s.db.Create(&g).Error; err != nil {
		errf(c, http.StatusInternalServerError, "save grant")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": g})
}

func (s *Service) listGrants(c *gin.Context) {
	cn, _, err := s.loadAccessible(c.Param("pluginId"), c.Param("id"), c.GetString("userID"))
	if err != nil {
		errf(c, http.StatusNotFound, "Connection not found")
		return
	}
	var grants []models.PluginConnectionGrant
	if err := s.db.Where("connection_id = ?", cn.ID).Order("created_at DESC").Find(&grants).Error; err != nil {
		errf(c, http.StatusInternalServerError, "list grants")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": grants})
}

func (s *Service) deleteGrant(c *gin.Context) {
	pluginID := c.Param("pluginId")
	cn, err := s.loadOwned(pluginID, c.Param("id"), c.GetString("userID"))
	if err != nil {
		errf(c, http.StatusNotFound, "Connection not found")
		return
	}
	res := s.db.Where("id = ? AND connection_id = ?", c.Param("grantId"), cn.ID).Delete(&models.PluginConnectionGrant{})
	if res.Error != nil {
		errf(c, http.StatusInternalServerError, "delete grant")
		return
	}
	if res.RowsAffected == 0 {
		errf(c, http.StatusNotFound, "Grant not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"deleted": true}})
}
