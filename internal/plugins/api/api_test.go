package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"gorm.io/gorm"

	"patchman-be/internal/authtoken"
	"patchman-be/internal/mcp"
	"patchman-be/internal/mcpscope"
	"patchman-be/internal/middleware"
	"patchman-be/internal/models"
	"patchman-be/internal/services"
)

var demoWasm []byte

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "pmapi")
	if err != nil {
		panic(err)
	}
	out := filepath.Join(dir, "demo.wasm")
	cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", out, "./cmd/demo")
	cmd.Dir = "../runtime/testdata/guest"
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "GOWORK=off")
	if b, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build demo guest: %v\n%s", err, b)
		os.Exit(1)
	}
	if demoWasm, err = os.ReadFile(out); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

const jwtSecret = "test-secret"

const demoManifest = `{
  "id": %q, "version": %q, "name": "Demo", "publisher": "test",
  "connection": {
    "schema": {"type": "object", "properties": {"host": {"type": "string"}, "token": {"type": "string"}}, "required": ["host", "token"]},
    "secretFields": ["token"]
  },
  "permissions": {"net": [{"from": "connection.host", "ports": [443]}%s]},
  "ops": {
    "echo": {"input": {"type": "object", "properties": {"msg": {"type": "string"}}}},
    "whoami": {}, "counter": {}, "test": {},
    "drop": {"write": true},
    "danger": {"sensitive": true}
  },
  "mcp": {"prefix": "demo", "tools": [
    {"name": "echo", "op": "echo", "description": "Echo the input."},
    {"name": "drop", "op": "drop", "description": "Drop things."}
  ]}
}`

func pluginDir(t *testing.T, id, version, extraNet string, wasm []byte) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(fmt.Sprintf(demoManifest, id, version, extraNet)), 0o600)
	os.WriteFile(filepath.Join(dir, "backend.wasm"), wasm, 0o600)
	return dir
}

type harness struct {
	t     *testing.T
	db    *gorm.DB
	r     *gin.Engine
	svc   *Service
	cache string
}

