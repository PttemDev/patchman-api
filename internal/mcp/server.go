// Package mcp exposes Patchman's existing CRUD surface to external MCP
// clients (Claude, MCP Inspector, …) as Model Context Protocol tools.
//
// Design: each tool does NOT re-implement business logic. It performs an
// in-process HTTP call against the very same *gin.Engine that serves the
// public REST API, authenticated as the token's user via a freshly minted
// short-lived JWT. That means every MCP tool call flows through the exact
// same auth, "act-as", validation and permission stack as a hand-made
// request — so an MCP client can do precisely what the user can do by
// hand, and nothing more. No second permission model to keep in sync.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"time"

	"patchman-be/internal/cloudsession"
	"patchman-be/internal/mcpscope"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// caller performs in-process REST calls as a fixed user.
type caller struct {
	engine    *gin.Engine
	jwtSecret string
	userID    string
	agent     string // the connected agent's name ("" when unknown)
	project   string // the project it works in (its working directory's name; "" when unknown)
}

// mintJWT issues a short-lived HS256 token for the in-process call. Mirrors
// the claim shape produced by handlers.AuthHandler.generateToken so the
// existing JWTAuth middleware accepts it unchanged.
func (c *caller) mintJWT() string {
	claims := jwt.MapClaims{
		"user_id": c.userID,
		"exp":     time.Now().Add(5 * time.Minute).Unix(),
		"iat":     time.Now().Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte(c.jwtSecret))
	if err != nil {
		// Misconfigured JWT secret turns every tool call into a silent
		// 401 from JWTAuth — log it so the cause is visible in server
		// logs instead of leaving the operator guessing.
		log.Printf("mcp: failed to sign in-process JWT: %v", err)
		return ""
	}
	return signed
}

// call runs method+path (with optional JSON body) through the gin engine
// and returns the status code and raw response body. It is the
// context-less wrapper used by the light management/list tools; execution
// tools use callCtx so client cancellation reaches the upstream op.
func (c *caller) call(method, path string, body any) (int, string) {
	return c.callCtx(context.Background(), method, path, body)
}

// callCtx is call but threads the tool handler's context.Context onto the
// in-process request. Gin exposes it as c.Request.Context(), so an aborted
// or timed-out MCP call cancels the long-running upstream work it drives
// (a ~60s DB query, a Kafka consume, an ES/S3/Redis round-trip) instead of
// leaking it. Used by the execution tools.
func (c *caller) callCtx(ctx context.Context, method, path string, body any) (int, string) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.mintJWT())
	req.Header.Set(clientHeader, clientHeaderMCP)
	if c.agent != "" {
		req.Header.Set(AgentHeader, c.agent)
	}
	if c.project != "" {
		req.Header.Set(ProjectHeader, c.project)
	}
	rec := httptest.NewRecorder()
	c.engine.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// clientHeader marks requests made by MCP tools so handlers can attribute the
// write (e.g. a note revision shows "AI agent" instead of a human edit).
// Must match handlers.mcpClientHeader.
const (
	clientHeader    = "X-PatchMan-Client"
	clientHeaderMCP = "mcp"
)

// cloudOrLocal routes a call to the hosted BE (as the signed-in user) when this
// embedded backend holds a live cloud session, else runs it in-process as the
// local user. Used by:
//   - the whiteboard tools, so an AI on the desktop's local MCP acts on the
//     account the user is actually looking at (not the invisible on-device
//     store) — see WHITEBOARD_UNIFIED_LOCAL_RFC.md;
//   - the git tools, because GitLab auth is the CALLER's linked GitLab account,
//     which lives on the hosted BE (the FE pins git remote reads to 'remote'
//     for the same reason); locally the embedded local-user has no link → 412.
//
// Every other tool stays local by design — executions run against
// dual-written / LAN connections and must not go to the cloud. The cloud
// session only ever exists on the embedded BE, so on the hosted server this
// is always the in-process path.
func (c *caller) cloudOrLocal(method, path string, body any) (int, string) {
	return c.cloudOrLocalCtx(context.Background(), method, path, body)
}

// cloudOrLocalCtx is cloudOrLocal carrying the tool call's context, so an
// aborted MCP call cancels the cloud round-trip too.
func (c *caller) cloudOrLocalCtx(ctx context.Context, method, path string, body any) (int, string) {
	if sess, ok := cloudsession.Get(); ok {
		return callCloud(ctx, sess, method, path, body, c.agent, c.project)
	}
	return c.callCtx(ctx, method, path, body)
}

// callCloud performs a real HTTPS call to the hosted BE with the user's cloud
// token. A transport error or non-2xx falls through to result() as an MCP tool
// error carrying the backend's message — the model sees the real reason (e.g. a
// 401 from an expired token) rather than a silent success.
func callCloud(ctx context.Context, sess cloudsession.Session, method, path string, body any, agent, project string) (int, string) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, sess.BaseURL+path, rdr)
	if err != nil {
		return http.StatusInternalServerError, fmt.Sprintf(`{"error":%q}`, err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sess.Token)
	req.Header.Set(clientHeader, clientHeaderMCP)
	if agent != "" {
		req.Header.Set(AgentHeader, agent)
	}
	if project != "" {
		req.Header.Set(ProjectHeader, project)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return http.StatusBadGateway, fmt.Sprintf(`{"error":%q}`, err.Error())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	// An expired/invalid cloud token can't be refreshed from here (only FE
	// traffic mints a fresh one). Drop the dead session so the next tool call
	// routes locally instead of 401-looping, and tell the model why — the next
	// FE request re-posts a fresh session.
	if resp.StatusCode == http.StatusUnauthorized {
		cloudsession.Clear()
		return http.StatusUnauthorized, `{"error":"cloud session expired — open PatchMan to refresh, then retry"}`
	}
	return resp.StatusCode, string(b)
}

// result packs a REST response into an MCP tool result. A non-2xx status
// becomes an MCP tool error (IsError) carrying the backend's JSON body so
// the model sees the real validation/permission message.
func result(code int, body string) (*mcpsdk.CallToolResult, any, error) {
	return &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: body}},
		IsError: code >= 400,
	}, nil, nil
}

