package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/realtime"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// notifyCollectionMutated centralises the recipient-resolution + Notify
// pair used by Create/Update. The root id is resolved so a write on a
// nested folder still fans out to the recipients of its top-level
// collection (shares + grants live on the root). The optional gin
// context lets us echo the request's X-Client-Id back into the event
// so the originating tab can suppress its self-echo. Pass nil from
// non-request contexts (cron jobs, background syncs).
func notifyCollectionMutated(db *gorm.DB, c *gin.Context, collectionID, eventType string) {
	root := realtime.RootCollectionID(db, collectionID)
	if root == "" {
		return
	}
	recipients, everyone := realtime.CollectionRecipients(db, root)
	realtime.Notify(db, realtime.Event{
		Type:           eventType,
		CollectionID:   root,
		Recipients:     recipients,
		Everyone:       everyone,
		OriginClientID: originClientID(c),
	})
}

// originClientID is a tiny helper that the inline Notify callsites use
// when they construct the event by hand (delete, share/unshare paths
// that need to capture recipients before the rows go away).
func originClientID(c *gin.Context) string {
	if c == nil {
		return ""
	}
	return c.GetHeader("X-Client-Id")
}

// notifyRT wraps realtime.Notify so the inline-Event callsites pick up
// X-Client-Id without each one repeating the same `OriginClientID:` line.
// Equivalent in behaviour to setting the field directly on the literal —
// kept as a helper so a future change to the origin stamp (e.g. user
// agent, session id) only has to land in one place.
func notifyRT(db *gorm.DB, c *gin.Context, e realtime.Event) {
	if e.OriginClientID == "" {
		e.OriginClientID = originClientID(c)
	}
	realtime.Notify(db, e)
}

type CollectionHandler struct {
	db *gorm.DB
}

func NewCollectionHandler(db *gorm.DB) *CollectionHandler {
	return &CollectionHandler{db: db}
}

type createCollectionInput struct {
	ID          string  `json:"id"`
	Name        string  `json:"name" binding:"required"`
	Description string  `json:"description"`
	ParentID    *string `json:"parentId"`
	Type        string  `json:"type"`
	WorkspaceID string  `json:"workspaceId"`
	// FE-side imports (resolver → parse → persistParsedImport) carry the
	// source URL / type / baseUrl on the create payload so the resulting
	// collection looks identical to one created through the BE-side
	// /api/import/swagger handler. Without these the row gets created but
	// the Update Collection dialog later finds a blank source URL,
	// forcing the user to paste it again on every refresh.
	SourceURL  string `json:"sourceUrl"`
	SourceType string `json:"sourceType"`
	BaseURL    string `json:"baseUrl"`
}

type updateCollectionInput struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	BaseURL     *string `json:"baseUrl"`
	SortOrder   *int    `json:"sortOrder"`
	// WorkspaceID uses *string so "" can be sent explicitly to clear the
	// binding (→ "No Workspace" bucket). nil means "leave as-is".
	WorkspaceID *string `json:"workspaceId"`
	// ParentID lets the FE drag-and-drop a folder into a different parent.
	// "" moves it back to the workspace root (NULL parent_id), nil means
	// "leave as-is". Must point at a collection the user owns or has edit
	// access to (validated below).
	ParentID *string `json:"parentId"`
}

type collectionResponse struct {
	models.Collection
	Shared    bool   `json:"shared"`
	OwnerName string `json:"ownerName,omitempty"`
	// SharedByMe is set on collections the *current user* owns when they
	// have given at least one collaborator access. Renders the shared
	// icon for the owner so they can see at a glance which of their own
	// collections are exposed.
	SharedByMe       bool `json:"sharedByMe,omitempty"`
	CollaboratorCount int  `json:"collaboratorCount,omitempty"`
	// MyRole is the caller's effective role on the collection: 'owner',
	// 'editor', or 'viewer'. Populated by the Public-scope discovery
	// path so the FE can gate edit affordances on non-owned public
	// collections (editors get the 3-dot menu; viewers don't). Stays
	// empty on the legacy private List path where owner-only is implied.
	MyRole string `json:"myRole,omitempty"`
	// CascadedViaWorkspace = true when the caller can see this row
	// because the collection's workspace is bound to a team they're in
	// (read-only workspace-cascade), NOT because of a per-collection
	// share row or grant. The FE uses this to pick a distinct icon +
	// disable "Leave shared collection" — there's no per-user share row
	// to drop, so leaving doesn't apply.
	CascadedViaWorkspace bool `json:"cascadedViaWorkspace,omitempty"`
}

// WorkspaceNone is the sentinel value the FE sends to select the "No
// Workspace" bucket — collections that haven't been bound to any workspace
// (legacy pre-workspace rows, or collections explicitly created with no
// workspace). Keeping it as a magic string avoids adding a second query
// param just for this one case.
const WorkspaceNone = "__none__"

