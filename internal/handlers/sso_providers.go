package handlers

// GitHub and Google sign-in providers, plus the public list of sign-in
// methods the login page shows. A provider whose keys aren't set is neither
// listed nor accepted (resolveProvider refuses it).
//
// Identity rule for every provider: only an email the provider has VERIFIED
// may sign in — GitHub's verified primary email, Google's email_verified.
// The account is then matched by email (Callback), so the same person lands
// on the same PatchMan account whichever provider they use.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"patchman-be/internal/config"

	"github.com/gin-gonic/gin"
)

// ssoDeniedError is a refusal about the person (unverified email, wrong
// Workspace domain), answered with 403 rather than a gateway error.
type ssoDeniedError struct{ msg string }

func (e *ssoDeniedError) Error() string { return e.msg }

func denied(format string, a ...any) error { return &ssoDeniedError{msg: fmt.Sprintf(format, a...)} }

// ── sign-in methods ───────────────────────────────────────────────────

type authMethod struct {
	ID string `json:"id"` // "password" | "gitlab" | "github" | "google"
	// Domains (Google): the Workspace domains sign-in is limited to.
	Domains []string `json:"domains,omitempty"`
}

// Methods — GET /api/auth/methods (public). The sign-in methods this
// server accepts; the login page shows a button for each.
func (h *SSOHandler) Methods(c *gin.Context) {
	out := []authMethod{{ID: "password"}}
	if h.cfg.GitLabEnabled() {
		out = append(out, authMethod{ID: "gitlab"})
	}
	if h.cfg.GitHubSSOEnabled() {
		out = append(out, authMethod{ID: "github"})
	}
	if h.cfg.GoogleSSOEnabled() {
		out = append(out, authMethod{ID: "google", Domains: h.cfg.GoogleWorkspaceDomains})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// ── shared HTTP helpers ───────────────────────────────────────────────

func (h *SSOHandler) postForm(endpoint string, form url.Values, who string) ([]byte, error) {
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
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s token %d", who, resp.StatusCode)
	}
	return body, nil
}

func (h *SSOHandler) getJSON(endpoint, access string, out any, who string) error {
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Accept", "application/json")
	resp, err := h.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %d", who, resp.StatusCode)
	}
	return json.Unmarshal(body, out)
}

// tokenFrom decodes an OAuth token response. GitHub answers some failures
// with 200 + {"error": …}, so that is checked too.
func tokenFrom(body []byte, who string) (*ssoToken, error) {
	var t struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, err
	}
	if t.Error != "" {
		return nil, fmt.Errorf("%s: %s %s", who, t.Error, t.Description)
	}
	if t.AccessToken == "" {
		return nil, errors.New(who + " returned no access_token")
	}
	return &ssoToken{AccessToken: t.AccessToken}, nil
}

// ── GitHub ────────────────────────────────────────────────────────────

type githubSSO struct {
	cfg     *config.Config
	webBase string // https://github.com or the Enterprise root
	apiBase string // https://api.github.com or <root>/api/v3
}

func newGitHubSSO(cfg *config.Config) *githubSSO {
	web := strings.TrimRight(cfg.GitHubBaseURL, "/")
	if web == "" {
		web = "https://github.com"
	}
	api := "https://api.github.com"
	if u, err := url.Parse(web); err == nil && !strings.EqualFold(u.Host, "github.com") {
		api = web + "/api/v3"
	}
	return &githubSSO{cfg: cfg, webBase: web, apiBase: api}
}

func (g *githubSSO) authURL(redirect, state, challenge string) string {
	q := url.Values{}
	q.Set("client_id", g.cfg.GitHubClientID)
	q.Set("redirect_uri", redirect)
	q.Set("state", state)
	// Identity only: profile + email addresses. No repo access.
	q.Set("scope", "read:user user:email")
	q.Set("allow_signup", "false")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	return g.webBase + "/login/oauth/authorize?" + q.Encode()
}

