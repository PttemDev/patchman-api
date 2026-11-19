package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"patchman-be/internal/models"
	"patchman-be/internal/plugins/manifest"
	"patchman-be/internal/plugins/runtime"
)

const maxManifestBytes = 1 << 20

type pluginDTO struct {
	ID          string                 `json:"id"`
	Version     string                 `json:"version"`
	Name        string                 `json:"name"`
	Publisher   string                 `json:"publisher"`
	Description string                 `json:"description,omitempty"`
	Source      string                 `json:"source"`
	Enabled     bool                   `json:"enabled"`
	WasmSHA256  string                 `json:"wasmSha256"`
	Connection  manifest.Connection    `json:"connection"`
	Permissions manifest.Permissions   `json:"permissions"`
	Limits      manifest.Limits        `json:"limits"`
	Ops         map[string]manifest.Op `json:"ops"`
	MCPTools    []string               `json:"mcpTools,omitempty"`
	UI          *manifest.UI           `json:"ui,omitempty"`
	UpdatedAt   string                 `json:"updatedAt"`
	// UIRev is the custom UI's cache key: /plugin-ui/<id>/<uiRev>/<path>.
	UIRev string `json:"uiRev,omitempty"`
	// Signed installs: where it came from and who signed it.
	SourcePath string `json:"sourcePath,omitempty"`
	Signer     string `json:"signer,omitempty"`
}

func toPluginDTO(p *installed) pluginDTO {
	d := pluginDTO{
		ID: p.row.ID, Version: p.row.Version, Name: p.m.Name, Publisher: p.m.Publisher,
		Description: p.m.Description, Source: p.row.Source, Enabled: p.row.Enabled,
		WasmSHA256: p.row.WasmSHA256, Connection: p.m.Connection, Permissions: p.m.Permissions,
		Limits: p.m.Limits, Ops: p.m.Ops, UI: p.m.UI, UpdatedAt: p.row.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	for _, t := range p.m.MCP.Tools {
		d.MCPTools = append(d.MCPTools, p.m.ToolName(t))
	}
	if p.m.UI != nil && p.m.UI.Entry != "" {
		d.UIRev = uiRev(p.row)
	}
	if p.row.Source == "signed" {
		d.SourcePath = p.row.SourcePath
		d.Signer = p.row.SignerKeyID
	}
	return d
}

func (s *Service) listPlugins(c *gin.Context) {
	ps := s.snapshot()
	sort.Slice(ps, func(i, j int) bool { return ps[i].row.ID < ps[j].row.ID })
	out := make([]pluginDTO, 0, len(ps))
	for _, p := range ps {
		out = append(out, toPluginDTO(p))
	}
	c.JSON(http.StatusOK, gin.H{"data": out, "devInstall": s.opts.DevInstall, "manage": s.opts.Manage})
}

func (s *Service) getPlugin(c *gin.Context) {
	p, ok := s.lookup(c.Param("pluginId"), false)
	if !ok {
		errf(c, http.StatusNotFound, "Plugin not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": toPluginDTO(p)})
}

func readCapped(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errTooLarge
	}
	return b, nil
}

type sizeErr struct{}

func (sizeErr) Error() string { return "file too large" }

var errTooLarge error = sizeErr{}

// permDiff lists what a reinstall adds compared with the installed version,
// so the UI (P4) can ask for consent on widened permissions.
func permDiff(old, next *manifest.Manifest) []string {
	var out []string
	if old == nil {
		return nil
	}
	key := func(r manifest.NetRule) string { b, _ := json.Marshal(r); return string(b) }
	have := map[string]bool{}
	for _, r := range old.Permissions.Net {
		have[key(r)] = true
	}
	for _, r := range next.Permissions.Net {
		if !have[key(r)] {
			out = append(out, "net "+key(r))
		}
	}
	for _, f := range next.Connection.SecretFields {
		if !slices.Contains(old.Connection.SecretFields, f) {
			out = append(out, "secret "+f)
		}
	}
	for name, op := range next.Ops {
		was, existed := old.Ops[name]
		if op.Write && (!existed || !was.Write) {
			out = append(out, "write op "+name)
		}
	}
	for _, t := range next.MCP.Tools {
		found := false
		for _, o := range old.MCP.Tools {
			if o == t {
				found = true
			}
		}
		if !found {
			out = append(out, "mcp tool "+next.ToolName(t))
		}
	}
	sort.Strings(out)
	return out
}