func (h *CollectionHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	// Optional workspace filter.
	//   empty       → no filter (bootstrap / legacy callers)
	//   __none__    → collections with workspace_id NULL or ''
	//   <id>        → strict match on workspace_id
	workspaceID := c.Query("workspaceId")

	// Optional scope filter introduced by the Teams + Public Sharing RFC:
	//   ""        → legacy behavior (own collections + individual shares)
	//   "private" → same as legacy (kept for FE clarity)
	//   "public"  → collections the user can see via public grants
	//               (visibility='public' AND effective_role >= viewer)
	scope := c.Query("scope")
	if scope == "public" {
		h.listPublic(c, userID)
		return
	}

	log.Printf("[collections.List] userID=%s workspaceID=%q", userID, workspaceID)

	// Session(&gorm.Session{}) forces a fresh Statement so no conditions
	// accumulated on h.db from a prior request leak into this query.
	// The visibility-set is "own collections + collections whose
	// workspace is bound to a team I'm in" (workspace-cascade). The
	// legacy CollectionShare collaborator path is intentionally NOT
	// here — those rows surface in the Public list per the 1.15
	// rebalance. The workspace-cascade IS here because looking AT a
	// workspace should show all content in it: a teammate's collection
	// inside our shared workspace is part of "this workspace's view",
	// not the global Public discovery.
	query := h.db.Session(&gorm.Session{}).
		Model(&models.Collection{}).
		Where("parent_id IS NULL").
		Where(
			"user_id = ?"+
				" OR (workspace_id IN ("+
				"   SELECT w.id FROM workspaces w"+
				"   WHERE w.team_id IS NOT NULL"+
				"     AND w.team_id IN (SELECT team_id FROM team_members WHERE user_id = ?)"+
				" ))",
			userID, userID,
		)
	if workspaceID == WorkspaceNone {
		query = query.Where("workspace_id IS NULL OR workspace_id = ?", "")
	} else if workspaceID != "" {
		query = query.Where("workspace_id = ?", workspaceID)
	}
	var ownCollections []models.Collection
	if err := query.
		Preload("Requests", func(db *gorm.DB) *gorm.DB {
			return db.Order("sort_order ASC, created_at ASC")
		}).
		Order("sort_order ASC, created_at ASC").
		Find(&ownCollections).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch collections"})
		return
	}

	log.Printf("[collections.List] matched %d own collections for workspaceID=%q", len(ownCollections), workspaceID)

	for i := range ownCollections {
		h.loadChildren(&ownCollections[i])
	}

	// Build response with own collections first. Bulk-count collaborators
	// on the user's own root collections so the FE can render the shared
	// badge for outgoing shares — one query instead of N.
	ownCollabCounts := map[string]int{}
	if len(ownCollections) > 0 {
		ownIDs := make([]string, 0, len(ownCollections))
		for _, col := range ownCollections {
			ownIDs = append(ownIDs, col.ID)
		}
		type row struct {
			CollectionID string
			Count        int64
		}
		var rows []row
		h.db.Session(&gorm.Session{}).
			Model(&models.CollectionShare{}).
			Select("collection_id, COUNT(*) AS count").
			Where("collection_id IN ? AND user_id IS NOT NULL AND user_id != ''", ownIDs).
			Group("collection_id").
			Find(&rows)
		for _, r := range rows {
			ownCollabCounts[r.CollectionID] = int(r.Count)
		}
	}

	// Build the response. Two row classes coming out of the query above:
	//
	//   - Own collections (col.UserID == userID) — Shared:false, with the
	//     outgoing-collaborator badge if the owner has individual share
	//     rows of their own.
	//   - Workspace-cascade collections (col.UserID != userID) — Shared:
	//     true, OwnerName decorated so the FE can render the "shared
	//     by X" sub-text. These are the collections sitting in a
	//     team-bound workspace I'm a member of; per the 1.16 cascade
	//     they're part of "this workspace's view" so they belong in the
	//     Private accordion when I'm focused on the workspace, NOT only
	//     in the global Public discovery list.
	//
	// Legacy CollectionShare collaborator content still goes through
	// listPublic (1.15 rebalance) — it's not a workspace concept, it's
	// a per-collection access route.
	ownerIDs := make([]string, 0)
	for _, col := range ownCollections {
		if col.UserID != "" && col.UserID != userID {
			ownerIDs = append(ownerIDs, col.UserID)
		}
	}
	ownerNames := map[string]string{}
	if len(ownerIDs) > 0 {
		var rows []struct{ ID, Name, Email string }
		h.db.Session(&gorm.Session{}).Table("users").
			Select("id, name, email").
			Where("id IN ?", ownerIDs).
			Scan(&rows)
		for _, r := range rows {
			label := r.Name
			if label == "" {
				label = r.Email
			}
			ownerNames[r.ID] = label
		}
	}

	result := make([]collectionResponse, 0, len(ownCollections))
	for _, col := range ownCollections {
		if col.UserID == userID {
			count := ownCollabCounts[col.ID]
			result = append(result, collectionResponse{
				Collection:        col,
				Shared:            false,
				SharedByMe:        count > 0,
				CollaboratorCount: count,
			})
		} else {
			// Non-owner row in the Private path always arrives via the
			// workspace-cascade branch — legacy CollectionShare
			// collaborator content moved to Public in 1.15, so the
			// only way a non-owner shows up here is the workspace +
			// team binding cascade. Mark it so the FE can render the
			// workspace-share icon + disable "Leave" (no share row
			// to drop). The role is resolved, not assumed: team admins
			// edit, and a grant on the collection can raise it too.
			result = append(result, collectionResponse{
				Collection:           col,
				Shared:               true,
				OwnerName:            ownerNames[col.UserID],
				MyRole:               string(ResolveCollectionRole(h.db, userID, col.ID)),
				CascadedViaWorkspace: true,
			})
		}
	}

	c.JSON(http.StatusOK, gin.H{"data": result})
}

