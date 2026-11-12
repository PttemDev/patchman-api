package mcp

import (
	"strings"
	"testing"
)

// The notes tools hit the right REST routes, carry the MCP client header (so
// history shows the write as an agent's), and update_note's two modes map to
// replace-with-baseVersion vs atomic append.
func TestNotesToolsRouting(t *testing.T) {
	cs := connect(t, echoEngine())

	if e := decode(t, call(t, cs, "get_note", map[string]any{"id": "n1"})); e.Method != "GET" || e.Path != "/api/notes/n1" || e.UserID != testUser {
		t.Errorf("get_note → %+v", e)
	}
	if e := decode(t, call(t, cs, "search_notes", map[string]any{"query": "kafka lag", "limit": 5})); e.Path != "/api/notes/search" || !strings.Contains(e.Query, "q=kafka+lag") || !strings.Contains(e.Query, "limit=5") {
		t.Errorf("search_notes → %+v", e)
	}
	e := decode(t, call(t, cs, "create_note", map[string]any{"title": "Log", "body": "hi", "parentId": "s1"}))
	if e.Method != "POST" || e.Path != "/api/notes" || !strings.Contains(e.Body, `"parentId":"s1"`) {
		t.Errorf("create_note → %+v", e)
	}
	e = decode(t, call(t, cs, "update_note", map[string]any{"id": "n1", "body": "new", "baseVersion": 7}))
	if e.Method != "PATCH" || e.Path != "/api/notes/n1" || !strings.Contains(e.Body, `"baseVersion":7`) || !strings.Contains(e.Body, `"body":"new"`) {
		t.Errorf("update_note replace → %+v", e)
	}
	e = decode(t, call(t, cs, "update_note", map[string]any{"id": "n1", "mode": "append", "body": "- step"}))
	if e.Method != "POST" || e.Path != "/api/notes/n1/append" || !strings.Contains(e.Body, `"text":"- step"`) {
		t.Errorf("update_note append → %+v", e)
	}
	if e := decode(t, call(t, cs, "delete_note", map[string]any{"id": "n1"})); e.Method != "DELETE" || e.Path != "/api/notes/n1" {
		t.Errorf("delete_note → %+v", e)
	}
	// list_notes stitches own + shared into one result.
	if txt := text(t, call(t, cs, "list_notes", map[string]any{})); !strings.Contains(txt, `"own":`) || !strings.Contains(txt, `"/api/notes/shared"`) {
		t.Errorf("list_notes → %s", txt)
	}
}
