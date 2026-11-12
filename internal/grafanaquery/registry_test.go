package grafanaquery

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/services"
)

func testCipher(t *testing.T) *services.TokenCipher {
	t.Helper()
	c, err := services.NewTokenCipher(base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type memStore struct {
	mu   sync.Mutex
	last map[string]string
}

func (m *memStore) SaveSession(id, cookie string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last == nil {
		m.last = map[string]string{}
	}
	m.last[id] = cookie
	return nil
}

func enc(t *testing.T, c *services.TokenCipher, s string) string {
	t.Helper()
	v, err := c.Encrypt(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestTokenAuthAndOrgHeader(t *testing.T) {
	cipher := testCipher(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer glsa_x" || r.Header.Get("X-Grafana-Org-Id") != "3" {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(`{"login":"sa"}`))
	}))
	defer srv.Close()
	reg := NewRegistry(cipher, nil)
	conn := models.GrafanaConnection{ID: "c1", URL: srv.URL + "/", AuthKind: "token", TokenEnc: enc(t, cipher, "glsa_x"), OptionsJSON: `{"orgId":3}`}
	var u map[string]any
	if err := reg.DoJSON(context.Background(), conn, "GET", "/api/user", nil, nil, &u); err != nil || u["login"] != "sa" {
		t.Fatalf("token call: %v %v", err, u)
	}
}

// fakeGrafana refuses HTTP Basic (auth.basic disabled), accepts the login
// form and enforces Grafana 10+ client token rotation.
type fakeGrafana struct {
	mu       sync.Mutex
	valid    map[string]bool // live session tokens
	logins   int32
	rotates  int32
	requests int32
	password string
	mustRot  map[string]bool // token → next API call answers session.token.rotate
}

func (f *fakeGrafana) handler(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&f.requests, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/login":
		atomic.AddInt32(&f.logins, 1)
		var b struct{ User, Password string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b.User != "alice" || b.Password != f.password {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"message":"Invalid username or password"}`))
			return
		}
		tok := "s" + string(rune('0'+len(f.valid)))
		f.valid[tok] = true
		http.SetCookie(w, &http.Cookie{Name: "grafana_session", Value: tok, Path: "/"})
		_, _ = w.Write([]byte(`{"message":"Logged in"}`))
		return
	case "/api/user/auth-tokens/rotate":
		atomic.AddInt32(&f.rotates, 1)
		c, err := r.Cookie("grafana_session")
		if err != nil || !f.valid[c.Value] {
			w.WriteHeader(401)
			return
		}
		delete(f.valid, c.Value)
		nt := c.Value + "r"
		f.valid[nt] = true
		http.SetCookie(w, &http.Cookie{Name: "grafana_session", Value: nt, Path: "/"})
		w.WriteHeader(200)
		return
	}
	if strings.HasPrefix(r.Header.Get("Authorization"), "Basic ") {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"message":"Basic auth is disabled"}`))
		return
	}
	c, err := r.Cookie("grafana_session")
	if err != nil || !f.valid[c.Value] {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"message":"Unauthorized","messageId":"auth.unauthorized"}`))
		return
	}
	if f.mustRot[c.Value] {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"message":"token needs rotation","messageId":"session.token.rotate"}`))
		return
	}
	_, _ = w.Write([]byte(`{"ok":true,"session":"` + c.Value + `"}`))
}

func newFake(password string) (*fakeGrafana, *httptest.Server) {
	f := &fakeGrafana{valid: map[string]bool{}, password: password, mustRot: map[string]bool{}}
	return f, httptest.NewServer(http.HandlerFunc(f.handler))
}

func TestBasicFallsBackToFormLogin(t *testing.T) {
	cipher := testCipher(t)
	f, srv := newFake("pw")
	defer srv.Close()
	reg := NewRegistry(cipher, nil)
	conn := models.GrafanaConnection{ID: "b1", URL: srv.URL, AuthKind: "basic", Username: "alice", PasswordEnc: enc(t, cipher, "pw")}
	for i := 0; i < 3; i++ {
		var out map[string]any
		if err := reg.DoJSON(context.Background(), conn, "GET", "/api/search", nil, nil, &out); err != nil || out["ok"] != true {
			t.Fatalf("call %d: %v %v", i, err, out)
		}
	}
	if f.logins != 1 {
		t.Fatalf("form login should happen once and be reused, got %d", f.logins)
	}
	// Session expires server-side → transparent re-login.
	f.mu.Lock()
	f.valid = map[string]bool{}
	f.mu.Unlock()
	if err := reg.DoJSON(context.Background(), conn, "GET", "/api/search", nil, nil, nil); err != nil {
		t.Fatalf("re-login: %v", err)
	}
	if f.logins != 2 {
		t.Fatalf("expected re-login, logins=%d", f.logins)
	}
}

