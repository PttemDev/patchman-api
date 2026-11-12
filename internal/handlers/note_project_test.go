package handlers

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// rememberIn sends remember as an MCP agent working in project (header, as
// the bridge sends it) and files it at once.
func rememberIn(t *testing.T, db *gorm.DB, h *NoteHandler, user, project string, body map[string]any) models.NoteInbox {
	t.Helper()
	r := mcpCallH(t, h.Remember, user, body, map[string]string{projectHeader: project})
	if r.code != 202 && r.code != 200 {
		t.Fatalf("remember = %d %s", r.code, errOf(r))
	}
	h.processInbox()
	var item models.NoteInbox
	db.Where("id = ?", noteField(t, r, "inboxId")).First(&item)
	return item
}

func TestRememberFilesUnderProjectFolder(t *testing.T) {
	db, h, sh := noteTestDB(t)
	seedTeams(db)
	a := rememberIn(t, db, h, "alice", "getman", map[string]any{"text": "Release order\nPush test before main, fast-forward only."})
	b := rememberIn(t, db, h, "alice", "GetMan", map[string]any{"text": "Changelog per push\nEvery push updates changelog.ts."})
	c := rememberIn(t, db, h, "alice", "crm", map[string]any{"text": "CRM tenants\nTwo Postgres databases: platform and tenants."})
	none := rememberIn(t, db, h, "alice", "", map[string]any{"text": "A fact without a project."})

	var pa, pc models.Note
	db.Where("id = ?", a.FolderID).First(&pa)
	db.Where("id = ?", c.FolderID).First(&pc)
	if pa.Project != "getman" || pa.Title != "getman" || pa.Type != "folder" || b.FolderID != a.FolderID {
		t.Fatalf("project folder = %+v (b in %s)", pa, b.FolderID)
	}
	if pc.Project != "crm" || pc.ID == pa.ID || *pc.ParentID != *pa.ParentID {
		t.Fatalf("crm folder = %+v", pc)
	}
	var root models.Note
	db.Where("id = ?", none.FolderID).First(&root)
	if !root.Memory || *pa.ParentID != root.ID {
		t.Fatalf("no-project card should sit in the memory root %s, project folders under it (%v)", root.ID, *pa.ParentID)
	}

	// The overview counts every project's cards and lists the projects.
	r := noteCall(t, h.MemoryOverview, "alice", "GET", nil, nil, "")
	var ov struct {
		Folders []memoryFolderView `json:"folders"`
	}
	_ = json.Unmarshal(r.data, &ov)
	if len(ov.Folders) != 1 || ov.Folders[0].Cards != 4 || len(ov.Folders[0].Projects) != 2 {
		t.Fatalf("overview = %+v", ov.Folders)
	}

	// A merge suggestion on a project card lands in the memory's inbox.
	rememberIn(t, db, h, "alice", "getman", map[string]any{"text": "Release order\nAlso tag the commit."})
	r = noteCall(t, h.Suggestions, "alice", "GET", nil, nil, "folderId="+root.ID)
	var sugg []suggestionView
	_ = json.Unmarshal(r.data, &sugg)
	if len(sugg) != 1 || sugg[0].Kind != "merge" {
		t.Fatalf("suggestions for the memory = %+v", sugg)
	}

	// Recall: the current project's card first, other projects tagged.
	text := noteField(t, mcpCallQ(t, h.Recall, "alice", "q=postgres+push+changelog", map[string]string{projectHeader: "crm"}), "text").(string)
	if !strings.HasPrefix(text, "#") || !strings.Contains(strings.SplitN(text, "\n", 2)[0], "CRM tenants") {
		t.Errorf("current project's card should lead:\n%s", text)
	}
	if !strings.Contains(text, "[project: getman]") || strings.Contains(text, "[project: crm]") {
		t.Errorf("other projects should be tagged, the current one not:\n%s", text)
	}

	// A team member sees the shared memory's project cards in recall and in
	// the graph; a stranger sees nothing.
	shareWithTeam(t, sh, root.ID, "t1")
	if text := recallText(t, h, "bob", "changelog", "800"); !strings.Contains(text, "Changelog per push") {
		t.Errorf("bob (team) should recall the project card:\n%s", text)
	}
	g := graphOf(t, h, "bob", "")
	if g.kinds["card"] != 4 || g.kinds["project"] != 2 || g.kinds["memory"] != 1 {
		t.Errorf("bob's graph = %v", g.kinds)
	}
	if g := graphOf(t, h, "carol", ""); len(g.Nodes) != 0 {
		t.Errorf("carol (no access) graph = %v", g.kinds)
	}
	if r := noteCall(t, h.MemoryGraph, "carol", "GET", nil, nil, "root="+root.ID); r.code != 404 {
		t.Errorf("carol asking for alice's memory = %d", r.code)
	}
}

