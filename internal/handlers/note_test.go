package handlers

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"patchman-be/internal/models"
	"patchman-be/internal/notesearch"
	"patchman-be/internal/realtime"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func noteTestDB(t *testing.T) (*gorm.DB, *NoteHandler, *NoteSharingHandler) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Note{}, &models.NoteRevision{}, &models.NotePublicGrant{}, &models.NoteAttachment{}, &models.NoteLink{}, &models.NoteSuggestion{}, &models.NoteInbox{}, &models.NoteMemorySettings{}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"CREATE TABLE IF NOT EXISTS team_members (team_id TEXT, user_id TEXT)",
		"CREATE TABLE IF NOT EXISTS teams (id TEXT, name TEXT, deleted_at DATETIME)",
		"CREATE TABLE IF NOT EXISTS users (id TEXT, email TEXT, name TEXT)",
		"CREATE TABLE IF NOT EXISTS workspaces (id TEXT, team_id TEXT)",
	} {
		db.Exec(q)
	}
	if err := notesearch.Ensure(db); err != nil {
		t.Fatal(err)
	}
	db.Exec("INSERT INTO users (id, email, name) VALUES ('alice','alice@x','Alice'),('bob','bob@x','Bob'),('carol','carol@x','')")
	return db, NewNoteHandler(db), NewNoteSharingHandler(db)
}

type noteResp struct {
	code int
	data json.RawMessage
	raw  map[string]json.RawMessage
}

func noteCall(t *testing.T, fn gin.HandlerFunc, user, method string, params gin.Params, body any, query string) noteResp {
	t.Helper()
	gin.SetMode(gin.TestMode)
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", user)
	c.Params = params
	c.Request = httptest.NewRequest(method, "/", bytes.NewReader(b))
	c.Request.URL.RawQuery = query
	c.Request.Header.Set("Content-Type", "application/json")
	fn(c)
	r := noteResp{code: w.Code}
	_ = json.Unmarshal(w.Body.Bytes(), &r.raw)
	r.data = r.raw["data"]
	return r
}

func id(v string) gin.Params { return gin.Params{{Key: "id", Value: v}} }

func noteField(t *testing.T, r noteResp, key string) any {
	t.Helper()
	var m map[string]any
	_ = json.Unmarshal(r.data, &m)
	return m[key]
}

func mkNote(t *testing.T, h *NoteHandler, user string, body map[string]any) string {
	t.Helper()
	r := noteCall(t, h.Create, user, "POST", nil, body, "")
	if r.code != 201 {
		t.Fatalf("create %v as %s = %d %s", body, user, r.code, r.raw["error"])
	}
	return noteField(t, r, "id").(string)
}

