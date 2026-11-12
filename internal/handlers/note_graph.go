package handlers

import (
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
)

// The memory graph ("Brain"): every node under the caller's agent memories —
// memory folders, their project folders and cards — and the typed links
// between them, plus the notes and PatchMan entities those links reach.
//
// Read access is inherited down the tree (resolveTreeRole walks up and takes
// the best role on the way), so everything under a readable memory folder is
// readable: one access check per root, none per card. Deleted nodes are left
// out with their whole subtree, as access resolution does. A linked note
// outside the memories is checked one by one (capped).

const (
	graphMaxNodes   = 4000
	graphMaxOutside = 300
	graphChunk      = 500
)

type graphNode struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"` // memory | project | folder | card | page | entity
	Title     string    `json:"title"`
	Summary   string    `json:"summary,omitempty"`
	ParentID  string    `json:"parentId,omitempty"`
	Memory    string    `json:"memory,omitempty"`  // the memory folder it sits in
	Project   string    `json:"project,omitempty"` // the project folder it sits in
	Scope     *memScope `json:"scope,omitempty"`   // memory folders only
	Agent     string    `json:"agent,omitempty"`
	Source    string    `json:"source,omitempty"`
	Stale     bool      `json:"stale,omitempty"`
	Uses      float64   `json:"uses,omitempty"`
	Outside   bool      `json:"outside,omitempty"` // a linked note outside agent memory
	UpdatedAt time.Time `json:"updatedAt"`
}

type graphLink struct {
	Src    string `json:"src"`
	Dst    string `json:"dst"`
	Type   string `json:"type"`
	Source string `json:"source"`
}

type graphRow struct {
	ID        string
	ParentID  *string
	Type      string
	Title     string
	Summary   string
	Source    string
	Agent     string
	Stale     bool
	Memory    bool
	Project   string
	UseCount  float64
	UpdatedAt time.Time
}

const graphCols = "id, parent_id, type, title, summary, source, agent, stale, memory, project, use_count, updated_at"

