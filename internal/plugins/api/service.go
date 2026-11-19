// Package api is the plugin backbone's HTTP surface (PLUGIN_MARKETPLACE_RFC
// §6.2, §15): installs, plugin connections with grants, op calls, the step-up
// registry hooks and the MCP tools of installed plugins.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"patchman-be/internal/mcp"
	"patchman-be/internal/mcpscope"
	"patchman-be/internal/middleware"
	"patchman-be/internal/models"
	"patchman-be/internal/plugins/manifest"
	"patchman-be/internal/plugins/pkgsig"
	"patchman-be/internal/plugins/runtime"
	"patchman-be/internal/services"
)

// Options configure the plugin subsystem.
type Options struct {
	// DevInstall allows installing from a local folder (embedded mode with
	// PATCHMAN_PLUGIN_DEV=1). Off = no install, enable or uninstall at all:
	// P3 has no signed source yet (RFC invariant 5).
	DevInstall bool
	// Manage allows signed installs, enable/disable and uninstall (the
	// desktop's embedded backend). Off on the server until P5.
	Manage bool
	// EnvTrustedKeys are publisher keys from the environment (read-only).
	EnvTrustedKeys []pkgsig.TrustedKey
	// AllowHTTPSources lets tests serve packages over plain http.
	AllowHTTPSources bool
	// CacheDir is the root of the per-plugin compilation caches ("" = memory).
	CacheDir string
	// RestrictPrivate / PrivateAllow: hosted egress policy (server mode).
	RestrictPrivate bool
	PrivateAllow    []netip.Prefix
	// RuntimeOptions overrides (tests); CacheDir/Restrict* above win.
	Runtime runtime.Options
}

// Service owns the runtime and an in-memory view of installed plugins (the
// step-up matcher and the MCP tool list read it on every request).
type Service struct {
	db     *gorm.DB
	cipher *services.TokenCipher
	rt     *runtime.Runtime
	opts   Options

	mu        sync.RWMutex
	installed map[string]*installed
}

type installed struct {
	row models.Plugin
	m   *manifest.Manifest
}

// New builds the service and loads the installed set.
func New(db *gorm.DB, cipher *services.TokenCipher, o Options) (*Service, error) {
	s := &Service{db: db, cipher: cipher, opts: o, installed: map[string]*installed{}}
	ro := o.Runtime
	ro.CacheDir = o.CacheDir
	ro.RestrictPrivate = o.RestrictPrivate
	ro.PrivateAllow = o.PrivateAllow
	if ro.Log == nil {
		ro.Log = func(pluginID, connID, level, msg string) {
			if level == "warn" || level == "error" {
				log.Printf("plugin %s [%s] %s: %s", pluginID, connID, level, msg)
			}
		}
	}
	s.rt = runtime.New(s.load, ro)
	if err := s.refresh(); err != nil {
		return nil, err
	}
	return s, nil
}

// Close releases the runtime.
func (s *Service) Close() { s.rt.Close() }

// Runtime exposes the runtime (tests, diagnostics).
func (s *Service) Runtime() *runtime.Runtime { return s.rt }

func (s *Service) refresh() error {
	var rows []models.Plugin
	if err := s.db.Find(&rows).Error; err != nil {
		return err
	}
	next := map[string]*installed{}
	for _, r := range rows {
		m, err := manifest.Parse([]byte(r.ManifestJSON))
		if err != nil {
			// A manifest the current lint rejects stays installed but inert.
			log.Printf("plugin %s: stored manifest no longer valid: %v", r.ID, err)
			continue
		}
		next[r.ID] = &installed{row: r, m: m}
	}
	s.mu.Lock()
	s.installed = next
	s.mu.Unlock()
	return nil
}

// lookup returns an installed plugin; enabledOnly filters disabled ones.
func (s *Service) lookup(id string, enabledOnly bool) (*installed, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.installed[id]
	if !ok || (enabledOnly && !p.row.Enabled) {
		return nil, false
	}
	return p, true
}

func (s *Service) snapshot() []*installed {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*installed, 0, len(s.installed))
	for _, p := range s.installed {
		out = append(out, p)
	}
	return out
}

