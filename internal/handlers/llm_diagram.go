package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	patchmcp "patchman-be/internal/mcp"
	"patchman-be/internal/mcpscope"
	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"gorm.io/gorm"
)

// LLMDiagramHandler powers "AI draws a diagram from our live data". It runs a
// bounded, provider-agnostic ReAct loop SERVER-SIDE: the model is given a
// read-only slice of the user's MCP tools (ES indices, K8s topology, Kafka
// topics, collections, …), inspects live data by emitting a strict ACTION/ARGS
// directive we execute in-process (as the user, through the same gin engine +
// auth stack the MCP server uses), and finishes by emitting a Mermaid diagram
// the FE converts onto the canvas.
//
// It is deliberately SEPARATE from LLMProviderHandler.Chat (the streaming
// single-turn proxy) so the existing chat contract is untouched. Tool-calling
// uses a text protocol rather than provider-native function-calling, so it works
// with ANY OpenAI-/Anthropic-compatible endpoint (incl. local models) uniformly.
type LLMDiagramHandler struct {
	db        *gorm.DB
	cipher    *services.TokenCipher
	engine    *gin.Engine
	jwtSecret string
	http      *http.Client
}

func NewLLMDiagramHandler(db *gorm.DB, cipher *services.TokenCipher, engine *gin.Engine, jwtSecret string) *LLMDiagramHandler {
	return &LLMDiagramHandler{db: db, cipher: cipher, engine: engine, jwtSecret: jwtSecret, http: &http.Client{Timeout: 90 * time.Second}}
}

// diagramReadOnlyTools is the allowlist the loop may call. Every entry is a
// non-mutating inspection tool — arbitrary-write surfaces (run_db_query,
// es_request, redis_command, send_http_request, all create/update/delete, git
// mutations) are intentionally excluded so an autonomous loop can never change
// or destroy data.
// Every entry is non-mutating AND non-secret-bearing. Deliberately EXCLUDED even
// though read-only, because their output can carry credential material that would
// then be shipped to a third-party LLM in the prompt: list_environments (env var
// VALUES are the app's token store), k8s_get_resource (can fetch Secret/ConfigMap
// bodies), list_collection_requests (saved requests carry Authorization headers).
// k8s topology stays available via *_overview / *_list_resources (names only).
var diagramReadOnlyTools = map[string]bool{
	"list_db_connections": true, "list_es_connections": true, "list_k8s_connections": true,
	"list_kafka_connections": true, "list_redis_connections": true, "list_s3_connections": true,
	"list_collections": true,
	"es_indices":       true, "es_mapping": true, "es_cluster": true,
	"k8s_overview": true, "k8s_contexts": true, "k8s_list_resources": true,
	"kafka_topics": true, "kafka_groups": true, "kafka_lag": true, "kafka_brokers": true,
	"redis_scan_keys": true, "redis_key_meta": true,
	"s3_list_buckets": true, "s3_list_objects": true,
	// The service map never carries configuration values (names + extracted
	// host:port only), so it is safe to hand to the model.
	"list_observability_connections": true, "service_map": true,
}

type diagramRequest struct {
	ProviderID string `json:"providerId"`
	Model      string `json:"model"`
	Prompt     string `json:"prompt" binding:"required"`
}

const diagramMaxSteps = 6
const diagramObsLimit = 3000

var actionRe = regexp.MustCompile(`(?m)^\s*ACTION:\s*([A-Za-z0-9_]+)\s*$`)
var argsRe = regexp.MustCompile(`(?m)^\s*ARGS:\s*(\{.*\})\s*$`)
var mermaidFenceRe = regexp.MustCompile("(?s)```(?:mermaid)?\\s*\\n?(.*?)```")
var thinkRe = regexp.MustCompile(`(?s)<think>.*?</think>`)

// stripThink removes reasoning-model <think>…</think> blocks so they don't
// confuse the ACTION / mermaid parsing (qwen3 and similar emit them).
func stripThink(s string) string {
	return strings.TrimSpace(thinkRe.ReplaceAllString(s, ""))
}

func (h *LLMDiagramHandler) Generate(c *gin.Context) {
	userID := c.GetString("userID")
	var req diagramRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var prov models.LLMProvider
	if err := h.db.Where("id = ? AND user_id = ?", req.ProviderID, userID).First(&prov).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Provider not found"})
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = prov.Model
	}
	if model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "a model is required"})
		return
	}
	key := ""
	if prov.APIKeyEnc != "" {
		k, err := h.cipher.Decrypt(prov.APIKeyEnc)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "decrypt key"})
			return
		}
		key = k
	}

	// Bound the whole loop so a stalled provider can't pin the request (and the
	// MCP server goroutine dies with the context).
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Minute)
	defer cancel()

	// Spin up an in-memory MCP session acting as this user, reusing the exact
	// tool implementations + auth stack the external MCP server uses.
	session, closeSession, err := h.openMCP(ctx, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "tools unavailable: " + err.Error()})
		return
	}
	defer closeSession()

	toolList, toolNames := h.describeTools(ctx, session)
	system := diagramSystemPrompt(toolList)

	msgs := []map[string]string{{"role": "user", "content": req.Prompt}}
	steps := []string{}
	for i := 0; i < diagramMaxSteps; i++ {
		text, err := h.complete(ctx, &prov, key, model, system, msgs)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		text = stripThink(text)
		msgs = append(msgs, map[string]string{"role": "assistant", "content": text})

		if m := actionRe.FindStringSubmatch(text); m != nil {
			tool := m[1]
			args := "{}"
			if am := argsRe.FindStringSubmatch(text); am != nil {
				args = am[1]
			}
			obs := h.runTool(ctx, session, tool, args, toolNames)
			steps = append(steps, tool)
			msgs = append(msgs, map[string]string{"role": "user", "content": "OBSERVATION:\n" + obs})
			continue
		}
		if mm := mermaidFenceRe.FindStringSubmatch(text); mm != nil {
			c.JSON(http.StatusOK, gin.H{"data": gin.H{"mermaid": strings.TrimSpace(mm[1]), "steps": steps}})
			return
		}
		// No action, no fenced diagram — nudge once toward the final format.
		msgs = append(msgs, map[string]string{"role": "user", "content": "Now reply with ONLY a ```mermaid fenced diagram and nothing else."})
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"mermaid": "", "steps": steps, "error": "The model didn't produce a diagram in time."}})
}

