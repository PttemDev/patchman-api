package handlers

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"patchman-be/internal/models"
	"patchman-be/internal/notecards"

	"github.com/gin-gonic/gin"
)

// mcpCall is noteCall made by an AI agent (the MCP client header).
func mcpCall(t *testing.T, fn gin.HandlerFunc, user, method string, params gin.Params, body any, query string) noteResp {
	t.Helper()
	gin.SetMode(gin.TestMode)
	b, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", user)
	c.Params = params
	c.Request = httptest.NewRequest(method, "/", bytes.NewReader(b))
	c.Request.URL.RawQuery = query
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set(mcpClientHeader, mcpClientHeaderValue)
	c.Request.Header.Set(agentHeader, "Claude Code")
	fn(c)
	r := noteResp{code: w.Code}
	_ = json.Unmarshal(w.Body.Bytes(), &r.raw)
	r.data = r.raw["data"]
	return r
}

// errOf is a response's error message.
func errOf(r noteResp) string {
	var s string
	_ = json.Unmarshal(r.raw["error"], &s)
	return s
}

func recallText(t *testing.T, h *NoteHandler, user, q string, budget string) string {
	t.Helper()
	r := noteCall(t, h.Recall, user, "GET", nil, nil, "q="+q+"&budget="+budget)
	if r.code != 200 {
		t.Fatalf("recall = %d %s", r.code, r.raw["error"])
	}
	return noteField(t, r, "text").(string)
}

func TestRememberFilesCardsAndSkipsDuplicates(t *testing.T) {
	db, h, _ := noteTestDB(t)
	r := mcpCall(t, h.Remember, "alice", "POST", nil, map[string]any{
		"text":    "Refresh token rotation race\nTwo parallel refreshes could both succeed. Fix: single-use refresh tokens, checked in Redis.",
		"context": "open: patchman://request/42, patchman://k8s/c1/prod/Deployment/auth",
	}, "")
	if r.code != 202 || noteField(t, r, "queued") != true {
		t.Fatalf("remember = %d %v", r.code, r.raw)
	}
	h.processInbox()

	var card models.Note
	if err := db.Where("type = ?", "card").First(&card).Error; err != nil {
		t.Fatal("no card created")
	}
	if card.Title != "Refresh token rotation race" || card.Summary != "Two parallel refreshes could both succeed." ||
		card.Source != "agent" || card.Agent != "Claude Code" {
		t.Errorf("card = %q | %q | %s %q", card.Title, card.Summary, card.Source, card.Agent)
	}
	var folder models.Note
	db.Where("id = ?", *card.ParentID).First(&folder)
	if folder.Title != "Agent Memory" || !folder.Memory || folder.Type != "folder" || folder.UserID != "alice" {
		t.Errorf("folder = %+v", folder)
	}
	var uris []string
	db.Model(&models.NoteLink{}).Where("src_note_id = ? AND type = ?", card.ID, "refers_to").Pluck("dst_uri", &uris)
	if len(uris) != 2 {
		t.Errorf("context links = %v", uris)
	}

	// The same title again: no second card, a merge suggestion instead.
	mcpCall(t, h.Remember, "alice", "POST", nil, map[string]any{"text": "Refresh token rotation race\nAlso affects the mobile client."}, "")
	// The identical text again: nothing at all.
	mcpCall(t, h.Remember, "alice", "POST", nil, map[string]any{"text": "Refresh token rotation race\nTwo parallel refreshes could both succeed. Fix: single-use refresh tokens, checked in Redis."}, "")
	h.processInbox()
	var cards, merges, done int64
	db.Model(&models.Note{}).Where("type = ?", "card").Count(&cards)
	db.Model(&models.NoteSuggestion{}).Where("kind = ?", "merge").Count(&merges)
	db.Model(&models.NoteInbox{}).Where("status = ?", "done").Count(&done)
	if cards != 1 || merges != 1 || done != 3 {
		t.Errorf("cards=%d merges=%d done=%d", cards, merges, done)
	}
	// A re-run processes nothing twice.
	h.processInbox()
	db.Model(&models.Note{}).Where("type = ?", "card").Count(&cards)
	if cards != 1 {
		t.Errorf("re-run created cards: %d", cards)
	}
}

