// Package agenteval holds the MCP agent test set: 30 tasks an AI agent should
// solve with PatchMan's tools, each with the ideal tool path. The offline
// check (internal/mcp TestAgentEvalOffline) runs every ideal path against the
// server and measures its context cost; cmd/mcp-eval runs the same tasks
// against a real model (opt-in: it sends the tool list and results to the
// model provider).
package agenteval

import (
	_ "embed"
	"encoding/json"
)

type Step struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

type Task struct {
	ID          string   `json:"id"`
	Prompt      string   `json:"prompt"`
	Groups      []string `json:"groups"`
	Ideal       []Step   `json:"ideal"`
	MustNotCall []string `json:"mustNotCall,omitempty"`
}

//go:embed tasks.json
var raw []byte

// Tasks returns the test set.
func Tasks() ([]Task, error) {
	var ts []Task
	err := json.Unmarshal(raw, &ts)
	return ts, err
}
