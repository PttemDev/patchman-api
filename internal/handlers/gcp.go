package handlers

// Google Cloud (GOOGLE_CLOUD_RFC.md): shared credentials + the services that
// use them — Cloud Storage, Pub/Sub, Cloud Logging and GKE clusters.
//
// Invariants kept here:
//   - secrets (key JSON, refresh tokens) are encrypted with the DB cipher and
//     never returned;
//   - a credential belongs to its owner; nobody else can use it through these
//     routes (a GKE cluster added to the K8s tool runs with its owner's
//     credential — that's what sharing that K8s connection means);
//   - "adc" (gcloud on this computer) only on the desktop backend;
//   - writes need the credential's readWrite and allowWrite on the call.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/config"
	"patchman-be/internal/gcp"
	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"gorm.io/gorm"
)

type GCPHandler struct {
	db     *gorm.DB
	cipher *services.TokenCipher
	cfg    *config.Config
}

func NewGCPHandler(db *gorm.DB, cipher *services.TokenCipher, cfg *config.Config) *GCPHandler {
	return &GCPHandler{db: db, cipher: cipher, cfg: cfg}
}

// oauthApp picks the OAuth client for an email (RFC ADR-4): the External
// client for addresses outside the Workspace domains when it is configured.
func (h *GCPHandler) oauthApp(email string) gcp.OAuthApp {
	if h.cfg == nil {
		return gcp.OAuthApp{}
	}
	ext := gcp.OAuthApp{ClientID: h.cfg.GoogleExternalClientID, ClientSecret: h.cfg.GoogleExternalClientSecret}
	if ext.ClientID != "" && email != "" && len(h.cfg.GoogleWorkspaceDomains) > 0 {
		_, domain, _ := strings.Cut(strings.ToLower(email), "@")
		inside := false
		for _, d := range h.cfg.GoogleWorkspaceDomains {
			if strings.EqualFold(d, domain) {
				inside = true
			}
		}
		if !inside {
			return ext
		}
	}
	if h.cfg.GoogleClientID != "" {
		return gcp.OAuthApp{ClientID: h.cfg.GoogleClientID, ClientSecret: h.cfg.GoogleClientSecret}
	}
	return ext
}

// ── credentials ─────────────────────────────────────────────────────────────

func gcpDTO(c models.GCPCredential) gin.H {
	return gin.H{"id": c.ID, "name": c.Name, "kind": c.Kind, "projectId": c.ProjectID, "account": c.Account,
		"readWrite": c.ReadWrite, "bigQuery": gcpGrantsBigQuery(c), "createdAt": c.CreatedAt, "updatedAt": c.UpdatedAt}
}

// gcpGrantsBigQuery: can the credential run BigQuery jobs? A linked Google
// account only has the scopes ticked when it was connected (bigquery, or
// cloud-platform via GKE); service accounts / gcloud ask for the scope on
// every token. A legacy row without recorded scopes is given the benefit of
// the doubt (Google answers 403 then).
func gcpGrantsBigQuery(c models.GCPCredential) bool {
	if c.Kind != "oauth" || strings.TrimSpace(c.Scopes) == "" {
		return true
	}
	for _, s := range strings.Fields(c.Scopes) {
		if s == gcp.ScopeBigQuery || s == gcp.ScopeCloud {
			return true
		}
	}
	return false
}

func (h *GCPHandler) own(c *gin.Context) (models.GCPCredential, bool) {
	var row models.GCPCredential
	if err := h.db.Where("id = ? AND user_id = ?", c.Param("id"), c.GetString("userID")).First(&row).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Google Cloud credential not found"})
		return row, false
	}
	return row, true
}

func (h *GCPHandler) source(row models.GCPCredential) (gcp.Source, error) {
	s := gcp.Source{Kind: row.Kind, OAuth: h.oauthApp(row.Account)}
	if row.Kind == "adc" && !embeddedMode.Load() {
		return s, errors.New("gcloud credentials can only be used by the desktop app")
	}
	if row.SecretEnc != "" {
		sec, err := h.cipher.Decrypt(row.SecretEnc)
		if err != nil {
			return s, errors.New("can't decrypt the credential")
		}
		s.Secret = sec
	}
	return s, nil
}