func TestRememberRefusesSecrets(t *testing.T) {
	_, h, _ := noteTestDB(t)
	for _, text := range []string{
		"The deploy token is glpat-abcdefghijklmnopqrstuvwxyz123",
		"db password: hunter2hunter2",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIE...",
	} {
		if r := mcpCall(t, h.Remember, "alice", "POST", nil, map[string]any{"text": text}, ""); r.code != 400 || !strings.Contains(errOf(r), "secret") {
			t.Errorf("%q: %d %v", text, r.code, r.raw["error"])
		}
	}
	if r := mcpCall(t, h.Remember, "alice", "POST", nil, map[string]any{"text": "Refresh tokens rotate every 15 minutes."}, ""); r.code != 202 {
		t.Errorf("plain fact refused: %d %v", r.code, r.raw["error"])
	}
}

func TestRecallBudgetLinksAndPermissions(t *testing.T) {
	db, h, _ := noteTestDB(t)
	space := mkNote(t, h, "alice", map[string]any{"title": "Auth", "type": "folder"})
	session := mkNote(t, h, "alice", map[string]any{"title": "Redis session store", "type": "card", "parentId": space,
		"summary": "Refresh tokens live in Redis with a TTL.", "body": "Keys are `rt:<user>`; TTL 30 days."})
	flow := mkNote(t, h, "alice", map[string]any{"title": "Auth flow", "type": "card", "parentId": space,
		"summary": "JWT access tokens of 15 minutes, rotated refresh tokens.",
		"body":    strings.Repeat("The refresh endpoint rotates the token and revokes the old one. ", 80)})
	old := mkNote(t, h, "alice", map[string]any{"title": "Auth flow (2024)", "type": "card", "parentId": space,
		"summary": "Access tokens lived 60 minutes.", "body": "Old auth design."})
	if r := noteCall(t, h.AddLink, "alice", "POST", id(flow), map[string]any{"dst": session, "type": "depends_on"}, ""); r.code != 201 {
		t.Fatalf("link = %d %v", r.code, r.raw["error"])
	}
	if r := noteCall(t, h.AddLink, "alice", "POST", id(flow), map[string]any{"dst": "#" + notecards.ShortID(old), "type": "supersedes"}, ""); r.code != 201 {
		t.Fatalf("supersede = %d %v", r.code, r.raw["error"])
	}

	text := recallText(t, h, "alice", "auth flow", "300")
	if !strings.HasPrefix(text, "#"+notecards.ShortID(flow)+" Auth flow | JWT access tokens") {
		t.Errorf("top line:\n%s", text)
	}
	if !strings.Contains(text, "→ depends_on #"+notecards.ShortID(session)+" Redis session store") {
		t.Errorf("link line missing:\n%s", text)
	}
	if strings.Contains(text, "Auth flow (2024) |") || !strings.Contains(text, "hint: #"+notecards.ShortID(old)+" is stale, superseded by #"+notecards.ShortID(flow)) {
		t.Errorf("stale card shown or hint missing:\n%s", text)
	}
	if len(text) > 300*4 {
		t.Errorf("answer is %d chars, over a 300-token budget", len(text))
	}
	if !strings.Contains(text, "---\nThe refresh endpoint") {
		t.Errorf("best hit's body missing:\n%s", text)
	}

	// Bob can't see Alice's private cards — not even their titles.
	if got := recallText(t, h, "bob", "auth flow", "800"); got != notecards.NoMatch {
		t.Errorf("bob recall leaked:\n%s", got)
	}

	// Body links show up as backlinks, typed.
	page := mkNote(t, h, "alice", map[string]any{"title": "Runbook", "body": "See [session store](note:" + session + ")."})
	r := noteCall(t, h.Backlinks, "alice", "GET", id(session), nil, "")
	var bl []map[string]any
	_ = json.Unmarshal(r.data, &bl)
	found := map[string]string{}
	for _, b := range bl {
		found[b["id"].(string)] = b["linkType"].(string)
	}
	if found[page] != "related" || found[flow] != "depends_on" {
		t.Errorf("backlinks = %v", found)
	}

	// Short ids work where an id is expected.
	if r := noteCall(t, h.Get, "alice", "GET", id("#"+notecards.ShortID(session)), nil, ""); r.code != 200 {
		t.Errorf("get by short id = %d", r.code)
	}
	var stale models.Note
	db.Where("id = ?", old).First(&stale)
	if !stale.Stale || stale.SupersededBy == nil || *stale.SupersededBy != flow {
		t.Errorf("superseded card = stale %v by %v", stale.Stale, stale.SupersededBy)
	}
}

