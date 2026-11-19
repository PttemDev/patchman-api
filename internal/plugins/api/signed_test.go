package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"patchman-be/internal/middleware"
	"patchman-be/internal/models"
	"patchman-be/internal/plugins/pkgsig"
)

// signDir signs a package folder like `pm-plugin sign` would.
func signDir(t *testing.T, dir string, priv ed25519.PrivateKey, extra ...string) {
	t.Helper()
	files := map[string][]byte{}
	for _, f := range append([]string{"manifest.json", "backend.wasm"}, extra...) {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		files[f] = b
	}
	var m struct{ ID, Version string }
	json.Unmarshal(files["manifest.json"], &m)
	out, _ := json.Marshal(pkgsig.Sign(priv, m.ID, m.Version, files))
	os.WriteFile(filepath.Join(dir, pkgsig.FileName), out, 0o644)
}

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func (h *harness) trust(name string, pub ed25519.PublicKey) {
	h.t.Helper()
	code, out := h.do("owner", "POST", "/api/plugins/trusted-keys", gin.H{"name": name, "publicKey": base64.StdEncoding.EncodeToString(pub)})
	h.must(code, 201, out, "trust key")
}

func TestSignedInstall(t *testing.T) {
	// Manage on, dev install OFF: signed packages install, unsigned do not.
	h := newHarness(t, true)
	h.svc.opts.DevInstall = false
	pub, priv := newKey(t)
	_, evilPriv := newKey(t)

	dir := pluginDir(t, "dev.test.signed", "1.0.0", "", demoWasm)
	signDir(t, dir, priv)
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()

	code, out := h.do("owner", "POST", "/api/plugins/install/preview", gin.H{"source": dir})
	h.must(code, 400, out, "no trusted keys yet")
	h.trust("Jane", pub)

	code, out = h.do("owner", "POST", "/api/plugins/install/preview", gin.H{"source": srv.URL})
	h.must(code, 200, out, "preview from a URL")
	if dataField(out, "signerName") != "Jane" || dataField(out, "version") != "1.0.0" {
		t.Errorf("preview = %v", out)
	}
	var n int64
	h.db.Model(&models.Plugin{}).Count(&n)
	if n != 0 {
		t.Fatal("preview installed the plugin")
	}
	code, out = h.do("owner", "POST", "/api/plugins/install", gin.H{"source": srv.URL})
	h.must(code, 200, out, "install from a URL")
	if dataField(out, "source") != "signed" || dataField(out, "signer") != pkgsig.KeyID(pub) {
		t.Errorf("installed = %v", out)
	}
	id := h.conn("owner", "dev.test.signed", false)
	code, out = h.do("owner", "POST", "/api/plugins/dev.test.signed/"+id+"/ops/whoami", nil)
	h.must(code, 200, out, "signed plugin runs")

	// Tampering after signing.
	tampered := pluginDir(t, "dev.test.signed", "1.1.0", "", demoWasm)
	signDir(t, tampered, priv)
	os.WriteFile(filepath.Join(tampered, "backend.wasm"), append(append([]byte{}, demoWasm...), 0), 0o644)
	code, out = h.do("owner", "POST", "/api/plugins/install", gin.H{"source": tampered})
	h.must(code, 400, out, "changed backend.wasm")
	if !strings.Contains(fmt.Sprint(out["error"]), "does not match its signed hash") {
		t.Errorf("tamper error = %v", out["error"])
	}

	// Signed by someone else.
	other := pluginDir(t, "dev.test.signed", "1.2.0", "", demoWasm)
	signDir(t, other, evilPriv)
	code, out = h.do("owner", "POST", "/api/plugins/install", gin.H{"source": other})
	h.must(code, 403, out, "untrusted signer")

	// Unsigned folder.
	code, out = h.do("owner", "POST", "/api/plugins/install", gin.H{"source": pluginDir(t, "dev.test.unsigned", "1.0.0", "", demoWasm)})
	h.must(code, 400, out, "unsigned package")

	// Downgrade refused; upgrade lists new permissions.
	older := pluginDir(t, "dev.test.signed", "0.9.0", "", demoWasm)
	signDir(t, older, priv)
	code, out = h.do("owner", "POST", "/api/plugins/install", gin.H{"source": older})
	h.must(code, 409, out, "downgrade")
	newer := pluginDir(t, "dev.test.signed", "1.3.0", `, {"host": "api.example.com", "ports": [443]}`, demoWasm)
	signDir(t, newer, priv)
	code, out = h.do("owner", "POST", "/api/plugins/install/preview", gin.H{"source": newer})
	h.must(code, 200, out, "update preview")
	if dataField(out, "installedVersion") != "1.0.0" || !strings.Contains(fmt.Sprint(dataField(out, "addedPermissions")), "api.example.com") {
		t.Errorf("update preview = %v", out)
	}
	code, out = h.do("owner", "POST", "/api/plugins/install", gin.H{"source": newer})
	h.must(code, 200, out, "update")

	// Manifest/signature mismatch: sign 2.0.0, then change the manifest
	// version and re-hash it into the file list — the signature breaks.
	mism := pluginDir(t, "dev.test.signed", "2.0.0", "", demoWasm)
	signDir(t, mism, priv)
	raw, _ := os.ReadFile(filepath.Join(mism, "manifest.json"))
	os.WriteFile(filepath.Join(mism, "manifest.json"), []byte(strings.Replace(string(raw), `"2.0.0"`, `"2.0.1"`, 1)), 0o644)
	code, out = h.do("owner", "POST", "/api/plugins/install", gin.H{"source": mism})
	h.must(code, 400, out, "manifest changed after signing")

	// Step-up and people-only gates.
	if !middleware.IsSensitiveRoute("POST", "/api/plugins/install") || !middleware.IsSensitiveRoute("POST", "/api/plugins/trusted-keys") ||
		!middleware.IsSensitiveRoute("DELETE", "/api/plugins/trusted-keys/abc") || middleware.IsSensitiveRoute("POST", "/api/plugins/install/preview") {
		t.Error("sensitive route registration")
	}
	code, out = h.do("owner", "POST", "/api/plugins/trusted-keys", gin.H{"name": "x", "publicKey": base64.StdEncoding.EncodeToString(pub)}, middleware.WithInProcess(t.Context()))
	h.must(code, 403, out, "agent adds a key")

	// Removing the key blocks new installs; installed plugins keep working.
	code, out = h.do("owner", "DELETE", "/api/plugins/trusted-keys/"+pkgsig.KeyID(pub), nil)
	h.must(code, 200, out, "untrust")
	code, out = h.do("owner", "POST", "/api/plugins/install/preview", gin.H{"source": newer})
	h.must(code, 400, out, "no keys left")
	code, out = h.do("owner", "POST", "/api/plugins/dev.test.signed/"+id+"/ops/whoami", nil)
	h.must(code, 200, out, "installed plugin after untrust")
}

