package qalink

import (
	"reflect"
	"testing"
	"time"
)

func ip(n int) *int   { return &n }
func bp(b bool) *bool { return &b }

func kinds(s []Signal) []string {
	out := []string{}
	for _, x := range s {
		out = append(out, x.Kind)
	}
	return out
}

func TestSignals(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	full := Settings{SLAHours: 24, LargeFiles: 30, TestPatterns: []string{"**/*_test.go", "test/**"}}
	cases := []struct {
		name string
		mr   MRFacts
		s    Settings
		want []string
	}{
		{"clean", MRFacts{State: "opened", ApprovalsGiven: ip(1), UpdatedAt: now}, full, []string{}},
		{"stuck conflict+pipeline", MRFacts{State: "opened", HasConflicts: true, Pipeline: "failed"}, full, []string{SigStuck}},
		{"threads", MRFacts{State: "opened", UnresolvedThreads: bp(true)}, full, []string{SigStuck}},
		{"awaiting", MRFacts{State: "opened", ApprovalsGiven: ip(0), UpdatedAt: now.Add(-25 * time.Hour)}, full, []string{SigAwaitingReview}},
		{"awaiting draft off", MRFacts{State: "opened", Draft: true, ApprovalsGiven: ip(0), UpdatedAt: now.Add(-48 * time.Hour)}, full, []string{}},
		{"awaiting unknown approvals off", MRFacts{State: "opened", UpdatedAt: now.Add(-48 * time.Hour)}, full, []string{}},
		{"sla empty = off", MRFacts{State: "opened", ApprovalsGiven: ip(0), UpdatedAt: now.Add(-480 * time.Hour)}, Settings{}, []string{}},
		{"large", MRFacts{State: "opened", ChangedFiles: ip(31)}, full, []string{SigLarge}},
		{"large unknown", MRFacts{State: "opened"}, full, []string{}},
		{"no tests", MRFacts{State: "opened", Paths: []string{"internal/a.go"}}, full, []string{SigNoTests}},
		{"has tests", MRFacts{State: "opened", Paths: []string{"internal/a.go", "internal/a_test.go"}}, full, []string{}},
		{"paths not fetched", MRFacts{State: "opened", Paths: nil}, full, []string{}},
		{"no jira", MRFacts{State: "opened", JiraLinked: true}, full, []string{SigNoJira}},
		{"no jira link → off", MRFacts{State: "opened"}, full, []string{}},
		{"jira missing", MRFacts{State: "opened", JiraLinked: true, JiraKey: "MER-1", JiraMissing: true}, full, []string{SigNoJira}},
		{"jira done MR open", MRFacts{State: "opened", JiraLinked: true, JiraKey: "MER-1", Jira: &JiraFacts{Status: "Done", StatusCategory: "done"}}, full, []string{SigJiraMismatch}},
		{"merged jira open", MRFacts{State: "merged", JiraLinked: true, JiraKey: "MER-1", Jira: &JiraFacts{Status: "In Test", StatusCategory: "indeterminate"}}, full, []string{SigJiraMismatch}},
		{"merged jira done", MRFacts{State: "merged", JiraLinked: true, JiraKey: "MER-1", Jira: &JiraFacts{Status: "Done", StatusCategory: "done"}}, full, []string{}},
		{"ai", MRFacts{State: "opened", AIAssisted: bp(true)}, full, []string{SigAIAssisted}},
		{"disabled", MRFacts{State: "opened", HasConflicts: true}, Settings{Disabled: map[string]bool{SigStuck: true}}, []string{}},
	}
	for _, c := range cases {
		if got := kinds(Signals(c.mr, c.s, now)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestGlobs(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		{"**/*_test.go", "a/b/c_test.go", true},
		{"**/*_test.go", "c_test.go", true},
		{"*_test.go", "x/y_test.go", true},
		{"test/**", "test/a/b.ts", true},
		{"test/**", "src/test/a.ts", false},
		{"src/*.ts", "src/a/b.ts", false},
		{"**/__tests__/**", "web/src/__tests__/x.tsx", true},
		{"*.spec.?s", "a/b.spec.ts", true},
	}
	for _, c := range cases {
		if got := matchAny(compileGlobs([]string{c.pat}), c.path); got != c.want {
			t.Errorf("%s ~ %s: got %v", c.pat, c.path, got)
		}
	}
}

func TestCollisions(t *testing.T) {
	got := Collisions([]string{"p", "p", "q", "p"}, [][]string{{"a.go", "b.go"}, {"b.go"}, {"a.go"}, nil})
	want := map[int][]int{0: {1}, 1: {0}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestAIAssisted(t *testing.T) {
	if !AIAssisted([]string{"fix\n\nCo-Authored-By: Claude Opus <noreply@anthropic.com>"}) {
		t.Fatal("claude trailer")
	}
	if !AIAssisted([]string{"x\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)"}) {
		t.Fatal("generated footer")
	}
	if AIAssisted([]string{"Co-Authored-By: Ayşe <a@example.com>", "copilot config tweak"}) {
		t.Fatal("false positive")
	}
}
