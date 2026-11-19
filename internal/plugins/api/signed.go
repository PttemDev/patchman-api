package api

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"patchman-be/internal/middleware"
	"patchman-be/internal/models"
	"patchman-be/internal/plugins/manifest"
	"patchman-be/internal/plugins/pkgsig"
	"patchman-be/internal/plugins/runtime"
)

// Signed installs (PLUGIN_MARKETPLACE_RFC §18): a package folder — a local
// path or a folder in GitHub / GitLab — whose SIGNATURE.json verifies against
// a trusted publisher key. Only the files the signature lists are read, each
// must match its signed hash, and the signed id/version must match the
// manifest. Unsigned packages stay dev-mode only.

const maxSignatureBytes = 1 << 20

type signedPkg struct {
	m      *manifest.Manifest
	wasm   []byte
	ui     []models.PluginUIFile
	signer *pkgsig.TrustedKey
	src    source
}

// trustedKeys merges the stored keys and the environment's.
func (s *Service) trustedKeys() ([]pkgsig.TrustedKey, error) {
	var rows []models.PluginTrustedKey
	if err := s.db.Order("created_at").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := append([]pkgsig.TrustedKey{}, s.opts.EnvTrustedKeys...)
	for _, r := range rows {
		k, err := pkgsig.ParsePublicKey(r.PublicKey)
		if err != nil {
			continue
		}
		out = append(out, pkgsig.TrustedKey{KeyID: r.KeyID, Name: r.Name, Key: k})
	}
	return out, nil
}

// signedPath reports whether a signed file path may be part of a package.
func signedPath(p string) bool {
	switch {
	case p == "manifest.json", p == "backend.wasm":
		return true
	case strings.HasPrefix(p, "ui/"):
		return manifest.UIPathOK(strings.TrimPrefix(p, "ui/"))
	}
	return false
}

// loadSigned fetches and verifies a package. Errors are user-facing.
func (s *Service) loadSigned(ctx context.Context, src source) (*signedPkg, error) {
	trusted, err := s.trustedKeys()
	if err != nil {
		return nil, errors.New("read trusted keys")
	}
	if len(trusted) == 0 {
		return nil, errors.New("no trusted publisher keys: add one in Settings → Plugins first")
	}
	raw, err := src.read(ctx, pkgsig.FileName, maxSignatureBytes)
	if err != nil {
		return nil, err
	}
	sig, err := pkgsig.Parse(raw)
	if err != nil {
		return nil, err
	}
	signer, err := sig.Verify(trusted)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(sig.Files))
	for p := range sig.Files {
		if !signedPath(p) {
			return nil, errors.New("signed file list has a path that cannot be part of a package: " + p)
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)
	if _, ok := sig.Files["manifest.json"]; !ok {
		return nil, errors.New("signed package has no manifest.json")
	}
	if _, ok := sig.Files["backend.wasm"]; !ok {
		return nil, errors.New("signed package has no backend.wasm")
	}
	pkg := &signedPkg{signer: signer, src: src}
	var uiTotal int64
	var uiFiles []models.PluginUIFile
	files := map[string][]byte{}
	for _, p := range paths {
		max := int64(manifest.MaxUIBytes)
		switch p {
		case "manifest.json":
			max = maxManifestBytes
		case "backend.wasm":
			max = manifest.MaxWasmBytes
		}
		b, err := src.read(ctx, p, max)
		if err != nil {
			return nil, err
		}
		if err := sig.CheckFile(p, b); err != nil {
			return nil, err
		}
		files[p] = b
		if strings.HasPrefix(p, "ui/") {
			uiTotal += int64(len(b))
			uiFiles = append(uiFiles, models.PluginUIFile{Path: strings.TrimPrefix(p, "ui/"), Data: b})
		}
	}
	if len(uiFiles) > manifest.MaxUIFiles || uiTotal > manifest.MaxUIBytes {
		return nil, errors.New("ui/ bundle is over the size limits")
	}
	m, err := manifest.Parse(files["manifest.json"])
	if err != nil {
		return nil, err
	}
	if m.ID != sig.ID || m.Version != sig.Version {
		return nil, errors.New("SIGNATURE.json is for " + sig.ID + "@" + sig.Version + ", the manifest is " + m.ID + "@" + m.Version)
	}
	if m.UI != nil && m.UI.Entry != "" {
		if _, ok := files[m.UI.Entry]; !ok {
			return nil, errors.New("ui.entry " + m.UI.Entry + " is not in the signed package")
		}
	} else {
		uiFiles = nil // no custom UI: ui/ files are not served
	}
	if err := runtime.Validate(ctx, m, files["backend.wasm"]); err != nil {
		return nil, errors.New("backend.wasm: " + err.Error())
	}
	pkg.m, pkg.wasm, pkg.ui = m, files["backend.wasm"], uiFiles
	return pkg, nil
}

