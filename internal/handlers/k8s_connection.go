package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/k8sclient"
	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// K8sConnectionHandler covers the Kubernetes connection surface — CRUD, the
// one-shot Test probe (Discovery ServerVersion), owner-only kubeconfig reveal.
// Mirrors ESConnectionHandler; the read-only cluster ops (contexts / version /
// resources / logs) live in k8s_ops.go because they carry the per-op access
// check + audit. v1 is READ ONLY — there is no write/mutate surface.
type K8sConnectionHandler struct {
	db       *gorm.DB
	cipher   *services.TokenCipher
	registry *k8sclient.Registry
}

func NewK8sConnectionHandler(db *gorm.DB, cipher *services.TokenCipher, reg *k8sclient.Registry) *K8sConnectionHandler {
	return &K8sConnectionHandler{db: db, cipher: cipher, registry: reg}
}

type createK8sInput struct {
	ID             string         `json:"id"`
	Name           string         `json:"name" binding:"required"`
	Kubeconfig     string         `json:"kubeconfig"`
	DefaultContext string         `json:"defaultContext"`
	AuthMode       string         `json:"authMode"`
	Options        map[string]any `json:"options"`
	WorkspaceID    string         `json:"workspaceId"`
	PrimaryScope   string         `json:"primaryScope"`
}

type updateK8sInput struct {
	Name           *string        `json:"name"`
	Kubeconfig     *string        `json:"kubeconfig"` // nil = keep existing
	DefaultContext *string        `json:"defaultContext"`
	AuthMode       *string        `json:"authMode"`
	Options        map[string]any `json:"options"`
	PrimaryScope   *string        `json:"primaryScope"`
}

type k8sConnectionDTO struct {
	ID             string                  `json:"id"`
	UserID         string                  `json:"userId"`
	WorkspaceID    string                  `json:"workspaceId,omitempty"`
	Name           string                  `json:"name"`
	DefaultContext string                  `json:"defaultContext,omitempty"`
	Contexts       []k8sclient.ContextInfo `json:"contexts"`
	AuthMode       string                  `json:"authMode"`
	HasKubeconfig  bool                    `json:"hasKubeconfig"`
	Options        any                     `json:"options,omitempty"`
	OwnerName      string                  `json:"ownerName,omitempty"`
	Shared         bool                    `json:"shared"`
	GrantRole      string                  `json:"grantRole,omitempty"`
	PrimaryScope   string                  `json:"primaryScope,omitempty"`
	CreatedAt      time.Time               `json:"createdAt"`
	UpdatedAt      time.Time               `json:"updatedAt"`
}

func normalizeK8sAuthMode(a string) string {
	v := strings.ToLower(strings.TrimSpace(a))
	if v == "" {
		return "kubeconfig"
	}
	return v
}

func (h *K8sConnectionHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")
	var input createK8sInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(input.Kubeconfig) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "kubeconfig is required"})
		return
	}
	// Parse (no cluster call) to cache the non-secret context list + derive the
	// default context; also validates the kubeconfig before we store it.
	contexts, current, err := k8sclient.ParseContexts(input.Kubeconfig)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	enc, err := h.cipher.Encrypt(input.Kubeconfig)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt kubeconfig"})
		return
	}
	ctxName := strings.TrimSpace(input.DefaultContext)
	if ctxName == "" {
		ctxName = current
	}
	ctxJSON, _ := json.Marshal(contexts)
	optBytes, _ := json.Marshal(or(input.Options, map[string]any{}))
	id := strings.TrimSpace(input.ID)
	if id == "" {
		id = uuid.New().String()
	}
	row := models.K8sConnection{
		ID:             id,
		UserID:         userID,
		WorkspaceID:    input.WorkspaceID,
		Name:           input.Name,
		KubeconfigEnc:  enc,
		DefaultContext: ctxName,
		ContextsJSON:   string(ctxJSON),
		AuthMode:       normalizeK8sAuthMode(input.AuthMode),
		OptionsJSON:    string(optBytes),
		PrimaryScope:   normalizeScope(input.PrimaryScope),
	}
	if err := h.db.Create(&row).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save connection"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": toK8sDTO(row, "", false, "owner")})
}

