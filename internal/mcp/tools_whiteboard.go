package mcp

import (
	"net/url"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---- Whiteboards -----------------------------------------------------------

type createWhiteboardIn struct {
	Name     string `json:"name" jsonschema:"whiteboard (or folder) name"`
	ParentID string `json:"parentId,omitempty" jsonschema:"parent folder id; omit for a top-level board"`
	Type     string `json:"type,omitempty" jsonschema:"'whiteboard' (default) or 'folder'"`
}

type updateWhiteboardIn struct {
	ID   string `json:"id" jsonschema:"whiteboard id to update"`
	Name string `json:"name,omitempty"`
	// Pointer so an explicit empty string ("move to root") survives JSON
	// marshalling; a plain string with omitempty would drop "" and the BE
	// would read it as "parent unchanged". Omitted (nil) => unchanged.
	ParentID *string `json:"parentId,omitempty" jsonschema:"move under this folder id; empty string moves to root; omit to leave unchanged"`
}

type drawWhiteboardIn struct {
	ID      string `json:"id" jsonschema:"whiteboard id to draw on"`
	Mermaid string `json:"mermaid" jsonschema:"Mermaid diagram source (e.g. 'graph TD; A-->B')"`
	Title   string `json:"title,omitempty" jsonschema:"optional label for the queued diagram"`
}

// registerWhiteboardTools exposes the caller's whiteboards over MCP: list / get
// / create / update / delete (the same owner+share rules as the REST API), plus
// draw_on_whiteboard, which QUEUES a Mermaid diagram the user's open board then
// renders onto the canvas (Mermaid→Excalidraw is a browser-only conversion, so
// the BE can't rasterize it here — it enqueues and the FE draws it).
func registerWhiteboardTools(s *mcpsdk.Server, c *caller) {
	// cloudOrLocal (not call): when signed in on desktop these act on the user's
	// CLOUD account, so an AI on the local MCP works on the boards the user
	// actually sees; offline/local they act on the on-device store. See
	// WHITEBOARD_UNIFIED_LOCAL_RFC.md.
	reg(s, "list_whiteboards", "List the caller's whiteboards + folders (metadata; no scene body).",
		func(_ struct{}) (int, string) { return c.cloudOrLocal("GET", "/api/whiteboards", nil) })
	reg(s, "get_whiteboard", "Get one whiteboard by id, including its full scene JSON.",
		func(in idIn) (int, string) { return c.cloudOrLocal("GET", "/api/whiteboards/"+url.PathEscape(in.ID), nil) })
	reg(s, "create_whiteboard", "Create a new whiteboard or folder.",
		func(in createWhiteboardIn) (int, string) { return c.cloudOrLocal("POST", "/api/whiteboards", in) })
	reg(s, "update_whiteboard", "Rename a whiteboard/folder or move it under another folder.",
		func(in updateWhiteboardIn) (int, string) {
			return c.cloudOrLocal("PATCH", "/api/whiteboards/"+url.PathEscape(in.ID), in)
		})
	reg(s, "delete_whiteboard", "Delete a whiteboard or folder (a folder deletes its contents) by id.",
		func(in idIn) (int, string) { return c.cloudOrLocal("DELETE", "/api/whiteboards/"+url.PathEscape(in.ID), nil) })
	reg(s, "draw_on_whiteboard", "Draw a Mermaid diagram onto a whiteboard. Queues it; it appears on the board when the user has it open.",
		func(in drawWhiteboardIn) (int, string) {
			return c.cloudOrLocal("POST", "/api/whiteboards/"+url.PathEscape(in.ID)+"/diagrams", in)
		})
}
