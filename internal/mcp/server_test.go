package mcp

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"patchman-be/internal/mcpscope"
	"patchman-be/internal/middleware"

	"github.com/gin-gonic/gin"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	testSecret = "test-secret"
	testUser   = "user-123"
)

// echoEngine is a JWT-protected catch-all that reflects what an MCP tool's
// in-process call actually sent: the resolved userID (proving the minted
// JWT was accepted), the HTTP method, the path, the query string and the
// raw body. Tools are asserted against this echo.
func echoEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	api.Use(middleware.JWTAuth(testSecret, 7*24*time.Hour))
	api.Any("/*path", func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		c.JSON(200, gin.H{
			"userID": c.GetString("userID"),
			"method": c.Request.Method,
			"path":   c.Request.URL.Path,
			"query":  c.Request.URL.RawQuery,
			"body":   string(body),
		})
	})
	return r
}

// statusEngine always answers with the given status — used to assert that
// a non-2xx backend response surfaces as an MCP tool error.
func statusEngine(status int) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	api.Use(middleware.JWTAuth(testSecret, 7*24*time.Hour))
	api.Any("/*path", func(c *gin.Context) {
		c.JSON(status, gin.H{"error": "boom"})
	})
	return r
}

func connect(t *testing.T, engine *gin.Engine) *mcpsdk.ClientSession {
	t.Helper()
	// nil scopes → all tool groups (the default), matching the pre-scopes surface.
	return connectScoped(t, engine, mcpscope.Parse(nil))
}

func connectScoped(t *testing.T, engine *gin.Engine, scopes mcpscope.ToolScopes) *mcpsdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	srv := BuildServer(engine, testSecret, testUser, scopes)
	clientT, serverT := mcpsdk.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcpsdk.ClientSession, name string, args any) *mcpsdk.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

func text(t *testing.T, res *mcpsdk.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("result has no content")
	}
	tc, ok := res.Content[0].(*mcpsdk.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want *TextContent", res.Content[0])
	}
	return tc.Text
}

