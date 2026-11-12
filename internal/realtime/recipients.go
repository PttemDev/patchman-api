package realtime

import (
	"gorm.io/gorm"
)

// CollectionRecipients returns the set of user ids that should receive
// realtime events about the given collection. Always includes the owner.
// Adds every individual CollectionShare collaborator. If the collection
// is public, adds every grant subject (user grants directly; team grants
// expand to members; an `everyone` grant sets the everyone flag).
//
// Result is deduped via a map. Callers feed it into Event.Recipients +
// Event.Everyone before calling Notify.
//
// Best-effort: any sub-query error is logged-via-return and silently
// degrades to "owner only" so a write never gets blocked by realtime
// machinery. The mutation already succeeded; we'd rather under-notify
// than fail the user-visible call.
func CollectionRecipients(db *gorm.DB, collectionID string) (recipients []string, everyone bool) {
	if db == nil || collectionID == "" {
		return nil, false
	}
	// Fresh session so we don't inherit a leftover WHERE/Model from the
	// handler's chain. Without this guard a stale condition on h.db
	// could narrow our scans to zero rows and silently produce an
	// empty recipient list — collaborators would stop hearing events.
	db = db.Session(&gorm.Session{})
	set := make(map[string]struct{})

	// Owner.
	var ownerID string
	if err := db.Table("collections").
		Select("user_id").
		Where("id = ?", collectionID).
		Scan(&ownerID).Error; err == nil && ownerID != "" {
		set[ownerID] = struct{}{}
	}

	// Individual CollectionShare collaborators (user_id IS NOT NULL).
	var shareUserIDs []string
	db.Table("collection_shares").
		Select("user_id").
		Where("collection_id = ? AND user_id IS NOT NULL AND user_id != ''", collectionID).
		Scan(&shareUserIDs)
	for _, u := range shareUserIDs {
		set[u] = struct{}{}
	}

	// Public grants. Need to check visibility first — a private collection
	// has no effective grants even if rows exist.
	var visibility string
	db.Table("collections").
		Select("visibility").
		Where("id = ?", collectionID).
		Scan(&visibility)
	if visibility == "public" {
		// Walk the three grant kinds. `everyone` is a flag, not a row
		// list — short-circuit and return immediately so the caller
		// uses the cheap "broadcast to all" path.
		type grantRow struct {
			SubjectKind string
			SubjectID   *string
		}
		var grants []grantRow
		db.Table("collection_public_grants").
			Select("subject_kind, subject_id").
			Where("collection_id = ?", collectionID).
			Scan(&grants)
		for _, g := range grants {
			switch g.SubjectKind {
			case "everyone":
				return nil, true
			case "user":
				if g.SubjectID != nil && *g.SubjectID != "" {
					set[*g.SubjectID] = struct{}{}
				}
			case "team":
				if g.SubjectID != nil && *g.SubjectID != "" {
					var teamMembers []string
					db.Table("team_members").
						Select("user_id").
						Where("team_id = ?", *g.SubjectID).
						Scan(&teamMembers)
					for _, u := range teamMembers {
						set[u] = struct{}{}
					}
				}
			}
		}
	}

	// Workspace-team cascade: if the collection sits in a team-bound
	// workspace, every member of that team also receives the event so
	// their sidebar refreshes. Mirrors the read-side cascade in
	// ResolveCollectionRole.
	addWorkspaceTeamMembers(db, "collections", collectionID, set)

	recipients = make([]string, 0, len(set))
	for u := range set {
		recipients = append(recipients, u)
	}
	return recipients, false
}

// addWorkspaceTeamMembers expands the workspace-team cascade into the
// recipient set: for the asset (collection or environment), find its
// workspace and, if that workspace is team-bound, add every team
// member. Idempotent — uses the same map the caller is already
// deduping into.
func addWorkspaceTeamMembers(db *gorm.DB, table, assetID string, set map[string]struct{}) {
	var workspaceID string
	if err := db.Table(table).
		Select("workspace_id").
		Where("id = ?", assetID).
		Scan(&workspaceID).Error; err != nil || workspaceID == "" {
		return
	}
	var teamID *string
	if err := db.Table("workspaces").
		Select("team_id").
		Where("id = ?", workspaceID).
		Scan(&teamID).Error; err != nil || teamID == nil || *teamID == "" {
		return
	}
	var memberIDs []string
	db.Table("team_members").
		Select("user_id").
		Where("team_id = ?", *teamID).
		Scan(&memberIDs)
	for _, u := range memberIDs {
		set[u] = struct{}{}
	}
}