func TestNoteCRUDVersioningAndHistory(t *testing.T) {
	db, h, _ := noteTestDB(t)
	space := mkNote(t, h, "alice", map[string]any{"title": "Team Space", "type": "folder"})
	page := mkNote(t, h, "alice", map[string]any{"title": "Deploy Runbook", "body": "# Deploy\nsteps", "parentId": space})

	var n models.Note
	db.First(&n, "id = ?", page)
	if n.Slug != "deploy-runbook" || n.Version != 1 || n.UserID != "alice" {
		t.Fatalf("created note = %+v", n)
	}
	// Sibling with the same title gets a unique slug.
	dup := mkNote(t, h, "alice", map[string]any{"title": "Deploy Runbook", "parentId": space})
	var d models.Note
	db.First(&d, "id = ?", dup)
	if d.Slug != "deploy-runbook-2" {
		t.Errorf("duplicate slug = %q", d.Slug)
	}

	// Edit needs baseVersion; a stale one is a 409 carrying the current note.
	if r := noteCall(t, h.Update, "alice", "PATCH", id(page), map[string]any{"body": "x"}, ""); r.code != 400 {
		t.Errorf("edit without baseVersion = %d", r.code)
	}
	r := noteCall(t, h.Update, "alice", "PATCH", id(page), map[string]any{"body": "v2", "baseVersion": 1}, "")
	if r.code != 200 || noteField(t, r, "version").(float64) != 2 {
		t.Fatalf("edit = %d %s", r.code, r.data)
	}
	r = noteCall(t, h.Update, "alice", "PATCH", id(page), map[string]any{"body": "stale", "baseVersion": 1}, "")
	if r.code != 409 || !strings.Contains(string(r.raw["current"]), `"body":"v2"`) {
		t.Errorf("stale edit = %d current=%s", r.code, r.raw["current"])
	}

	// Same author within the window coalesces into one revision; restore adds one.
	noteCall(t, h.Update, "alice", "PATCH", id(page), map[string]any{"body": "v3", "baseVersion": 2}, "")
	var revs []models.NoteRevision
	db.Where("note_id = ?", page).Order("created_at").Find(&revs)
	if len(revs) != 1 || revs[0].Body != "v3" || revs[0].Version != 3 {
		t.Fatalf("coalesced revisions = %+v", revs)
	}
	db.Model(&models.NoteRevision{}).Where("id = ?", revs[0].ID).Update("body", "# Deploy\nsteps") // pretend it's the original
	r = noteCall(t, h.Restore, "alice", "POST", gin.Params{{Key: "id", Value: page}, {Key: "revId", Value: revs[0].ID}}, nil, "")
	if r.code != 200 || noteField(t, r, "body") != "# Deploy\nsteps" {
		t.Errorf("restore = %d %s", r.code, r.data)
	}
	var count int64
	db.Model(&models.NoteRevision{}).Where("note_id = ?", page).Count(&count)
	if count != 2 {
		t.Errorf("revisions after restore = %d, want 2", count)
	}

	// Append is atomic and needs no baseVersion.
	r = noteCall(t, h.Append, "alice", "POST", id(page), map[string]any{"text": "- [x] done"}, "")
	if r.code != 200 || !strings.HasSuffix(noteField(t, r, "body").(string), "steps\n\n- [x] done") {
		t.Errorf("append = %d %v", r.code, noteField(t, r, "body"))
	}
}