// listPublic answers the Public accordion's discovery query. Three
// origins of access fold into one list:
//
//  1. Owner of a public-visibility collection — appears here AND in the
//     Private list (single source of truth, two listings).
//  2. Public-grant viewer/editor (user, team membership, or 'everyone')
//     on a visibility='public' collection.
//  3. Legacy CollectionShare collaborator — regardless of visibility.
//     Share-code joins go in here so the recipient sees their shared
//     collection in Public (their own Private stays a "mine only" view).
//
// All three branches union into a single id set; downstream loadChildren
// + role stamping work uniformly.
func (h *CollectionHandler) listPublic(c *gin.Context, userID string) {
	var rootIDs []string
	err := h.db.Session(&gorm.Session{}).
		Table("collections").
		Select("collections.id").
		Where("collections.parent_id IS NULL").
		Where(
			// Branch 1: own + visibility='public' (owner copy in Public).
			"(collections.user_id = ? AND collections.visibility = 'public')"+
				// Branch 2: visibility='public' AND a matching public grant.
				" OR (collections.visibility = 'public' AND EXISTS ("+
				"   SELECT 1 FROM collection_public_grants g"+
				"   WHERE g.collection_id = collections.id"+
				"     AND ( (g.subject_kind = 'user' AND g.subject_id = ?)"+
				"        OR (g.subject_kind = 'team' AND g.subject_id IN ("+
				"             SELECT tm.team_id FROM team_members tm WHERE tm.user_id = ?"+
				"           ))"+
				"        OR (g.subject_kind = 'everyone') ) ))"+
				// Branch 3: legacy CollectionShare collaborator (any visibility,
				// excludes the share-link template rows where user_id IS NULL).
				" OR EXISTS ("+
				"   SELECT 1 FROM collection_shares s"+
				"   WHERE s.collection_id = collections.id"+
				"     AND s.user_id = ?"+
				" )",
			// Workspace-team cascade is intentionally NOT a branch here.
			// Cascade is a workspace-scoped access — the user reaches
			// it by being IN the shared workspace, not by discovering
			// it from the global Public list. Putting it in both lists
			// double-counted: the same private-visibility collection
			// would appear under Public AND Private for every team
			// member just because the workspace happened to be team-
			// bound. Cascade rows surface in Collection.List's Private
			// path (workspaceId-filtered there) where they belong.
			userID, userID, userID, userID).
		Order("collections.created_at ASC").
		Pluck("collections.id", &rootIDs).Error
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch public collections"})
		return
	}

	if len(rootIDs) == 0 {
		c.JSON(http.StatusOK, gin.H{"data": []collectionResponse{}})
		return
	}

	var cols []models.Collection
	if err := h.db.Session(&gorm.Session{}).
		Preload("Requests", func(db *gorm.DB) *gorm.DB {
			return db.Order("sort_order ASC, created_at ASC")
		}).
		Where("id IN ?", rootIDs).
		Order("created_at ASC").
		Find(&cols).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch public collections"})
		return
	}
	for i := range cols {
		h.loadChildren(&cols[i])
	}

	// Fetch owner display names in one batch so the FE can render
	// "shared by <owner>" sub-text.
	ownerIDs := make([]string, 0, len(cols))
	for _, col := range cols {
		ownerIDs = append(ownerIDs, col.UserID)
	}
	owners := map[string]string{}
	if len(ownerIDs) > 0 {
		var rows []struct{ ID, Name, Email string }
		h.db.Session(&gorm.Session{}).Table("users").
			Select("id, name, email").
			Where("id IN ?", ownerIDs).
			Scan(&rows)
		for _, r := range rows {
			label := r.Name
			if label == "" {
				label = r.Email
			}
			owners[r.ID] = label
		}
	}

	result := make([]collectionResponse, 0, len(cols))
	for _, col := range cols {
		role := string(ResolveCollectionRole(h.db, userID, col.ID))
		result = append(result, collectionResponse{
			Collection: col,
			Shared:     col.UserID != userID,
			OwnerName:  owners[col.UserID],
			MyRole:     role,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": result})
}

// defaultWorkspaceID returns the oldest workspace the user owns, or an
// empty string if they have none. New collections fall back to this when
// the caller didn't provide an explicit workspaceId so rows never end up
// orphaned. The FE's workspace bootstrap guarantees a workspace exists
// before the user can create a collection, so the empty case is only a
// safety net (not a normal path). Post-RFC, workspaces are owner-only,
// so this is a direct owner_id filter (no team_members join).
func (h *CollectionHandler) defaultWorkspaceID(userID string) string {
	var ws models.Workspace
	err := h.db.
		Where("owner_id = ?", userID).
		Order("created_at ASC").
		First(&ws).Error
	if err != nil {
		return ""
	}
	return ws.ID
}

func (h *CollectionHandler) loadChildren(collection *models.Collection) {
	var children []models.Collection
	h.db.Where("parent_id = ?", collection.ID).
		Order("sort_order ASC, created_at ASC").
		Find(&children)
	collection.Children = children

	// Load requests for this collection
	var requests []models.SavedRequest
	h.db.Where("collection_id = ?", collection.ID).
		Order("sort_order ASC, created_at ASC").
		Find(&requests)
	collection.Requests = requests

	for i := range children {
		h.loadChildren(&children[i])
	}
}

func (h *CollectionHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")

	var input createCollectionInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	collType := input.Type
	if collType == "" {
		collType = "collection"
	}

	newID, ok := pickID(input.ID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid id format"})
		return
	}

	// Child collections inherit their parent's workspace so trees stay
	// coherent. Root collections take the explicit workspaceId from the
	// request; the __none__ sentinel means "create as an orphan" so the
	// row shows up under the FE's "No Workspace" bucket.
	workspaceID := input.WorkspaceID
	if input.ParentID != nil && *input.ParentID != "" {
		var parent models.Collection
		if err := h.db.Select("workspace_id").First(&parent, "id = ?", *input.ParentID).Error; err == nil {
			workspaceID = parent.WorkspaceID
		}
	}
	if workspaceID == WorkspaceNone {
		workspaceID = ""
	} else if workspaceID == "" {
		// Last-resort fallback for callers that didn't pick a workspace:
		// stamp the user's oldest workspace so the row is still reachable.
		workspaceID = h.defaultWorkspaceID(userID)
	}

	collection := models.Collection{
		ID:          newID,
		UserID:      userID,
		WorkspaceID: workspaceID,
		Name:        input.Name,
		Description: input.Description,
		ParentID:    input.ParentID,
		Type:        collType,
		SourceURL:   input.SourceURL,
		SourceType:  input.SourceType,
		BaseURL:     input.BaseURL,
	}
	if input.SourceURL != "" {
		now := time.Now()
		collection.LastSynced = &now
	}

	if err := h.db.Create(&collection).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create collection"})
		return
	}

	notifyCollectionMutated(h.db, c, collection.ID, realtime.EventCollectionCreated)

	c.JSON(http.StatusCreated, gin.H{"data": collection})
}

func (h *CollectionHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var collection models.Collection
	if err := h.db.First(&collection, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Collection not found"})
		return
	}
	if !canEditCollection(h.db, id, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You need editor access to update this collection"})
		return
	}

	var input updateCollectionInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	updates := map[string]interface{}{}
	if input.Name != "" {
		updates["name"] = input.Name
	}
	if input.Description != "" {
		updates["description"] = input.Description
	}
	if input.BaseURL != nil {
		updates["base_url"] = *input.BaseURL
	}
	if input.SortOrder != nil {
		updates["sort_order"] = *input.SortOrder
	}
	if input.WorkspaceID != nil {
		updates["workspace_id"] = *input.WorkspaceID
	}
	if input.ParentID != nil {
		newParent := *input.ParentID
		if newParent == "" {
			// Move to root — clear parent_id so the row appears as a top
			// level collection again.
			updates["parent_id"] = gorm.Expr("NULL")
		} else {
			// Validate the new parent exists, the user can *edit* it, and
			// it's not a descendant of the row being moved (no cycles).
			if !canEditCollection(h.db, newParent, userID) {
				c.JSON(http.StatusForbidden, gin.H{"error": "You need editor access to the target parent"})
				return
			}
			if newParent == id || isDescendantOf(h.db, newParent, id) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "cannot move a collection into itself or its descendants"})
				return
			}
			updates["parent_id"] = newParent
			// Inherit the new parent's workspace so the move doesn't leave
			// the row stranded under the wrong workspace view.
			var parent models.Collection
			if err := h.db.Select("workspace_id").First(&parent, "id = ?", newParent).Error; err == nil {
				updates["workspace_id"] = parent.WorkspaceID
			}
		}
	}

	if err := h.db.Model(&collection).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update collection"})
		return
	}

	// When workspace changes, cascade to every descendant so the whole tree
	// stays in the same workspace. Without this, child folders/collections
	// would vanish from the new workspace view (they still carry the old
	// workspace_id) and appear stranded under the old one.
	if input.WorkspaceID != nil {
		h.cascadeWorkspace(id, *input.WorkspaceID)
	}

	h.db.First(&collection, "id = ?", id)
	notifyCollectionMutated(h.db, c, collection.ID, realtime.EventCollectionUpdated)
	c.JSON(http.StatusOK, gin.H{"data": collection})
}

