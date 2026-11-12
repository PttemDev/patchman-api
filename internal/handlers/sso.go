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
	"time"

	"patchman-be/internal/authtoken"
	"patchman-be/internal/config"
	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// SSOHandler implements third-party sign-in. Designed to plug additional
// providers (Google, etc.) without duplicating the OAuth dance: each
// provider implements the small `ssoProvider` interface below; the rest
// of the flow (state, code exchange shape, find-or-create) is shared.
//
// Crucial difference from the GitLab *integration* feature: integration
// stores per-user tokens for ongoing API calls (push/pull). SSO uses the
// OAuth provider only to verify identity at login time and discards the
// token immediately.
type SSOHandler struct {
	db        *gorm.DB
	cfg       *config.Config
	jwtSecret string
	http      *http.Client
}

// ssoPendingState is the in-process view of an OAuth round-trip. The
// row lives in models.OAuthPendingState — this struct only exists for
// callers that don't want to touch GORM directly.
type ssoPendingState struct {
	Provider     string
	CodeVerifier string
	RedirectURI  string
	CreatedAt    time.Time
}

func NewSSOHandler(db *gorm.DB, cfg *config.Config, jwtSecret string) *SSOHandler {
	return &SSOHandler{
		db:        db,
		cfg:       cfg,
		jwtSecret: jwtSecret,
		http:      &http.Client{Timeout: 30 * time.Second},
	}
}

// ----------------------------------------------------------------------
// Provider abstraction — narrow on purpose. New providers add a struct
// with these three methods + a registration block in resolveProvider().
// ----------------------------------------------------------------------

type ssoProvider interface {
	authURL(redirect, state, codeChallenge string) string
	exchange(h *SSOHandler, code, verifier, redirect string) (*ssoToken, error)
	fetchUser(h *SSOHandler, accessToken string) (*ssoUserInfo, error)
}

type ssoToken struct {
	AccessToken string `json:"access_token"`
}

type ssoUserInfo struct {
	Email     string
	Name      string
	AvatarURL string
}

func (h *SSOHandler) resolveProvider(name string) (ssoProvider, error) {
	switch name {
	case "gitlab":
		if !h.cfg.GitLabEnabled() {
			return nil, errors.New("GitLab SSO is not configured on the server")
		}
		return &gitlabSSO{cfg: h.cfg}, nil
	default:
		return nil, fmt.Errorf("unsupported sso provider %q", name)
	}
}

// ----------------------------------------------------------------------
// GitLab provider implementation
// ----------------------------------------------------------------------

type gitlabSSO struct {
	cfg *config.Config
}

func (g *gitlabSSO) authURL(redirect, state, challenge string) string {
	q := url.Values{}
	q.Set("client_id", g.cfg.GitLabClientID)
	q.Set("redirect_uri", redirect)
	q.Set("response_type", "code")
	q.Set("state", state)
	// Sign-in needs identity only — `read_user` is enough. We deliberately
	// do NOT request `api` scope here: the integration feature has its
	// own flow for that, and it's preferable to keep sign-in low-trust.
	q.Set("scope", "read_user")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	return strings.TrimRight(g.cfg.GitLabBaseURL, "/") + "/oauth/authorize?" + q.Encode()
}

func (g *gitlabSSO) exchange(h *SSOHandler, code, verifier, redirect string) (*ssoToken, error) {
	form := url.Values{}
	form.Set("client_id", g.cfg.GitLabClientID)
	form.Set("client_secret", g.cfg.GitLabClientSecret)
	form.Set("code", code)
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", redirect)
	form.Set("code_verifier", verifier)
	endpoint := strings.TrimRight(g.cfg.GitLabBaseURL, "/") + "/oauth/token"
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
	var tok ssoToken
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, err
	}
	if tok.AccessToken == "" {
		return nil, errors.New("gitlab returned empty access_token")
	}
	return &tok, nil
}

