package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/s3client"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// S3ConnectionHandler covers the S3 connection surface — CRUD, Test,
// owner-only secret reveal. Object ops live in s3_object.go.
type S3ConnectionHandler struct {
	db       *gorm.DB
	cipher   *services.TokenCipher
	registry *s3client.Registry
}

func NewS3ConnectionHandler(db *gorm.DB, cipher *services.TokenCipher, reg *s3client.Registry) *S3ConnectionHandler {
	return &S3ConnectionHandler{db: db, cipher: cipher, registry: reg}
}

type createS3Input struct {
	ID                 string         `json:"id"`
	Name               string         `json:"name" binding:"required"`
	Provider           string         `json:"provider"`
	Endpoint           string         `json:"endpoint"`
	Region             string         `json:"region"`
	AccessKeyID        string         `json:"accessKeyId"`
	SecretKey          string         `json:"secretKey"`
	SessionToken       string         `json:"sessionToken"`
	ForcePathStyle     bool           `json:"forcePathStyle"`
	InsecureSkipVerify bool           `json:"insecureSkipVerify"`
	Options            map[string]any `json:"options"`
	WorkspaceID        string         `json:"workspaceId"`
	PrimaryScope       string         `json:"primaryScope"`
}

type updateS3Input struct {
	Name               *string        `json:"name"`
	Provider           *string        `json:"provider"`
	Endpoint           *string        `json:"endpoint"`
	Region             *string        `json:"region"`
	AccessKeyID        *string        `json:"accessKeyId"`
	SecretKey          *string        `json:"secretKey"`   // nil = keep, "" = clear
	SessionToken       *string        `json:"sessionToken"`
	ForcePathStyle     *bool          `json:"forcePathStyle"`
	InsecureSkipVerify *bool          `json:"insecureSkipVerify"`
	Options            map[string]any `json:"options"`
	PrimaryScope       *string        `json:"primaryScope"`
}

type s3ConnectionDTO struct {
	ID                 string    `json:"id"`
	UserID             string    `json:"userId"`
	WorkspaceID        string    `json:"workspaceId,omitempty"`
	Name               string    `json:"name"`
	Provider           string    `json:"provider"`
	Endpoint           string    `json:"endpoint,omitempty"`
	Region             string    `json:"region"`
	AccessKeyID        string    `json:"accessKeyId,omitempty"`
	HasSecretKey       bool      `json:"hasSecretKey"`
	HasSessionToken    bool      `json:"hasSessionToken"`
	ForcePathStyle     bool      `json:"forcePathStyle"`
	InsecureSkipVerify bool      `json:"insecureSkipVerify"`
	Options            any       `json:"options,omitempty"`
	OwnerName          string    `json:"ownerName,omitempty"`
	Shared             bool      `json:"shared"`
	GrantRole          string    `json:"grantRole,omitempty"`
	PrimaryScope       string    `json:"primaryScope,omitempty"`
	CreatedAt          time.Time `json:"createdAt"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

func (h *S3ConnectionHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")
	var input createS3Input
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	provider := strings.TrimSpace(input.Provider)
	if provider == "" {
		provider = "aws"
	}
	secretEnc := ""
	if input.SecretKey != "" {
		e, err := h.cipher.Encrypt(input.SecretKey)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt secret key"})
			return
		}
		secretEnc = e
	}
	tokenEnc := ""
	if input.SessionToken != "" {
		e, err := h.cipher.Encrypt(input.SessionToken)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt session token"})
			return
		}
		tokenEnc = e
	}
	region := strings.TrimSpace(input.Region)
	if region == "" {
		region = "us-east-1"
	}
	optBytes, _ := json.Marshal(or(input.Options, map[string]any{}))
	id := strings.TrimSpace(input.ID)
	if id == "" {
		id = uuid.New().String()
	}
	row := models.S3Connection{
		ID: id, UserID: userID, WorkspaceID: input.WorkspaceID, Name: input.Name,
		Provider: provider, Endpoint: input.Endpoint, Region: region,
		AccessKeyID: input.AccessKeyID, SecretKeyEnc: secretEnc, SessionTokenEnc: tokenEnc,
		ForcePathStyle: input.ForcePathStyle, InsecureSkipVerify: input.InsecureSkipVerify,
		OptionsJSON: string(optBytes), PrimaryScope: normalizeScope(input.PrimaryScope),
	}
	if err := h.db.Create(&row).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save connection"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": toS3DTO(row, "", false, "owner")})
}

func (h *S3ConnectionHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	var own []models.S3Connection
	if err := h.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&own).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list connections"})
		return
	}
	var shared []models.S3Connection
	err := h.db.
		Where(`id IN (
			SELECT g.connection_id FROM s3_connection_grants g
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
	out := make([]s3ConnectionDTO, 0, len(own)+len(shared))
	for _, cn := range own {
		out = append(out, toS3DTO(cn, "", false, "owner"))
	}
	for _, cn := range shared {
		out = append(out, toS3DTO(cn, owners[cn.UserID], true, h.grantRole(cn.ID, userID)))
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *S3ConnectionHandler) Get(c *gin.Context) {
	userID := c.GetString("userID")
	conn, role, err := h.loadAccessible(c.Param("id"), userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": toS3DTO(*conn, "", conn.UserID != userID, role)})
}

func (h *S3ConnectionHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn models.S3Connection
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	var input updateS3Input
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updates := map[string]any{}
	if input.Name != nil {
		updates["name"] = *input.Name
	}
	if input.Provider != nil {
		updates["provider"] = *input.Provider
	}
	if input.Endpoint != nil {
		updates["endpoint"] = *input.Endpoint
	}
	if input.Region != nil {
		updates["region"] = *input.Region
	}
	if input.AccessKeyID != nil {
		updates["access_key_id"] = *input.AccessKeyID
	}
	if input.SecretKey != nil {
		if *input.SecretKey == "" {
			updates["secret_key_enc"] = ""
		} else {
			e, err := h.cipher.Encrypt(*input.SecretKey)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt secret key"})
				return
			}
			updates["secret_key_enc"] = e
		}
	}
	if input.SessionToken != nil {
		if *input.SessionToken == "" {
			updates["session_token_enc"] = ""
		} else {
			e, err := h.cipher.Encrypt(*input.SessionToken)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt session token"})
				return
			}
			updates["session_token_enc"] = e
		}
	}
	if input.ForcePathStyle != nil {
		updates["force_path_style"] = *input.ForcePathStyle
	}
	if input.InsecureSkipVerify != nil {
		updates["insecure_skip_verify"] = *input.InsecureSkipVerify
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
	h.registry.Close(id)
	h.db.First(&conn, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{"data": toS3DTO(conn, "", false, "owner")})
}

func (h *S3ConnectionHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	res := h.db.Where("id = ? AND user_id = ?", id, userID).Delete(&models.S3Connection{})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete connection"})
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	h.db.Where("connection_id = ?", id).Delete(&models.S3ConnectionGrant{})
	h.registry.Close(id)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "deleted"}})
}

