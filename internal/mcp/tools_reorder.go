package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---- Reordering ------------------------------------------------------------
//
// The app shows sibling collections/folders, and the requests inside one
// collection, by sortOrder (then creation time). These tools set that order
// the way the sidebar's drag and drop does: read the current siblings, put
// the listed ids first, then everything not listed in its current order, and
// write sortOrder = index through the regular PUT endpoints, so permissions
// stay enforced by the REST layer. Only rows whose stored sortOrder differs
// from their new index are written; afterwards every sibling's sortOrder is
// its index, so ties from older rows (all 0) are resolved too.

type reorderCollectionsIn struct {
	ParentID    string   `json:"parentId,omitempty" jsonschema:"the collection or folder whose children you reorder; omit for top-level collections"`
	IDs         []string `json:"ids" jsonschema:"the sibling collection/folder ids in the order you want; children you leave out keep their current relative order after these"`
	WorkspaceID string   `json:"workspaceId,omitempty" jsonschema:"required at top level: the workspace whose collections you reorder (the app shows one workspace at a time); __none__ = collections in no workspace"`
}

type reorderRequestsIn struct {
	CollectionID string   `json:"collectionId" jsonschema:"the collection or folder whose saved requests you reorder"`
	IDs          []string `json:"ids" jsonschema:"request ids in the order you want; requests you leave out keep their current relative order after these"`
}

// orderItem is one sibling as the list endpoints return it.
type orderItem struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	SortOrder int    `json:"sortOrder"`
}

type reorderFailure struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Status int    `json:"status"`
	Error  string `json:"error"`
}

type reorderResult struct {
	Order     []orderItem      `json:"order"`
	Updated   []string         `json:"updated"`
	Unchanged int              `json:"unchanged"`
	Failed    []reorderFailure `json:"failed,omitempty"`
	Note      string           `json:"note,omitempty"`
}

func registerReorderTools(s *mcpsdk.Server, c *caller) {
	reg(s, "reorder_collections", "Set the order of sibling collections/folders, as the app's sidebar shows them. "+
		"Pass parentId (omit it for top-level collections; then workspaceId is required — the app orders one workspace at a time, __none__ = no workspace) and ids in the order you want. "+
		"Top level covers that workspace's collections as list_collections returns them; collections shared with you individually are listed by creation time and can't be reordered there. "+
		"Every id must be a current child of that parent — list_collections shows them in display order with their sortOrder; unknown ids are rejected and nothing is changed. "+
		"Children you leave out keep their current relative order AFTER the listed ones. "+
		"Only positions that change are written (sortOrder = position); needs editor access. Answers with the new order and any per-id failures.",
		func(in reorderCollectionsIn) (int, string) {
			var (
				current []orderItem
				code    int
				body    string
			)
			if strings.TrimSpace(in.ParentID) == "" {
				// One workspace at a time, like the sidebar: without it the
				// list spans every workspace, and renumbering it would reshuffle
				// shared team workspaces for everyone.
				if strings.TrimSpace(in.WorkspaceID) == "" {
					return http.StatusBadRequest, errJSON("workspaceId is required for top-level collections (use __none__ for collections in no workspace)")
				}
				p := "/api/collections?workspaceId=" + url.QueryEscape(strings.TrimSpace(in.WorkspaceID))
				code, body = c.call("GET", p, nil)
				if code >= 400 {
					return code, body
				}
				var res struct {
					Data []orderItem `json:"data"`
				}
				if err := json.Unmarshal([]byte(body), &res); err != nil {
					return http.StatusBadGateway, errJSON("couldn't read the collection list: " + err.Error())
				}
				current = res.Data
			} else {
				// The export view carries the parent's direct children in
				// display order, for any collection or folder the caller can read.
				code, body = c.call("GET", "/api/collections/"+url.PathEscape(in.ParentID)+"/export", nil)
				if code >= 400 {
					return code, body
				}
				var res struct {
					Data struct {
						Collection struct {
							Children []orderItem `json:"children"`
						} `json:"collection"`
					} `json:"data"`
				}
				if err := json.Unmarshal([]byte(body), &res); err != nil {
					return http.StatusBadGateway, errJSON("couldn't read the collection: " + err.Error())
				}
				current = res.Data.Collection.Children
			}
			return applyOrder(c, current, in.IDs, "collection", func(id string) string {
				return "/api/collections/" + url.PathEscape(id)
			})
		})

	reg(s, "reorder_requests", "Set the order of the saved requests inside one collection or folder, as the app shows them. "+
		"ids: request ids in the order you want. Every id must be in that collection — list_collection_requests shows them in display order with their sortOrder; unknown ids are rejected and nothing is changed. "+
		"Requests you leave out keep their current relative order AFTER the listed ones. "+
		"Only positions that change are written (sortOrder = position); needs editor access. Answers with the new order and any per-id failures.",
		func(in reorderRequestsIn) (int, string) {
			if strings.TrimSpace(in.CollectionID) == "" {
				return http.StatusBadRequest, errJSON("collectionId is required")
			}
			code, body := c.call("GET", "/api/collections/"+url.PathEscape(in.CollectionID)+"/requests", nil)
			if code >= 400 {
				return code, body
			}
			var res struct {
				Data []orderItem `json:"data"`
			}
			if err := json.Unmarshal([]byte(body), &res); err != nil {
				return http.StatusBadGateway, errJSON("couldn't read the request list: " + err.Error())
			}
			return applyOrder(c, res.Data, in.IDs, "request", func(id string) string {
				return "/api/requests/" + url.PathEscape(id)
			})
		})
}

