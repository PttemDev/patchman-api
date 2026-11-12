package handlers

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"patchman-be/internal/config"
	"patchman-be/internal/models"
	"patchman-be/internal/services"
)

// fakeOAuth rotates refresh tokens like GitLab: each one works once.
type fakeOAuth struct {
	mu      sync.Mutex
	current string
	granted int
	delay   time.Duration // before answering a successful refresh
}

func (f *fakeOAuth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	if r.Form.Get("refresh_token") != f.current {
		f.mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		return
	}
	f.granted++
	n := f.granted
	f.current = fmt.Sprintf("r%d", n)
	f.mu.Unlock()
	time.Sleep(f.delay)
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("a%d", n), "refresh_token": fmt.Sprintf("r%d", n), "expires_in": 7200})
}

// refreshTestHandler stores user u1's token pair (a0, r0) expiring in 30 s —
// inside the refresh window.
func refreshTestHandler(t *testing.T, oauth *fakeOAuth) *GitIntegrationHandler {
	t.Helper()
	srv := httptest.NewServer(oauth)
	t.Cleanup(srv.Close)
	dsn := fmt.Sprintf("file:%s_%d?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"), time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1) // one connection: no shared-cache table locks between goroutines
	if err := db.AutoMigrate(&models.UserGitIntegration{}); err != nil {
		t.Fatal(err)
	}
	cipher, _ := services.NewTokenCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)))
	acc, _ := cipher.Encrypt("a0")
	ref, _ := cipher.Encrypt("r0")
	if err := db.Create(&models.UserGitIntegration{UserID: "u1", Provider: "gitlab", AccessTokenEnc: acc, RefreshTokenEnc: ref,
		ExpiresAt: time.Now().Add(30 * time.Second)}).Error; err != nil {
		t.Fatal(err)
	}
	oauth.current = "r0"
	return &GitIntegrationHandler{db: db, cfg: &config.Config{GitLabBaseURL: srv.URL}, cipher: cipher, http: srv.Client()}
}

func concurrently(n int, fn func() (string, error)) (tokens []string, errs []error) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := fn()
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			} else {
				tokens = append(tokens, tok)
			}
		}()
	}
	wg.Wait()
	return tokens, errs
}

// The Git panel's parallel requests (repos, branches, commits, pipelines) hit
// a due token together: exactly one refresh, every request gets the new token.
// Before the per-user lock, 3 of 4 failed with invalid_grant ("link your
// GitLab account"), which left e.g. the branch list randomly empty.
func TestValidAccessTokenConcurrentRefreshOnce(t *testing.T) {
	oauth := &fakeOAuth{delay: 50 * time.Millisecond}
	h := refreshTestHandler(t, oauth)
	tokens, errs := concurrently(4, func() (string, error) { return h.validAccessToken("u1") })
	if len(errs) > 0 || oauth.granted != 1 {
		t.Fatalf("errs=%v granted=%d", errs, oauth.granted)
	}
	for _, tok := range tokens {
		if tok != "a1" {
			t.Fatalf("tokens = %v", tokens)
		}
	}
}

// Another pod of the hosted backend can spend the refresh token first (the
// in-process lock can't see it): the loser waits for the winner's row instead
// of failing.
func TestRefreshStoredTokenPeerPodWins(t *testing.T) {
	oauth := &fakeOAuth{delay: 150 * time.Millisecond}
	h := refreshTestHandler(t, oauth)
	// No lock: each call stands for a different pod.
	tokens, errs := concurrently(2, func() (string, error) { return h.refreshStoredToken("u1") })
	if len(errs) > 0 || oauth.granted != 1 || len(tokens) != 2 || tokens[0] != "a1" || tokens[1] != "a1" {
		t.Fatalf("tokens=%v errs=%v granted=%d", tokens, errs, oauth.granted)
	}
}

// GitLab unreachable while the stored token still has seconds left: use it.
func TestValidAccessTokenRefreshFailsTokenStillValid(t *testing.T) {
	oauth := &fakeOAuth{}
	h := refreshTestHandler(t, oauth)
	oauth.current = "revoked-elsewhere" // every refresh fails; nobody stores a new token
	old := peerRefreshWait
	peerRefreshWait = 300 * time.Millisecond
	t.Cleanup(func() { peerRefreshWait = old })
	tok, err := h.validAccessToken("u1")
	if err != nil || tok != "a0" {
		t.Fatalf("tok=%q err=%v", tok, err)
	}
}
