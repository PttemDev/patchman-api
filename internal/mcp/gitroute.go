package mcp

// Per-connection routing for the git tools — the same rule the app's own
// screens use (fe gitConnectionStore → setGitReadScopeResolver):
//
//   - a token (PAT) connection carries its own GitLab token on every backend
//     that holds it, so it runs on THIS backend when it is here — on the
//     desktop that is often the only machine that can reach that GitLab;
//   - a connection that exists only here runs here (the hosted BE has never
//     heard of it: routing it there returned "not found" / empty lists);
//   - an OAuth connection otherwise goes to the hosted BE while the desktop is
//     signed in, because the caller's linked GitLab account lives there.
//
// Without a cloud session everything runs in-process, as before.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"patchman-be/internal/cloudsession"
)

// embedded is set at startup (SetEmbedded): only the hosted server's MCP adds
// the "this connection lives in the desktop app" hint.
var embedded atomic.Bool

// SetEmbedded records whether this backend runs inside the desktop app.
func SetEmbedded(v bool) { embedded.Store(v) }

type gitConnInfo struct {
	ID       string `json:"id"`
	AuthKind string `json:"authKind"`
}

// localGitConn returns this backend's view of a git connection (ok=false when
// it isn't here or isn't accessible to the caller).
func (c *caller) localGitConn(ctx context.Context, connID string) (gitConnInfo, bool) {
	code, body := c.callCtx(ctx, "GET", "/api/git/connections/"+url.PathEscape(connID), nil)
	if code != http.StatusOK {
		return gitConnInfo{}, false
	}
	var out struct {
		Data gitConnInfo `json:"data"`
	}
	if json.Unmarshal([]byte(body), &out) != nil || out.Data.ID == "" {
		return gitConnInfo{}, false
	}
	return out.Data, true
}

// gitCallCtx runs one git tool call where its connection works (see above).
func (c *caller) gitCallCtx(ctx context.Context, connID, method, path string, body any) (int, string) {
	sess, signedIn := cloudsession.Get()
	if !signedIn {
		return c.withDesktopHint(c.callCtx(ctx, method, path, body))
	}
	if conn, here := c.localGitConn(ctx, connID); here {
		if strings.EqualFold(conn.AuthKind, "pat") {
			return c.callCtx(ctx, method, path, body)
		}
		// OAuth: the server, unless the server doesn't have this connection.
		if !c.cloudHasGitConn(ctx, sess, connID) {
			return c.callCtx(ctx, method, path, body)
		}
	}
	return callCloud(ctx, sess, method, path, body, c.agent, c.project)
}

// cloudHasGitConn asks the hosted BE whether it knows this connection; the
// answer is cached for a few minutes per user so a busy agent doesn't pay an
// extra round trip on every call. A network error counts as "has it" (the
// call then reports the real error from the server, as before).
func (c *caller) cloudHasGitConn(ctx context.Context, sess cloudsession.Session, connID string) bool {
	key := c.userID + "|" + connID
	if v, ok := cloudConnCache.Load(key); ok {
		if e := v.(cloudConnEntry); time.Since(e.at) < cloudConnTTL {
			return e.has
		}
	}
	code, _ := callCloud(ctx, sess, "GET", "/api/git/connections/"+url.PathEscape(connID), nil, c.agent, c.project)
	has := code != http.StatusNotFound
	cloudConnCache.Store(key, cloudConnEntry{has: has, at: time.Now()})
	return has
}

type cloudConnEntry struct {
	has bool
	at  time.Time
}

const cloudConnTTL = 5 * time.Minute

var cloudConnCache sync.Map

// withDesktopHint: on the hosted server, a git connection id it doesn't know
// is most likely one kept only in someone's desktop app — say so instead of a
// bare "not found", so an agent switches to the desktop's MCP.
func (c *caller) withDesktopHint(code int, body string) (int, string) {
	if embedded.Load() || code != http.StatusNotFound || !strings.Contains(body, "Connection not found") {
		return code, body
	}
	b, _ := json.Marshal(map[string]string{
		"error": "Connection not found on this PatchMan server. If it is a GitLab connection saved only in the PatchMan desktop app (local scope), " +
			"call this tool through the desktop app's MCP server (patchman-local) instead — the hosted server cannot see or reach it.",
	})
	return code, string(b)
}

// listGitConnections merges this backend's git connections with the hosted
// BE's (when signed in), each tagged with where its tools run ("runsOn":
// "local" | "cloud"), so local-only connections are visible to agents too.
func (c *caller) listGitConnections(ctx context.Context) (int, string) {
	lc, lb := c.callCtx(ctx, "GET", "/api/git/connections", nil)
	sess, signedIn := cloudsession.Get()
	if !signedIn {
		return lc, lb
	}
	cc, cb := callCloud(ctx, sess, "GET", "/api/git/connections", nil, c.agent, c.project)
	parse := func(code int, body string) []map[string]any {
		if code != http.StatusOK {
			return nil
		}
		var out struct {
			Data []map[string]any `json:"data"`
		}
		_ = json.Unmarshal([]byte(body), &out)
		return out.Data
	}
	local, cloud := parse(lc, lb), parse(cc, cb)
	onCloud := map[string]bool{}
	for _, m := range cloud {
		if id, _ := m["id"].(string); id != "" {
			onCloud[id] = true
		}
	}
	merged := make([]map[string]any, 0, len(local)+len(cloud))
	seen := map[string]bool{}
	for _, m := range local {
		id, _ := m["id"].(string)
		if id == "" {
			continue
		}
		seen[id] = true
		kind, _ := m["authKind"].(string)
		m["runsOn"] = "cloud"
		if strings.EqualFold(kind, "pat") || !onCloud[id] {
			m["runsOn"] = "local"
		}
		merged = append(merged, m)
	}
	for _, m := range cloud {
		id, _ := m["id"].(string)
		if id == "" || seen[id] {
			continue
		}
		m["runsOn"] = "cloud"
		merged = append(merged, m)
	}
	resp := map[string]any{"data": merged}
	if cc != http.StatusOK {
		resp["note"] = "PatchMan server connections could not be listed (status " + http.StatusText(cc) + "); showing this computer's only."
	}
	b, _ := json.Marshal(resp)
	return http.StatusOK, string(b)
}
