package handlers

import (
	"gorm.io/gorm"
)

// Role models the effective permission a user has on a collection or
// environment. Ordering matters — `OwnerRole > EditorRole > ViewerRole >
// NoRole` — and `(*Role).AtLeast(other)` uses that ordering for guards.
// Values are kept as strings so they can round-trip through JSON and
// the DB columns (`collection_shares.role`, `collection_public_grants.role`).
type Role string

const (
	NoRole     Role = ""
	ViewerRole Role = "viewer"
	EditorRole Role = "editor"
	OwnerRole  Role = "owner"
)

var roleRank = map[Role]int{
	NoRole:     0,
	ViewerRole: 1,
	EditorRole: 2,
	OwnerRole:  3,
}

// AtLeast returns true when r is at least as permissive as min. NoRole
// is never AtLeast anything except itself.
func (r Role) AtLeast(min Role) bool {
	return roleRank[r] >= roleRank[min]
}

// ResolveCollectionRole computes the effective role a user has on a
// (root) collection. **Max-role wins** across every access path the
// Teams RFC plus the workspace-cascade extension defines:
//
//  1. Owner match — `collections.user_id = userID` → OwnerRole, short-circuits
//  2. Legacy direct share — a CollectionShare row with `userID` set →
//     editor or viewer (legacy share-code collaborators)
//  3. Public visibility + grant — only when `collections.visibility =
//     'public'`. Editor wins over Viewer on this surface.
//  4. Workspace-team cascade — if the collection's workspace is bound
//     to a team and `userID` is in that team → EditorRole for a team
//     admin, ViewerRole for a member (see workspaceTeamRole).
//
// Prior to 1.17.5 these branches short-circuited the first match, so
// a stale CollectionShare viewer row would mask a freshly-granted
// public-grant editor row (legacy was checked before public). Max-wins
// guarantees the user gets the highest privilege any path grants.
//
// `userID` may be empty (anonymous) → NoRole. Pass the root collection
// id; child folders inherit, so callers should walk up to ParentID=nil
// before calling (see rootCollectionID).
func ResolveCollectionRole(db *gorm.DB, userID, collectionID string) Role {
	role, _ := ResolveCollectionRoleDetailed(db, userID, collectionID)
	return role
}

// ResolveCollectionRoleDetailed returns the resolved role plus the
// intermediate hits the resolver found. Used by the debug endpoint to
// surface why a user landed at a given role; production callers should
// stick with ResolveCollectionRole.
type RoleSources struct {
	IsOwner       bool `json:"isOwner"`
	LegacyShare   Role `json:"legacyShare,omitempty"`
	PublicGrant   Role `json:"publicGrant,omitempty"`
	WorkspaceTeam bool `json:"workspaceTeam"`
	// WorkspaceTeamRole is what the workspace's team grants: editor for a
	// team admin, viewer for a member.
	WorkspaceTeamRole Role   `json:"workspaceTeamRole,omitempty"`
	Visibility        string `json:"visibility"`
	WorkspaceID       string `json:"workspaceId,omitempty"`
	ResolvedRootID    string `json:"resolvedRootId,omitempty"`
}

func ResolveCollectionRoleDetailed(db *gorm.DB, userID, collectionID string) (Role, RoleSources) {
	src := RoleSources{ResolvedRootID: collectionID}
	if collectionID == "" {
		return NoRole, src
	}

	var col struct {
		UserID      string
		Visibility  string
		WorkspaceID string
	}
	if err := db.Table("collections").
		Select("user_id, visibility, workspace_id").
		Where("id = ?", collectionID).
		Scan(&col).Error; err != nil || col.UserID == "" {
		return NoRole, src
	}
	src.Visibility = col.Visibility
	src.WorkspaceID = col.WorkspaceID

	if userID != "" && col.UserID == userID {
		src.IsOwner = true
		return OwnerRole, src
	}
	if userID == "" {
		return NoRole, src
	}

	best := NoRole
	bump := func(r Role) {
		if roleRank[r] > roleRank[best] {
			best = r
		}
	}

	// Legacy direct share (CollectionShare).
	var share struct{ Role string }
	if err := db.Table("collection_shares").
		Select("role").
		Where("collection_id = ? AND user_id = ?", collectionID, userID).
		Limit(1).
		Scan(&share).Error; err == nil && share.Role != "" {
		r := roleFromShareString(share.Role)
		src.LegacyShare = r
		bump(r)
	}

	// Public visibility + grant. Check editor first; viewer only matters
	// when editor isn't there (a single row can carry only one role).
	if col.Visibility == "public" {
		if hasPublicGrant(db, "collection_public_grants", "collection_id", collectionID, "editor", userID) {
			src.PublicGrant = EditorRole
			bump(EditorRole)
		} else if hasPublicGrant(db, "collection_public_grants", "collection_id", collectionID, "viewer", userID) {
			src.PublicGrant = ViewerRole
			bump(ViewerRole)
		}
	}

	// Workspace-team cascade: admins edit, members view.
	if r := workspaceTeamRole(db, col.WorkspaceID, userID); r != NoRole {
		src.WorkspaceTeam = true
		src.WorkspaceTeamRole = r
		bump(r)
	}

	return best, src
}