type graphResp struct {
	Nodes []graphNode `json:"nodes"`
	Links []graphLink `json:"links"`
	kinds map[string]int
}

func graphOf(t *testing.T, h *NoteHandler, user, root string) graphResp {
	t.Helper()
	q := ""
	if root != "" {
		q = "root=" + root
	}
	r := noteCall(t, h.MemoryGraph, user, "GET", nil, nil, q)
	if r.code != 200 {
		t.Fatalf("graph = %d %s", r.code, errOf(r))
	}
	var g graphResp
	_ = json.Unmarshal(r.data, &g)
	g.kinds = map[string]int{}
	for _, n := range g.Nodes {
		g.kinds[n.Kind]++
	}
	return g
}

func TestMemoryGraphLinksAndOutsideNotes(t *testing.T) {
	db, h, _ := noteTestDB(t)
	a := rememberIn(t, db, h, "alice", "getman", map[string]any{"text": "Tauri signing\nThe updater key password is an empty string.", "context": "patchman://request/42"})
	b := rememberIn(t, db, h, "alice", "getman", map[string]any{"text": "Notarization\nNotarize after signing; see [[Tauri signing]]."})
	page := mkNote(t, h, "alice", map[string]any{"title": "Release runbook", "body": "steps"})
	bobPage := mkNote(t, h, "bob", map[string]any{"title": "Bob's private page", "body": "x"})
	mcpCall(t, h.AddLink, "alice", "POST", id(a.NoteID), map[string]any{"dst": page, "type": "refers_to"}, "")
	// A link to a note alice can't read must not leak it.
	db.Create(&models.NoteLink{ID: "l-bob", SrcNoteID: a.NoteID, DstNoteID: bobPage, Type: "related", Source: "agent"})

	g := graphOf(t, h, "alice", "")
	byID := map[string]graphNode{}
	for _, n := range g.Nodes {
		byID[n.ID] = n
	}
	if byID[page].Kind != "page" || !byID[page].Outside {
		t.Errorf("linked page = %+v", byID[page])
	}
	if _, leaked := byID[bobPage]; leaked {
		t.Error("bob's page leaked into alice's graph")
	}
	if byID["patchman://request/42"].Kind != "entity" {
		t.Errorf("entity node missing: %v", g.kinds)
	}
	if byID[b.NoteID].Project == "" || byID[b.NoteID].Project != byID[a.NoteID].Project || byID[b.NoteID].Memory == "" {
		t.Errorf("project/memory not propagated: %+v", byID[b.NoteID])
	}
	wiki := false
	for _, l := range g.Links {
		if l.Src == b.NoteID && l.Dst == a.NoteID && l.Type == "related" {
			wiki = true
		}
		if l.Dst == bobPage {
			t.Error("link to an unreadable note kept")
		}
	}
	if !wiki {
		t.Errorf("[[Tauri signing]] should link the cards: %+v", g.Links)
	}
}

func TestRememberAnswersWithTheCardAndGuardsFolders(t *testing.T) {
	db, h, _ := noteTestDB(t)
	h.StartCardWorkers()
	r := mcpCallH(t, h.Remember, "alice", map[string]any{"text": "Sidecar keep-alive\nThe proxy disables keep-alives to stop fd leaks."}, map[string]string{projectHeader: "getman"})
	card, _ := noteField(t, r, "card").(map[string]any)
	if r.code != 200 || card == nil || !strings.HasPrefix(card["short"].(string), "#") || noteField(t, r, "outcome") != "created" || noteField(t, r, "project") != "getman" {
		t.Fatalf("remember answer = %d %s", r.code, r.data)
	}
	var n models.Note
	if db.Where("id = ?", card["id"]).First(&n).Error != nil || n.Type != "card" {
		t.Fatalf("answered card %v not found", card["id"])
	}
	r = mcpCallH(t, h.Remember, "alice", map[string]any{"text": "Sidecar keep-alive\nThe proxy disables keep-alives to stop fd leaks."}, map[string]string{projectHeader: "getman"})
	if noteField(t, r, "outcome") != "duplicate" {
		t.Errorf("same text again = %s", r.data)
	}

	// 10000 characters is the limit, not 10000 bytes; past it the answer
	// tells the agent how to split.
	if r := mcpCallH(t, h.Remember, "alice", map[string]any{"text": strings.Repeat("ş", 9000)}, nil); r.code == 413 || !strings.Contains(noteField(t, r, "hint").(string), "split") {
		t.Errorf("9000 characters: want saved with a split hint, got %d %s", r.code, r.data)
	}
	if r := mcpCallH(t, h.Remember, "alice", map[string]any{"text": "Short card\nfine."}, nil); noteField(t, r, "hint") != nil {
		t.Errorf("a short card got a hint: %s", r.data)
	}
	if r := mcpCallH(t, h.Remember, "alice", map[string]any{"text": strings.Repeat("ş", 10001)}, nil); r.code != 413 || !strings.Contains(errOf(r), "Split it") {
		t.Errorf("10001 characters = %d %s", r.code, errOf(r))
	}
	// folderId must be (inside) a memory.
	plain := mkNote(t, h, "alice", map[string]any{"title": "Runbooks", "type": "folder"})
	if r := mcpCallH(t, h.Remember, "alice", map[string]any{"text": "x y z", "folderId": plain}, nil); r.code != 400 {
		t.Errorf("plain folder accepted: %d", r.code)
	}
}

