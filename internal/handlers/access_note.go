package handlers

import "gorm.io/gorm"

var noteTree = treeSpec{table: "notes", grantTable: "note_public_grants", grantFK: "note_id", softDelete: true}

// ResolveNoteRole is the effective role on a note: the max over the node and
// its ancestors (owner of the tree, a grant on any public ancestor, or the
// workspace-team cascade). A soft-deleted node or ancestor resolves to NoRole.
func ResolveNoteRole(db *gorm.DB, userID, noteID string) Role {
	return resolveTreeRole(db, noteTree, userID, noteID)
}

func canReadNote(db *gorm.DB, userID, id string) bool {
	return ResolveNoteRole(db, userID, id).AtLeast(ViewerRole)
}

func canEditNote(db *gorm.DB, userID, id string) bool {
	return ResolveNoteRole(db, userID, id).AtLeast(EditorRole)
}

// isNoteOwner: userID owns this exact node (the tree owner), not via a grant.
func isNoteOwner(db *gorm.DB, userID, id string) bool {
	if userID == "" || id == "" {
		return false
	}
	var n int64
	db.Table("notes").Where("id = ? AND user_id = ? AND deleted_at IS NULL", id, userID).Count(&n)
	return n > 0
}
