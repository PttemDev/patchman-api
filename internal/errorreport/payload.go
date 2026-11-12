// Package errorreport forwards FE crash reports to Slack via an incoming
// webhook. It is deliberately small: a payload type the handler decodes
// from JSON, a Block Kit builder that turns that payload into a readable
// Slack message, an in-memory dedupe so the same crash from a flapping
// page doesn't spam the channel, and a Reporter that POSTs the JSON.
//
// Toggles live in models.SystemSetting; the handler reads them and only
// calls Reporter.Post when the relevant source's toggle is on. Empty
// webhook URL is treated as "feature off" — Reporter.Post returns a
// noop status so the FE call still gets a clean response.
package errorreport

import "time"

// Source identifies which FE listener captured the error. Strings match
// the FE's `ErrorSource` literal so we don't need a translation layer.
type Source string

const (
	SourceBoundary   Source = "boundary"   // React render-time crash
	SourceRejection  Source = "rejection"  // window.onunhandledrejection
	SourceWindow     Source = "window"     // window.onerror
	SourceManual     Source = "manual"     // explicit FE report() call (admin "test integration")
)

// Payload is the wire shape the FE posts to /api/errors/report. The handler
// fills in server-known fields (user email, server time, IP) before
// passing this to the Reporter, so most fields here are FE-supplied
// context.
type Payload struct {
	// Required: error semantics.
	Source  Source `json:"source"`
	Message string `json:"message"`
	Stack   string `json:"stack,omitempty"`
	// Optional: minified-but-grouped React component stack from the
	// boundary. Useful enough to keep in the message even if it's not
	// pretty.
	ComponentStack string `json:"componentStack,omitempty"`

	// Build context — lets us spot whether a fix has shipped to a given
	// reporter or not.
	AppName    string `json:"appName,omitempty"`    // "patchman-ui"
	AppVersion string `json:"appVersion,omitempty"` // semver from package.json
	BuildEnv   string `json:"buildEnv,omitempty"`   // Vite's MODE: production / test / development
	BuildCommit string `json:"buildCommit,omitempty"` // short git sha
	Platform   string `json:"platform,omitempty"`   // "web" | "desktop"
	OSInfo     string `json:"osInfo,omitempty"`     // userAgent (web) or "macOS 26.3.1 / aarch64" (desktop)

	// Where in the app the user was. Route + active tab name.
	Route   string `json:"route,omitempty"`
	TabName string `json:"tabName,omitempty"`

	// Workspace context (optional — admins reading the message want to
	// know which workspace's user hit this).
	WorkspaceID   string `json:"workspaceId,omitempty"`
	WorkspaceName string `json:"workspaceName,omitempty"`

	// Filled in by the BE handler after auth — never trust FE for these.
	UserID    string    `json:"-"`
	UserEmail string    `json:"-"`
	UserName  string    `json:"-"`
	ServerAt  time.Time `json:"-"`
}

// Signature returns a stable identifier for this kind of error so the
// dedupe layer can collapse repeats. We deliberately ignore the user and
// time so a flapping bug across ten clients still produces ten Slack
// messages — but a single client refresh-storming the same error gets
// collapsed.
func (p *Payload) Signature() string {
	// First non-empty stack frame is the throw site. Two errors with the
	// same message but different origins should report separately.
	frame := firstStackFrame(p.Stack)
	return p.UserID + "::" + string(p.Source) + "::" + p.Message + "::" + frame
}

func firstStackFrame(stack string) string {
	for i := 0; i < len(stack); i++ {
		if stack[i] == '\n' {
			return stack[:i]
		}
	}
	return stack
}