func newHarness(t *testing.T, devInstall bool) *harness {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:plugins_%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.User{}, &models.Team{}, &models.TeamMember{}, &models.Plugin{}, &models.PluginBlob{}, &models.PluginUIFile{}, &models.PluginTrustedKey{},
		&models.PluginConnection{}, &models.PluginConnectionGrant{}, &models.PluginActivity{}); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	rand.Read(key)
	cipher, err := services.NewTokenCipher(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	svc, err := New(db, cipher, Options{DevInstall: devInstall, Manage: devInstall, CacheDir: cache, AllowHTTPSources: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	api.Use(middleware.JWTAuth(jwtSecret, time.Hour))
	svc.Mount(api)
	svc.MountUI(r, DefaultFrameAncestors)
	t.Cleanup(func() { mcp.PluginTools = nil; mcpscope.PluginInstalled = nil })
	return &harness{t: t, db: db, r: r, svc: svc, cache: cache}
}

func (h *harness) do(user, method, path string, body any, ctx ...context.Context) (int, map[string]any) {
	h.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	if len(ctx) > 0 {
		req = req.WithContext(ctx[0])
	}
	tok, _ := authtoken.Mint(jwtSecret, user, time.Hour)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.r.ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (h *harness) must(code int, want int, out map[string]any, what string) {
	h.t.Helper()
	if code != want {
		h.t.Fatalf("%s: status %d want %d: %v", what, code, want, out)
	}
}

func (h *harness) install(id, version, extraNet string) map[string]any {
	h.t.Helper()
	code, out := h.do("owner", "POST", "/api/plugins/dev-install", gin.H{"path": pluginDir(h.t, id, version, extraNet, demoWasm)})
	h.must(code, 200, out, "install "+id)
	return out
}

func (h *harness) conn(user, pluginID string, allowWrite bool) string {
	h.t.Helper()
	code, out := h.do(user, "POST", "/api/plugin-connections/"+pluginID, gin.H{
		"name": "c", "config": gin.H{"host": "a.example.com"}, "secrets": gin.H{"token": "tok-" + user}, "allowWrite": allowWrite})
	h.must(code, 201, out, "create connection")
	return out["data"].(map[string]any)["id"].(string)
}

func dataField(out map[string]any, k string) any { return out["data"].(map[string]any)[k] }

func TestInstallGates(t *testing.T) {
	off := newHarness(t, false)
	code, out := off.do("owner", "POST", "/api/plugins/dev-install", gin.H{"path": pluginDir(t, "dev.test.demo", "1.0.0", "", demoWasm)})
	off.must(code, 403, out, "install with dev install off")

	h := newHarness(t, true)
	code, out = h.do("owner", "POST", "/api/plugins/dev-install", gin.H{"path": "relative/dir"})
	h.must(code, 400, out, "relative path")
	bad := pluginDir(t, "dev.test.demo", "1.0.0", "", demoWasm)
	os.WriteFile(filepath.Join(bad, "manifest.json"), []byte(`{"id":"x"}`), 0o600)
	code, out = h.do("owner", "POST", "/api/plugins/dev-install", gin.H{"path": bad})
	h.must(code, 400, out, "bad manifest")
	code, out = h.do("owner", "POST", "/api/plugins/dev-install", gin.H{"path": pluginDir(t, "dev.test.demo", "1.0.0", "", []byte("\x00asm\x01\x00\x00\x00"))})
	h.must(code, 400, out, "wasm without the declared ops")

	h.install("dev.test.demo", "1.0.0", "")
	out = h.install("dev.test.demo", "1.1.0", `, {"host": "api.example.com", "ports": [443]}`)
	if out["reinstall"] != true || !strings.Contains(fmt.Sprint(out["addedPermissions"]), "api.example.com") {
		t.Errorf("reinstall diff: %v", out)
	}
	code, out = h.do("someone", "GET", "/api/plugins", nil)
	h.must(code, 200, out, "list")
	if list := out["data"].([]any); len(list) != 1 || list[0].(map[string]any)["version"] != "1.1.0" {
		t.Errorf("list = %v", list)
	}
}

func TestConnectionsAndOps(t *testing.T) {
	h := newHarness(t, true)
	h.install("dev.test.demo", "1.0.0", "")
	h.install("dev.test.other", "1.0.0", "")
	base := "/api/plugin-connections/dev.test.demo"

	// Config/secret split and schema.
	for name, body := range map[string]gin.H{
		"secret in config":   {"name": "c", "config": gin.H{"host": "a.example.com", "token": "x"}},
		"undeclared secret":  {"name": "c", "config": gin.H{"host": "a.example.com"}, "secrets": gin.H{"password": "x"}},
		"missing required":   {"name": "c", "config": gin.H{}},
		"host with a scheme": {"name": "c", "config": gin.H{"host": "https://a.example.com"}},
	} {
		code, out := h.do("owner", "POST", base, body)
		h.must(code, 400, out, name)
	}
	id := h.conn("owner", "dev.test.demo", false)
	code, out := h.do("owner", "GET", base+"/"+id, nil)
	h.must(code, 200, out, "get connection")
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "tok-owner") || !strings.Contains(string(raw), `"secretsSet":["token"]`) {
		t.Errorf("connection DTO: %s", raw)
	}
	var row models.PluginConnection
	h.db.First(&row, "id = ?", id)
	if strings.Contains(row.ConfigJSON+row.SecretsEnc, "tok-owner") {
		t.Error("secret stored in clear")
	}

	ops := "/api/plugins/dev.test.demo/" + id + "/ops/"
	code, out = h.do("owner", "POST", ops+"whoami", nil)
	h.must(code, 200, out, "whoami")
	if out["data"].(map[string]any)["text"] != "tok-owner" || out["source"].(map[string]any)["untrusted"] != true {
		t.Errorf("whoami = %v", out)
	}
	code, out = h.do("owner", "POST", ops+"echo", gin.H{"msg": 5})
	h.must(code, 400, out, "input schema")
	code, out = h.do("owner", "POST", ops+"nope", nil)
	h.must(code, 404, out, "unknown op")

	// Plugin A's connection never runs under plugin B.
	code, out = h.do("owner", "POST", "/api/plugins/dev.test.other/"+id+"/ops/whoami", nil)
	h.must(code, 404, out, "cross-plugin call")
	code, out = h.do("owner", "GET", "/api/plugin-connections/dev.test.other/"+id, nil)
	h.must(code, 404, out, "cross-plugin get")

	// Access and write gates.
	code, out = h.do("stranger", "POST", ops+"whoami", nil)
	h.must(code, 404, out, "stranger")
	code, out = h.do("owner", "POST", base+"/"+id+"/grants", gin.H{"subjectKind": "user", "subjectId": "reader"})
	h.must(code, 201, out, "grant")
	code, out = h.do("reader", "POST", ops+"whoami", nil)
	h.must(code, 200, out, "reader runs a read op")
	code, out = h.do("reader", "POST", ops+"drop", nil)
	h.must(code, 403, out, "reader runs a write op")
	code, out = h.do("owner", "POST", ops+"drop", nil)
	h.must(code, 403, out, "write op on a read-only connection")
	code, out = h.do("owner", "PATCH", base+"/"+id, gin.H{"allowWrite": true})
	h.must(code, 200, out, "enable read-write")
	code, out = h.do("owner", "POST", ops+"drop", nil)
	h.must(code, 200, out, "owner write op")
	code, out = h.do("owner", "POST", ops+"drop", nil, middleware.WithInProcess(context.Background()))
	h.must(code, 403, out, "agent write op without allowWrite")
	code, out = h.do("owner", "POST", ops+"drop?allowWrite=true", nil, middleware.WithInProcess(context.Background()))
	h.must(code, 200, out, "agent write op with allowWrite")

	// Sensitive op: step-up via the gate matcher; never for agents.
	if !middleware.IsSensitiveRoute("POST", ops+"danger") || middleware.IsSensitiveRoute("POST", ops+"echo") {
		t.Error("sensitive matcher")
	}
	if !middleware.IsSensitiveRoute("POST", base+"/"+id+"/reveal-secret") || !middleware.IsSensitiveRoute("DELETE", "/api/plugins/dev.test.demo") {
		t.Error("reveal / uninstall not step-up gated")
	}
	code, out = h.do("owner", "POST", ops+"danger", nil, middleware.WithInProcess(context.Background()))
	h.must(code, 403, out, "agent sensitive op")
	code, out = h.do("owner", "POST", ops+"danger", nil)
	h.must(code, 200, out, "person sensitive op (gate does step-up)")

	// Reveal: owner only, people only.
	code, out = h.do("owner", "POST", base+"/"+id+"/reveal-secret", gin.H{"which": "token"})
	h.must(code, 200, out, "reveal")
	if dataField(out, "value") != "tok-owner" {
		t.Errorf("reveal = %v", out)
	}
	code, out = h.do("reader", "POST", base+"/"+id+"/reveal-secret", gin.H{"which": "token"})
	h.must(code, 404, out, "reader reveal")
	code, out = h.do("owner", "POST", base+"/"+id+"/reveal-secret", gin.H{"which": "token"}, middleware.WithInProcess(context.Background()))
	h.must(code, 403, out, "agent reveal")
	code, out = h.do("owner", "POST", base+"/"+id+"/reveal-secret", gin.H{"which": "host"})
	h.must(code, 400, out, "reveal a config field")

	// Edit: secrets merge, pooled instance dropped.
	h.do("owner", "POST", ops+"counter", nil)
	code, out = h.do("owner", "POST", ops+"counter", nil)
	if fmt.Sprint(out["data"]) != "2" {
		t.Fatalf("counter before edit = %v", out)
	}
	code, out = h.do("owner", "PATCH", base+"/"+id, gin.H{"secrets": gin.H{"token": "tok-new"}})
	h.must(code, 200, out, "rotate secret")
	_, out = h.do("owner", "POST", ops+"counter", nil)
	if fmt.Sprint(out["data"]) != "1" {
		t.Errorf("edit kept the pooled instance: counter=%v", out["data"])
	}
	_, out = h.do("owner", "POST", ops+"whoami", nil)
	if out["data"].(map[string]any)["text"] != "tok-new" {
		t.Errorf("rotated secret not used: %v", out)
	}

	// Test op on saved and unsaved connections.
	code, out = h.do("owner", "POST", base+"/"+id+"/test", nil)
	h.must(code, 200, out, "test saved")
	code, out = h.do("owner", "POST", base+"/test", gin.H{"config": gin.H{"host": "b.example.com"}, "secrets": gin.H{"token": "t"}})
	h.must(code, 200, out, "test unsaved")
	if dataField(out, "ok") != true {
		t.Errorf("test = %v", out)
	}

	var acts int64
	h.db.Model(&models.PluginActivity{}).Where("action = 'op'").Count(&acts)
	if acts < 8 { // every call that reached the runtime
		t.Errorf("op calls audited: %d", acts)
	}
	var refused int64
	h.db.Model(&models.PluginActivity{}).Where("error_text LIKE 'refused:%'").Count(&refused)
	if refused < 4 { // reader write, read-only conn, agent without allowWrite, agent sensitive
		t.Errorf("refused attempts audited: %d", refused)
	}
	var delegated int64
	h.db.Model(&models.PluginActivity{}).Where("action = 'op' AND user_id = 'reader' AND delegated_owner = 'owner'").Count(&delegated)
	if delegated == 0 {
		t.Error("delegated call not marked")
	}
}

func TestMCPTools(t *testing.T) {
	h := newHarness(t, true)
	h.install("dev.test.demo", "1.0.0", "")
	id := h.conn("owner", "dev.test.demo", true)

	session := func(scopes *string) *mcpsdk.ClientSession {
		ct, st := mcpsdk.NewInMemoryTransports()
		if _, err := mcp.BuildServer(h.r, jwtSecret, "owner", mcpscope.Parse(scopes)).Connect(context.Background(), st, nil); err != nil {
			t.Fatal(err)
		}
		cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "t", Version: "0"}, nil).Connect(context.Background(), ct, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cs.Close() })
		return cs
	}
	names := func(cs *mcpsdk.ClientSession) []string {
		res, err := cs.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, tl := range res.Tools {
			if strings.HasPrefix(tl.Name, "plugin_") {
				out = append(out, tl.Name)
			}
		}
		slices.Sort(out)
		return out
	}

	all := session(nil)
	if got := names(all); !slices.Equal(got, []string{"plugin_demo_connections", "plugin_demo_drop", "plugin_demo_echo"}) {
		t.Fatalf("tools = %v", got)
	}
	only := "db,notes"
	if got := names(session(&only)); len(got) != 0 {
		t.Errorf("tools without the plugin scope: %v", got)
	}
	withPlugin := "notes," + mcpscope.PluginGroup("dev.test.demo")
	if got := names(session(&withPlugin)); len(got) != 3 {
		t.Errorf("tools with the plugin scope: %v", got)
	}
	if !mcpscope.IsValidGroup("plugin:dev.test.demo") || mcpscope.IsValidGroup("plugin:dev.test.nope") {
		t.Error("IsValidGroup for plugin groups")
	}

	call := func(name string, args map[string]any) (bool, string) {
		res, err := all.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return res.IsError, res.Content[0].(*mcpsdk.TextContent).Text
	}
	if isErr, out := call("plugin_demo_echo", map[string]any{"connectionId": id, "msg": "hi"}); isErr || !strings.Contains(out, `"msg":"hi"`) || !strings.Contains(out, `"untrusted":true`) {
		t.Errorf("echo via MCP: %v %s", isErr, out)
	}
	if isErr, out := call("plugin_demo_drop", map[string]any{"connectionId": id}); !isErr {
		t.Errorf("write tool without allowWrite: %s", out)
	}
	if isErr, out := call("plugin_demo_drop", map[string]any{"connectionId": id, "allowWrite": true}); isErr {
		t.Errorf("write tool with allowWrite: %s", out)
	}
	if isErr, out := call("plugin_demo_connections", map[string]any{}); isErr || !strings.Contains(out, id) {
		t.Errorf("connections tool: %s", out)
	}
	var mcpActs int64
	h.db.Model(&models.PluginActivity{}).Where("client = 'mcp'").Count(&mcpActs)
	if mcpActs == 0 {
		t.Error("MCP calls not attributed in the audit")
	}

	// Disabled plugin: no tools, no ops.
	code, out := h.do("owner", "PATCH", "/api/plugins/dev.test.demo", gin.H{"enabled": false})
	h.must(code, 200, out, "disable")
	if got := names(session(nil)); len(got) != 0 {
		t.Errorf("tools of a disabled plugin: %v", got)
	}
	code, out = h.do("owner", "POST", "/api/plugins/dev.test.demo/"+id+"/ops/echo", nil)
	h.must(code, 404, out, "op of a disabled plugin")
}

