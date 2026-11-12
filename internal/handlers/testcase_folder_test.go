package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"patchman-be/internal/database"
	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type folderCall func(user, method, path string, body any) (int, map[string]any)

func testFolderEnv(t *testing.T) (*gorm.DB, folderCall) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := database.Init("sqlite", filepath.Join(t.TempDir(), "tf.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []models.User{{ID: "alice", Email: "alice@example.com", Name: "Alice"}, {ID: "bob", Email: "bob@example.com", Name: "Bob"}} {
		if err := db.Create(&u).Error; err != nil {
			t.Fatal(err)
		}
	}
	fh := NewTestCaseFolderHandler(db)
	ch := NewTestCaseHandler(db)
	ex := NewTestExecutorHandler(db)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", c.GetHeader("X-User")); c.Next() })
	r.GET("/test-folders", fh.List)
	r.POST("/test-folders", fh.Create)
	r.PUT("/test-folders/:id", fh.Rename)
	r.DELETE("/test-folders/:id", fh.Delete)
	r.POST("/test-folders/:id/execute", fh.Execute)
	r.PUT("/test-cases/:id/move", fh.MoveTestCase)
	r.POST("/test-cases", ch.Create)
	r.GET("/test-cases", ch.List)
	r.POST("/test-cases/:id/execute", ex.ExecuteTestCase)
	call := func(user, method, path string, body any) (int, map[string]any) {
		var rd *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		} else {
			rd = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, rd)
		req.Header.Set("X-User", user)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	return db, call
}

func mustFolder(t *testing.T, call folderCall, user, name string, parent *string) string {
	t.Helper()
	body := map[string]any{"name": name}
	if parent != nil {
		body["parentId"] = *parent
	}
	code, res := call(user, "POST", "/test-folders", body)
	if code != http.StatusCreated {
		t.Fatalf("create folder %q: %d %v", name, code, res)
	}
	return res["data"].(map[string]any)["id"].(string)
}

// seedCase inserts a case directly (controls created_at for ordering checks).
func seedCase(t *testing.T, db *gorm.DB, id, user string, folder *string, title, steps string, created time.Time) {
	t.Helper()
	tc := models.TestCase{ID: id, UserID: user, FolderID: folder, Title: title, Steps: steps, CreatedAt: created, UpdatedAt: created}
	if err := db.Create(&tc).Error; err != nil {
		t.Fatal(err)
	}
}

// loadFolder reads a folder into a FRESH struct (GORM adds a populated
// struct's primary key to the WHERE, so reusing one would mis-query).
func loadFolder(t *testing.T, db *gorm.DB, id string) models.TestCaseFolder {
	t.Helper()
	var f models.TestCaseFolder
	if err := db.First(&f, "id = ?", id).Error; err != nil {
		t.Fatalf("folder %s: %v", id, err)
	}
	return f
}

func folderOf(t *testing.T, db *gorm.DB, caseID string) *string {
	t.Helper()
	var tc models.TestCase
	if err := db.First(&tc, "id = ?", caseID).Error; err != nil {
		t.Fatalf("case %s: %v", caseID, err)
	}
	return tc.FolderID
}

const waitSteps = `[{"stepNo":1,"type":"wait","waitMs":0}]`

