package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/esquery"
	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ESConnectionHandler covers the Elasticsearch connection surface — CRUD, the
// one-shot Test probe, owner-only secret reveal. Mirrors
// RedisConnectionHandler; cluster/index/mapping browse + the request forwarder
// live in es_ops.go because they need per-op permission + the write gate.
type ESConnectionHandler struct {
	db       *gorm.DB
	cipher   *services.TokenCipher
	registry *esquery.Registry
}

func NewESConnectionHandler(db *gorm.DB, cipher *services.TokenCipher, reg *esquery.Registry) *ESConnectionHandler {
	return &ESConnectionHandler{db: db, cipher: cipher, registry: reg}
}

type createESInput struct {
	ID           string         `json:"id"`
	Name         string         `json:"name" binding:"required"`
	Nodes        string         `json:"nodes"`
	CloudID      string         `json:"cloudId"`
	Flavor       string         `json:"flavor"`
	AuthKind     string         `json:"authKind"`
	Username     string         `json:"username"`
	Password     string         `json:"password"`
	ApiKey       string         `json:"apiKey"`
	Bearer       string         `json:"bearer"`
	Cookie       string         `json:"cookie"` // kibanaProxy cookie mode: the pasted Kibana `sid` cookie
	TLS          bool           `json:"tls"`
	Options      map[string]any `json:"options"`
	WorkspaceID  string         `json:"workspaceId"`
	PrimaryScope string         `json:"primaryScope"`
}

type updateESInput struct {
	Name         *string        `json:"name"`
	Nodes        *string        `json:"nodes"`
	CloudID      *string        `json:"cloudId"`
	Flavor       *string        `json:"flavor"`
	AuthKind     *string        `json:"authKind"`
	Username     *string        `json:"username"`
	Password     *string        `json:"password"` // nil = keep existing
	ApiKey       *string        `json:"apiKey"`   // nil = keep existing
	Bearer       *string        `json:"bearer"`   // nil = keep existing
	Cookie       *string        `json:"cookie"`   // nil = keep existing (kibanaProxy cookie mode)
	TLS          *bool          `json:"tls"`
	Options      map[string]any `json:"options"`
	PrimaryScope *string        `json:"primaryScope"`
}