func TestNoteSharingPermissions(t *testing.T) {
	db, h, sh := noteTestDB(t)
	space := mkNote(t, h, "alice", map[string]any{"title": "Wiki", "type": "folder"})
	sub := mkNote(t, h, "alice", map[string]any{"title": "Ops", "type": "folder", "parentId": space})
	page := mkNote(t, h, "alice", map[string]any{"title": "Oncall", "body": "pager", "parentId": sub})
	other := mkNote(t, h, "alice", map[string]any{"title": "Private"})

	// Bob sees nothing yet.
	if r := noteCall(t, h.Get, "bob", "GET", id(page), nil, ""); r.code != 404 {
		t.Errorf("bob get before share = %d", r.code)
	}
	// Share the space with Bob as editor (needs public first).
	if r := noteCall(t, sh.AddGrant, "alice", "POST", id(space), map[string]any{"role": "editor", "subjectKind": "user", "subjectId": "bob"}, ""); r.code != 400 {
		t.Errorf("grant on private = %d", r.code)
	}
	noteCall(t, sh.SetVisibility, "alice", "PATCH", id(space), map[string]any{"visibility": "public"}, "")
	if r := noteCall(t, sh.AddGrant, "alice", "POST", id(space), map[string]any{"role": "editor", "subjectKind": "user", "subjectId": "bob"}, ""); r.code != 201 {
		t.Fatalf("grant = %d", r.code)
	}
	if got := ResolveNoteRole(db, "bob", page); got != EditorRole {
		t.Errorf("bob role on nested page = %q", got)
	}
	if got := ResolveNoteRole(db, "bob", other); got != NoRole {
		t.Errorf("bob role on unshared note = %q", got)
	}

	// Bob edits, adds a page (owned by Alice's tree), but can't move/delete the share root.
	r := noteCall(t, h.Update, "bob", "PATCH", id(page), map[string]any{"body": "pager + runbook", "baseVersion": 1}, "")
	if r.code != 200 {
		t.Errorf("bob edit = %d", r.code)
	}
	child := mkNote(t, h, "bob", map[string]any{"title": "Bob's page", "parentId": sub})
	var cn models.Note
	db.First(&cn, "id = ?", child)
	if cn.UserID != "alice" || cn.CreatedBy != "bob" {
		t.Errorf("child owner=%s createdBy=%s", cn.UserID, cn.CreatedBy)
	}
	if r := noteCall(t, h.Delete, "bob", "DELETE", id(space), nil, ""); r.code != 403 {
		t.Errorf("bob delete share root = %d", r.code)
	}
	if r := noteCall(t, h.Update, "bob", "PATCH", id(space), map[string]any{"parentId": ""}, ""); r.code != 403 {
		t.Errorf("bob move share root = %d", r.code)
	}
	if r := noteCall(t, h.Create, "bob", "POST", nil, map[string]any{"title": "x", "parentId": other}, ""); r.code != 403 {
		t.Errorf("bob create under unshared = %d", r.code)
	}
	// Inside the share Bob can reorganise: move his page under Ops' sibling page.
	if r := noteCall(t, h.Update, "bob", "PATCH", id(child), map[string]any{"parentId": page}, ""); r.code != 200 {
		t.Errorf("bob move inside share = %d %s", r.code, r.raw["error"])
	}
	// Cycles and cross-owner moves are rejected.
	if r := noteCall(t, h.Update, "alice", "PATCH", id(space), map[string]any{"parentId": page}, ""); r.code != 400 {
		t.Errorf("cycle move = %d", r.code)
	}
	bobSpace := mkNote(t, h, "bob", map[string]any{"title": "Bob space", "type": "folder"})
	if r := noteCall(t, h.Update, "bob", "PATCH", id(child), map[string]any{"parentId": bobSpace}, ""); r.code != 400 {
		t.Errorf("cross-owner move = %d", r.code)
	}

	// Viewer can read but not write.
	noteCall(t, sh.AddGrant, "alice", "POST", id(space), map[string]any{"role": "viewer", "subjectKind": "user", "subjectId": "carol"}, "")
	if r := noteCall(t, h.Get, "carol", "GET", id(page), nil, ""); r.code != 200 || noteField(t, r, "role") != "viewer" {
		t.Errorf("carol get = %d role=%v", r.code, noteField(t, r, "role"))
	}
	if r := noteCall(t, h.Update, "carol", "PATCH", id(page), map[string]any{"body": "x", "baseVersion": 2}, ""); r.code != 403 {
		t.Errorf("viewer edit = %d", r.code)
	}

	// Shared list: the space is a root with its subtree; Bob's own space is not in it.
	r = noteCall(t, h.ListShared, "bob", "GET", nil, nil, "")
	var shared []noteMeta
	_ = json.Unmarshal(r.data, &shared)
	ids := map[string]noteMeta{}
	for _, m := range shared {
		ids[m.ID] = m
	}
	if _, ok := ids[space]; !ok || ids[space].ParentID != nil || ids[page].Role != "editor" || len(shared) != 4 {
		t.Errorf("bob shared list = %+v", shared)
	}

	// "Everyone" spaces appear for every user, flagged.
	pub := mkNote(t, h, "alice", map[string]any{"title": "Company Wiki", "type": "folder"})
	noteCall(t, sh.SetVisibility, "alice", "PATCH", id(pub), map[string]any{"visibility": "public"}, "")
	noteCall(t, sh.AddGrant, "alice", "POST", id(pub), map[string]any{"role": "viewer", "subjectKind": "everyone"}, "")
	r = noteCall(t, h.ListShared, "carol", "GET", nil, nil, "")
	if !strings.Contains(string(r.data), `"id":"`+pub+`"`) || !strings.Contains(string(r.data), `"everyone":true`) {
		t.Errorf("everyone space missing for carol: %s", r.data)
	}

	// Soft delete hides the subtree (role, list, search) — owner only for the root.
	if r := noteCall(t, h.Delete, "alice", "DELETE", id(sub), nil, ""); r.code != 200 {
		t.Fatalf("delete = %d", r.code)
	}
	if got := ResolveNoteRole(db, "alice", page); got != NoRole {
		t.Errorf("role on deleted subtree = %q", got)
	}
	r = noteCall(t, h.Search, "alice", "GET", nil, nil, "q=pager")
	if string(r.data) != "[]" {
		t.Errorf("search after delete = %s", r.data)
	}
	var stillThere int64
	db.Unscoped().Model(&models.Note{}).Where("id = ?", page).Count(&stillThere)
	if stillThere != 1 {
		t.Error("delete should be soft")
	}
}

