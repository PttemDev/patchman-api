package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func digestFolders(t *testing.T) []memoryFolder {
	t.Helper()
	var f []memoryFolder
	if err := json.Unmarshal([]byte(`[
	 {"title":"Agent Memory","isOwner":true,"cards":139,"scope":{"kind":"personal"},"projects":[{"title":"acme","cards":120},{"title":"orders","cards":19}]},
	 {"title":"Agent Memory","ownerName":"Sam","cards":182,"scope":{"kind":"team","teams":["platform"]}},
	 {"title":"Payments know-how","ownerName":"Alex","cards":40,"scope":{"kind":"shared","owner":"Alex"}}]`), &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestMemoryInstructionsDigest(t *testing.T) {
	d := memoryInstructionsDigest(digestFolders(t))
	for _, want := range []string{"recall searches all of them", "Agent Memory — yours, 139 cards (projects: acme 120, orders 19)", "team: platform, 182 cards", "shared by Alex, 40 cards", "say so"} {
		if !strings.Contains(d, want) {
			t.Errorf("digest lacks %q:\n%s", want, d)
		}
	}
	if memoryInstructionsDigest(nil) != "" {
		t.Error("no memories → no block")
	}
}

func TestRecallFooter(t *testing.T) {
	f := digestFolders(t)
	all := recallFooter(f, "", "")
	if !strings.HasPrefix(all, "memories searched: ") || strings.Count(all, "·") != 2 {
		t.Errorf("all: %s", all)
	}
	if team := recallFooter(f, "team", ""); strings.Contains(team, "yours") || !strings.Contains(team, "platform") {
		t.Errorf("team scope: %s", team)
	}
	if recallFooter(f, "", "Alex") != "" || recallFooter(f[:1], "", "") != "" {
		t.Error("no footer for from: or a single memory")
	}
}