// MemoryGraph answers GET /notes/memory/graph[?root=<memory or project folder>].
func (h *NoteHandler) MemoryGraph(c *gin.Context) {
	userID := c.GetString("userID")
	scopes := newScopeResolver(h.db)
	var roots []graphRow
	if root := strings.TrimSpace(c.Query("root")); root != "" {
		var r graphRow
		if h.db.Table("notes").Select(graphCols).Where("id = ? AND deleted_at IS NULL AND type = ?", root, "folder").Scan(&r).Error != nil ||
			r.ID == "" || !canReadNote(h.db, userID, r.ID) || (!r.Memory && h.memoryRoot(&r.ID) == "") {
			c.JSON(http.StatusNotFound, gin.H{"error": "Memory folder not found"})
			return
		}
		roots = append(roots, r)
	} else {
		for _, f := range h.memoryFolders(userID) {
			roots = append(roots, graphRow{ID: f.ID, ParentID: f.ParentID, Type: f.Type, Title: f.Title, Memory: true, UpdatedAt: f.UpdatedAt})
		}
	}

	nodes := []graphNode{}
	index := map[string]int{}
	add := func(r graphRow, memory, project string) graphNode {
		n := graphNode{ID: r.ID, Title: r.Title, Summary: r.Summary, Agent: r.Agent, Source: r.Source, Stale: r.Stale,
			Uses: r.UseCount, UpdatedAt: r.UpdatedAt, Memory: memory, Project: project}
		if r.ParentID != nil {
			n.ParentID = *r.ParentID
		}
		switch {
		case r.Type == "folder" && r.Memory:
			n.Kind, n.Memory = "memory", r.ID
			sc := scopes.scope(userID, r.ID)
			n.Scope = &sc
		case r.Type == "folder" && r.Project != "":
			n.Kind, n.Project = "project", r.ID
		case r.Type == "folder":
			n.Kind = "folder"
		case r.Type == "card":
			n.Kind = "card"
		default:
			n.Kind = "page"
		}
		index[n.ID] = len(nodes)
		nodes = append(nodes, n)
		return n
	}
	truncated := false
	type ctx struct{ memory, project string }
	frontier := map[string]ctx{}
	for _, r := range roots {
		if _, dup := index[r.ID]; dup {
			continue
		}
		memory := r.ID
		if !r.Memory {
			memory = h.memoryRoot(&r.ID)
		}
		n := add(r, memory, "")
		frontier[r.ID] = ctx{n.Memory, n.Project}
	}
	for len(frontier) > 0 && !truncated {
		ids := make([]string, 0, len(frontier))
		for id := range frontier {
			ids = append(ids, id)
		}
		next := map[string]ctx{}
		for start := 0; start < len(ids) && !truncated; start += graphChunk {
			var rows []graphRow
			h.db.Table("notes").Select(graphCols).Where("parent_id IN ? AND deleted_at IS NULL", ids[start:min(start+graphChunk, len(ids))]).
				Order("sort_order ASC, created_at ASC").Scan(&rows)
			for _, r := range rows {
				if _, seen := index[r.ID]; seen {
					continue
				}
				if len(nodes) >= graphMaxNodes {
					truncated = true
					break
				}
				parent := frontier[*r.ParentID]
				n := add(r, parent.memory, parent.project)
				if r.Type == "folder" || r.Type == "page" {
					next[r.ID] = ctx{n.Memory, n.Project}
				}
			}
		}
		frontier = next
	}

	// Links touching the set, both ways.
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	links := []graphLink{}
	seenLink := map[string]bool{}
	var outside []string
	outsideSeen := map[string]bool{}
	for start := 0; start < len(ids); start += graphChunk {
		chunk := ids[start:min(start+graphChunk, len(ids))]
		var rows []models.NoteLink
		h.db.Where("src_note_id IN ? OR dst_note_id IN ?", chunk, chunk).Find(&rows)
		for _, l := range rows {
			if seenLink[l.ID] {
				continue
			}
			seenLink[l.ID] = true
			dst := l.DstNoteID
			if l.DstURI != "" {
				dst = l.DstURI
				if _, ok := index[dst]; !ok {
					index[dst] = len(nodes)
					nodes = append(nodes, graphNode{ID: dst, Kind: "entity", Title: entityTitle(dst)})
				}
			}
			for _, end := range []string{l.SrcNoteID, dst} {
				if _, in := index[end]; !in && !outsideSeen[end] {
					outsideSeen[end] = true
					outside = append(outside, end)
				}
			}
			links = append(links, graphLink{Src: l.SrcNoteID, Dst: dst, Type: l.Type, Source: l.Source})
		}
	}
	// Linked notes outside the memories, when the caller may read them.
	for start := 0; start < len(outside) && start < graphMaxOutside; start += graphChunk {
		var rows []graphRow
		h.db.Table("notes").Select(graphCols).Where("id IN ? AND deleted_at IS NULL", outside[start:min(min(start+graphChunk, len(outside)), graphMaxOutside)]).Scan(&rows)
		for _, r := range rows {
			if !canReadNote(h.db, userID, r.ID) {
				continue
			}
			n := add(r, "", "")
			nodes[index[n.ID]].Outside = true
		}
	}
	kept := links[:0]
	for _, l := range links {
		_, a := index[l.Src]
		_, b := index[l.Dst]
		if a && b {
			kept = append(kept, l)
		}
	}
	if len(outside) > graphMaxOutside {
		truncated = true
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"nodes": nodes, "links": kept, "truncated": truncated}})
}

// entityTitle is a short label for a patchman:// URI: its kind and last part
// ("k8s Deployment/auth", "request 42").
func entityTitle(uri string) string {
	parts := strings.Split(strings.TrimPrefix(uri, "patchman://"), "/")
	if len(parts) == 0 {
		return uri
	}
	if len(parts) <= 2 {
		return strings.Join(parts, " ")
	}
	return parts[0] + " " + strings.Join(parts[len(parts)-2:], "/")
}
