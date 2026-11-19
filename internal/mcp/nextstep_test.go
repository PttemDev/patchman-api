package mcp

import (
	"strings"
	"testing"
)

// A hosted server's "connection not found" points at the desktop MCP (the
// connection may be local-only); the desktop's own server doesn't.
func TestNextStepDesktopHint(t *testing.T) {
	body := `{"error":"Connection not found"}`
	embedded.Store(false)
	if got := withNextStep(404, body); !strings.Contains(got, "patchman-local") || !strings.Contains(got, "list_*_connections") {
		t.Fatalf("hosted 404: %s", got)
	}
	embedded.Store(true)
	defer embedded.Store(false)
	if got := withNextStep(404, body); strings.Contains(got, "patchman-local") {
		t.Fatalf("desktop 404 must not point at itself: %s", got)
	}
	if got := withNextStep(404, `{"error":"Note not found"}`); strings.Contains(got, "patchman-local") {
		t.Fatalf("non-connection 404: %s", got)
	}
}