func TestTestFolderCRUDAndAccess(t *testing.T) {
	db, call := testFolderEnv(t)

	top := mustFolder(t, call, "alice", "  Checkout  ", nil)
	sub := mustFolder(t, call, "alice", "Payments", &top)
	bobs := mustFolder(t, call, "bob", "Bob's", nil)

	f := loadFolder(t, db, top)
	if f.Name != "Checkout" || f.ParentID != nil {
		t.Errorf("top folder stored as %+v, want trimmed name + NULL parent", f)
	}
	f = loadFolder(t, db, sub)
	if f.ParentID == nil || *f.ParentID != top {
		t.Errorf("subfolder parent = %v, want %s", f.ParentID, top)
	}

	// Validation.
	if code, _ := call("alice", "POST", "/test-folders", map[string]any{"name": "   "}); code != http.StatusBadRequest {
		t.Errorf("blank name: %d, want 400", code)
	}
	long := make([]byte, 256)
	for i := range long {
		long[i] = 'x'
	}
	if code, _ := call("alice", "POST", "/test-folders", map[string]any{"name": string(long)}); code != http.StatusBadRequest {
		t.Errorf("256-char name: %d, want 400", code)
	}
	// A parent the caller doesn't own behaves like a missing one.
	if code, _ := call("alice", "POST", "/test-folders", map[string]any{"name": "x", "parentId": bobs}); code != http.StatusNotFound {
		t.Errorf("foreign parent: %d, want 404", code)
	}

	// List is per-user.
	_, res := call("alice", "GET", "/test-folders", nil)
	if n := len(res["data"].([]any)); n != 2 {
		t.Errorf("alice sees %d folders, want 2", n)
	}
	_, res = call("bob", "GET", "/test-folders", nil)
	if n := len(res["data"].([]any)); n != 1 {
		t.Errorf("bob sees %d folders, want 1", n)
	}

	// Rename.
	code, res := call("alice", "PUT", "/test-folders/"+sub, map[string]any{"name": "Payments v2"})
	if code != http.StatusOK || res["data"].(map[string]any)["name"] != "Payments v2" {
		t.Errorf("rename: %d %v", code, res)
	}
	if code, _ := call("alice", "PUT", "/test-folders/"+sub, map[string]any{"name": ""}); code != http.StatusBadRequest {
		t.Errorf("rename to blank: %d, want 400", code)
	}

	// Another user can't touch alice's folders (404, not 403).
	for _, req := range []struct{ method, path string }{
		{"PUT", "/test-folders/" + top},
		{"DELETE", "/test-folders/" + top},
		{"POST", "/test-folders/" + top + "/execute"},
	} {
		if code, _ := call("bob", req.method, req.path, map[string]any{"name": "pwned"}); code != http.StatusNotFound {
			t.Errorf("bob %s %s: %d, want 404", req.method, req.path, code)
		}
	}
	f = loadFolder(t, db, top)
	if f.Name != "Checkout" {
		t.Errorf("bob's rename leaked through: %q", f.Name)
	}
}

func TestMoveTestCase(t *testing.T) {
	db, call := testFolderEnv(t)
	folder := mustFolder(t, call, "alice", "A", nil)
	bobs := mustFolder(t, call, "bob", "B", nil)
	now := time.Now()
	seedCase(t, db, "11111111-1111-1111-1111-111111111111", "alice", nil, "case", waitSteps, now)
	seedCase(t, db, "22222222-2222-2222-2222-222222222222", "bob", nil, "bob case", waitSteps, now)
	id := "11111111-1111-1111-1111-111111111111"

	if code, res := call("alice", "PUT", "/test-cases/"+id+"/move", map[string]any{"folderId": folder}); code != http.StatusOK {
		t.Fatalf("move into folder: %d %v", code, res)
	}
	if got := folderOf(t, db, id); got == nil || *got != folder {
		t.Errorf("folder_id = %v, want %s", got, folder)
	}
	// null → root; "" → root.
	call("alice", "PUT", "/test-cases/"+id+"/move", map[string]any{"folderId": nil})
	if got := folderOf(t, db, id); got != nil {
		t.Errorf("null move: folder_id = %v, want NULL", *got)
	}
	call("alice", "PUT", "/test-cases/"+id+"/move", map[string]any{"folderId": folder})
	call("alice", "PUT", "/test-cases/"+id+"/move", map[string]any{"folderId": ""})
	if got := folderOf(t, db, id); got != nil {
		t.Errorf("empty-string move: folder_id = %v, want NULL", *got)
	}

	// Into someone else's folder / someone else's case → 404, nothing changes.
	if code, _ := call("alice", "PUT", "/test-cases/"+id+"/move", map[string]any{"folderId": bobs}); code != http.StatusNotFound {
		t.Errorf("move into foreign folder: %d, want 404", code)
	}
	if code, _ := call("alice", "PUT", "/test-cases/22222222-2222-2222-2222-222222222222/move", map[string]any{"folderId": folder}); code != http.StatusNotFound {
		t.Errorf("move foreign case: %d, want 404", code)
	}
	if got := folderOf(t, db, "22222222-2222-2222-2222-222222222222"); got != nil {
		t.Errorf("bob's case was moved into %v", *got)
	}

	// A move is not an edit: no version snapshot is written.
	var versions int64
	db.Model(&models.TestCaseVersion{}).Count(&versions)
	if versions != 0 {
		t.Errorf("move wrote %d version snapshots, want 0", versions)
	}

	// Create can file a case directly; a foreign folder is refused.
	code, res := call("alice", "POST", "/test-cases", map[string]any{"title": "new", "folderId": folder})
	if code != http.StatusCreated || res["data"].(map[string]any)["folderId"] != folder {
		t.Errorf("create in folder: %d %v", code, res)
	}
	if code, _ := call("alice", "POST", "/test-cases", map[string]any{"title": "new", "folderId": bobs}); code != http.StatusNotFound {
		t.Errorf("create in foreign folder: %d, want 404", code)
	}
	// Existing clients that send no folderId keep landing at the root.
	code, res = call("alice", "POST", "/test-cases", map[string]any{"title": "legacy"})
	if code != http.StatusCreated || res["data"].(map[string]any)["folderId"] != nil {
		t.Errorf("legacy create: %d %v", code, res)
	}
}