// isDescendantOf reports whether `nodeID` lives anywhere under `ancestorID`
// in the collection tree. Used to refuse self-move loops in Update.
func isDescendantOf(db *gorm.DB, nodeID, ancestorID string) bool {
	if nodeID == ancestorID {
		return true
	}
	current := nodeID
	// Bounded walk — collection trees in practice never get deeper than a
	// handful of levels; 64 is a generous sanity cap that stops at any
	// pathological cycle in the data.
	for i := 0; i < 64; i++ {
		var col models.Collection
		if err := db.Select("parent_id").First(&col, "id = ?", current).Error; err != nil {
			return false
		}
		if col.ParentID == nil {
			return false
		}
		if *col.ParentID == ancestorID {
			return true
		}
		current = *col.ParentID
	}
	return false
}

// cascadeWorkspace walks the collection tree under parentID and rewrites
// workspace_id on every descendant. Recursive instead of a single recursive
// CTE so the same code works on sqlite (embedded) and postgres (hosted)
// without driver-specific SQL.
func (h *CollectionHandler) cascadeWorkspace(parentID, workspaceID string) {
	var children []models.Collection
	h.db.Session(&gorm.Session{}).Where("parent_id = ?", parentID).Find(&children)
	for _, child := range children {
		h.db.Model(&models.Collection{}).Where("id = ?", child.ID).Update("workspace_id", workspaceID)
		h.cascadeWorkspace(child.ID, workspaceID)
	}
}

