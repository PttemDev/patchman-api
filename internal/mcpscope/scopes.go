// Package mcpscope defines the tool-group permission model for MCP tokens.
//
// An MCP token may be restricted to a subset of tool GROUPS (collections, db,
// kafka, …). The persisted form is a comma-separated list of enabled group
// keys stored on the token row; NULL means "all groups" — the default, so a
// token whose scopes were never set (and every pre-existing token after the
// column is added) keeps full access.
//
// This is a leaf package (imports only stdlib) so both internal/mcp (which
// filters tool registration) and internal/handlers (which validates writes)
// can import it without an import cycle.
package mcpscope

import "strings"

// Tool-group keys. Each maps to a register* group in internal/mcp/server.go
// and must stay in sync with the FE's group list (settings:mcp.perms.groups.*).
const (
	GroupCollections   = "collections"
	GroupRequests      = "requests"
	GroupEnvironments  = "environments"
	GroupTestCases     = "testcases"
	GroupMocks         = "mocks"
	GroupConnections   = "connections"
	GroupHTTP          = "http"
	GroupDB            = "db"
	GroupES            = "es"
	GroupRedis         = "redis"
	GroupKafka         = "kafka"
	GroupS3            = "s3"
	GroupK8s           = "k8s"
	GroupGit           = "git"
	GroupDevTools      = "devtools"
	GroupWhiteboards   = "whiteboards"
	GroupNotes         = "notes"
	GroupGrafana       = "grafana"
	GroupRabbitMQ      = "rabbitmq"
	GroupObservability = "observability"
	GroupGraylog       = "graylog"
)

// AllGroups is the canonical, ordered list of tool-group keys.
func AllGroups() []string {
	return []string{
		GroupCollections, GroupRequests, GroupEnvironments, GroupTestCases, GroupMocks,
		GroupConnections, GroupHTTP, GroupDB, GroupES, GroupRedis, GroupKafka, GroupS3, GroupK8s, GroupGit, GroupDevTools,
		GroupWhiteboards, GroupNotes, GroupGrafana, GroupRabbitMQ, GroupObservability, GroupGraylog,
	}
}

var validGroup = func() map[string]bool {
	m := make(map[string]bool, 15)
	for _, g := range AllGroups() {
		m[g] = true
	}
	return m
}()

// IsValidGroup reports whether key is a known tool group.
func IsValidGroup(key string) bool { return validGroup[key] }

// ToolScopes decides which tool groups a token may use. All=true means every
// group (the default for a token whose scopes were never set — NULL in the DB).
type ToolScopes struct {
	All     bool
	enabled map[string]bool
}

// Parse turns the persisted scopes column (nil = all) into a ToolScopes. A
// non-nil value is a comma-separated list of enabled group keys; an empty
// string therefore means "no groups".
func Parse(raw *string) ToolScopes {
	if raw == nil {
		return ToolScopes{All: true}
	}
	enabled := map[string]bool{}
	for _, part := range strings.Split(*raw, ",") {
		if g := strings.TrimSpace(part); g != "" {
			enabled[g] = true
		}
	}
	return ToolScopes{enabled: enabled}
}

// Allows reports whether the given tool group is enabled.
func (s ToolScopes) Allows(group string) bool {
	if s.All {
		return true
	}
	return s.enabled[group]
}
