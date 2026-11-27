package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// qa_radar (CR_QA_P0_PLAN P0.c): the CR + QA Hub's MR Radar — open MRs of
// every Git project linked in a QA space, matched to Jira, with signals —
// or, with mine=true, the caller's Inbox. Read-only; it goes through the
// same REST routes as the Hub, so each link uses the caller's own access.
// Lists are never cut silently: each link line says "N of M".

type qaRadarIn struct {
	WorkspaceID string `json:"workspaceId,omitempty" jsonschema:"QA space: a workspace id (see the Hub's space picker); empty = your personal space"`
	Mine        bool   `json:"mine,omitempty" jsonschema:"true = your Inbox (MRs waiting for your review, your stuck MRs, Jira issues in a test status assigned to you)"`
	Signal      string `json:"signal,omitempty" jsonschema:"only MRs with this signal: stuck | awaitingReview | noJira | jiraMismatch | noTests | large | collision | aiAssisted"`
	Refresh     bool   `json:"refresh,omitempty" jsonschema:"bypass the 60 s cache"`
}

// allowJira: the token has the jira group; without it Jira keys, statuses
// and Jira-derived signals are left out of the reply.
func registerQARadarTools(s *mcpsdk.Server, c *caller, allowJira bool) {
	regCtx(s, "qa_radar", "CR + QA Hub MR Radar: open merge requests of the Git projects linked in a QA space, one line each (project!iid, title, author, reviewers, pipeline, Jira key + status, signals), plus recently merged MRs whose Jira issue isn't done. mine=true gives your Inbox instead. Signals are per-MR facts (stuck, awaitingReview, noJira, jiraMismatch, noTests, large, collision, possibly AI-assisted), never per person. Read-only.",
		func(ctx context.Context, in qaRadarIn) (int, string) {
			q := url.Values{}
			if ws := strings.TrimSpace(in.WorkspaceID); ws != "" {
				q.Set("workspaceId", ws)
			}
			if in.Refresh {
				q.Set("refresh", "1")
			}
			path := "/api/qa/radar"
			if in.Mine {
				path = "/api/qa/inbox"
			}
			code, body := c.cloudOrLocalCtx(ctx, "GET", withQuery(path, q), nil)
			if code != 200 {
				return code, body
			}
			if in.Mine {
				return code, compactQAInbox(body, allowJira)
			}
			return code, compactQARadar(body, strings.TrimSpace(in.Signal), allowJira)
		})
}

type qaRadarMRWire struct {
	Ref       string `json:"ref"`
	Project   string `json:"project"`
	Provider  string `json:"provider"`
	IID       int64  `json:"iid"`
	Title     string `json:"title"`
	State     string `json:"state"`
	WebURL    string `json:"webUrl"`
	Draft     bool   `json:"draft"`
	UpdatedAt string `json:"updatedAt"`
	Author    struct {
		Username string `json:"username"`
	} `json:"author"`
	Reviewers []struct {
		Username string `json:"username"`
	} `json:"reviewers"`
	ApprovedBy   []string `json:"approvedBy"`
	Pipeline     *string  `json:"pipeline"`
	ChangedFiles *int     `json:"changedFiles"`
	JiraKey      string   `json:"jiraKey"`
	Jira         *struct {
		Status  string `json:"status"`
		Missing bool   `json:"missing"`
	} `json:"jira"`
	Signals []struct {
		Kind   string `json:"kind"`
		Reason string `json:"reason"`
	} `json:"signals"`
}

type qaRadarLinkWire struct {
	GitProject      string `json:"gitProject"`
	Status          string `json:"status"`
	Error           string `json:"error"`
	JiraStatus      string `json:"jiraStatus"`
	JiraError       string `json:"jiraError"`
	Shown           int    `json:"shown"`
	Total           *int   `json:"total"`
	Truncated       bool   `json:"truncated"`
	MergedTruncated bool   `json:"mergedTruncated"`
	Incomplete      int    `json:"incomplete"`
	Personal        bool   `json:"personal"`
}