func TestRecallWidensToAnyTermAndPrintsTheMatch(t *testing.T) {
	db, h, _ := noteTestDB(t)
	rememberIn(t, db, h, "alice", "getman", map[string]any{"text": "Web bundle: Monaco lazy-loaded\nThe entry chunk went from 10.96MB to 4.86MB once Monaco loaded lazily."})
	rememberIn(t, db, h, "alice", "getman", map[string]any{"text": "Stale-chunk reload (1.43.1)\nUse lazyWithReload for every React.lazy."})
	long := "Agent memory cards\nCards are notes of type card.\n\n" + strings.Repeat("Unrelated filler paragraph about folders and sharing.\n\n", 40) +
		"The agent's name comes from MCP initialize clientInfo, stored on the token."
	rememberIn(t, db, h, "alice", "getman", map[string]any{"text": long})

	if text := recallText(t, h, "alice", "Monaco+bundle+size", "800"); !strings.Contains(text, "Monaco lazy-loaded") {
		t.Errorf("one extra word emptied the answer:\n%s", text)
	}
	if text := recallText(t, h, "alice", "1.43.1", "800"); !strings.Contains(text, "Stale-chunk reload") {
		t.Errorf("version not found:\n%s", text)
	}
	rememberIn(t, db, h, "alice", "getman", map[string]any{"text": "Env-var {{var}} input\nVariableInput highlights {{var}} tokens."})
	if text := recallText(t, h, "alice", "var", "800"); !strings.Contains(text, "Env-var {{var}} input") {
		t.Errorf("an identifier that looks like a stop word must still be found:\n%s", text)
	}
	if text := recallText(t, h, "alice", "agent+name+clientInfo", "800"); !strings.Contains(text, "clientInfo, stored on the token") {
		t.Errorf("the matching paragraph should be printed, not the head:\n%s", text)
	}
}

// mcpCallH is mcpCall with extra request headers.
func mcpCallH(t *testing.T, fn gin.HandlerFunc, user string, body map[string]any, headers map[string]string) noteResp {
	t.Helper()
	return mcpCallWith(t, fn, user, "POST", body, "", headers)
}

// mcpCallQ is a GET as an MCP agent with a query and headers.
func mcpCallQ(t *testing.T, fn gin.HandlerFunc, user, query string, headers map[string]string) noteResp {
	t.Helper()
	return mcpCallWith(t, fn, user, "GET", nil, query, headers)
}

func mcpCallWith(t *testing.T, fn gin.HandlerFunc, user, method string, body any, query string, headers map[string]string) noteResp {
	t.Helper()
	gin.SetMode(gin.TestMode)
	b, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", user)
	c.Request = httptest.NewRequest(method, "/", bytes.NewReader(b))
	c.Request.URL.RawQuery = query
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set(mcpClientHeader, mcpClientHeaderValue)
	c.Request.Header.Set(agentHeader, "Claude Code")
	for k, v := range headers {
		c.Request.Header.Set(k, v)
	}
	fn(c)
	r := noteResp{code: w.Code}
	_ = json.Unmarshal(w.Body.Bytes(), &r.raw)
	r.data = r.raw["data"]
	return r
}

