package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"patchman-be/internal/config"
	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// GitIntegrationHandler manages OAuth account-linking against GitLab and the
// typed GitLab API surface (file get/commit, branches, MRs). Tokens are
// stored encrypted in user_git_integrations and refreshed transparently.
type GitIntegrationHandler struct {
	db     *gorm.DB
	cfg    *config.Config
	cipher *services.TokenCipher
	http   *http.Client
}

type pendingOAuth struct {
	UserID       string
	CodeVerifier string
	RedirectURI  string
	CreatedAt    time.Time
}

// NewGitIntegrationHandler returns nil + error when GITLAB_TOKEN_ENC_KEY is
// missing or invalid. Callers should treat this as a config error and either
// crash or skip route registration with a clear log line.
func NewGitIntegrationHandler(db *gorm.DB, cfg *config.Config) (*GitIntegrationHandler, error) {
	if !cfg.GitLabEnabled() {
		return nil, errors.New("gitlab integration disabled: missing GITLAB_OAUTH_CLIENT_ID/SECRET or GITLAB_TOKEN_ENC_KEY")
	}
	cipher, err := services.NewTokenCipher(cfg.GitLabTokenEncKey)
	if err != nil {
		return nil, fmt.Errorf("init token cipher: %w", err)
	}
	return &GitIntegrationHandler{
		db:     db,
		cfg:    cfg,
		cipher: cipher,
		http:   &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// ------------------------------------------------------------------
// OAuth start / callback
// ------------------------------------------------------------------

type oauthStartInput struct {
	// "web" | "desktop" — chooses the registered redirect URI. Desktop uses
	// the Tauri deep-link scheme; web uses the FE callback page.
	Surface string `json:"surface"`
}

type oauthStartOutput struct {
	AuthURL string `json:"authUrl"`
	State   string `json:"state"`
}

func (h *GitIntegrationHandler) Start(c *gin.Context) {
	userID := c.GetString("userID")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "auth required"})
		return
	}

	var in oauthStartInput
	_ = c.ShouldBindJSON(&in) // empty body is fine, defaults to web

	redirect, err := h.redirectURIFor(in.Surface, c.GetHeader("Origin"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	state, err := randomURLToken(24)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "rng failure"})
		return
	}
	verifier, err := randomURLToken(48)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "rng failure"})
		return
	}
	challenge := pkceChallenge(verifier)

	h.rememberPending(state, &pendingOAuth{
		UserID:       userID,
		CodeVerifier: verifier,
		RedirectURI:  redirect,
		CreatedAt:    time.Now(),
	})

	q := url.Values{}
	q.Set("client_id", h.cfg.GitLabClientID)
	q.Set("redirect_uri", redirect)
	q.Set("response_type", "code")
	q.Set("state", state)
	q.Set("scope", "api read_user")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")

	c.JSON(http.StatusOK, oauthStartOutput{
		AuthURL: strings.TrimRight(h.cfg.GitLabBaseURL, "/") + "/oauth/authorize?" + q.Encode(),
		State:   state,
	})
}

type oauthCallbackInput struct {
	Code  string `json:"code" binding:"required"`
	State string `json:"state" binding:"required"`
}

func (h *GitIntegrationHandler) Callback(c *gin.Context) {
	userID := c.GetString("userID")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "auth required"})
		return
	}

	var in oauthCallbackInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	pending := h.takePending(in.State)
	if pending == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown or expired state — restart the OAuth flow"})
		return
	}
	if pending.UserID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "state belongs to a different user"})
		return
	}

	tok, err := h.exchangeCode(in.Code, pending.CodeVerifier, pending.RedirectURI)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("token exchange failed: %v", err)})
		return
	}

	gitlabUser, err := h.fetchGitLabUser(tok.AccessToken)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("fetch gitlab user: %v", err)})
		return
	}

	accessEnc, err := h.cipher.Encrypt(tok.AccessToken)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt access token"})
		return
	}
	refreshEnc, err := h.cipher.Encrypt(tok.RefreshToken)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt refresh token"})
		return
	}

	now := time.Now()
	row := models.UserGitIntegration{
		UserID:          userID,
		Provider:        "gitlab",
		GitLabUserID:    gitlabUser.ID,
		GitLabUsername:  gitlabUser.Username,
		GitLabName:      gitlabUser.Name,
		GitLabAvatar:    gitlabUser.AvatarURL,
		AccessTokenEnc:  accessEnc,
		RefreshTokenEnc: refreshEnc,
		ExpiresAt:       tok.expiresAt(),
		Scopes:          tok.Scope,
		LinkedAt:        now,
		LastRefreshedAt: now,
	}
	// Upsert — re-linking overwrites prior row for the same (user, provider).
	if err := h.db.Save(&row).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "persist integration"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"gitlabUserId":   row.GitLabUserID,
		"gitlabUsername": row.GitLabUsername,
		"gitlabName":     row.GitLabName,
		"gitlabAvatar":   row.GitLabAvatar,
		"linkedAt":       row.LinkedAt,
		"scopes":         row.Scopes,
	}})
}

