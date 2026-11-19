package middleware

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// Embedded (desktop) BE access gate — DESKTOP_SECURITY_RFC L1/L2.
//
// The embedded BE listens on 127.0.0.1 and used to trust every caller, so any
// local process — and any web page the user opened, via CORS * — could read
// every connection and secret. The desktop shell now mints a random launch
// token per run, hands it to this process (env) and to its own windows (IPC);
// every request must carry it. Same-user malware can still read process env /
// memory: this stops browsers and other OS users, not code already running as
// the user.

// LocalTokenHeader carries the launch token on fetch/XHR requests.
const LocalTokenHeader = "X-Local-Token"

// LocalTokenQuery carries it where headers can't be set (WebSocket, EventSource).
const LocalTokenQuery = "pm_lt"

// LocalGateExempt reports paths that skip the token check:
//   - health probes (the shell polls readiness before it has a window),
//   - /api/mcp (authenticated by its own pmcp_ bearer token),
//   - /mock/* (mock servers are meant to be called by external clients).
func LocalGateExempt(path string) bool {
	switch {
	case path == "/healthz" || path == "/readyz":
		return true
	case path == "/api/mcp" || strings.HasPrefix(path, "/api/mcp/"):
		return true
	case path == "/mock" || strings.HasPrefix(path, "/mock/"):
		return true
	// Plugin UI files: static, not secret, loaded by a sandboxed iframe that
	// cannot carry the launch token (and must never see it). Served with
	// their own CSP (PLUGIN_MARKETPLACE_RFC §16.3).
	case strings.HasPrefix(path, "/plugin-ui/"):
		return true
	}
	return false
}

// isLoopbackHost reports whether a Host header names this machine's loopback
// interface. Anything else (a DNS-rebinding name resolving to 127.0.0.1, a LAN
// address) is refused.
func isLoopbackHost(host string) bool {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.Trim(h, "[]")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// LocalSecurityPath is the desktop shell's control endpoint: it pushes the
// lock state and the step-up window here (DESKTOP_SECURITY_RFC L6/L7).
const LocalSecurityPath = "/api/_local/security"

// LocalControlHeader carries the shell-only control token on that endpoint.
const LocalControlHeader = "X-Local-Control"

// LocalSecurity is the lock / step-up state the desktop shell controls. Only
// the shell knows the control token, so the webview's JS can't unlock the BE
// or extend the step-up window by itself.
type LocalSecurity struct {
	control []byte

	mu          sync.RWMutex
	locked      bool
	blockMCP    bool
	sudoEnabled bool
	sudoUntil   int64 // unix seconds
	now         func() time.Time
}

// NewLocalSecurity starts locked when the shell says the app lock is on, so
// nothing is served before the first unlock. MCP blocking and step-up default
// on until the shell pushes the user's settings.
func NewLocalSecurity(control string, startLocked bool) *LocalSecurity {
	return &LocalSecurity{control: []byte(control), locked: startLocked, blockMCP: true, sudoEnabled: true, now: time.Now}
}

type localSecurityState struct {
	Locked      bool  `json:"locked"`
	BlockMCP    bool  `json:"blockMcp"`
	SudoEnabled bool  `json:"sudoEnabled"`
	SudoUntil   int64 `json:"sudoUntil"`
}

// Handler applies a state push from the shell.
func (s *LocalSecurity) Handler(c *gin.Context) {
	got := []byte(c.GetHeader(LocalControlHeader))
	if len(s.control) == 0 || subtle.ConstantTimeCompare(got, s.control) != 1 {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return
	}
	var in localSecurityState
	if err := c.ShouldBindJSON(&in); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	s.mu.Lock()
	s.locked, s.blockMCP, s.sudoEnabled, s.sudoUntil = in.Locked, in.BlockMCP, in.SudoEnabled, in.SudoUntil
	s.mu.Unlock()
	c.Status(http.StatusNoContent)
}

func (s *LocalSecurity) state() localSecurityState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return localSecurityState{Locked: s.locked, BlockMCP: s.blockMCP, SudoEnabled: s.sudoEnabled, SudoUntil: s.sudoUntil}
}