// withoutJira drops Jira facts and Jira-derived signals.
func withoutJira(m qaRadarMRWire) qaRadarMRWire {
	m.JiraKey, m.Jira = "", nil
	sig := m.Signals[:0:0]
	for _, s := range m.Signals {
		if s.Kind != "noJira" && s.Kind != "jiraMismatch" {
			sig = append(sig, s)
		}
	}
	m.Signals = sig
	return m
}

func mrLine(m qaRadarMRWire) string {
	var b strings.Builder
	sep := "!"
	if m.Provider == "github" {
		sep = "#"
	}
	fmt.Fprintf(&b, "%s%s%d %q by %s", m.Project, sep, m.IID, m.Title, m.Author.Username)
	if m.Draft {
		b.WriteString(" [draft]")
	}
	if m.State == "merged" {
		b.WriteString(" [merged]")
	}
	if len(m.Reviewers) > 0 {
		rs := []string{}
		for _, r := range m.Reviewers {
			mark := ""
			for _, a := range m.ApprovedBy {
				if strings.EqualFold(a, r.Username) {
					mark = "✓"
				}
			}
			rs = append(rs, r.Username+mark)
		}
		b.WriteString(" · reviewers " + strings.Join(rs, ","))
	}
	if m.Pipeline != nil {
		b.WriteString(" · pipeline " + *m.Pipeline)
	}
	if m.ChangedFiles != nil {
		fmt.Fprintf(&b, " · %d files", *m.ChangedFiles)
	}
	switch {
	case m.JiraKey == "":
	case m.Jira == nil:
		b.WriteString(" · " + m.JiraKey + " (Jira not read)")
	case m.Jira.Missing:
		b.WriteString(" · " + m.JiraKey + " (not found)")
	default:
		b.WriteString(" · " + m.JiraKey + " " + m.Jira.Status)
	}
	if len(m.Signals) > 0 {
		ss := []string{}
		for _, s := range m.Signals {
			k := s.Kind
			if k == "aiAssisted" {
				k = "possibly AI-assisted"
			}
			if s.Reason != "" {
				k += "(" + s.Reason + ")"
			}
			ss = append(ss, k)
		}
		b.WriteString(" · signals: " + strings.Join(ss, ", "))
	}
	if m.WebURL != "" {
		b.WriteString(" · " + m.WebURL)
	}
	return b.String()
}

func linkLines(links []qaRadarLinkWire, allowJira bool) string {
	var b strings.Builder
	for _, l := range links {
		fmt.Fprintf(&b, "\n- %s: %s", l.GitProject, l.Status)
		if l.Error != "" {
			b.WriteString(" (" + l.Error + ")")
		}
		if l.Status == "ok" {
			switch {
			case l.Total != nil:
				fmt.Fprintf(&b, ", %d of %d open MRs", l.Shown, *l.Total)
			default:
				fmt.Fprintf(&b, ", %d open MRs", l.Shown)
			}
			if l.Truncated {
				b.WriteString(" — TRUNCATED: more open MRs exist than the radar reads (300 per project)")
			}
			if l.MergedTruncated {
				b.WriteString("; more merged MRs in the look-back than were checked")
			}
			if l.Incomplete > 0 {
				fmt.Fprintf(&b, "; %d MRs not fully read (their unread facts are unknown)", l.Incomplete)
			}
			if !l.Personal {
				b.WriteString("; the connection's token is its owner's, so your own reviews/MRs aren't marked here")
			}
		}
		if allowJira && l.JiraStatus != "" && l.JiraStatus != "ok" {
			b.WriteString("; Jira " + l.JiraStatus)
			if l.JiraError != "" {
				b.WriteString(" (" + l.JiraError + ")")
			}
		}
	}
	return b.String()
}

