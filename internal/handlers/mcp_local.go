package handlers

import (
	"net/http"
	"os"
	"path/filepath"

	"patchman-be/internal/mcpdisco"

	"github.com/gin-gonic/gin"
)

// mcpLocalServerName is the Claude Desktop server key for the local (stdio)
// connector. Distinct from the remote HTTP connector's "patchman" so a user
// can add both simultaneously.
const mcpLocalServerName = "patchman-local"

// MCPLocalHandler backs GET /api/mcp/local — the embedded-only endpoint the
// desktop FE's "Local (this device)" MCP card reads to render status and the
// copy-paste claude_desktop_config.json snippet. No token is returned to the
// webview: the stdio bridge self-discovers the URL + token from the discovery
// file.
type MCPLocalHandler struct {
	// bridgePath is the resolved absolute path of the staged bridge binary
	// (~/.patchman/bin/patchman-mcp). May be empty if staging failed; the FE
	// then shows a clear "bridge unavailable" state rather than a ~ path.
	bridgePath string
}

func NewMCPLocalHandler(bridgePath string) *MCPLocalHandler {
	return &MCPLocalHandler{bridgePath: bridgePath}
}

// Get returns the local MCP wiring the FE needs to build the stdio snippet
// and show status. `running` reflects that the discovery file exists and was
// written by THIS process (its recorded PID matches).
func (h *MCPLocalHandler) Get(c *gin.Context) {
	resp := gin.H{
		"running":          false,
		"url":              "",
		"bridgePath":       h.bridgePath,
		"discoveryPath":    "",
		"claudeConfigPath": claudeDesktopConfigPath(),
		"serverName":       mcpLocalServerName,
	}
	if p, err := mcpdisco.Path(); err == nil {
		resp["discoveryPath"] = p
	}
	if info, err := mcpdisco.Read(); err == nil {
		resp["url"] = info.URL
		// This endpoint answering already proves the app is up; confirm the
		// discovery file is present and names this process.
		resp["running"] = info.PID == os.Getpid()
	}
	c.JSON(http.StatusOK, resp)
}

// claudeDesktopConfigPath returns the per-OS Claude Desktop config location.
// <UserConfigDir>/Claude/claude_desktop_config.json is correct on both macOS
// (~/Library/Application Support) and Windows (%AppData%).
func claudeDesktopConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "Claude", "claude_desktop_config.json")
}
