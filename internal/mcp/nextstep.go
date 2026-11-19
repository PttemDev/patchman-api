package mcp

// Errors an agent can act on: every failed tool call carries a nextStep that
// says what to do (ask the user to unlock, link GitLab, check the id…) instead
// of a bare status. The original error text is kept as is.

import (
	"encoding/json"
	"net/http"
	"strings"
)

func nextStepFor(code int, errText string) string {
	e := strings.ToLower(errText)
	switch {
	case strings.Contains(e, "confirm to run it"):
		return "Tell the user how much data this BigQuery query will scan (bytes in this reply) and ask whether to run it. Only if they agree, call again with confirmCost=true."
	case strings.Contains(e, "over the limit of"):
		return "The query scans more than the user's BigQuery limit. Narrow it (filter on the partition / date column, select fewer columns, use LIMIT on a smaller table) or ask the user to raise the limit in Settings."
	case code == http.StatusLocked || strings.Contains(e, "patchman is locked"):
		return "PatchMan is locked. Ask the user to unlock the app (Touch ID or password), then retry the same call."
	case code == http.StatusPreconditionRequired || strings.Contains(e, "confirm it's you"):
		return "This action needs the user's confirmation (Touch ID) in PatchMan. Ask them to approve it in the app, then retry."
	case code == http.StatusPreconditionFailed && strings.Contains(e, "gitlab"):
		return "Ask the user to link their GitLab account in PatchMan → Settings → Integrations, or use a git connection with its own access token (list_git_connections shows authKind)."
	case code == http.StatusUnauthorized:
		return "The PatchMan session is not valid. Ask the user to open PatchMan (and sign in if asked), then retry."
	case code == http.StatusForbidden && (strings.Contains(e, "write") || strings.Contains(e, "read-only") || strings.Contains(e, "readonly")):
		return "You only have read access here. Use a read-only call, or ask the connection's owner for write access (and allowWrite where the tool has it)."
	case code == http.StatusForbidden:
		return "You don't have access to this. Ask the owner to share it with the user, or use another connection from the list_* tools."
	case code == http.StatusNotFound && strings.Contains(e, "connection"):
		return "Check the connectionId with the matching list_*_connections tool; ids are UUIDs, not names."
	case code == http.StatusNotFound:
		return "Not found. Check the id or name with the matching list_*/search tool."
	case code == http.StatusTooManyRequests:
		return "Rate limited by the target system. Wait a little, then retry once."
	case code == http.StatusBadGateway || code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout:
		return "PatchMan could not reach the target system. It may be down, need VPN, or be reachable only from the user's computer (then use the desktop app's MCP server, patchman-local). Don't retry in a loop."
	case code == http.StatusBadRequest:
		return "The request was rejected as invalid. Fix the arguments the error names (see the tool's input schema) and retry."
	}
	return ""
}

// withNextStep adds "nextStep" to a JSON error body ({"error": …}); a body
// that already has one, or isn't a JSON object, gets it appended as text.
func withNextStep(code int, body string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil || m == nil {
		if step := nextStepFor(code, body); step != "" {
			return strings.TrimSpace(body) + "\nnextStep: " + step
		}
		return body
	}
	if _, has := m["nextStep"]; has {
		return body
	}
	errText, _ := m["error"].(string)
	step := nextStepFor(code, errText)
	if step == "" {
		return body
	}
	m["nextStep"] = step
	b, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return string(b)
}
