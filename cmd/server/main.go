package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"patchman-be/internal/apicontract"
	"patchman-be/internal/apm"
	"patchman-be/internal/cleanup"
	"patchman-be/internal/config"
	"patchman-be/internal/database"
	"patchman-be/internal/dbquery"
	"patchman-be/internal/errorreport"
	"patchman-be/internal/esquery"
	"patchman-be/internal/forwarder"
	"patchman-be/internal/grafanaquery"
	"patchman-be/internal/graylogquery"
	"patchman-be/internal/handlers"
	"patchman-be/internal/k8sclient"
	"patchman-be/internal/kafkaquery"
	"patchman-be/internal/rabbitquery"
	patchmcp "patchman-be/internal/mcp"
	"patchman-be/internal/mcpdisco"
	"patchman-be/internal/mcpscope"
	"patchman-be/internal/middleware"
	"patchman-be/internal/models"
	"patchman-be/internal/realtime"
	"patchman-be/internal/redisquery"
	"patchman-be/internal/s3client"
	"patchman-be/internal/services"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"gorm.io/gorm"
)

// buildVersion is stamped via -ldflags "-X main.buildVersion=<v>" at build
// time (fe/scripts/build-sidecar.mjs reads it from tauri.conf.json). It is
// written into the discovery file for diagnostics only.
var buildVersion = "dev"

// mcpUserCtxKey carries the authenticated MCP user id from the transport
// auth shim into the MCP server factory.
type mcpUserCtxKey struct{}

// mcpScopesCtxKey carries the token's persisted scope list (nil = all tool
// groups) alongside the user id, so the factory can restrict the tool surface.
type mcpScopesCtxKey struct{}

// mcpAgentCtxKey carries the connected agent's name (see mcpInitializeClient).
type mcpAgentCtxKey struct{}

// mcpProjectCtxKey carries the project the agent works in (bridge header).
type mcpProjectCtxKey struct{}

// mcpInitializeClient reads the client's name from an MCP initialize request
// ({"method":"initialize","params":{"clientInfo":{…}}}) without consuming the
// body the MCP handler still has to read.
func mcpInitializeClient(c *gin.Context) (string, bool) {
	if c.Request.Method != http.MethodPost || c.Request.Body == nil {
		return "", false
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil || !bytes.Contains(raw, []byte(`"initialize"`)) {
		return "", false
	}
	var msg struct {
		Method string `json:"method"`
		Params struct {
			ClientInfo struct {
				Name  string `json:"name"`
				Title string `json:"title"`
			} `json:"clientInfo"`
		} `json:"params"`
	}
	if json.Unmarshal(raw, &msg) != nil || msg.Method != "initialize" {
		return "", false
	}
	name := patchmcp.AgentName(msg.Params.ClientInfo.Name, msg.Params.ClientInfo.Title)
	return name, name != ""
}

// parseBearer pulls the token out of an "Authorization: Bearer <token>"
// header, returning "" when absent or malformed.
func parseBearer(h string) string {
	parts := strings.SplitN(h, " ", 2)
	if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
		return strings.TrimSpace(parts[1])
	}
	return ""
}

