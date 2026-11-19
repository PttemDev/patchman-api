package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func gateRouter(token string) *gin.Engine { return gateRouterSec(token, nil) }

// Reveal endpoints outside the -connections naming (missed by 1.60.0).
var revealRoutes = [][3]string{
	{"POST", "/api/db-connections/:id/reveal-password", "/api/db-connections/x/reveal-password"},
	{"POST", "/api/llm-providers/:id/reveal-api-key", "/api/llm-providers/x/reveal-api-key"},
	{"GET", "/api/git/connections/:id/secret", "/api/git/connections/x/secret"},
	{"GET", "/api/environments/:id/variables/:key/reveal", "/api/environments/x/variables/K/reveal"},
}

func gateRouterSec(token string, sec *LocalSecurity) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	if sec != nil {
		r.POST(LocalSecurityPath, sec.Handler)
	}
	r.Use(LocalGate(token, sec))
	ok := func(c *gin.Context) { c.String(200, "ok") }
	r.GET("/api/k8s-connections", ok)
	r.GET("/healthz", ok)
	r.POST("/api/mcp", ok)
	r.GET("/mock/x", ok)
	r.POST("/api/k8s-connections/:id/reveal-secret", ok)
	r.GET("/api/k8s/:id/exec", ok)
	for _, rt := range revealRoutes {
		r.Handle(rt[0], rt[1], ok)
	}
	return r
}