// client builds an authenticated client for the credential in the route.
func (h *GCPHandler) client(c *gin.Context, scopes ...string) (*gcp.Client, models.GCPCredential, bool) {
	row, ok := h.own(c)
	if !ok {
		return nil, row, false
	}
	src, err := h.source(row)
	if err != nil {
		c.JSON(http.StatusPreconditionFailed, gin.H{"error": err.Error()})
		return nil, row, false
	}
	cl, err := gcp.NewClient(c.Request.Context(), src, scopes...)
	if err != nil {
		c.JSON(http.StatusPreconditionFailed, gin.H{"error": err.Error()})
		return nil, row, false
	}
	return cl, row, true
}

// TokenSourceFor serves other tools (GKE in the K8s registry): the owner's
// credential as a refreshing token source.
func (h *GCPHandler) TokenSourceFor(ownerID, credID string, scopes ...string) (oauth2.TokenSource, error) {
	var row models.GCPCredential
	if err := h.db.Where("id = ? AND user_id = ?", credID, ownerID).First(&row).Error; err != nil {
		return nil, errors.New("the Google Cloud credential of this cluster is gone — add the cluster again")
	}
	src, err := h.source(row)
	if err != nil {
		return nil, err
	}
	if gcp.Overridden() {
		return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake-gcp-token", Expiry: time.Now().Add(time.Hour)}), nil
	}
	return gcp.TokenSource(context.Background(), src, scopes...)
}

// BigQueryTokenSource serves BigQuery DB connections with auth "credential"
// (dbquery.GCPTokenSource): the connection OWNER's credential — anyone else's
// is refused, exactly like GKE — as a refreshing BigQuery token source.
func (h *GCPHandler) BigQueryTokenSource(ownerID, credID string) (oauth2.TokenSource, error) {
	row, err := lookupBigQueryCredential(h.db, ownerID, credID)
	if err != nil {
		return nil, err
	}
	src, err := h.source(row)
	if err != nil {
		return nil, err
	}
	if gcp.Overridden() {
		return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake-gcp-token", Expiry: time.Now().Add(time.Hour)}), nil
	}
	return gcp.TokenSource(context.Background(), src, gcp.ScopeBigQuery)
}

func (h *GCPHandler) List(c *gin.Context) {
	var rows []models.GCPCredential
	h.db.Where("user_id = ?", c.GetString("userID")).Order("created_at").Find(&rows)
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		out = append(out, gcpDTO(r))
	}
	c.JSON(http.StatusOK, gin.H{"data": out, "meta": gin.H{
		"oauthAvailable": h.oauthApp("").ClientID != "", "adcAllowed": embeddedMode.Load(),
	}})
}

type gcpCredInput struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Key       string `json:"key"` // service account key JSON
	ProjectID string `json:"projectId"`
	ReadWrite bool   `json:"readWrite"`
}

func (h *GCPHandler) Create(c *gin.Context) {
	var in gcpCredInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	row := models.GCPCredential{ID: strings.TrimSpace(in.ID), UserID: c.GetString("userID"), Name: strings.TrimSpace(in.Name),
		Kind: in.Kind, ProjectID: strings.TrimSpace(in.ProjectID), ReadWrite: in.ReadWrite}
	if row.ID == "" {
		row.ID = uuid.NewString()
	} else if _, err := uuid.Parse(row.ID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id must be a UUID"})
		return
	}
	switch in.Kind {
	case "service_account":
		email, project, err := gcp.ServiceAccountInfo(in.Key)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		enc, err := h.cipher.Encrypt(in.Key)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt key"})
			return
		}
		row.SecretEnc, row.Account = enc, email
		if row.ProjectID == "" {
			row.ProjectID = project
		}
	case "adc":
		if !embeddedMode.Load() {
			c.JSON(http.StatusBadRequest, gin.H{"error": "gcloud credentials can only be added in the desktop app"})
			return
		}
		info := gcp.FindADC()
		if !info.Found {
			c.JSON(http.StatusBadRequest, gin.H{"error": "No gcloud credentials on this computer — run `gcloud auth application-default login` first."})
			return
		}
		row.Account = info.Account
		if row.ProjectID == "" {
			row.ProjectID = info.Project
		}
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind must be service_account or adc (a Google account is linked with Connect Google Cloud)"})
		return
	}
	if row.Name == "" {
		row.Name = firstNonEmpty(row.Account, row.ProjectID, "Google Cloud")
	}
	if err := h.db.Create(&row).Error; err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "save credential: " + err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gcpDTO(row)})
}