func (g *githubSSO) exchange(h *SSOHandler, code, verifier, redirect string) (*ssoToken, error) {
	form := url.Values{}
	form.Set("client_id", g.cfg.GitHubClientID)
	form.Set("client_secret", g.cfg.GitHubClientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", redirect)
	form.Set("code_verifier", verifier)
	body, err := h.postForm(g.webBase+"/login/oauth/access_token", form, "github")
	if err != nil {
		return nil, err
	}
	return tokenFrom(body, "github")
}

func (g *githubSSO) fetchUser(h *SSOHandler, access string) (*ssoUserInfo, error) {
	var u struct {
		Login     string `json:"login"`
		Name      string `json:"name"`
		AvatarURL string `json:"avatar_url"`
	}
	if err := h.getJSON(g.apiBase+"/user", access, &u, "github /user"); err != nil {
		return nil, err
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := h.getJSON(g.apiBase+"/user/emails", access, &emails, "github /user/emails"); err != nil {
		return nil, err
	}
	email := ""
	for _, e := range emails {
		if e.Primary && e.Verified {
			email = e.Email
			break
		}
	}
	if email == "" {
		return nil, denied("your GitHub account has no verified primary email — verify it on GitHub, then try again")
	}
	return &ssoUserInfo{Email: email, Name: orFallback(u.Name, u.Login), AvatarURL: u.AvatarURL}, nil
}

// ── Google ────────────────────────────────────────────────────────────

type googleSSO struct {
	cfg         *config.Config
	authBase    string
	tokenURL    string
	userinfoURL string
}

func newGoogleSSO(cfg *config.Config) *googleSSO {
	return &googleSSO{
		cfg:         cfg,
		authBase:    "https://accounts.google.com/o/oauth2/v2/auth",
		tokenURL:    "https://oauth2.googleapis.com/token",
		userinfoURL: "https://openidconnect.googleapis.com/v1/userinfo",
	}
}

func (g *googleSSO) authURL(redirect, state, challenge string) string {
	q := url.Values{}
	q.Set("client_id", g.cfg.GoogleClientID)
	q.Set("redirect_uri", redirect)
	q.Set("response_type", "code")
	q.Set("scope", "openid email profile")
	q.Set("state", state)
	q.Set("prompt", "select_account")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	// A hint for Google's account picker only; the domain is enforced in
	// fetchUser against the hd claim.
	if d := g.cfg.GoogleWorkspaceDomains; len(d) == 1 {
		q.Set("hd", d[0])
	}
	return g.authBase + "?" + q.Encode()
}

func (g *googleSSO) exchange(h *SSOHandler, code, verifier, redirect string) (*ssoToken, error) {
	form := url.Values{}
	form.Set("client_id", g.cfg.GoogleClientID)
	form.Set("client_secret", g.cfg.GoogleClientSecret)
	form.Set("code", code)
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", redirect)
	form.Set("code_verifier", verifier)
	body, err := h.postForm(g.tokenURL, form, "google")
	if err != nil {
		return nil, err
	}
	return tokenFrom(body, "google")
}

func (g *googleSSO) fetchUser(h *SSOHandler, access string) (*ssoUserInfo, error) {
	var u struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
		Picture       string `json:"picture"`
		HD            string `json:"hd"`
	}
	if err := h.getJSON(g.userinfoURL, access, &u, "google userinfo"); err != nil {
		return nil, err
	}
	if u.Email == "" || !u.EmailVerified {
		return nil, denied("your Google account's email is not verified")
	}
	if allowed := g.cfg.GoogleWorkspaceDomains; len(allowed) > 0 {
		ok := false
		for _, d := range allowed {
			if strings.EqualFold(u.HD, d) {
				ok = true
				break
			}
		}
		if !ok {
			return nil, denied("sign in with a Google Workspace account of %s", strings.Join(allowed, ", "))
		}
	}
	return &ssoUserInfo{Email: u.Email, Name: u.Name, AvatarURL: u.Picture}, nil
}