func doReq(r http.Handler, method, target, host string, hdr map[string]string) int {
	req := httptest.NewRequest(method, target, nil)
	req.Host = host
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

// Regression for DESKTOP_SECURITY_RFC F1/F3: the embedded BE answered every
// caller without credentials.
func TestLocalGate(t *testing.T) {
	r := gateRouter("s3cret")
	const host = "127.0.0.1:60883"
	cases := []struct {
		name   string
		method string
		target string
		host   string
		hdr    map[string]string
		want   int
	}{
		{"no token", "GET", "/api/k8s-connections", host, nil, 401},
		{"wrong token", "GET", "/api/k8s-connections", host, map[string]string{LocalTokenHeader: "nope"}, 401},
		{"header token", "GET", "/api/k8s-connections", host, map[string]string{LocalTokenHeader: "s3cret"}, 200},
		{"query token (ws/sse)", "GET", "/api/k8s-connections?pm_lt=s3cret", host, nil, 200},
		{"localhost host", "GET", "/api/k8s-connections", "localhost:1", map[string]string{LocalTokenHeader: "s3cret"}, 200},
		{"ipv6 loopback", "GET", "/api/k8s-connections", "[::1]:1", map[string]string{LocalTokenHeader: "s3cret"}, 200},
		{"rebinding host", "GET", "/api/k8s-connections", "evil.example:60883", map[string]string{LocalTokenHeader: "s3cret"}, 403},
		{"lan host", "GET", "/healthz", "192.168.1.5:60883", nil, 403},
		{"health exempt", "GET", "/healthz", host, nil, 200},
		{"mcp exempt", "POST", "/api/mcp", host, nil, 200},
		{"mock exempt", "GET", "/mock/x", host, nil, 200},
	}
	for _, tc := range cases {
		if got := doReq(r, tc.method, tc.target, tc.host, tc.hdr); got != tc.want {
			t.Errorf("%s: got %d want %d", tc.name, got, tc.want)
		}
	}
}

func TestLocalGateEmptyTokenFailsClosed(t *testing.T) {
	r := gateRouter("")
	if got := doReq(r, "GET", "/api/k8s-connections", "127.0.0.1:1", map[string]string{LocalTokenHeader: ""}); got != 401 {
		t.Fatalf("empty token must fail closed, got %d", got)
	}
	if got := doReq(r, "GET", "/api/k8s-connections?pm_lt=", "127.0.0.1:1", nil); got != 401 {
		t.Fatalf("empty query token must fail closed, got %d", got)
	}
}

func TestEmbeddedOriginAllowed(t *testing.T) {
	allow := EmbeddedOriginAllowed([]string{"http://localhost:5173/"})
	for o, want := range map[string]bool{
		"tauri://localhost":      true,
		"http://tauri.localhost": true,
		"http://localhost:5173":  true,
		"https://evil.example":   false,
		"http://localhost:3000":  false,
		"null":                   false,
	} {
		if allow(o) != want {
			t.Errorf("%s: got %v", o, !want)
		}
	}
}

func TestLocalLockAndStepUp(t *testing.T) {
	sec := NewLocalSecurity("ctl", true)
	clock := time.Unix(1_000_000, 0)
	sec.now = func() time.Time { return clock }
	r := gateRouterSec("tok", sec)
	const host = "127.0.0.1:1"
	tok := map[string]string{LocalTokenHeader: "tok"}

	if got := doReq(r, "GET", "/api/k8s-connections", host, tok); got != 423 {
		t.Fatalf("locked: want 423, got %d", got)
	}
	if got := doReq(r, "POST", "/api/mcp", host, nil); got != 423 {
		t.Fatalf("locked MCP: want 423, got %d", got)
	}
	if got := doReq(r, "GET", "/healthz", host, nil); got != 200 {
		t.Fatalf("health while locked: %d", got)
	}

	push := func(body string, ctl string) int {
		req := httptest.NewRequest("POST", LocalSecurityPath, strings.NewReader(body))
		req.Host = host
		req.Header.Set(LocalControlHeader, ctl)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	// The webview only knows the launch token — it can't unlock.
	if got := push(`{"locked":false}`, "tok"); got != 403 {
		t.Fatalf("unlock with wrong control token: want 403, got %d", got)
	}
	if got := push(`{"locked":false,"blockMcp":true,"sudoEnabled":true,"sudoUntil":1000060}`, "ctl"); got != 204 {
		t.Fatalf("unlock push: %d", got)
	}
	if got := doReq(r, "GET", "/api/k8s-connections", host, tok); got != 200 {
		t.Fatalf("unlocked: %d", got)
	}
	if got := doReq(r, "POST", "/api/k8s-connections/x/reveal-secret", host, tok); got != 200 {
		t.Fatalf("fresh step-up: %d", got)
	}
	for _, rt := range revealRoutes {
		if got := doReq(r, rt[0], rt[2], host, tok); got != 200 {
			t.Fatalf("fresh step-up on %s %s: %d", rt[0], rt[2], got)
		}
	}
	clock = clock.Add(2 * time.Minute)
	if got := doReq(r, "POST", "/api/k8s-connections/x/reveal-secret", host, tok); got != 428 {
		t.Fatalf("stale step-up: want 428, got %d", got)
	}
	for _, rt := range revealRoutes {
		if got := doReq(r, rt[0], rt[2], host, tok); got != 428 {
			t.Fatalf("stale step-up on %s %s: want 428, got %d", rt[0], rt[2], got)
		}
	}
	if got := doReq(r, "GET", "/api/k8s/x/exec?pm_lt=tok", host, nil); got != 428 {
		t.Fatalf("stale step-up on exec: want 428, got %d", got)
	}
	if got := push(`{"locked":false,"blockMcp":false,"sudoEnabled":false,"sudoUntil":0}`, "ctl"); got != 204 {
		t.Fatalf("push: %d", got)
	}
	if got := doReq(r, "POST", "/api/k8s-connections/x/reveal-secret", host, tok); got != 200 {
		t.Fatalf("step-up disabled: %d", got)
	}
}

func TestSensitiveRoutes(t *testing.T) {
	for _, tc := range []struct {
		m, p string
		want bool
	}{
		{"POST", "/api/db-connections/1/reveal-secret", true},
		{"POST", "/api/k8s-connections/1/reveal-secret", true},
		{"POST", "/api/observability-connections/1/reveal-secret", true},
		{"GET", "/api/k8s/1/exec", true},
		{"GET", "/api/k8s/1/resources", false},
		{"POST", "/api/k8s/1/port-forward", true},
		{"DELETE", "/api/k8s/1/port-forward/abc", false},
		{"DELETE", "/api/k8s/1/resource", true},
		{"GET", "/api/ai/claude-shell", true},
		// Reveal endpoints outside the -connections naming (missed in 1.60.0).
		{"POST", "/api/db-connections/1/reveal-password", true},
		{"POST", "/api/llm-providers/1/reveal-api-key", true},
		{"GET", "/api/git/connections/1/secret", true},
		{"GET", "/api/environments/1/variables/API_KEY/reveal", true},
		{"GET", "/api/environments/1/variables", false},
		{"GET", "/api/git/connections/1", false},
		{"POST", "/api/git/connections/1/secret", false},
		{"POST", "/api/k8s/1/cronjob/trigger", true},
		{"POST", "/api/k8s/1/cronjob/other", false},
		{"POST", "/api/K8S-connections/1/reveal-secret", false},
	} {
		if got := IsSensitiveRoute(tc.m, tc.p); got != tc.want {
			t.Errorf("%s %s: got %v", tc.m, tc.p, got)
		}
	}
}

func TestRegisterSensitiveRoute(t *testing.T) {
	if IsSensitiveRoute("POST", "/api/plugins/acme.tool/c1/ops/dump") {
		t.Fatal("not registered yet")
	}
	RegisterSensitiveRoute("post", "/api/plugins/acme.tool/:conn/ops/dump")
	if !IsSensitiveRoute("POST", "/api/plugins/acme.tool/c1/ops/dump") {
		t.Fatal("registered route not sensitive")
	}
	if IsSensitiveRoute("POST", "/api/plugins/acmeXtool/c1/ops/dump") {
		t.Fatal("literal dot must not act as a wildcard")
	}
}

// Regression (packaged build 2026-11-16): MCP tools dispatch through the gin
// engine in-process with httptest's Host "example.com" and no launch token;
// the gate refused them ("host not allowed"). The context marker can't come
// from the network, so they pass — but the lock still holds.
func TestInProcessRequests(t *testing.T) {
	sec := NewLocalSecurity("ctl", false)
	r := gateRouterSec("tok", sec)
	inproc := func() int {
		req := httptest.NewRequest("GET", "/api/k8s-connections", nil)
		req = req.WithContext(WithInProcess(req.Context()))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	if got := inproc(); got != 200 {
		t.Fatalf("in-process: want 200, got %d", got)
	}
	if got := doReq(r, "GET", "/api/k8s-connections", "example.com", nil); got != 403 {
		t.Fatalf("network request with the same Host: want 403, got %d", got)
	}
	sec.mu.Lock()
	sec.locked = true
	sec.mu.Unlock()
	if got := inproc(); got != 423 {
		t.Fatalf("in-process while locked: want 423, got %d", got)
	}
}
