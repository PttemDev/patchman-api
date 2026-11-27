package handlers

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// QA rows (test cases, folders, plans, runs, defects, requirements, shared
// steps, load tests) are personal (workspace_id NULL) or shared with a
// workspace (CR_QA_RFC O5). Access:
//
//   - personal row: only its owner (user_id);
//   - workspace row: its owner, plus the workspace's people —
//     the workspace owner and a team admin act as owner (they may delete
//     anything), a team member as editor (create, edit, run; delete only
//     what they own).
//
// Lists and creates take ?workspaceId= (absent = personal); single rows are
// resolved from the row itself, so links to a shared case just work.

type qaOwned interface {
	QAOwner() (userID string, workspaceID *string)
}

// qaWorkspaceRole: the role a user has on a workspace's QA content.
func qaWorkspaceRole(db *gorm.DB, workspaceID, userID string) Role {
	if workspaceID == "" || userID == "" {
		return NoRole
	}
	var ws struct {
		OwnerID string
		TeamID  *string
	}
	if err := db.Table("workspaces").Select("owner_id, team_id").Where("id = ?", workspaceID).Scan(&ws).Error; err != nil {
		return NoRole
	}
	if ws.OwnerID == userID {
		return OwnerRole
	}
	if ws.TeamID == nil || *ws.TeamID == "" {
		return NoRole
	}
	var member struct{ Role string }
	if err := db.Table("team_members").Select("role").
		Where("team_id = ? AND user_id = ?", *ws.TeamID, userID).Limit(1).Scan(&member).Error; err != nil {
		return NoRole
	}
	switch member.Role {
	case "":
		return NoRole
	case "admin", "owner":
		return OwnerRole
	default:
		return EditorRole
	}
}

// qaRowRole: the caller's role on one QA row.
func qaRowRole(db *gorm.DB, userID string, row qaOwned) Role {
	owner, ws := row.QAOwner()
	if owner == userID && userID != "" {
		return OwnerRole
	}
	if ws == nil || *ws == "" {
		return NoRole
	}
	return qaWorkspaceRole(db, *ws, userID)
}

// qaLoad loads a QA row by id into dst and checks the caller may act on it
// with role `need`. It writes the error response (404 when the row is not
// visible, 403 when visible but the role is too low) and returns false.
func qaLoad(db *gorm.DB, c *gin.Context, userID, id string, dst qaOwned, need Role, notFound string) bool {
	if err := db.Where("id = ?", id).First(dst).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": notFound})
		return false
	}
	role := qaRowRole(db, userID, dst)
	if role == NoRole {
		c.JSON(http.StatusNotFound, gin.H{"error": notFound})
		return false
	}
	if !role.AtLeast(need) {
		c.JSON(http.StatusForbidden, gin.H{"error": qaForbidden(need)})
		return false
	}
	return true
}

func qaForbidden(need Role) string {
	if need == OwnerRole {
		return "Only its creator, the workspace owner or a team admin can delete this"
	}
	return "You can only view this workspace's QA items"
}

// qaWorkspaceParam is the requested workspace ("" = personal).
func qaWorkspaceParam(c *gin.Context) string {
	return strings.TrimSpace(c.Query("workspaceId"))
}

// qaListScope scopes a list query to the requested workspace (the caller
// must be able to view it) or to the caller's personal rows.
func qaListScope(db *gorm.DB, c *gin.Context, userID string) (*gorm.DB, bool) {
	ws := qaWorkspaceParam(c)
	if ws == "" {
		return db.Where("user_id = ? AND (workspace_id IS NULL OR workspace_id = '')", userID), true
	}
	if qaWorkspaceRole(db, ws, userID) == NoRole {
		c.JSON(http.StatusNotFound, gin.H{"error": "Workspace not found"})
		return nil, false
	}
	return db.Where("workspace_id = ?", ws), true
}

// qaCreateWorkspace is the workspace a new row goes to (nil = personal);
// the caller must be able to edit there.
func qaCreateWorkspace(db *gorm.DB, c *gin.Context, userID string) (*string, bool) {
	ws := qaWorkspaceParam(c)
	if ws == "" {
		return nil, true
	}
	switch r := qaWorkspaceRole(db, ws, userID); {
	case r == NoRole:
		c.JSON(http.StatusNotFound, gin.H{"error": "Workspace not found"})
		return nil, false
	case !r.AtLeast(EditorRole):
		c.JSON(http.StatusForbidden, gin.H{"error": qaForbidden(EditorRole)})
		return nil, false
	}
	return &ws, true
}

// qaSameScope scopes a query to the rows sharing `row`'s scope: the same
// workspace, or the same owner's personal rows (folder trees, plan runs).
func qaSameScope(db *gorm.DB, row qaOwned) *gorm.DB {
	owner, ws := row.QAOwner()
	if ws != nil && *ws != "" {
		return db.Where("workspace_id = ?", *ws)
	}
	return db.Where("user_id = ? AND (workspace_id IS NULL OR workspace_id = '')", owner)
}
