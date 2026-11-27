package mcp

import (
	"strings"
	"testing"

	"patchman-be/internal/mcpscope"
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

// A target the hosted server can't reach (VPN / localhost-only host) points
// at the desktop MCP, whatever status the handler chose; on the desktop's
// own server, or for an error the target itself returned, it doesn't.
func TestNextStepHostedUnreachable(t *testing.T) {
	cases := []struct {
		code int
		body string
	}{
		{502, `{"error":"failed to connect to host=10.1.2.3 user=x database=y: dial tcp 10.1.2.3:5432: i/o timeout"}`},
		{500, `{"error":"dial tcp: lookup redis.internal on 8.8.8.8:53: no such host"}`},
		{400, `{"error":"kafka: dial tcp 127.0.0.1:9092: connect: connection refused"}`},
	}
	embedded.Store(false)
	for _, c := range cases {
		got := withNextStep(c.code, c.body)
		if !strings.Contains(got, "patchman-local") || !strings.Contains(got, "Settings") {
			t.Fatalf("hosted %d: %s", c.code, got)
		}
		if !strings.Contains(got, "dial tcp") {
			t.Fatalf("original error must stay: %s", got)
		}
	}
	if got := withNextStep(400, `{"error":"ERROR: relation \"x\" does not exist (SQLSTATE 42P01)"}`); strings.Contains(got, "patchman-local") {
		t.Fatalf("SQL error is not a reachability problem: %s", got)
	}
	embedded.Store(true)
	defer embedded.Store(false)
	if got := withNextStep(502, cases[0].body); strings.Contains(got, "Settings → MCP") {
		t.Fatalf("desktop server must not send the user to itself: %s", got)
	}
}

// Only the hosted server tells agents that its tools never run locally.
func TestHostedInstructions(t *testing.T) {
	all := mcpscope.ToolScopes{All: true}
	embedded.Store(false)
	if got := serverInstructions(all); !strings.Contains(got, "this is the hosted PatchMan server") {
		t.Fatalf("hosted instructions missing: %s", got)
	}
	embedded.Store(true)
	defer embedded.Store(false)
	if got := serverInstructions(all); strings.Contains(got, "this is the hosted PatchMan server") {
		t.Fatalf("desktop instructions must not claim to be hosted")
	}
}