// openMCP wires an in-memory client↔server MCP session for userID.
func (h *LLMDiagramHandler) openMCP(ctx context.Context, userID string) (*mcpsdk.ClientSession, func(), error) {
	server := patchmcp.BuildServer(h.engine, h.jwtSecret, userID, mcpscope.Parse(nil))
	clientT, serverT := mcpsdk.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, serverT) }()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "patchman-diagram", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		return nil, func() {}, err
	}
	return session, func() { _ = session.Close() }, nil
}

// describeTools lists the session's tools, keeps only the read-only allowlist,
// and renders a compact catalogue for the system prompt.
func (h *LLMDiagramHandler) describeTools(ctx context.Context, s *mcpsdk.ClientSession) (string, map[string]bool) {
	names := map[string]bool{}
	res, err := s.ListTools(ctx, nil)
	if err != nil || res == nil {
		return "(no tools available)", names
	}
	var b strings.Builder
	for _, t := range res.Tools {
		if !diagramReadOnlyTools[t.Name] {
			continue
		}
		names[t.Name] = true
		schema := ""
		if t.InputSchema != nil {
			if raw, e := json.Marshal(t.InputSchema); e == nil {
				schema = compactSchema(string(raw))
			}
		}
		fmt.Fprintf(&b, "- %s: %s\n  args: %s\n", t.Name, oneLine(t.Description), schema)
	}
	if b.Len() == 0 {
		return "(no data connections are configured yet)", names
	}
	return b.String(), names
}

func (h *LLMDiagramHandler) runTool(ctx context.Context, s *mcpsdk.ClientSession, tool, argsJSON string, allowed map[string]bool) string {
	if !allowed[tool] {
		return fmt.Sprintf("ERROR: tool %q is not available. Choose one from the provided list.", tool)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "ERROR: ARGS was not valid JSON. Provide a one-line JSON object."
	}
	res, err := s.CallTool(ctx, &mcpsdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return "ERROR: " + err.Error()
	}
	var out strings.Builder
	for _, ct := range res.Content {
		if tc, ok := ct.(*mcpsdk.TextContent); ok {
			out.WriteString(tc.Text)
		}
	}
	s2 := out.String()
	if len(s2) > diagramObsLimit {
		s2 = s2[:diagramObsLimit] + "\n…(truncated)"
	}
	if res.IsError {
		return "TOOL ERROR: " + s2
	}
	return s2
}

// complete makes ONE non-streaming completion call to the provider.
func (h *LLMDiagramHandler) complete(ctx context.Context, prov *models.LLMProvider, key, model, system string, msgs []map[string]string) (string, error) {
	return llmComplete(ctx, h.http, prov, key, model, system, msgs, 1500)
}

func diagramSystemPrompt(tools string) string {
	return "You are a diagramming assistant embedded in an API/infra tool. You draw Mermaid " +
		"diagrams that visualize the user's OWN live data and infrastructure.\n\n" +
		"You can inspect live data with READ-ONLY tools. To call one, reply with EXACTLY two lines " +
		"and nothing else:\n" +
		"ACTION: <tool_name>\n" +
		"ARGS: <one-line JSON object of arguments, {} if none>\n\n" +
		"You will then receive an OBSERVATION with the result. Call tools only when live data is " +
		"needed; keep calls minimal. When you have enough information (or the request needs none), " +
		"reply with ONLY a Mermaid diagram inside a ```mermaid fenced block — no prose, no ACTION.\n\n" +
		"Use a connection id from a list_* tool before calling a tool that needs one.\n\n" +
		"Available tools:\n" + tools
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 160 {
		s = s[:160]
	}
	return strings.TrimSpace(s)
}

// compactSchema shrinks a JSON-schema blob to just its property names so the
// prompt stays small: {"a":..,"b":..} → "a, b".
func compactSchema(raw string) string {
	var sch struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if json.Unmarshal([]byte(raw), &sch) != nil || len(sch.Properties) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(sch.Properties))
	req := map[string]bool{}
	for _, r := range sch.Required {
		req[r] = true
	}
	for k := range sch.Properties {
		if req[k] {
			k += "*"
		}
		keys = append(keys, k)
	}
	return strings.Join(keys, ", ")
}