func TestNoteSearch(t *testing.T) {
	_, h, sh := noteTestDB(t)
	a := mkNote(t, h, "alice", map[string]any{"title": "Kafka consumer lag", "body": "Check lag with the kafka tool."})
	mkNote(t, h, "alice", map[string]any{"title": "Random", "body": "Something about kafka in the body only."})
	mkNote(t, h, "alice", map[string]any{"title": "Ödeme servisi", "body": "Çağrı akışı ve hatalar"})
	secret := mkNote(t, h, "bob", map[string]any{"title": "Bob kafka secrets", "body": "kafka"})

	r := noteCall(t, h.Search, "alice", "GET", nil, nil, "q=kafka")
	var hits []map[string]any
	_ = json.Unmarshal(r.data, &hits)
	if len(hits) != 2 || hits[0]["id"] != a {
		t.Fatalf("kafka hits = %s", r.data)
	}
	if _, isList := hits[0]["breadcrumb"].([]any); !isList {
		t.Errorf("breadcrumb must be a JSON array (FE reads .length), got %#v", hits[0]["breadcrumb"])
	}
	if !strings.Contains(hits[0]["snippet"].(string), "kafka") {
		t.Errorf("snippet = %q", hits[0]["snippet"])
	}
	// Prefix on the last term, diacritics folded, hostile syntax neutralised.
	for q, want := range map[string]int{"kaf": 2, "odeme": 1, "cagri akisi": 1, "ÇAĞRI": 1, `"kafka" OR NEAR(*) -lag'; DROP`: 0} {
		q := "q=" + url.QueryEscape(q)
		r = noteCall(t, h.Search, "alice", "GET", nil, nil, q)
		_ = json.Unmarshal(r.data, &hits)
		if r.code != 200 || len(hits) != want {
			t.Errorf("%s → %d hits (code %d) %s, want %d", q, len(hits), r.code, r.data, want)
		}
	}
	// Bob's note shows up for Alice only once shared.
	r = noteCall(t, h.Search, "alice", "GET", nil, nil, "q=secrets")
	if string(r.data) != "[]" {
		t.Errorf("unshared note leaked: %s", r.data)
	}
	noteCall(t, sh.SetVisibility, "bob", "PATCH", id(secret), map[string]any{"visibility": "public"}, "")
	noteCall(t, sh.AddGrant, "bob", "POST", id(secret), map[string]any{"role": "viewer", "subjectKind": "user", "subjectId": "alice"}, "")
	r = noteCall(t, h.Search, "alice", "GET", nil, nil, "q=secrets")
	if !strings.Contains(string(r.data), secret) {
		t.Errorf("shared note not found: %s", r.data)
	}
}

func TestNoteSearchTerms(t *testing.T) {
	got := notesearch.Terms(`  "Kafka" OR NEAR(x*) -lag; a 'DROP' kafka ÖDEME `)
	want := []string{"kafka", "or", "near", "lag", "drop", "odeme"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("terms = %v, want %v", got, want)
	}
}