// Sensitive routes need a fresh step-up: revealing stored secrets, shells and
// exec, port-forwards and cluster writes. It is a registry, not a naming
// convention: a pattern-only list missed reveal endpoints that don't follow
// the `-connections/:id/reveal-secret` shape (1.60.0). Built-ins are listed
// below; RegisterSensitiveRoute adds more (plugins, new tools) at startup.
type sensitiveRoute struct {
	method string
	re     *regexp.Regexp
}

var (
	sensitiveMu       sync.RWMutex
	sensitiveRoutes   []sensitiveRoute
	sensitiveMatchers []func(method, path string) bool
)

func init() {
	for _, r := range [][2]string{
		// Stored secrets.
		{"POST", "/api/*-connections/:id/reveal-secret"},
		{"POST", "/api/db-connections/:id/reveal-password"},
		{"POST", "/api/llm-providers/:id/reveal-api-key"},
		{"GET", "/api/git/connections/:id/secret"},
		{"GET", "/api/environments/:id/variables/:key/reveal"},
		// Shells.
		{"GET", "/api/k8s/:id/exec"},
		{"GET", "/api/k8s/:id/local-shell"},
		{"GET", "/api/ai/claude-shell"},
		// Cluster writes and port-forwards.
		{"POST", "/api/k8s/:id/apply"},
		{"POST", "/api/k8s/:id/scale"},
		{"POST", "/api/k8s/:id/rollout-restart"},
		{"POST", "/api/k8s/:id/cordon"},
		{"POST", "/api/k8s/:id/drain"},
		{"POST", "/api/k8s/:id/cronjob/trigger"},
		{"POST", "/api/k8s/:id/port-forward"},
		{"DELETE", "/api/k8s/:id/resource"},
		// Git: drops unmerged commits.
		{"POST", "/api/git/repos/:repoId/branches/force-delete"},
		{"POST", "/api/git/repos/:repoId/push-to/force"},
		{"POST", "/api/git/repos/:repoId/remote-branches/delete"},
		{"POST", "/api/git/connections/:id/merge_request_merge"},
	} {
		RegisterSensitiveRoute(r[0], r[1])
	}
}

// RegisterSensitiveRoute marks method+route as needing a fresh step-up. The
// route uses gin syntax: ":name" matches one path segment; a segment like
// "*-connections" matches any lowercase name with that suffix.
func RegisterSensitiveRoute(method, route string) {
	re := regexp.MustCompile(routePattern(route))
	sensitiveMu.Lock()
	sensitiveRoutes = append(sensitiveRoutes, sensitiveRoute{strings.ToUpper(method), re})
	sensitiveMu.Unlock()
}

func routePattern(route string) string {
	segs := strings.Split(strings.Trim(route, "/"), "/")
	for i, s := range segs {
		switch {
		case strings.HasPrefix(s, ":"):
			segs[i] = `[^/]+`
		case strings.HasPrefix(s, "*"):
			segs[i] = `[a-z0-9-]+` + regexp.QuoteMeta(s[1:])
		default:
			segs[i] = regexp.QuoteMeta(s)
		}
	}
	return "^/" + strings.Join(segs, "/") + "$"
}

// RegisterSensitiveMatcher adds a dynamic rule for routes whose sensitivity
// changes at runtime (plugin ops: a sensitive op appears on install and goes
// away on uninstall). fn must be fast and safe for concurrent use.
func RegisterSensitiveMatcher(fn func(method, path string) bool) {
	sensitiveMu.Lock()
	sensitiveMatchers = append(sensitiveMatchers, fn)
	sensitiveMu.Unlock()
}

