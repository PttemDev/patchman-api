package config

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	// Runtime mode: "server" (multi-user, remote PostgreSQL) or "embedded"
	// (single local user, bundled SQLite, no auth). Embedded mode is the
	// desktop Tauri sidecar path.
	Mode string

	// Network binding
	Port     string
	BindAddr string

	// Auth
	JWTSecret string

	// SessionTTL is how long an issued JWT stays valid. Kept generous so
	// people aren't logged out mid-week; the JWTAuth middleware slides it
	// forward (re-issues a fresh token past the half-life) so anyone who
	// keeps using the app effectively never logs out. Env: SESSION_TTL
	// (a Go duration like "720h"); default 30 days.
	SessionTTL time.Duration

	// AppName is a friendly name for this backend, surfaced by the public
	// GET /api/config discovery endpoint so a desktop client connecting via
	// `patchman://connect?backend=…` can label the remote without the user
	// typing one. Env: APP_NAME; default "PatchMan".
	AppName string

	// Database driver selection: "postgres" or "sqlite"
	DBDriver string

	// SQLite — used when DBDriver == "sqlite"
	DBPath string

	// Postgres — used when DBDriver == "postgres"
	DBHost    string
	DBPort    string
	DBName    string
	DBUser    string
	DBPass    string
	DBSSLMode string

	// Upstream forwarding (embedded mode only). When both are set, the
	// embedded BE mirrors history and test-run writes to this remote BE
	// using the given JWT. Used when execution runs on the user's laptop
	// but their data-of-record lives on the remote server.
	ForwardTarget string
	ForwardToken  string

	// GitLab integration — used by the collection-sync feature. When the
	// OAuth client is unset, the integration endpoints stay disabled and
	// surface a clear "not configured" error instead of crashing.
	GitLabBaseURL      string
	GitLabClientID     string
	GitLabClientSecret string
	// Base64-encoded 32-byte key. AES-GCM encrypts access/refresh tokens
	// at rest in user_git_integrations.
	GitLabTokenEncKey string

	// Slack incoming-webhook URL used to forward FE crash reports. Empty
	// in dev / embedded mode — the error-report handler refuses to post
	// when this is unset, so the FE still gets a clean 204 instead of an
	// upstream connection error.
	SlackErrorWebhookURL string

	// RealtimeAllowedOrigins is the comma-separated allowlist that the
	// WS Upgrade handler checks against the request's `Origin` header.
	// Empty (default) means "allow any origin" — the route is already
	// JWT-guarded so an attacker would need a valid token to reach the
	// Upgrade. Set this in prod to lock the socket to the known FE
	// deployment URLs (defense-in-depth against CSRF-style token leaks).
	RealtimeAllowedOrigins string

	// Tenant-specific values (per-deployment). All optional; sensible
	// behavior when empty so the same binary can be reused across
	// clients by just swapping the env file.

	// AllowedEmailDomains gates Register/Login/SSO sign-in to the listed
	// domains. Empty slice => no restriction (open registration).
	// Values are lowercased + leading-`@` stripped at parse time so the
	// env supports both forms (`example.com` and `@example.com`).
	AllowedEmailDomains []string

	// DefaultAdminEmail is the bootstrap admin row promoted to admin at
	// migrate time. Empty => no auto-admin (greenfield install). Same
	// address is protected from demotion / cross-admin password reset.
	DefaultAdminEmail string

	// AllowedRedirectOrigins enumerates the FE hosts the BE will hand
	// back as OAuth redirect URIs (GitLab integration + SSO). Empty =>
	// localhost dev hosts only. Always allows the Tauri deep-link
	// scheme (`patchman://`) for the desktop surface.
	AllowedRedirectOrigins []string
}

const (
	ModeServer   = "server"
	ModeEmbedded = "embedded"

	DriverPostgres = "postgres"
	DriverSQLite   = "sqlite"
)