// workspaceTeamRole is the role a workspace's team grants on the content
// in it (collections, environments, whiteboards, notes): EditorRole for a
// team admin, ViewerRole for any other member, NoRole when the workspace
// is not bound to a team or `userID` is not in it. Editors change content
// but cannot delete, rename or re-share the owner's items. Centralised so
// every resolver shares one lookup and a future swap (e.g. multi-team
// workspaces) lands in one place.
func workspaceTeamRole(db *gorm.DB, workspaceID, userID string) Role {
	if workspaceID == "" || userID == "" {
		return NoRole
	}
	var teamID *string
	if err := db.Table("workspaces").
		Select("team_id").
		Where("id = ?", workspaceID).
		Scan(&teamID).Error; err != nil || teamID == nil || *teamID == "" {
		return NoRole
	}
	var n int64
	db.Table("team_members").
		Where("team_id = ? AND user_id = ?", *teamID, userID).
		Count(&n)
	if n == 0 {
		return NoRole
	}
	var admins int64
	db.Table("team_members").
		Where("team_id = ? AND user_id = ? AND role = ?", *teamID, userID, "admin").
		Count(&admins)
	if admins > 0 {
		return EditorRole
	}
	return ViewerRole
}

// ResolveEnvironmentRole — env-side parity of ResolveCollectionRole.
// Same max-wins semantics; legacy direct-share consults
// `environment_shares` instead of `collection_shares`.
func ResolveEnvironmentRole(db *gorm.DB, userID, environmentID string) Role {
	role, _ := ResolveEnvironmentRoleDetailed(db, userID, environmentID)
	return role
}

func ResolveEnvironmentRoleDetailed(db *gorm.DB, userID, environmentID string) (Role, RoleSources) {
	src := RoleSources{ResolvedRootID: environmentID}
	if environmentID == "" {
		return NoRole, src
	}

	var env struct {
		UserID      string
		Visibility  string
		WorkspaceID string
	}
	if err := db.Table("environments").
		Select("user_id, visibility, workspace_id").
		Where("id = ?", environmentID).
		Scan(&env).Error; err != nil || env.UserID == "" {
		return NoRole, src
	}
	src.Visibility = env.Visibility
	src.WorkspaceID = env.WorkspaceID

	if userID != "" && env.UserID == userID {
		src.IsOwner = true
		return OwnerRole, src
	}
	if userID == "" {
		return NoRole, src
	}

	best := NoRole
	bump := func(r Role) {
		if roleRank[r] > roleRank[best] {
			best = r
		}
	}

	var share struct{ Role string }
	if err := db.Table("environment_shares").
		Select("role").
		Where("environment_id = ? AND user_id = ?", environmentID, userID).
		Limit(1).
		Scan(&share).Error; err == nil && share.Role != "" {
		r := roleFromShareString(share.Role)
		src.LegacyShare = r
		bump(r)
	}

	if env.Visibility == "public" {
		if hasPublicGrant(db, "environment_public_grants", "environment_id", environmentID, "editor", userID) {
			src.PublicGrant = EditorRole
			bump(EditorRole)
		} else if hasPublicGrant(db, "environment_public_grants", "environment_id", environmentID, "viewer", userID) {
			src.PublicGrant = ViewerRole
			bump(ViewerRole)
		}
	}

	if r := workspaceTeamRole(db, env.WorkspaceID, userID); r != NoRole {
		src.WorkspaceTeam = true
		src.WorkspaceTeamRole = r
		bump(r)
	}

	return best, src
}

// hasPublicGrant checks for ANY grant row on the given asset/role that
// covers `userID` — either directly (subject_kind='user'), through one
// of their teams (subject_kind='team'), or via the everyone singleton.
// Team subject rows pointing at soft-deleted teams are excluded.
func hasPublicGrant(db *gorm.DB, table, fkColumn, assetID, role, userID string) bool {
	var n int64
	// One SQL with three OR branches; user_team_ids is materialized via a
	// correlated subquery against team_members. Index hits:
	//   - cpg/epg compound (asset_id, role) for the filter
	//   - team_members(user_id) for the inner-IN
	//   - cpg/epg(subject_id) partial-index for the user/team variants
	q := db.Table(table).
		Where(fkColumn+" = ? AND role = ?", assetID, role).
		Where(
			"(subject_kind = 'user' AND subject_id = ?)"+
				" OR (subject_kind = 'team' AND subject_id IN (?))"+
				" OR (subject_kind = 'everyone')",
			userID,
			db.Table("team_members tm").Select("tm.team_id").Where("tm.user_id = ?", userID),
		)
	// Skip orphaned team grants pointing at soft-deleted teams.
	q = q.Where("subject_kind <> 'team' OR EXISTS (SELECT 1 FROM teams t WHERE t.id = " + table + ".subject_id AND t.deleted_at IS NULL)")
	q.Count(&n)
	return n > 0
}

// roleFromShareString maps the persisted share role string (which may
// be 'editor', 'viewer', or — older rows — 'admin' for collaborators)
// into a Role enum. Anything unknown is treated as ViewerRole (the
// safest fallback for read-paths).
func roleFromShareString(s string) Role {
	switch s {
	case "editor", "admin":
		return EditorRole
	case "viewer":
		return ViewerRole
	default:
		return ViewerRole
	}
}