func TestLinkTypesAndHumanCardsAreSuggested(t *testing.T) {
	db, h, _ := noteTestDB(t)
	space := mkNote(t, h, "alice", map[string]any{"title": "Ops", "type": "folder"})
	human := mkNote(t, h, "alice", map[string]any{"title": "Deploys need a change ticket", "type": "card", "parentId": space,
		"summary": "Every prod deploy needs an approved change ticket first."})
	other := mkNote(t, h, "alice", map[string]any{"title": "Change tickets", "type": "card", "parentId": space})

	if r := noteCall(t, h.AddLink, "alice", "POST", id(human), map[string]any{"dst": other, "type": "uses"}, ""); r.code != 400 ||
		!strings.Contains(errOf(r), "depends_on, part_of") {
		t.Errorf("bad type = %d %v", r.code, r.raw["error"])
	}
	// Claude linking from a person's card → suggestion, no edge.
	if r := mcpCall(t, h.AddLink, "alice", "POST", id(human), map[string]any{"dst": other, "type": "depends_on"}, ""); r.code != 202 {
		t.Errorf("claude link from human card = %d", r.code)
	}
	// Claude editing a person's card → suggestion, card unchanged.
	var n models.Note
	db.Where("id = ?", human).First(&n)
	if r := mcpCall(t, h.Update, "alice", "PATCH", id(human), map[string]any{"body": "rewritten", "baseVersion": n.Version}, ""); r.code != 202 {
		t.Errorf("claude edit of human card = %d", r.code)
	}
	db.Where("id = ?", human).First(&n)
	var edges, sugg int64
	db.Model(&models.NoteLink{}).Where("src_note_id = ? AND source <> ?", human, "body").Count(&edges)
	db.Model(&models.NoteSuggestion{}).Where("note_id = ?", human).Count(&sugg)
	if n.Body == "rewritten" || edges != 0 || sugg != 2 {
		t.Errorf("body=%q edges=%d suggestions=%d", n.Body, edges, sugg)
	}
	var named int64
	db.Model(&models.NoteSuggestion{}).Where("note_id = ? AND agent = ?", human, "Claude Code").Count(&named)
	if named != 2 {
		t.Errorf("suggestions signed by the agent: %d", named)
	}
	// A person links directly; the same edge twice is a no-op.
	noteCall(t, h.AddLink, "alice", "POST", id(human), map[string]any{"dst": other, "type": "depends_on"}, "")
	if r := noteCall(t, h.AddLink, "alice", "POST", id(human), map[string]any{"dst": other, "type": "depends_on"}, ""); noteField(t, r, "hint") != "edge exists" {
		t.Errorf("duplicate edge = %d %v", r.code, r.raw)
	}
	// Summary rules.
	if r := noteCall(t, h.Create, "alice", "POST", nil, map[string]any{"title": "Same", "type": "card", "summary": "same"}, ""); r.code != 400 {
		t.Errorf("summary = title accepted: %d", r.code)
	}
}
