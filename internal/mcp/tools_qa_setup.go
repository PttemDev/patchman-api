package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// CR + QA setup over MCP. Every change is the user's decision: unless they
// turned the question off (Settings → MCP), the agent's change is shown to
// them first — asked in the client when it can ask, otherwise left in the
// CR + QA Hub as a proposal they apply or reject. Secrets an agent passes
// (a Jira token, a channel URL) never come back in a reply.

// regSetup registers a setup tool; its handler returns the change to make.
func regSetup[In any](s *mcpsdk.Server, c *caller, name, desc string, build func(In) (setupChange, error)) {
	mcpsdk.AddTool(s, &mcpsdk.Tool{Name: name, Description: desc + setupNote, Annotations: annotationsFor(name)},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, in In) (*mcpsdk.CallToolResult, any, error) {
			stop := withProgress(ctx, req, name)
			defer stop()
			ch, err := build(in)
			if err != nil {
				return result(400, fmt.Sprintf(`{"error":%q}`, err.Error()))
			}
			return result(c.applySetup(ctx, req, name, ch))
		})
}

const setupNote = " The user decides: unless they turned this off in Settings → MCP, they are asked first (or the change waits for them in CR + QA Hub → Settings)."

type setupChange struct {
	Summary     string
	Method      string
	Path        string // without /api
	Query       url.Values
	Body        any
	WorkspaceID string
}

func (c *caller) applySetup(ctx context.Context, req *mcpsdk.CallToolRequest, tool string, ch setupChange) (int, string) {
	q := url.Values{}
	for k, v := range ch.Query {
		q[k] = v
	}
	if ch.WorkspaceID != "" {
		q.Set("workspaceId", ch.WorkspaceID)
	}
	apply := func() (int, string) {
		code, body := c.cloudOrLocalCtx(ctx, ch.Method, withQuery("/api"+ch.Path, q), ch.Body)
		if code >= 400 {
			return code, body
		}
		return code, "Done: " + ch.Summary + "\n" + compactSetupReply(body)
	}
	// The user's choice (default: ask).
	ask := true
	if code, body := c.cloudOrLocalCtx(ctx, "GET", "/api/qa/agent-setup-policy", nil); code == 200 {
		var env struct {
			Data struct {
				Ask bool `json:"ask"`
			} `json:"data"`
		}
		if json.Unmarshal([]byte(body), &env) == nil {
			ask = env.Data.Ask
		}
	}
	if !ask {
		return apply()
	}
	if canElicit(req) {
		res, err := req.Session.Elicit(ctx, &mcpsdk.ElicitParams{
			Message: "Allow the agent to change CR + QA setup?\n" + ch.Summary,
			RequestedSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"confirm": map[string]any{"type": "boolean", "title": "Apply it", "default": false}},
				"required":   []string{"confirm"},
			},
		})
		if err == nil {
			if v, _ := res.Content["confirm"].(bool); res.Action == "accept" && v {
				return apply()
			}
			return 409, fmt.Sprintf(`{"error":"The user did not approve: %s","nextStep":"Don't retry; ask the user what they want instead."}`, strings.ReplaceAll(ch.Summary, `"`, `'`))
		}
	}
	// The client can't ask: leave it for the user in the Hub.
	pq := url.Values{}
	if ch.WorkspaceID != "" {
		pq.Set("workspaceId", ch.WorkspaceID)
	}
	rq := map[string]any{"method": ch.Method, "path": ch.Path, "body": ch.Body}
	if len(q) > 0 {
		flat := map[string]string{}
		for k := range q {
			flat[k] = q.Get(k)
		}
		rq["query"] = flat
	}
	code, body := c.cloudOrLocalCtx(ctx, "POST", withQuery("/api/qa/proposals", pq), map[string]any{"tool": tool, "summary": ch.Summary, "request": rq})
	if code >= 400 {
		return code, body
	}
	return 202, "Waiting for the user: " + ch.Summary + "\nThe change is in CR + QA Hub → Settings → Pending changes; it is applied when the user approves it there. Tell the user so."
}