// Status returns whether the user has a linked GitLab account, and basic
// identity. Tokens are never returned. Returns 200 + null payload when not
// linked, so the FE can branch on the body without needing 404 handling.
func (h *GitIntegrationHandler) Status(c *gin.Context) {
	userID := c.GetString("userID")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "auth required"})
		return
	}
	var row models.UserGitIntegration
	err := h.db.Where("user_id = ? AND provider = ?", userID, "gitlab").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusOK, gin.H{"data": nil})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"gitlabUserId":   row.GitLabUserID,
		"gitlabUsername": row.GitLabUsername,
		"gitlabName":     row.GitLabName,
		"gitlabAvatar":   row.GitLabAvatar,
		"linkedAt":       row.LinkedAt,
		"scopes":         row.Scopes,
	}})
}

// Token mints a fresh, short-lived GitLab access token for the caller (with a
// transparent refresh if the stored one is near expiry). The Git tool's local
// clone/push flows run git on the user's own machine and need a token to hand
// to GIT_ASKPASS — the FE fetches it here and passes it into the clone/push
// request bodies (never stored at rest, never written into .git/config).
// GET /integrations/gitlab/token -> {token, expiresAt}
func (h *GitIntegrationHandler) Token(c *gin.Context) {
	userID := c.GetString("userID")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "auth required"})
		return
	}
	tok, err := h.validAccessToken(userID)
	if err != nil {
		c.JSON(http.StatusPreconditionFailed, gin.H{"error": "Link your GitLab account in Settings to browse repositories"})
		return
	}
	// Re-read for the authoritative expiry (validAccessToken persists refreshes).
	var row models.UserGitIntegration
	_ = h.db.Where("user_id = ? AND provider = ?", userID, "gitlab").First(&row).Error
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"token":     tok,
		"expiresAt": row.ExpiresAt,
	}})
}

// Disconnect revokes the token at GitLab (best-effort) and deletes the row.
func (h *GitIntegrationHandler) Disconnect(c *gin.Context) {
	userID := c.GetString("userID")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "auth required"})
		return
	}
	var row models.UserGitIntegration
	err := h.db.Where("user_id = ? AND provider = ?", userID, "gitlab").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if access, decErr := h.cipher.Decrypt(row.AccessTokenEnc); decErr == nil {
		_ = h.revokeToken(access) // best effort; ignore failures
	}
	if err := h.db.Delete(&row).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete integration"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

// ------------------------------------------------------------------
// Helpers — redirect URIs, token exchange, user fetch, refresh
// ------------------------------------------------------------------

// redirectURIFor returns the URI we hand to GitLab. Must exactly match one of
// the registered URIs on the OAuth Application, so we keep an explicit list.
// Web flow uses the caller's Origin to support test/prod/local-dev FE hosts;
// desktop flow always uses the Tauri deep-link scheme.
func (h *GitIntegrationHandler) redirectURIFor(surface, origin string) (string, error) {
	switch surface {
	case "desktop":
		return "patchman://auth/gitlab/callback", nil
	case "web", "":
		// Trust the caller's origin — must be one we know about. Avoids
		// hardcoding the FE host on the BE.
		if !originAllowed(h.cfg, origin) {
			return "", fmt.Errorf("origin %q is not an allowed web callback host", origin)
		}
		return origin + "/auth/gitlab/callback", nil
	default:
		return "", fmt.Errorf("unknown surface %q (want web|desktop)", surface)
	}
}

