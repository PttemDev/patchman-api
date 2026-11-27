package handlers

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"patchman-be/internal/jira"
	"patchman-be/internal/qalink"

	"github.com/gin-gonic/gin"
)

// Inbox (CR_QA_P0_PLAN P0.c): the radar filtered to the caller —
//   - review: MRs where I'm a reviewer and haven't approved;
//   - stuck:  my MRs with a stuck signal;
//   - test:   Jira issues assigned to me in one of the space's test
//     statuses (QASettings.TestStatuses; off when empty).
//
// Snoozed items are left out and counted.

type inboxItem struct {
	Ref              string     `json:"ref"`
	Kind             string     `json:"kind"` // review | stuck | test | verify
	Reason           string     `json:"reason,omitempty"`
	MR               *radarMR   `json:"mr,omitempty"`
	Issue            *radarJira `json:"issue,omitempty"`
	JiraConnectionID string     `json:"jiraConnectionId,omitempty"`
	Since            string     `json:"since,omitempty"`
}

type inboxResult struct {
	GeneratedAt string      `json:"generatedAt"`
	Items       []inboxItem `json:"items"`
	Snoozed     int         `json:"snoozed"`
	Links       []radarLink `json:"links"`
	// TestStatuses watched in Jira ([] = the test kind is off).
	TestStatuses []string `json:"testStatuses"`
	// JiraTruncated: more test issues exist than the Inbox shows.
	JiraTruncated bool `json:"jiraTruncated,omitempty"`
	// JiraSkipped: Jira connections left out of the test queue because their
	// stored credential is someone else's (currentUser() would be the owner).
	JiraSkipped int `json:"jiraSkipped"`
	// PendingSetup: setup changes agents proposed, waiting for the user.
	PendingSetup int `json:"pendingSetup"`
	// DigestHour: local hour of the daily "Today" summary (nil = off).
	DigestHour *int `json:"digestHour"`
}

