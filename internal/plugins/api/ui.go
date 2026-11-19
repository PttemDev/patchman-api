package api

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"patchman-be/internal/models"
	"patchman-be/internal/plugins/manifest"
)

// Custom plugin UI (PLUGIN_MARKETPLACE_RFC §16.3, ADR-2 amendment A): the
// ui/ bundle is served from the backend's own origin under /plugin-ui, with
// a CSP that sandboxes the page (opaque origin even if opened top-level),
// lets it load only its own files and reach no network at all. The app talks
// to it only through a MessageChannel (FE pluginBridge).

// uiCSP is the policy of every plugin UI response. frame-ancestors is the
// app's own origins (filled per service).
const uiCSPBase = "sandbox allow-scripts; default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob:; font-src 'self' data:; media-src 'self' data: blob:; connect-src 'none'; " +
	"frame-src 'none'; worker-src 'none'; child-src 'none'; object-src 'none'; manifest-src 'none'; " +
	"base-uri 'none'; form-action 'none'"

// DefaultFrameAncestors are the desktop webview origins.
var DefaultFrameAncestors = []string{"tauri://localhost", "http://tauri.localhost", "https://tauri.localhost"}

// guardPath is served by PatchMan itself (a leading dot can never be a
// bundle path) and is injected as the first script of every HTML page.
const guardPath = ".pm/guard.js"

// guardJS removes APIs that reach the network around CSP: WebRTC (STUN/ICE
// traffic is not governed by connect-src). Plugin code runs after it and
// cannot get the constructors back: frame-src/child-src 'none' and the
// sandbox leave it no fresh realm (checked by the hostile UI probe).
const guardJS = `(function(){var k=["RTCPeerConnection","webkitRTCPeerConnection","RTCDataChannel","RTCRtpSender","RTCRtpReceiver","RTCRtpTransceiver","RTCIceCandidate","RTCSessionDescription","RTCDTMFSender","RTCCertificate"];for(var i=0;i<k.length;i++){try{Object.defineProperty(window,k[i],{value:undefined,writable:false,configurable:false});}catch(e){}}})();`

var doctypeRe = regexp.MustCompile(`(?is)^\s*<!doctype[^>]*>`)

// injectGuard puts the guard script first: right after the doctype (a
// script before <html> is parsed into <head>, ahead of anything the plugin
// wrote).
func injectGuard(html []byte) []byte {
	tag := []byte(`<script src="` + guardPath + `"></script>`)
	if loc := doctypeRe.FindIndex(html); loc != nil {
		out := append([]byte{}, html[:loc[1]]...)
		out = append(out, tag...)
		return append(out, html[loc[1]:]...)
	}
	return append(tag, html...)
}

func uiRev(row models.Plugin) string { return strconv.FormatInt(row.UpdatedAt.UnixNano(), 36) }

// readUIBundle loads ui/ when the manifest declares a custom UI entry.
// Regular files only (no symlinks), clean paths, allowed types, capped.
func readUIBundle(dir string, m *manifest.Manifest) ([]models.PluginUIFile, error) {
	if m.UI == nil || m.UI.Entry == "" {
		return nil, nil
	}
	root := filepath.Join(dir, "ui")
	var out []models.PluginUIFile
	var total int64
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s: symlinks are not allowed", rel)
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: not a regular file", rel)
		}
		if !manifest.UIPathOK(rel) {
			return fmt.Errorf("%s: path or file type not allowed", rel)
		}
		if len(out) >= manifest.MaxUIFiles {
			return fmt.Errorf("more than %d files", manifest.MaxUIFiles)
		}
		b, err := readCapped(p, manifest.MaxUIBytes)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		total += int64(len(b))
		if total > manifest.MaxUIBytes {
			return fmt.Errorf("bundle over %d bytes", manifest.MaxUIBytes)
		}
		out = append(out, models.PluginUIFile{PluginID: m.ID, Path: rel, Data: b})
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("ui/ folder not found")
		}
		return nil, err
	}
	entry := strings.TrimPrefix(m.UI.Entry, "ui/")
	for _, f := range out {
		if f.Path == entry {
			return out, nil
		}
	}
	return nil, fmt.Errorf("entry %s not found in ui/", m.UI.Entry)
}

// MountUI registers GET /plugin-ui/:pluginId/:rev/*path on the engine root
// (outside /api auth: the sandboxed iframe carries no credentials).
// frameAncestors are the origins allowed to embed it.
func (s *Service) MountUI(r *gin.Engine, frameAncestors []string) {
	csp := uiCSPBase + "; frame-ancestors " + strings.Join(frameAncestors, " ")
	r.GET("/plugin-ui/:pluginId/:rev/*path", func(c *gin.Context) { s.serveUI(c, csp) })
}

func (s *Service) serveUI(c *gin.Context, csp string) {
	h := c.Writer.Header()
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	p, ok := s.lookup(c.Param("pluginId"), true)
	if !ok || p.m.UI == nil || p.m.UI.Entry == "" || c.Param("rev") != uiRev(p.row) {
		c.String(http.StatusNotFound, "not found")
		return
	}
	rel := strings.TrimPrefix(c.Param("path"), "/")
	// The guard is referenced relative to the page, so it is found under
	// any directory of the bundle.
	if rel == guardPath || strings.HasSuffix(rel, "/"+guardPath) {
		c.Data(http.StatusOK, "text/javascript; charset=utf-8", []byte(guardJS))
		return
	}
	if !manifest.UIPathOK(rel) {
		c.String(http.StatusNotFound, "not found")
		return
	}
	var f models.PluginUIFile
	if err := s.db.First(&f, "plugin_id = ? AND path = ?", p.row.ID, rel).Error; err != nil {
		c.String(http.StatusNotFound, "not found")
		return
	}
	ext := strings.ToLower(rel[strings.LastIndex(rel, "."):])
	data := f.Data
	if ext == ".html" {
		data = injectGuard(data)
	}
	c.Data(http.StatusOK, manifest.UIContentTypes[ext], data)
}