func (h *GitIntegrationHandler) rememberPending(state string, p *pendingOAuth) {
	// Opportunistic GC of stale entries (>10 min). Keeps the table
	// bounded without a separate goroutine. Persisting to DB rather
	// than process memory means a BE restart between Start and
	// Callback no longer wipes the in-flight state and surfaces
	// "unknown or expired state" to the user mid-consent.
	cutoff := time.Now().Add(-10 * time.Minute)
	h.db.Where("kind = ? AND created_at < ?", models.OAuthKindGitIntegration, cutoff).
		Delete(&models.OAuthPendingState{})
	h.db.Create(&models.OAuthPendingState{
		State:        state,
		Kind:         models.OAuthKindGitIntegration,
		UserID:       p.UserID,
		CodeVerifier: p.CodeVerifier,
		RedirectURI:  p.RedirectURI,
		CreatedAt:    p.CreatedAt,
	})
}

func (h *GitIntegrationHandler) takePending(state string) *pendingOAuth {
	var row models.OAuthPendingState
	if err := h.db.Where("state = ? AND kind = ?", state, models.OAuthKindGitIntegration).First(&row).Error; err != nil {
		return nil
	}
	// One-shot — delete immediately.
	h.db.Where("state = ? AND kind = ?", state, models.OAuthKindGitIntegration).Delete(&models.OAuthPendingState{})
	if time.Since(row.CreatedAt) > 10*time.Minute {
		return nil
	}
	return &pendingOAuth{
		UserID:       row.UserID,
		CodeVerifier: row.CodeVerifier,
		RedirectURI:  row.RedirectURI,
		CreatedAt:    row.CreatedAt,
	}
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	CreatedAt    int64  `json:"created_at"`
}

func (t tokenResponse) expiresAt() time.Time {
	if t.CreatedAt > 0 {
		return time.Unix(t.CreatedAt+t.ExpiresIn, 0)
	}
	return time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
}

func (h *GitIntegrationHandler) exchangeCode(code, verifier, redirect string) (*tokenResponse, error) {
	form := url.Values{}
	form.Set("client_id", h.cfg.GitLabClientID)
	form.Set("client_secret", h.cfg.GitLabClientSecret)
	form.Set("code", code)
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", redirect)
	form.Set("code_verifier", verifier)
	return h.postToken(form)
}

func (h *GitIntegrationHandler) refreshAccessToken(refresh string) (*tokenResponse, error) {
	form := url.Values{}
	form.Set("client_id", h.cfg.GitLabClientID)
	form.Set("client_secret", h.cfg.GitLabClientSecret)
	form.Set("refresh_token", refresh)
	form.Set("grant_type", "refresh_token")
	return h.postToken(form)
}

func (h *GitIntegrationHandler) postToken(form url.Values) (*tokenResponse, error) {
	endpoint := strings.TrimRight(h.cfg.GitLabBaseURL, "/") + "/oauth/token"
	req, err := http.NewRequest("POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := h.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("gitlab oauth %d: %s", resp.StatusCode, string(body))
	}
	var tok tokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, errors.New("gitlab returned empty access_token")
	}
	return &tok, nil
}