func TestNoteAttachments(t *testing.T) {
	_, h, _ := noteTestDB(t)
	page := mkNote(t, h, "alice", map[string]any{"title": "Screens"})
	upload := func(user string, data []byte, name string) (int, map[string]any) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", name)
		fw.Write(data)
		mw.Close()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set("userID", user)
		c.Params = id(page)
		c.Request = httptest.NewRequest("POST", "/", &buf)
		c.Request.Header.Set("Content-Type", mw.FormDataContentType())
		h.UploadAttachment(c)
		var resp struct{ Data map[string]any }
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		return w.Code, resp.Data
	}
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.RGBA{255, 0, 0, 255})
	var pngBuf bytes.Buffer
	png.Encode(&pngBuf, img)

	code, data := upload("alice", pngBuf.Bytes(), "shot.png")
	if code != 201 || data["mime"] != "image/png" || !strings.HasPrefix(data["markdown"].(string), "![shot](attachment:") {
		t.Fatalf("upload = %d %v", code, data)
	}
	// Same bytes again → same attachment.
	if _, again := upload("alice", pngBuf.Bytes(), "copy.png"); again["id"] != data["id"] {
		t.Errorf("dedupe failed: %v vs %v", again["id"], data["id"])
	}
	if code, _ := upload("alice", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), "x.svg"); code != 415 {
		t.Errorf("svg upload = %d, want 415", code)
	}
	if code, _ := upload("alice", bytes.Repeat([]byte{0x89, 'P', 'N', 'G'}, 2<<20), "big.png"); code != 413 && code != 400 {
		t.Errorf("oversize upload = %d", code)
	}
	if code, _ := upload("bob", pngBuf.Bytes(), "x.png"); code != 403 {
		t.Errorf("bob upload = %d", code)
	}
	// Serving: owner yes, stranger no.
	get := func(user string) (int, string) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set("userID", user)
		c.Params = gin.Params{{Key: "attId", Value: data["id"].(string)}}
		c.Request = httptest.NewRequest("GET", "/", nil)
		h.GetAttachment(c)
		return w.Code, w.Header().Get("Content-Type")
	}
	if code, ct := get("alice"); code != 200 || ct != "image/png" {
		t.Errorf("alice get = %d %s", code, ct)
	}
	if code, _ := get("bob"); code != 404 {
		t.Errorf("bob get = %d", code)
	}
}

// Writes made through MCP are attributed to the agent in the history, and the
// realtime audience walks up to every ancestor's grants.
func TestNoteSourceAndRecipients(t *testing.T) {
	db, h, sh := noteTestDB(t)
	space := mkNote(t, h, "alice", map[string]any{"title": "S", "type": "folder"})
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", "alice")
	c.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{"title":"Agent log","parentId":"`+space+`"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set(mcpClientHeader, mcpClientHeaderValue)
	h.Create(c)
	var resp struct{ Data struct{ ID string } }
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	var rev models.NoteRevision
	db.Where("note_id = ?", resp.Data.ID).First(&rev)
	if rev.Source != noteSourceMCP {
		t.Errorf("MCP-created revision source = %q", rev.Source)
	}

	noteCall(t, sh.SetVisibility, "alice", "PATCH", id(space), map[string]any{"visibility": "public"}, "")
	noteCall(t, sh.AddGrant, "alice", "POST", id(space), map[string]any{"role": "viewer", "subjectKind": "user", "subjectId": "bob"}, "")
	recips, everyone := realtimeNoteRecipients(db, resp.Data.ID)
	if everyone || !contains(recips, "alice") || !contains(recips, "bob") || contains(recips, "carol") {
		t.Errorf("recipients = %v everyone=%v", recips, everyone)
	}
	noteCall(t, sh.AddGrant, "alice", "POST", id(space), map[string]any{"role": "viewer", "subjectKind": "everyone"}, "")
	if _, everyone := realtimeNoteRecipients(db, resp.Data.ID); !everyone {
		t.Error("everyone grant on an ancestor should broadcast")
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

var realtimeNoteRecipients = realtime.NoteRecipients
