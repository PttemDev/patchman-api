package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// AccessDebugHandler exposes a read-only diagnostic endpoint so a user
// who's seeing an unexpected "you need editor access" 403 can pull the
// resolver's intermediate state for the affected collection or env from
// the BE. Auth'd routes (caller must be signed in) and scoped to the
// caller's own userID — there's no impersonation surface here.
//
// Routes (registered in cmd/server/main.go):
//
//	GET /api/debug/role/collection/:id
//	GET /api/debug/role/environment/:id
//
// Response shape:
//
//	{
//	  "data": {
//	    "resolved": "editor" | "viewer" | "owner" | "",
//	    "rootId": "<root collection id>",
//	    "requestedId": "<id from the URL>",
//	    "sources": {
//	      "isOwner": false,
//	      "legacyShare": "viewer",          // empty when no legacy row
//	      "publicGrant": "editor",          // empty when no public grant
//	      "workspaceTeam": false,
//	      "visibility": "public" | "private",
//	      "workspaceId": "...",
//	      "resolvedRootId": "..."
//	    }
//	  }
//	}
type AccessDebugHandler struct {
	db *gorm.DB
}

func NewAccessDebugHandler(db *gorm.DB) *AccessDebugHandler {
	return &AccessDebugHandler{db: db}
}

func (h *AccessDebugHandler) Collection(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	rootID := rootCollectionID(h.db, id)
	role, src := ResolveCollectionRoleDetailed(h.db, userID, rootID)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"resolved":    role,
		"rootId":      rootID,
		"requestedId": id,
		"userId":      userID,
		"sources":     src,
	}})
}

func (h *AccessDebugHandler) Environment(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	role, src := ResolveEnvironmentRoleDetailed(h.db, userID, id)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"resolved":    role,
		"requestedId": id,
		"userId":      userID,
		"sources":     src,
	}})
}