func compactQARadar(body, signal string, allowJira bool) string {
	var env struct {
		Data struct {
			GeneratedAt string            `json:"generatedAt"`
			Links       []qaRadarLinkWire `json:"links"`
			MRs         []qaRadarMRWire   `json:"mrs"`
			Merged      []qaRadarMRWire   `json:"merged"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(body), &env) != nil {
		return body
	}
	d := env.Data
	if !allowJira {
		for i := range d.MRs {
			d.MRs[i] = withoutJira(d.MRs[i])
		}
		merged := d.Merged[:0:0]
		for _, m := range d.Merged {
			if m = withoutJira(m); len(m.Signals) > 0 {
				merged = append(merged, m)
			}
		}
		d.Merged = merged
	}
	if len(d.Links) == 0 {
		return "No Git projects are linked in this QA space. Link one in the CR + QA Hub → Settings."
	}
	has := func(m qaRadarMRWire) bool {
		if signal == "" {
			return true
		}
		for _, s := range m.Signals {
			if s.Kind == signal {
				return true
			}
		}
		return false
	}
	var b strings.Builder
	open := []qaRadarMRWire{}
	for _, m := range d.MRs {
		if has(m) {
			open = append(open, m)
		}
	}
	if signal != "" {
		fmt.Fprintf(&b, "MR Radar (%s): %d of %d open MRs have signal %s.", d.GeneratedAt, len(open), len(d.MRs), signal)
	} else {
		fmt.Fprintf(&b, "MR Radar (%s): %d open MRs.", d.GeneratedAt, len(d.MRs))
	}
	b.WriteString("\nLinked projects:" + linkLines(d.Links, allowJira))
	if !allowJira {
		b.WriteString("\n(Jira details hidden: this token lacks the jira tool group.)")
	}
	if len(open) > 0 {
		b.WriteString("\n\nOpen:")
		for _, m := range open {
			b.WriteString("\n- " + mrLine(m))
		}
	}
	merged := []qaRadarMRWire{}
	for _, m := range d.Merged {
		if has(m) {
			merged = append(merged, m)
		}
	}
	if len(merged) > 0 {
		b.WriteString("\n\nRecently merged with a signal:")
		for _, m := range merged {
			b.WriteString("\n- " + mrLine(m))
		}
	}
	return b.String()
}

func compactQAInbox(body string, allowJira bool) string {
	var env struct {
		Data struct {
			GeneratedAt   string            `json:"generatedAt"`
			Snoozed       int               `json:"snoozed"`
			Links         []qaRadarLinkWire `json:"links"`
			TestStatuses  []string          `json:"testStatuses"`
			JiraTruncated bool              `json:"jiraTruncated"`
			Items         []struct {
				Kind   string         `json:"kind"`
				Reason string         `json:"reason"`
				MR     *qaRadarMRWire `json:"mr"`
				Issue  *struct {
					Key     string `json:"key"`
					Summary string `json:"summary"`
					Status  string `json:"status"`
					URL     string `json:"url"`
				} `json:"issue"`
			} `json:"items"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(body), &env) != nil {
		return body
	}
	d := env.Data
	var b strings.Builder
	fmt.Fprintf(&b, "Inbox (%s): %d items", d.GeneratedAt, len(d.Items))
	if d.Snoozed > 0 {
		fmt.Fprintf(&b, ", %d snoozed", d.Snoozed)
	}
	b.WriteString(".")
	if len(d.TestStatuses) == 0 {
		b.WriteString(" Jira test queue is off (no test statuses set in Hub → Settings).")
	} else if d.JiraTruncated {
		b.WriteString(" More Jira test issues exist than shown (50 per connection).")
	}
	label := map[string]string{"review": "Waiting for your review", "stuck": "Your MR is stuck", "test": "To test (Jira)"}
	if !allowJira {
		b.WriteString(" Jira items hidden: this token lacks the jira tool group.")
	}
	for _, it := range d.Items {
		if !allowJira && it.Kind == "test" {
			continue
		}
		if !allowJira && it.MR != nil {
			m := withoutJira(*it.MR)
			it.MR = &m
		}
		b.WriteString("\n- " + label[it.Kind] + ": ")
		switch {
		case it.MR != nil:
			b.WriteString(mrLine(*it.MR))
		case it.Issue != nil:
			fmt.Fprintf(&b, "%s %q — %s · %s", it.Issue.Key, it.Issue.Summary, it.Issue.Status, it.Issue.URL)
		}
	}
	b.WriteString("\nLinked projects:" + linkLines(d.Links, allowJira))
	return b.String()
}