// A suggestion on a note outside agent memory is filed on its own folder.
func TestSuggestionOutsideMemoryKeepsItsFolder(t *testing.T) {
	db, h, _ := noteTestDB(t)
	space := mkNote(t, h, "alice", map[string]any{"title": "Runbooks", "type": "folder"})
	page := mkNote(t, h, "alice", map[string]any{"title": "Deploy", "body": "steps", "parentId": space})
	other := mkNote(t, h, "alice", map[string]any{"title": "Rollback", "body": "x", "parentId": space})
	if r := mcpCall(t, h.AddLink, "alice", "POST", id(page), map[string]any{"dst": other, "type": "related"}, ""); r.code != 202 {
		t.Fatalf("agent link on a person's page = %d", r.code)
	}
	var s models.NoteSuggestion
	db.First(&s)
	if s.FolderID != space {
		t.Errorf("suggestion folder = %q, want %q", s.FolderID, space)
	}
}

// Agents may link two projects directly (project folders are theirs).
func TestAgentLinksProjects(t *testing.T) {
	db, h, _ := noteTestDB(t)
	a := rememberIn(t, db, h, "alice", "getman", map[string]any{"text": "A\nfirst."})
	b := rememberIn(t, db, h, "alice", "crm", map[string]any{"text": "B\nsecond."})
	if r := mcpCall(t, h.AddLink, "alice", "POST", id(b.FolderID), map[string]any{"dst": a.FolderID, "type": "depends_on"}, ""); r.code != 201 {
		t.Fatalf("project link = %d %s", r.code, r.data)
	}
	g := graphOf(t, h, "alice", "")
	found := false
	for _, l := range g.Links {
		found = found || l.Src == b.FolderID && l.Dst == a.FolderID && l.Type == "depends_on"
	}
	if !found {
		t.Errorf("project link not in the graph: %+v", g.Links)
	}
}

// Lists say who a node is shared with, and a shared root whose it is.
func TestListsCarrySharingAndOwner(t *testing.T) {
	db, h, sh := noteTestDB(t)
	seedTeams(db)
	space := mkNote(t, h, "alice", map[string]any{"title": "Team KB", "type": "folder", "memory": true})
	shareWithTeam(t, sh, space, "t1")
	mkNote(t, h, "alice", map[string]any{"title": "Runbook", "body": "x", "parentId": space})

	var own []noteMeta
	_ = json.Unmarshal(noteCall(t, h.List, "alice", "GET", nil, nil, "").data, &own)
	for _, n := range own {
		if n.ID == space && (len(n.SharedWith) != 1 || n.SharedWith[0].Kind != "team" || n.SharedWith[0].Name != "Backend" || n.SharedWith[0].Role != "editor") {
			t.Errorf("own shared folder tags = %+v", n.SharedWith)
		}
	}
	var shared []noteMeta
	_ = json.Unmarshal(noteCall(t, h.ListShared, "bob", "GET", nil, nil, "").data, &shared)
	found := false
	for _, n := range shared {
		if n.ID == space {
			found = true
			if n.OwnerName != "Alice" || len(n.SharedWith) != 1 {
				t.Errorf("shared root = owner %q tags %+v", n.OwnerName, n.SharedWith)
			}
		} else if n.OwnerName != "" {
			t.Errorf("only the shared root names its owner, not %q", n.Title)
		}
	}
	if !found {
		t.Fatal("bob doesn't see the shared memory")
	}
}

// recall(from: person) answers only from that person's notes the caller can read.
func TestRecallFromAPerson(t *testing.T) {
	db, h, sh := noteTestDB(t)
	seedTeams(db)
	rememberIn(t, db, h, "alice", "oms", map[string]any{"text": "Kafka retry policy\nOrder events retry three times, then go to the DLQ."})
	var root models.Note
	db.Where("user_id = ? AND memory = ?", "alice", true).First(&root)
	shareWithTeam(t, sh, root.ID, "t1")
	rememberIn(t, db, h, "bob", "oms", map[string]any{"text": "Kafka retry notes\nBob thinks retries should be five."})
	rememberIn(t, db, h, "carol", "", map[string]any{"text": "Kafka retry private\nCarol's private take on retries."})

	from := func(user, who string) string {
		return noteField(t, noteCall(t, h.Recall, user, "GET", nil, nil, "q=kafka+retry&from="+who), "text").(string)
	}
	if got := from("bob", "alice"); !strings.Contains(got, "DLQ") || strings.Contains(got, "five") {
		t.Errorf("bob asking Alice's memory:\n%s", got)
	}
	if got := from("bob", "carol"); strings.Contains(got, "Carol") {
		t.Errorf("Carol's private card leaked:\n%s", got)
	}
	if got := from("bob", "nobody-here"); !strings.Contains(got, "No one called") {
		t.Errorf("unknown person: %s", got)
	}
	if got := from("bob", "bob"); !strings.Contains(got, "five") || strings.Contains(got, "DLQ") {
		t.Errorf("own memory only:\n%s", got)
	}
}
