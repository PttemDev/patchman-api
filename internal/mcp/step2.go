package mcp

// MCP step 2 — beyond tools:
//   - resources: patchman:// URIs an agent (or the user, by @-mentioning) can
//     read directly: a saved request, a note, a collection, a merge request,
//     a Kubernetes object. Same grants as the tools; nothing new is exposed.
//   - prompts: ready-made workflows (/patchman:review-mr, incident,
//     explain-query, release-check) that pull the right data and tell the
//     agent what to do with it.
//   - progress: a tool call that carries a progressToken gets a heartbeat
//     ("still running, 6s") every 2 s until it ends; the call's context is
//     already threaded into the request, so notifications/cancelled stops the
//     upstream work.
//   - elicitation: before a delete / CI write the server asks the user to
//     confirm — only when the client declared elicitation and the session can
//     carry a request back (not the stateless HTTP endpoint); otherwise the
//     call runs as before, guarded by grants and annotations.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"patchman-be/internal/mcpscope"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ── progress ────────────────────────────────────────────────────────────────

// progressInterval is how often a running call reports (a var for tests).
var progressInterval = 2 * time.Second

// withProgress sends heartbeats for a call that asked for progress and
// returns a stop function.
func withProgress(ctx context.Context, req *mcpsdk.CallToolRequest, name string) func() {
	if req == nil || req.Session == nil || req.Params == nil {
		return func() {}
	}
	tok := req.Params.GetProgressToken()
	if tok == nil {
		return func() {}
	}
	done := make(chan struct{})
	start := time.Now()
	go func() {
		t := time.NewTicker(progressInterval)
		defer t.Stop()
		n := 0.0
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				n++
				_ = req.Session.NotifyProgress(ctx, &mcpsdk.ProgressNotificationParams{
					ProgressToken: tok, Progress: n,
					Message: fmt.Sprintf("%s still running (%ds)", name, int(time.Since(start).Seconds())),
				})
			}
		}
	}()
	return func() { close(done) }
}

// ── elicitation ─────────────────────────────────────────────────────────────

// needsConfirm: deletes and remote writes (CI runs, cancels, publishes) ask
// the user first when the client can be asked.
func needsConfirm(name string) bool {
	if strings.HasPrefix(name, "delete_") {
		return true
	}
	k, ok := toolKinds[name]
	return ok && k&kDestructive != 0 && k&kOpenWorld != 0
}

// canElicit: the client declared elicitation in initialize (a stateless HTTP
// session never has initialize params, so it never asks).
func canElicit(req *mcpsdk.CallToolRequest) bool {
	if req == nil || req.Session == nil {
		return false
	}
	ip := req.Session.InitializeParams()
	return ip != nil && ip.Capabilities != nil && ip.Capabilities.Elicitation != nil
}

// confirmCall asks the user before a destructive call. ok=false → don't run;
// the string says why (declined / cancelled).
func confirmCall(ctx context.Context, req *mcpsdk.CallToolRequest, name string) (bool, string) {
	if !needsConfirm(name) || !canElicit(req) {
		return true, ""
	}
	args := ""
	if req.Params != nil && len(req.Params.Arguments) > 0 {
		args = string(req.Params.Arguments)
		if len(args) > 400 {
			args = args[:400] + "…"
		}
	}
	res, err := req.Session.Elicit(ctx, &mcpsdk.ElicitParams{
		Message: fmt.Sprintf("Allow the agent to run %s?\n%s", name, args),
		RequestedSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"confirm": map[string]any{"type": "boolean", "title": "Run it", "default": false},
			},
			"required": []string{"confirm"},
		},
	})
	if err != nil {
		// The client said it could but the request failed: fall back to the
		// pre-elicitation behaviour rather than blocking every write.
		return true, ""
	}
	if res.Action != "accept" {
		return false, "The user " + map[string]string{"decline": "declined", "cancel": "cancelled"}[res.Action] + " " + name + "."
	}
	if v, _ := res.Content["confirm"].(bool); !v {
		return false, "The user didn't confirm " + name + "."
	}
	return true, ""
}

// ── resources ───────────────────────────────────────────────────────────────

