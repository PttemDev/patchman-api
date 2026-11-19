package mcp

import (
	"strings"

	"patchman-be/internal/mcpinstr"
	"patchman-be/internal/mcpscope"
)

// Rules shared by many tools, sent once as server instructions instead of in
// every tool description.
const connectionRules = `Connections (databases, Elasticsearch, Redis, Kafka, S3, Kubernetes, Grafana, RabbitMQ, Graylog, observability, Git, Google Cloud):
- Find a connection's id with the matching list_*_connections tool, then pass it as connectionId (Google Cloud: list_gcp_credentials → credentialId). It is resolved under your grants: you see the user's own connections and the ones shared with them.
- A connection kept only in the PatchMan desktop app (local scope) is reachable only through the desktop app's MCP server (patchman-local), not through a hosted PatchMan server.
- Tool annotations say which tools only read (readOnlyHint) and which may change things (destructiveHint). Writes still need the user's grants, and some need allowWrite=true on a read-write connection.`

const gitRules = `Git tools (GitLab and GitHub):
- list_git_connections shows each connection with runsOn: local (runs on this computer) or cloud (runs on the PatchMan server). Use any of them the same way; PatchMan routes the call.
- A token connection calls GitLab / GitHub with its own access token. A GitLab connection without one authenticates as the user's linked GitLab account; when it isn't linked the call fails with 412 — ask the user to link GitLab in PatchMan Settings.
- project is a GitLab project (numeric id or 'group/repo') or a GitHub repository (numeric id or 'owner/name').
- On GitHub the same tools work with GitHub's names: merge request = pull request (iid = PR number), pipeline = workflow run, job = workflow job, schedule = a workflow with on: schedule (its id is the workflow id). git_run_pipeline needs workflow (see git_workflows); GitHub has no manual jobs (git_play_job fails), and secrets list names only — GitHub never returns their values.`

// serverInstructions assembles the instructions for the tool groups this
// token can use.
func serverInstructions(scopes mcpscope.ToolScopes) string {
	var parts []string
	if scopes.Allows(mcpscope.GroupNotes) {
		parts = append(parts, mcpinstr.MemoryInstructions)
	}
	if scopes.Allows(mcpscope.GroupConnections) || scopes.Allows(mcpscope.GroupDB) || scopes.Allows(mcpscope.GroupK8s) ||
		scopes.Allows(mcpscope.GroupGit) || scopes.Allows(mcpscope.GroupES) || scopes.Allows(mcpscope.GroupRedis) {
		parts = append(parts, connectionRules)
	}
	if scopes.Allows(mcpscope.GroupGit) {
		parts = append(parts, gitRules)
	}
	return strings.Join(parts, "\n\n")
}
