package handlers

import "gorm.io/gorm"

// ResolveWhiteboardRole computes the effective role a user has on a whiteboard
// or folder. Unlike collections (grants only on the absolute root), whiteboards
// support sharing at ANY node: a shared folder cascades to everything inside it.
// So we walk from the node UP through its ancestors (capped) and take the
// max-wins role across every level:
//
//   - Owner match at any level (whiteboards.user_id = userID) → OwnerRole.
//   - Public visibility + a grant covering the user (direct / team / everyone).
//   - Workspace-team cascade → EditorRole for a team admin, ViewerRole for a
//     member (workspaceTeamRole).
//
// userID may be empty (anonymous) → NoRole (anonymous access uses the public
// link path, not this resolver). Reuses the generic hasPublicGrant helper.
func ResolveWhiteboardRole(db *gorm.DB, userID, whiteboardID string) Role {
	return resolveTreeRole(db, whiteboardTree, userID, whiteboardID)
}

// treeSpec names the tables of a shareable, nestable asset (whiteboards,
// notes) so one resolver serves both.
type treeSpec struct {
	table      string // node table with user_id, visibility, workspace_id, parent_id
	grantTable string // <x>_public_grants
	grantFK    string // FK column on the grant table
	softDelete bool   // node table has deleted_at; a deleted node grants nothing
}

var whiteboardTree = treeSpec{table: "whiteboards", grantTable: "whiteboard_public_grants", grantFK: "whiteboard_id"}

// resolveTreeRole walks from the node up through its ancestors (capped) and
// returns the max-wins role across every level — see ResolveWhiteboardRole.
// With softDelete, a deleted node anywhere on the path means "not found".
func resolveTreeRole(db *gorm.DB, spec treeSpec, userID, id string) Role {
	if id == "" {
		return NoRole
	}
	best := NoRole
	bump := func(r Role) {
		if roleRank[r] > roleRank[best] {
			best = r
		}
	}
	cur := id
	for i := 0; i < 64 && cur != ""; i++ {
		var node struct {
			UserID      string
			Visibility  string
			WorkspaceID string
			ParentID    *string
		}
		q := db.Table(spec.table).
			Select("user_id, visibility, workspace_id, parent_id").
			Where("id = ?", cur)
		if spec.softDelete {
			q = q.Where("deleted_at IS NULL")
		}
		if err := q.Scan(&node).Error; err != nil || node.UserID == "" {
			if spec.softDelete {
				return NoRole // a missing/deleted node or ancestor hides the subtree
			}
			return best
		}
		if userID != "" && node.UserID == userID {
			return OwnerRole
		}
		if userID != "" && node.Visibility == "public" {
			if hasPublicGrant(db, spec.grantTable, spec.grantFK, cur, "editor", userID) {
				bump(EditorRole)
			} else if hasPublicGrant(db, spec.grantTable, spec.grantFK, cur, "viewer", userID) {
				bump(ViewerRole)
			}
		}
		if userID != "" {
			bump(workspaceTeamRole(db, node.WorkspaceID, userID))
		}
		if node.ParentID == nil {
			return best
		}
		cur = *node.ParentID
	}
	return best
}

// canAccessWhiteboard: at least viewer. canEditWhiteboard: at least editor.
func canAccessWhiteboard(db *gorm.DB, userID, id string) bool {
	return ResolveWhiteboardRole(db, userID, id).AtLeast(ViewerRole)
}
func canEditWhiteboard(db *gorm.DB, userID, id string) bool {
	return ResolveWhiteboardRole(db, userID, id).AtLeast(EditorRole)
}

// isWhiteboardOwner reports true only when userID owns the node itself (not via
// a grant) — used to gate owner-only operations (delete, reparent, sharing).
func isWhiteboardOwner(db *gorm.DB, userID, id string) bool {
	if userID == "" || id == "" {
		return false
	}
	var n int64
	db.Table("whiteboards").Where("id = ? AND user_id = ?", id, userID).Count(&n)
	return n > 0
}