func textResource(uri string, code int, body string) (*mcpsdk.ReadResourceResult, error) {
	if code == http.StatusNotFound {
		return nil, mcpsdk.ResourceNotFoundError(uri)
	}
	if code >= 400 {
		return nil, fmt.Errorf("patchman %d: %s", code, strings.TrimSpace(body))
	}
	return &mcpsdk.ReadResourceResult{Contents: []*mcpsdk.ResourceContents{{URI: uri, MIMEType: "application/json", Text: compactReply(body)}}}, nil
}

// uriParts splits patchman://<kind>/<a>/<b>… into unescaped segments.
func uriParts(uri, kind string) ([]string, bool) {
	rest, ok := strings.CutPrefix(uri, "patchman://"+kind+"/")
	if !ok {
		return nil, false
	}
	raw := strings.Split(rest, "/")
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		u, err := url.PathUnescape(r)
		if err != nil || u == "" {
			return nil, false
		}
		out = append(out, u)
	}
	return out, true
}

func registerResources(s *mcpsdk.Server, c *caller, scopes mcpscope.ToolScopes) {
	add := func(tmpl, name, desc string, h func(ctx context.Context, uri string) (*mcpsdk.ReadResourceResult, error)) {
		s.AddResourceTemplate(&mcpsdk.ResourceTemplate{URITemplate: tmpl, Name: name, Description: desc, MIMEType: "application/json"},
			func(ctx context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
				return h(ctx, req.Params.URI)
			})
	}
	if scopes.Allows(mcpscope.GroupRequests) {
		add("patchman://request/{id}", "Saved request", "A saved HTTP request: method, URL, headers, body, auth (secrets masked as the API returns them).",
			func(ctx context.Context, uri string) (*mcpsdk.ReadResourceResult, error) {
				p, ok := uriParts(uri, "request")
				if !ok || len(p) != 1 {
					return nil, mcpsdk.ResourceNotFoundError(uri)
				}
				code, body := c.callCtx(ctx, "GET", "/api/requests/"+url.PathEscape(p[0]), nil)
				return textResource(uri, code, body)
			})
	}
	if scopes.Allows(mcpscope.GroupCollections) {
		add("patchman://collection/{id}", "Collection", "A collection's saved requests, in the app's order.",
			func(ctx context.Context, uri string) (*mcpsdk.ReadResourceResult, error) {
				p, ok := uriParts(uri, "collection")
				if !ok || len(p) != 1 {
					return nil, mcpsdk.ResourceNotFoundError(uri)
				}
				code, body := c.callCtx(ctx, "GET", "/api/collections/"+url.PathEscape(p[0])+"/requests", nil)
				return textResource(uri, code, body)
			})
	}
	if scopes.Allows(mcpscope.GroupNotes) {
		add("patchman://note/{id}", "Note", "A note or memory card: markdown body, title, version.",
			func(ctx context.Context, uri string) (*mcpsdk.ReadResourceResult, error) {
				p, ok := uriParts(uri, "note")
				if !ok || len(p) != 1 {
					return nil, mcpsdk.ResourceNotFoundError(uri)
				}
				code, body := c.cloudOrLocalCtx(ctx, "GET", "/api/notes/"+url.PathEscape(p[0]), nil)
				if code >= 400 {
					return textResource(uri, code, body)
				}
				// Markdown reads better than JSON for a note.
				var n struct {
					Data struct {
						Title string `json:"title"`
						Body  string `json:"body"`
					} `json:"data"`
				}
				if json.Unmarshal([]byte(body), &n) == nil && (n.Data.Title != "" || n.Data.Body != "") {
					return &mcpsdk.ReadResourceResult{Contents: []*mcpsdk.ResourceContents{{URI: uri, MIMEType: "text/markdown", Text: "# " + n.Data.Title + "\n\n" + n.Data.Body}}}, nil
				}
				return textResource(uri, code, body)
			})
	}
	if scopes.Allows(mcpscope.GroupGit) {
		add("patchman://git/{connectionId}/{project}/merge_request/{iid}", "Merge / pull request", "A merge request (GitLab) or pull request (GitHub) with its changed files. project is url-escaped ('group%2Frepo' or a numeric id).",
			func(ctx context.Context, uri string) (*mcpsdk.ReadResourceResult, error) {
				p, ok := uriParts(uri, "git")
				if !ok || len(p) != 4 || p[2] != "merge_request" {
					return nil, mcpsdk.ResourceNotFoundError(uri)
				}
				v := url.Values{"project": {p[1]}, "iid": {p[3]}}
				code, mr := c.gitCallCtx(ctx, p[0], "GET", gitPath(p[0], "/merge_request", v), nil)
				if code >= 400 {
					return textResource(uri, code, mr)
				}
				code, ch := c.gitCallCtx(ctx, p[0], "GET", gitPath(p[0], "/merge_request_changes", v), nil)
				if code >= 400 {
					return textResource(uri, code, ch)
				}
				return textResource(uri, 200, `{"mergeRequest":`+mr+`,"changes":`+ch+`}`)
			})
	}
	if scopes.Allows(mcpscope.GroupK8s) {
		add("patchman://k8s/{connectionId}/{namespace}/{kind}/{name}", "Kubernetes object", "One Kubernetes object as JSON; namespace '_' for cluster-scoped kinds.",
			func(ctx context.Context, uri string) (*mcpsdk.ReadResourceResult, error) {
				p, ok := uriParts(uri, "k8s")
				if !ok || len(p) != 4 {
					return nil, mcpsdk.ResourceNotFoundError(uri)
				}
				v := url.Values{"kind": {p[2]}, "name": {p[3]}}
				if p[1] != "_" {
					v.Set("namespace", p[1])
				}
				code, body := c.callCtx(ctx, "GET", withQuery("/api/k8s/"+url.PathEscape(p[0])+"/resource", v), nil)
				return textResource(uri, code, body)
			})
	}
}

