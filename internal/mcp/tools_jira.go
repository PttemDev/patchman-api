package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Jira tools (read-only, CR_QA_P0_PLAN.md P0.b). They go through the same
// REST routes as the UI, so grants and the read audit apply unchanged.
// Replies are compact text: one line per issue; an issue's acceptance
// criteria in full (rendered as text), a long description cut with a note.
// Lists are never cut silently: a reply says "showing N of M" or that more
// pages exist.

type jiraSearchIn struct {
	ID    string `json:"id" jsonschema:"Jira connection id (see list_jira_connections)"`
	JQL   string `json:"jql,omitempty" jsonschema:"JQL, e.g. project = MER AND status = \"In Test\" ORDER BY updated DESC; default = the connection's default JQL"`
	Limit int    `json:"limit,omitempty" jsonschema:"issues per page (default 25, max 100)"`
	Next  string `json:"next,omitempty" jsonschema:"the next cursor a previous jira_search reply gave, for the following page"`
}

type jiraIssueIn struct {
	ID  string `json:"id" jsonschema:"Jira connection id (see list_jira_connections)"`
	Key string `json:"key" jsonschema:"issue key, e.g. MER-936"`
}

// jiraDescCap bounds the description in a jira_issue reply; the acceptance
// criteria get a larger cap because they are what the agent works from.
const (
	jiraDescCap = 2000
	jiraACCap   = 8000
)

func jiraPath(id, rest string) string { return "/api/jira-connections/" + url.PathEscape(id) + rest }

func registerJiraTools(s *mcpsdk.Server, c *caller) {
	regCtx(s, "jira_search", "Search Jira issues with JQL: one line per issue (key, status, type, priority, assignee, summary). Paged: pass the reply's next cursor to get the following page. Read-only; searches are audited."+connNote,
		func(ctx context.Context, in jiraSearchIn) (int, string) {
			q := url.Values{}
			if j := strings.TrimSpace(in.JQL); j != "" {
				q.Set("jql", j)
			}
			if in.Limit > 0 {
				q.Set("limit", strconv.Itoa(in.Limit))
			}
			if n := strings.TrimSpace(in.Next); n != "" {
				q.Set("next", n)
			}
			code, body := c.callCtx(ctx, "GET", withQuery(jiraPath(in.ID, "/search"), q), nil)
			if code != 200 {
				return code, body
			}
			return code, compactJiraSearch(body)
		})
	regCtx(s, "jira_issue", "Read one Jira issue: status, type, priority, assignee, labels, sprint, the acceptance criteria (the connection's configured field, as text), description, links and subtasks. Read-only; reads are audited."+connNote,
		func(ctx context.Context, in jiraIssueIn) (int, string) {
			key := strings.TrimSpace(in.Key)
			if key == "" {
				return 400, `{"error":"key is required (e.g. MER-936)"}`
			}
			code, body := c.callCtx(ctx, "GET", jiraPath(in.ID, "/issue/"+url.PathEscape(key)), nil)
			if code != 200 {
				return code, body
			}
			return code, compactJiraIssue(body)
		})
}