type echo struct {
	UserID string `json:"userID"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Query  string `json:"query"`
	Body   string `json:"body"`
}

func decode(t *testing.T, res *mcpsdk.CallToolResult) echo {
	t.Helper()
	raw := text(t, res)
	var e echo
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatalf("decode echo: %v (raw=%q)", err, raw)
	}
	return e
}

// TestScopedToolsFiltered asserts that a restricted token exposes ONLY the
// tool groups in its scopes — enforcement, not just display. nil scopes must
// still expose everything (the default), so an existing token is unaffected.
func TestScopedToolsFiltered(t *testing.T) {
	// Restrict to db + http groups only.
	scoped := "db,http"
	cs := connectScoped(t, echoEngine(), mcpscope.Parse(&scoped))
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	got := map[string]bool{}
	for _, tl := range res.Tools {
		got[tl.Name] = true
	}
	if !got["run_db_query"] {
		t.Error("run_db_query should be present under scope 'db'")
	}
	if !got["send_http_request"] {
		t.Error("send_http_request should be present under scope 'http'")
	}
	if got["list_collections"] {
		t.Error("list_collections must NOT be present when 'collections' is not scoped")
	}
	if got["kafka_topics"] {
		t.Error("kafka_topics must NOT be present when 'kafka' is not scoped")
	}

	// Empty (non-nil) scopes → no tools at all.
	empty := ""
	csNone := connectScoped(t, echoEngine(), mcpscope.Parse(&empty))
	resNone, err := csNone.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools (none): %v", err)
	}
	if len(resNone.Tools) != 0 {
		t.Errorf("empty scopes tool count = %d, want 0", len(resNone.Tools))
	}
}

// TestListTools asserts the full tool surface is registered.
func TestListTools(t *testing.T) {
	cs := connect(t, echoEngine())
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	got := map[string]bool{}
	for _, tl := range res.Tools {
		got[tl.Name] = true
	}
	if len(got) != 119 {
		t.Errorf("tool count = %d, want 119", len(got))
	}
	for _, want := range []string{
		"list_collections", "create_collection", "update_collection", "delete_collection",
		"reorder_collections", "reorder_requests",
		"create_request", "update_request", "delete_request", "get_request",
		"list_environments", "create_environment",
		"list_test_cases", "create_test_case",
		"list_test_folders", "create_test_folder", "rename_test_folder", "delete_test_folder",
		"move_test_case", "run_test_folder",
		"list_mock_servers", "create_mock_route",
		// Execution + discovery surface.
		"list_db_connections", "list_es_connections", "list_redis_connections",
		"list_kafka_connections", "list_s3_connections", "list_k8s_connections", "list_grafana_connections",
		"list_rabbitmq_connections",
		"send_http_request", "run_db_query",
		"es_cluster", "es_indices", "es_mapping", "es_request",
		"redis_scan_keys", "redis_get", "redis_key_meta", "redis_command",
		"kafka_topics", "kafka_brokers", "kafka_groups", "kafka_lag", "kafka_consume",
		"s3_list_buckets", "s3_list_objects", "s3_get_object",
		"k8s_contexts", "k8s_overview", "k8s_list_resources", "k8s_get_resource",
		"list_git_connections", "git_repos", "git_file",
		"git_merge_requests", "git_merge_request", "git_merge_request_changes",
		"git_merge_request_notes", "git_create_merge_request_note", "git_approve_merge_request",
		"dns_lookup",
		"list_whiteboards", "get_whiteboard", "create_whiteboard",
		"update_whiteboard", "delete_whiteboard", "draw_on_whiteboard",
		"list_notes", "search_notes", "get_note", "create_note", "update_note", "delete_note",
		"grafana_datasources", "grafana_dashboards", "grafana_dashboard", "grafana_panel_data", "grafana_query",
		"rabbitmq_overview", "rabbitmq_queues", "rabbitmq_queue", "rabbitmq_exchanges", "rabbitmq_topology",
		"list_observability_connections", "service_map",
		"rabbitmq_peek", "rabbitmq_publish",
		"list_graylog_connections", "graylog_streams", "graylog_search", "graylog_dashboards",
	} {
		if !got[want] {
			t.Errorf("missing tool %q", want)
		}
	}
}

// TestUserIdentityPropagates proves the minted JWT carries the session's
// user through the existing JWTAuth middleware — the core permission
// guarantee of the in-process design.
func TestUserIdentityPropagates(t *testing.T) {
	cs := connect(t, echoEngine())
	e := decode(t, call(t, cs, "list_collections", map[string]any{}))
	if e.UserID != testUser {
		t.Errorf("userID = %q, want %q", e.UserID, testUser)
	}
}

func TestCreateCollection(t *testing.T) {
	cs := connect(t, echoEngine())
	e := decode(t, call(t, cs, "create_collection", map[string]any{
		"name":        "Demo",
		"description": "d",
	}))
	if e.Method != "POST" {
		t.Errorf("method = %q, want POST", e.Method)
	}
	if e.Path != "/api/collections" {
		t.Errorf("path = %q, want /api/collections", e.Path)
	}
	if !strings.Contains(e.Body, `"name":"Demo"`) {
		t.Errorf("body missing name: %q", e.Body)
	}
}

func TestListCollectionsWorkspaceQuery(t *testing.T) {
	cs := connect(t, echoEngine())
	e := decode(t, call(t, cs, "list_collections", map[string]any{"workspaceId": "ws1"}))
	if e.Method != "GET" {
		t.Errorf("method = %q, want GET", e.Method)
	}
	if e.Query != "workspaceId=ws1" {
		t.Errorf("query = %q, want workspaceId=ws1", e.Query)
	}
}

func TestUpdateCollectionPath(t *testing.T) {
	cs := connect(t, echoEngine())
	e := decode(t, call(t, cs, "update_collection", map[string]any{"id": "abc", "name": "x"}))
	if e.Method != "PUT" || e.Path != "/api/collections/abc" {
		t.Errorf("got %s %s, want PUT /api/collections/abc", e.Method, e.Path)
	}
}

func TestDeleteCollectionPath(t *testing.T) {
	cs := connect(t, echoEngine())
	e := decode(t, call(t, cs, "delete_collection", map[string]any{"id": "abc"}))
	if e.Method != "DELETE" || e.Path != "/api/collections/abc" {
		t.Errorf("got %s %s, want DELETE /api/collections/abc", e.Method, e.Path)
	}
}

func TestCreateRequestBodyPassthrough(t *testing.T) {
	cs := connect(t, echoEngine())
	e := decode(t, call(t, cs, "create_request", map[string]any{
		"collectionId": "c1",
		"name":         "Get Users",
		"method":       "GET",
		"url":          "https://x/y",
	}))
	if e.Method != "POST" || e.Path != "/api/requests" {
		t.Errorf("got %s %s, want POST /api/requests", e.Method, e.Path)
	}
	if !strings.Contains(e.Body, `"collectionId":"c1"`) {
		t.Errorf("body missing collectionId: %q", e.Body)
	}
}

// TestNestedMockRoutePath checks server-id path interpolation.
func TestNestedMockRoutePath(t *testing.T) {
	cs := connect(t, echoEngine())
	e := decode(t, call(t, cs, "create_mock_route", map[string]any{
		"serverId": "srv1", "method": "GET", "path": "/ping",
	}))
	if e.Method != "POST" || e.Path != "/api/mock-servers/srv1/routes" {
		t.Errorf("got %s %s, want POST /api/mock-servers/srv1/routes", e.Method, e.Path)
	}
}

// TestRedisScanPatternMapsToMatch asserts the pattern input reaches the
// backend as the ?match= query param the handler actually reads.
func TestRedisScanPatternMapsToMatch(t *testing.T) {
	cs := connect(t, echoEngine())
	e := decode(t, call(t, cs, "redis_scan_keys", map[string]any{"id": "r1", "pattern": "user:*"}))
	if e.Method != "GET" || e.Path != "/api/redis/r1/keys" {
		t.Errorf("got %s %s, want GET /api/redis/r1/keys", e.Method, e.Path)
	}
	if !strings.Contains(e.Query, "match=user") {
		t.Errorf("query %q should carry match=user:*", e.Query)
	}
}

// TestKafkaConsumeDefaultsAllPartitions asserts an omitted partition is sent
// as -1 (all partitions), matching the UI convention.
func TestKafkaConsumeDefaultsAllPartitions(t *testing.T) {
	cs := connect(t, echoEngine())
	e := decode(t, call(t, cs, "kafka_consume", map[string]any{"id": "k1", "topic": "orders"}))
	if e.Method != "POST" || e.Path != "/api/kafka/k1/consume" {
		t.Errorf("got %s %s, want POST /api/kafka/k1/consume", e.Method, e.Path)
	}
	if !strings.Contains(e.Body, `"partition":-1`) {
		t.Errorf("body %q should default partition to -1", e.Body)
	}
}

// TestS3GetObjectRanged asserts the object fetch is bounded with a Range so a
// large object can't be pulled whole into memory.
func TestS3GetObjectRanged(t *testing.T) {
	cs := connect(t, echoEngine())
	e := decode(t, call(t, cs, "s3_get_object", map[string]any{"id": "s1", "bucket": "b", "key": "k"}))
	if e.Path != "/api/s3/s1/object" {
		t.Errorf("path = %q, want /api/s3/s1/object", e.Path)
	}
	if !strings.Contains(e.Query, "range=bytes") {
		t.Errorf("query %q should carry a byte range", e.Query)
	}
}

// TestErrorMappingIsError asserts a 4xx backend response becomes an MCP
// tool error carrying the backend body.
func TestErrorMappingIsError(t *testing.T) {
	cs := connect(t, statusEngine(404))
	res := call(t, cs, "list_collections", map[string]any{})
	if !res.IsError {
		t.Error("expected IsError=true for 404 backend response")
	}
	if !strings.Contains(text(t, res), "boom") {
		t.Errorf("error body not surfaced: %q", text(t, res))
	}
}

// TestSchemaValidationRejectsMissingRequired confirms the SDK rejects a
// create_collection with no name before any in-process call happens.
func TestSchemaValidationRejectsMissingRequired(t *testing.T) {
	cs := connect(t, echoEngine())
	res := call(t, cs, "create_collection", map[string]any{})
	if !res.IsError {
		t.Error("expected IsError=true for missing required 'name'")
	}
	if !strings.Contains(strings.ToLower(text(t, res)), "name") {
		t.Errorf("validation message should mention 'name': %q", text(t, res))
	}
}