// Inbox — GET /api/qa/inbox?workspaceId=&refresh=1.
func (h *QARadarHandler) Inbox(c *gin.Context) {
	res, ok := h.result(c)
	if !ok {
		return
	}
	userID := c.GetString("userID")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	out := inboxResult{
		GeneratedAt:  res.GeneratedAt,
		Items:        []inboxItem{},
		Links:        res.Links,
		TestStatuses: qalink.SplitPatterns(res.Settings.TestStatuses),
		DigestHour:   res.Settings.DigestHour,
		PendingSetup: pendingProposals(h.db, userID),
	}
	items := inboxFromRadar(res, userID)
	if len(out.TestStatuses) > 0 {
		test, truncated, skipped := h.inboxJira(ctx, userID, qaWorkspaceParam(c), res, out.TestStatuses, c.Query("refresh") == "1")
		items = append(items, test...)
		out.JiraTruncated = truncated
		out.JiraSkipped = skipped
	}
	snoozes := activeSnoozes(h.db, userID, h.now())
	for _, it := range items {
		if _, ok := snoozes[snoozeKey(it)]; ok {
			out.Snoozed++
			continue
		}
		out.Items = append(out.Items, it)
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// snoozeKey: an MR snooze ends when the MR gets a new head commit.
func snoozeKey(it inboxItem) string {
	if it.MR != nil && it.MR.HeadSHA != "" {
		return it.Ref + "@" + it.MR.HeadSHA
	}
	return it.Ref
}

func inboxFromRadar(res *radarResult, userID string) []inboxItem {
	items := []inboxItem{}
	for _, m := range res.MRs {
		// Fix checks: to run (open findings, new commits) or to apply (verdicts
		// in, nothing sent yet). Own ref, so snoozing it leaves the review item.
		if (m.OpenFindings > 0 || m.VerifyPending > 0) && containsFold(m.verifyOwners, userID) {
			reason := "check " + strconv.Itoa(m.OpenFindings)
			if m.OpenFindings == 0 {
				reason = "apply " + strconv.Itoa(m.VerifyPending)
			}
			items = append(items, inboxItem{Ref: m.Ref + "#verify", Kind: "verify", Reason: reason, MR: m, Since: m.UpdatedAt})
		}
		me := res.Me[m.GitConnectionID]
		if me == "" {
			continue
		}
		if !m.Draft && hasUser(m.Reviewers, me) && !containsFold(m.ApprovedBy, me) {
			items = append(items, inboxItem{Ref: m.Ref, Kind: "review", MR: m, Since: m.UpdatedAt})
		}
		if strings.EqualFold(m.Author.Username, me) {
			for _, s := range m.Signals {
				if s.Kind == qalink.SigStuck {
					items = append(items, inboxItem{Ref: m.Ref, Kind: "stuck", Reason: s.Reason, MR: m, Since: m.UpdatedAt})
				}
			}
		}
	}
	sort.SliceStable(items, func(a, b int) bool { return items[a].Since < items[b].Since })
	return items
}

func hasUser(us []radarUser, name string) bool {
	for _, u := range us {
		if strings.EqualFold(u.Username, name) {
			return true
		}
	}
	return false
}

func containsFold(xs []string, s string) bool {
	for _, x := range xs {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

type inboxJiraCached struct {
	items     []inboxItem
	truncated bool
	skipped   int
	at        time.Time
}

var inboxJiraCache sync.Map // user|ws → inboxJiraCached

// inboxJira: issues assigned to the caller in a test status, per Jira
// connection the caller can use, limited to the linked projects.
func (h *QARadarHandler) inboxJira(ctx context.Context, userID, ws string, res *radarResult, statuses []string, refresh bool) ([]inboxItem, bool, int) {
	key := userID + "|" + ws
	if !refresh {
		if v, ok := inboxJiraCache.Load(key); ok {
			if e := v.(inboxJiraCached); h.now().Sub(e.at) < radarResultTTL {
				return e.items, e.truncated, e.skipped
			}
		}
	}
	projects := map[string][]string{} // jira connection → project keys
	for _, l := range res.links {
		if l.JiraConnectionID != "" && l.JiraProjectKey != "" && res.jiraConns[l.JiraConnectionID] {
			if !containsFold(projects[l.JiraConnectionID], l.JiraProjectKey) {
				projects[l.JiraConnectionID] = append(projects[l.JiraConnectionID], l.JiraProjectKey)
			}
		}
	}
	items := []inboxItem{}
	truncated := false
	skipped := 0
	for connID, keys := range projects {
		conn, _, err := h.jira.loadAccessible(connID, userID)
		if err != nil {
			continue
		}
		// currentUser() is whoever the stored credential belongs to: only
		// the connection's owner gets a personal test queue from it.
		if conn.UserID != userID {
			skipped++
			continue
		}
		cl, err := h.jira.client(*conn)
		if err != nil {
			continue
		}
		jql := inboxJQL(keys, statuses)
		r, err := cl.Search(ctx, jira.SearchRequest{JQL: jql, Limit: 50})
		if err != nil {
			continue
		}
		if r.Next != "" {
			truncated = true
		}
		for _, row := range r.Issues {
			items = append(items, inboxItem{
				Ref: "jira:" + connID + ":" + row.Key, Kind: "test", Reason: row.Status, JiraConnectionID: connID, Since: row.Updated,
				Issue: &radarJira{Key: row.Key, Summary: row.Summary, Status: row.Status, StatusCategory: row.StatusCategory, Assignee: row.Assignee, URL: cl.IssueWebURL(row.Key)},
			})
		}
	}
	inboxJiraCache.Store(key, inboxJiraCached{items: items, truncated: truncated, skipped: skipped, at: h.now()})
	return items, truncated, skipped
}

// inboxJQL builds the test-queue JQL; project keys are validated link keys,
// status names are quoted with JQL escaping.
func inboxJQL(projectKeys, statuses []string) string {
	q := func(s string) string {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
	}
	ps := make([]string, 0, len(projectKeys))
	for _, k := range projectKeys {
		ps = append(ps, q(k))
	}
	ss := make([]string, 0, len(statuses))
	for _, s := range statuses {
		ss = append(ss, q(s))
	}
	return "assignee = currentUser() AND project in (" + strings.Join(ps, ",") + ") AND status in (" + strings.Join(ss, ",") + ") ORDER BY updated DESC"
}