func TestDeleteTestFolderRules(t *testing.T) {
	db, call := testFolderEnv(t)
	now := time.Now()
	top := mustFolder(t, call, "alice", "Top", nil)
	mid := mustFolder(t, call, "alice", "Mid", &top)
	leaf := mustFolder(t, call, "alice", "Leaf", &mid)
	empty := mustFolder(t, call, "alice", "Empty", nil)
	seedCase(t, db, "c-mid", "alice", &mid, "in mid", waitSteps, now)
	seedCase(t, db, "c-top", "alice", &top, "in top", waitSteps, now)

	// Empty folder: deleted without the flag.
	if code, res := call("alice", "DELETE", "/test-folders/"+empty, nil); code != http.StatusOK {
		t.Errorf("delete empty: %d %v", code, res)
	}

	// Non-empty without the flag → 409 with counts; nothing changes.
	code, res := call("alice", "DELETE", "/test-folders/"+mid, nil)
	if code != http.StatusConflict {
		t.Fatalf("delete non-empty: %d %v, want 409", code, res)
	}
	if res["cases"].(float64) != 1 || res["folders"].(float64) != 1 {
		t.Errorf("409 counts = %v, want cases=1 folders=1", res)
	}
	// A folder holding only a subfolder is not empty either.
	if code, _ := call("alice", "DELETE", "/test-folders/"+top, nil); code != http.StatusConflict {
		t.Errorf("delete folder with subfolder only: %d, want 409", code)
	}

	// With moveToParent: its case and subfolder move up to its parent.
	if code, res := call("alice", "DELETE", "/test-folders/"+mid+"?moveToParent=true", nil); code != http.StatusOK {
		t.Fatalf("delete with moveToParent: %d %v", code, res)
	}
	if got := folderOf(t, db, "c-mid"); got == nil || *got != top {
		t.Errorf("case moved to %v, want parent %s", got, top)
	}
	f := loadFolder(t, db, leaf)
	if f.ParentID == nil || *f.ParentID != top {
		t.Errorf("subfolder reparented to %v, want %s", f.ParentID, top)
	}
	if err := db.First(&models.TestCaseFolder{}, "id = ?", mid).Error; err == nil {
		t.Error("mid folder still exists")
	}

	// Top-level folder with moveToParent: contents go to the root (NULL).
	if code, res := call("alice", "DELETE", "/test-folders/"+top+"?moveToParent=true", nil); code != http.StatusOK {
		t.Fatalf("delete top with moveToParent: %d %v", code, res)
	}
	for _, id := range []string{"c-mid", "c-top"} {
		if got := folderOf(t, db, id); got != nil {
			t.Errorf("%s folder = %s, want root (NULL)", id, *got)
		}
	}
	f = loadFolder(t, db, leaf)
	if f.ParentID != nil {
		t.Errorf("leaf parent = %s, want NULL", *f.ParentID)
	}
	// Cases are never deleted by deleting a folder.
	var n int64
	db.Model(&models.TestCase{}).Count(&n)
	if n != 2 {
		t.Errorf("case count = %d, want 2", n)
	}
}

