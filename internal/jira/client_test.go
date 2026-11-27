package jira_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"patchman-be/internal/jira"
	"patchman-be/internal/jira/jiratest"
)

func client(t *testing.T, flavor string) (*jira.Client, *jiratest.Server) {
	t.Helper()
	srv := jiratest.New(flavor)
	t.Cleanup(srv.Close)
	cfg := jira.Config{BaseURL: srv.URL + "/browse/MER-1", Flavor: flavor, Secret: jiratest.PAT}
	if flavor == jira.FlavorCloud {
		cfg.Email, cfg.Secret = jiratest.Email, jiratest.APIToken
	}
	c, err := jira.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

func TestClientBothFlavors(t *testing.T) {
	for _, tc := range []struct {
		flavor, me, apiPrefix, searchPath string
		total                             bool
	}{
		{jira.FlavorCloud, "Ada Lovelace", "/rest/api/3", "/rest/api/3/search/jql", false},
		{jira.FlavorDataCenter, "Ada (DC)", "/rest/api/2", "/rest/api/2/search", true},
	} {
		t.Run(tc.flavor, func(t *testing.T) {
			c, srv := client(t, tc.flavor)
			ctx := context.Background()
			if c.Base() != srv.URL {
				t.Fatalf("base = %q, want the pasted /browse path cut: %q", c.Base(), srv.URL)
			}
			me, err := c.Myself(ctx)
			if err != nil || me.DisplayName != tc.me {
				t.Fatalf("myself = %+v, %v", me, err)
			}
			fields, err := c.Fields(ctx)
			if err != nil || len(fields) != 3 || jira.SprintFieldID(fields) != jiratest.Sprint {
				t.Fatalf("fields = %+v, %v", fields, err)
			}

			// Page through all five issues, two at a time.
			var keys []string
			next := ""
			for i := 0; i < 5; i++ {
				res, err := c.Search(ctx, jira.SearchRequest{JQL: "project = MER", Limit: 2, Next: next})
				if err != nil {
					t.Fatalf("search page %d: %v", i, err)
				}
				if tc.total && (res.Total == nil || *res.Total != 5) {
					t.Fatalf("DC total = %v", res.Total)
				}
				if !tc.total && res.Total != nil {
					t.Fatalf("Cloud total must be null (unknown), got %d", *res.Total)
				}
				for _, is := range res.Issues {
					keys = append(keys, is.Key)
				}
				if res.Issues[0].Status != "In Test" || res.Issues[0].Assignee != "Ada Lovelace" || res.Issues[0].IssueType != "Story" {
					t.Fatalf("row = %+v", res.Issues[0])
				}
				if res.IsLast {
					break
				}
				next = res.Next
			}
			if strings.Join(keys, ",") != "MER-1,MER-2,MER-3,MER-4,MER-5" {
				t.Fatalf("paged keys = %v", keys)
			}
			seen := strings.Join(srv.Seen(), "\n")
			if !strings.Contains(seen, tc.searchPath+"?") || !strings.Contains(seen, "fields=summary%2Cstatus") {
				t.Fatalf("search requests:\n%s", seen)
			}
			if tc.flavor == jira.FlavorDataCenter && !strings.Contains(seen, "startAt=4") {
				t.Fatalf("DC paging should send startAt:\n%s", seen)
			}

			is, err := c.Issue(ctx, "mer-936", jira.IssueOptions{AcceptanceFieldID: jiratest.ACField, SprintFieldID: jiratest.Sprint})
			if err != nil {
				t.Fatal(err)
			}
			ac := ""
			if is.AcceptanceCriteria != nil {
				ac = *is.AcceptanceCriteria
			}
			if !strings.Contains(ac, "Given a merchant") || !strings.Contains(ac, "lists orders by date") || !strings.Contains(ac, "sees 20 per page") {
				t.Fatalf("AC = %q", ac)
			}
			if tc.flavor == jira.FlavorCloud && !strings.Contains(ac, "- lists orders by date") {
				t.Fatalf("ADF list should render as '- ' lines: %q", ac)
			}
			if is.Status != "In Test" || is.StatusCategory != "In Progress" || is.Reporter != "" || len(is.Labels) != 2 ||
				len(is.Sprints) != 1 || !strings.HasPrefix(is.Sprints[0], "Sprint 12") ||
				len(is.Links) != 2 || is.Links[0].Type != "blocks" || is.Links[1].Type != "relates to" || is.Links[1].Status != "Done" ||
				len(is.Subtasks) != 1 || is.WebURL != srv.URL+"/browse/MER-936" || !strings.HasPrefix(is.Description, "The order list") {
				t.Fatalf("issue = %+v", is)
			}
			if !strings.Contains(strings.Join(srv.Seen(), "\n"), tc.apiPrefix+"/issue/MER-936?fields=") {
				t.Fatalf("issue path: %v", srv.Seen())
			}
			// No AC field configured → null, not "".
			is2, _ := c.Issue(ctx, "MER-936", jira.IssueOptions{})
			if is2.AcceptanceCriteria != nil {
				t.Fatalf("AC without a field = %v", *is2.AcceptanceCriteria)
			}
			b, _ := json.Marshal(is2)
			if !strings.Contains(string(b), `"sprints":[]`) || !strings.Contains(string(b), `"acceptanceCriteria":null`) {
				t.Fatalf("lists must be [] not null: %s", b)
			}

			// Errors: bad JQL is a readable 400, a missing issue a 404, a
			// crafted key never reaches Jira.
			_, err = c.Search(ctx, jira.SearchRequest{JQL: "project = (("})
			if ae, ok := jira.AsAPIError(err); !ok || ae.Code != "bad_query" || !strings.Contains(ae.Message, "Expecting") {
				t.Fatalf("bad jql = %v", err)
			}
			_, err = c.Issue(ctx, "MER-1", jira.IssueOptions{})
			if ae, ok := jira.AsAPIError(err); !ok || ae.Code != "not_found" {
				t.Fatalf("missing issue = %v", err)
			}
			before := len(srv.Seen())
			for _, bad := range []string{"../myself", "MER-1/../../myself", "MER", "MER-1?x=1"} {
				if _, err := c.Issue(ctx, bad, jira.IssueOptions{}); err == nil {
					t.Fatalf("key %q accepted", bad)
				}
			}
			if len(srv.Seen()) != before {
				t.Fatal("an invalid key reached Jira")
			}
		})
	}
}

func TestClientWrongCredentials(t *testing.T) {
	srv := jiratest.New(jira.FlavorCloud)
	defer srv.Close()
	c, _ := jira.New(jira.Config{BaseURL: srv.URL, Flavor: "cloud", Email: jiratest.Email, Secret: "wrong-secret"})
	_, err := c.Myself(context.Background())
	ae, ok := jira.AsAPIError(err)
	if !ok || ae.Code != "auth" || strings.Contains(err.Error(), "wrong-secret") {
		t.Fatalf("err = %v", err)
	}
	// A PAT against Cloud is the wrong header shape → auth.
	c2, _ := jira.New(jira.Config{BaseURL: srv.URL, Flavor: "datacenter", Secret: jiratest.APIToken})
	if _, err := c2.Myself(context.Background()); err == nil {
		t.Fatal("DC client against a Cloud fake should fail")
	}
	// DC search: a non-numeric cursor is refused before any call.
	dc := jiratest.New(jira.FlavorDataCenter)
	defer dc.Close()
	c3, _ := jira.New(jira.Config{BaseURL: dc.URL, Flavor: "datacenter", Secret: jiratest.PAT})
	if _, err := c3.Search(context.Background(), jira.SearchRequest{JQL: "x", Next: "abc"}); err == nil {
		t.Fatal("non-numeric DC cursor accepted")
	}
	// Cloud without email can't build a client.
	if _, err := jira.New(jira.Config{BaseURL: srv.URL, Flavor: "cloud", Secret: "x"}); err == nil {
		t.Fatal("cloud without email accepted")
	}
}

func TestBaseURL(t *testing.T) {
	cases := map[string]string{
		"acme.atlassian.net":                                             "https://acme.atlassian.net",
		"https://acme.atlassian.net/":                                    "https://acme.atlassian.net",
		"https://acme.atlassian.net/browse/MER-1":                        "https://acme.atlassian.net",
		"https://acme.atlassian.net/jira/software/projects/MER/boards/1": "https://acme.atlassian.net",
		"https://example.com/jira/rest/api/2/myself":                     "https://example.com/jira",
		"http://jira.example.com:8080/secure/Dashboard.jspa?x=1":         "http://jira.example.com:8080",
	}
	for in, want := range cases {
		if got, err := jira.BaseURL(in); err != nil || got != want {
			t.Errorf("BaseURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "  ", "https://ada:secret@acme.atlassian.net"} {
		if _, err := jira.BaseURL(bad); err == nil {
			t.Errorf("BaseURL(%q) should fail", bad)
		}
	}
}

func TestFieldText(t *testing.T) {
	cases := map[string]string{
		`"plain"`:                               "plain",
		`{"value":"Yes"}`:                       "Yes",
		`[{"value":"A"},{"value":"B"}]`:         "- A\n- B",
		`{"value":"EU","child":{"value":"TR"}}`: "EU / TR",
		`12.5`:                                  "12.5",
		`null`:                                  "",
		`{"type":"doc","version":1,"content":[{"type":"orderedList","content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"one"}]}]},{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"two"}]}]}]},{"type":"taskList","content":[{"type":"taskItem","attrs":{"state":"DONE"},"content":[{"type":"text","text":"done"}]}]}]}`: "1. one\n2. two\n[x] done",
	}
	for in, want := range cases {
		if got := jira.FieldText(json.RawMessage(in)); got != want {
			t.Errorf("FieldText(%s) = %q; want %q", in, got, want)
		}
	}
}

func TestWikiText(t *testing.T) {
	in := "h2. Rules\n# first *bold*\n# second _it_\n## nested {{code}}\n* see [docs|https://example.com/d]\n\nplain"
	want := "Rules\n1. first bold\n2. second it\n  1. nested code\n- see docs (https://example.com/d)\n\nplain"
	if got := jira.WikiText(in); got != want {
		t.Fatalf("WikiText =\n%q\nwant\n%q", got, want)
	}
}
