package mcp

import (
	"strings"
	"unicode"
)

// AgentHeader carries the connected agent's name on the in-process REST
// calls MCP tools make, so what an agent writes (a memory card, a
// suggestion) shows who wrote it. Must match handlers.agentHeader.
const AgentHeader = "X-PatchMan-Agent"

// ProjectHeader carries the project an agent works in: the stdio bridge
// sends its working directory's name on every message, and the MCP tools
// forward it on their REST calls. Must match handlers.projectHeader.
const ProjectHeader = "X-PatchMan-Project"

// Headers the stdio bridge adds to every message: the clientInfo of the
// agent that started it. MCP is stateless here, so without them an agent is
// known only by the name last stored on its token — and every local agent
// shares the desktop's one token.
const (
	ClientNameHeader  = "X-PatchMan-Client-Name"
	ClientTitleHeader = "X-PatchMan-Client-Title"
)

// knownAgents maps the clientInfo.name MCP clients send on initialize to a
// readable name. Unknown clients fall back to a tidied version of their name.
var knownAgents = map[string]string{
	"claude-code":      "Claude Code",
	"claude-ai":        "Claude",
	"claude-desktop":   "Claude Desktop",
	"cursor-vscode":    "Cursor",
	"cursor":           "Cursor",
	"codex-mcp-client": "Codex",
	"codex":            "Codex",
	"gemini-cli":       "Gemini CLI",
	"windsurf-client":  "Windsurf",
	"windsurf":         "Windsurf",
	"cline":            "Cline",
	"zed":              "Zed",
	"goose":            "Goose",
	"mcp-inspector":    "MCP Inspector",
}

// AgentName is how an MCP client is shown: its title when it sends one,
// else a known name, else its name with dashes as spaces and a trailing
// "mcp client" dropped ("acme-agent-mcp-client" → "Acme Agent").
func AgentName(name, title string) string {
	if t := clean(title); t != "" {
		return t
	}
	n := clean(name)
	if n == "" {
		return ""
	}
	if k, ok := knownAgents[strings.ToLower(n)]; ok {
		return k
	}
	words := strings.FieldsFunc(n, func(r rune) bool { return r == '-' || r == '_' || r == ' ' || r == '/' })
	for len(words) > 1 {
		last := strings.ToLower(words[len(words)-1])
		if last == "client" || last == "mcp" {
			words = words[:len(words)-1]
			continue
		}
		break
	}
	for i, w := range words {
		if w == strings.ToLower(w) {
			r := []rune(w)
			r[0] = unicode.ToUpper(r[0])
			words[i] = string(r)
		}
	}
	return clean(strings.Join(words, " "))
}

// clean keeps a name printable and short (it lands in a varchar(64) and in
// an HTTP header).
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if r := []rune(s); len(r) > 60 {
		s = string(r[:60])
	}
	return strings.TrimSpace(s)
}

// CleanProject keeps a project name printable and short ("" if unusable).
func CleanProject(s string) string { return clean(s) }