func (h *S3ConnectionHandler) Test(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn *models.S3Connection
	if id != "" && id != "new" {
		var existing models.S3Connection
		if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&existing).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
			return
		}
		conn = &existing
	} else {
		var input createS3Input
		if err := c.ShouldBindJSON(&input); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		secretEnc := ""
		if input.SecretKey != "" {
			e, err := h.cipher.Encrypt(input.SecretKey)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt"})
				return
			}
			secretEnc = e
		}
		tokenEnc := ""
		if input.SessionToken != "" {
			e, _ := h.cipher.Encrypt(input.SessionToken)
			tokenEnc = e
		}
		region := strings.TrimSpace(input.Region)
		if region == "" {
			region = "us-east-1"
		}
		conn = &models.S3Connection{
			Provider: input.Provider, Endpoint: input.Endpoint, Region: region,
			AccessKeyID: input.AccessKeyID, SecretKeyEnc: secretEnc, SessionTokenEnc: tokenEnc,
			ForcePathStyle: input.ForcePathStyle, InsecureSkipVerify: input.InsecureSkipVerify,
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 12*time.Second)
	defer cancel()
	if err := h.registry.Test(ctx, *conn); err != nil {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": false, "error": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

func (h *S3ConnectionHandler) RevealSecret(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var conn models.S3Connection
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&conn).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	secret := ""
	if conn.SecretKeyEnc != "" {
		s, err := h.cipher.Decrypt(conn.SecretKeyEnc)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "decrypt"})
			return
		}
		secret = s
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"secretKey": secret}})
}

// ── helpers ──────────────────────────────────────────────────────────

func (h *S3ConnectionHandler) loadAccessible(id, userID string) (*models.S3Connection, string, error) {
	var conn models.S3Connection
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

func (h *S3ConnectionHandler) grantRole(connID, userID string) string {
	var grants []models.S3ConnectionGrant
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

func toS3DTO(c models.S3Connection, ownerName string, shared bool, grantRole string) s3ConnectionDTO {
	var opts any
	if c.OptionsJSON != "" {
		_ = json.Unmarshal([]byte(c.OptionsJSON), &opts)
	}
	return s3ConnectionDTO{
		ID: c.ID, UserID: c.UserID, WorkspaceID: c.WorkspaceID, Name: c.Name,
		Provider: c.Provider, Endpoint: c.Endpoint, Region: c.Region,
		AccessKeyID: c.AccessKeyID, HasSecretKey: c.SecretKeyEnc != "",
		HasSessionToken: c.SessionTokenEnc != "", ForcePathStyle: c.ForcePathStyle,
		InsecureSkipVerify: c.InsecureSkipVerify, Options: opts,
		OwnerName: ownerName, Shared: shared, GrantRole: grantRole,
		PrimaryScope: c.PrimaryScope, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}