func (h *CollectionHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var collection models.Collection
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&collection).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Collection not found"})
		return
	}

	// Capture recipients BEFORE the rows go away — once the collection
	// + shares + grants are deleted, recipient resolution returns empty
	// and nobody hears that their share was revoked.
	recipients, everyone := realtime.CollectionRecipients(h.db, id)

	// Saved responses: a folder's move up to its parent; a whole
	// collection's go with it (no orphans either way).
	rehomeSavedResponsesOnDelete(h.db, collection)

	// Delete all requests in this collection
	h.db.Where("collection_id = ?", id).Delete(&models.SavedRequest{})

	// Delete child collections and their requests recursively
	h.deleteChildren(id)

	if err := h.db.Delete(&collection).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete collection"})
		return
	}

	notifyRT(h.db, c, realtime.Event{
		Type:         realtime.EventCollectionDeleted,
		CollectionID: id,
		Recipients:   recipients,
		Everyone:     everyone,
	})

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Collection deleted"}})
}

func (h *CollectionHandler) deleteChildren(parentID string) {
	var children []models.Collection
	h.db.Where("parent_id = ?", parentID).Find(&children)
	for _, child := range children {
		h.db.Where("collection_id = ?", child.ID).Delete(&models.SavedRequest{})
		h.deleteChildren(child.ID)
		h.db.Delete(&child)
	}
}

func (h *CollectionHandler) ListRequests(c *gin.Context) {
	userID := c.GetString("userID")
	collectionID := c.Param("id")

	if !canAccessCollection(h.db, collectionID, userID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Collection not found"})
		return
	}

	var requests []models.SavedRequest
	if err := h.db.Where("collection_id = ?", collectionID).
		Order("sort_order ASC, created_at ASC").
		Find(&requests).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch requests"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": requests})
}