func TestUninstallCascade(t *testing.T) {
	h := newHarness(t, true)
	h.install("dev.test.demo", "1.0.0", "")
	id := h.conn("owner", "dev.test.demo", false)
	h.do("owner", "POST", "/api/plugin-connections/dev.test.demo/"+id+"/grants", gin.H{"subjectKind": "user", "subjectId": "reader"})
	code, out := h.do("owner", "POST", "/api/plugins/dev.test.demo/"+id+"/ops/whoami", nil)
	h.must(code, 200, out, "op before uninstall")
	cacheDir := filepath.Join(h.cache, "dev.test.demo")
	if _, err := os.Stat(cacheDir); err != nil {
		t.Fatalf("no compile cache: %v", err)
	}

	code, out = h.do("owner", "DELETE", "/api/plugins/dev.test.demo", nil)
	h.must(code, 200, out, "uninstall")
	count := func(m any, where string, args ...any) int64 {
		var n int64
		h.db.Model(m).Where(where, args...).Count(&n)
		return n
	}
	if count(&models.PluginConnection{}, "plugin_id = ?", "dev.test.demo") != 0 ||
		count(&models.PluginConnectionGrant{}, "connection_id = ?", id) != 0 ||
		count(&models.PluginBlob{}, "plugin_id = ?", "dev.test.demo") != 0 ||
		count(&models.Plugin{}, "id = ?", "dev.test.demo") != 0 {
		t.Error("uninstall left rows behind")
	}
	if count(&models.PluginActivity{}, "plugin_id = ? AND action = 'uninstall'", "dev.test.demo") != 1 {
		t.Error("uninstall not audited / audit trail removed")
	}
	if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
		t.Errorf("compile cache left behind: %v", err)
	}
	// The service's own matcher (other tests' services stay registered globally).
	if h.svc.sensitive("POST", "/api/plugins/dev.test.demo/"+id+"/ops/danger") {
		t.Error("sensitive matcher still matches an uninstalled plugin")
	}
	code, out = h.do("owner", "POST", "/api/plugins/dev.test.demo/"+id+"/ops/whoami", nil)
	h.must(code, 404, out, "op after uninstall")
	if ids, _ := h.svc.Runtime().Loaded(); len(ids) != 0 {
		t.Errorf("still loaded: %v", ids)
	}
}