// EnvironmentRecipients mirrors CollectionRecipients for environments.
// The schema parity (environments.visibility, environment_public_grants,
// environment_shares) means the same shape with different table names.
func EnvironmentRecipients(db *gorm.DB, environmentID string) (recipients []string, everyone bool) {
	if db == nil || environmentID == "" {
		return nil, false
	}
	db = db.Session(&gorm.Session{})
	set := make(map[string]struct{})

	var ownerID string
	if err := db.Table("environments").
		Select("user_id").
		Where("id = ?", environmentID).
		Scan(&ownerID).Error; err == nil && ownerID != "" {
		set[ownerID] = struct{}{}
	}

	var shareUserIDs []string
	db.Table("environment_shares").
		Select("user_id").
		Where("environment_id = ? AND user_id IS NOT NULL AND user_id != ''", environmentID).
		Scan(&shareUserIDs)
	for _, u := range shareUserIDs {
		set[u] = struct{}{}
	}

	var visibility string
	db.Table("environments").
		Select("visibility").
		Where("id = ?", environmentID).
		Scan(&visibility)
	if visibility == "public" {
		type grantRow struct {
			SubjectKind string
			SubjectID   *string
		}
		var grants []grantRow
		db.Table("environment_public_grants").
			Select("subject_kind, subject_id").
			Where("environment_id = ?", environmentID).
			Scan(&grants)
		for _, g := range grants {
			switch g.SubjectKind {
			case "everyone":
				return nil, true
			case "user":
				if g.SubjectID != nil && *g.SubjectID != "" {
					set[*g.SubjectID] = struct{}{}
				}
			case "team":
				if g.SubjectID != nil && *g.SubjectID != "" {
					var teamMembers []string
					db.Table("team_members").
						Select("user_id").
						Where("team_id = ?", *g.SubjectID).
						Scan(&teamMembers)
					for _, u := range teamMembers {
						set[u] = struct{}{}
					}
				}
			}
		}
	}

	addWorkspaceTeamMembers(db, "environments", environmentID, set)

	recipients = make([]string, 0, len(set))
	for u := range set {
		recipients = append(recipients, u)
	}
	return recipients, false
}

// RootCollectionID walks the parent chain to the top-level id. Mirrors
// the helper of the same job in handlers/collection_share.go — duplicated
// here so the realtime package doesn't take a handlers dependency (the
// handlers package already depends on this one). Returns the input id if
// the parent lookup fails (orphan rows behave as their own root).
func RootCollectionID(db *gorm.DB, collectionID string) string {
	if collectionID == "" {
		return ""
	}
	db = db.Session(&gorm.Session{})
	cur := collectionID
	for i := 0; i < 32; i++ { // depth cap defends against accidental cycles
		var row struct {
			ID       string
			ParentID *string
		}
		err := db.Table("collections").
			Select("id, parent_id").
			Where("id = ?", cur).
			Scan(&row).Error
		if err != nil || row.ID == "" {
			return cur
		}
		if row.ParentID == nil || *row.ParentID == "" {
			return row.ID
		}
		cur = *row.ParentID
	}
	return cur
}

// NoteRecipients returns who should hear about a change to a note. Notes can
// be shared at ANY level of the tree (a grant on a folder covers everything
// under it), so unlike CollectionRecipients this walks the note's ancestors and
// collects the grant subjects of every public node on the way up, plus the
// tree owner. An `everyone` grant anywhere short-circuits to a broadcast.
func NoteRecipients(db *gorm.DB, noteID string) (recipients []string, everyone bool) {
	if db == nil || noteID == "" {
		return nil, false
	}
	db = db.Session(&gorm.Session{})
	set := make(map[string]struct{})
	cur := noteID
	for i := 0; i < 64 && cur != ""; i++ {
		var n struct {
			UserID     string
			Visibility string
			ParentID   *string
		}
		// Unscoped on purpose: a just-deleted note still needs its audience.
		if err := db.Table("notes").Select("user_id, visibility, parent_id").Where("id = ?", cur).Scan(&n).Error; err != nil || n.UserID == "" {
			break
		}
		set[n.UserID] = struct{}{}
		if n.Visibility == "public" {
			var grants []struct {
				SubjectKind string
				SubjectID   *string
			}
			db.Table("note_public_grants").Select("subject_kind, subject_id").Where("note_id = ?", cur).Scan(&grants)
			for _, g := range grants {
				switch g.SubjectKind {
				case "everyone":
					return nil, true
				case "user":
					if g.SubjectID != nil && *g.SubjectID != "" {
						set[*g.SubjectID] = struct{}{}
					}
				case "team":
					if g.SubjectID != nil && *g.SubjectID != "" {
						var members []string
						db.Table("team_members").Select("user_id").Where("team_id = ?", *g.SubjectID).Scan(&members)
						for _, u := range members {
							set[u] = struct{}{}
						}
					}
				}
			}
		}
		if n.ParentID == nil {
			addWorkspaceTeamMembers(db, "notes", cur, set)
			break
		}
		cur = *n.ParentID
	}
	recipients = make([]string, 0, len(set))
	for u := range set {
		recipients = append(recipients, u)
	}
	return recipients, false
}