// Export exports a collection with all its requests as JSON
func (h *CollectionHandler) Export(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	if !canAccessCollection(h.db, id, userID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Collection not found"})
		return
	}

	var collection models.Collection
	if err := h.db.First(&collection, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Collection not found"})
		return
	}

	var requests []models.SavedRequest
	h.db.Where("collection_id = ?", id).Order("sort_order ASC, created_at ASC").Find(&requests)

	// Load children recursively
	h.loadChildren(&collection)

	type exportData struct {
		Collection models.Collection    `json:"collection"`
		Requests   []models.SavedRequest `json:"requests"`
		Version    string                `json:"version"`
	}

	c.JSON(http.StatusOK, gin.H{
		"data": exportData{
			Collection: collection,
			Requests:   requests,
			Version:    "1.0",
		},
	})
}

// Import imports a collection from JSON export
func (h *CollectionHandler) Import(c *gin.Context) {
	userID := c.GetString("userID")

	type importData struct {
		Collection struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Type        string `json:"type"`
		} `json:"collection"`
		Requests []struct {
			Name             string `json:"name"`
			Method           string `json:"method"`
			URL              string `json:"url"`
			Headers          string `json:"headers"`
			Params           string `json:"params"`
			Body             string `json:"body"`
			Auth             string `json:"auth"`
			PreRequestScript string `json:"preRequestScript"`
			TestScript       string `json:"testScript"`
			SortOrder        int    `json:"sortOrder"`
		} `json:"requests"`
	}

	var input importData
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	collType := input.Collection.Type
	if collType == "" {
		collType = "collection"
	}

	collectionID := uuid.New().String()
	collection := models.Collection{
		ID:          collectionID,
		UserID:      userID,
		Name:        input.Collection.Name,
		Description: input.Collection.Description,
		Type:        collType,
	}

	if err := h.db.Create(&collection).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create collection"})
		return
	}

	for _, r := range input.Requests {
		method := r.Method
		if method == "" {
			method = "GET"
		}

		req := models.SavedRequest{
			ID:               uuid.New().String(),
			CollectionID:     collectionID,
			Name:             r.Name,
			Method:           method,
			URL:              r.URL,
			Headers:          r.Headers,
			Params:           r.Params,
			Body:             r.Body,
			Auth:             r.Auth,
			PreRequestScript: r.PreRequestScript,
			TestScript:       r.TestScript,
			SortOrder:        r.SortOrder,
		}
		h.db.Create(&req)
	}

	notifyCollectionMutated(h.db, c, collection.ID, realtime.EventCollectionCreated)

	c.JSON(http.StatusCreated, gin.H{"data": collection})
}

