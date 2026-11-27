package handlers

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"patchman-be/internal/jira"
)

type stubSearcher struct {
	known map[string]bool
	calls int
}

func (s *stubSearcher) Search(_ context.Context, req jira.SearchRequest) (*jira.SearchResult, error) {
	s.calls++
	inner := strings.TrimSuffix(strings.TrimPrefix(req.JQL, "key in ("), ")")
	out := &jira.SearchResult{Issues: []jira.IssueRow{}}
	for _, k := range strings.Split(inner, ",") {
		if !s.known[k] {
			return nil, &jira.APIError{Status: http.StatusBadRequest, Code: "bad_query", Message: "no " + k}
		}
		out.Issues = append(out.Issues, jira.IssueRow{Key: k, StatusCategory: "done"})
	}
	return out, nil
}

func TestBatchJiraIssuesBisects(t *testing.T) {
	s := &stubSearcher{known: map[string]bool{"A-1": true, "A-2": true, "A-4": true}}
	found, missing, err := batchJiraIssues(context.Background(), s, []string{"A-1", "A-2", "A-3", "A-4", "bad key"})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 3 || !missing["A-3"] || !missing["bad key"] || missing["A-1"] {
		t.Fatalf("found %v missing %v", found, missing)
	}
	if s.calls > 6 {
		t.Fatalf("too many searches: %d", s.calls)
	}
}

func TestBatchJiraIssuesOtherErrorFails(t *testing.T) {
	_, _, err := batchJiraIssues(context.Background(), errSearcher{}, []string{"A-1"})
	if err == nil {
		t.Fatal("auth error must surface")
	}
}

type errSearcher struct{}

func (errSearcher) Search(context.Context, jira.SearchRequest) (*jira.SearchResult, error) {
	return nil, &jira.APIError{Status: http.StatusUnauthorized, Code: "auth", Message: "auth"}
}

func TestInboxJQLEscapes(t *testing.T) {
	got := inboxJQL([]string{"MER"}, []string{`In "QA"`, `a\b`})
	want := `assignee = currentUser() AND project in ("MER") AND status in ("In \"QA\"","a\\b") ORDER BY updated DESC`
	if got != want {
		t.Fatalf("got %s", got)
	}
}