func TestCustomUI(t *testing.T) {
	h := newHarness(t, true)
	mk := func(entry string, files map[string]string) string {
		dir := pluginDir(t, "dev.test.ui", "1.0.0", "", demoWasm)
		raw, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
		m := strings.Replace(string(raw), `"mcp": {`, `"ui": {"entry": "`+entry+`"}, "mcp": {`, 1)
		os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(m), 0o600)
		for p, c := range files {
			os.MkdirAll(filepath.Dir(filepath.Join(dir, "ui", p)), 0o700)
			os.WriteFile(filepath.Join(dir, "ui", p), []byte(c), 0o600)
		}
		return dir
	}
	install := func(dir string) (int, map[string]any) {
		return h.do("owner", "POST", "/api/plugins/dev-install", gin.H{"path": dir})
	}
	code, out := install(mk("ui/index.html", map[string]string{"app.js": "1"}))
	h.must(code, 400, out, "entry missing from ui/")
	code, out = install(mk("ui/index.html", map[string]string{"index.html": "x", "evil.exe": "x"}))
	h.must(code, 400, out, "disallowed file type")
	dir := mk("ui/index.html", map[string]string{"index.html": "<script src=app.js></script>", "js/app.js": "1"})
	os.Symlink("/etc/hosts", filepath.Join(dir, "ui", "hosts.txt"))
	code, out = install(dir)
	h.must(code, 400, out, "symlink in ui/")
	os.Remove(filepath.Join(dir, "ui", "hosts.txt"))
	code, out = install(dir)
	h.must(code, 200, out, "install with ui")
	rev := dataField(out, "uiRev").(string)
	if rev == "" {
		t.Fatal("no uiRev")
	}

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.r.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}
	base := "/plugin-ui/dev.test.ui/" + rev + "/"
	rec := get(base + "index.html")
	csp := rec.Header().Get("Content-Security-Policy")
	if rec.Code != 200 || !strings.HasPrefix(csp, "sandbox allow-scripts;") || !strings.Contains(csp, "connect-src 'none'") ||
		!strings.Contains(csp, "frame-ancestors tauri://localhost") || rec.Header().Get("X-Content-Type-Options") != "nosniff" ||
		rec.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("entry: %d %v", rec.Code, rec.Header())
	}
	if body := rec.Body.String(); !strings.HasPrefix(body, `<script src=".pm/guard.js"></script><script src=app.js>`) {
		t.Errorf("guard not injected first: %q", body)
	}
	if g := get(base + ".pm/guard.js"); g.Code != 200 || !strings.Contains(g.Body.String(), "RTCPeerConnection") {
		t.Errorf("guard: %d", g.Code)
	}
	if got := string(injectGuard([]byte("<!DOCTYPE html>\n<html><script>x</script>"))); got != `<!DOCTYPE html><script src=".pm/guard.js"></script>`+"\n<html><script>x</script>" {
		t.Errorf("doctype injection: %q", got)
	}
	if rec := get(base + "js/app.js"); rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") {
		t.Errorf("js: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	for _, p := range []string{"/plugin-ui/dev.test.ui/stale/index.html", base + "..%2fmanifest.json", base + "../manifest.json", base + "nope.js", "/plugin-ui/dev.test.nope/" + rev + "/index.html"} {
		if rec := get(p); rec.Code != 404 {
			t.Errorf("%s: %d", p, rec.Code)
		}
		if rec := get(p); rec.Header().Get("Content-Security-Policy") == "" && rec.Code != 301 {
			t.Errorf("%s: no CSP on error response", p)
		}
	}
	h.do("owner", "PATCH", "/api/plugins/dev.test.ui", gin.H{"enabled": false})
	if rec := get(base + "index.html"); rec.Code != 404 {
		t.Errorf("disabled plugin UI served: %d", rec.Code)
	}
	h.do("owner", "DELETE", "/api/plugins/dev.test.ui", nil)
	var n int64
	h.db.Model(&models.PluginUIFile{}).Where("plugin_id = ?", "dev.test.ui").Count(&n)
	if n != 0 {
		t.Errorf("uninstall left %d ui files", n)
	}
}