type esConnectionDTO struct {
	ID          string `json:"id"`
	UserID      string `json:"userId"`
	WorkspaceID string `json:"workspaceId,omitempty"`
	Name        string `json:"name"`
	Nodes       string `json:"nodes"`
	CloudID     string `json:"cloudId,omitempty"`
	Flavor      string `json:"flavor"`
	AuthKind    string `json:"authKind"`
	Username    string `json:"username,omitempty"`
	HasPassword bool   `json:"hasPassword"`
	HasApiKey   bool   `json:"hasApiKey"`
	HasBearer   bool   `json:"hasBearer"`
	HasCookie   bool   `json:"hasCookie"`
	// kibanaProxy knobs surfaced from OptionsJSON (the cookie itself is never
	// returned — HasCookie only).
	KibanaURL      string    `json:"kibanaUrl,omitempty"`
	KibanaAuthMode string    `json:"kibanaAuthMode,omitempty"`
	KibanaSpace    string    `json:"kibanaSpace,omitempty"`
	TLS            bool      `json:"tls"`
	Options        any       `json:"options,omitempty"`
	OwnerName      string    `json:"ownerName,omitempty"`
	Shared         bool      `json:"shared"`
	GrantRole      string    `json:"grantRole,omitempty"`
	PrimaryScope   string    `json:"primaryScope,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

func validESFlavor(f string) bool {
	switch strings.ToLower(strings.TrimSpace(f)) {
	case "elasticsearch", "opensearch", "auto", "":
		return true
	}
	return false
}

func validESAuthKind(a string) bool {
	switch strings.ToLower(strings.TrimSpace(a)) {
	case "none", "basic", "apikey", "bearer", "kibanaproxy", "":
		return true
	}
	return false
}

func normalizeESFlavor(f string) string {
	v := strings.ToLower(strings.TrimSpace(f))
	if v == "" {
		return "auto"
	}
	return v
}

func normalizeESAuthKind(a string) string {
	v := strings.ToLower(strings.TrimSpace(a))
	switch v {
	case "apikey":
		return "apiKey"
	case "kibanaproxy":
		return "kibanaProxy"
	case "":
		return "none"
	}
	return v
}

// kibanaURLFromOptions reads options.kibanaUrl (trimmed) from a create/update
// options map. Empty when absent.
func kibanaURLFromOptions(opts map[string]any) string {
	if opts == nil {
		return ""
	}
	s, _ := opts["kibanaUrl"].(string)
	return strings.TrimSpace(s)
}

func (h *ESConnectionHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")
	var input createESInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !validESFlavor(input.Flavor) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "flavor must be elasticsearch, opensearch, or auto"})
		return
	}
	if !validESAuthKind(input.AuthKind) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "authKind must be none, basic, apiKey, bearer, or kibanaProxy"})
		return
	}
	if normalizeESAuthKind(input.AuthKind) == "kibanaProxy" {
		if kibanaURLFromOptions(input.Options) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "kibanaUrl is required for the Kibana proxy auth mode"})
			return
		}
	} else if strings.TrimSpace(input.Nodes) == "" && strings.TrimSpace(input.CloudID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "nodes or cloudId is required"})
		return
	}
	passEnc, err := h.encOrEmpty(input.Password)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt password"})
		return
	}
	apiEnc, err := h.encOrEmpty(input.ApiKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt apiKey"})
		return
	}
	bearerEnc, err := h.encOrEmpty(input.Bearer)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt bearer"})
		return
	}
	cookieEnc, err := h.encOrEmpty(input.Cookie)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt cookie"})
		return
	}
	optBytes, _ := json.Marshal(or(input.Options, map[string]any{}))
	id := strings.TrimSpace(input.ID)
	if id == "" {
		id = uuid.New().String()
	}
	row := models.ESConnection{
		ID:           id,
		UserID:       userID,
		WorkspaceID:  input.WorkspaceID,
		Name:         input.Name,
		Nodes:        input.Nodes,
		CloudID:      input.CloudID,
		Flavor:       normalizeESFlavor(input.Flavor),
		AuthKind:     normalizeESAuthKind(input.AuthKind),
		Username:     input.Username,
		PasswordEnc:  passEnc,
		ApiKeyEnc:    apiEnc,
		BearerEnc:    bearerEnc,
		CookieEnc:    cookieEnc,
		TLS:          input.TLS,
		OptionsJSON:  string(optBytes),
		PrimaryScope: normalizeScope(input.PrimaryScope),
	}
	if err := h.db.Create(&row).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save connection"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": toESDTO(row, "", false, "owner")})
}

func (h *ESConnectionHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	var own []models.ESConnection
	if err := h.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&own).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list connections"})
		return
	}
	var shared []models.ESConnection
	err := h.db.
		Where(`id IN (
			SELECT g.connection_id FROM es_connection_grants g
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
	out := make([]esConnectionDTO, 0, len(own)+len(shared))
	for _, cn := range own {
		out = append(out, toESDTO(cn, "", false, "owner"))
	}
	for _, cn := range shared {
		role := h.grantRole(cn.ID, userID)
		out = append(out, toESDTO(cn, owners[cn.UserID], true, role))
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *ESConnectionHandler) Get(c *gin.Context) {
	userID := c.GetString("userID")
	conn, role, err := h.loadAccessible(c.Param("id"), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": toESDTO(*conn, "", conn.UserID != userID, role)})
}

func (h *ESConnectionHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn models.ESConnection
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	var input updateESInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updates := map[string]any{}
	if input.Name != nil {
		updates["name"] = *input.Name
	}
	if input.Nodes != nil {
		updates["nodes"] = *input.Nodes
	}
	if input.CloudID != nil {
		updates["cloud_id"] = *input.CloudID
	}
	if input.Flavor != nil {
		if !validESFlavor(*input.Flavor) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid flavor"})
			return
		}
		updates["flavor"] = normalizeESFlavor(*input.Flavor)
	}
	if input.AuthKind != nil {
		if !validESAuthKind(*input.AuthKind) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid authKind"})
			return
		}
		updates["auth_kind"] = normalizeESAuthKind(*input.AuthKind)
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
	if input.ApiKey != nil {
		if *input.ApiKey == "" {
			updates["api_key_enc"] = ""
		} else {
			enc, err := h.cipher.Encrypt(*input.ApiKey)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt apiKey"})
				return
			}
			updates["api_key_enc"] = enc
		}
	}
	if input.Bearer != nil {
		if *input.Bearer == "" {
			updates["bearer_enc"] = ""
		} else {
			enc, err := h.cipher.Encrypt(*input.Bearer)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt bearer"})
				return
			}
			updates["bearer_enc"] = enc
		}
	}
	if input.Cookie != nil {
		if *input.Cookie == "" {
			updates["cookie_enc"] = ""
		} else {
			enc, err := h.cipher.Encrypt(*input.Cookie)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt cookie"})
				return
			}
			updates["cookie_enc"] = enc
		}
	}
	if input.TLS != nil {
		updates["tls"] = *input.TLS
	}
	if input.Options != nil {
		// When the effective auth mode is kibanaProxy and options are being
		// (re)written, kibanaUrl must survive — the proxy has no other endpoint.
		effKind := conn.AuthKind
		if input.AuthKind != nil {
			effKind = normalizeESAuthKind(*input.AuthKind)
		}
		if effKind == "kibanaProxy" && kibanaURLFromOptions(input.Options) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "kibanaUrl is required for the Kibana proxy auth mode"})
			return
		}
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
	c.JSON(http.StatusOK, gin.H{"data": toESDTO(conn, "", false, "owner")})
}