// ── prompts ─────────────────────────────────────────────────────────────────

func promptText(desc, text string) *mcpsdk.GetPromptResult {
	return &mcpsdk.GetPromptResult{Description: desc, Messages: []*mcpsdk.PromptMessage{{Role: "user", Content: &mcpsdk.TextContent{Text: text}}}}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…(cut; fetch the rest with the tools)"
}

func registerPrompts(s *mcpsdk.Server, c *caller, scopes mcpscope.ToolScopes) {
	arg := func(name, desc string, req bool) *mcpsdk.PromptArgument {
		return &mcpsdk.PromptArgument{Name: name, Description: desc, Required: req}
	}
	if scopes.Allows(mcpscope.GroupGit) {
		s.AddPrompt(&mcpsdk.Prompt{Name: "review-mr", Title: "Review a merge / pull request",
			Description: "Review a GitLab merge request or GitHub pull request: correctness, security, tests; line comments ready to post.",
			Arguments:   []*mcpsdk.PromptArgument{arg("connectionId", "git connection id (list_git_connections)", true), arg("project", "project id or path", true), arg("iid", "merge request iid / PR number", true)}},
			func(ctx context.Context, req *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
				a := req.Params.Arguments
				v := url.Values{"project": {a["project"]}, "iid": {a["iid"]}}
				code, mr := c.gitCallCtx(ctx, a["connectionId"], "GET", gitPath(a["connectionId"], "/merge_request", v), nil)
				if code >= 400 {
					return nil, fmt.Errorf("merge request: %s", mr)
				}
				_, ch := c.gitCallCtx(ctx, a["connectionId"], "GET", gitPath(a["connectionId"], "/merge_request_changes", v), nil)
				_, notes := c.gitCallCtx(ctx, a["connectionId"], "GET", gitPath(a["connectionId"], "/merge_request_notes", v), nil)
				return promptText("Review "+a["project"]+" !"+a["iid"], fmt.Sprintf(`Review this merge request as a senior engineer.

1. Say in two sentences what it changes and whether the description matches the diff.
2. List real problems first — bugs, security, data loss, concurrency, missing error handling — each with file:line and why it matters. Then smaller points. Skip style nits a linter would catch.
3. Say which tests are missing.
4. Before posting, check recall for this project's conventions. Post comments with git_create_merge_request_note only if the user asks; approve with git_approve_merge_request only if the user asks.

Merge request:
%s

Changes:
%s

Existing comments (don't repeat them):
%s`, compactReply(mr), clip(compactReply(ch), 60000), clip(compactReply(notes), 8000))), nil
			})
		s.AddPrompt(&mcpsdk.Prompt{Name: "release-check", Title: "Release readiness check",
			Description: "Is this ref ready to ship? Latest pipeline / workflow runs, failures and their logs, open merge requests into it.",
			Arguments:   []*mcpsdk.PromptArgument{arg("connectionId", "git connection id", true), arg("project", "project id or path", true), arg("ref", "branch or tag (default: the default branch)", false)}},
			func(ctx context.Context, req *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
				a := req.Params.Arguments
				v := url.Values{"project": {a["project"]}}
				if a["ref"] != "" {
					v.Set("ref", a["ref"])
				}
				_, pipes := c.gitCallCtx(ctx, a["connectionId"], "GET", gitPath(a["connectionId"], "/pipelines", v), nil)
				_, mrs := c.gitCallCtx(ctx, a["connectionId"], "GET", gitPath(a["connectionId"], "/merge_requests", url.Values{"project": {a["project"]}, "state": {"opened"}}), nil)
				return promptText("Release check for "+a["project"], fmt.Sprintf(`Decide whether %s (ref %q) is ready to release.
- From the pipelines below, take the latest one on the ref. If it failed, open it with git_pipeline and read the failed jobs' logs with git_job_log; say what broke and whether it looks flaky or real.
- List open merge requests targeting the ref that look release-relevant.
- Check recall for release notes or known issues of this project.
- End with GO / NO-GO and the reasons, and what to do next.

Recent pipelines:
%s

Open merge requests:
%s`, a["project"], a["ref"], clip(compactReply(pipes), 20000), clip(compactReply(mrs), 12000))), nil
			})
	}
	if scopes.Allows(mcpscope.GroupDB) {
		s.AddPrompt(&mcpsdk.Prompt{Name: "explain-query", Title: "Explain and tune a query",
			Description: "Explain what a SQL / Mongo / BigQuery query does, its plan and cost, and how to make it faster — without changing data.",
			Arguments:   []*mcpsdk.PromptArgument{arg("connectionId", "DB connection id (list_db_connections)", true), arg("query", "the query", true)}},
			func(ctx context.Context, req *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
				a := req.Params.Arguments
				return promptText("Explain a query", fmt.Sprintf(`Explain this query on DB connection %s and suggest how to make it faster.
1. In plain words: what it returns and which tables / collections it touches.
2. Get the plan read-only with run_db_query: EXPLAIN (Postgres / MySQL: EXPLAIN ANALYZE only for a SELECT), SET SHOWPLAN_XML for SQL Server, explain for Mongo; BigQuery's dry run reports the bytes it would scan. Never run a write.
3. Point at full scans, missing indexes, bad joins, N+1 shapes; propose concrete indexes or rewrites and estimate the gain.
4. Check recall for this database's conventions before proposing schema changes.

Query:
%s`, a["connectionId"], a["query"])), nil
			})
	}
	if scopes.Allows(mcpscope.GroupK8s) || scopes.Allows(mcpscope.GroupObservability) || scopes.Allows(mcpscope.GroupGraylog) {
		s.AddPrompt(&mcpsdk.Prompt{Name: "incident", Title: "Investigate an incident",
			Description: "Work an incident from symptom to cause: memory first, then service map, Kubernetes, logs and dashboards; ends with a timeline and next steps.",
			Arguments:   []*mcpsdk.PromptArgument{arg("service", "affected service / app", true), arg("symptom", "what is wrong (errors, latency, …)", true), arg("since", "when it started (e.g. 30m, 2h)", false)}},
			func(ctx context.Context, req *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
				a := req.Params.Arguments
				since := a["since"]
				if since == "" {
					since = "1h"
				}
				return promptText("Incident: "+a["service"], fmt.Sprintf(`Investigate an incident. Service: %s. Symptom: %s. Since: %s.
Work read-only unless the user says otherwise.
1. recall(q: "%s incident runbook") — past incidents, runbooks, owners.
2. service_map — callers / dependencies of the service and their error rates; then service_detail for its throughput, latency, error classes and alerts over the window.
3. Kubernetes: k8s_list_resources / k8s_get_resource for its pods (restarts, OOMKilled, pending), recent rollouts.
4. Logs: graylog_search or es_data_view_search for errors in the window; group them by message.
5. Dashboards: grafana_query for latency / error rate around the start time.
6. Write a timeline (what changed when), the most likely cause with evidence, and the next steps. Save the root cause with remember once it is confirmed.`, a["service"], a["symptom"], since, a["service"])), nil
			})
	}
}
