// mcp-eval runs the MCP agent test set (internal/mcp/agenteval/tasks.json)
// against a real model connected to a PatchMan MCP endpoint, and reports per
// task: did it call the expected tools, did it avoid the forbidden ones, how
// many calls and tokens it took.
//
// It sends the tool list, the prompts and the tools' answers to the Anthropic
// API — run it only against a PatchMan with test data, and only with the
// owner's OK. Usage:
//
//	ANTHROPIC_API_KEY=… go run . -mcp http://127.0.0.1:8091/api/mcp -token pmcp_… [-model claude-opus-5-5] [-only git-] [-out report.json]
//
// Grading is mechanical (no LLM judge): success = every tool of the ideal
// path was called at least once and no mustNotCall tool was called.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type step struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

type task struct {
	ID          string   `json:"id"`
	Prompt      string   `json:"prompt"`
	Groups      []string `json:"groups"`
	Ideal       []step   `json:"ideal"`
	MustNotCall []string `json:"mustNotCall"`
}

type result struct {
	ID           string   `json:"id"`
	Success      bool     `json:"success"`
	Calls        int      `json:"calls"`
	IdealCalls   int      `json:"idealCalls"`
	Called       []string `json:"called"`
	Missing      []string `json:"missing,omitempty"`
	Forbidden    []string `json:"forbidden,omitempty"`
	InputTokens  int64    `json:"inputTokens"`
	OutputTokens int64    `json:"outputTokens"`
	Turns        int      `json:"turns"`
	Error        string   `json:"error,omitempty"`
	Answer       string   `json:"answer,omitempty"`
}

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	r.Header.Set("X-PatchMan-Client-Name", "mcp-eval")
	return b.base.RoundTrip(r)
}

func main() {
	endpoint := flag.String("mcp", "http://127.0.0.1:8091/api/mcp", "PatchMan MCP endpoint")
	token := flag.String("token", os.Getenv("PATCHMAN_MCP_TOKEN"), "MCP token (pmcp_…)")
	model := flag.String("model", "claude-opus-5-5", "model id")
	tasksPath := flag.String("tasks", "../../internal/mcp/agenteval/tasks.json", "task file")
	only := flag.String("only", "", "run tasks whose id starts with this")
	maxTurns := flag.Int("turns", 12, "max model turns per task")
	out := flag.String("out", "", "write the JSON report here")
	flag.Parse()
	if *token == "" {
		log.Fatal("an MCP token is required (-token or PATCHMAN_MCP_TOKEN)")
	}
	raw, err := os.ReadFile(*tasksPath)
	if err != nil {
		log.Fatal(err)
	}
	var tasks []task
	if err := json.Unmarshal(raw, &tasks); err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	mc := mcp.NewClient(&mcp.Implementation{Name: "mcp-eval", Version: "1"}, nil)
	sess, err := mc.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: *endpoint,
		HTTPClient: &http.Client{Transport: bearer{token: *token, base: http.DefaultTransport}, Timeout: 5 * time.Minute}}, nil)
	if err != nil {
		log.Fatalf("connect MCP: %v", err)
	}
	defer sess.Close()
	instructions := ""
	if ir := sess.InitializeResult(); ir != nil {
		instructions = ir.Instructions
	}
	lt, err := sess.ListTools(ctx, nil)
	if err != nil {
		log.Fatalf("list tools: %v", err)
	}
	tools := make([]anthropic.ToolUnionParam, 0, len(lt.Tools))
	for _, t := range lt.Tools {
		schema := anthropic.ToolInputSchemaParam{}
		if b, err := json.Marshal(t.InputSchema); err == nil {
			var m struct {
				Properties map[string]any `json:"properties"`
				Required   []string       `json:"required"`
			}
			_ = json.Unmarshal(b, &m)
			schema.Properties = m.Properties
			schema.Required = m.Required
		}
		tp := anthropic.ToolParam{Name: t.Name, Description: anthropic.String(t.Description), InputSchema: schema}
		tools = append(tools, anthropic.ToolUnionParam{OfTool: &tp})
	}

	client := anthropic.NewClient()
	system := "You are an AI agent using the PatchMan tools to answer the user. Use the tools; don't guess. Keep the final answer short.\n\n" + instructions
	var results []result
	for _, tk := range tasks {
		if *only != "" && !strings.HasPrefix(tk.ID, *only) {
			continue
		}
		r := run(ctx, client, sess, tools, *model, system, tk, *maxTurns)
		results = append(results, r)
		status := "FAIL"
		if r.Success {
			status = "ok"
		}
		fmt.Printf("%-9s %-4s calls=%d (ideal %d) in=%d out=%d %s\n", r.ID, status, r.Calls, r.IdealCalls, r.InputTokens, r.OutputTokens, r.Error)
	}
	ok, calls, in, outT := 0, 0, int64(0), int64(0)
	for _, r := range results {
		if r.Success {
			ok++
		}
		calls += r.Calls
		in += r.InputTokens
		outT += r.OutputTokens
	}
	fmt.Printf("\n%d/%d tasks passed · %d tool calls · %d input + %d output tokens · model %s\n", ok, len(results), calls, in, outT, *model)
	if *out != "" {
		b, _ := json.MarshalIndent(map[string]any{"model": *model, "passed": ok, "tasks": len(results), "calls": calls,
			"inputTokens": in, "outputTokens": outT, "results": results}, "", "  ")
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			log.Fatal(err)
		}
	}
}

