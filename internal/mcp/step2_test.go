package mcp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"patchman-be/internal/mcpscope"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func connectWith(t *testing.T, engine *gin.Engine, opts *mcpsdk.ClientOptions) *mcpsdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	srv := BuildServer(engine, testSecret, testUser, mcpscope.Parse(nil))
	clientT, serverT := mcpsdk.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, serverT, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, opts).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestResources(t *testing.T) {
	cs := connect(t, echoEngine())
	ctx := context.Background()
	lt, err := cs.ListResourceTemplates(ctx, nil)
	if err != nil || len(lt.ResourceTemplates) != 5 {
		t.Fatalf("templates: %v %v", lt, err)
	}
	for uri, path := range map[string]string{
		"patchman://request/r1":                        "/api/requests/r1",
		"patchman://collection/c1":                     "/api/collections/c1/requests",
		"patchman://k8s/k1/default/Pod/api-0":          "/api/k8s/k1/resource",
		"patchman://k8s/k1/_/Node/n1":                  "/api/k8s/k1/resource",
		"patchman://git/g1/acme%2Fapi/merge_request/7": "/api/git/connections/g1/merge_request_changes",
	} {
		res, err := cs.ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: uri})
		if err != nil {
			t.Errorf("%s: %v", uri, err)
			continue
		}
		if txt := res.Contents[0].Text; !strings.Contains(txt, path) {
			t.Errorf("%s → %s", uri, txt)
		}
	}
	res, _ := cs.ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: "patchman://k8s/k1/_/Node/n1"})
	if strings.Contains(res.Contents[0].Text, "namespace=") {
		t.Errorf("cluster-scoped '_' must not send a namespace: %s", res.Contents[0].Text)
	}
	if _, err := cs.ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: "patchman://request/a/b"}); err == nil {
		t.Error("malformed uri should be not found")
	}
}

func TestPrompts(t *testing.T) {
	cs := connect(t, echoEngine())
	ctx := context.Background()
	lp, err := cs.ListPrompts(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, p := range lp.Prompts {
		names[p.Name] = true
	}
	for _, n := range []string{"review-mr", "incident", "explain-query", "release-check"} {
		if !names[n] {
			t.Errorf("missing prompt %s (have %v)", n, names)
		}
	}
	gp, err := cs.GetPrompt(ctx, &mcpsdk.GetPromptParams{Name: "review-mr", Arguments: map[string]string{"connectionId": "g1", "project": "acme/api", "iid": "7"}})
	if err != nil {
		t.Fatal(err)
	}
	txt := gp.Messages[0].Content.(*mcpsdk.TextContent).Text
	if !strings.Contains(txt, "merge_request_changes") || !strings.Contains(txt, "merge_request_notes") || !strings.Contains(txt, "file:line") {
		t.Errorf("review prompt carries the MR data: %s", txt[:min(400, len(txt))])
	}
	// Scopes gate prompts like tools.
	cs2 := connectScoped(t, echoEngine(), mcpscope.Parse(ptr("notes")))
	lp2, _ := cs2.ListPrompts(ctx, nil)
	if lp2 == nil || len(lp2.Prompts) != 0 {
		t.Errorf("a notes-only token sees no git/db prompts: %v", lp2.Prompts)
	}
}

func ptr(s string) *string { return &s }

func slowEngine(d time.Duration, hits *atomic.Int32) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Any("/api/*path", func(c *gin.Context) {
		hits.Add(1)
		time.Sleep(d)
		c.JSON(200, gin.H{"ok": true})
	})
	return r
}