// IsSensitiveRoute reports whether method+path needs a fresh step-up.
func IsSensitiveRoute(method, path string) bool {
	sensitiveMu.RLock()
	defer sensitiveMu.RUnlock()
	for _, r := range sensitiveRoutes {
		if r.method == method && r.re.MatchString(path) {
			return true
		}
	}
	for _, fn := range sensitiveMatchers {
		if fn(method, path) {
			return true
		}
	}
	return false
}

type inProcessKey struct{}

// WithInProcess marks a request the BE dispatches to its own router (the MCP
// tools call handlers through the gin engine). It travels in the Go context,
// which no network client can set, so LocalGate can trust it: such requests
// skip the Host / launch-token checks (they never left the process) and
// step-up (agents can't answer Touch ID; their own token scopes govern). The
// lock still applies.
func WithInProcess(ctx context.Context) context.Context {
	return context.WithValue(ctx, inProcessKey{}, true)
}

// IsInProcessRequest reports whether ctx belongs to a request the BE
// dispatched to itself (an MCP tool). Such requests skip step-up, so a
// handler that must never run without a human confirmation refuses them.
func IsInProcessRequest(ctx context.Context) bool {
	v, _ := ctx.Value(inProcessKey{}).(bool)
	return v
}

func isInProcess(c *gin.Context) bool {
	v, _ := c.Request.Context().Value(inProcessKey{}).(bool)
	return v
}

// LocalGate enforces, in order: the loopback Host check (every request), the
// launch token (non-exempt requests; an empty token fails closed), the app
// lock (423 while locked; MCP too when the user blocks it) and the step-up
// window on sensitive routes (428). sec may be nil (no lock / step-up).
func LocalGate(token string, sec *LocalSecurity) gin.HandlerFunc {
	want := []byte(token)
	return func(c *gin.Context) {
		if isInProcess(c) {
			if sec != nil {
				if st := sec.state(); st.Locked && st.BlockMCP {
					c.AbortWithStatusJSON(http.StatusLocked, gin.H{"error": "PatchMan is locked", "locked": true})
					return
				}
			}
			c.Next()
			return
		}
		if !isLoopbackHost(c.Request.Host) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "host not allowed"})
			return
		}
		path := c.Request.URL.Path
		if c.Request.Method == http.MethodOptions {
			c.Next()
			return
		}
		var st localSecurityState
		if sec != nil {
			st = sec.state()
		}
		if LocalGateExempt(path) {
			isMCP := path == "/api/mcp" || strings.HasPrefix(path, "/api/mcp/")
			if isMCP && st.Locked && st.BlockMCP {
				c.AbortWithStatusJSON(http.StatusLocked, gin.H{"error": "PatchMan is locked — unlock the app to use its tools", "locked": true})
				return
			}
			c.Next()
			return
		}
		got := c.GetHeader(LocalTokenHeader)
		if got == "" {
			got = c.Query(LocalTokenQuery)
		}
		if len(want) == 0 || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "local access token required"})
			return
		}
		if sec != nil && path != LocalSecurityPath {
			if st.Locked {
				c.AbortWithStatusJSON(http.StatusLocked, gin.H{"error": "PatchMan is locked", "locked": true})
				return
			}
			if st.SudoEnabled && IsSensitiveRoute(c.Request.Method, path) && sec.now().Unix() >= st.SudoUntil {
				c.AbortWithStatusJSON(http.StatusPreconditionRequired, gin.H{"error": "confirm it's you to continue", "stepUp": true})
				return
			}
		}
		c.Next()
	}
}

// EmbeddedOriginAllowed is the CORS origin policy for the embedded BE: the
// desktop webview origins plus any explicitly configured extra (dev server).
func EmbeddedOriginAllowed(extra []string) func(origin string) bool {
	allowed := map[string]bool{
		"tauri://localhost":       true,
		"http://tauri.localhost":  true,
		"https://tauri.localhost": true,
	}
	for _, o := range extra {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" {
			allowed[o] = true
		}
	}
	return func(origin string) bool { return allowed[origin] }
}