func run(ctx context.Context, client anthropic.Client, sess *mcp.ClientSession, tools []anthropic.ToolUnionParam, model, system string, tk task, maxTurns int) result {
	r := result{ID: tk.ID, IdealCalls: len(tk.Ideal)}
	messages := []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(tk.Prompt))}
	called := map[string]bool{}
	for turn := 0; turn < maxTurns; turn++ {
		r.Turns++
		resp, err := client.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     anthropic.Model(model),
			MaxTokens: 16000,
			System:    []anthropic.TextBlockParam{{Text: system, CacheControl: anthropic.NewCacheControlEphemeralParam()}},
			Messages:  messages,
			Tools:     tools,
		})
		if err != nil {
			r.Error = err.Error()
			break
		}
		r.InputTokens += resp.Usage.InputTokens + resp.Usage.CacheReadInputTokens + resp.Usage.CacheCreationInputTokens
		r.OutputTokens += resp.Usage.OutputTokens
		messages = append(messages, resp.ToParam())
		if resp.StopReason == anthropic.StopReasonRefusal {
			r.Error = "refusal"
			break
		}
		results := []anthropic.ContentBlockParamUnion{}
		for _, block := range resp.Content {
			switch v := block.AsAny().(type) {
			case anthropic.TextBlock:
				r.Answer = v.Text
			case anthropic.ToolUseBlock:
				r.Calls++
				r.Called = append(r.Called, v.Name)
				called[v.Name] = true
				var args map[string]any
				_ = json.Unmarshal([]byte(v.JSON.Input.Raw()), &args)
				res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: v.Name, Arguments: args})
				if err != nil {
					results = append(results, anthropic.NewToolResultBlock(v.ID, err.Error(), true))
					continue
				}
				var sb strings.Builder
				for _, c := range res.Content {
					if tc, ok := c.(*mcp.TextContent); ok {
						sb.WriteString(tc.Text)
					}
				}
				results = append(results, anthropic.NewToolResultBlock(v.ID, sb.String(), res.IsError))
			}
		}
		if resp.StopReason != anthropic.StopReasonToolUse || len(results) == 0 {
			break
		}
		messages = append(messages, anthropic.NewUserMessage(results...))
	}
	for _, st := range tk.Ideal {
		if !called[st.Tool] {
			r.Missing = append(r.Missing, st.Tool)
		}
	}
	for _, bad := range tk.MustNotCall {
		if called[bad] {
			r.Forbidden = append(r.Forbidden, bad)
		}
	}
	r.Success = r.Error == "" && len(r.Missing) == 0 && len(r.Forbidden) == 0
	return r
}