func TestProgressHeartbeat(t *testing.T) {
	old := progressInterval
	progressInterval = 10 * time.Millisecond
	defer func() { progressInterval = old }()
	var got atomic.Int32
	var hits atomic.Int32
	cs := connectWith(t, slowEngine(80*time.Millisecond, &hits), &mcpsdk.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, r *mcpsdk.ProgressNotificationClientRequest) {
			if strings.Contains(r.Params.Message, "run_db_query still running") {
				got.Add(1)
			}
		},
	})
	p := &mcpsdk.CallToolParams{Name: "run_db_query", Arguments: map[string]any{"connectionId": "d1", "query": "select 1"}}
	p.SetProgressToken("t1")
	if _, err := cs.CallTool(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if got.Load() < 2 {
		t.Errorf("progress notifications = %d", got.Load())
	}
	// No token → no notifications.
	got.Store(0)
	_, _ = cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "run_db_query", Arguments: map[string]any{"connectionId": "d1", "query": "select 1"}})
	if got.Load() != 0 {
		t.Errorf("no token, yet %d notifications", got.Load())
	}
}

func TestElicitationGate(t *testing.T) {
	var hits atomic.Int32
	answer := "decline"
	asked := 0
	cs := connectWith(t, slowEngine(0, &hits), &mcpsdk.ClientOptions{
		ElicitationHandler: func(_ context.Context, r *mcpsdk.ElicitRequest) (*mcpsdk.ElicitResult, error) {
			asked++
			if !strings.Contains(r.Params.Message, "delete_request") && !strings.Contains(r.Params.Message, "git_cancel_pipeline") {
				t.Errorf("message: %s", r.Params.Message)
			}
			if answer == "accept" {
				return &mcpsdk.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
			}
			return &mcpsdk.ElicitResult{Action: answer}, nil
		},
	})
	res := call(t, cs, "delete_request", map[string]any{"id": "r1"})
	if !res.IsError || hits.Load() != 0 || !strings.Contains(text(t, res), "declined") {
		t.Errorf("declined delete must not run: hits=%d %s", hits.Load(), text(t, res))
	}
	answer = "accept"
	if res = call(t, cs, "delete_request", map[string]any{"id": "r1"}); res.IsError || hits.Load() != 1 {
		t.Errorf("accepted delete runs: %d", hits.Load())
	}
	if res = call(t, cs, "git_cancel_pipeline", map[string]any{"connectionId": "g", "project": "p", "pipeline": 1}); res.IsError || hits.Load() != 2 {
		t.Errorf("CI write asks too: %d", hits.Load())
	}
	before := asked
	call(t, cs, "get_request", map[string]any{"id": "r1"})
	call(t, cs, "update_request", map[string]any{"id": "r1", "name": "x"})
	if asked != before {
		t.Error("reads and plain updates don't ask")
	}
	// A client without elicitation runs the delete as before.
	var h2 atomic.Int32
	cs2 := connectWith(t, slowEngine(0, &h2), nil)
	if res = call(t, cs2, "delete_request", map[string]any{"id": "r1"}); res.IsError || h2.Load() != 1 {
		t.Errorf("no elicitation → unchanged: %d", h2.Load())
	}
}

// Over the stateless HTTP endpoint in SSE mode (what the bridge asks for),
// a long call's progress notifications reach the client before the result.
func TestProgressOverStatelessSSE(t *testing.T) {
	old := progressInterval
	progressInterval = 10 * time.Millisecond
	defer func() { progressInterval = old }()
	var hits atomic.Int32
	eng := slowEngine(80*time.Millisecond, &hits)
	h := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server {
		return BuildServer(eng, testSecret, testUser, mcpscope.Parse(nil))
	}, &mcpsdk.StreamableHTTPOptions{Stateless: true})
	srv := httptest.NewServer(h)
	defer srv.Close()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"run_db_query","arguments":{"connectionId":"d","query":"select 1"},"_meta":{"progressToken":"p1"}}}`
	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	s := string(raw)
	pi, ri := strings.Index(s, "notifications/progress"), strings.Index(s, `"result"`)
	if pi < 0 || ri < 0 || pi > ri {
		t.Fatalf("progress before result over SSE: %s", s)
	}
}