func (h *GCPHandler) Update(c *gin.Context) {
	row, ok := h.own(c)
	if !ok {
		return
	}
	var in struct {
		Name      *string `json:"name"`
		ProjectID *string `json:"projectId"`
		ReadWrite *bool   `json:"readWrite"`
		Key       string  `json:"key"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	upd := map[string]any{}
	if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
		upd["name"] = strings.TrimSpace(*in.Name)
	}
	if in.ProjectID != nil {
		upd["project_id"] = strings.TrimSpace(*in.ProjectID)
	}
	if in.ReadWrite != nil {
		upd["read_write"] = *in.ReadWrite
	}
	if strings.TrimSpace(in.Key) != "" {
		if row.Kind != "service_account" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "only a service account takes a key"})
			return
		}
		email, _, err := gcp.ServiceAccountInfo(in.Key)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		enc, err := h.cipher.Encrypt(in.Key)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt key"})
			return
		}
		upd["secret_enc"], upd["account"] = enc, email
	}
	if len(upd) > 0 {
		h.db.Model(&row).Updates(upd)
	}
	h.db.First(&row, "id = ?", row.ID)
	c.JSON(http.StatusOK, gin.H{"data": gcpDTO(row)})
}

// Delete removes the credential; GKE clusters added with it stop working, so
// the answer names them (the FE lists them before asking).
func (h *GCPHandler) Delete(c *gin.Context) {
	row, ok := h.own(c)
	if !ok {
		return
	}
	used := h.usedBy(row)
	if c.Query("confirm") != "1" && len(used) > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "This credential is used by other connections.", "usedBy": used})
		return
	}
	// A linked account: revoke at Google too (best effort).
	if row.Kind == "oauth" && row.SecretEnc != "" && !gcp.Overridden() {
		if tok, err := h.cipher.Decrypt(row.SecretEnc); err == nil {
			_, _ = http.PostForm("https://oauth2.googleapis.com/revoke", url.Values{"token": {tok}})
		}
	}
	h.db.Delete(&row)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"deleted": true, "usedBy": used}})
}

func (h *GCPHandler) usedBy(row models.GCPCredential) []string {
	var ks []models.K8sConnection
	h.db.Where("user_id = ? AND options_json LIKE ?", row.UserID, "%"+row.ID+"%").Find(&ks)
	out := []string{}
	for _, k := range ks {
		out = append(out, "Kubernetes: "+k.Name)
	}
	var bqs []models.DBConnection
	h.db.Where("user_id = ? AND kind = ? AND options_json LIKE ?", row.UserID, "bigquery", "%"+row.ID+"%").Find(&bqs)
	for _, b := range bqs {
		out = append(out, "BigQuery: "+b.Name)
	}
	return out
}

// Projects lists the projects the credential can see (Resource Manager);
// without that permission the credential's own project is returned.
func (h *GCPHandler) Projects(c *gin.Context) {
	cl, row, ok := h.client(c, gcp.ScopeReadOnly)
	if !ok {
		return
	}
	var out struct {
		Projects []struct {
			ProjectID      string `json:"projectId"`
			Name           string `json:"name"`
			LifecycleState string `json:"lifecycleState"`
		} `json:"projects"`
	}
	err := cl.Do(c.Request.Context(), "GET", gcp.ProjectsBase()+"/projects", url.Values{"pageSize": {"200"}, "filter": {"lifecycleState:ACTIVE"}}, nil, &out)
	list := []gin.H{}
	for _, p := range out.Projects {
		list = append(list, gin.H{"id": p.ProjectID, "name": p.Name})
	}
	if row.ProjectID != "" {
		found := false
		for _, p := range list {
			if p["id"] == row.ProjectID {
				found = true
			}
		}
		if !found {
			list = append([]gin.H{{"id": row.ProjectID, "name": row.ProjectID}}, list...)
		}
	}
	if err != nil && len(list) == 0 {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	resp := gin.H{"data": list}
	if err != nil {
		resp["warning"] = "Listing projects isn't allowed for this credential; showing its own project."
	}
	c.JSON(http.StatusOK, resp)
}

// Test checks the credential can get a token and see its project.
func (h *GCPHandler) Test(c *gin.Context) {
	h.Projects(c)
}

// ADC — GET /gcp/adc: what gcloud left on this computer (desktop only).
func (h *GCPHandler) ADC(c *gin.Context) {
	if !embeddedMode.Load() {
		c.JSON(http.StatusOK, gin.H{"data": gcp.ADCInfo{}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gcp.FindADC()})
}

// ── linked Google account (OAuth, incremental scopes) ───────────────────────

const gcpOAuthKind = "gcp_link"

// gcpScopesFor maps the services the user picked to OAuth scopes; read-only
// unless write was asked for.
func gcpScopesFor(services []string, write bool) []string {
	scopes := []string{"openid", "email", gcp.ScopeReadOnly}
	for _, s := range services {
		switch s {
		case "storage":
			if write {
				scopes = append(scopes, gcp.ScopeStorageWrite)
			} else {
				scopes = append(scopes, gcp.ScopeStorageRead)
			}
		case "pubsub":
			scopes = append(scopes, gcp.ScopePubSub)
		case "logging":
			scopes = append(scopes, gcp.ScopeLoggingRead)
		case "bigquery":
			scopes = append(scopes, gcp.ScopeBigQuery)
		case "gke":
			scopes = append(scopes, gcp.ScopeCloud) // the Kubernetes API takes cloud-platform tokens
		}
	}
	return scopes
}

// OAuthStart — POST /gcp/oauth/start {origin, services[], write, email}:
// the Google consent URL. The callback page (<origin>/auth/google-cloud/callback)
// posts code + state back to OAuthComplete.
func (h *GCPHandler) OAuthStart(c *gin.Context) {
	var in struct {
		Origin   string   `json:"origin"`
		Services []string `json:"services"`
		Write    bool     `json:"write"`
		Email    string   `json:"email"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	app := h.oauthApp(in.Email)
	if app.ClientID == "" {
		c.JSON(http.StatusPreconditionFailed, gin.H{"error": "Google sign-in isn't configured on this server (GOOGLE_OAUTH_CLIENT_ID)."})
		return
	}
	if !originAllowed(h.cfg, in.Origin) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "origin isn't an allowed callback host"})
		return
	}
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	state := hex.EncodeToString(b)
	redirect := strings.TrimRight(in.Origin, "/") + "/auth/google-cloud/callback"
	h.db.Where("kind = ? AND created_at < ?", gcpOAuthKind, time.Now().Add(-15*time.Minute)).Delete(&models.OAuthPendingState{})
	h.db.Create(&models.OAuthPendingState{State: state, Kind: gcpOAuthKind, Provider: strings.Join(in.Services, ","), UserID: c.GetString("userID"),
		RedirectURI: redirect, CodeVerifier: strconv.FormatBool(in.Write) + "|" + in.Email, CreatedAt: time.Now()})
	cfg := &oauth2.Config{ClientID: app.ClientID, ClientSecret: app.ClientSecret, Endpoint: google.Endpoint, RedirectURL: redirect,
		Scopes: gcpScopesFor(in.Services, in.Write)}
	u := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"),
		oauth2.SetAuthURLParam("include_granted_scopes", "true"))
	if in.Email != "" {
		u += "&login_hint=" + url.QueryEscape(in.Email)
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"url": u}})
}