func TestWrongPasswordBacksOff(t *testing.T) {
	cipher := testCipher(t)
	f, srv := newFake("right")
	defer srv.Close()
	reg := NewRegistry(cipher, nil)
	conn := models.GrafanaConnection{ID: "b2", URL: srv.URL, AuthKind: "basic", Username: "alice", PasswordEnc: enc(t, cipher, "wrong")}
	err := reg.DoJSON(context.Background(), conn, "GET", "/api/search", nil, nil, nil)
	ae, ok := AsAPIError(err)
	if !ok || ae.Code != "auth" || !strings.Contains(ae.Message, "SSO") {
		t.Fatalf("expected auth error with SSO hint, got %v", err)
	}
	before := atomic.LoadInt32(&f.requests)
	for i := 0; i < 5; i++ {
		_ = reg.DoJSON(context.Background(), conn, "GET", "/api/search", nil, nil, nil)
	}
	if atomic.LoadInt32(&f.requests) != before {
		t.Fatalf("failed login must back off instead of hammering Grafana (lockout risk)")
	}
	reg.Close("b2") // an edit clears the backoff
	_ = reg.DoJSON(context.Background(), conn, "GET", "/api/search", nil, nil, nil)
	if atomic.LoadInt32(&f.requests) == before {
		t.Fatal("Close should clear the backoff")
	}
}

func TestSessionRotation(t *testing.T) {
	cipher := testCipher(t)
	f, srv := newFake("pw")
	defer srv.Close()
	f.valid["s0"] = true
	f.mustRot["s0"] = true
	store := &memStore{}
	reg := NewRegistry(cipher, store)
	conn := models.GrafanaConnection{ID: "s1", URL: srv.URL, AuthKind: "session", SessionEnc: enc(t, cipher, "grafana_session=s0; other=1")}
	var out map[string]any
	if err := reg.DoJSON(context.Background(), conn, "GET", "/api/search", nil, nil, &out); err != nil || out["session"] != "s0r" {
		t.Fatalf("rotate+retry: %v %v", err, out)
	}
	if store.last["s1"] != "s0r" {
		t.Fatalf("rotated cookie must be persisted, got %q", store.last["s1"])
	}
	// The stale DB copy (s0) is ignored: the live cookie wins.
	if err := reg.DoJSON(context.Background(), conn, "GET", "/api/search", nil, nil, &out); err != nil || out["session"] != "s0r" {
		t.Fatalf("second call: %v %v", err, out)
	}
	if f.rotates != 1 {
		t.Fatalf("rotations=%d", f.rotates)
	}
	// Revoked session → clear "sign in again" error.
	f.mu.Lock()
	f.valid = map[string]bool{}
	f.mu.Unlock()
	err := reg.DoJSON(context.Background(), conn, "GET", "/api/search", nil, nil, nil)
	if ae, ok := AsAPIError(err); !ok || ae.Code != "session_expired" {
		t.Fatalf("expected session_expired, got %v", err)
	}
}

func TestConcurrentRotationSingleflight(t *testing.T) {
	cipher := testCipher(t)
	f, srv := newFake("pw")
	defer srv.Close()
	f.valid["s0"] = true
	f.mustRot["s0"] = true
	reg := NewRegistry(cipher, &memStore{})
	conn := models.GrafanaConnection{ID: "s2", URL: srv.URL, AuthKind: "session", SessionEnc: enc(t, cipher, "s0")}
	var wg sync.WaitGroup
	var fails int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if reg.DoJSON(context.Background(), conn, "GET", "/api/search", nil, nil, nil) != nil {
				atomic.AddInt32(&fails, 1)
			}
		}()
	}
	wg.Wait()
	if fails != 0 || f.rotates != 1 {
		t.Fatalf("parallel calls must share one rotation: fails=%d rotates=%d", fails, f.rotates)
	}
}

func TestRedirectIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	defer srv.Close()
	cipher := testCipher(t)
	reg := NewRegistry(cipher, nil)
	conn := models.GrafanaConnection{URL: srv.URL, AuthKind: "token", TokenEnc: enc(t, cipher, "x")}
	err := reg.DoJSON(context.Background(), conn, "GET", "/api/search", nil, nil, nil)
	if ae, ok := AsAPIError(err); !ok || !strings.Contains(ae.Message, "redirected") {
		t.Fatalf("expected redirect error, got %v", err)
	}
}