func TestSignedUIOnlyListedFiles(t *testing.T) {
	h := newHarness(t, true)
	pub, priv := newKey(t)
	h.trust("Jane", pub)
	dir := pluginDir(t, "dev.test.sui", "1.0.0", "", demoWasm)
	raw, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(strings.Replace(string(raw), `"mcp": {`, `"ui": {"entry": "ui/index.html"}, "mcp": {`, 1)), 0o644)
	os.MkdirAll(filepath.Join(dir, "ui"), 0o755)
	os.WriteFile(filepath.Join(dir, "ui", "index.html"), []byte("<p>signed</p>"), 0o644)
	signDir(t, dir, priv, "ui/index.html")
	os.WriteFile(filepath.Join(dir, "ui", "sneaky.js"), []byte("evil()"), 0o644) // added after signing
	code, out := h.do("owner", "POST", "/api/plugins/install", gin.H{"source": dir})
	h.must(code, 200, out, "install signed UI")
	var files []models.PluginUIFile
	h.db.Where("plugin_id = ?", "dev.test.sui").Find(&files)
	if len(files) != 1 || files[0].Path != "index.html" {
		t.Errorf("ui files = %v", files)
	}
}

func TestVersionOrder(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{{"1.0.0", "1.0.0", 0}, {"1.0.1", "1.0.0", 1}, {"1.10.0", "1.9.9", 1}, {"1.0.0-rc.1", "1.0.0", -1}, {"2.0.0", "10.0.0", -1}} {
		if got := versionOrder(c.a, c.b); got != c.want {
			t.Errorf("%s vs %s = %d", c.a, c.b, got)
		}
	}
}