// reg is a tiny helper to register a typed tool.
func reg[In any](s *mcpsdk.Server, name, desc string, h func(In) (int, string)) {
	mcpsdk.AddTool(s, &mcpsdk.Tool{Name: name, Description: desc},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, in In) (*mcpsdk.CallToolResult, any, error) {
			code, body := h(in)
			return result(code, body)
		})
}

// regCtx is reg for tools that must honor client cancellation: its handler
// receives the request's context.Context to thread into callCtx.
func regCtx[In any](s *mcpsdk.Server, name, desc string, h func(context.Context, In) (int, string)) {
	mcpsdk.AddTool(s, &mcpsdk.Tool{Name: name, Description: desc},
		func(ctx context.Context, _ *mcpsdk.CallToolRequest, in In) (*mcpsdk.CallToolResult, any, error) {
			code, body := h(ctx, in)
			return result(code, body)
		})
}

// BuildServer constructs an MCP server whose tools act as userID by
// calling the in-process gin engine. A fresh server is built per MCP
// request (cheap) so the user identity AND the token's scopes are captured
// in the closures — two tokens under the same user can carry different scopes.
//
// `scopes` gates which tool GROUPS get registered. An unregistered tool is
// absent from tools/list AND rejected on tools/call by the SDK, so this is the
// actual enforcement point, not just a display filter. scopes.All (a token with
// NULL scopes) registers everything — the default.
func BuildServer(engine *gin.Engine, jwtSecret, userID string, scopes mcpscope.ToolScopes) *mcpsdk.Server {
	return BuildServerAs(engine, jwtSecret, userID, scopes, "", "")
}

// BuildServerAs is BuildServer for a named agent (AgentName of the client
// that connected with the token): its writes carry that name. project is the
// project the agent works in, when its bridge said ("" otherwise): agent
// memory groups and ranks cards by it.
func BuildServerAs(engine *gin.Engine, jwtSecret, userID string, scopes mcpscope.ToolScopes, agent, project string) *mcpsdk.Server {
	var opts *mcpsdk.ServerOptions
	if scopes.Allows(mcpscope.GroupNotes) {
		opts = &mcpsdk.ServerOptions{Instructions: memoryInstructions}
	}
	s := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "patchman",
		Title:   "Patchman",
		Version: "0.1.0",
	}, opts)
	c := &caller{engine: engine, jwtSecret: jwtSecret, userID: userID, agent: agent, project: project}

	if scopes.Allows(mcpscope.GroupCollections) {
		registerCollectionTools(s, c)
	}
	if scopes.Allows(mcpscope.GroupRequests) {
		registerRequestTools(s, c)
	}
	if scopes.Allows(mcpscope.GroupEnvironments) {
		registerEnvironmentTools(s, c)
	}
	if scopes.Allows(mcpscope.GroupTestCases) {
		registerTestCaseTools(s, c)
	}
	if scopes.Allows(mcpscope.GroupMocks) {
		registerMockTools(s, c)
	}

	// Read-first CONNECTION/EXECUTION surface (discovery + drivers).
	if scopes.Allows(mcpscope.GroupConnections) {
		registerConnectionDiscoveryTools(s, c)
	}
	if scopes.Allows(mcpscope.GroupHTTP) {
		registerHTTPExecTools(s, c)
	}
	if scopes.Allows(mcpscope.GroupDB) {
		registerDBExecTools(s, c)
	}
	if scopes.Allows(mcpscope.GroupES) {
		registerESTools(s, c)
	}
	if scopes.Allows(mcpscope.GroupRedis) {
		registerRedisTools(s, c)
	}
	if scopes.Allows(mcpscope.GroupKafka) {
		registerKafkaTools(s, c)
	}
	if scopes.Allows(mcpscope.GroupS3) {
		registerS3Tools(s, c)
	}
	if scopes.Allows(mcpscope.GroupK8s) {
		registerK8sTools(s, c)
	}
	if scopes.Allows(mcpscope.GroupGit) {
		registerGitTools(s, c)
	}
	if scopes.Allows(mcpscope.GroupDevTools) {
		registerDevToolsTools(s, c)
	}
	if scopes.Allows(mcpscope.GroupWhiteboards) {
		registerWhiteboardTools(s, c)
	}
	if scopes.Allows(mcpscope.GroupNotes) {
		registerNotesTools(s, c)
	}
	if scopes.Allows(mcpscope.GroupGrafana) {
		registerGrafanaTools(s, c)
	}
	if scopes.Allows(mcpscope.GroupRabbitMQ) {
		registerRabbitTools(s, c)
	}
	if scopes.Allows(mcpscope.GroupObservability) {
		registerServiceMapTools(s, c)
	}
	if scopes.Allows(mcpscope.GroupGraylog) {
		registerGraylogTools(s, c)
	}

	return s
}
