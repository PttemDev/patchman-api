package middleware

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"patchman-be/internal/models"
)

// routeResource maps URL path segments to human-readable resource names.
var routeResource = map[string]string{
	"collections":  "collection",
	"requests":     "request",
	"environments": "environment",
	"test-cases":   "test_case",
	"test-plans":   "test_plan",
	"test-runs":    "test_run",
	"test-folders": "test_folder",
	"requirements": "requirement",
	"defects":      "defect",
	"mock-servers": "mock_server",
	"mock-routes":  "mock_route",
	"shared-steps": "shared_step",
	"cookies":      "cookie",
	"load-tests":   "load_test",
	"workspaces":   "workspace",
	"comments":     "comment",
	"import":       "import",
	"runner":       "runner",
	"proxy":        "proxy",
	"history":      "history",
	// New surface from the Teams + Public Sharing RFC. Visibility
	// flips and grant CRUD are sub-resources of collection/environment
	// — keeping their own names so audit filters can distinguish e.g.
	// "made public" from "renamed".
	"teams":      "team",
	"visibility": "visibility",
	"grants":     "grant",
	// DB Query Tool (RFC v1.13). Sub-actions (execute / cancel / test /
	// reveal-password / refresh-schema) get their own action verbs via
	// routeActionOverride below so audit reads "executed db_query"
	// instead of the noisy "created unknown" fallback.
	"db-connections":   "db_connection",
	"db-query":         "db_query",
	"saved-db-queries": "saved_db_query",
	// Redis tool. Connection CRUD + key/command ops audited under their
	// own resource names so audit filters can distinguish them.
	"redis-connections": "redis_connection",
	"redis":             "redis",
	// Elasticsearch tool. Connection CRUD + cluster/index/request ops
	// audited under their own resource names.
	"es-connections": "es_connection",
	"es":             "es",
	// Kubernetes tool (read-only explorer). Connection CRUD + read-only
	// cluster ops audited under their own resource names.
	"k8s-connections": "k8s_connection",
	"k8s":             "k8s",
	// Git tools (v1). Unlike the other tools, git nests its sub-resources
	// under /git (e.g. /git/connections/:id, /git/repos/:repoId), so the
	// nested "connections"/"repos" segments (git-only) map too — this lets
	// parseRoute read the real id after them instead of the sub-resource word.
	"git":         "git",
	"connections": "git_connection",
	"repos":       "git_repo",
}

// methodAction maps HTTP methods to action verbs.
var methodAction = map[string]string{
	"POST":   "created",
	"PUT":    "updated",
	"PATCH":  "updated",
	"DELETE": "deleted",
}

// routeActionOverride matches a path suffix to a more specific action
// verb. Iteration order is irrelevant — only one suffix can match per
// request because we test against the last segment.
var routeActionOverride = map[string]string{
	"execute":         "executed",
	"cancel":          "cancelled",
	"test":            "tested",
	"reveal-password": "revealed_password",
	"reveal-secret":   "revealed_secret",
	"refresh-schema":  "refreshed_schema",
	"schema":          "fetched_schema",
}

// AuditLog is a gin middleware that records mutating API calls
// (POST/PUT/PATCH/DELETE) into the audit_logs and activity_feeds tables.
// It runs AFTER the handler, so it can read the HTTP status code and only
// log successful operations (2xx).
func AuditLog(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Only log mutating methods
		method := c.Request.Method
		if method == "GET" || method == "HEAD" || method == "OPTIONS" {
			c.Next()
			return
		}

		// Let the handler run first
		c.Next()

		// Only log successful responses
		status := c.Writer.Status()
		if status < 200 || status >= 300 {
			return
		}

		userID := c.GetString("userID")
		if userID == "" {
			return
		}

		action, ok := methodAction[method]
		if !ok {
			action = strings.ToLower(method)
		}

		resource, resourceID := parseRoute(c.Request.URL.Path)

		// Sub-verb endpoints (POST /db-query/execute, POST /…/test, …)
		// deserve a more specific action than the generic "created" the
		// HTTP method would imply. The override table maps the last
		// non-id segment to its real verb.
		segments := strings.Split(strings.TrimPrefix(c.Request.URL.Path, "/api/"), "/")
		if len(segments) > 0 {
			last := segments[len(segments)-1]
			if override, has := routeActionOverride[last]; has {
				action = override
			}
		}

		now := time.Now()
		id := uuid.New().String()

		auditLog := models.AuditLog{
			ID:         id,
			UserID:     userID,
			Action:     action,
			Resource:   resource,
			ResourceID: resourceID,
			Details:    method + " " + c.Request.URL.Path,
			IPAddress:  c.ClientIP(),
			UserAgent:  c.Request.UserAgent(),
			CreatedAt:  now,
		}

		activityFeed := models.ActivityFeed{
			ID:         uuid.New().String(),
			UserID:     userID,
			Action:     action,
			Resource:   resource,
			ResourceID: resourceID,
			Summary:    action + " " + resource,
			CreatedAt:  now,
		}

		// Fire-and-forget: don't block the response
		go func() {
			db.Create(&auditLog)
			db.Create(&activityFeed)
		}()
	}
}

// parseRoute extracts a resource name and optional resource ID from an API path.
// Example: /api/collections/abc-123 → ("collection", "abc-123")
func parseRoute(path string) (string, string) {
	// Strip /api/ prefix
	path = strings.TrimPrefix(path, "/api/")
	segments := strings.Split(path, "/")

	resource := "unknown"
	resourceID := ""

	for i, seg := range segments {
		if mapped, ok := routeResource[seg]; ok {
			resource = mapped
			// Next segment is likely the ID
			if i+1 < len(segments) {
				candidate := segments[i+1]
				// Skip if next segment is another known route keyword
				if _, isRoute := routeResource[candidate]; !isRoute {
					resourceID = candidate
				}
			}
		}
	}

	return resource, resourceID
}