func TestCollectFolderCasesSelection(t *testing.T) {
	db, call := testFolderEnv(t)
	base := time.Now().Add(-time.Hour)
	at := func(m int) time.Time { return base.Add(time.Duration(m) * time.Minute) }

	root := mustFolder(t, call, "alice", "Root", nil)
	zeta := mustFolder(t, call, "alice", "zeta", &root)
	alpha := mustFolder(t, call, "alice", "Alpha", &root)
	deep := mustFolder(t, call, "alice", "Deep", &alpha)
	other := mustFolder(t, call, "alice", "Other", nil)

	seedCase(t, db, "root-old", "alice", &root, "root old", waitSteps, at(1))
	seedCase(t, db, "root-new", "alice", &root, "root new", waitSteps, at(5))
	seedCase(t, db, "alpha-1", "alice", &alpha, "alpha", waitSteps, at(2))
	seedCase(t, db, "deep-1", "alice", &deep, "deep", waitSteps, at(3))
	seedCase(t, db, "zeta-1", "alice", &zeta, "zeta", waitSteps, at(4))
	seedCase(t, db, "manual", "alice", &root, "manual only", `[{"stepNo":1,"action":"click","expected":"ok"}]`, at(6))
	seedCase(t, db, "nosteps", "alice", &alpha, "no steps", "", at(7))
	seedCase(t, db, "elsewhere", "alice", &other, "other folder", waitSteps, at(8))
	seedCase(t, db, "at-root", "alice", nil, "unfiled", waitSteps, at(9))
	// A foreign case pointing at alice's folder id must never be picked up.
	seedCase(t, db, "bobs", "bob", &root, "bob's", waitSteps, at(10))

	ids := func(cs []models.TestCase) []string {
		out := make([]string, len(cs))
		for i, c := range cs {
			out[i] = c.ID
		}
		return out
	}

	runnable, skipped, err := collectFolderCases(db, "alice", root, true)
	if err != nil {
		t.Fatal(err)
	}
	// Display order: subfolders A→Z (case-insensitive, depth-first), then
	// the folder's own cases newest first.
	want := []string{"deep-1", "alpha-1", "zeta-1", "root-new", "root-old"}
	if got := ids(runnable); !equalStrings(got, want) {
		t.Errorf("recursive order = %v, want %v", got, want)
	}
	if got := ids(skipped); !equalStrings(got, []string{"nosteps", "manual"}) {
		t.Errorf("skipped = %v, want [nosteps manual]", got)
	}

	runnable, _, _ = collectFolderCases(db, "alice", root, false)
	if got := ids(runnable); !equalStrings(got, []string{"root-new", "root-old"}) {
		t.Errorf("non-recursive = %v, want [root-new root-old]", got)
	}

	// Another user asking for alice's folder gets nothing.
	runnable, skipped, _ = collectFolderCases(db, "bob", root, true)
	if got := ids(runnable); !equalStrings(got, []string{"bobs"}) || len(skipped) != 0 {
		t.Errorf("bob's view of alice's folder = %v / %v, want only his own case", got, ids(skipped))
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestHasExecutableSteps(t *testing.T) {
	for in, want := range map[string]bool{
		"":   false,
		"[]": false,
		`[{"stepNo":1,"action":"x","expected":"y"}]`:             false,
		`[{"stepNo":1,"type":"request"}]`:                        true,
		`[{"stepNo":1,"action":"x"},{"stepNo":2,"type":"wait"}]`: true,
		`not json`: true, // surfaces as a parse-error result instead of vanishing
	} {
		if got := hasExecutableSteps(in); got != want {
			t.Errorf("hasExecutableSteps(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestExecuteTestFolder(t *testing.T) {
	db, call := testFolderEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Token") != "tok-123" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	env := models.Environment{ID: "env-alice", UserID: "alice", Name: "dev", Visibility: "private",
		Variables: `[{"key":"base_url","value":"` + srv.URL + `","enabled":true},{"key":"token","value":"tok-123","enabled":true,"secret":true}]`}
	if err := db.Create(&env).Error; err != nil {
		t.Fatal(err)
	}

	folder := mustFolder(t, call, "alice", "Smoke", nil)
	sub := mustFolder(t, call, "alice", "Sub", &folder)
	now := time.Now()
	reqSteps := `[{"stepNo":1,"type":"request","request":{"method":"GET","url":"{{base_url}}/ping","headers":{"X-Token":"{{token}}"}}},
	              {"stepNo":2,"type":"assertion","assertions":[{"type":"status","operator":"equals","value":"200"}]}]`
	seedCase(t, db, "ping", "alice", &folder, "ping", reqSteps, now)
	seedCase(t, db, "broken", "alice", &sub, "broken", `[{"stepNo":1,"type":"assertion","assertions":[{"type":"status","operator":"equals","value":"200"}]}]`, now)
	seedCase(t, db, "manual", "alice", &folder, "manual", `[{"stepNo":1,"action":"look","expected":"ok"}]`, now)

	code, res := call("alice", "POST", "/test-folders/"+folder+"/execute", map[string]any{"environmentId": "env-alice"})
	if code != http.StatusOK {
		t.Fatalf("execute: %d %v", code, res)
	}
	data := res["data"].(map[string]any)
	sum := data["summary"].(map[string]any)
	if sum["total"].(float64) != 2 || sum["passed"].(float64) != 1 || sum["failed"].(float64) != 1 || sum["skipped"].(float64) != 1 {
		t.Errorf("summary = %v, want total 2 / passed 1 / failed 1 / skipped 1", sum)
	}
	if data["overallStatus"] != "failed" {
		t.Errorf("overallStatus = %v, want failed", data["overallStatus"])
	}
	cases := data["testCases"].([]any)
	byID := map[string]map[string]any{}
	for _, c := range cases {
		m := c.(map[string]any)
		byID[m["testCaseId"].(string)] = m
	}
	if byID["ping"]["status"] != "passed" {
		t.Errorf("ping status = %v (env vars not substituted?) result=%v", byID["ping"]["status"], byID["ping"]["result"])
	}
	if byID["broken"]["status"] != "failed" || byID["broken"]["folderId"] != sub {
		t.Errorf("broken = %v", byID["broken"])
	}

	// One TestRun persisted: folder-keyed, no plan, final status + results.
	runID := data["testRun"].(map[string]any)["id"].(string)
	var run models.TestRun
	if err := db.First(&run, "id = ?", runID).Error; err != nil {
		t.Fatalf("run not persisted: %v", err)
	}
	if run.TestPlanID != "" || run.TestFolderID == nil || *run.TestFolderID != folder || run.Status != "failed" || run.CompletedAt == nil {
		t.Errorf("persisted run = %+v", run)
	}
	if data["testRun"].(map[string]any)["testFolderId"] != folder {
		t.Errorf("serialized run lacks testFolderId: %v", data["testRun"])
	}

	// recursive=false: only the folder's own executable case.
	_, res = call("alice", "POST", "/test-folders/"+folder+"/execute", map[string]any{"environmentId": "env-alice", "recursive": false})
	if got := res["data"].(map[string]any)["summary"].(map[string]any)["total"]; got.(float64) != 1 {
		t.Errorf("non-recursive total = %v, want 1", got)
	}

	// A folder with nothing executable → 400 listing what was skipped.
	manualOnly := mustFolder(t, call, "alice", "Manual", nil)
	seedCase(t, db, "manual-2", "alice", &manualOnly, "manual 2", "", now)
	code, res = call("alice", "POST", "/test-folders/"+manualOnly+"/execute", nil)
	if code != http.StatusBadRequest || len(res["skipped"].([]any)) != 1 {
		t.Errorf("manual-only folder: %d %v, want 400 with 1 skipped", code, res)
	}

	// An environment the caller can't access → 404.
	if code, _ := call("alice", "POST", "/test-folders/"+folder+"/execute", map[string]any{"environmentId": "missing"}); code != http.StatusNotFound {
		t.Errorf("unknown env: %d, want 404", code)
	}
}

// TestRunEnvVarsAccess: the run resolves the same environments the editor's
// {{var}} autocomplete offers — owned and shared ones — but never hands a
// Viewer a secret, and refuses an inaccessible env.
func TestRunEnvVarsAccess(t *testing.T) {
	db, _ := testFolderEnv(t)
	env := models.Environment{ID: "env-shared", UserID: "alice", Name: "shared", Visibility: "public",
		Variables: `[{"key":"base_url","value":"http://x","enabled":true},{"key":"off","value":"1","enabled":false},{"key":"token","value":"s3cr3t","enabled":true,"secret":true}]`}
	if err := db.Create(&env).Error; err != nil {
		t.Fatal(err)
	}

	vars, ok := resolveRunEnvVars(db, "alice", "env-shared")
	if !ok || vars["base_url"] != "http://x" || vars["token"] != "s3cr3t" {
		t.Errorf("owner vars = %v ok=%v", vars, ok)
	}
	if _, has := vars["off"]; has {
		t.Error("disabled variable must not be substituted")
	}

	if _, ok := resolveRunEnvVars(db, "bob", "env-shared"); ok {
		t.Error("bob has no grant yet: env must be refused")
	}

	bob := "bob"
	grant := models.EnvironmentPublicGrant{ID: "g1", EnvironmentID: "env-shared", Role: "viewer", SubjectKind: "user", SubjectID: &bob}
	if err := db.Create(&grant).Error; err != nil {
		t.Fatal(err)
	}
	vars, ok = resolveRunEnvVars(db, "bob", "env-shared")
	if !ok || vars["base_url"] != "http://x" {
		t.Errorf("viewer vars = %v ok=%v", vars, ok)
	}
	if _, has := vars["token"]; has {
		t.Error("a viewer must not get secret values substituted")
	}

	db.Model(&grant).Update("role", "editor")
	vars, _ = resolveRunEnvVars(db, "bob", "env-shared")
	if vars["token"] != "s3cr3t" {
		t.Errorf("editor should get secrets, got %v", vars)
	}

	if vars, ok := resolveRunEnvVars(db, "bob", ""); !ok || len(vars) != 0 {
		t.Errorf("no env → empty vars, got %v ok=%v", vars, ok)
	}
}