func Load() *Config {
	// Load .env file based on APP_ENV (production, test, or default .env)
	appEnv := os.Getenv("APP_ENV")
	if appEnv != "" {
		envFile := ".env." + appEnv
		if err := godotenv.Load(envFile); err != nil {
			log.Printf("Warning: could not load %s: %v", envFile, err)
		}
	} else {
		_ = godotenv.Load() // .env (ignore if missing)
	}

	// CLI flags override env defaults. We use a scoped FlagSet so repeated
	// Load() calls (e.g. tests) don't trip Go's default flag registration.
	fs := flag.NewFlagSet("patchman-be", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	mode := fs.String("mode", getEnv("APP_MODE", ModeServer), "run mode: server | embedded")
	bind := fs.String("bind", getEnv("BIND_ADDR", ""), "override listen address, e.g. 127.0.0.1:8080")
	driver := fs.String("db-driver", getEnv("DB_DRIVER", ""), "database driver: postgres | sqlite")
	dbPath := fs.String("db-path", getEnv("DB_PATH", ""), "sqlite file path (embedded mode)")
	forwardTarget := fs.String("forward-target", getEnv("FORWARD_TARGET", ""), "remote BE base URL to mirror writes to (embedded mode)")
	forwardToken := fs.String("forward-token", getEnv("FORWARD_TOKEN", ""), "JWT used when forwarding to remote BE (embedded mode)")

	// Parse and silently discard unknown flags — keeps us friendly when run
	// via `go run` with trailing args, tests, or Tauri sidecar launchers.
	_ = fs.Parse(os.Args[1:])

	cfg := &Config{
		Mode:      normalizeMode(*mode),
		Port:      getEnv("PORT", "8085"),
		BindAddr:  *bind,
		JWTSecret:  getEnv("JWT_SECRET", "patchman-jwt-secret-change-in-production"),
		SessionTTL: getDurationEnv("SESSION_TTL", 30*24*time.Hour),
		AppName:    getEnv("APP_NAME", "PatchMan"),
		DBHost:    getEnv("DB_HOST", "localhost"),
		DBPort:    getEnv("DB_PORT", "5432"),
		DBName:    getEnv("DB_NAME", "getman"),
		DBUser:    getEnv("DB_USER", "umds"),
		DBPass:    getEnv("DB_PASSWORD", "secret"),
		DBSSLMode: getEnv("DB_SSLMODE", "disable"),
	}

	cfg.DBDriver = resolveDriver(*driver, cfg.Mode)
	cfg.DBPath = resolveDBPath(*dbPath, cfg.DBDriver)
	cfg.ForwardTarget = *forwardTarget
	cfg.ForwardToken = *forwardToken

	cfg.GitLabBaseURL = getEnv("GITLAB_BASE_URL", "https://gitlab.com")
	cfg.GitLabClientID = getEnv("GITLAB_OAUTH_CLIENT_ID", "")
	cfg.GitLabClientSecret = getEnv("GITLAB_OAUTH_CLIENT_SECRET", "")
	cfg.GitLabTokenEncKey = getEnv("GITLAB_TOKEN_ENC_KEY", "")

	cfg.SlackErrorWebhookURL = getEnv("SLACK_ERROR_WEBHOOK_URL", "")

	cfg.RealtimeAllowedOrigins = getEnv("REALTIME_ALLOWED_ORIGINS", "")

	cfg.AllowedEmailDomains = parseEmailDomains(getEnv("ALLOWED_EMAIL_DOMAINS", ""))
	cfg.DefaultAdminEmail = strings.ToLower(strings.TrimSpace(getEnv("DEFAULT_ADMIN_EMAIL", "")))
	cfg.AllowedRedirectOrigins = parseCSV(getEnv("ALLOWED_REDIRECT_ORIGINS", ""))

	return cfg
}

// EmailAllowed reports whether the given address may register / sign in.
// An empty allow-list means "any domain accepted" (open install).
func (c *Config) EmailAllowed(email string) bool {
	if len(c.AllowedEmailDomains) == 0 {
		return true
	}
	at := strings.LastIndexByte(email, '@')
	if at < 0 || at == len(email)-1 {
		return false
	}
	domain := strings.ToLower(email[at+1:])
	for _, d := range c.AllowedEmailDomains {
		if domain == d {
			return true
		}
	}
	return false
}

// EmailDomainsLabel renders the allow-list for user-facing error messages
// ("Only @example.com email addresses are allowed"). Returns "" when no
// restriction is in force so callers can branch on it.
func (c *Config) EmailDomainsLabel() string {
	if len(c.AllowedEmailDomains) == 0 {
		return ""
	}
	parts := make([]string, len(c.AllowedEmailDomains))
	for i, d := range c.AllowedEmailDomains {
		parts[i] = "@" + d
	}
	return strings.Join(parts, ", ")
}

// IsDefaultAdmin returns true when `email` matches the configured
// bootstrap admin (case-insensitive). Always false when no admin email
// is configured — clients with no bootstrap admin shouldn't accidentally
// match the empty-string sentinel.
func (c *Config) IsDefaultAdmin(email string) bool {
	if c.DefaultAdminEmail == "" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(email), c.DefaultAdminEmail)
}