// Sync updates a collection's requests from a source (URL, JSON body, or Postman file)
func (h *CollectionHandler) Sync(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	if !canEditCollection(h.db, id, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You need editor access to sync this collection"})
		return
	}

	var collection models.Collection
	if err := h.db.First(&collection, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Collection not found"})
		return
	}

	mode := c.Query("mode") // refresh, url, json, postman
	if mode == "" {
		mode = "refresh"
	}

	var specBytes []byte
	var sourceURL string
	var sourceType string

	switch mode {
	case "refresh":
		// Re-fetch from stored source URL — through the resolver so a
		// Scalar / Swagger UI / Redoc page that the user pasted on the
		// initial import still works on refresh. Doing a raw http.Get
		// here used to surface "invalid character '<'" because the HTML
		// landing page came back instead of the underlying JSON.
		if collection.SourceURL == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Collection has no source URL to refresh from"})
			return
		}
		body, _, _, err := ResolveOpenAPISpec(collection.SourceURL)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Failed to refresh from source: %v", err)})
			return
		}
		specBytes = body
		sourceURL = collection.SourceURL
		sourceType = collection.SourceType

	case "url":
		// Fetch from a new URL — same resolver path as refresh so a
		// Scalar / Swagger UI / Redoc URL is supported on first sync
		// too, not just on initial import.
		var input struct {
			URL string `json:"url" binding:"required"`
		}
		if err := c.ShouldBindJSON(&input); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "URL is required"})
			return
		}
		body, _, _, err := ResolveOpenAPISpec(input.URL)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Failed to fetch URL: %v", err)})
			return
		}
		specBytes = body
		sourceURL = input.URL
		sourceType = "swagger"

	case "json":
		// Raw JSON body with OpenAPI spec
		var err error
		specBytes, err = io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read request body"})
			return
		}
		sourceType = "swagger"
		// Optional ?sourceUrl=... lets the FE record where this JSON
		// came from (e.g. the user pasted a docs URL that the FE
		// resolved to JSON before sending). The dialog reads this back
		// to pre-fill the URL field on next open.
		if u := c.Query("sourceUrl"); u != "" {
			sourceURL = u
		}

	case "postman":
		// Postman collection JSON (file upload or raw body)
		contentType := c.ContentType()
		if strings.Contains(contentType, "multipart/form-data") {
			file, _, fileErr := c.Request.FormFile("file")
			if fileErr != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "No file provided"})
				return
			}
			defer file.Close()
			specBytes, _ = io.ReadAll(file)
		} else {
			specBytes, _ = io.ReadAll(c.Request.Body)
		}
		sourceType = "postman"
		if u := c.Query("sourceUrl"); u != "" {
			sourceURL = u
		}

	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid mode. Use: refresh, url, json, postman"})
		return
	}

	// Delete existing requests and children. Saved responses are re-attached
	// to the rebuilt tree (same folder path / request name + method) on every
	// way out of this handler, parse errors included.
	anchors := snapshotSavedResponses(h.db, id)
	defer restoreSavedResponses(h.db, id, anchors)
	h.deleteCollectionRequests(id)
	h.deleteCollectionChildren(id)

	now := time.Now()
	requestCount := 0

	if sourceType == "postman" {
		// Parse as Postman collection
		var postmanColl postmanCollection
		if err := json.Unmarshal(specBytes, &postmanColl); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Failed to parse Postman collection: %v", err)})
			return
		}

		importH := &ImportHandler{db: h.db}
		importH.processPostmanItems(postmanColl.Item, id, userID, &requestCount)

		if postmanColl.Info.Name != "" && mode != "refresh" {
			h.db.Model(&collection).Update("name", postmanColl.Info.Name)
		}
	} else {
		// Parse as OpenAPI/Swagger
		var spec openAPISpec
		if err := json.Unmarshal(specBytes, &spec); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Failed to parse OpenAPI spec: %v", err)})
			return
		}

		var specBaseURL string
		requestCount, specBaseURL = h.syncFromSwaggerSpec(spec, id)

		if spec.Info.Title != "" && mode != "refresh" {
			h.db.Model(&collection).Update("name", spec.Info.Title)
		}
		if specBaseURL != "" {
			h.db.Model(&collection).Update("base_url", specBaseURL)
		}
	}

	// Update collection metadata
	updates := map[string]interface{}{
		"last_synced": now,
		"source_type": sourceType,
	}
	if sourceURL != "" {
		updates["source_url"] = sourceURL
	}
	h.db.Model(&collection).Updates(updates)

	// Reload collection
	h.db.Preload("Requests", func(db *gorm.DB) *gorm.DB {
		return db.Order("sort_order ASC, created_at ASC")
	}).First(&collection, "id = ?", id)
	h.loadChildren(&collection)

	// Sync wipes + re-imports the entire tree, so every recipient needs
	// to drop their cache and refetch. Use the standard root-id resolver
	// + recipient set so collaborators on a shared collection see the
	// fresh tree without manual refresh.
	notifyCollectionMutated(h.db, c, id, realtime.EventCollectionUpdated)

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"collection": collection,
			"count":      requestCount,
		},
	})
}

// syncFromSwaggerSpec creates requests from an OpenAPI spec and returns (count, baseURL)
func (h *CollectionHandler) syncFromSwaggerSpec(spec openAPISpec, collectionID string) (int, string) {
	count := buildRequestsGroupedByTag(h.db, spec, collectionID)
	return count, spec.baseURL()
}