// gcpExchange is swapped in tests.
var gcpExchange = func(ctx context.Context, cfg *oauth2.Config, code string) (*oauth2.Token, error) {
	return cfg.Exchange(ctx, code)
}

// OAuthComplete — POST /gcp/oauth/complete {code, state}: stores the linked
// account as a credential (kind oauth, the refresh token encrypted).
func (h *GCPHandler) OAuthComplete(c *gin.Context) {
	var in struct {
		Code  string `json:"code"`
		State string `json:"state"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || in.Code == "" || in.State == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "code and state are required"})
		return
	}
	var p models.OAuthPendingState
	if err := h.db.Where("state = ? AND kind = ?", in.State, gcpOAuthKind).First(&p).Error; err != nil || p.UserID != c.GetString("userID") ||
		time.Since(p.CreatedAt) > 15*time.Minute {
		c.JSON(http.StatusBadRequest, gin.H{"error": "This sign-in link expired or belongs to someone else — start again."})
		return
	}
	h.db.Delete(&p)
	_, email, _ := strings.Cut(p.CodeVerifier, "|")
	write := strings.HasPrefix(p.CodeVerifier, "true|")
	services := strings.Split(p.Provider, ",")
	app := h.oauthApp(email)
	cfg := &oauth2.Config{ClientID: app.ClientID, ClientSecret: app.ClientSecret, Endpoint: google.Endpoint, RedirectURL: p.RedirectURI,
		Scopes: gcpScopesFor(services, write)}
	tok, err := gcpExchange(c.Request.Context(), cfg, in.Code)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Google didn't accept the sign-in: " + err.Error()})
		return
	}
	if tok.RefreshToken == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Google returned no refresh token — remove PatchMan's access at myaccount.google.com/permissions and connect again."})
		return
	}
	account := email
	if idt, _ := tok.Extra("id_token").(string); idt != "" {
		if e := jwtEmail(idt); e != "" {
			account = e
		}
	}
	enc, err := h.cipher.Encrypt(tok.RefreshToken)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt token"})
		return
	}
	userID := c.GetString("userID")
	// One linked credential per Google account: reconnecting updates it.
	var row models.GCPCredential
	if account != "" && h.db.Where("user_id = ? AND kind = ? AND account = ?", userID, "oauth", account).First(&row).Error == nil {
		h.db.Model(&row).Updates(map[string]any{"secret_enc": enc, "scopes": strings.Join(cfg.Scopes, " "), "read_write": write || row.ReadWrite})
	} else {
		row = models.GCPCredential{ID: uuid.NewString(), UserID: userID, Name: firstNonEmpty(account, "Google account"), Kind: "oauth",
			SecretEnc: enc, Account: account, Scopes: strings.Join(cfg.Scopes, " "), ReadWrite: write}
		if err := h.db.Create(&row).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "save credential"})
			return
		}
	}
	h.db.First(&row, "id = ?", row.ID)
	c.JSON(http.StatusOK, gin.H{"data": gcpDTO(row)})
}

// jwtEmail reads the email claim of an ID token Google just issued to us over
// TLS (not used for authentication, only to name the credential).
func jwtEmail(idt string) string {
	parts := strings.Split(idt, ".")
	if len(parts) < 2 {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var cl struct {
		Email string `json:"email"`
	}
	_ = json.Unmarshal(b, &cl)
	return cl.Email
}

// ── shared helpers for the services ─────────────────────────────────────────

func gcpErr(c *gin.Context, err error) {
	var ae *gcp.APIError
	if errors.As(err, &ae) {
		code := http.StatusBadGateway
		if ae.Status == http.StatusNotFound {
			code = http.StatusNotFound
		}
		c.JSON(code, gin.H{"error": ae.Error()})
		return
	}
	c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
}

func gcpProject(c *gin.Context, row models.GCPCredential) (string, bool) {
	p := strings.TrimSpace(c.Query("project"))
	if p == "" {
		p = row.ProjectID
	}
	if p == "" || strings.ContainsAny(p, "/?# ") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "project is required (set a default project on the credential)"})
		return "", false
	}
	return p, true
}

// gcpWriteAllowed: the credential allows writes and the call asked for one.
func gcpWriteAllowed(c *gin.Context, row models.GCPCredential, allow bool) bool {
	if !row.ReadWrite {
		c.JSON(http.StatusForbidden, gin.H{"error": "This credential is read-only — turn on \"Allow writes\" in Settings → Google Cloud first."})
		return false
	}
	if !allow {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Writes need allowWrite=true."})
		return false
	}
	return true
}

func nameOK(c *gin.Context, field, v string) bool {
	if strings.TrimSpace(v) == "" || strings.ContainsAny(v, "/?#") {
		c.JSON(http.StatusBadRequest, gin.H{"error": field + " is required"})
		return false
	}
	return true
}

func readAllLimit(r io.Reader, n int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, n+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > n {
		return nil, fmt.Errorf("larger than %d MB", n>>20)
	}
	return b, nil
}
