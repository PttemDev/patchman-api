package handlers

import (
	"bytes"
	"net/http/httptest"
	"testing"

	"patchman-be/internal/cloudsession"

	"github.com/gin-gonic/gin"
)

// TestCloudSessionOriginGuard locks the security control that keeps a malicious
// web page in the user's browser from POSTing its own token to the loopback
// embedded endpoint (which would silently redirect the AI's writes to the
// attacker's account). Only the desktop webview's Origin — or no Origin at all
// (a non-browser local process, already able to run arbitrary code here) — is
// accepted; any other site's Origin is rejected.
func TestCloudSessionOriginGuard(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewCloudSessionHandler()

	post := func(origin string) int {
		cloudsession.Clear()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/api/cloud-session",
			bytes.NewReader([]byte(`{"baseUrl":"https://x.example/api","token":"t"}`)))
		c.Request.Header.Set("Content-Type", "application/json")
		if origin != "" {
			c.Request.Header.Set("Origin", origin)
		}
		h.Set(c)
		return w.Code
	}

	cases := []struct {
		origin   string
		wantCode int
		wantSet  bool
	}{
		{"tauri://localhost", 200, true},       // macOS/Linux webview — allowed
		{"https://tauri.localhost", 200, true}, // Windows webview — allowed
		{"", 200, true},                        // non-browser local process — allowed
		{"https://evil.example", 403, false},   // browser attack — rejected
		{"http://127.0.0.1:1234", 403, false},  // another loopback page — rejected
	}
	for _, tc := range cases {
		if code := post(tc.origin); code != tc.wantCode {
			t.Errorf("origin %q: got %d want %d", tc.origin, code, tc.wantCode)
		}
		if _, ok := cloudsession.Get(); ok != tc.wantSet {
			t.Errorf("origin %q: session set=%v want %v", tc.origin, ok, tc.wantSet)
		}
	}
	cloudsession.Clear()
}