// buildRequestsGroupedByTag mirrors the spec's tag layout into PatchMan
// folders. Modern OpenAPI specs (Swashbuckle, NSwag, FastAPI, NestJS,
// etc.) don't carry a directory structure — every operation lists its
// group via `tags[]`, and the rendered docs (Swagger UI / Redoc / Scalar)
// show side-nav buckets per tag. A flat import flattens that hierarchy
// into a 200-row list of operations and the user has to scroll past
// unrelated routes to find anything.
//
// Behaviour:
//   - Operations with at least one tag go under a folder named for the
//     first tag. Folders are created on demand and reused.
//   - Untagged operations stay at the root of the collection (no folder),
//     so a tiny spec with three endpoints still imports cleanly.
//   - Folders are sort-ordered alphabetically by tag name; the
//     untagged-root requests come first so they're still discoverable.
//   - Within a folder, operations land in path-then-method order so
//     re-syncing the same spec twice produces the same on-screen layout.
//
// Shared by ImportSwagger (new collection) and Sync (refresh existing).
func buildRequestsGroupedByTag(db *gorm.DB, spec openAPISpec, rootCollectionID string) int {
	type opEntry struct {
		path       string
		method     string
		op         openAPIOp
		tag        string // "" → keep at root
		sortBucket int    // 0 = root, >0 = per-folder so untagged comes first
	}

	// 1. Walk every (path, method, op) triple once and bucket by tag.
	pathKeys := make([]string, 0, len(spec.Paths))
	for p := range spec.Paths {
		pathKeys = append(pathKeys, p)
	}
	sort.Strings(pathKeys)

	// Stable method order so re-syncing produces the same UI.
	methodOrder := []string{"get", "post", "put", "patch", "delete", "head", "options", "trace"}
	methodRank := map[string]int{}
	for i, m := range methodOrder {
		methodRank[m] = i
	}

	tagSet := map[string]struct{}{}
	var entries []opEntry
	for _, p := range pathKeys {
		methods := spec.Paths[p]
		methodKeys := make([]string, 0, len(methods))
		for m := range methods {
			methodKeys = append(methodKeys, strings.ToLower(m))
		}
		sort.SliceStable(methodKeys, func(i, j int) bool {
			ri, oi := methodRank[methodKeys[i]]
			rj, oj := methodRank[methodKeys[j]]
			if oi && oj {
				return ri < rj
			}
			return methodKeys[i] < methodKeys[j]
		})
		for _, m := range methodKeys {
			methodUpper := strings.ToUpper(m)
			if methodUpper == "PARAMETERS" || methodUpper == "SERVERS" {
				continue
			}
			op := methods[m]
			tag := ""
			if len(op.Tags) > 0 {
				tag = strings.TrimSpace(op.Tags[0])
			}
			if tag != "" {
				tagSet[tag] = struct{}{}
			}
			entries = append(entries, opEntry{path: p, method: methodUpper, op: op, tag: tag})
		}
	}

	// 2. Create one folder per distinct tag (alphabetical for stability).
	tags := make([]string, 0, len(tagSet))
	for t := range tagSet {
		tags = append(tags, t)
	}
	sort.Strings(tags)

	folderID := map[string]string{}
	folderSort := 1 // leave 0 for the untagged-root group
	now := time.Now()
	for _, t := range tags {
		id := uuid.New().String()
		parent := rootCollectionID
		folder := models.Collection{
			ID:        id,
			Name:      t,
			Type:      "folder",
			ParentID:  &parent,
			SortOrder: folderSort,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := db.Create(&folder).Error; err == nil {
			folderID[t] = id
		}
		folderSort++
	}

	// 3. Materialise the requests under their resolved parent (root or
	// folder). Untagged requests sit at the root; we run them first so
	// the user sees them before scrolling past the tagged folders.
	count := 0
	rootSort := 0
	folderSortCounters := map[string]int{}
	for _, e := range entries {
		parentID := rootCollectionID
		var sortOrder int
		if e.tag != "" {
			if id, ok := folderID[e.tag]; ok {
				parentID = id
				sortOrder = folderSortCounters[e.tag]
				folderSortCounters[e.tag]++
			} else {
				sortOrder = rootSort
				rootSort++
			}
		} else {
			sortOrder = rootSort
			rootSort++
		}

		name := e.op.Summary
		if name == "" {
			name = e.op.OperationID
		}
		if name == "" {
			name = fmt.Sprintf("%s %s", e.method, e.path)
		}

		headers := make([]map[string]interface{}, 0)
		params := make([]map[string]interface{}, 0)
		for _, p := range e.op.Parameters {
			entry := map[string]interface{}{
				"key":     p.Name,
				"value":   "",
				"enabled": p.Required,
			}
			if p.In == "header" {
				headers = append(headers, entry)
			} else if p.In == "query" {
				params = append(params, entry)
			}
		}

		headersJSON, _ := json.Marshal(headers)
		paramsJSON, _ := json.Marshal(params)
		bodyJSON, _ := json.Marshal(map[string]string{"type": "none", "content": ""})
		authJSON, _ := json.Marshal(map[string]string{"type": "none"})

		req := models.SavedRequest{
			ID:           uuid.New().String(),
			CollectionID: parentID,
			Name:         name,
			Method:       e.method,
			URL:          e.path,
			Headers:      string(headersJSON),
			Params:       string(paramsJSON),
			Body:         string(bodyJSON),
			Auth:         string(authJSON),
			SortOrder:    sortOrder,
		}
		db.Create(&req)
		count++
	}
	return count
}

// deleteCollectionRequests removes all requests belonging to a collection
func (h *CollectionHandler) deleteCollectionRequests(collectionID string) {
	h.db.Where("collection_id = ?", collectionID).Delete(&models.SavedRequest{})
}

// deleteCollectionChildren removes all child collections and their requests recursively
func (h *CollectionHandler) deleteCollectionChildren(collectionID string) {
	var children []models.Collection
	h.db.Where("parent_id = ?", collectionID).Find(&children)
	for _, child := range children {
		h.deleteCollectionRequests(child.ID)
		h.deleteCollectionChildren(child.ID)
		h.db.Delete(&child)
	}
}