// load is the runtime's Loader: enabled plugins only.
func (s *Service) load(ctx context.Context, id string) (*manifest.Manifest, []byte, string, error) {
	p, ok := s.lookup(id, true)
	if !ok {
		return nil, nil, "", runtime.ErrNotFound
	}
	var blob models.PluginBlob
	if err := s.db.WithContext(ctx).First(&blob, "plugin_id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, "", runtime.ErrNotFound
		}
		return nil, nil, "", err
	}
	return p.m, blob.Wasm, p.row.WasmSHA256, nil
}

// opPath matches "/api/plugins/<pluginId>/<connId>/ops/<op>".
func opPath(path string) (pluginID, op string, ok bool) {
	rest, found := strings.CutPrefix(path, "/api/plugins/")
	if !found {
		return "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 4 || parts[2] != "ops" || parts[0] == "" || parts[1] == "" || parts[3] == "" {
		return "", "", false
	}
	return parts[0], parts[3], true
}

// sensitive is the LocalGate matcher: a POST to a sensitive op of an
// installed plugin needs a fresh step-up.
func (s *Service) sensitive(method, path string) bool {
	if method != "POST" {
		return false
	}
	id, op, ok := opPath(path)
	if !ok {
		return false
	}
	p, ok := s.lookup(id, false)
	if !ok {
		return false
	}
	return p.m.Ops[op].Sensitive
}

// Mount registers the routes and the gate/MCP hooks. Call once.
func (s *Service) Mount(api *gin.RouterGroup) {
	middleware.RegisterSensitiveRoute("POST", "/api/plugins/dev-install")
	middleware.RegisterSensitiveRoute("POST", "/api/plugins/install")
	middleware.RegisterSensitiveRoute("POST", "/api/plugins/trusted-keys")
	middleware.RegisterSensitiveRoute("DELETE", "/api/plugins/trusted-keys/:keyId")
	middleware.RegisterSensitiveRoute("DELETE", "/api/plugins/:pluginId")
	middleware.RegisterSensitiveRoute("POST", "/api/plugin-connections/:pluginId/:id/reveal-secret")
	middleware.RegisterSensitiveMatcher(s.sensitive)
	mcpscope.PluginInstalled = func(id string) bool { _, ok := s.lookup(id, false); return ok }
	mcp.PluginTools = s.registerMCPTools

	api.GET("/plugins", s.listPlugins)
	api.GET("/plugins/:pluginId", s.getPlugin)
	api.POST("/plugins/dev-install", s.devInstall)
	api.POST("/plugins/install/preview", s.installPreview)
	api.POST("/plugins/install", s.installSigned)
	api.GET("/plugins/trusted-keys", s.listTrustedKeys)
	api.POST("/plugins/trusted-keys", s.addTrustedKey)
	api.DELETE("/plugins/trusted-keys/:keyId", s.deleteTrustedKey)
	api.PATCH("/plugins/:pluginId", s.setEnabled)
	api.DELETE("/plugins/:pluginId", s.uninstall)
	api.POST("/plugins/:pluginId/:connId/ops/:op", s.callOp)

	api.POST("/plugin-connections/:pluginId", s.createConn)
	api.GET("/plugin-connections/:pluginId", s.listConns)
	api.POST("/plugin-connections/:pluginId/test", s.testConn)
	api.GET("/plugin-connections/:pluginId/:id", s.getConn)
	api.PATCH("/plugin-connections/:pluginId/:id", s.updateConn)
	api.DELETE("/plugin-connections/:pluginId/:id", s.deleteConn)
	api.POST("/plugin-connections/:pluginId/:id/test", s.testConn)
	api.POST("/plugin-connections/:pluginId/:id/reveal-secret", s.revealSecret)
	api.GET("/plugin-connections/:pluginId/:id/grants", s.listGrants)
	api.POST("/plugin-connections/:pluginId/:id/grants", s.addGrant)
	api.DELETE("/plugin-connections/:pluginId/:id/grants/:grantId", s.deleteGrant)
}

const mcpClientHeader = "X-PatchMan-Client"

func (s *Service) audit(c *gin.Context, a models.PluginActivity) {
	a.ID = uuid.New().String()
	a.ExecutedAt = time.Now()
	if a.UserID == "" {
		a.UserID = c.GetString("userID")
	}
	if c.GetHeader(mcpClientHeader) == "mcp" {
		a.Client = "mcp"
	}
	if err := s.db.Create(&a).Error; err != nil {
		log.Printf("plugin audit: %v", err)
	}
}

func detail(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func errf(c *gin.Context, code int, f string, a ...any) {
	c.JSON(code, gin.H{"error": fmt.Sprintf(f, a...)})
}
