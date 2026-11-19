package mcp

import (
	"context"
	"encoding/json"
	"testing"
)

// TestMeasureToolSurface logs what the tool list costs an agent's context
// (bytes and a rough token count, bytes/4). Run with -v to see the numbers.
func TestMeasureToolSurface(t *testing.T) {
	cs := connect(t, echoEngine())
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res.Tools)
	annotated := 0
	for _, tl := range res.Tools {
		if tl.Annotations != nil {
			annotated++
		}
	}
	t.Logf("tools=%d annotated=%d tools/list=%d bytes (~%d tokens)", len(res.Tools), annotated, len(b), len(b)/4)
}

// Every built-in tool must be classified in annotations.go.
func TestEveryToolAnnotated(t *testing.T) {
	cs := connect(t, echoEngine())
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tl := range res.Tools {
		seen[tl.Name] = true
		if tl.Annotations == nil {
			t.Errorf("tool %s has no annotations — classify it in annotations.go", tl.Name)
		}
	}
	for name := range toolKinds {
		if !seen[name] {
			t.Errorf("annotations.go lists %s but no such tool is registered", name)
		}
	}
	// Spot checks.
	for _, tl := range res.Tools {
		switch tl.Name {
		case "git_repos":
			if !tl.Annotations.ReadOnlyHint {
				t.Error("git_repos must be read-only")
			}
		case "delete_note", "git_play_job", "run_db_query":
			if tl.Annotations.ReadOnlyHint || tl.Annotations.DestructiveHint == nil || !*tl.Annotations.DestructiveHint {
				t.Errorf("%s must be destructive", tl.Name)
			}
		case "remember":
			if tl.Annotations.ReadOnlyHint || *tl.Annotations.DestructiveHint {
				t.Error("remember is an additive write")
			}
		}
	}
}
