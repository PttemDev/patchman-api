package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/notecards"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

func seedTeams(db *gorm.DB) {
	db.Exec("INSERT INTO teams (id, name) VALUES ('t1','Backend'),('t2','Ops')")
	db.Exec("INSERT INTO team_members (team_id, user_id) VALUES ('t1','alice'),('t2','alice'),('t1','bob'),('t2','carol')")
}

func shareWithTeam(t *testing.T, sh *NoteSharingHandler, folder, team string) {
	t.Helper()
	noteCall(t, sh.SetVisibility, "alice", "PATCH", id(folder), map[string]any{"visibility": "public"}, "")
	if r := noteCall(t, sh.AddGrant, "alice", "POST", id(folder), map[string]any{"role": "editor", "subjectKind": "team", "subjectId": team}, ""); r.code >= 300 {
		t.Fatalf("grant = %d %s", r.code, errOf(r))
	}
}

// rememberNow queues a memory and runs the worker; the card's folder comes back.
func rememberNow(t *testing.T, db *gorm.DB, h *NoteHandler, user string, body map[string]any) (int, string, string) {
	t.Helper()
	r := mcpCall(t, h.Remember, user, "POST", nil, body, "")
	if r.code != 202 && r.code != 200 {
		return r.code, errOf(r), ""
	}
	h.processInbox()
	var item models.NoteInbox
	db.Where("id = ?", noteField(t, r, "inboxId")).First(&item)
	return r.code, item.Status, item.FolderID
}

func TestMemoryScopesAndTeamPicker(t *testing.T) {
	db, h, sh := noteTestDB(t)
	seedTeams(db)
	_, _, personal := rememberNow(t, db, h, "alice", map[string]any{"text": "Alice's personal fact about caching."})
	backend := mkNote(t, h, "alice", map[string]any{"title": "Backend memory", "type": "folder", "memory": true})
	ops := mkNote(t, h, "alice", map[string]any{"title": "Ops memory", "type": "folder", "memory": true})
	shareWithTeam(t, sh, backend, "t1")
	// A grant on a private folder grants nothing, so it stays personal.
	db.Create(&models.NotePublicGrant{ID: uuid.New().String(), NoteID: ops, Role: "editor", SubjectKind: "team", SubjectID: ptr("t2")})

	r := noteCall(t, h.MemoryOverview, "alice", "GET", nil, nil, "")
	var ov struct {
		Folders []memoryFolderView `json:"folders"`
	}
	_ = json.Unmarshal(r.data, &ov)
	kinds := map[string]string{}
	for _, f := range ov.Folders {
		kinds[f.ID] = f.Scope.Label()
	}
	if len(ov.Folders) != 3 || kinds[personal] != "" || kinds[backend] != "team: Backend" || kinds[ops] != "" {
		t.Fatalf("scopes = %v", kinds)
	}

	// One team memory: used without asking.
	if code, status, folder := rememberNow(t, db, h, "alice", map[string]any{"text": "Backend deploys go through ArgoCD.", "scope": "team"}); code != 202 || status != "done" || folder != backend {
		t.Errorf("team remember = %d %s → %s", code, status, folder)
	}
	// Bob (Backend) writes to Alice's team memory; Carol (Ops) has none.
	if _, _, folder := rememberNow(t, db, h, "bob", map[string]any{"text": "Backend on-call rotates on Mondays.", "scope": "team"}); folder != backend {
		t.Errorf("bob's team memory = %s", folder)
	}
	if code, msg, _ := rememberNow(t, db, h, "carol", map[string]any{"text": "Ops thing.", "scope": "team"}); code != 400 || !strings.Contains(msg, "No team memory") {
		t.Errorf("carol = %d %s", code, msg)
	}

	// Two team memories: the caller must pick, or set a default.
	noteCall(t, sh.SetVisibility, "alice", "PATCH", id(ops), map[string]any{"visibility": "private"}, "")
	shareWithTeam(t, sh, ops, "t2")
	code, msg, _ := rememberNow(t, db, h, "alice", map[string]any{"text": "Which one?", "scope": "team"})
	if code != 400 || !strings.Contains(msg, "pass folderId") || !strings.Contains(msg, ops) || !strings.Contains(msg, backend) {
		t.Errorf("ambiguous team = %d %s", code, msg)
	}
	if r := noteCall(t, h.UpdateMemorySettings, "alice", "PUT", nil, map[string]any{"teamFolderId": personal}, ""); r.code != 400 {
		t.Errorf("personal folder accepted as the team default: %d", r.code)
	}
	noteCall(t, h.UpdateMemorySettings, "alice", "PUT", nil, map[string]any{"teamFolderId": ops}, "")
	if _, _, folder := rememberNow(t, db, h, "alice", map[string]any{"text": "Ops pages go to #ops-alerts.", "scope": "team"}); folder != ops {
		t.Errorf("default team memory not used: %s", folder)
	}
	if _, _, folder := rememberNow(t, db, h, "alice", map[string]any{"text": "Explicit folder wins.", "scope": "team", "folderId": backend}); folder != backend {
		t.Errorf("folderId ignored: %s", folder)
	}

	// recall tags whose memory a hit is in, and can stay in one kind.
	text := recallText(t, h, "bob", "argocd", "400")
	if !strings.Contains(text, "[team: Backend]") {
		t.Errorf("bob's recall untagged:\n%s", text)
	}
	if text := recallText(t, h, "alice", "caching", "400"); strings.Contains(text, "[") {
		t.Errorf("personal hit tagged:\n%s", text)
	}
	if r := noteCall(t, h.Recall, "alice", "GET", nil, nil, "q=argocd&scope=personal"); noteField(t, r, "text") != notecards.NoMatch {
		t.Errorf("scope=personal returned a team card: %v", noteField(t, r, "text"))
	}

	// Only the owner turns a folder into a memory (or back).
	if r := noteCall(t, h.Update, "bob", "PATCH", id(backend), map[string]any{"memory": false}, ""); r.code != 403 {
		t.Errorf("editor toggled memory: %d", r.code)
	}
	if r := noteCall(t, h.Update, "alice", "PATCH", id(backend), map[string]any{"memory": false}, ""); r.code != 200 {
		t.Errorf("owner toggle = %d %s", r.code, errOf(r))
	}
}