// compactSetupReply keeps a write reply short (id + name-ish fields).
func compactSetupReply(body string) string {
	var env struct {
		Data map[string]any `json:"data"`
	}
	if json.Unmarshal([]byte(body), &env) != nil || env.Data == nil {
		return ""
	}
	parts := []string{}
	for _, k := range []string{"id", "name", "gitProject", "jiraProjectKey", "kind", "url", "installed", "deleted"} {
		if v, ok := env.Data[k]; ok && v != nil && v != "" {
			if k == "url" && strings.Contains(fmt.Sprint(v), "hooks.") {
				continue // a channel URL is a secret
			}
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
	}
	return strings.Join(parts, " ")
}

// ── inputs ─────────────────────────────────────────────────────────────

type qaSpaceIn struct {
	WorkspaceID string `json:"workspaceId,omitempty" jsonschema:"QA space: workspace id; empty = personal"`
}

type qaLinkSaveIn struct {
	ID                string `json:"id,omitempty" jsonschema:"link id to update; omit to create"`
	WorkspaceID       string `json:"workspaceId,omitempty" jsonschema:"QA space for a new link: workspace id; empty = personal"`
	GitConnectionID   string `json:"gitConnectionId,omitempty" jsonschema:"git connection id (list_git_connections)"`
	GitProject        string `json:"gitProject,omitempty" jsonschema:"GitLab group/repo or id, GitHub owner/name (git_repos)"`
	JiraConnectionID  string `json:"jiraConnectionId,omitempty" jsonschema:"Jira connection id (list_jira_connections)"`
	JiraProjectKey    string `json:"jiraProjectKey,omitempty" jsonschema:"Jira project key, e.g. MER (jira_projects)"`
	KeyPattern        string `json:"keyPattern,omitempty" jsonschema:"regex for issue keys; empty = PROJECT-123"`
	TestEnvironmentID string `json:"testEnvironmentId,omitempty" jsonschema:"environment id for MR test runs (list_environments)"`
}

type qaIDIn struct {
	ID string `json:"id" jsonschema:"id"`
}

type qaMineIn struct {
	LinkID           string `json:"linkId" jsonschema:"link id"`
	GitConnectionID  string `json:"gitConnectionId,omitempty" jsonschema:"your own git connection for this link; empty = the link's"`
	JiraConnectionID string `json:"jiraConnectionId,omitempty" jsonschema:"your own Jira connection for this link; empty = the link's"`
}

type qaSettingsIn struct {
	WorkspaceID     string  `json:"workspaceId,omitempty" jsonschema:"QA space; empty = personal"`
	SLAHours        *int    `json:"slaHours,omitempty" jsonschema:"awaiting-review SLA in hours (0 = off)"`
	LargeFiles      *int    `json:"largeFiles,omitempty" jsonschema:"large MR threshold in files (0 = off)"`
	TestPatterns    *string `json:"testPatterns,omitempty" jsonschema:"test file globs, one per line"`
	SourcePatterns  *string `json:"sourcePatterns,omitempty" jsonschema:"source file globs, one per line (empty = every non-test file)"`
	TestStatuses    *string `json:"testStatuses,omitempty" jsonschema:"Jira test statuses, comma-separated (Inbox test queue)"`
	DisabledSignals *string `json:"disabledSignals,omitempty" jsonschema:"signals to turn off, comma-separated: stuck,awaitingReview,noJira,jiraMismatch,noTests,large,collision,aiAssisted"`
	MergedDays      *int    `json:"mergedDays,omitempty" jsonschema:"merged MR look-back in days (default 7)"`
	DigestHour      *int    `json:"digestHour,omitempty" jsonschema:"daily digest hour 0–23"`
	Timezone        *string `json:"timezone,omitempty" jsonschema:"IANA timezone of digestHour, e.g. Europe/Istanbul"`
	ReviewGuide     *string `json:"reviewGuide,omitempty" jsonschema:"the team's review rules (conventions, risky areas, severity policy) given to every AI review and fix check"`
}

type qaScheduleSaveIn struct {
	ID            string `json:"id,omitempty" jsonschema:"schedule id to update; omit to create"`
	WorkspaceID   string `json:"workspaceId,omitempty" jsonschema:"QA space for a new schedule"`
	Name          string `json:"name,omitempty"`
	TargetKind    string `json:"targetKind,omitempty" jsonschema:"folder or plan"`
	TargetID      string `json:"targetId,omitempty" jsonschema:"test folder or plan id"`
	EnvironmentID string `json:"environmentId,omitempty"`
	Days          string `json:"days,omitempty" jsonschema:"1 (Mon) … 7 (Sun), comma-separated; empty = every day"`
	Hour          *int   `json:"hour,omitempty"`
	Minute        *int   `json:"minute,omitempty"`
	Timezone      string `json:"timezone,omitempty" jsonschema:"IANA timezone, e.g. Europe/Istanbul"`
	Enabled       *bool  `json:"enabled,omitempty"`
}

type qaChannelSaveIn struct {
	ID          string `json:"id,omitempty" jsonschema:"channel id to update; omit to create"`
	WorkspaceID string `json:"workspaceId,omitempty" jsonschema:"QA space for a new channel"`
	Kind        string `json:"kind,omitempty" jsonschema:"slack or teams"`
	Name        string `json:"name,omitempty"`
	URL         string `json:"url,omitempty" jsonschema:"incoming webhook URL (hooks.slack.com / *.webhook.office.com); stored encrypted, never shown again"`
	Events      string `json:"events,omitempty" jsonschema:"comma-separated: mr_opened,pipeline_failed,mr_approved,mr_merged,digest"`
	Enabled     *bool  `json:"enabled,omitempty"`
}

type qaWebhookIn struct {
	LinkID  string `json:"linkId" jsonschema:"link id (GitLab)"`
	BaseURL string `json:"baseUrl" jsonschema:"public URL of this PatchMan server that GitLab can reach, e.g. https://patchman.example.com"`
}

type jiraConnCreateIn struct {
	Name        string `json:"name"`
	BaseURL     string `json:"baseUrl" jsonschema:"Jira site URL, e.g. https://acme.atlassian.net"`
	Flavor      string `json:"flavor,omitempty" jsonschema:"cloud (default) or datacenter"`
	Email       string `json:"email,omitempty" jsonschema:"account email (Cloud)"`
	Token       string `json:"token" jsonschema:"API token (Cloud) or personal access token (Data Center); stored encrypted, never shown again"`
	ACFieldID   string `json:"acFieldId,omitempty" jsonschema:"acceptance-criteria custom field id, e.g. customfield_10042"`
	WorkspaceID string `json:"workspaceId,omitempty"`
}

type jiraProjectsIn struct {
	ID    string `json:"id" jsonschema:"Jira connection id"`
	Query string `json:"query,omitempty" jsonschema:"filter by key or name"`
}

func put(m map[string]any, k string, v any) {
	switch x := v.(type) {
	case string:
		if x != "" {
			m[k] = x
		}
	case *int:
		if x != nil {
			m[k] = *x
		}
	case *bool:
		if x != nil {
			m[k] = *x
		}
	case *string:
		if x != nil {
			m[k] = *x
		}
	}
}

func space(ws string) string {
	if ws == "" {
		return "your personal QA space"
	}
	return "QA space " + ws
}

// ── registration ───────────────────────────────────────────────────────

func registerQASetupTools(s *mcpsdk.Server, c *caller, allowJira bool) {
	regCtx(s, "qa_setup", "CR + QA setup of a QA space: linked projects (git project ↔ Jira project, your own-connection mapping, webhook), radar thresholds, scheduled test runs, notification channels (no URLs) and whether you must ask the user before changing setup.",
		func(ctx context.Context, in qaSpaceIn) (int, string) {
			q := url.Values{}
			if in.WorkspaceID != "" {
				q.Set("workspaceId", in.WorkspaceID)
			}
			var b strings.Builder
			fmt.Fprintf(&b, "CR + QA setup of %s.\n", space(in.WorkspaceID))
			if code, body := c.cloudOrLocalCtx(ctx, "GET", "/api/qa/agent-setup-policy", nil); code == 200 {
				if strings.Contains(body, `"ask":false`) {
					b.WriteString("Setup changes apply directly (the user turned the question off).\n")
				} else {
					b.WriteString("Setup changes need the user's OK.\n")
				}
			}
			for _, sec := range []struct{ title, path string }{
				{"Linked projects", "/api/qa/links"}, {"Radar settings", "/api/qa/settings"},
				{"Scheduled test runs", "/api/qa/schedules"}, {"Channels", "/api/qa/channels"},
			} {
				code, body := c.cloudOrLocalCtx(ctx, "GET", withQuery(sec.path, q), nil)
				b.WriteString("\n## " + sec.title + "\n")
				if code != 200 {
					fmt.Fprintf(&b, "(not available: %d)\n", code)
					continue
				}
				b.WriteString(compactReply(body) + "\n")
			}
			return 200, b.String()
		})

	regSetup(s, c, "qa_link_save", "Link a Git project to a Jira project in a QA space (create), or change a link (id). MRs of the project are matched to issues by key.",
		func(in qaLinkSaveIn) (setupChange, error) {
			body := map[string]any{}
			put(body, "gitConnectionId", in.GitConnectionID)
			put(body, "gitProject", in.GitProject)
			put(body, "jiraConnectionId", in.JiraConnectionID)
			put(body, "jiraProjectKey", strings.ToUpper(in.JiraProjectKey))
			put(body, "keyPattern", in.KeyPattern)
			put(body, "testEnvironmentId", in.TestEnvironmentID)
			if in.ID != "" {
				return setupChange{Summary: fmt.Sprintf("Update link %s: %v", in.ID, body), Method: "PUT", Path: "/qa/links/" + url.PathEscape(in.ID), Body: body}, nil
			}
			if in.GitConnectionID == "" || in.GitProject == "" {
				return setupChange{}, fmt.Errorf("gitConnectionId and gitProject are required")
			}
			return setupChange{Summary: fmt.Sprintf("Link %s to Jira %s in %s", in.GitProject, orDash(strings.ToUpper(in.JiraProjectKey)), space(in.WorkspaceID)),
				Method: "POST", Path: "/qa/links", Body: body, WorkspaceID: in.WorkspaceID}, nil
		})
	regSetup(s, c, "qa_link_delete", "Delete a project ↔ Jira link (the project and issues are not touched).",
		func(in qaIDIn) (setupChange, error) {
			return setupChange{Summary: "Delete link " + in.ID, Method: "DELETE", Path: "/qa/links/" + url.PathEscape(in.ID)}, nil
		})
	regSetup(s, c, "qa_link_my_connection", "Read a shared link through YOUR own Git / Jira connection, so the radar knows who you are there (your reviews, MRs, Jira test queue). Both empty = back to the link's connections.",
		func(in qaMineIn) (setupChange, error) {
			return setupChange{Summary: fmt.Sprintf("Use my connections for link %s (git %s, jira %s)", in.LinkID, orDash(in.GitConnectionID), orDash(in.JiraConnectionID)),
				Method: "PUT", Path: "/qa/links/" + url.PathEscape(in.LinkID) + "/mine", Body: map[string]any{"gitConnectionId": in.GitConnectionID, "jiraConnectionId": in.JiraConnectionID}}, nil
		})
	regSetup(s, c, "qa_settings_save", "Set the radar thresholds of a QA space (only the fields you pass change).",
		func(in qaSettingsIn) (setupChange, error) {
			body := map[string]any{}
			put(body, "slaHours", in.SLAHours)
			put(body, "largeFiles", in.LargeFiles)
			put(body, "testPatterns", in.TestPatterns)
			put(body, "sourcePatterns", in.SourcePatterns)
			put(body, "testStatuses", in.TestStatuses)
			put(body, "disabledSignals", in.DisabledSignals)
			put(body, "mergedDays", in.MergedDays)
			put(body, "digestHour", in.DigestHour)
			put(body, "timezone", in.Timezone)
			put(body, "reviewGuide", in.ReviewGuide)
			if len(body) == 0 {
				return setupChange{}, fmt.Errorf("pass at least one setting")
			}
			return setupChange{Summary: fmt.Sprintf("Radar settings of %s: %v", space(in.WorkspaceID), body), Method: "PUT", Path: "/qa/settings", Body: body, WorkspaceID: in.WorkspaceID}, nil
		})
	regSetup(s, c, "qa_schedule_save", "Create (no id) or change a scheduled test run of a folder or plan on the server.",
		func(in qaScheduleSaveIn) (setupChange, error) {
			body := map[string]any{}
			put(body, "name", in.Name)
			put(body, "targetKind", in.TargetKind)
			put(body, "targetId", in.TargetID)
			put(body, "environmentId", in.EnvironmentID)
			if in.Days != "" || in.ID == "" {
				body["days"] = in.Days
			}
			put(body, "hour", in.Hour)
			put(body, "minute", in.Minute)
			put(body, "timezone", in.Timezone)
			put(body, "enabled", in.Enabled)
			if in.ID != "" {
				return setupChange{Summary: fmt.Sprintf("Update schedule %s: %v", in.ID, body), Method: "PUT", Path: "/qa/schedules/" + url.PathEscape(in.ID), Body: body}, nil
			}
			return setupChange{Summary: fmt.Sprintf("Schedule %s %s %v in %s", in.TargetKind, in.TargetID, body, space(in.WorkspaceID)), Method: "POST", Path: "/qa/schedules", Body: body, WorkspaceID: in.WorkspaceID}, nil
		})
	regSetup(s, c, "qa_schedule_delete", "Delete a scheduled test run.",
		func(in qaIDIn) (setupChange, error) {
			return setupChange{Summary: "Delete schedule " + in.ID, Method: "DELETE", Path: "/qa/schedules/" + url.PathEscape(in.ID)}, nil
		})
	regSetup(s, c, "qa_channel_save", "Add (no id) or change a Slack / Teams channel that gets MR events and the daily digest.",
		func(in qaChannelSaveIn) (setupChange, error) {
			body := map[string]any{}
			put(body, "kind", in.Kind)
			put(body, "name", in.Name)
			put(body, "url", in.URL)
			put(body, "events", in.Events)
			put(body, "enabled", in.Enabled)
			urlNote := ""
			if in.URL != "" {
				urlNote = " (with a new webhook URL)"
			}
			if in.ID != "" {
				return setupChange{Summary: fmt.Sprintf("Update channel %s%s", in.ID, urlNote), Method: "PUT", Path: "/qa/channels/" + url.PathEscape(in.ID), Body: body}, nil
			}
			return setupChange{Summary: fmt.Sprintf("Add %s channel %q for %s in %s", orDash(in.Kind), in.Name, orDash(in.Events), space(in.WorkspaceID)), Method: "POST", Path: "/qa/channels", Body: body, WorkspaceID: in.WorkspaceID}, nil
		})
	regSetup(s, c, "qa_channel_delete", "Delete a notification channel.",
		func(in qaIDIn) (setupChange, error) {
			return setupChange{Summary: "Delete channel " + in.ID, Method: "DELETE", Path: "/qa/channels/" + url.PathEscape(in.ID)}, nil
		})
	regSetup(s, c, "qa_webhook_setup", "Create the GitLab webhook of a link in GitLab (needs Maintainer there), so Radar and Inbox update at once. The secret is made and kept on the server.",
		func(in qaWebhookIn) (setupChange, error) {
			if in.LinkID == "" || in.BaseURL == "" {
				return setupChange{}, fmt.Errorf("linkId and baseUrl are required")
			}
			return setupChange{Summary: fmt.Sprintf("Create the GitLab webhook for link %s → %s", in.LinkID, in.BaseURL), Method: "POST",
				Path: "/qa/links/" + url.PathEscape(in.LinkID) + "/webhook/setup", Body: map[string]any{"baseUrl": in.BaseURL}}, nil
		})
	if allowJira {
		regCtx(s, "jira_projects", "List the Jira projects a Jira connection can see (key and name), optionally filtered.",
			func(ctx context.Context, in jiraProjectsIn) (int, string) {
				q := url.Values{}
				if in.Query != "" {
					q.Set("q", in.Query)
				}
				code, body := c.cloudOrLocalCtx(ctx, "GET", withQuery(jiraPath(in.ID, "/projects"), q), nil)
				if code != 200 {
					return code, body
				}
				var env struct {
					Data []struct {
						Key  string `json:"key"`
						Name string `json:"name"`
					} `json:"data"`
				}
				if json.Unmarshal([]byte(body), &env) != nil {
					return code, body
				}
				lines := make([]string, 0, len(env.Data))
				for _, p := range env.Data {
					lines = append(lines, p.Key+" — "+p.Name)
				}
				note := ""
				if len(env.Data) == 50 {
					note = "\n(first 50 shown; pass query to narrow)"
				}
				return code, fmt.Sprintf("%d projects:\n%s%s", len(env.Data), strings.Join(lines, "\n"), note)
			})
		regSetup(s, c, "jira_connection_create", "Add a Jira connection (Cloud: email + API token; Data Center: personal access token).",
			func(in jiraConnCreateIn) (setupChange, error) {
				if in.Name == "" || in.BaseURL == "" || in.Token == "" {
					return setupChange{}, fmt.Errorf("name, baseUrl and token are required")
				}
				body := map[string]any{"name": in.Name, "baseUrl": in.BaseURL, "secret": in.Token}
				put(body, "flavor", in.Flavor)
				put(body, "email", in.Email)
				put(body, "workspaceId", in.WorkspaceID)
				if in.ACFieldID != "" {
					body["options"] = map[string]any{"acFieldId": in.ACFieldID}
				}
				return setupChange{Summary: fmt.Sprintf("Add Jira connection %q (%s, %s) — the token is stored encrypted", in.Name, orDash(in.Flavor), in.BaseURL),
					Method: "POST", Path: "/jira-connections", Body: body}, nil
			})
	}
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
