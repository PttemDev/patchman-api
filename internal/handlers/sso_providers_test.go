package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"patchman-be/internal/config"
	"patchman-be/internal/models"
)

// fakeIdP plays GitHub and Google: token endpoints answer a fixed token, and
// the profile endpoints answer what the test sets.
type fakeIdP struct {
	srv          *httptest.Server
	githubEmails []map[string]any
	googleUser   map[string]any
	tokenForms   []url.Values
}

func newFakeIdP(t *testing.T) *fakeIdP {
	f := &fakeIdP{}
	mux := http.NewServeMux()
	tok := func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.tokenForms = append(f.tokenForms, r.PostForm)
		if r.PostForm.Get("code") == "bad" {
			_, _ = w.Write([]byte(`{"error":"bad_verification_code","error_description":"The code passed is incorrect or expired."}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"tok-1","token_type":"bearer"}`))
	}
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer tok-1" {
				w.WriteHeader(401)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("/login/oauth/access_token", tok)
	mux.HandleFunc("/token", tok)
	mux.HandleFunc("/user", auth(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"login":"octo","name":"","avatar_url":"https://a/x.png"}`))
	}))
	mux.HandleFunc("/user/emails", auth(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(f.githubEmails)
	}))
	mux.HandleFunc("/userinfo", auth(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(f.googleUser)
	}))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func ssoRig(t *testing.T, cfg *config.Config, idp *fakeIdP) (*gin.Engine, *gorm.DB) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.User{}, &models.OAuthPendingState{}); err != nil {
		t.Fatal(err)
	}
	h := NewSSOHandler(db, cfg, "secret")
	if idp != nil {
		h.newGitHub = func(c *config.Config) ssoProvider {
			return &githubSSO{cfg: c, webBase: idp.srv.URL, apiBase: idp.srv.URL}
		}
		h.newGoogle = func(c *config.Config) ssoProvider {
			return &googleSSO{cfg: c, authBase: idp.srv.URL + "/auth", tokenURL: idp.srv.URL + "/token", userinfoURL: idp.srv.URL + "/userinfo"}
		}
	}
	r := gin.New()
	r.GET("/auth/methods", h.Methods)
	r.POST("/auth/sso/start", h.Start)
	r.POST("/auth/sso/callback", h.Callback)
	return r, db
}

func doJSON(r *gin.Engine, method, path string, body any) (int, map[string]any) {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// signIn runs start → callback for a provider and returns the callback answer.
func signIn(t *testing.T, r *gin.Engine, provider, code string) (int, map[string]any, string) {
	t.Helper()
	code0, start := doJSON(r, "POST", "/auth/sso/start", gin.H{"provider": provider, "surface": "web", "origin": "http://localhost:3000"})
	if code0 != 200 {
		t.Fatalf("start %s: %d %v", provider, code0, start)
	}
	authURL, _ := start["authUrl"].(string)
	st, _ := start["state"].(string)
	c, out := doJSON(r, "POST", "/auth/sso/callback", gin.H{"code": code, "state": st})
	return c, out, authURL
}

func baseCfg() *config.Config {
	return &config.Config{GitHubBaseURL: "https://github.com"}
}

func TestAuthMethodsListOnlyConfigured(t *testing.T) {
	ids := func(cfg *config.Config) []string {
		r, _ := ssoRig(t, cfg, nil)
		_, out := doJSON(r, "GET", "/auth/methods", nil)
		var got []string
		for _, m := range out["data"].([]any) {
			got = append(got, m.(map[string]any)["id"].(string))
		}
		return got
	}
	if got := strings.Join(ids(baseCfg()), ","); got != "password" {
		t.Errorf("nothing configured: %s", got)
	}
	cfg := baseCfg()
	cfg.GitHubClientID, cfg.GitHubClientSecret = "id", "sec"
	cfg.GoogleClientID = "only-id" // no secret → not configured
	if got := strings.Join(ids(cfg), ","); got != "password,github" {
		t.Errorf("github only: %s", got)
	}
	cfg.GoogleClientSecret = "sec"
	cfg.GitLabClientID, cfg.GitLabClientSecret, cfg.GitLabTokenEncKey = "a", "b", "c"
	if got := strings.Join(ids(cfg), ","); got != "password,gitlab,github,google" {
		t.Errorf("all: %s", got)
	}

	// An unconfigured provider is refused by the server, not just hidden.
	r, _ := ssoRig(t, baseCfg(), nil)
	for _, p := range []string{"github", "google", "gitlab"} {
		if c, _ := doJSON(r, "POST", "/auth/sso/start", gin.H{"provider": p, "surface": "web", "origin": "http://localhost:3000"}); c != 400 {
			t.Errorf("start %s unconfigured: %d", p, c)
		}
	}
}

func TestGitHubSignIn(t *testing.T) {
	idp := newFakeIdP(t)
	cfg := baseCfg()
	cfg.GitHubClientID, cfg.GitHubClientSecret = "gh-id", "gh-sec"
	r, db := ssoRig(t, cfg, idp)

	// Unverified primary / only a verified secondary → refused (403).
	idp.githubEmails = []map[string]any{
		{"email": "me@corp.com", "primary": true, "verified": false},
		{"email": "other@corp.com", "primary": false, "verified": true},
	}
	if c, out, authURL := signIn(t, r, "github", "c1"); c != 403 || !strings.Contains(out["error"].(string), "verified") {
		t.Fatalf("unverified: %d %v", c, out)
	} else {
		u, _ := url.Parse(authURL)
		q := u.Query()
		if u.Path != "/login/oauth/authorize" || q.Get("scope") != "read:user user:email" || q.Get("redirect_uri") != "http://localhost:3000/auth/github/callback" || q.Get("code_challenge") == "" {
			t.Errorf("authorize URL: %s", authURL)
		}
	}
	var n int64
	db.Model(&models.User{}).Count(&n)
	if n != 0 {
		t.Fatal("refused sign-in must not create a user")
	}

	// Verified primary → account created, name falls back to the login.
	idp.githubEmails = []map[string]any{{"email": "Me@Corp.com", "primary": true, "verified": true}}
	c, out, _ := signIn(t, r, "github", "c2")
	if c != 200 || out["data"].(map[string]any)["new"] != true || out["data"].(map[string]any)["token"] == "" {
		t.Fatalf("verified: %d %v", c, out)
	}
	var u models.User
	db.First(&u)
	if u.Email != "me@corp.com" || u.Name != "octo" || u.SsoProvider != "github" {
		t.Errorf("user: %+v", u)
	}
	// The PKCE verifier and the provider-specific redirect reach the token call.
	last := idp.tokenForms[len(idp.tokenForms)-1]
	if last.Get("code_verifier") == "" || last.Get("redirect_uri") != "http://localhost:3000/auth/github/callback" || last.Get("client_secret") != "gh-sec" {
		t.Errorf("token form: %v", last)
	}

	// GitHub's 200 + {"error"} on a bad code is a failure, not a sign-in.
	if c, _, _ := signIn(t, r, "github", "bad"); c != http.StatusBadGateway {
		t.Errorf("bad code: %d", c)
	}

	// Same email again → the same account, not a new one.
	c, out, _ = signIn(t, r, "github", "c3")
	if c != 200 || out["data"].(map[string]any)["new"] != false {
		t.Fatalf("second sign-in: %d %v", c, out)
	}
	db.Model(&models.User{}).Count(&n)
	if n != 1 {
		t.Errorf("users = %d, want 1", n)
	}
}

func TestGoogleSignInWorkspaceAndVerified(t *testing.T) {
	idp := newFakeIdP(t)
	cfg := baseCfg()
	cfg.GoogleClientID, cfg.GoogleClientSecret = "g-id", "g-sec"
	cfg.GoogleWorkspaceDomains = []string{"corp.com"}
	r, db := ssoRig(t, cfg, idp)

	idp.googleUser = map[string]any{"email": "a@corp.com", "email_verified": false, "hd": "corp.com"}
	if c, _, authURL := signIn(t, r, "google", "c1"); c != 403 {
		t.Fatalf("unverified: %d", c)
	} else {
		q, _ := url.Parse(authURL)
		if q.Query().Get("hd") != "corp.com" || q.Query().Get("scope") != "openid email profile" || q.Query().Get("redirect_uri") != "http://localhost:3000/auth/google/callback" {
			t.Errorf("authorize URL: %s", authURL)
		}
	}
	// A personal Gmail (no hd) or another Workspace is refused server-side.
	for _, hd := range []string{"", "other.com"} {
		idp.googleUser = map[string]any{"email": "a@gmail.com", "email_verified": true, "hd": hd}
		if c, _, _ := signIn(t, r, "google", "c2"); c != 403 {
			t.Errorf("hd=%q must be refused: %d", hd, c)
		}
	}
	idp.googleUser = map[string]any{"email": "a@corp.com", "email_verified": true, "hd": "corp.com", "name": "Ayşe"}
	if c, out, _ := signIn(t, r, "google", "c3"); c != 200 {
		t.Fatalf("workspace user: %d %v", c, out)
	}
	var u models.User
	db.First(&u)
	if u.Email != "a@corp.com" || u.Name != "Ayşe" || u.SsoProvider != "google" {
		t.Errorf("user: %+v", u)
	}
}

// ALLOWED_EMAIL_DOMAINS still applies to the new providers, and an existing
// password account is reused (merged) by email.
func TestNewProvidersKeepDomainRuleAndMergeByEmail(t *testing.T) {
	idp := newFakeIdP(t)
	cfg := baseCfg()
	cfg.GitHubClientID, cfg.GitHubClientSecret = "gh-id", "gh-sec"
	cfg.AllowedEmailDomains = []string{"corp.com"}
	r, db := ssoRig(t, cfg, idp)

	idp.githubEmails = []map[string]any{{"email": "x@gmail.com", "primary": true, "verified": true}}
	if c, _, _ := signIn(t, r, "github", "c1"); c != 403 {
		t.Errorf("outside ALLOWED_EMAIL_DOMAINS: %d", c)
	}

	db.Create(&models.User{ID: "u-1", Email: "dev@corp.com", Password: "x", Name: "Dev", Role: models.RoleUser})
	idp.githubEmails = []map[string]any{{"email": "dev@corp.com", "primary": true, "verified": true}}
	c, out, _ := signIn(t, r, "github", "c2")
	d := out["data"].(map[string]any)
	if c != 200 || d["new"] != false || d["merged"] != true || d["user"].(map[string]any)["id"] != "u-1" {
		t.Fatalf("merge: %d %v", c, out)
	}
}