func ptr(s string) *string { return &s }

func TestSuggestionsAcceptAndReject(t *testing.T) {
	db, h, _ := noteTestDB(t)
	space := mkNote(t, h, "alice", map[string]any{"title": "Ops", "type": "folder", "memory": true})
	human := mkNote(t, h, "alice", map[string]any{"title": "Deploy rule", "type": "card", "parentId": space,
		"summary": "Prod deploys need a change ticket.", "body": "Open a ticket first."})
	other := mkNote(t, h, "alice", map[string]any{"title": "Change tickets", "type": "card", "parentId": space})
	var n models.Note
	db.Where("id = ?", human).First(&n)
	mcpCall(t, h.Update, "alice", "PATCH", id(human), map[string]any{"body": "Open a ticket and get one approval.", "baseVersion": n.Version}, "")
	mcpCall(t, h.AddLink, "alice", "POST", id(human), map[string]any{"dst": other, "type": "depends_on"}, "")
	_, _, _ = rememberNow(t, db, h, "alice", map[string]any{"text": "Deploy window\nNo deploys on Friday afternoons."})
	var claudeCard models.Note
	db.Where("title = ?", "Deploy window").First(&claudeCard)
	mcpCall(t, h.AddLink, "alice", "POST", id(claudeCard.ID), map[string]any{"dst": human, "type": "supersedes"}, "")
	rememberNow(t, db, h, "alice", map[string]any{"text": "Deploy window\nHotfixes are exempt."})

	list := func(user string) map[string]suggestionView {
		r := noteCall(t, h.Suggestions, user, "GET", nil, nil, "")
		var rows []suggestionView
		_ = json.Unmarshal(r.data, &rows)
		out := map[string]suggestionView{}
		for _, s := range rows {
			out[s.Kind] = s
		}
		return out
	}
	got := list("alice")
	if len(got) != 4 || got["link"].Target["title"] != "Change tickets" || got["supersede"].Target["title"] != "Deploy window" {
		t.Fatalf("suggestions = %+v", got)
	}
	if len(list("bob")) != 0 {
		t.Error("bob sees alice's suggestions")
	}

	// A person edited the card after Claude suggested: accepting saves on top
	// of the current version (the UI showed both), no conflict.
	db.Where("id = ?", human).First(&n)
	noteCall(t, h.Update, "alice", "PATCH", id(human), map[string]any{"body": "Open a ticket (human edit).", "baseVersion": n.Version}, "")
	if cur := list("alice")["edit"].Current["body"]; cur != "Open a ticket (human edit)." {
		t.Errorf("current shown = %v", cur)
	}
	accept := func(kind string, body map[string]any) noteResp {
		return noteCall(t, h.AcceptSuggestion, "alice", "POST", id(got[kind].ID), body, "")
	}
	if r := accept("edit", nil); r.code != 200 {
		t.Fatalf("accept edit = %d %s", r.code, errOf(r))
	}
	db.Where("id = ?", human).First(&n)
	if n.Body != "Open a ticket and get one approval." || n.Source != "human" {
		t.Errorf("after edit: %q %s", n.Body, n.Source)
	}
	if r := accept("link", nil); r.code != 200 {
		t.Fatalf("accept link = %d %s", r.code, errOf(r))
	}
	var edge models.NoteLink
	if db.Where("src_note_id = ? AND dst_note_id = ? AND type = ?", human, other, "depends_on").First(&edge).Error != nil || edge.Source != "agent" {
		t.Errorf("edge = %+v", edge)
	}
	accept("supersede", nil)
	db.Where("id = ?", human).First(&n)
	if !n.Stale || n.SupersededBy == nil || *n.SupersededBy != claudeCard.ID {
		t.Errorf("supersede not applied: %v %v", n.Stale, n.SupersededBy)
	}
	if r := accept("merge", map[string]any{"body": "No deploys on Friday afternoons; hotfixes are exempt."}); r.code != 200 {
		t.Fatalf("accept merge = %d %s", r.code, errOf(r))
	}
	db.Where("id = ?", claudeCard.ID).First(&claudeCard)
	if claudeCard.Body != "No deploys on Friday afternoons; hotfixes are exempt." {
		t.Errorf("merged body = %q", claudeCard.Body)
	}
	if r := accept("merge", nil); r.code != 404 {
		t.Errorf("decided twice: %d", r.code)
	}
	if len(list("alice")) != 0 {
		t.Errorf("open after deciding: %v", list("alice"))
	}

	// Reject leaves the note alone.
	mcpCall(t, h.Update, "alice", "PATCH", id(other), map[string]any{"summary": "Tickets live in Jira."}, "")
	rej := list("alice")["edit"]
	if r := noteCall(t, h.RejectSuggestion, "alice", "POST", id(rej.ID), nil, ""); r.code != 200 {
		t.Fatalf("reject = %d", r.code)
	}
	var o models.Note
	db.Where("id = ?", other).First(&o)
	if o.Summary != "" {
		t.Errorf("rejected edit applied: %q", o.Summary)
	}
}