// devInstall installs or reinstalls from a local folder holding manifest.json
// and backend.wasm (the output of `pm plugin dev`). Step-up gated.
func (s *Service) devInstall(c *gin.Context) {
	if !s.opts.DevInstall {
		errf(c, http.StatusForbidden, "Plugin dev install is off (embedded mode with PATCHMAN_PLUGIN_DEV=1 only)")
		return
	}
	var in struct {
		Path string `json:"path" binding:"required"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		errf(c, http.StatusBadRequest, "%v", err)
		return
	}
	if !filepath.IsAbs(in.Path) {
		errf(c, http.StatusBadRequest, "path must be absolute")
		return
	}
	dir := filepath.Clean(in.Path)
	raw, err := readCapped(filepath.Join(dir, "manifest.json"), maxManifestBytes)
	if err != nil {
		errf(c, http.StatusBadRequest, "manifest.json: %v", err)
		return
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		errf(c, http.StatusBadRequest, "%v", err)
		return
	}
	wasm, err := readCapped(filepath.Join(dir, "backend.wasm"), manifest.MaxWasmBytes)
	if err != nil {
		errf(c, http.StatusBadRequest, "backend.wasm: %v", err)
		return
	}
	if err := runtime.Validate(c.Request.Context(), m, wasm); err != nil {
		errf(c, http.StatusBadRequest, "backend.wasm: %v", err)
		return
	}
	uiFiles, err := readUIBundle(dir, m)
	if err != nil {
		errf(c, http.StatusBadRequest, "ui: %v", err)
		return
	}
	s.commitInstall(c, m, wasm, uiFiles, "dev", dir, "")
}

// commitInstall stores a validated package and reloads it. source is "dev"
// or "signed"; signer is the key id for signed installs.
func (s *Service) commitInstall(c *gin.Context, m *manifest.Manifest, wasm []byte, uiFiles []models.PluginUIFile, source, location, signer string) {
	sum := sha256.Sum256(wasm)
	canon, _ := json.Marshal(m) // normalized hosts, applied defaults
	var oldM *manifest.Manifest
	if p, ok := s.lookup(m.ID, false); ok {
		oldM = p.m
	}
	row := models.Plugin{
		ID: m.ID, Version: m.Version, Name: m.Name, Publisher: m.Publisher,
		ManifestJSON: string(canon), WasmSHA256: hex.EncodeToString(sum[:]),
		Source: source, SourcePath: location, SignerKeyID: signer, Enabled: true, InstalledBy: c.GetString("userID"),
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(&row).Error; err != nil {
			return err
		}
		if err := tx.Where("plugin_id = ?", m.ID).Delete(&models.PluginUIFile{}).Error; err != nil {
			return err
		}
		for i := range uiFiles {
			uiFiles[i].PluginID = m.ID
			if err := tx.Create(&uiFiles[i]).Error; err != nil {
				return err
			}
		}
		return tx.Save(&models.PluginBlob{PluginID: m.ID, Wasm: wasm}).Error
	})
	if err != nil {
		errf(c, http.StatusInternalServerError, "save plugin")
		return
	}
	// Refresh first: a load that starts after Invalidate must already see
	// the new manifest (the runtime's generation check covers loads in flight).
	if err := s.refresh(); err != nil {
		errf(c, http.StatusInternalServerError, "reload plugins")
		return
	}
	s.rt.Invalidate(m.ID, true)
	diff := permDiff(oldM, m)
	s.audit(c, models.PluginActivity{PluginID: m.ID, PluginVersion: m.Version, Action: "install",
		DetailJSON: detail(gin.H{"source": source, "location": location, "signer": signer, "sha256": row.WasmSHA256, "addedPermissions": diff})})
	p, _ := s.lookup(m.ID, false)
	c.JSON(http.StatusOK, gin.H{"data": toPluginDTO(p), "addedPermissions": diff, "reinstall": oldM != nil})
}

func (s *Service) setEnabled(c *gin.Context) {
	if !s.opts.Manage {
		errf(c, http.StatusForbidden, "Plugin management is off")
		return
	}
	id := c.Param("pluginId")
	p, ok := s.lookup(id, false)
	if !ok {
		errf(c, http.StatusNotFound, "Plugin not found")
		return
	}
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || in.Enabled == nil {
		errf(c, http.StatusBadRequest, "enabled is required")
		return
	}
	if err := s.db.Model(&models.Plugin{}).Where("id = ?", id).Update("enabled", *in.Enabled).Error; err != nil {
		errf(c, http.StatusInternalServerError, "update plugin")
		return
	}
	_ = s.refresh()
	s.rt.Invalidate(id, false)
	action := "disable"
	if *in.Enabled {
		action = "enable"
	}
	s.audit(c, models.PluginActivity{PluginID: id, PluginVersion: p.row.Version, Action: action})
	p, _ = s.lookup(id, false)
	c.JSON(http.StatusOK, gin.H{"data": toPluginDTO(p)})
}

// uninstall removes the plugin, its wasm, its connections and their grants,
// its compiled code and compile cache. Audit rows stay (RFC §7).
func (s *Service) uninstall(c *gin.Context) {
	if !s.opts.Manage {
		errf(c, http.StatusForbidden, "Plugin management is off")
		return
	}
	id := c.Param("pluginId")
	p, ok := s.lookup(id, false)
	if !ok {
		errf(c, http.StatusNotFound, "Plugin not found")
		return
	}
	var removed int64
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("connection_id IN (?)", tx.Model(&models.PluginConnection{}).Select("id").Where("plugin_id = ?", id)).
			Delete(&models.PluginConnectionGrant{}).Error; err != nil {
			return err
		}
		res := tx.Where("plugin_id = ?", id).Delete(&models.PluginConnection{})
		if res.Error != nil {
			return res.Error
		}
		removed = res.RowsAffected
		if err := tx.Where("plugin_id = ?", id).Delete(&models.PluginBlob{}).Error; err != nil {
			return err
		}
		if err := tx.Where("plugin_id = ?", id).Delete(&models.PluginUIFile{}).Error; err != nil {
			return err
		}
		return tx.Where("id = ?", id).Delete(&models.Plugin{}).Error
	})
	if err != nil {
		errf(c, http.StatusInternalServerError, "uninstall plugin")
		return
	}
	_ = s.refresh()
	s.rt.Invalidate(id, true)
	s.audit(c, models.PluginActivity{PluginID: id, PluginVersion: p.row.Version, Action: "uninstall",
		DetailJSON: detail(gin.H{"connectionsRemoved": removed})})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "uninstalled", "connectionsRemoved": removed}})
}