// planOrder validates ids against the current siblings (already in display
// order) and returns the new order: the listed ids first, in the given order,
// then the unlisted ones in their current order. An empty, duplicated or
// unknown id is an error and nothing should be written.
func planOrder(current []orderItem, ids []string, kind string) ([]orderItem, string) {
	if len(ids) == 0 {
		return nil, "ids is empty: list the " + kind + " ids in the order you want"
	}
	byID := make(map[string]orderItem, len(current))
	for _, it := range current {
		byID[it.ID] = it
	}
	seen := make(map[string]bool, len(ids))
	var unknown, dup []string
	for _, id := range ids {
		switch {
		case seen[id]:
			dup = append(dup, id)
		case byID[id].ID == "":
			unknown = append(unknown, id)
		}
		seen[id] = true
	}
	if len(dup) > 0 {
		return nil, "these ids are listed more than once: " + strings.Join(dup, ", ")
	}
	if len(unknown) > 0 {
		return nil, fmt.Sprintf("not a %s here: %s. Current ones, in order: %s",
			kind, strings.Join(unknown, ", "), describeItems(current))
	}
	out := make([]orderItem, 0, len(current))
	for _, id := range ids {
		out = append(out, byID[id])
	}
	for _, it := range current {
		if !seen[it.ID] {
			out = append(out, it)
		}
	}
	return out, ""
}

// describeItems lists siblings as `id "name"` for an error the agent can act
// on, capped so a huge folder doesn't flood the answer.
func describeItems(items []orderItem) string {
	if len(items) == 0 {
		return "(none)"
	}
	const max = 50
	parts := make([]string, 0, len(items))
	for i, it := range items {
		if i == max {
			parts = append(parts, fmt.Sprintf("… and %d more", len(items)-max))
			break
		}
		parts = append(parts, fmt.Sprintf("%s %q", it.ID, it.Name))
	}
	return strings.Join(parts, "; ")
}

// applyOrder plans the new order, then writes sortOrder = position for every
// sibling whose stored value differs, one PUT each, collecting failures per id.
// Any failure makes the answer a tool error (with the first failure's status)
// whose body still says what was and wasn't moved.
func applyOrder(c *caller, current []orderItem, ids []string, kind string, path func(string) string) (int, string) {
	order, msg := planOrder(current, ids, kind)
	if msg != "" {
		return http.StatusBadRequest, errJSON(msg)
	}
	out := reorderResult{Order: make([]orderItem, 0, len(order)), Updated: []string{}}
	status := http.StatusOK
	for i, it := range order {
		if it.SortOrder == i {
			out.Unchanged++
			out.Order = append(out.Order, it)
			continue
		}
		// A map, not a struct with omitempty: position 0 must reach the wire.
		code, body := c.call("PUT", path(it.ID), map[string]any{"sortOrder": i})
		if code >= 400 {
			out.Failed = append(out.Failed, reorderFailure{ID: it.ID, Name: it.Name, Status: code, Error: errorText(body)})
			if status == http.StatusOK {
				status = code
			}
			out.Order = append(out.Order, it)
			continue
		}
		out.Updated = append(out.Updated, it.ID)
		out.Order = append(out.Order, orderItem{ID: it.ID, Name: it.Name, SortOrder: i})
	}
	if len(out.Failed) > 0 {
		out.Note = fmt.Sprintf("%d of %d moves failed; the others were saved, so the order is only partly applied. Check the failures, then list again to see the current order.",
			len(out.Failed), len(out.Failed)+len(out.Updated))
	}
	b, _ := json.Marshal(gin.H{"data": out})
	return status, string(b)
}

// errorText pulls the message out of a REST error body ({"error": "..."}),
// falling back to the raw body.
func errorText(body string) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(body), &e) == nil && e.Error != "" {
		return e.Error
	}
	return strings.TrimSpace(body)
}

func errJSON(msg string) string {
	b, _ := json.Marshal(gin.H{"error": msg})
	return string(b)
}