// fakeLLM is an OpenAI-compatible endpoint answering from a script.
type fakeLLM struct {
	mu      sync.Mutex
	replies []string
	calls   int
	delay   time.Duration
}

func (f *fakeLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	reply := ""
	if f.calls < len(f.replies) {
		reply = f.replies[f.calls]
	}
	f.calls++
	delay := f.delay
	f.mu.Unlock()
	time.Sleep(delay)
	_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": reply}}}})
}

func TestRememberSplitsWithModel(t *testing.T) {
	db, h, _ := noteTestDB(t)
	if err := db.AutoMigrate(&models.LLMProvider{}); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{}
	srv := httptest.NewServer(llm)
	defer srv.Close()
	db.Create(&models.LLMProvider{ID: "p1", UserID: "alice", Name: "local", Kind: "openai", BaseURL: srv.URL, Model: "m"})
	noteCall(t, h.UpdateMemorySettings, "alice", "PUT", nil, map[string]any{"llmProviderId": "p1"}, "")
	h.SetLLM(nil)

	mem := mkNote(t, h, "alice", map[string]any{"title": "Agent Memory", "type": "folder", "memory": true})
	session := mkNote(t, h, "alice", map[string]any{"title": "Session store", "type": "card", "parentId": mem, "summary": "Sessions live in Redis."})
	old := mkNote(t, h, "alice", map[string]any{"title": "Old rotation", "type": "card", "parentId": mem, "summary": "Tokens never rotated."})
	_, _, _ = rememberNow(t, db, h, "alice", map[string]any{"text": "Rate limits\nAPI is limited to 100 rps."}) // no model reply → fallback
	var rate models.Note
	db.Where("title = ?", "Rate limits").First(&rate)

	sh := func(id string) string { return "#" + notecards.ShortID(id) }
	llm.replies = []string{"```json\n" + `{"cards":[
		{"title":"Token rotation","summary":"Refresh tokens are single-use.","body":"Each refresh revokes the old token.",
		 "links":[{"to":"card:2","type":"depends_on","confidence":0.97},{"to":"` + sh(session) + `","type":"related","confidence":0.5},
		          {"to":"` + sh(old) + `","type":"supersedes","confidence":0.99},{"to":"#deadbeef","type":"related","confidence":1}]},
		{"title":"Rotation store","summary":"Used tokens are kept in Redis for 30 days.","body":"Key rt:used:<id>."},
		{"title":"Rate limit","duplicate_of":"` + sh(rate.ID) + `","body":"Burst of 200 allowed."}]}` + "\n```"}
	llm.calls = 0
	rememberNow(t, db, h, "alice", map[string]any{"text": "Token rotation: refresh tokens are single-use, used ones kept in Redis 30 days; rate limit burst is 200."})

	var rot, store models.Note
	if db.Where("title = ?", "Token rotation").First(&rot).Error != nil || db.Where("title = ?", "Rotation store").First(&store).Error != nil {
		t.Fatal("model cards not created")
	}
	if rot.Summary != "Refresh tokens are single-use." || rot.Source != "agent" || rot.Agent != "Claude Code" || *rot.ParentID != mem {
		t.Errorf("card = %+v", rot)
	}
	var edges int64
	db.Model(&models.NoteLink{}).Where("src_note_id = ? AND dst_note_id = ? AND type = ?", rot.ID, store.ID, "depends_on").Count(&edges)
	sug := map[string]int{}
	var rows []models.NoteSuggestion
	db.Find(&rows)
	for _, s := range rows {
		sug[s.Kind+":"+s.NoteID]++
	}
	if edges != 1 || sug["link:"+rot.ID] != 1 || sug["supersede:"+old] != 1 || sug["merge:"+rate.ID] != 1 || len(rows) != 3 {
		t.Errorf("edges=%d suggestions=%v", edges, sug)
	}
	var stale models.Note
	db.Where("id = ?", old).First(&stale)
	if stale.Stale {
		t.Error("the model superseded a card without a person")
	}

	// Unusable twice → one card the old way, after exactly one retry.
	llm.replies, llm.calls = []string{"Sure! Here are your cards.", "{\"cards\": []}"}, 0
	rememberNow(t, db, h, "alice", map[string]any{"text": "Cache TTL\nThe edge cache keeps pages 5 minutes."})
	var ttl models.Note
	if db.Where("title = ?", "Cache TTL").First(&ttl).Error != nil || llm.calls != 2 || ttl.Summary != "The edge cache keeps pages 5 minutes." {
		t.Errorf("fallback: calls=%d card=%+v", llm.calls, ttl)
	}

	// A stalled provider doesn't hold the queue: timeout → fallback.
	llm.replies, llm.calls, llm.delay = nil, 0, 300*time.Millisecond
	h.llmHTTP = &http.Client{Timeout: 50 * time.Millisecond}
	start := time.Now()
	rememberNow(t, db, h, "alice", map[string]any{"text": "Queue limits\nThe import queue takes 50 jobs."})
	var q models.Note
	if db.Where("title = ?", "Queue limits").First(&q).Error != nil || time.Since(start) > 2*time.Second {
		t.Errorf("timeout fallback: found=%v after %s", q.ID != "", time.Since(start))
	}
}
