package handlers

import (
	"encoding/json"
	"strings"
	"testing"

	"patchman-be/internal/models"
)

// remember(key) keeps one card per source: created, then unchanged, then
// replaced in place (same id, new title/summary/body, version up); a card
// written before keys existed is adopted by its title.
func TestRememberKeyedUpsert(t *testing.T) {
	db, h, _ := noteTestDB(t)
	send := func(text, key string) noteResp {
		r := mcpCall(t, h.Remember, "alice", "POST", nil, map[string]any{"text": text, "key": key, "project": "getman"}, "")
		h.processInbox()
		return r
	}
	outcome := func(key string) (models.Note, string) {
		var item models.NoteInbox
		db.Where("source_key = ?", key).Order("created_at DESC").First(&item)
		var n models.Note
		db.Where("id = ?", item.NoteID).First(&n)
		return n, item.Outcome
	}
	send("FE typecheck\nRun tsc -p tsconfig.app.json. The root config checks nothing.", "mem/fe.md")
	first, out := outcome("mem/fe.md")
	if out != "created" || first.SourceKey != "mem/fe.md" || first.Summary != "Run tsc -p tsconfig.app.json." {
		t.Fatalf("created: %s %+v", out, first)
	}
	send("FE typecheck\nRun tsc -p tsconfig.app.json. The root config checks nothing.", "mem/fe.md")
	if _, out = outcome("mem/fe.md"); out != "unchanged" {
		t.Errorf("same text again: %s", out)
	}
	send("FE typecheck command\nUse tsc -b (as npm run build does). The root config checks nothing.", "mem/fe.md")
	upd, out := outcome("mem/fe.md")
	if out != "updated" || upd.ID != first.ID || upd.Title != "FE typecheck command" || upd.Summary != "Use tsc -b (as npm run build does)." || upd.Version != first.Version+1 {
		t.Errorf("updated in place: %s %+v", out, upd)
	}
	var cards int64
	db.Model(&models.Note{}).Where("type = ?", "card").Count(&cards)
	if cards != 1 {
		t.Errorf("one card per key, have %d", cards)
	}
	// A legacy card (no key) with the same title adopts the key.
	send("Monaco lazy-loaded\nPending: lazy-load Monaco.", "")
	send("Monaco lazy-loaded\nDone in 1.37. Never call loader.init().", "mem/monaco.md")
	adopted, out := outcome("mem/monaco.md")
	if out != "adopted" || !strings.Contains(adopted.Body, "loader.init") || adopted.Summary != "Done in 1.37." {
		t.Errorf("adopt: %s %+v", out, adopted)
	}
	db.Model(&models.Note{}).Where("type = ?", "card").Count(&cards)
	if cards != 2 {
		t.Errorf("adoption must not add a card: %d", cards)
	}
	// Another user's key never matches.
	r := mcpCall(t, h.Remember, "bob", "POST", nil, map[string]any{"text": "FE typecheck\nBob's own.", "key": "mem/fe.md"}, "")
	h.processInbox()
	if r.code >= 400 {
		t.Fatalf("bob: %d", r.code)
	}
	var mine models.Note
	db.Where("id = ?", first.ID).First(&mine)
	if strings.Contains(mine.Body, "Bob") {
		t.Error("bob's remember overwrote alice's card")
	}
}

// Replacing a card's body refreshes a derived summary; a hand-written one stays.
func TestCardSummaryRefreshOnEdit(t *testing.T) {
	db, h, _ := noteTestDB(t)
	mcpCall(t, h.Remember, "alice", "POST", nil, map[string]any{"text": "Bundle\nPending: lazy-load Monaco. Details follow."}, "")
	h.processInbox()
	var n models.Note
	db.Where("type = ?", "card").First(&n)
	body := "Done in 1.37: Monaco is lazy. Never call loader.init()."
	conflict, err := applyNoteContent(db, nil, &n, nil, &body, n.Version, "alice", noteSourceEdit)
	if err != nil || conflict || n.Summary != "Done in 1.37: Monaco is lazy." {
		t.Fatalf("derived summary refresh: %v %v %q", err, conflict, n.Summary)
	}
	db.Model(&n).Update("summary", "Hand-written line")
	n.Summary = "Hand-written line"
	body2 := "Something else entirely."
	_, _ = applyNoteContent(db, nil, &n, nil, &body2, n.Version, "alice", noteSourceEdit)
	if n.Summary != "Hand-written line" {
		t.Errorf("a hand-written summary must stay: %q", n.Summary)
	}
}

// A person's card with the same title is never adopted (overwritten); an
// agent card named with adopt is, even under another title.
func TestRememberKeyedAdoptionRules(t *testing.T) {
	db, h, _ := noteTestDB(t)
	send := func(body map[string]any) string {
		mcpCall(t, h.Remember, "alice", "POST", nil, body, "")
		h.processInbox()
		var item models.NoteInbox
		db.Order("created_at DESC").First(&item)
		return item.Outcome
	}
	// An agent card filed first gives us the memory folder; a person's card
	// with title "Runbook" goes next to it.
	send(map[string]any{"text": "Old title for monaco\nPending: lazy-load it."})
	var agentCard models.Note
	db.Where("type = ? AND title = ?", "card", "Old title for monaco").First(&agentCard)
	human := models.Note{ID: "human-card-0000-0000-0000-000000000001", UserID: "alice", ParentID: agentCard.ParentID, Type: "card",
		Title: "Runbook", Body: "Written by a person.", Source: "human", Version: 1}
	db.Create(&human)

	if out := send(map[string]any{"text": "Runbook\nAgent text.", "key": "mem/runbook.md"}); out != "created" {
		t.Errorf("same title as a person's card must create, got %s", out)
	}
	db.First(&human, "id = ?", human.ID)
	if human.Body != "Written by a person." || human.SourceKey != "" {
		t.Errorf("person's card changed: %+v", human)
	}
	short := agentCard.ID[len(agentCard.ID)-8:]
	if out := send(map[string]any{"text": "Monaco lazy-loaded\nDone in 1.37.", "key": "mem/monaco.md", "adopt": "#" + short}); out != "adopted" {
		t.Errorf("adopt by id: %s", out)
	}
	db.First(&agentCard, "id = ?", agentCard.ID)
	if agentCard.Title != "Monaco lazy-loaded" || agentCard.SourceKey != "mem/monaco.md" {
		t.Errorf("adopted card: %+v", agentCard)
	}
	// adopt can't take a person's card.
	if out := send(map[string]any{"text": "Other\nx.", "key": "mem/other.md", "adopt": human.ID}); out != "created" {
		t.Errorf("adopting a person's card must not happen: %s", out)
	}
}