func (h *K8sConnectionHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	var own []models.K8sConnection
	if err := h.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&own).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list connections"})
		return
	}
	var shared []models.K8sConnection
	err := h.db.
		Where(`id IN (
			SELECT g.connection_id FROM k8s_connection_grants g
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
	out := make([]k8sConnectionDTO, 0, len(own)+len(shared))
	for _, cn := range own {
		out = append(out, toK8sDTO(cn, "", false, "owner"))
	}
	for _, cn := range shared {
		role := h.grantRole(cn.ID, userID)
		out = append(out, toK8sDTO(cn, owners[cn.UserID], true, role))
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *K8sConnectionHandler) Get(c *gin.Context) {
	userID := c.GetString("userID")
	conn, role, err := h.loadAccessible(c.Param("id"), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": toK8sDTO(*conn, "", conn.UserID != userID, role)})
}

func (h *K8sConnectionHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn models.K8sConnection
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	var input updateK8sInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updates := map[string]any{}
	if input.Name != nil {
		updates["name"] = *input.Name
	}
	if input.Kubeconfig != nil {
		if strings.TrimSpace(*input.Kubeconfig) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "kubeconfig cannot be empty"})
			return
		}
		contexts, current, err := k8sclient.ParseContexts(*input.Kubeconfig)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		enc, err := h.cipher.Encrypt(*input.Kubeconfig)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt kubeconfig"})
			return
		}
		ctxJSON, _ := json.Marshal(contexts)
		updates["kubeconfig_enc"] = enc
		updates["contexts_json"] = string(ctxJSON)
		// If the default context is now gone, fall back to the new current.
		if input.DefaultContext == nil && !contextExists(contexts, conn.DefaultContext) {
			updates["default_context"] = current
		}
	}
	if input.DefaultContext != nil {
		updates["default_context"] = *input.DefaultContext
	}
	if input.AuthMode != nil {
		updates["auth_mode"] = normalizeK8sAuthMode(*input.AuthMode)
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
	h.registry.Close(id) // pooled clientsets may be stale after an edit
	h.db.First(&conn, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{"data": toK8sDTO(conn, "", false, "owner")})
}

func (h *K8sConnectionHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	res := h.db.Where("id = ? AND user_id = ?", id, userID).Delete(&models.K8sConnection{})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete connection"})
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	h.db.Where("connection_id = ?", id).Delete(&models.K8sConnectionGrant{})
	h.registry.Close(id)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "deleted"}})
}

// Test probes reachability via the Discovery ServerVersion endpoint. Works for
// both a saved connection (:id) and an unsaved draft (POST /test with a body).
func (h *K8sConnectionHandler) Test(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn *models.K8sConnection
	if id != "" && id != "new" {
		var existing models.K8sConnection
		if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&existing).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
			return
		}
		conn = &existing
	} else {
		var input createK8sInput
		if err := c.ShouldBindJSON(&input); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if strings.TrimSpace(input.Kubeconfig) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "kubeconfig is required"})
			return
		}
		enc, err := h.cipher.Encrypt(input.Kubeconfig)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt"})
			return
		}
		optBytes, _ := json.Marshal(or(input.Options, map[string]any{}))
		conn = &models.K8sConnection{
			KubeconfigEnc:  enc,
			DefaultContext: input.DefaultContext,
			AuthMode:       normalizeK8sAuthMode(input.AuthMode),
			OptionsJSON:    string(optBytes),
		}
	}
	contextName := c.Query("context")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	res, err := h.registry.Test(ctx, *conn, contextName)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": false, "reachable": false, "error": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"ok":        res.Reachable,
		"reachable": res.Reachable,
		"version":   res.Version,
		"platform":  res.Platform,
	}})
}

// RevealSecret returns the decrypted kubeconfig to the connection owner only.
// POST /k8s-connections/:id/reveal-secret  {} -> {kubeconfig}
func (h *K8sConnectionHandler) RevealSecret(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn models.K8sConnection
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	value := ""
	if conn.KubeconfigEnc != "" {
		p, err := h.cipher.Decrypt(conn.KubeconfigEnc)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "decrypt"})
			return
		}
		value = p
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"kubeconfig": value}})
}

// ── helpers ─────────────────────────────────────────────────────────

func contextExists(list []k8sclient.ContextInfo, name string) bool {
	for _, ci := range list {
		if ci.Name == name {
			return true
		}
	}
	return false
}

// loadAccessible loads a connection if the user owns it or holds any grant.
func (h *K8sConnectionHandler) loadAccessible(id, userID string) (*models.K8sConnection, string, error) {
	var conn models.K8sConnection
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

// grantRole returns the highest role the user holds on a connection ("write"
// beats "read"), or "" if none. v1 reads accept any grant.
func (h *K8sConnectionHandler) grantRole(connID, userID string) string {
	var grants []models.K8sConnectionGrant
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

func toK8sDTO(c models.K8sConnection, ownerName string, shared bool, grantRole string) k8sConnectionDTO {
	var opts any
	if c.OptionsJSON != "" {
		_ = json.Unmarshal([]byte(c.OptionsJSON), &opts)
	}
	contexts := []k8sclient.ContextInfo{}
	if strings.TrimSpace(c.ContextsJSON) != "" {
		_ = json.Unmarshal([]byte(c.ContextsJSON), &contexts)
	}
	return k8sConnectionDTO{
		ID: c.ID, UserID: c.UserID, WorkspaceID: c.WorkspaceID, Name: c.Name,
		DefaultContext: c.DefaultContext, Contexts: contexts, AuthMode: c.AuthMode,
		HasKubeconfig: c.KubeconfigEnc != "", Options: opts, OwnerName: ownerName,
		Shared: shared, GrantRole: grantRole, PrimaryScope: c.PrimaryScope,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}