func TestOutputSchemaEnforced(t *testing.T) {
	h := newHarness(t, true)
	dir := pluginDir(t, "dev.test.out", "1.0.0", "", demoWasm)
	raw, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	m := strings.Replace(string(raw), `"whoami": {},`, `"whoami": {"output": {"type": "object"}}, "echo2": {},`, 1)
	m = strings.Replace(m, `"echo": {"input"`, `"echo": {"output": {"type": "object", "required": ["msg"]}, "input"`, 1)
	m = strings.Replace(m, `"echo2": {},`, ``, 1)
	os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(m), 0o600)
	code, out := h.do("owner", "POST", "/api/plugins/dev-install", gin.H{"path": dir})
	h.must(code, 200, out, "install")
	id := h.conn("owner", "dev.test.out", false)
	ops := "/api/plugins/dev.test.out/" + id + "/ops/"
	code, out = h.do("owner", "POST", ops+"echo", gin.H{"msg": "ok"})
	h.must(code, 200, out, "output matches schema")
	code, out = h.do("owner", "POST", ops+"echo", gin.H{})
	h.must(code, 502, out, "output missing a required field")
	code, out = h.do("owner", "POST", ops+"whoami", nil)
	h.must(code, 502, out, "non-JSON output for an object schema")
	if !strings.Contains(fmt.Sprint(out["error"]), "does not match its schema") {
		t.Errorf("error = %v", out["error"])
	}
}
