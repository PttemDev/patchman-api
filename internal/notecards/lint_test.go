package notecards

import (
	"strings"
	"testing"
)

func has(hs []string, prefix string) bool {
	for _, h := range hs {
		if strings.HasPrefix(h, prefix) {
			return true
		}
	}
	return false
}

func TestLint(t *testing.T) {
	good := "Tauri updater signing\nTAURI_SIGNING_PRIVATE_KEY_PASSWORD must be the empty string. Export it before tauri build or no .sig is written."
	if hs := Lint(good, true); len(hs) != 0 {
		t.Errorf("a good card gets no hints: %v", hs)
	}
	if hs := Lint("we fixed it", true); !has(hs, "Start with a title line") || !has(hs, "Name the thing") {
		t.Errorf("one bare sentence: %v", hs)
	}
	if hs := Lint(good, false); !has(hs, "If this is about a specific") {
		t.Errorf("no context: %v", hs)
	}
	long := "Big release notes\nEverything in 1.66.0.\n\n" + strings.Repeat("## Section\nwords words words words.\n\n", 4) + strings.Repeat("x", 4100)
	if hs := Lint(long, true); !has(hs, "Long for one card") {
		t.Errorf("long: %v", hs)
	}
	multi := "Release 1.66 notes\nWhat shipped.\n\n" + strings.Repeat("- **Area**: "+strings.Repeat("detail ", 60)+"\n", 5)
	if hs := Lint(multi, true); !has(hs, "This reads like several topics") {
		t.Errorf("several topics: %v", hs)
	}
}