func main() {
	cfg := config.Load()

	db, err := database.Init(cfg.DBDriver, cfg.DSN(), cfg.DefaultAdminEmail)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}

	// Local MCP discovery (embedded desktop only). localToken is the stable
	// pmcp_ plaintext written into the discovery file so the stdio bridge can
	// authenticate; bridgePath is the staged ~/.patchman/bin/patchman-mcp the
	// FE surfaces in the Claude Desktop config snippet. Both stay empty on the
	// hosted server, and both provisioning steps are non-fatal so a failure
	// never blocks boot.
	var localToken string
	var bridgePath string
	if cfg.IsEmbedded() {
		if err := ensureLocalUser(db); err != nil {
			log.Fatalf("Failed to bootstrap local user: %v", err)
		}
		if t, err := ensureLocalMCPToken(db, cfg.DBPath); err != nil {
			log.Printf("mcp: could not provision local token: %v", err)
		} else {
			localToken = t
		}
		if p, err := mcpdisco.EnsureStableBridge(); err != nil {
			log.Printf("mcp: could not stage local bridge binary: %v", err)
		} else {
			bridgePath = p
		}
	}

	// Upstream forwarder: only active in embedded mode when both
	// --forward-target and --forward-token are provided. On the remote BE
	// (non-embedded) this stays a no-op so the handlers below can be wired
	// unconditionally without a mode check.
	fwd := forwarder.New(cfg.ForwardTarget, cfg.ForwardToken)
	if cfg.ForwardEnabled() {
		log.Printf("Upstream forwarding enabled → %s", cfg.ForwardTarget)
	}

	// Background cleanup: purge high-growth table rows older than 30 days.
	// In embedded mode this is still useful — keeps the SQLite file trim.
	cleanup.Start(db, 30*24*time.Hour)

	// Realtime (WebSocket live sync). Init the hub first so the handler
	// can register clients; start the Postgres LISTEN goroutine only in
	// hosted (multi-pod) mode. In embedded SQLite there's exactly one
	// process, so Notify broadcasts locally without a NOTIFY round-trip.
	realtime.Init()
	if cfg.DBDriver == database.DriverPostgres {
		realtime.StartListener(cfg.DSN())
	}

	// Crash-report → Slack reporter. Constructed once at boot; an empty
	// webhook URL turns it into a silent noop so the FE still gets a
	// clean 204 from /api/errors/report.
	errReporter := errorreport.New(cfg.SlackErrorWebhookURL)
	if cfg.SlackErrorWebhookURL == "" {
		log.Printf("Crash reporting disabled: set SLACK_ERROR_WEBHOOK_URL to enable")
	}

	// Desktop sidecar: no gin debug output. Its route listing alone is ~65 KB
	// written to stdout before the port is bound, and the desktop shell reads
	// that pipe — 1.58.0 crossed the 64 KiB pipe buffer and hung boot. The
	// shell now drains the pipe too; this keeps the pre-listen output small.
	if cfg.IsEmbedded() {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.Default()

	// CORS
	r.Use(cors.New(cors.Config{
		AllowAllOrigins: true,
		AllowMethods:    []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"},
		// X-Act-As-User is required for the admin "Manage as user" mode —
		// without it on this allowlist the browser's CORS preflight
		// rejects the actual request and the FE sees a status=0 network
		// error on every impersonated call. Local dev didn't catch this
		// because Vite proxies /api as same-origin and skips CORS.
		AllowHeaders: []string{"Origin", "Content-Type", "Accept", "Authorization", "X-Requested-With", "X-Act-As-User", "X-Client-Id", "X-Client-Version"},
		// X-Refreshed-Token lets the FE read the slid-forward session token
		// off any response (sliding-session refresh); without exposing it,
		// browser JS can't see the header cross-origin.
		ExposeHeaders:    []string{"Content-Length", "Content-Type", middleware.RefreshedTokenHeader},
		AllowCredentials: false,
		MaxAge:           86400,
	}))

	// Health probes (no auth)
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})
	r.GET("/readyz", func(c *gin.Context) {
		sqlDB, err := db.DB()
		if err != nil {
			c.JSON(503, gin.H{"status": "error", "reason": "db connection lost"})
			return
		}
		if err := sqlDB.Ping(); err != nil {
			c.JSON(503, gin.H{"status": "error", "reason": "db ping failed"})
			return
		}
		c.JSON(200, gin.H{"status": "ok"})
	})

	// Pick auth middleware. In embedded mode we bypass JWT entirely and pin
	// all writes to the synthetic local user.
	authMW := middleware.JWTAuth(cfg.JWTSecret, cfg.SessionTTL)
	if cfg.IsEmbedded() {
		authMW = middleware.LocalUserAuth()
	}

	// Auth handlers (no middleware)
	authHandler := handlers.NewAuthHandler(db, cfg, cfg.JWTSecret)
	authGroup := r.Group("/api/auth")
	{
		authGroup.POST("/register", authHandler.Register)
		authGroup.POST("/login", authHandler.Login)
		authGroup.GET("/me", authMW, authHandler.Me)
		// SSO sign-in (OAuth-via-third-party). Public — these endpoints
		// run BEFORE the user has a JWT.
		ssoH := handlers.NewSSOHandler(db, cfg, cfg.JWTSecret)
		authGroup.POST("/sso/start", ssoH.Start)
		authGroup.POST("/sso/callback", ssoH.Callback)
	}

	// Public proxy routes (no auth) — used by anonymous/offline clients to bypass CORS
	publicProxyHandler := handlers.NewProxyHandler(db).WithForwarder(fwd)
	publicCORSProxyHandler := handlers.NewCORSProxyHandler()
	r.POST("/api/public/proxy/send", publicProxyHandler.Send)
	r.POST("/api/public/cors-proxy", publicCORSProxyHandler.Proxy)

	// No-account whiteboard links (no auth) — anyone with the token can view,
	// and edit if the link's role is 'editor'. Gated entirely by the token.
	wbShareH := handlers.NewWhiteboardSharingHandler(db)
	r.GET("/api/public/whiteboards/:token", wbShareH.PublicGet)
	r.PATCH("/api/public/whiteboards/:token", wbShareH.PublicUpdate)
	// A folder link opens the boards inside it (any depth) by id.
	r.GET("/api/public/whiteboards/:token/boards/:boardId", wbShareH.PublicGetBoard)
	r.PATCH("/api/public/whiteboards/:token/boards/:boardId", wbShareH.PublicUpdateBoard)

	// Real-time collaboration WS (mounted WITHOUT authMW: it authenticates per
	// connection via ?token=<jwt> or ?link=<share token>, so anonymous editor
	// links can collaborate). Resource-scoped rooms, not the user-keyed hub.
	wbCollabH := handlers.NewWhiteboardCollabHandler(db, cfg.JWTSecret, cfg.IsEmbedded(), cfg.RealtimeAllowedOrigins)
	r.GET("/api/whiteboards/:id/collab", wbCollabH.Connect)

	// Admin endpoints — registered first so the impersonation middleware
	// added below to /api can't accidentally swap the userID before
	// RequireAdmin gets to look at it.
	errReportH := handlers.NewErrorReportHandler(db, errReporter)
	if !cfg.IsEmbedded() {
		adminGroup := r.Group("/api/admin")
		adminGroup.Use(authMW)
		adminGroup.Use(middleware.RequireAdmin(db))
		adminH := handlers.NewAdminHandler(db, cfg)
		adminGroup.GET("/users", adminH.ListUsers)
		// Per-user usage (per-tool counts + last use), fetched when the
		// admin expands a user's row. Metadata only, no content.
		adminGroup.GET("/users/:id/usage", adminH.UserUsage)
		adminGroup.PUT("/users/:id/role", adminH.SetRole)
		adminGroup.PUT("/users/:id/password", adminH.ResetPassword)
		adminGroup.GET("/stats", adminH.Stats)
		// Collection ownership transfer (RFC Q8 — used when a user leaves
		// and someone else needs to take over their public/shared rows).
		adminGroup.PUT("/collections/:id/owner", adminH.ReassignCollection)
		// Crash-report admin surface: read/write toggles + send a test
		// payload to the configured Slack webhook.
		adminGroup.GET("/error-reporting/config", errReportH.GetConfig)
		adminGroup.PATCH("/error-reporting/config", errReportH.PatchConfig)
		adminGroup.POST("/error-reporting/test", errReportH.Test)
	}

	// Protected routes
	api := r.Group("/api")
	api.Use(authMW)
	// AuditLog writes are noisy and pointless for a single local user. Skip
	// them in embedded mode to keep the SQLite file small.
	if !cfg.IsEmbedded() {
		api.Use(middleware.AuditLog(db))
	}
	// "Manage as user" mode — admins can swap their effective userID via
	// the X-Act-As-User header so the regular CRUD endpoints operate on
	// a target user's data without dual handlers. Skip in embedded mode
	// where there's only one user.
	if !cfg.IsEmbedded() {
		api.Use(middleware.ActAsUser(db))
	}

	// User search
	api.GET("/users/search", authHandler.SearchUsers)

	// Excalidraw whiteboards — per-user scene CRUD (no secrets, no cipher).
	wbH := handlers.NewWhiteboardHandler(db)
	api.GET("/whiteboards", wbH.List)
	api.GET("/whiteboards/shared", wbH.ListShared)
	api.POST("/whiteboards", wbH.Create)
	api.GET("/whiteboards/:id", wbH.Get)
	api.PATCH("/whiteboards/:id", wbH.Update)
	api.DELETE("/whiteboards/:id", wbH.Delete)
	// Queued AI/MCP diagrams: draw_on_whiteboard enqueues, the open FE drains.
	api.POST("/whiteboards/:id/diagrams", wbH.EnqueueDiagram)
	api.POST("/whiteboards/:id/diagrams/drain", wbH.DrainDiagrams)
	// Sharing: visibility + user/team/everyone grants + no-account links.
	api.PATCH("/whiteboards/:id/visibility", wbShareH.SetVisibility)
	api.GET("/whiteboards/:id/grants", wbShareH.ListGrants)
	api.POST("/whiteboards/:id/grants", wbShareH.AddGrant)
	api.DELETE("/whiteboards/:id/grants/:grantId", wbShareH.DeleteGrant)
	api.GET("/whiteboards/:id/links", wbShareH.ListLinks)
	api.POST("/whiteboards/:id/links", wbShareH.CreateLink)
	api.DELETE("/whiteboards/:id/links/:linkId", wbShareH.RevokeLink)

	// Notes knowledge base — markdown pages in a shareable tree, history,
	// images, full-text search. See notes plan (NOTES).
	noteH := handlers.NewNoteHandler(db)
	noteH.StartCardWorkers()
	noteShareH := handlers.NewNoteSharingHandler(db)
	api.GET("/notes", noteH.List)
	api.GET("/notes/shared", noteH.ListShared)
	api.GET("/notes/search", noteH.Search)
	api.POST("/notes", noteH.Create)
	api.GET("/notes/attachments/:attId", noteH.GetAttachment)
	api.GET("/notes/:id", noteH.Get)
	api.PATCH("/notes/:id", noteH.Update)
	api.DELETE("/notes/:id", noteH.Delete)
	api.POST("/notes/:id/append", noteH.Append)
	api.GET("/notes/:id/revisions", noteH.Revisions)
	api.GET("/notes/:id/revisions/:revId", noteH.Revision)
	api.POST("/notes/:id/revisions/:revId/restore", noteH.Restore)
	api.GET("/notes/:id/backlinks", noteH.Backlinks)
	// Claude Memory: knowledge cards, typed links, recall / remember.
	api.GET("/notes/recall", noteH.Recall)
	api.POST("/notes/remember", noteH.Remember)
	api.GET("/notes/inbox/:id", noteH.InboxStatus)
	api.GET("/notes/links/by-uri", noteH.LinksByURI)
	api.GET("/notes/memory", noteH.MemoryOverview)
	api.GET("/notes/memory/graph", noteH.MemoryGraph)
	api.PUT("/notes/memory/settings", noteH.UpdateMemorySettings)
	api.GET("/notes/suggestions", noteH.Suggestions)
	api.POST("/notes/suggestions/:id/accept", noteH.AcceptSuggestion)
	api.POST("/notes/suggestions/:id/reject", noteH.RejectSuggestion)
	api.GET("/notes/:id/links", noteH.Links)
	api.POST("/notes/:id/links", noteH.AddLink)
	api.DELETE("/notes/:id/links", noteH.DeleteLink)
	api.POST("/notes/:id/attachments", noteH.UploadAttachment)
	api.PATCH("/notes/:id/visibility", noteShareH.SetVisibility)
	api.GET("/notes/:id/grants", noteShareH.ListGrants)
	api.POST("/notes/:id/grants", noteShareH.AddGrant)
	api.DELETE("/notes/:id/grants/:grantId", noteShareH.DeleteGrant)

	// Crash report ingest. Always returns 204 — the FE doesn't need to
	// distinguish "Slack accepted" from "toggle off" from "deduped".
	api.POST("/errors/report", errReportH.Report)

	// Access debug — surfaces the resolver's intermediate hits for the
	// signed-in user against a given collection/environment. Lets us
	// trace unexpected 403s without poking the DB by hand.
	accessDebugH := handlers.NewAccessDebugHandler(db)
	api.GET("/debug/role/collection/:id", accessDebugH.Collection)
	api.GET("/debug/role/environment/:id", accessDebugH.Environment)

	// Collections
	collectionHandler := handlers.NewCollectionHandler(db)
	api.GET("/collections", collectionHandler.List)
	api.POST("/collections", collectionHandler.Create)
	api.PUT("/collections/:id", collectionHandler.Update)
	api.DELETE("/collections/:id", collectionHandler.Delete)
	api.GET("/collections/:id/requests", collectionHandler.ListRequests)
	api.GET("/collections/:id/export", collectionHandler.Export)
	api.POST("/collections/import", collectionHandler.Import)
	api.POST("/collections/:id/sync", collectionHandler.Sync)

	// Import (Swagger/OpenAPI)
	importHandler := handlers.NewImportHandler(db)
	api.POST("/import/swagger", importHandler.ImportSwagger)
	// Import (WSDL / SOAP) — creates one SavedRequest per WSDL operation
	// with a shallow SOAP envelope skeleton. Mirrors the swagger importer.
	api.POST("/import/wsdl", importHandler.ImportWSDL)
	// Resolve a docs URL (Scalar / Swagger UI / Redoc / direct JSON) to
	// the underlying spec. Server-side because the browser would hit CORS
	// reading most third-party docs pages.
	specResolver := handlers.NewSpecResolverHandler()
	api.POST("/import/resolve-spec", specResolver.Resolve)

	// Requests
	requestHandler := handlers.NewRequestHandler(db)
	api.POST("/requests", requestHandler.Create)
	api.GET("/requests/:id", requestHandler.Get)
	api.PUT("/requests/:id", requestHandler.Update)
	api.DELETE("/requests/:id", requestHandler.Delete)

	// Proxy
	proxyHandler := handlers.NewProxyHandler(db).WithForwarder(fwd)
	api.POST("/proxy/send", proxyHandler.Send)

	// Environments
	envHandler := handlers.NewEnvironmentHandler(db)
	api.GET("/environments", envHandler.List)
	api.POST("/environments", envHandler.Create)
	api.PUT("/environments/:id", envHandler.Update)
	api.DELETE("/environments/:id", envHandler.Delete)
	api.PUT("/environments/:id/activate", envHandler.Activate)
	api.POST("/environments/import", envHandler.Import)
	api.GET("/environments/:id/export", envHandler.Export)
	// Bulk multi-format export — supersedes the single-id GET above
	// for new clients. Accepts ids or workspaceId scope, format, and
	// secrets policy in the body.
	api.POST("/environments/export", envHandler.BulkExport)
	// Sharing — mirrors the collection-share workflow.
	// Legacy env share-code endpoints (CreateShare / JoinByToken /
	// ListCollaborators / RemoveCollaborator / LeaveShare / RevokeShare)
	// removed in 1.16 — env sharing is now public visibility + grants
	// only. Existing collaborator rows were migrated to public grants
	// at boot (see database.migrateEnvironmentSharesToPublicGrants);
	// the handler code lives on for now in case we need to restore the
	// surface, but no routes are registered. New share flow lives on
	// /api/environments/:id/visibility + /api/environments/:id/grants.
	// Per-variable secret reveal — Viewer-side opt-in for masked
	// values. Editors/Owners get raw values through List anyway.
	api.GET("/environments/:id/variables/:key/reveal", envHandler.RevealSecret)

	// History
	historyHandler := handlers.NewHistoryHandler(db)
	api.GET("/history", historyHandler.List)
	api.DELETE("/history/:id", historyHandler.Delete)
	api.DELETE("/history", historyHandler.ClearAll)

	// Saved responses — request/response pairs kept under a collection.
	savedRespH := handlers.NewSavedResponseHandler(db)
	api.GET("/saved-responses", savedRespH.List)
	api.POST("/saved-responses", savedRespH.Create)
	api.GET("/saved-responses/:id", savedRespH.Get)
	api.PATCH("/saved-responses/:id", savedRespH.Rename)
	api.DELETE("/saved-responses/:id", savedRespH.Delete)

	// Code Generation
	codegenHandler := handlers.NewCodegenHandler()
	api.POST("/codegen", codegenHandler.Generate)

	// DNS Toolkit (Dev Tools) — stateless stdlib resolver front-end. No DB,
	// no cipher; registered with the plain utility handlers so it works on
	// every BE (embedded + remote) regardless of cipher configuration.
	dnsHandler := handlers.NewDNSHandler()
	api.POST("/devtools/dns", dnsHandler.Lookup)

	// Web Audit (Dev Tools) — browser-less SEO / web-quality checker. Fetches a
	// URL, parses the HTML, best-effort probes robots.txt + sitemap.xml, and
	// returns a categorized issue checklist. Stateless like the DNS handler.
	webAuditHandler := handlers.NewWebAuditHandler()
	api.POST("/devtools/web-audit", webAuditHandler.Audit)

	// Mock Servers
	mockHandler := handlers.NewMockHandler(db)
	api.GET("/mock-servers", mockHandler.ListServers)
	api.POST("/mock-servers", mockHandler.CreateServer)
	api.PUT("/mock-servers/:id", mockHandler.UpdateServer)
	api.DELETE("/mock-servers/:id", mockHandler.DeleteServer)
	api.GET("/mock-servers/:id/routes", mockHandler.ListRoutes)
	api.POST("/mock-servers/:id/routes", mockHandler.CreateRoute)
	api.PUT("/mock-routes/:id", mockHandler.UpdateRoute)
	api.DELETE("/mock-routes/:id", mockHandler.DeleteRoute)

	// Test Cases
	testCaseHandler := handlers.NewTestCaseHandler(db)
	api.GET("/test-cases", testCaseHandler.List)
	api.POST("/test-cases", testCaseHandler.Create)
	api.GET("/test-cases/:id", testCaseHandler.Get)
	api.PUT("/test-cases/:id", testCaseHandler.Update)
	api.DELETE("/test-cases/:id", testCaseHandler.Delete)

	// Test-case folders (owner-only, like test cases) + move + "Run all".
	testFolderHandler := handlers.NewTestCaseFolderHandler(db)
	api.GET("/test-folders", testFolderHandler.List)
	api.POST("/test-folders", testFolderHandler.Create)
	api.PUT("/test-folders/:id", testFolderHandler.Rename)
	api.DELETE("/test-folders/:id", testFolderHandler.Delete)
	api.POST("/test-folders/:id/execute", testFolderHandler.Execute)
	api.PUT("/test-cases/:id/move", testFolderHandler.MoveTestCase)

	// Test Executor (execute test cases and plans)
	testExecutor := handlers.NewTestExecutorHandler(db).WithForwarder(fwd)
	api.POST("/test-cases/:id/execute", testExecutor.ExecuteTestCase)
	api.POST("/test-plans/:id/execute", testExecutor.ExecuteTestPlan)

	// Test Plans
	testPlanHandler := handlers.NewTestPlanHandler(db)
	api.GET("/test-plans", testPlanHandler.List)
	api.POST("/test-plans", testPlanHandler.Create)
	api.GET("/test-plans/:id", testPlanHandler.Get)
	api.PUT("/test-plans/:id", testPlanHandler.Update)
	api.DELETE("/test-plans/:id", testPlanHandler.Delete)

	// Test Runs
	testRunHandler := handlers.NewTestRunHandler(db).WithForwarder(fwd)
	api.POST("/test-plans/:id/runs", testRunHandler.Create)
	api.GET("/test-plans/:id/runs", testRunHandler.ListByPlan)
	api.GET("/test-runs", testRunHandler.ListAll)
	api.GET("/test-runs/:id", testRunHandler.Get)
	api.PUT("/test-runs/:id", testRunHandler.Update)
	api.DELETE("/test-runs/:id", testRunHandler.Delete)

	// Workspaces — now owner-only (no team-member roster). Sharing has
	// moved to the dedicated Teams entity + public-grant tables on
	// individual collections / environments. See
	// TEAMS_AND_PUBLIC_SHARING_RFC.md in the repo root.
	workspaceHandler := handlers.NewWorkspaceHandler(db)
	api.GET("/workspaces", workspaceHandler.ListWorkspaces)
	api.POST("/workspaces", workspaceHandler.CreateWorkspace)
	api.GET("/workspaces/:id", workspaceHandler.GetWorkspace)
	api.PUT("/workspaces/:id", workspaceHandler.UpdateWorkspace)
	api.DELETE("/workspaces/:id", workspaceHandler.DeleteWorkspace)
	// Bind / unbind workspace to a team — owner-only, makes the
	// workspace visible in every team member's selector.
	api.PATCH("/workspaces/:id/team", workspaceHandler.SetTeam)

	// Teams — asset-free user groups referenced from public-grant rows.
	// Every authenticated user may create, list, and inspect any team;
	// admin-only operations (rename, add member, change role, delete)
	// are guarded inside the handler against the caller's TeamMember row.
	teamHandler := handlers.NewTeamHandler(db)
	api.GET("/teams", teamHandler.ListTeams)
	api.POST("/teams", teamHandler.CreateTeam)
	api.GET("/teams/:id", teamHandler.GetTeam)
	api.PATCH("/teams/:id", teamHandler.UpdateTeam)
	api.DELETE("/teams/:id", teamHandler.DeleteTeam)
	api.GET("/teams/:id/members", teamHandler.ListMembers)
	api.POST("/teams/:id/members", teamHandler.AddMember)
	api.PATCH("/teams/:id/members/:userId", teamHandler.UpdateMemberRole)
	api.DELETE("/teams/:id/members/:userId", teamHandler.RemoveMember)

	// Public visibility + Editors/Viewers grants for collections.
	// Owner-only mutations (start owner-only per RFC Q4; design allows
	// future editor extension without a schema change).
	collectionSharing := handlers.NewCollectionSharingHandler(db)
	api.PATCH("/collections/:id/visibility", collectionSharing.SetVisibility)
	api.GET("/collections/:id/grants", collectionSharing.ListGrants)
	api.POST("/collections/:id/grants", collectionSharing.AddGrant)
	api.DELETE("/collections/:id/grants/:grantId", collectionSharing.DeleteGrant)

	// Environment parity — same shape, env-side tables/models.
	envSharing := handlers.NewEnvironmentSharingHandler(db)
	api.PATCH("/environments/:id/visibility", envSharing.SetVisibility)
	api.GET("/environments/:id/grants", envSharing.ListGrants)
	api.POST("/environments/:id/grants", envSharing.AddGrant)
	api.DELETE("/environments/:id/grants/:grantId", envSharing.DeleteGrant)

	// Defects (Bug Tracking)
	defectHandler := handlers.NewDefectHandler(db)
	api.GET("/defects", defectHandler.List)
	api.POST("/defects", defectHandler.Create)
	api.GET("/defects/:id", defectHandler.Get)
	api.PUT("/defects/:id", defectHandler.Update)
	api.DELETE("/defects/:id", defectHandler.Delete)

	// Comments
	commentHandler := handlers.NewCommentHandler(db)
	api.GET("/comments", commentHandler.List)
	api.POST("/comments", commentHandler.Create)
	api.PUT("/comments/:id", commentHandler.Update)
	api.DELETE("/comments/:id", commentHandler.Delete)

	// Audit & Activity
	auditHandler := handlers.NewAuditHandler(db)
	api.GET("/audit-logs", auditHandler.ListAuditLogs)
	api.GET("/activity-feed", auditHandler.ListActivityFeed)

	// Collection Runner
	runnerHandler := handlers.NewRunnerHandler(db)
	api.POST("/runner/run", runnerHandler.Run)

	// Data-Driven Testing
	dataDrivenHandler := handlers.NewDataDrivenHandler(db)
	api.POST("/runner/data-driven", dataDrivenHandler.Run)

	// Cookies
	cookieHandler := handlers.NewCookieHandler(db)
	api.GET("/cookies", cookieHandler.List)
	api.POST("/cookies", cookieHandler.Set)
	api.DELETE("/cookies/:id", cookieHandler.Delete)
	api.DELETE("/cookies", cookieHandler.Clear)

	// Import - Postman
	api.POST("/import/postman", importHandler.ImportPostman)
	api.POST("/import/postman/bulk", importHandler.ImportPostmanBulk)

	// Import - JUnit XML
	junitHandler := handlers.NewJUnitImportHandler(db)
	api.POST("/import/junit", junitHandler.Import)
	api.GET("/import/junit", junitHandler.List)
	api.GET("/import/junit/:id", junitHandler.Get)
	api.DELETE("/import/junit/:id", junitHandler.Delete)

	// Script Execution
	scriptHandler := handlers.NewScriptHandler(db)
	api.POST("/script/execute", scriptHandler.Execute)

	// Shared Steps
	sharedStepHandler := handlers.NewSharedStepHandler(db)
	api.GET("/shared-steps", sharedStepHandler.List)
	api.POST("/shared-steps", sharedStepHandler.Create)
	api.GET("/shared-steps/:id", sharedStepHandler.Get)
	api.PUT("/shared-steps/:id", sharedStepHandler.Update)
	api.DELETE("/shared-steps/:id", sharedStepHandler.Delete)
	api.POST("/shared-steps/:id/increment-usage", sharedStepHandler.IncrementUsage)
	api.POST("/shared-steps/:id/decrement-usage", sharedStepHandler.DecrementUsage)

	// Test Case Versions
	tcVersionHandler := handlers.NewTestCaseVersionHandler(db)
	api.GET("/test-cases/:id/versions", tcVersionHandler.ListVersions)
	api.GET("/test-cases/:id/versions/:versionId", tcVersionHandler.GetVersion)
	api.POST("/test-cases/:id/versions/:versionId/restore", tcVersionHandler.RestoreVersion)

	// Requirements & Traceability
	reqHandler := handlers.NewRequirementHandler(db)
	api.GET("/requirements", reqHandler.List)
	api.POST("/requirements", reqHandler.Create)
	api.GET("/requirements/:id", reqHandler.Get)
	api.PUT("/requirements/:id", reqHandler.Update)
	api.DELETE("/requirements/:id", reqHandler.Delete)
	api.POST("/requirements/:id/test-cases", reqHandler.LinkTestCase)
	api.DELETE("/requirements/:id/test-cases/:testCaseId", reqHandler.UnlinkTestCase)
	api.GET("/requirements/traceability", reqHandler.GetTraceabilityMatrix)

	// Global Search
	searchHandler := handlers.NewSearchHandler(db)
	api.GET("/search", searchHandler.Search)

	// Collection Sharing & Collaboration
	shareHandler := handlers.NewCollectionShareHandler(db)
	api.POST("/collections/:id/share", shareHandler.CreateShare)
	api.DELETE("/collections/:id/share", shareHandler.RevokeShare)
	api.GET("/collections/:id/collaborators", shareHandler.ListCollaborators)
	api.DELETE("/collections/:id/collaborators/:userId", shareHandler.RemoveCollaborator)
	api.DELETE("/collections/:id/leave", shareHandler.LeaveShare)
	api.POST("/collections/join", shareHandler.JoinByToken)

	// CORS Proxy (lightweight transparent proxy for browser direct mode)
	corsProxyHandler := handlers.NewCORSProxyHandler()
	api.POST("/cors-proxy", corsProxyHandler.Proxy)

	// DB Query tool — see DB_QUERY_TOOL_RFC.md. We reuse the existing
	// GitLab token cipher key (32-byte AES-256-GCM) so deployments
	// don't need a new secret env to start using this feature. If the
	// key is absent the routes still register but the encryption-using
	// handlers refuse with a clear error at request time — that way a
	// half-configured BE doesn't crash at boot.
	// In embedded mode the user almost never sets GITLAB_TOKEN_ENC_KEY
	// in their environment — there's no real `env` to set it in. Derive
	// a stable per-install key from a sibling file next to the SQLite
	// DB so the encryption-gated DB Query Tool routes register on the
	// desktop sidecar without extra configuration. The file is created
	// with 0600 on first run.
	tokenKey := cfg.GitLabTokenEncKey
	if tokenKey == "" && cfg.IsEmbedded() && cfg.DBDriver == "sqlite" {
		if k, err := ensureEmbeddedCipherKey(cfg.DBPath); err == nil {
			tokenKey = k
		}
	}

	// GitLab integration handler — constructed once and shared by BOTH the
	// Git tool's remote-read surface (git_remote.go, in the cipher-gated
	// block below) and the existing /integrations/gitlab OAuth routes. Reuses
	// the same token infra (validAccessToken/refresh) so remote reads
	// authenticate as the caller's linked account. Stays nil when GitLab is
	// disabled — remote reads then return the "link your account" 412.
	var gitH *handlers.GitIntegrationHandler
	if cfg.GitLabEnabled() {
		if h, gErr := handlers.NewGitIntegrationHandler(db, cfg); gErr != nil {
			log.Printf("GitLab integration disabled: %v", gErr)
		} else {
			gitH = h
		}
	}

	if tokenKey != "" {
		dbCipher, cErr := services.NewTokenCipher(tokenKey)
		if cErr == nil {
			registry := dbquery.NewRegistry(dbCipher)
			connH := handlers.NewDBConnectionHandler(db, dbCipher, registry)
			execH := handlers.NewDBQueryExecuteHandler(db, registry, connH)
			grantH := handlers.NewDBGrantHandler(db)
			savedH := handlers.NewDBSavedQueryHandler(db)
			histH := handlers.NewDBHistoryHandler(db)

			// Connection CRUD + ops
			api.POST("/db-connections", connH.Create)
			api.GET("/db-connections", connH.List)
			api.GET("/db-connections/:id", connH.Get)
			api.PATCH("/db-connections/:id", connH.Update)
			api.DELETE("/db-connections/:id", connH.Delete)
			api.POST("/db-connections/:id/test", connH.Test)
			api.POST("/db-connections/test", connH.Test) // pre-save inline payload
			api.POST("/db-connections/:id/reveal-password", connH.RevealPassword)
			api.GET("/db-connections/:id/schema", connH.Schema)
			api.POST("/db-connections/:id/refresh-schema", connH.RefreshSchema)

			// Grants
			api.GET("/db-connections/:id/grants", grantH.List)
			api.POST("/db-connections/:id/grants", grantH.Add)
			api.DELETE("/db-connections/:id/grants/:grantId", grantH.Delete)

			// Execute + cancel
			api.POST("/db-query/execute", execH.Execute)
			api.POST("/db-query/cancel", execH.Cancel)

			// Saved queries + history
			api.GET("/saved-db-queries", savedH.List)
			api.POST("/saved-db-queries", savedH.Create)
			api.PATCH("/saved-db-queries/:id", savedH.Update)
			api.DELETE("/saved-db-queries/:id", savedH.Delete)
			api.GET("/db-query/history", histH.List)

			// LLM providers — encrypted API key store (same cipher) + a
			// server-side streaming chat proxy. The key is decrypted only in
			// the proxy/reveal paths and never reaches the browser.
			llmH := handlers.NewLLMProviderHandler(db, dbCipher)
			api.POST("/llm-providers", llmH.Create)
			api.GET("/llm-providers", llmH.List)
			api.GET("/llm-providers/:id", llmH.Get)
			api.PATCH("/llm-providers/:id", llmH.Update)
			api.DELETE("/llm-providers/:id", llmH.Delete)
			api.POST("/llm-providers/:id/test", llmH.Test)
			api.POST("/llm-providers/test", llmH.Test) // pre-save inline payload
			api.POST("/llm-providers/:id/reveal-api-key", llmH.RevealAPIKey)
			api.POST("/llm/chat", llmH.Chat) // SSE streaming completion
			// AI-drawn diagrams from the user's live data: a server-side ReAct
			// loop over a read-only slice of the user's MCP tools → Mermaid.
			// Claude Memory splits remembered text into cards with the
			// person's chosen provider (decrypted with the same cipher).
			noteH.SetLLM(dbCipher)
			llmDiagH := handlers.NewLLMDiagramHandler(db, dbCipher, r, cfg.JWTSecret)
			api.POST("/llm/diagram", llmDiagH.Generate)

			// Claude Code terminal — spawns the locally-installed `claude` CLI in
			// a PTY rooted at the managed git-repos workspace. Desktop only (the
			// handler 403s on a hosted BE). WebSocket, OUR stdin/stdout framing.
			aiShellH := handlers.NewAIShellHandler(db, cfg.Mode == config.ModeEmbedded)
			api.GET("/ai/claude-status", aiShellH.ClaudeStatus)
			api.GET("/ai/claude-shell", aiShellH.ClaudeShell)

			// Kafka tool (RFC: KAFKA_S3_AND_DBQUERY_FIXES_RFC.md §A).
			// Reuses the same cipher for SASL secrets — no new env needed.
			kafkaReg := kafkaquery.NewRegistry(dbCipher)
			kConnH := handlers.NewKafkaConnectionHandler(db, dbCipher, kafkaReg)
			kAdminH := handlers.NewKafkaAdminHandler(db, kafkaReg, kConnH)
			kMsgH := handlers.NewKafkaMessageHandler(kAdminH)
			kGrantH := handlers.NewKafkaGrantHandler(db)

			api.POST("/kafka-connections", kConnH.Create)
			api.GET("/kafka-connections", kConnH.List)
			api.GET("/kafka-connections/:id", kConnH.Get)
			api.PATCH("/kafka-connections/:id", kConnH.Update)
			api.DELETE("/kafka-connections/:id", kConnH.Delete)
			api.POST("/kafka-connections/:id/test", kConnH.Test)
			api.POST("/kafka-connections/test", kConnH.Test)
			api.POST("/kafka-connections/:id/reveal-secret", kConnH.RevealSecret)
			api.GET("/kafka-connections/:id/grants", kGrantH.List)
			api.POST("/kafka-connections/:id/grants", kGrantH.Add)
			api.DELETE("/kafka-connections/:id/grants/:grantId", kGrantH.Delete)

			api.GET("/kafka/:id/topics", kAdminH.Topics)
			api.GET("/kafka/:id/topic-overview", kAdminH.TopicsOverview)
			api.GET("/kafka/:id/topics/:topic/detail", kAdminH.TopicDetail)
			api.GET("/kafka/:id/topics/:topic/config", kAdminH.TopicConfig)
			api.PATCH("/kafka/:id/topics/:topic/config", kAdminH.AlterTopicConfig)
			api.POST("/kafka/:id/topics", kAdminH.CreateTopic)
			api.POST("/kafka/:id/topics/:topic/partitions", kAdminH.AddPartitions)
			api.DELETE("/kafka/:id/topics/:topic", kAdminH.DeleteTopic)
			api.GET("/kafka/:id/brokers", kAdminH.Brokers)
			api.GET("/kafka/:id/groups", kAdminH.Groups)
			api.GET("/kafka/:id/lag", kAdminH.GroupsLag)
			api.GET("/kafka/:id/groups/:group", kAdminH.GroupDetail)
			api.GET("/kafka/:id/groups/:group/lag", kAdminH.GroupLag)
			api.POST("/kafka/:id/consume", kMsgH.Consume)
			api.POST("/kafka/:id/produce", kMsgH.Produce)

			// S3 / object-storage tool (RFC §B).
			s3Reg := s3client.NewRegistry(dbCipher)
			sConnH := handlers.NewS3ConnectionHandler(db, dbCipher, s3Reg)
			sObjH := handlers.NewS3ObjectHandler(db, s3Reg, sConnH)
			sGrantH := handlers.NewS3GrantHandler(db)

			api.POST("/s3-connections", sConnH.Create)
			api.GET("/s3-connections", sConnH.List)
			api.GET("/s3-connections/:id", sConnH.Get)
			api.PATCH("/s3-connections/:id", sConnH.Update)
			api.DELETE("/s3-connections/:id", sConnH.Delete)
			api.POST("/s3-connections/:id/test", sConnH.Test)
			api.POST("/s3-connections/test", sConnH.Test)
			api.POST("/s3-connections/:id/reveal-secret", sConnH.RevealSecret)
			api.GET("/s3-connections/:id/grants", sGrantH.List)
			api.POST("/s3-connections/:id/grants", sGrantH.Add)
			api.DELETE("/s3-connections/:id/grants/:grantId", sGrantH.Delete)

			api.GET("/s3/:id/buckets", sObjH.Buckets)
			api.GET("/s3/:id/objects", sObjH.ListObjects)
			api.GET("/s3/:id/object/meta", sObjH.ObjectMeta)
			api.GET("/s3/:id/object", sObjH.GetObject)
			api.POST("/s3/:id/object", sObjH.PutObject)
			api.POST("/s3/:id/object/copy", sObjH.CopyObject)
			api.DELETE("/s3/:id/object", sObjH.DeleteObject)
			api.POST("/s3/:id/presign", sObjH.Presign)
			api.POST("/s3/:id/folder", sObjH.CreateFolder)

			// Redis tool. Mirrors the Kafka/S3 pattern: encrypted endpoint
			// credentials + delegated grants + activity audit; SCAN-only
			// browse, type-aware value view, guarded command console,
			// commit-less pub/sub peek.
			redisReg := redisquery.NewRegistry(dbCipher)
			rConnH := handlers.NewRedisConnectionHandler(db, dbCipher, redisReg)
			rOpsH := handlers.NewRedisOpsHandler(db, redisReg, rConnH)
			rGrantH := handlers.NewRedisGrantHandler(db)

			api.POST("/redis-connections", rConnH.Create)
			api.GET("/redis-connections", rConnH.List)
			api.GET("/redis-connections/:id", rConnH.Get)
			api.PATCH("/redis-connections/:id", rConnH.Update)
			api.DELETE("/redis-connections/:id", rConnH.Delete)
			api.POST("/redis-connections/:id/test", rConnH.Test)
			api.POST("/redis-connections/test", rConnH.Test)
			api.POST("/redis-connections/:id/reveal-secret", rConnH.RevealSecret)
			api.GET("/redis-connections/:id/grants", rGrantH.List)
			api.POST("/redis-connections/:id/grants", rGrantH.Add)
			api.DELETE("/redis-connections/:id/grants/:grantId", rGrantH.Delete)

			api.GET("/redis/:id/keys", rOpsH.ScanKeys)
			api.GET("/redis/:id/key/meta", rOpsH.KeyMeta)
			api.GET("/redis/:id/key", rOpsH.GetValue)
			api.POST("/redis/:id/command", rOpsH.RunCommand)
			api.POST("/redis/:id/pubsub/peek", rOpsH.PeekChannel)
			api.DELETE("/redis/:id/key", rOpsH.DeleteKey)
			api.POST("/redis/:id/key/expire", rOpsH.SetTTL)
			api.POST("/redis/:id/key/element/remove", rOpsH.RemoveElement)

			// Elasticsearch tool. Mirrors the Redis pattern: encrypted
			// endpoint credentials + delegated grants + activity audit; a
			// thin authenticated HTTP forwarder (net/http only) with a
			// read/write classifier gating mutating requests. Reuses
			// dbCipher for secret encryption.
			esReg := esquery.NewRegistry(dbCipher)
			esConnH := handlers.NewESConnectionHandler(db, dbCipher, esReg)
			esOpsH := handlers.NewESOpsHandler(db, esReg, esConnH)
			esGrantH := handlers.NewESGrantHandler(db)

			api.POST("/es-connections", esConnH.Create)
			api.GET("/es-connections", esConnH.List)
			api.GET("/es-connections/:id", esConnH.Get)
			api.PATCH("/es-connections/:id", esConnH.Update)
			api.DELETE("/es-connections/:id", esConnH.Delete)
			api.POST("/es-connections/:id/test", esConnH.Test)
			api.POST("/es-connections/test", esConnH.Test)
			api.POST("/es-connections/:id/reveal-secret", esConnH.RevealSecret)
			api.GET("/es-connections/:id/grants", esGrantH.List)
			api.POST("/es-connections/:id/grants", esGrantH.Add)
			api.DELETE("/es-connections/:id/grants/:grantId", esGrantH.Delete)

			api.GET("/es/:id/cluster", esOpsH.Cluster)
			api.GET("/es/:id/indices", esOpsH.Indices)
			api.GET("/es/:id/aliases", esOpsH.Aliases)
			api.GET("/es/:id/mapping", esOpsH.Mapping)
			api.POST("/es/:id/request", esOpsH.Request)

			// Grafana — native dashboards from Grafana's HTTP API (no iframe).
			grafanaReg := grafanaquery.NewRegistry(dbCipher, nil)
			grafanaConnH := handlers.NewGrafanaConnectionHandler(db, dbCipher, grafanaReg)
			grafanaOpsH := handlers.NewGrafanaOpsHandler(db, grafanaReg, grafanaConnH)
			grafanaGrantH := handlers.NewGrafanaGrantHandler(db, grafanaConnH)
			api.POST("/grafana-connections", grafanaConnH.Create)
			api.GET("/grafana-connections", grafanaConnH.List)
			api.GET("/grafana-connections/:id", grafanaConnH.Get)
			api.PATCH("/grafana-connections/:id", grafanaConnH.Update)
			api.DELETE("/grafana-connections/:id", grafanaConnH.Delete)
			api.POST("/grafana-connections/:id/test", grafanaConnH.Test)
			api.POST("/grafana-connections/test", grafanaConnH.Test)
			api.POST("/grafana-connections/:id/reveal-secret", grafanaConnH.RevealSecret)
			api.GET("/grafana-connections/:id/grants", grafanaGrantH.List)
			api.POST("/grafana-connections/:id/grants", grafanaGrantH.Add)
			api.DELETE("/grafana-connections/:id/grants/:grantId", grafanaGrantH.Delete)
			api.GET("/grafana/:id/info", grafanaOpsH.Info)
			api.GET("/grafana/:id/search", grafanaOpsH.Search)
			api.GET("/grafana/:id/dashboards/:uid", grafanaOpsH.Dashboard)
			api.POST("/grafana/:id/variables", grafanaOpsH.Variables)
			api.POST("/grafana/:id/query", grafanaOpsH.Query)
			api.POST("/grafana/:id/panel-data", grafanaOpsH.PanelData)
			api.GET("/grafana/:id/ds/:uid/resource", grafanaOpsH.Resource)
			api.GET("/grafana/:id/public/:token", grafanaOpsH.PublicDashboard)
			api.POST("/grafana/:id/public/:token/panels/:panelId/query", grafanaOpsH.PublicPanelQuery)

			// RabbitMQ — management HTTP API + AMQP (stream reads, tail, move).
			// vhost / names travel as query params or in the body (gin would
			// decode the default vhost's %2F in a path segment).
			rabbitReg := rabbitquery.NewRegistry(dbCipher)
			rabbitConnH := handlers.NewRabbitConnectionHandler(db, dbCipher, rabbitReg)
			rabbitOpsH := handlers.NewRabbitOpsHandler(db, rabbitReg, rabbitConnH)
			rabbitGrantH := handlers.NewRabbitGrantHandler(db, rabbitConnH)
			api.POST("/rabbit-connections", rabbitConnH.Create)
			api.GET("/rabbit-connections", rabbitConnH.List)
			api.GET("/rabbit-connections/:id", rabbitConnH.Get)
			api.PATCH("/rabbit-connections/:id", rabbitConnH.Update)
			api.DELETE("/rabbit-connections/:id", rabbitConnH.Delete)
			api.POST("/rabbit-connections/:id/test", rabbitConnH.Test)
			api.POST("/rabbit-connections/test", rabbitConnH.Test)
			api.POST("/rabbit-connections/:id/reveal-secret", rabbitConnH.RevealSecret)
			api.GET("/rabbit-connections/:id/grants", rabbitGrantH.List)
			api.POST("/rabbit-connections/:id/grants", rabbitGrantH.Add)
			api.DELETE("/rabbit-connections/:id/grants/:grantId", rabbitGrantH.Delete)
			api.GET("/rabbit/:id/overview", rabbitOpsH.Overview)
			api.GET("/rabbit/:id/queues", rabbitOpsH.Queues)
			api.GET("/rabbit/:id/queue", rabbitOpsH.Queue)
			api.POST("/rabbit/:id/queue", rabbitOpsH.CreateQueue)
			api.DELETE("/rabbit/:id/queue", rabbitOpsH.DeleteQueue)
			api.GET("/rabbit/:id/exchanges", rabbitOpsH.Exchanges)
			api.GET("/rabbit/:id/exchange", rabbitOpsH.Exchange)
			api.POST("/rabbit/:id/exchange", rabbitOpsH.CreateExchange)
			api.DELETE("/rabbit/:id/exchange", rabbitOpsH.DeleteExchange)
			api.GET("/rabbit/:id/topology", rabbitOpsH.Topology)
			api.GET("/rabbit/:id/connections", rabbitOpsH.Connections)
			api.POST("/rabbit/:id/connections/close", rabbitOpsH.CloseConnection)
			api.GET("/rabbit/:id/channels", rabbitOpsH.Channels)
			api.GET("/rabbit/:id/consumers", rabbitOpsH.Consumers)
			api.GET("/rabbit/:id/policies", rabbitOpsH.Policies)
			api.POST("/rabbit/:id/messages/get", rabbitOpsH.GetMessages)
			api.POST("/rabbit/:id/messages/stream", rabbitOpsH.StreamRead)
			api.GET("/rabbit/:id/tail", rabbitOpsH.Tail)
			api.POST("/rabbit/:id/publish", rabbitOpsH.Publish)
			api.POST("/rabbit/:id/purge", rabbitOpsH.Purge)
			api.POST("/rabbit/:id/move", rabbitOpsH.Move)
			api.POST("/rabbit/:id/bindings", rabbitOpsH.Bind)
			api.POST("/rabbit/:id/bindings/delete", rabbitOpsH.Unbind)

			// Graylog — read-only log search over the REST API (streams,
			// search + histogram, dashboards / saved searches list). Token or
			// username/password; dual-written like the other tools.
			graylogReg := graylogquery.NewRegistry(dbCipher)
			graylogConnH := handlers.NewGraylogConnectionHandler(db, dbCipher, graylogReg)
			graylogOpsH := handlers.NewGraylogOpsHandler(db, graylogReg, graylogConnH)
			graylogGrantH := handlers.NewGraylogGrantHandler(db, graylogConnH)
			api.POST("/graylog-connections", graylogConnH.Create)
			api.GET("/graylog-connections", graylogConnH.List)
			api.GET("/graylog-connections/:id", graylogConnH.Get)
			api.PATCH("/graylog-connections/:id", graylogConnH.Update)
			api.DELETE("/graylog-connections/:id", graylogConnH.Delete)
			api.POST("/graylog-connections/:id/test", graylogConnH.Test)
			api.POST("/graylog-connections/test", graylogConnH.Test)
			api.POST("/graylog-connections/:id/reveal-secret", graylogConnH.RevealSecret)
			api.GET("/graylog-connections/:id/grants", graylogGrantH.List)
			api.POST("/graylog-connections/:id/grants", graylogGrantH.Add)
			api.DELETE("/graylog-connections/:id/grants/:grantId", graylogGrantH.Delete)
			api.GET("/graylog/:id/streams", graylogOpsH.Streams)
			api.POST("/graylog/:id/search", graylogOpsH.Search)
			api.GET("/graylog/:id/views", graylogOpsH.Views)

			// Kubernetes tool (read-only explorer, v1). Connection CRUD +
			// delegated grants + activity audit ride the same spine as the
			// other tools; live cluster ops are READ ONLY — contexts (no
			// cluster call), the ServerVersion probe, resource list/get, and
			// the pod-log SSE stream. No create/patch/delete/exec/apply
			// surface exists in v1. Every rest.Config has ExecProvider forced
			// nil (exec-credential-plugin RCE guard). Reuses dbCipher.
			k8sReg := k8sclient.NewRegistry(dbCipher, cfg.Mode == config.ModeEmbedded)
			k8sPF := k8sclient.NewPFRegistry()
			k8sConnH := handlers.NewK8sConnectionHandler(db, dbCipher, k8sReg)
			k8sOpsH := handlers.NewK8sOpsHandler(db, k8sReg, k8sConnH, cfg.Mode == config.ModeEmbedded, k8sPF)
			k8sGrantH := handlers.NewK8sGrantHandler(db)

			api.POST("/k8s-connections", k8sConnH.Create)
			api.GET("/k8s-connections", k8sConnH.List)
			api.GET("/k8s-connections/:id", k8sConnH.Get)
			api.PATCH("/k8s-connections/:id", k8sConnH.Update)
			api.DELETE("/k8s-connections/:id", k8sConnH.Delete)
			api.POST("/k8s-connections/:id/test", k8sConnH.Test)
			api.POST("/k8s-connections/test", k8sConnH.Test)
			api.POST("/k8s-connections/:id/reveal-secret", k8sConnH.RevealSecret)
			api.GET("/k8s-connections/:id/grants", k8sGrantH.List)
			api.POST("/k8s-connections/:id/grants", k8sGrantH.Add)
			api.DELETE("/k8s-connections/:id/grants/:grantId", k8sGrantH.Delete)

			api.GET("/k8s/:id/contexts", k8sOpsH.Contexts)
			api.GET("/k8s/:id/version", k8sOpsH.Version)
			api.GET("/k8s/:id/resources", k8sOpsH.Resources)
			api.GET("/k8s/:id/resource", k8sOpsH.Resource)
			api.GET("/k8s/:id/related", k8sOpsH.Related)
			api.GET("/k8s/:id/logs", k8sOpsH.Logs)
			api.GET("/k8s/:id/watch", k8sOpsH.Watch)
			api.GET("/k8s/:id/overview", k8sOpsH.Overview)
			api.GET("/k8s/:id/pod-metrics", k8sOpsH.PodMetrics)
			api.GET("/k8s/:id/pod-metrics-history", k8sOpsH.PodMetricsHistory)

			// v2 WRITE/EXEC surface. Every mutating route enforces the
			// write grant (access(c,true)) + audits the action/target;
			// can-i is the read-only RBAC preflight the FE uses to gate
			// action buttons. Rollout-restart / cronjob-trigger take a
			// FE-supplied timestamp (no time.Now() on the server path).
			api.GET("/k8s/:id/exec", k8sOpsH.Exec)              // websocket; write grant
			api.GET("/k8s/:id/local-shell", k8sOpsH.LocalShell) // websocket; embedded-only + write grant
			api.POST("/k8s/:id/apply", k8sOpsH.Apply)
			api.POST("/k8s/:id/scale", k8sOpsH.Scale)
			api.POST("/k8s/:id/rollout-restart", k8sOpsH.RolloutRestart)
			api.DELETE("/k8s/:id/resource", k8sOpsH.DeleteResource)
			api.POST("/k8s/:id/cordon", k8sOpsH.Cordon)
			api.POST("/k8s/:id/drain", k8sOpsH.Drain)
			api.POST("/k8s/:id/cronjob/trigger", k8sOpsH.CronjobTrigger)
			api.POST("/k8s/:id/can-i", k8sOpsH.CanI)

			// Port-forward: bind a local 127.0.0.1 listener to a pod/service
			// port on the user's own machine. Embedded-only (never on a hosted
			// BE) + write grant, same profile as local-shell. Stateful — a
			// forward lives until stopped or the BE exits.
			api.POST("/k8s/:id/port-forward", k8sOpsH.PortForwardStart)        // embedded-only + write grant
			api.GET("/k8s/:id/port-forwards", k8sOpsH.PortForwardList)         // embedded-only + write grant
			api.DELETE("/k8s/:id/port-forward/:pfId", k8sOpsH.PortForwardStop) // embedded-only + write grant

			// Observability connections (Coroot, Prometheus, Jaeger, Kiali,
			// SigNoz, Datadog) + the merged service map: K8s config layer,
			// the cluster's own Prometheus and every APM source, with the
			// user's saved DB / queue connections matched onto the nodes.
			// All read-only.
			apmReg := apm.NewRegistry(dbCipher)
			obsConnH := handlers.NewObservabilityConnectionHandler(db, dbCipher, apmReg)
			serviceMapH := handlers.NewServiceMapHandler(db, dbCipher, k8sReg, k8sConnH, apmReg, obsConnH)
			api.GET("/observability-providers", obsConnH.Providers)
			api.POST("/observability-connections", obsConnH.Create)
			api.GET("/observability-connections", obsConnH.List)
			api.GET("/observability-connections/:id", obsConnH.Get)
			api.PATCH("/observability-connections/:id", obsConnH.Update)
			api.DELETE("/observability-connections/:id", obsConnH.Delete)
			api.POST("/observability-connections/:id/test", obsConnH.Test)
			api.POST("/observability-connections/test", obsConnH.Test)
			api.POST("/observability-connections/:id/reveal-secret", obsConnH.RevealSecret)
			api.GET("/observability-connections/:id/scopes", obsConnH.Scopes)
			api.GET("/observability-connections/:id/grants", obsConnH.ListGrants)
			api.POST("/observability-connections/:id/grants", obsConnH.AddGrant)
			api.DELETE("/observability-connections/:id/grants/:grantId", obsConnH.DeleteGrant)
			api.GET("/service-map", serviceMapH.Get)

			// Git tools (v1) — see GIT_V1_CONTRACT.md. Connection CRUD +
			// delegated grants ride the same encrypted-secret spine as the
			// other tools (dbCipher encrypts the PAT/SSH secret; oauth
			// connections carry none). Remote reads proxy GitLab REST v4 as
			// the CALLER's linked account (gitH.validAccessToken). Local
			// working-tree ops are embedded-only and land in git_ops.go —
			// Wave 1 ships them as a compiling 501 stub.
			gitConnH := handlers.NewGitConnectionHandler(db, dbCipher)
			gitRemoteH := handlers.NewGitRemoteHandler(db, cfg, gitConnH, gitH)
			gitOps := handlers.NewGitOpsHandler(db, dbCipher, cfg.IsEmbedded())

			gitGroup := api.Group("/git")
			// Connection CRUD + grants (all scopes, dual-write friendly)
			gitGroup.POST("/connections", gitConnH.Create)
			gitGroup.GET("/connections", gitConnH.List)
			gitGroup.GET("/connections/:id", gitConnH.Get)
			gitGroup.PATCH("/connections/:id", gitConnH.Update)
			gitGroup.DELETE("/connections/:id", gitConnH.Delete)
			gitGroup.POST("/connections/:id/test", gitConnH.Test)
			gitGroup.POST("/connections/test", gitConnH.Test) // pre-save draft test
			gitGroup.GET("/connections/:id/secret", gitConnH.RevealSecret)
			gitGroup.GET("/connections/:id/grants", gitConnH.ListGrants)
			gitGroup.POST("/connections/:id/grants", gitConnH.AddGrant)
			gitGroup.DELETE("/connections/:id/grants", gitConnH.DeleteGrant)
			// Remote reads (GitLab REST v4 as the caller; pin remote on the FE)
			gitGroup.GET("/connections/:id/repos", gitRemoteH.Repos)
			gitGroup.GET("/connections/:id/branches", gitRemoteH.Branches)
			gitGroup.GET("/connections/:id/commits", gitRemoteH.Commits)
			gitGroup.GET("/connections/:id/commit", gitRemoteH.Commit)
			gitGroup.GET("/connections/:id/tree", gitRemoteH.Tree)
			gitGroup.GET("/connections/:id/file", gitRemoteH.File)
			gitGroup.GET("/connections/:id/compare", gitRemoteH.Compare)
			gitGroup.GET("/connections/:id/contributors", gitRemoteH.Contributors)
			gitGroup.GET("/connections/:id/merge_requests", gitRemoteH.MergeRequests)
			// MR review: single MR + diffs + notes (read), post comment + approve
			// (write — needs a write grant on the connection).
			gitGroup.GET("/connections/:id/merge_request", gitRemoteH.MergeRequest)
			gitGroup.GET("/connections/:id/merge_request_changes", gitRemoteH.MergeRequestChanges)
			gitGroup.GET("/connections/:id/merge_request_notes", gitRemoteH.MergeRequestNotes)
			gitGroup.POST("/connections/:id/merge_request_notes", gitRemoteH.CreateMRNote)
			gitGroup.POST("/connections/:id/merge_request_approve", gitRemoteH.ApproveMR)
			// CI/CD: pipelines, jobs, logs, schedules, variables (read —
			// variables need a write grant), run / retry / cancel / play (write).
			gitGroup.GET("/connections/:id/pipelines", gitRemoteH.Pipelines)
			gitGroup.GET("/connections/:id/pipeline", gitRemoteH.Pipeline)
			gitGroup.GET("/connections/:id/job_log", gitRemoteH.JobLog)
			gitGroup.GET("/connections/:id/pipeline_schedules", gitRemoteH.PipelineSchedules)
			gitGroup.GET("/connections/:id/ci_variables", gitRemoteH.CIVariables)
			gitGroup.GET("/connections/:id/ci_variable", gitRemoteH.CIVariable)
			gitGroup.POST("/connections/:id/pipeline_run", gitRemoteH.RunPipeline)
			gitGroup.POST("/connections/:id/pipeline_retry", gitRemoteH.RetryPipeline)
			gitGroup.POST("/connections/:id/pipeline_cancel", gitRemoteH.CancelPipeline)
			gitGroup.POST("/connections/:id/job_play", gitRemoteH.PlayJob)
			gitGroup.POST("/connections/:id/job_retry", gitRemoteH.RetryJob)
			gitGroup.POST("/connections/:id/schedule_play", gitRemoteH.PlaySchedule)
			// Local working-tree ops (embedded-only; 501 stub in Wave 1,
			// real bodies land in Wave 2 — signatures are frozen)
			gitGroup.POST("/connections/:id/clone", gitOps.Clone)
			gitGroup.GET("/repos", gitOps.ListRepos)
			gitGroup.GET("/repos/:repoId/status", gitOps.Status)
			gitGroup.GET("/repos/:repoId/log", gitOps.Log)
			gitGroup.GET("/repos/:repoId/branches", gitOps.Branches)
			gitGroup.GET("/repos/:repoId/diff", gitOps.Diff)
			gitGroup.POST("/repos/:repoId/stage", gitOps.Stage)
			gitGroup.POST("/repos/:repoId/unstage", gitOps.Unstage)
			gitGroup.POST("/repos/:repoId/commit", gitOps.Commit)
			gitGroup.POST("/repos/:repoId/push", gitOps.Push)
			gitGroup.POST("/repos/:repoId/pull", gitOps.Pull)
			gitGroup.GET("/repos/:repoId/file", gitOps.LocalFile)
			gitGroup.POST("/repos/:repoId/file", gitOps.WriteFile)
			gitGroup.GET("/repos/:repoId/local-path", gitOps.RevealPath)
		}
	}

	// MCP (Model Context Protocol) support — Patchman acts as an MCP
	// server so external AI clients (Claude, MCP Inspector) can drive the
	// same CRUD surface a user has by hand.
	//
	// Token management lives under the regular JWT-protected /api group:
	// a signed-in user mints/lists/revokes personal access tokens here.
	mcpTokenH := handlers.NewMCPTokenHandler(db)
	api.POST("/mcp/tokens", mcpTokenH.Create)
	api.GET("/mcp/tokens", mcpTokenH.List)
	api.PATCH("/mcp/tokens/:id", mcpTokenH.UpdateScopes)
	api.DELETE("/mcp/tokens/:id", mcpTokenH.Delete)

	// Local (this-device) MCP discovery status for the desktop FE's "Local"
	// card. Embedded-only: the hosted server has no discovery file and no
	// staged bridge, so the route simply isn't registered there (404).
	if cfg.IsEmbedded() {
		mcpLocalH := handlers.NewMCPLocalHandler(bridgePath)
		api.GET("/mcp/local", mcpLocalH.Get)

		// Cloud session handoff: the desktop FE posts the signed-in user's cloud
		// token here (loopback, RAM-only) so the whiteboard MCP tools can act on
		// the hosted BE as the user. Embedded-only; never on the hosted server.
		// See WHITEBOARD_UNIFIED_LOCAL_RFC.md.
		cloudSessionH := handlers.NewCloudSessionHandler()
		api.POST("/cloud-session", cloudSessionH.Set)
		api.DELETE("/cloud-session", cloudSessionH.Clear)
	}

	// The MCP transport endpoint itself is NOT under the JWT group — it
	// authenticates via the MCP personal access token (Authorization:
	// Bearer pmcp_…) and then runs every tool call in-process as that
	// user. Stateless + JSON responses keep it simple and curl-friendly
	// while remaining compatible with Inspector and Claude.
	mcpStream := mcpsdk.NewStreamableHTTPHandler(
		func(req *http.Request) *mcpsdk.Server {
			userID, _ := req.Context().Value(mcpUserCtxKey{}).(string)
			if userID == "" {
				return nil
			}
			scopes, _ := req.Context().Value(mcpScopesCtxKey{}).(*string)
			agent, _ := req.Context().Value(mcpAgentCtxKey{}).(string)
			project, _ := req.Context().Value(mcpProjectCtxKey{}).(string)
			return patchmcp.BuildServerAs(r, cfg.JWTSecret, userID, mcpscope.Parse(scopes), agent, project)
		},
		&mcpsdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
	r.Any("/api/mcp", func(c *gin.Context) {
		tok, ok := handlers.ResolveMCPTokenInfo(db, parseBearer(c.GetHeader("Authorization")))
		userID, scopes := tok.UserID, tok.Scopes
		if !ok {
			// NB: deliberately no WWW-Authenticate header. This endpoint
			// authenticates with a static MCP personal access token, not
			// OAuth. Advertising "Bearer" here makes spec-compliant MCP
			// clients (Inspector, Claude) kick off OAuth discovery against
			// /.well-known/oauth-* — which 404s and breaks the connect
			// flow. Returning a plain 401 keeps clients on token auth.
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or missing MCP token"})
			return
		}
		// The agent names itself only on initialize (MCP is stateless here):
		// keep that name on the token so later tool calls can sign with it.
		agent := tok.ClientName
		if name, ok := mcpInitializeClient(c); ok {
			agent = name
			handlers.RecordMCPClient(db, tok.ID, name)
		}
		// A bridge (≥1.56) names its own agent on every message: several
		// agents share the desktop's token, so the token's last name could be
		// another one's.
		if name := patchmcp.AgentName(c.GetHeader(patchmcp.ClientNameHeader), c.GetHeader(patchmcp.ClientTitleHeader)); name != "" {
			agent = name
		}
		ctx := context.WithValue(c.Request.Context(), mcpUserCtxKey{}, userID)
		ctx = context.WithValue(ctx, mcpScopesCtxKey{}, scopes)
		ctx = context.WithValue(ctx, mcpAgentCtxKey{}, agent)
		ctx = context.WithValue(ctx, mcpProjectCtxKey{}, patchmcp.CleanProject(c.GetHeader(patchmcp.ProjectHeader)))
		mcpStream.ServeHTTP(c.Writer, c.Request.WithContext(ctx))
	})

	// Realtime live-sync WebSocket. Mounted with JWTAuth only (no Audit,
	// no ActAsUser) — Upgrade requests skip CORS preflight and the
	// auditing layer would log a row for every long-lived connection
	// which has no business value. Token arrives via the `?token=`
	// query fallback already supported by JWTAuth (browser WebSocket
	// API can't set custom headers on the Upgrade).
	realtimeHandler := handlers.NewRealtimeHandler(cfg.RealtimeAllowedOrigins)
	r.GET("/api/realtime", authMW, realtimeHandler.Subscribe)

	// WebSocket Client
	wsHandler := handlers.NewWebSocketHandler()
	api.POST("/ws/connect", wsHandler.Connect)
	api.POST("/ws/send/:connectionId", wsHandler.Send)
	api.POST("/ws/disconnect/:connectionId", wsHandler.Disconnect)
	api.GET("/ws/history/:connectionId", wsHandler.History)

	// WebSocket stream (no auth — browser WS can't send headers, connectionId acts as token)
	r.GET("/api/ws/stream/:connectionId", wsHandler.Stream)

	// Load Testing
	loadTestHandler := handlers.NewLoadTestHandler(db)
	api.POST("/load-tests", loadTestHandler.Create)
	api.GET("/load-tests", loadTestHandler.List)
	api.GET("/load-tests/:id", loadTestHandler.Get)
	api.GET("/load-tests/:id/live", loadTestHandler.LiveResults)
	api.POST("/load-tests/:id/cancel", loadTestHandler.Cancel)
	api.DELETE("/load-tests/:id", loadTestHandler.Delete)

	// SSE Client
	sseHandler := handlers.NewSSEHandler()
	api.POST("/sse/connect", sseHandler.Connect)
	api.GET("/sse/connect", sseHandler.ConnectGET)

	// Sync (offline → online data push for freshly registered users)
	syncHandler := handlers.NewSyncHandler(db)
	api.POST("/sync/initial-push", syncHandler.InitialPush)
	// Forward endpoints receive per-event writes mirrored from a user's
	// embedded BE when they execute requests/test-runs locally but want
	// the records stored on the remote (source-of-truth) BE.
	api.POST("/sync/forward/history", syncHandler.ForwardHistory)
	api.POST("/sync/forward/test-runs", syncHandler.ForwardTestRun)

	// GitLab integration — OAuth account linking + typed GitLab calls.
	// Disabled (logged + skipped) when env credentials are missing so dev
	// boxes without integration setup don't fail to start.
	// gitH was constructed once above (shared with the Git tool's remote-read
	// surface). Register the OAuth account-linking + typed GitLab routes when
	// it's available; otherwise log the enable hint.
	if gitH != nil {
		gi := api.Group("/integrations/gitlab")
		gi.POST("/oauth/start", gitH.Start)
		gi.POST("/oauth/callback", gitH.Callback)
		gi.GET("", gitH.Status)
		gi.DELETE("", gitH.Disconnect)
		gi.GET("/token", gitH.Token) // short-lived token for local clone/push (Git tool)
		gi.POST("/import", gitH.Import)
		gi.GET("/collections/:id/binding", gitH.Binding)
		gi.POST("/collections/:id/refresh", gitH.Refresh)
		gi.POST("/collections/:id/push", gitH.Push)
	} else {
		log.Printf("GitLab integration disabled: set GITLAB_OAUTH_CLIENT_ID/SECRET + GITLAB_TOKEN_ENC_KEY to enable")
	}

	// API Documentation (Scalar UI - no auth)
	docsHandler := handlers.NewDocsHandler()
	r.GET("/api/docs", docsHandler.ScalarUI)
	r.GET("/api/docs/openapi.json", docsHandler.OpenAPISpec)

	// Public discovery — no auth. A desktop client that received a
	// `patchman://connect?backend=…` deep link (or typed a URL into the
	// first-run connect screen) fetches this to confirm the backend is
	// reachable and label the remote without the user typing a name.
	// Returned unwrapped (no {data} envelope) so a raw fetch reads it directly.
	r.GET("/api/config", func(c *gin.Context) {
		// Tenant values a client resolves AFTER picking this backend, so an
		// open-source build needn't bake them. The client caches the last
		// value and keeps using it when the backend is unreachable.
		gitlabHost := ""
		if u, err := url.Parse(cfg.GitLabBaseURL); err == nil {
			gitlabHost = u.Host
		}
		emailDomain := ""
		if len(cfg.AllowedEmailDomains) > 0 {
			emailDomain = cfg.AllowedEmailDomains[0] // already bare + lowercased
		}
		// The FE web host — used to build shareable public links (e.g. a
		// no-account whiteboard link) that must open in a browser, not the
		// desktop shell. Reuses the auth redirect allowlist's first entry so
		// there's no separate env to configure and it stays tenant-safe.
		webOrigin := ""
		if len(cfg.AllowedRedirectOrigins) > 0 {
			webOrigin = strings.TrimRight(cfg.AllowedRedirectOrigins[0], "/")
		}
		c.JSON(http.StatusOK, gin.H{
			"webOrigin":              webOrigin,
			// `service` is the identity marker a client uses to confirm this is
			// really a PatchMan backend (not just any URL that returns 200) —
			// and `apiVersion` is the contract version it checks against its own
			// supported range to refuse an incompatible backend up front.
			"service":                "patchman",
			"apiVersion":             apicontract.Version,
			"name":                   cfg.AppName,
			"version":                buildVersion,
			"gitlabHostname":         gitlabHost,
			"emailDomainPlaceholder": emailDomain,
		})
	})

	// Mock catch-all (no auth). The full path after `/mock/` is matched as
	// a single capture so multi-segment user-namespaced prefixes
	// (`kenan.kurt/test`) survive routing — the handler then walks
	// segments to pick the longest registered prefix.
	mockCatchAll := handlers.NewMockCatchAllHandler(db)
	r.Any("/mock/*fullpath", mockCatchAll.Handle)

	// JSON 404 for anything unmatched. Gin's default writes plain-text
	// "404 page not found", which breaks JSON-only clients — notably MCP
	// clients probing /.well-known/oauth-* endpoints, whose JSON parser
	// chokes on the plain-text body. A valid JSON 404 lets them fall back
	// to token auth cleanly.
	r.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
	})

	addr := cfg.Listen()
	// Bind explicitly BEFORE writing the discovery file so there is no
	// listen/announce race: once net.Listen returns, the kernel accepts
	// connections into the backlog, so a bridge that reads the file and
	// dials immediately can't beat the listener into existence. (This is
	// equivalent to the old r.Run(addr)=http.ListenAndServe, just split so
	// the file names a port that is already bound.)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("Failed to bind %s: %v", addr, err)
	}
	if cfg.IsEmbedded() {
		disco := mcpdisco.Info{
			URL:       "http://" + normalizeHost(addr) + "/api/mcp",
			Token:     localToken,
			PID:       os.Getpid(),
			StartedAt: time.Now().UTC(),
			Version:   buildVersion,
		}
		if derr := mcpdisco.Write(disco); derr != nil {
			log.Printf("mcp: could not write discovery file: %v", derr) // non-fatal
		}
		// Self-heal: if another PatchMan (a second instance, a crashed run)
		// leaves the file missing or pointing at a dead backend, this one
		// re-announces itself within seconds — bridges re-read per message.
		go mcpdisco.Keep(context.Background(), disco, 5*time.Second)
		installDiscoveryCleanup() // best-effort SIGINT/SIGTERM removal of our own entry
	}
	log.Printf("PatchMan API server starting in %s mode on %s (driver=%s)", cfg.Mode, addr, cfg.DBDriver)
	if err := http.Serve(ln, r); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