func (h *GitIntegrationHandler) revokeToken(access string) error {
	form := url.Values{}
	form.Set("client_id", h.cfg.GitLabClientID)
	form.Set("client_secret", h.cfg.GitLabClientSecret)
	form.Set("token", access)
	endpoint := strings.TrimRight(h.cfg.GitLabBaseURL, "/") + "/oauth/revoke"
	req, err := http.NewRequest("POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := h.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("revoke %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

type gitlabUser struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
}

func (h *GitIntegrationHandler) fetchGitLabUser(access string) (*gitlabUser, error) {
	endpoint := strings.TrimRight(h.cfg.GitLabBaseURL, "/") + "/api/v4/user"
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Accept", "application/json")
	resp, err := h.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("gitlab /user %d: %s", resp.StatusCode, string(body))
	}
	var u gitlabUser
	if err := json.Unmarshal(body, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// tokenRefreshWindow: an access token this close to expiry is refreshed first.
const tokenRefreshWindow = 60 * time.Second

// refreshLocks holds one mutex per user so this process refreshes a user's
// GitLab token once at a time. GitLab refresh tokens are single-use: the Git
// panel fires repos / branches / commits / pipelines together, and when the
// token was due each request used to refresh with the same refresh token —
// one won, the rest got invalid_grant and answered "link your GitLab
// account", so e.g. the branch list randomly stayed empty.
var refreshLocks sync.Map // userID → *sync.Mutex

func userRefreshLock(userID string) *sync.Mutex {
	m, _ := refreshLocks.LoadOrStore(userID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

func (h *GitIntegrationHandler) loadIntegration(userID string) (*models.UserGitIntegration, error) {
	var row models.UserGitIntegration
	if err := h.db.Where("user_id = ? AND provider = ?", userID, "gitlab").First(&row).Error; err != nil {
		return nil, err
	}
	return &row, nil
}

// validAccessToken returns a usable access token for the user, transparently
// refreshing if it's about to expire. Used by all typed GitLab endpoints.
func (h *GitIntegrationHandler) validAccessToken(userID string) (string, error) {
	row, err := h.loadIntegration(userID)
	if err != nil {
		return "", fmt.Errorf("not linked: %w", err)
	}
	if time.Until(row.ExpiresAt) > tokenRefreshWindow {
		return h.cipher.Decrypt(row.AccessTokenEnc)
	}
	mu := userRefreshLock(userID)
	mu.Lock()
	defer mu.Unlock()
	return h.refreshStoredToken(userID)
}

// refreshStoredToken refreshes the user's token unless someone already did.
// The caller holds the user's refresh lock; another pod of the hosted backend
// can still race us, which awaitPeerRefresh covers.
func (h *GitIntegrationHandler) refreshStoredToken(userID string) (string, error) {
	// Re-read: a request we waited behind has usually refreshed already.
	row, err := h.loadIntegration(userID)
	if err != nil {
		return "", fmt.Errorf("not linked: %w", err)
	}
	if time.Until(row.ExpiresAt) > tokenRefreshWindow {
		return h.cipher.Decrypt(row.AccessTokenEnc)
	}
	refresh, err := h.cipher.Decrypt(row.RefreshTokenEnc)
	if err != nil {
		return "", fmt.Errorf("decrypt refresh token: %w", err)
	}
	tok, err := h.refreshAccessToken(refresh)
	if err != nil {
		// Another pod may have spent the same single-use refresh token first;
		// its new token lands in the row shortly.
		if access, ok := h.awaitPeerRefresh(userID, row.AccessTokenEnc); ok {
			return access, nil
		}
		// GitLab unreachable etc.: a token that hasn't expired yet still works.
		if time.Until(row.ExpiresAt) > 0 {
			return h.cipher.Decrypt(row.AccessTokenEnc)
		}
		return "", fmt.Errorf("refresh: %w", err)
	}
	accessEnc, err := h.cipher.Encrypt(tok.AccessToken)
	if err != nil {
		return "", err
	}
	refreshEnc := row.RefreshTokenEnc
	if tok.RefreshToken != "" {
		refreshEnc, err = h.cipher.Encrypt(tok.RefreshToken)
		if err != nil {
			return "", err
		}
	}
	row.AccessTokenEnc = accessEnc
	row.RefreshTokenEnc = refreshEnc
	row.ExpiresAt = tok.expiresAt()
	row.LastRefreshedAt = time.Now()
	if err := h.db.Save(row).Error; err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

// peerRefreshWait bounds how long a request that lost the refresh race waits
// for the winner (another pod) to store the new token.
var peerRefreshWait = 2 * time.Second

// awaitPeerRefresh polls the user's row until its access token differs from
// staleAccessEnc (every successful refresh re-encrypts it) and returns the
// new token.
func (h *GitIntegrationHandler) awaitPeerRefresh(userID, staleAccessEnc string) (string, bool) {
	deadline := time.Now().Add(peerRefreshWait)
	for {
		if row, err := h.loadIntegration(userID); err == nil && row.AccessTokenEnc != staleAccessEnc {
			access, derr := h.cipher.Decrypt(row.AccessTokenEnc)
			return access, derr == nil
		}
		if time.Now().After(deadline) {
			return "", false
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// ------------------------------------------------------------------
// Crypto/random helpers
// ------------------------------------------------------------------

func randomURLToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func pkceChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}