type installInput struct {
	Source string `json:"source" binding:"required"`
	Token  string `json:"token"` // private repos; used for this request only
}

// prepare parses the input and loads the package (both preview and install).
func (s *Service) prepare(c *gin.Context) (*signedPkg, bool) {
	if !s.opts.Manage {
		errf(c, http.StatusForbidden, "Plugin installs are not available on this backend")
		return nil, false
	}
	var in installInput
	if err := c.ShouldBindJSON(&in); err != nil {
		errf(c, http.StatusBadRequest, "%v", err)
		return nil, false
	}
	src, err := parseSource(in.Source, in.Token, s.opts.AllowHTTPSources)
	if err != nil {
		errf(c, http.StatusBadRequest, "%v", err)
		return nil, false
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Minute)
	defer cancel()
	pkg, err := s.loadSigned(ctx, src)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, pkgsig.ErrUntrusted) {
			code = http.StatusForbidden
		}
		errf(c, code, "%v", err)
		return nil, false
	}
	return pkg, true
}

// versionOrder compares semver versions (-1, 0, 1): numeric core first, and
// a pre-release sorts before its release (1.0.0-rc.1 < 1.0.0). Pre-release
// identifiers compare as strings (enough for ordering updates).
func versionOrder(a, b string) int {
	core := func(v string) ([3]int, string) {
		pre := ""
		if i := strings.IndexByte(v, '-'); i >= 0 {
			v, pre = v[:i], v[i+1:]
		}
		var n [3]int
		for i, part := range strings.SplitN(v, ".", 3) {
			n[i], _ = strconv.Atoi(part)
		}
		return n, pre
	}
	na, pa := core(a)
	nb, pb := core(b)
	for i := 0; i < 3; i++ {
		if na[i] != nb[i] {
			if na[i] < nb[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case pa == pb:
		return 0
	case pa == "":
		return 1
	case pb == "":
		return -1
	case pa < pb:
		return -1
	}
	return 1
}

type previewDTO struct {
	pluginDTO
	SignerName       string   `json:"signerName"`
	InstalledVersion string   `json:"installedVersion,omitempty"`
	AddedPermissions []string `json:"addedPermissions"`
	Downgrade        bool     `json:"downgrade"`
	HasUI            bool     `json:"hasUI"`
}

func (s *Service) preview(pkg *signedPkg) previewDTO {
	p := &installed{row: models.Plugin{ID: pkg.m.ID, Version: pkg.m.Version, Source: "signed", SourcePath: pkg.src.location(), SignerKeyID: pkg.signer.KeyID, Enabled: true}, m: pkg.m}
	d := previewDTO{pluginDTO: toPluginDTO(p), SignerName: pkg.signer.Name, AddedPermissions: []string{}, HasUI: pkg.m.UI != nil && pkg.m.UI.Entry != ""}
	d.UIRev = ""
	if old, ok := s.lookup(pkg.m.ID, false); ok {
		d.InstalledVersion = old.row.Version
		d.AddedPermissions = append(d.AddedPermissions, permDiff(old.m, pkg.m)...)
		d.Downgrade = versionOrder(pkg.m.Version, old.row.Version) < 0
	}
	return d
}

// installPreview shows what a signed package is and would add, without
// installing it.
func (s *Service) installPreview(c *gin.Context) {
	pkg, ok := s.prepare(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": s.preview(pkg)})
}

// installSigned installs (or updates to) a signed package. Step-up gated.
func (s *Service) installSigned(c *gin.Context) {
	pkg, ok := s.prepare(c)
	if !ok {
		return
	}
	if old, exists := s.lookup(pkg.m.ID, false); exists && versionOrder(pkg.m.Version, old.row.Version) < 0 {
		errf(c, http.StatusConflict, "%s %s is older than the installed %s; uninstall first to go back", pkg.m.ID, pkg.m.Version, old.row.Version)
		return
	}
	s.commitInstall(c, pkg.m, pkg.wasm, pkg.ui, "signed", pkg.src.location(), pkg.signer.KeyID)
}

// ── trusted publisher keys ──────────────────────────────────────────

type trustedKeyDTO struct {
	KeyID     string `json:"keyId"`
	Name      string `json:"name"`
	PublicKey string `json:"publicKey"`
	FromEnv   bool   `json:"fromEnv"`
	CreatedAt string `json:"createdAt,omitempty"`
}

func (s *Service) listTrustedKeys(c *gin.Context) {
	out := []trustedKeyDTO{}
	for _, k := range s.opts.EnvTrustedKeys {
		out = append(out, trustedKeyDTO{KeyID: k.KeyID, Name: k.Name, PublicKey: encodeKey(k.Key), FromEnv: true})
	}
	var rows []models.PluginTrustedKey
	s.db.Order("created_at").Find(&rows)
	for _, r := range rows {
		out = append(out, trustedKeyDTO{KeyID: r.KeyID, Name: r.Name, PublicKey: r.PublicKey, CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339)})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (s *Service) addTrustedKey(c *gin.Context) {
	if !s.opts.Manage {
		errf(c, http.StatusForbidden, "Plugin management is off")
		return
	}
	if middleware.IsInProcessRequest(c.Request.Context()) {
		errf(c, http.StatusForbidden, "Trusted keys are managed by people only")
		return
	}
	var in struct {
		Name      string `json:"name" binding:"required"`
		PublicKey string `json:"publicKey" binding:"required"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		errf(c, http.StatusBadRequest, "%v", err)
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > 120 {
		errf(c, http.StatusBadRequest, "name is required (≤120 chars)")
		return
	}
	k, err := pkgsig.ParsePublicKey(in.PublicKey)
	if err != nil {
		errf(c, http.StatusBadRequest, "%v", err)
		return
	}
	row := models.PluginTrustedKey{KeyID: pkgsig.KeyID(k), Name: name, PublicKey: encodeKey(k), AddedBy: c.GetString("userID"), CreatedAt: time.Now()}
	if err := s.db.Save(&row).Error; err != nil {
		errf(c, http.StatusInternalServerError, "save key")
		return
	}
	s.audit(c, models.PluginActivity{PluginID: "-", Action: "trust-key", DetailJSON: detail(gin.H{"keyId": row.KeyID, "name": name})})
	c.JSON(http.StatusCreated, gin.H{"data": trustedKeyDTO{KeyID: row.KeyID, Name: row.Name, PublicKey: row.PublicKey, CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339)}})
}

// deleteTrustedKey stops trusting a key for new installs and updates.
// Plugins it already signed stay installed.
func (s *Service) deleteTrustedKey(c *gin.Context) {
	if !s.opts.Manage {
		errf(c, http.StatusForbidden, "Plugin management is off")
		return
	}
	if middleware.IsInProcessRequest(c.Request.Context()) {
		errf(c, http.StatusForbidden, "Trusted keys are managed by people only")
		return
	}
	res := s.db.Where("key_id = ?", c.Param("keyId")).Delete(&models.PluginTrustedKey{})
	if res.RowsAffected == 0 {
		errf(c, http.StatusNotFound, "Key not found")
		return
	}
	s.audit(c, models.PluginActivity{PluginID: "-", Action: "untrust-key", DetailJSON: detail(gin.H{"keyId": c.Param("keyId")})})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"deleted": true}})
}

func encodeKey(k []byte) string { return base64.StdEncoding.EncodeToString(k) }