// ensureEmbeddedCipherKey returns a stable base64 AES-256 key for the
// TokenCipher when the embedded BE has none configured. The key lives
// next to the SQLite DB as `<db>.cipher.key` (mode 0600) so reads /
// writes / restarts share the same secret. On first call we create the
// file with 32 random bytes from crypto/rand.
func ensureEmbeddedCipherKey(dbPath string) (string, error) {
	keyPath := dbPath + ".cipher.key"
	if b, err := os.ReadFile(keyPath); err == nil {
		s := strings.TrimSpace(string(b))
		if s != "" {
			return s, nil
		}
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	encoded := base64.StdEncoding.EncodeToString(raw[:])
	if err := os.WriteFile(keyPath, []byte(encoded), 0o600); err != nil {
		return "", err
	}
	return encoded, nil
}

// mcpLocalTokenName is the fixed marker name for the embedded local user's
// MCP token row. EnsureLocalMCPToken keys on it so regeneration updates the
// same row rather than orphaning old ones.
const mcpLocalTokenName = "Local (desktop bridge)"

// ensureLocalMCPToken provisions a STABLE pmcp_ token for the embedded local
// user so the stdio bridge authenticates against /api/mcp without the user
// minting one by hand. The plaintext is persisted next to the SQLite DB as
// <db>.mcp.token (0600) — mirroring ensureEmbeddedCipherKey — and returned so
// main can write it into the discovery file (the only recoverable copy). The
// matching hashed row is upserted so ResolveMCPToken accepts it.
func ensureLocalMCPToken(db *gorm.DB, dbPath string) (string, error) {
	tokPath := dbPath + ".mcp.token"
	raw := ""
	if b, err := os.ReadFile(tokPath); err == nil {
		raw = strings.TrimSpace(string(b))
	}
	// Regenerate when the file is absent, empty, or doesn't look like a pmcp_
	// token — a truncated/garbage file must not crash-loop the sidecar boot
	// (EnsureLocalMCPToken slices the prefix).
	if len(raw) < 13 || !strings.HasPrefix(raw, "pmcp_") {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		raw = "pmcp_" + hex.EncodeToString(buf)
		if err := os.WriteFile(tokPath, []byte(raw), 0o600); err != nil {
			return "", err
		}
	}
	if err := handlers.EnsureLocalMCPToken(db, middleware.LocalUserID, raw, mcpLocalTokenName); err != nil {
		return "", err
	}
	return raw, nil
}

// normalizeHost guarantees the discovery URL carries a concrete loopback
// host:port even when the bind address omitted the host (":8080") or used a
// wildcard — the bridge only ever dials loopback. Embedded mode always binds
// an explicit 127.0.0.1:<port>, so this is defensive.
func normalizeHost(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// installDiscoveryCleanup removes the discovery file on a catchable exit
// signal. The Tauri shell kills the sidecar with SIGKILL (uncatchable), so
// this mainly covers `go run`/dev and any future graceful shutdown.
// Correctness does NOT depend on it: overwrite-on-start heals staleness and
// the bridge's dial check is the authoritative liveness signal.
func installDiscoveryCleanup() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-ch
		// Only our own entry: another PatchMan may have announced itself
		// since (a second instance must not cut the running app off).
		_ = mcpdisco.RemoveIfOwner(os.Getpid())
		os.Exit(0)
	}()
}

// ensureLocalUser creates the synthetic user row the embedded backend ties
// every record to. Idempotent — on startup we upsert by primary key.
func ensureLocalUser(db *gorm.DB) error {
	var existing models.User
	err := db.Where("id = ?", middleware.LocalUserID).First(&existing).Error
	if err == nil {
		return nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	user := models.User{
		ID:    middleware.LocalUserID,
		Email: "local@patchman.local",
		Name:  "Local User",
		// Password is never used — LocalUserAuth bypasses the login flow.
		Password: "-",
	}
	return db.Create(&user).Error
}
