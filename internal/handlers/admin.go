package handlers

import (
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/config"
	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// AdminHandler exposes the admin-only endpoints behind RequireAdmin.
// Includes a user roster with role management and an aggregate usage
// dashboard. Kept narrow on purpose — anything that would let an admin
// read another user's secrets (request bodies, env vars) needs explicit
// auditing first; we don't surface that here.
type AdminHandler struct {
	db  *gorm.DB
	cfg *config.Config
}

func NewAdminHandler(db *gorm.DB, cfg *config.Config) *AdminHandler {
	return &AdminHandler{db: db, cfg: cfg}
}

// ListUsers returns every account with role + creation time + integration
// state and the user's last activity, for the admin roster. Per-user counts
// (collections, requests, mocks, connections, …) moved to the on-demand
// GET /admin/users/:id/usage so the roster stays cheap on every page load.
type adminUserRow struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"createdAt"`
	// Integration state (two grouped counts over tiny tables).
	HasGitLab     bool `json:"hasGitlab"`
	MCPTokenCount int  `json:"mcpTokenCount"`
	// LastActivityAt is the latest of: app open / login (user row), audited
	// API writes, sent-request history and MCP token use.
	LastActivityAt  *time.Time `json:"lastActivityAt,omitempty"`
	LastLoginAt     *time.Time `json:"lastLoginAt,omitempty"`
	LastSeenAt      *time.Time `json:"lastSeenAt,omitempty"`
	LastSeenVersion string     `json:"lastSeenVersion,omitempty"`
}

func (h *AdminHandler) ListUsers(c *gin.Context) {
	var users []models.User
	if err := h.db.Order("created_at ASC").Find(&users).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load users"})
		return
	}
	if len(users) == 0 {
		c.JSON(http.StatusOK, gin.H{"data": []adminUserRow{}})
		return
	}

	ids := make([]string, 0, len(users))
	for _, u := range users {
		ids = append(ids, u.ID)
	}

	// aggregateBy swallows errors (returns an empty map), so a deployment
	// where a table is absent simply reports zero rather than failing.
	gitlab := h.aggregateBy("user_git_integrations", "user_id", ids)
	mcp := h.aggregateBy("mcp_tokens", "user_id", ids)
	activity := h.lastActivityByUser()

	out := make([]adminUserRow, 0, len(users))
	for _, u := range users {
		out = append(out, adminUserRow{
			ID:              u.ID,
			Email:           u.Email,
			Name:            u.Name,
			Role:            roleOrDefault(u.Role),
			CreatedAt:       u.CreatedAt,
			HasGitLab:       gitlab[u.ID] > 0,
			MCPTokenCount:   mcp[u.ID],
			LastActivityAt:  laterOf(laterOf(u.LastSeenAt, u.LastLoginAt), activity[u.ID]),
			LastLoginAt:     u.LastLoginAt,
			LastSeenAt:      u.LastSeenAt,
			LastSeenVersion: u.LastSeenVersion,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// SetRole flips a user's role between 'user' and 'admin'. The default-
// admin row (DEFAULT_ADMIN_EMAIL) is hard-protected from demotion so
// the system can't be left without any admin.
type setRoleInput struct {
	Role string `json:"role" binding:"required"`
}

func (h *AdminHandler) SetRole(c *gin.Context) {
	id := c.Param("id")
	var input setRoleInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	role := strings.ToLower(strings.TrimSpace(input.Role))
	if role != models.RoleUser && role != models.RoleAdmin {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Role must be 'user' or 'admin'"})
		return
	}

	var target models.User
	if err := h.db.First(&target, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}
	if h.cfg.IsDefaultAdmin(target.Email) && role != models.RoleAdmin {
		c.JSON(http.StatusForbidden, gin.H{"error": "The default admin cannot be demoted"})
		return
	}
	if err := h.db.Model(&target).UpdateColumn("role", role).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update role"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": target.ID, "role": role}})
}

// ResetPassword lets an admin set a new password for any account. The
// admin can either type a chosen password or have the client generate a
// random one and POST it here — the server only sees the final value.
// Same length floor as registration (>= 6) to avoid trivially weak
// resets. The default-admin row is protected: only the default admin
// itself can reset its own password through this endpoint.
type resetPasswordInput struct {
	Password string `json:"password" binding:"required,min=6"`
}

func (h *AdminHandler) ResetPassword(c *gin.Context) {
	id := c.Param("id")
	var input resetPasswordInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var target models.User
	if err := h.db.First(&target, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}

	// Default admin's password can only be reset by the default admin
	// itself — otherwise any admin could lock the bootstrap account out.
	if h.cfg.IsDefaultAdmin(target.Email) {
		actorID := c.GetString("userID")
		if actorID != target.ID {
			c.JSON(http.StatusForbidden, gin.H{"error": "Only the default admin can reset their own password"})
			return
		}
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to hash password"})
		return
	}
	if err := h.db.Model(&target).UpdateColumn("password", string(hashed)).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update password"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": target.ID}})
}

// Stats returns aggregate system usage. Cheap counts; no per-row data so
// the dashboard can render fast even on a busy server.
type adminStats struct {
	UserCount           int64 `json:"userCount"`
	CollectionCount     int64 `json:"collectionCount"`
	RequestCount        int64 `json:"requestCount"`
	MockServerCount     int64 `json:"mockServerCount"`
	MockRouteCount      int64 `json:"mockRouteCount"`
	WorkspaceCount      int64 `json:"workspaceCount"`
	HistoryCount        int64 `json:"historyCount"`
	TestCaseCount       int64 `json:"testCaseCount"`
	GitIntegrationCount int64 `json:"gitIntegrationCount"`
	NewUsers7d          int64 `json:"newUsers7d"`
	NewUsers30d         int64 `json:"newUsers30d"`
}

func (h *AdminHandler) Stats(c *gin.Context) {
	var s adminStats
	// Every Count gets its own Session so accumulated conditions from a
	// prior request (or a prior Count in this method) don't leak. Without
	// this guard, the WHERE attached to NewUsers7d/30d sticks to h.db
	// and bleeds into the next request's counts, making every table's
	// total collapse to the same filtered number. Mirrors the same
	// pattern documented in handlers/collection.go:107.
	fresh := func() *gorm.DB { return h.db.Session(&gorm.Session{}) }
	fresh().Model(&models.User{}).Count(&s.UserCount)
	fresh().Model(&models.Collection{}).Count(&s.CollectionCount)
	fresh().Model(&models.SavedRequest{}).Count(&s.RequestCount)
	fresh().Model(&models.MockServer{}).Count(&s.MockServerCount)
	fresh().Model(&models.MockRoute{}).Count(&s.MockRouteCount)
	fresh().Model(&models.Workspace{}).Count(&s.WorkspaceCount)
	fresh().Model(&models.History{}).Count(&s.HistoryCount)
	fresh().Model(&models.TestCase{}).Count(&s.TestCaseCount)
	fresh().Model(&models.UserGitIntegration{}).Count(&s.GitIntegrationCount)
	fresh().Model(&models.User{}).Where("created_at > ?", time.Now().AddDate(0, 0, -7)).Count(&s.NewUsers7d)
	fresh().Model(&models.User{}).Where("created_at > ?", time.Now().AddDate(0, 0, -30)).Count(&s.NewUsers30d)
	c.JSON(http.StatusOK, gin.H{"data": s})
}

// ----------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------

func (h *AdminHandler) aggregateBy(table, column string, userIDs []string) map[string]int {
	type row struct {
		UID string
		N   int64
	}
	var rows []row
	// Session() guards against condition leak between the aggregateBy
	// calls in ListUsers (user_git_integrations, mcp_tokens).
	// Without it, a Where from the previous call sticks to h.db and
	// every subsequent GROUP BY returns the same numbers.
	h.db.Session(&gorm.Session{}).Table(table).
		Select(column+" AS uid, COUNT(*) AS n").
		Where(column+" IN ?", userIDs).
		Group(column).
		Scan(&rows)
	out := make(map[string]int, len(rows))
	for _, r := range rows {
		out[r.UID] = int(r.N)
	}
	return out
}

func roleOrDefault(r string) string {
	if r == models.RoleAdmin {
		return models.RoleAdmin
	}
	return models.RoleUser
}

// ReassignCollection lets an admin transfer ownership of a collection
// (and every nested folder/request under it) to another user. Lifted
// out of the regular collection routes because changing ownership
// crosses the "you can only touch your own data" line that the rest
// of the API enforces — it lives behind RequireAdmin instead.
//
// Designed for the rare "user left the company / lost their account"
// recovery case from the Teams + Public Sharing RFC (open question #8):
// rather than leaking ghost ownership of shared/public collections, an
// admin can stamp a new owner and the rest of the access model (public
// grants, individual shares) keeps working.
type reassignCollectionInput struct {
	NewOwnerID    string `json:"newOwnerId" binding:"required"`
	NewOwnerEmail string `json:"newOwnerEmail"` // optional convenience; falls back to NewOwnerID
}

func (h *AdminHandler) ReassignCollection(c *gin.Context) {
	collectionID := c.Param("id")

	var input reassignCollectionInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Verify the target user exists. Accept either id or email as input
	// — the FE picker passes id, but we leave email as a fallback so
	// curl/admin scripts can use a stable handle.
	var newOwner models.User
	q := h.db.Model(&models.User{})
	if input.NewOwnerEmail != "" {
		q = q.Where("LOWER(email) = LOWER(?)", input.NewOwnerEmail)
	} else {
		q = q.Where("id = ?", input.NewOwnerID)
	}
	if err := q.First(&newOwner).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Target user not found"})
		return
	}

	// Verify the collection exists. Recursively walk down so nested
	// folders inherit the new owner — without this the children stay
	// linked to the original user_id and become unmanageable.
	var root models.Collection
	if err := h.db.First(&root, "id = ?", collectionID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Collection not found"})
		return
	}

	tx := h.db.Begin()
	// Collect every descendant id under this root, including the root
	// itself. Done iteratively so we don't hit Go's stack depth on
	// pathologically deep folder trees.
	ids := []string{collectionID}
	frontier := []string{collectionID}
	for len(frontier) > 0 {
		var next []string
		if err := tx.Model(&models.Collection{}).
			Where("parent_id IN ?", frontier).
			Pluck("id", &next).Error; err != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to walk collection tree"})
			return
		}
		if len(next) == 0 {
			break
		}
		ids = append(ids, next...)
		frontier = next
	}

	// Stamp the new owner on the collections AND every saved request
	// under them so the existing owner-only handlers still recognize
	// the new owner as the rightful caller.
	if err := tx.Model(&models.Collection{}).
		Where("id IN ?", ids).
		Update("user_id", newOwner.ID).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to reassign collections"})
		return
	}
	if err := tx.Model(&models.SavedRequest{}).
		Where("collection_id IN ?", ids).
		Update("user_id", newOwner.ID).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to reassign requests"})
		return
	}

	// Update the owner editor grant on the root if public (otherwise the
	// previous owner stays as Editor while the new owner has none).
	if err := tx.Model(&models.CollectionPublicGrant{}).
		Where("collection_id = ? AND role = 'editor' AND subject_kind = 'user' AND subject_id = ?", collectionID, root.UserID).
		Update("subject_id", newOwner.ID).Error; err != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to migrate owner editor grant"})
		return
	}

	if err := tx.Commit().Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to commit reassignment"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"collectionId":        collectionID,
		"newOwnerId":          newOwner.ID,
		"newOwnerEmail":       newOwner.Email,
		"affectedCollections": len(ids),
	}})
}