func (g *gitlabSSO) fetchUser(h *SSOHandler, access string) (*ssoUserInfo, error) {
	endpoint := strings.TrimRight(g.cfg.GitLabBaseURL, "/") + "/api/v4/user"
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := h.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("gitlab /user %d: %s", resp.StatusCode, string(body))
	}
	var u struct {
		Email     string `json:"email"`
		Name      string `json:"name"`
		AvatarURL string `json:"avatar_url"`
	}
	if err := json.Unmarshal(body, &u); err != nil {
		return nil, err
	}
	if u.Email == "" {
		return nil, errors.New("gitlab profile is missing an email — make sure the email is verified and visible")
	}
	return &ssoUserInfo{Email: u.Email, Name: u.Name, AvatarURL: u.AvatarURL}, nil
}

// ----------------------------------------------------------------------
// HTTP endpoints
// ----------------------------------------------------------------------

type ssoStartInput struct {
	Provider string `json:"provider" binding:"required"`
	Surface  string `json:"surface"` // "web" | "desktop"
	Origin   string `json:"origin"`  // override origin when surface=web
}

type ssoStartOutput struct {
	AuthURL string `json:"authUrl"`
	State   string `json:"state"`
}

func (h *SSOHandler) Start(c *gin.Context) {
	var in ssoStartInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	provider, err := h.resolveProvider(in.Provider)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	origin := in.Origin
	if origin == "" {
		origin = c.GetHeader("Origin")
	}
	redirect, err := h.redirectURIFor(in.Surface, origin)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	state, err := ssoRandomToken(24)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "rng failure"})
		return
	}
	verifier, err := ssoRandomToken(48)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "rng failure"})
		return
	}
	challenge := ssoPKCEChallenge(verifier)

	h.rememberPending(state, &ssoPendingState{
		Provider:     in.Provider,
		CodeVerifier: verifier,
		RedirectURI:  redirect,
		CreatedAt:    time.Now(),
	})

	c.JSON(http.StatusOK, ssoStartOutput{
		AuthURL: provider.authURL(redirect, state, challenge),
		State:   state,
	})
}

type ssoCallbackInput struct {
	Code  string `json:"code" binding:"required"`
	State string `json:"state" binding:"required"`
}

type ssoCallbackOutput struct {
	Token string      `json:"token"`
	User  models.User `json:"user"`
	// New is true when the SSO callback freshly created the PatchMan
	// account — the FE shows a "welcome" toast.
	New bool `json:"new"`
	// Merged is true when an existing PatchMan account at this email
	// was linked to an SSO provider for the FIRST time. The FE shows a
	// one-time notice so the user understands which account they're
	// signed into and that their old password still works.
	Merged bool `json:"merged"`
}

// Callback exchanges the OAuth code, finds-or-creates the matching
// PatchMan user (matched by lowercase email), and returns a JWT.
//
// "Find-or-create" preserves any pre-existing PatchMan account at that
// email — so a user who registered with email+password in the past can
// later sign in with GitLab and the same row stays in place. No
// "merge" UI needed; identity is the email.
func (h *SSOHandler) Callback(c *gin.Context) {
	var in ssoCallbackInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	pending := h.takePending(in.State)
	if pending == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown or expired state — restart sign-in"})
		return
	}
	provider, err := h.resolveProvider(pending.Provider)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	tok, err := provider.exchange(h, in.Code, pending.CodeVerifier, pending.RedirectURI)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("token exchange: %v", err)})
		return
	}
	info, err := provider.fetchUser(h, tok.AccessToken)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("fetch profile: %v", err)})
		return
	}

	email := strings.ToLower(strings.TrimSpace(info.Email))
	if !h.cfg.EmailAllowed(email) {
		c.JSON(http.StatusForbidden, gin.H{"error": emailDomainError(h.cfg)})
		return
	}

	var user models.User
	created := false
	merged := false
	if err := h.db.Where("LOWER(email) = ?", email).First(&user).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
			return
		}
		// Create a fresh account. The local password is never used (SSO
		// is the only sign-in path that minted it) — we store a bcrypt
		// hash of a random secret so password reset can later replace
		// it without leaving the field nullable.
		randomPw := make([]byte, 24)
		_, _ = rand.Read(randomPw)
		hash, _ := bcrypt.GenerateFromPassword(randomPw, bcrypt.DefaultCost)
		now := time.Now()
		user = models.User{
			ID:          uuid.New().String(),
			Email:       email,
			Name:        orFallback(info.Name, email),
			Password:    string(hash),
			Role:        models.RoleUser,
			SsoProvider: pending.Provider,
			SsoLinkedAt: &now,
		}
		if h.cfg.IsDefaultAdmin(email) {
			user.Role = models.RoleAdmin
		}
		if err := h.db.Create(&user).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create account"})
			return
		}
		created = true
	} else if user.SsoLinkedAt == nil {
		// First time this existing-by-email account is being linked to an
		// SSO provider. Stamp the link and tell the FE to surface the
		// "we found an existing account, here's what we did" notice.
		now := time.Now()
		h.db.Model(&user).Updates(map[string]any{
			"sso_provider":  pending.Provider,
			"sso_linked_at": &now,
		})
		user.SsoProvider = pending.Provider
		user.SsoLinkedAt = &now
		merged = true
	}

	jwtToken, err := h.generateToken(user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "token issue failed"})
		return
	}

	// Stamp last-login (+ last-seen/version) so SSO sign-ins count on the
	// admin dashboard just like password logins.
	loginAt := time.Now()
	loginUpdates := map[string]any{"last_login_at": loginAt, "last_seen_at": loginAt}
	if v := clientVersion(c); v != "" {
		loginUpdates["last_seen_version"] = v
	}
	h.db.Model(&models.User{}).Where("id = ?", user.ID).Updates(loginUpdates)
	user.LastLoginAt = &loginAt
	user.LastSeenAt = &loginAt

	c.JSON(http.StatusOK, gin.H{"data": ssoCallbackOutput{Token: jwtToken, User: user, New: created, Merged: merged}})
}