func TestServerSideRotationCaptured(t *testing.T) {
	cipher := testCipher(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Grafana < 10 rotates in its middleware and sets the cookie on any call.
		http.SetCookie(w, &http.Cookie{Name: "grafana_session", Value: "next", Path: "/"})
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	store := &memStore{}
	reg := NewRegistry(cipher, store)
	conn := models.GrafanaConnection{ID: "s3", URL: srv.URL, AuthKind: "session", SessionEnc: enc(t, cipher, "cur")}
	if err := reg.DoJSON(context.Background(), conn, "GET", "/api/search", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if store.last["s3"] != "next" {
		t.Fatalf("server-side rotation not captured: %v", store.last)
	}
}

// Test on an unsaved session connection that needed a rotation must hand the
// live cookie back — saving the pasted one would store a superseded token.
func TestTestReturnsRotatedSession(t *testing.T) {
	cipher := testCipher(t)
	f, srv := newFake("pw")
	defer srv.Close()
	f.valid["s0"] = true
	f.mustRot["s0"] = true
	reg := NewRegistry(cipher, &memStore{})
	conn := models.GrafanaConnection{URL: srv.URL, AuthKind: "session", SessionEnc: enc(t, cipher, "s0")}
	// fetchInfo needs /api/frontend/settings — the fake answers {"ok":true}
	// for any authenticated path, which decodes into an empty settings blob.
	_, rotated, err := reg.Test(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	if rotated != "s0r" || f.rotates != 1 {
		t.Fatalf("rotated=%q rotations=%d", rotated, f.rotates)
	}
}

// A newer rotation saved on the row (another replica, a re-login) wins over
// this process's stale live cookie.
func TestNewerRowSessionWins(t *testing.T) {
	cipher := testCipher(t)
	f, srv := newFake("pw")
	defer srv.Close()
	f.valid["s0"] = true
	f.mustRot["s0"] = true
	reg := NewRegistry(cipher, &memStore{})
	conn := models.GrafanaConnection{ID: "r1", URL: srv.URL, AuthKind: "session", SessionEnc: enc(t, cipher, "s0")}
	if err := reg.DoJSON(context.Background(), conn, "GET", "/api/search", nil, nil, nil); err != nil {
		t.Fatal(err) // rotates to s0r (live)
	}
	// Another replica rotated later and wrote "other" to the row.
	f.mu.Lock()
	f.valid["other"] = true
	f.mu.Unlock()
	later := time.Now().Add(5 * time.Second)
	conn.SessionEnc, conn.SessionRotatedAt = enc(t, cipher, "other"), &later
	var out map[string]any
	if err := reg.DoJSON(context.Background(), conn, "GET", "/api/search", nil, nil, &out); err != nil || out["session"] != "other" {
		t.Fatalf("row copy should win: %v %v", err, out)
	}
}

// Variables whose option list ends up empty (regex matching nothing, an
// interval with no values) serialize as [] — the dashboard view crashed on null.
func TestResolveVariablesEmptyOptionsAreArrays(t *testing.T) {
	cipher := testCipher(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"buildInfo":{"version":"11.6.0"},"datasources":{}}`))
	}))
	defer srv.Close()
	reg := NewRegistry(cipher, nil)
	conn := models.GrafanaConnection{ID: "v1", URL: srv.URL, AuthKind: "token", TokenEnc: enc(t, cipher, "x")}
	tl := []any{
		map[string]any{"name": "env", "type": "custom", "query": "prod,test", "regex": "/^nomatch$/"},
		map[string]any{"name": "iv", "type": "interval", "query": ""},
	}
	rng, _ := ParseRange("now-1h", "now", time.Now())
	vars, err := reg.ResolveVariables(context.Background(), conn, tl, nil, rng)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(vars)
	if strings.Contains(string(b), `"options":null`) || strings.Contains(string(b), `"values":null`) {
		t.Fatalf("null list in %s", b)
	}
}

// A datasource whose variable queries can't run through /api/ds/query
// yields saved values + a reason (shown in the FE), not a silent text box.
func TestUnlistableVariableHasReason(t *testing.T) {
	cipher := testCipher(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ds/query" {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"message":"plugin does not support queries"}`))
			return
		}
		_, _ = w.Write([]byte(`{"buildInfo":{"version":"11.6.0"},"datasources":{"X":{"id":1,"uid":"x","type":"acme-datasource","meta":{"backend":false}}}}`))
	}))
	defer srv.Close()
	reg := NewRegistry(cipher, nil)
	conn := models.GrafanaConnection{ID: "u1", URL: srv.URL, AuthKind: "token", TokenEnc: enc(t, cipher, "x")}
	tl := []any{map[string]any{
		"name": "svc", "type": "query", "datasource": map[string]any{"uid": "x"}, "query": "services()",
		"includeAll": true, "current": map[string]any{"text": []any{"api"}, "value": []any{"api"}},
	}}
	rng, _ := ParseRange("now-1h", "now", time.Now())
	vars, err := reg.ResolveVariables(context.Background(), conn, tl, nil, rng)
	if err != nil {
		t.Fatal(err)
	}
	v := vars[0]
	if !v.Free || !strings.Contains(v.Error, "acme-datasource") || len(v.Options) == 0 || v.Current.Values[0] != "api" {
		t.Fatalf("got %+v", v)
	}
}