func compactJiraSearch(body string) string {
	var env struct {
		Data struct {
			JQL    string `json:"jql"`
			Total  *int   `json:"total"`
			Next   string `json:"next"`
			IsLast bool   `json:"isLast"`
			Issues []struct {
				Key       string `json:"key"`
				Summary   string `json:"summary"`
				Status    string `json:"status"`
				IssueType string `json:"issueType"`
				Priority  string `json:"priority"`
				Assignee  string `json:"assignee"`
			} `json:"issues"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(body), &env) != nil {
		return body
	}
	d := env.Data
	var b strings.Builder
	n := len(d.Issues)
	switch {
	case d.Total != nil:
		fmt.Fprintf(&b, "%d issues match; showing %d of %d.", *d.Total, n, *d.Total)
	case d.Next != "":
		fmt.Fprintf(&b, "Showing %d issues; more match (total not reported by Jira Cloud).", n)
	default:
		fmt.Fprintf(&b, "Showing %d issues (last page).", n)
	}
	if d.Next != "" {
		fmt.Fprintf(&b, " Next page: next=%q.", d.Next)
	}
	if d.JQL != "" {
		b.WriteString("\nJQL: " + d.JQL)
	}
	for _, is := range d.Issues {
		fmt.Fprintf(&b, "\n%s  [%s]  %s", is.Key, is.Status, is.IssueType)
		if is.Priority != "" {
			b.WriteString("  " + is.Priority)
		}
		assignee := is.Assignee
		if assignee == "" {
			assignee = "unassigned"
		}
		b.WriteString("  @" + assignee + "  " + oneLine(is.Summary, 200))
	}
	return b.String()
}

func compactJiraIssue(body string) string {
	var env struct {
		Data struct {
			Key                         string   `json:"key"`
			Summary                     string   `json:"summary"`
			Status                      string   `json:"status"`
			StatusCategory              string   `json:"statusCategory"`
			IssueType                   string   `json:"issueType"`
			Priority                    string   `json:"priority"`
			Assignee                    string   `json:"assignee"`
			Reporter                    string   `json:"reporter"`
			Labels                      []string `json:"labels"`
			Sprints                     []string `json:"sprints"`
			Created                     string   `json:"created"`
			Updated                     string   `json:"updated"`
			Description                 string   `json:"description"`
			DescriptionTruncated        bool     `json:"descriptionTruncated"`
			AcceptanceFieldID           string   `json:"acceptanceFieldId"`
			AcceptanceCriteria          *string  `json:"acceptanceCriteria"`
			AcceptanceCriteriaTruncated bool     `json:"acceptanceCriteriaTruncated"`
			Links                       []struct {
				Type, Key, Summary, Status string
			} `json:"links"`
			Subtasks []struct {
				Key, Summary, Status string
			} `json:"subtasks"`
			WebURL string `json:"webUrl"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(body), &env) != nil {
		return body
	}
	d := env.Data
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n", d.Key, d.Summary)
	status := d.Status
	if d.StatusCategory != "" && d.StatusCategory != d.Status {
		status += " (" + d.StatusCategory + ")"
	}
	fmt.Fprintf(&b, "Status: %s | Type: %s", status, d.IssueType)
	if d.Priority != "" {
		b.WriteString(" | Priority: " + d.Priority)
	}
	b.WriteString("\nAssignee: " + or1(d.Assignee, "unassigned"))
	if d.Reporter != "" {
		b.WriteString(" | Reporter: " + d.Reporter)
	}
	if len(d.Labels) > 0 {
		b.WriteString("\nLabels: " + strings.Join(d.Labels, ", "))
	}
	if len(d.Sprints) > 0 {
		b.WriteString("\nSprint: " + strings.Join(d.Sprints, ", "))
	}
	if d.Updated != "" {
		b.WriteString("\nUpdated: " + d.Updated)
	}
	if d.WebURL != "" {
		b.WriteString("\nIn Jira: " + d.WebURL)
	}

	b.WriteString("\n\nAcceptance criteria")
	switch {
	case d.AcceptanceFieldID == "":
		b.WriteString(": no acceptance-criteria field is set on this connection (pick one in PatchMan Settings → Jira). They may be in the description.\n")
	case d.AcceptanceCriteria == nil || strings.TrimSpace(*d.AcceptanceCriteria) == "":
		fmt.Fprintf(&b, " (%s): empty on this issue.\n", d.AcceptanceFieldID)
	default:
		fmt.Fprintf(&b, " (%s):\n%s\n", d.AcceptanceFieldID, capNote(*d.AcceptanceCriteria, jiraACCap, d.AcceptanceCriteriaTruncated))
	}

	if strings.TrimSpace(d.Description) == "" {
		b.WriteString("\nDescription: (empty)\n")
	} else {
		b.WriteString("\nDescription:\n" + capNote(d.Description, jiraDescCap, d.DescriptionTruncated) + "\n")
	}

	if len(d.Links) > 0 {
		fmt.Fprintf(&b, "\nLinks (%d):\n", len(d.Links))
		for _, l := range d.Links {
			fmt.Fprintf(&b, "- %s %s [%s] %s\n", l.Type, l.Key, l.Status, oneLine(l.Summary, 160))
		}
	}
	if len(d.Subtasks) > 0 {
		fmt.Fprintf(&b, "\nSubtasks (%d):\n", len(d.Subtasks))
		for _, s := range d.Subtasks {
			fmt.Fprintf(&b, "- %s [%s] %s\n", s.Key, s.Status, oneLine(s.Summary, 160))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// capNote cuts s to n characters and says so; upstream marks the API's own
// cut (the BE caps a rich-text field before it reaches here).
func capNote(s string, n int, upstream bool) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + fmt.Sprintf("\n… (truncated: showing %d of %d characters; open the issue in Jira for the rest)", n, len(r))
	}
	if upstream {
		return s + "\n… (truncated by PatchMan: the field is longer; open the issue in Jira for the rest)"
	}
	return s
}

func or1(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