func (h *ESConnectionHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	res := h.db.Where("id = ? AND user_id = ?", id, userID).Delete(&models.ESConnection{})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete connection"})
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	h.db.Where("connection_id = ?", id).Delete(&models.ESConnectionGrant{})
	h.registry.Close(id)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "deleted"}})
}

func (h *ESConnectionHandler) Test(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn *models.ESConnection
	if id != "" && id != "new" {
		var existing models.ESConnection
		if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&existing).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
			return
		}
		conn = &existing
	} else {
		var input createESInput
		if err := c.ShouldBindJSON(&input); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if !validESFlavor(input.Flavor) || !validESAuthKind(input.AuthKind) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid flavor or authKind"})
			return
		}
		passEnc, err := h.encOrEmpty(input.Password)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt"})
			return
		}
		apiEnc, err := h.encOrEmpty(input.ApiKey)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt"})
			return
		}
		bearerEnc, err := h.encOrEmpty(input.Bearer)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt"})
			return
		}
		cookieEnc, err := h.encOrEmpty(input.Cookie)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt"})
			return
		}
		optBytes, _ := json.Marshal(or(input.Options, map[string]any{}))
		conn = &models.ESConnection{
			Nodes:       input.Nodes,
			CloudID:     input.CloudID,
			Flavor:      normalizeESFlavor(input.Flavor),
			AuthKind:    normalizeESAuthKind(input.AuthKind),
			Username:    input.Username,
			PasswordEnc: passEnc,
			ApiKeyEnc:   apiEnc,
			BearerEnc:   bearerEnc,
			CookieEnc:   cookieEnc,
			TLS:         input.TLS,
			OptionsJSON: string(optBytes),
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 12*time.Second)
	defer cancel()
	res, err := h.registry.Test(ctx, *conn)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": false, "error": err.Error()}})
		return
	}
	ok := res.Status >= 200 && res.Status < 300
	out := gin.H{"ok": ok, "status": res.Status}
	// Surface the detected version/flavor/cluster name from GET / when present.
	var info struct {
		ClusterName string `json:"cluster_name"`
		Tagline     string `json:"tagline"`
		Version     struct {
			Number       string `json:"number"`
			Distribution string `json:"distribution"`
		} `json:"version"`
	}
	if json.Unmarshal(res.Body, &info) == nil {
		out["version"] = info.Version.Number
		out["clusterName"] = info.ClusterName
		out["flavor"] = esquery.DeriveFlavorPublic(conn.Flavor, info.Version.Distribution, info.Tagline)
	}
	if !ok {
		out["error"] = string(res.Body)
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// RevealSecret returns a decrypted secret to the connection owner only. The
// body names which secret: {"which":"password"|"apiKey"|"bearer"}.
func (h *ESConnectionHandler) RevealSecret(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn models.ESConnection
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	var body struct {
		Which string `json:"which"`
	}
	_ = c.ShouldBindJSON(&body)
	enc := conn.PasswordEnc
	switch strings.ToLower(strings.TrimSpace(body.Which)) {
	case "apikey":
		enc = conn.ApiKeyEnc
	case "bearer":
		enc = conn.BearerEnc
	case "cookie":
		enc = conn.CookieEnc
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

func (h *ESConnectionHandler) encOrEmpty(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	return h.cipher.Encrypt(plain)
}

// loadAccessible loads a connection if the user owns it or holds any grant.
// Returns the row + effective role ("owner" | "write" | "read").
func (h *ESConnectionHandler) loadAccessible(id, userID string) (*models.ESConnection, string, error) {
	var conn models.ESConnection
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
func (h *ESConnectionHandler) grantRole(connID, userID string) string {
	var grants []models.ESConnectionGrant
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

func toESDTO(c models.ESConnection, ownerName string, shared bool, grantRole string) esConnectionDTO {
	var opts any
	var kibanaURL, kibanaAuthMode, kibanaSpace string
	if c.OptionsJSON != "" {
		_ = json.Unmarshal([]byte(c.OptionsJSON), &opts)
		var m map[string]any
		if json.Unmarshal([]byte(c.OptionsJSON), &m) == nil {
			kibanaURL, _ = m["kibanaUrl"].(string)
			kibanaAuthMode, _ = m["kibanaAuthMode"].(string)
			kibanaSpace, _ = m["kibanaSpace"].(string)
		}
	}
	return esConnectionDTO{
		ID: c.ID, UserID: c.UserID, WorkspaceID: c.WorkspaceID, Name: c.Name,
		Nodes: c.Nodes, CloudID: c.CloudID, Flavor: c.Flavor, AuthKind: c.AuthKind,
		Username: c.Username, HasPassword: c.PasswordEnc != "",
		HasApiKey: c.ApiKeyEnc != "", HasBearer: c.BearerEnc != "",
		HasCookie:      c.CookieEnc != "",
		KibanaURL:      kibanaURL,
		KibanaAuthMode: kibanaAuthMode,
		KibanaSpace:    kibanaSpace,
		TLS:            c.TLS, Options: opts, OwnerName: ownerName, Shared: shared,
		GrantRole: grantRole, PrimaryScope: c.PrimaryScope,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}
