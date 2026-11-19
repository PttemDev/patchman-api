package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"patchman-be/internal/mcp/agenteval"
	"patchman-be/internal/mcpscope"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestAgentEvalOffline runs each task's ideal tool path through the MCP
// server (echo backend): every tool must exist for the task's scopes, accept
// the arguments, and the forbidden tools must be writes. It reports what the
// path costs an agent: tools/list for the scopes + the calls (bytes, ~tokens
// = bytes/4). AGENT_EVAL_REPORT=<file> writes the numbers as JSON.
func TestAgentEvalOffline(t *testing.T) {
	tasks, err := agenteval.Tasks()
	if err != nil || len(tasks) != 30 {
		t.Fatalf("tasks: %d %v", len(tasks), err)
	}
	type row struct {
		ID        string `json:"id"`
		Calls     int    `json:"calls"`
		ListBytes int    `json:"toolsListBytes"`
		CallBytes int    `json:"callBytes"`
		Tokens    int    `json:"approxTokens"`
		Tools     int    `json:"toolsVisible"`
	}
	var rows []row
	seen := map[string]bool{}
	for _, tk := range tasks {
		if seen[tk.ID] {
			t.Errorf("duplicate id %s", tk.ID)
		}
		seen[tk.ID] = true
		groups := append([]string{mcpscope.GroupConnections}, tk.Groups...)
		gs := strings.Join(groups, ",")
		cs := connectScoped(t, echoEngine(), mcpscope.Parse(&gs))
		lt, err := cs.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		lb, _ := json.Marshal(lt.Tools)
		kinds := map[string]*mcpsdk.ToolAnnotations{}
		for _, tl := range lt.Tools {
			kinds[tl.Name] = tl.Annotations
		}
		r := row{ID: tk.ID, Calls: len(tk.Ideal), ListBytes: len(lb), Tools: len(lt.Tools)}
		for _, st := range tk.Ideal {
			if _, ok := kinds[st.Tool]; !ok {
				t.Errorf("%s: tool %s isn't available with scopes %v", tk.ID, st.Tool, groups)
				continue
			}
			res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: st.Tool, Arguments: st.Args})
			if err != nil {
				t.Errorf("%s: %s rejected the arguments: %v", tk.ID, st.Tool, err)
				continue
			}
			if res.IsError {
				t.Errorf("%s: %s failed: %s", tk.ID, st.Tool, text(t, res))
			}
			ab, _ := json.Marshal(st.Args)
			r.CallBytes += len(ab) + len(text(t, res))
		}
		for _, bad := range tk.MustNotCall {
			if a, ok := kinds[bad]; ok && a != nil && a.ReadOnlyHint {
				t.Errorf("%s: forbids %s, which is read-only — the check means nothing", tk.ID, bad)
			}
		}
		r.Tokens = (r.ListBytes + r.CallBytes) / 4
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	total, calls := 0, 0
	var b strings.Builder
	for _, r := range rows {
		total += r.Tokens
		calls += r.Calls
		fmt.Fprintf(&b, "%-9s calls=%d tools=%3d list=%6dB ~%5d tok\n", r.ID, r.Calls, r.Tools, r.ListBytes, r.Tokens)
	}
	t.Logf("agent eval (ideal paths, echo backend):\n%s30 tasks · %d calls · ~%d tokens (avg ~%d/task)", b.String(), calls, total, total/len(rows))
	if p := os.Getenv("AGENT_EVAL_REPORT"); p != "" {
		out, _ := json.MarshalIndent(map[string]any{"tasks": rows, "calls": calls, "approxTokens": total}, "", "  ")
		_ = os.WriteFile(p, out, 0o644)
	}
}