// [[project-x]] in a synced memory resolves to the card synced from
// project_x.md, so file links become note links.
func TestWikiLinkBySourceStem(t *testing.T) {
	db, h, _ := noteTestDB(t)
	send := func(text, key string) {
		mcpCall(t, h.Remember, "alice", "POST", nil, map[string]any{"text": text, "key": key}, "")
		h.processInbox()
	}
	send("Local web run recipe\nRun the BE from a scratch dir.", "claude-memory/acme/project_local_web_run.md")
	send("Desktop security\nSee [[project-local-web-run]] for the dev setup.", "claude-memory/acme/project_desktop_security.md")
	var sec, run models.Note
	db.Where("source_key = ?", "claude-memory/acme/project_desktop_security.md").First(&sec)
	db.Where("source_key = ?", "claude-memory/acme/project_local_web_run.md").First(&run)
	if !strings.Contains(sec.Body, "(note:"+run.ID+")") {
		t.Errorf("link not resolved: %q", sec.Body)
	}
	var n int64
	db.Model(&models.NoteLink{}).Where("src_note_id = ? AND dst_note_id = ?", sec.ID, run.ID).Count(&n)
	if n != 1 {
		t.Errorf("body link edge = %d", n)
	}
}

func TestSentenceLikeHint(t *testing.T) {
	for q, want := range map[string]bool{
		"tauri signing password":                   false,
		"what password does the signing key need?": true,
		"oturum süresi sliding ttl":                false,
		"one two three four five six seven eight":  true,
	} {
		if sentenceLike(q) != want {
			t.Errorf("%q: %v", q, !want)
		}
	}
}

func TestForgetByKey(t *testing.T) {
	db, h, _ := noteTestDB(t)
	for _, k := range []string{"mem/x.md#1", "mem/x.md#2", "mem/y.md"} {
		mcpCall(t, h.Remember, "alice", "POST", nil, map[string]any{"text": "Card " + k + "\nBody " + k + ".", "key": k}, "")
		h.processInbox()
	}
	if r := mcpCall(t, h.Forget, "bob", "POST", nil, map[string]any{"key": "mem/x.md", "parts": true}, ""); r.code != 200 {
		t.Fatal(r.code)
	}
	var n int64
	db.Model(&models.Note{}).Where("type = ?", "card").Count(&n)
	if n != 3 {
		t.Errorf("bob must not forget alice's cards: %d", n)
	}
	mcpCall(t, h.Forget, "alice", "POST", nil, map[string]any{"key": "mem/x.md", "parts": true}, "")
	db.Model(&models.Note{}).Where("type = ?", "card").Count(&n)
	if n != 1 {
		t.Errorf("both parts forgotten, y kept: %d left", n)
	}
	// Soft delete: the rows are still there for the owner to restore.
	var all int64
	db.Unscoped().Model(&models.Note{}).Where("type = ? AND source_key LIKE ? AND deleted_at IS NOT NULL", "card", "mem/x.md#%").Count(&all)
	if all != 2 {
		t.Errorf("forget must soft-delete (restorable), deleted rows = %d", all)
	}
}

// remember answers with lint hints and suggests related cards to link.
func TestRememberLintHints(t *testing.T) {
	_, h, _ := noteTestDB(t)
	mcpCall(t, h.Remember, "alice", "POST", nil, map[string]any{"text": "Kafka consumer lag alert\nlag over 10k on orders-worker pages the on-call."}, "")
	h.processInbox()
	r := mcpCall(t, h.Remember, "alice", "POST", nil, map[string]any{"text": "orders-worker scaling\nWhen kafka lag grows on orders-worker, scale replicas to 6."}, "")
	h.processInbox()
	var hints []string
	_ = json.Unmarshal(r.raw["data"], &struct {
		Hints *[]string `json:"hints"`
	}{&hints})
	joined := strings.Join(hints, " | ")
	if !strings.Contains(joined, "pass it as context") {
		t.Errorf("hints: %v", hints)
	}
	r = mcpCall(t, h.Remember, "alice", "POST", nil, map[string]any{"text": "Lag runbook\nSee [[Kafka consumer lag alert]] — `orders-worker` scales to 6.", "context": "patchman://k8s/c1/prod/Deployment/orders-worker", "evidence": "verified"}, "")
	h.processInbox()
	hints = nil
	_ = json.Unmarshal(r.raw["data"], &struct {
		Hints *[]string `json:"hints"`
	}{&hints})
	if len(hints) != 0 {
		t.Errorf("a linked, anchored, rated card needs no hints: %v", hints)
	}
}