// ----------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------

// redirectURIFor returns the registered callback for the requested
// surface. Reuses the GitLab integration's `/auth/gitlab/callback`
// path so we don't have to register a separate set of URIs on the
// GitLab OAuth Application. The FE callback page disambiguates SSO vs.
// integration by checking sessionStorage for the SSO state token —
// only the SSO flow stashes one, so its presence is the signal.
func (h *SSOHandler) redirectURIFor(surface, origin string) (string, error) {
	switch surface {
	case "desktop":
		return "patchman://auth/gitlab/callback", nil
	case "web", "":
		if !originAllowed(h.cfg, origin) {
			return "", fmt.Errorf("origin %q is not an allowed sign-in callback host", origin)
		}
		return origin + "/auth/gitlab/callback", nil
	default:
		return "", fmt.Errorf("unknown surface %q", surface)
	}
}

func (h *SSOHandler) rememberPending(state string, p *ssoPendingState) {
	// Sweep stale rows opportunistically — keeps the table from
	// growing unbounded if a lot of users abandon the consent screen.
	cutoff := time.Now().Add(-10 * time.Minute)
	h.db.Where("kind = ? AND created_at < ?", models.OAuthKindSSO, cutoff).
		Delete(&models.OAuthPendingState{})
	h.db.Create(&models.OAuthPendingState{
		State:        state,
		Kind:         models.OAuthKindSSO,
		Provider:     p.Provider,
		CodeVerifier: p.CodeVerifier,
		RedirectURI:  p.RedirectURI,
		CreatedAt:    p.CreatedAt,
	})
}

func (h *SSOHandler) takePending(state string) *ssoPendingState {
	var row models.OAuthPendingState
	if err := h.db.Where("state = ? AND kind = ?", state, models.OAuthKindSSO).First(&row).Error; err != nil {
		return nil
	}
	// One-shot — delete immediately so a leaked or replayed callback
	// can't reuse a previously-completed exchange.
	h.db.Where("state = ? AND kind = ?", state, models.OAuthKindSSO).Delete(&models.OAuthPendingState{})
	if time.Since(row.CreatedAt) > 10*time.Minute {
		return nil
	}
	return &ssoPendingState{
		Provider:     row.Provider,
		CodeVerifier: row.CodeVerifier,
		RedirectURI:  row.RedirectURI,
		CreatedAt:    row.CreatedAt,
	}
}

func (h *SSOHandler) generateToken(userID string) (string, error) {
	return authtoken.Mint(h.jwtSecret, userID, h.cfg.SessionTTL)
}

func ssoRandomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func ssoPKCEChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func orFallback(s, fallback string) string {
	if strings.TrimSpace(s) != "" {
		return s
	}
	return fallback
}