// parseEmailDomains splits a comma-separated env into a lowercased
// slice. Accepts both `example.com` and `@example.com` forms.
func parseEmailDomains(raw string) []string {
	parts := parseCSV(raw)
	out := parts[:0]
	for _, p := range parts {
		d := strings.ToLower(strings.TrimPrefix(p, "@"))
		if d != "" {
			out = append(out, d)
		}
	}
	return out
}

func parseCSV(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// GitLabEnabled reports whether the GitLab integration has all the env it
// needs to function. The route handlers should refuse with 503 when this
// returns false instead of trying to call GitLab without credentials.
func (c *Config) GitLabEnabled() bool {
	return c.GitLabClientID != "" && c.GitLabClientSecret != "" && c.GitLabTokenEncKey != ""
}

// ForwardEnabled reports whether upstream mirroring should run. Both target
// and token must be set — a target without a token cannot authenticate.
func (c *Config) ForwardEnabled() bool {
	return c.Mode == ModeEmbedded && c.ForwardTarget != "" && c.ForwardToken != ""
}

// DSN returns the data source string appropriate for the configured driver.
func (c *Config) DSN() string {
	if c.DBDriver == DriverSQLite {
		return c.DBPath
	}
	return fmt.Sprintf("host=%s user=%s password=%s dbname=%s port=%s sslmode=%s",
		c.DBHost, c.DBUser, c.DBPass, c.DBName, c.DBPort, c.DBSSLMode)
}

// Listen returns "host:port" to bind to. --bind wins, otherwise :PORT.
func (c *Config) Listen() string {
	if c.BindAddr != "" {
		return c.BindAddr
	}
	port := c.Port
	if port == "" {
		port = "8080"
	}
	return ":" + port
}

// IsEmbedded is true when the backend runs bundled inside the desktop app.
// Embedded mode skips JWT auth and pins all records to a single local user.
func (c *Config) IsEmbedded() bool {
	return c.Mode == ModeEmbedded
}

func normalizeMode(m string) string {
	if m == ModeEmbedded {
		return ModeEmbedded
	}
	return ModeServer
}

func resolveDriver(driver, mode string) string {
	if driver == DriverSQLite || driver == DriverPostgres {
		return driver
	}
	if mode == ModeEmbedded {
		return DriverSQLite
	}
	return DriverPostgres
}

func resolveDBPath(explicit, driver string) string {
	if driver != DriverSQLite {
		return ""
	}
	if explicit != "" {
		return explicit
	}
	// Default: ./patchman.db next to the binary's CWD. The Tauri sidecar
	// wrapper is responsible for pointing this at the app data directory.
	abs, err := filepath.Abs("patchman.db")
	if err != nil {
		return "patchman.db"
	}
	return abs
}

func getEnv(key, fallback string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return fallback
}

// getDurationEnv parses a Go duration string (e.g. "720h", "45m") from env,
// falling back to the default on an unset or malformed value. A malformed
// value logs a warning rather than crashing — a typo in SESSION_TTL should
// not take the server down.
func getDurationEnv(key string, fallback time.Duration) time.Duration {
	val, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(val) == "" {
		return fallback
	}
	d, err := time.ParseDuration(strings.TrimSpace(val))
	if err != nil || d <= 0 {
		log.Printf("config: ignoring invalid %s=%q (%v); using %s", key, val, err, fallback)
		return fallback
	}
	return d
}
