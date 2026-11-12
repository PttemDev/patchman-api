package handlers

import (
	"net/http"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type SyncHandler struct {
	db *gorm.DB
}

func NewSyncHandler(db *gorm.DB) *SyncHandler {
	return &SyncHandler{db: db}
}

// initialPushInput is the payload a client sends right after registering from
// anonymous-local mode. Every entity retains its client-generated UUID so
// foreign keys stay intact. Server forces UserID to the authed user.
type initialPushInput struct {
	Collections  []models.Collection   `json:"collections"`
	Requests     []models.SavedRequest `json:"requests"`
	Environments []models.Environment  `json:"environments"`
	Cookies      []models.CookieJar    `json:"cookies"`
	TestCases    []models.TestCase     `json:"testCases"`
	TestPlans    []models.TestPlan     `json:"testPlans"`
	Defects      []models.Defect       `json:"defects"`
	Requirements []models.Requirement  `json:"requirements"`
	SharedSteps  []models.SharedStep   `json:"sharedSteps"`
	MockServers  []models.MockServer   `json:"mockServers"`
	MockRoutes   []models.MockRoute    `json:"mockRoutes"`
	Whiteboards  []models.Whiteboard   `json:"whiteboards"`
}

type initialPushResult struct {
	Collections  int `json:"collections"`
	Requests     int `json:"requests"`
	Environments int `json:"environments"`
	Cookies      int `json:"cookies"`
	TestCases    int `json:"testCases"`
	TestPlans    int `json:"testPlans"`
	Defects      int `json:"defects"`
	Requirements int `json:"requirements"`
	SharedSteps  int `json:"sharedSteps"`
	MockServers  int `json:"mockServers"`
	MockRoutes   int `json:"mockRoutes"`
	Whiteboards  int `json:"whiteboards"`
	Skipped      int `json:"skipped"`
}

// validUUIDs returns true if every id is parseable as a UUID. Empty/invalid
// ids fail because every entity must carry a stable client-generated id.
func validUUIDs(ids ...string) bool {
	for _, id := range ids {
		if id == "" {
			return false
		}
		if _, err := uuid.Parse(id); err != nil {
			return false
		}
	}
	return true
}

// InitialPush bulk-creates local entities for a newly registered user. Uses
// OnConflict DoNothing so a retry after partial success is idempotent — the
// client can safely replay the same batch without duplicating work.
func (h *SyncHandler) InitialPush(c *gin.Context) {
	userID := c.GetString("userID")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	var input initialPushInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Validate every incoming entity has a parseable UUID. Reject the whole
	// batch on first bad id — client bugs shouldn't partially poison the DB.
	for i := range input.Collections {
		if !validUUIDs(input.Collections[i].ID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "collection has invalid id"})
			return
		}
	}
	for i := range input.Requests {
		if !validUUIDs(input.Requests[i].ID, input.Requests[i].CollectionID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "request has invalid id or collectionId"})
			return
		}
	}
	for i := range input.Environments {
		if !validUUIDs(input.Environments[i].ID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "environment has invalid id"})
			return
		}
	}
	for i := range input.Cookies {
		if !validUUIDs(input.Cookies[i].ID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cookie has invalid id"})
			return
		}
	}
	for i := range input.TestCases {
		if !validUUIDs(input.TestCases[i].ID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "test case has invalid id"})
			return
		}
	}
	for i := range input.TestPlans {
		if !validUUIDs(input.TestPlans[i].ID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "test plan has invalid id"})
			return
		}
	}
	for i := range input.Defects {
		if !validUUIDs(input.Defects[i].ID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "defect has invalid id"})
			return
		}
	}
	for i := range input.Requirements {
		if !validUUIDs(input.Requirements[i].ID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "requirement has invalid id"})
			return
		}
	}
	for i := range input.SharedSteps {
		if !validUUIDs(input.SharedSteps[i].ID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "shared step has invalid id"})
			return
		}
	}
	for i := range input.MockServers {
		if !validUUIDs(input.MockServers[i].ID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "mock server has invalid id"})
			return
		}
	}
	for i := range input.MockRoutes {
		if !validUUIDs(input.MockRoutes[i].ID, input.MockRoutes[i].MockServerID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "mock route has invalid id or mockServerId"})
			return
		}
	}
	for i := range input.Whiteboards {
		if !validUUIDs(input.Whiteboards[i].ID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "whiteboard has invalid id"})
			return
		}
	}

	// Force UserID on every record so a malicious client can't plant entities
	// under someone else's account.
	for i := range input.Collections {
		input.Collections[i].UserID = userID
	}
	for i := range input.Environments {
		input.Environments[i].UserID = userID
	}
	for i := range input.Cookies {
		input.Cookies[i].UserID = userID
	}
	for i := range input.TestCases {
		input.TestCases[i].UserID = userID
	}
	for i := range input.TestPlans {
		input.TestPlans[i].UserID = userID
	}
	for i := range input.Defects {
		input.Defects[i].UserID = userID
	}
	for i := range input.Requirements {
		input.Requirements[i].UserID = userID
	}
	for i := range input.SharedSteps {
		input.SharedSteps[i].UserID = userID
	}
	for i := range input.MockServers {
		input.MockServers[i].UserID = userID
	}
	for i := range input.Whiteboards {
		input.Whiteboards[i].UserID = userID
		// Sharing (grants, public links, collab rooms, the diagram queue) is
		// per-backend and NOT migrated, so a carried-over 'public' flag would be
		// a share with zero grants — misleading. Reset every pushed board to
		// private; the user re-shares on the new backend if they want to.
		input.Whiteboards[i].Visibility = "private"
	}
	// Requests/MockRoutes inherit ownership via FK (collection/server).

	result := initialPushResult{}

	err := h.db.Transaction(func(tx *gorm.DB) error {
		onConflict := clause.OnConflict{DoNothing: true}

		// Order matters: parents before children.
		if len(input.Collections) > 0 {
			res := tx.Clauses(onConflict).Create(&input.Collections)
			if res.Error != nil {
				return res.Error
			}
			result.Collections = int(res.RowsAffected)
			result.Skipped += len(input.Collections) - int(res.RowsAffected)
		}

		// Verify each request's CollectionID belongs to this user before
		// inserting — prevents attaching a request to someone else's collection.
		if len(input.Requests) > 0 {
			validReqs := make([]models.SavedRequest, 0, len(input.Requests))
			for _, r := range input.Requests {
				var col models.Collection
				if err := tx.Select("id", "user_id").Where("id = ? AND user_id = ?", r.CollectionID, userID).First(&col).Error; err != nil {
					result.Skipped++
					continue
				}
				validReqs = append(validReqs, r)
			}
			if len(validReqs) > 0 {
				res := tx.Clauses(onConflict).Create(&validReqs)
				if res.Error != nil {
					return res.Error
				}
				result.Requests = int(res.RowsAffected)
				result.Skipped += len(validReqs) - int(res.RowsAffected)
			}
		}

		if len(input.Environments) > 0 {
			res := tx.Clauses(onConflict).Create(&input.Environments)
			if res.Error != nil {
				return res.Error
			}
			result.Environments = int(res.RowsAffected)
			result.Skipped += len(input.Environments) - int(res.RowsAffected)
		}
		if len(input.Cookies) > 0 {
			res := tx.Clauses(onConflict).Create(&input.Cookies)
			if res.Error != nil {
				return res.Error
			}
			result.Cookies = int(res.RowsAffected)
			result.Skipped += len(input.Cookies) - int(res.RowsAffected)
		}
		if len(input.TestCases) > 0 {
			res := tx.Clauses(onConflict).Create(&input.TestCases)
			if res.Error != nil {
				return res.Error
			}
			result.TestCases = int(res.RowsAffected)
			result.Skipped += len(input.TestCases) - int(res.RowsAffected)
		}
		if len(input.TestPlans) > 0 {
			res := tx.Clauses(onConflict).Create(&input.TestPlans)
			if res.Error != nil {
				return res.Error
			}
			result.TestPlans = int(res.RowsAffected)
			result.Skipped += len(input.TestPlans) - int(res.RowsAffected)
		}
		if len(input.Defects) > 0 {
			res := tx.Clauses(onConflict).Create(&input.Defects)
			if res.Error != nil {
				return res.Error
			}
			result.Defects = int(res.RowsAffected)
			result.Skipped += len(input.Defects) - int(res.RowsAffected)
		}
		if len(input.Requirements) > 0 {
			res := tx.Clauses(onConflict).Create(&input.Requirements)
			if res.Error != nil {
				return res.Error
			}
			result.Requirements = int(res.RowsAffected)
			result.Skipped += len(input.Requirements) - int(res.RowsAffected)
		}
		if len(input.SharedSteps) > 0 {
			res := tx.Clauses(onConflict).Create(&input.SharedSteps)
			if res.Error != nil {
				return res.Error
			}
			result.SharedSteps = int(res.RowsAffected)
			result.Skipped += len(input.SharedSteps) - int(res.RowsAffected)
		}
		if len(input.MockServers) > 0 {
			// Prefix must be globally unique. OnConflict DoNothing will skip
			// rows whose prefix collides with another user's existing server.
			res := tx.Clauses(onConflict).Create(&input.MockServers)
			if res.Error != nil {
				return res.Error
			}
			result.MockServers = int(res.RowsAffected)
			result.Skipped += len(input.MockServers) - int(res.RowsAffected)
		}
		if len(input.MockRoutes) > 0 {
			validRoutes := make([]models.MockRoute, 0, len(input.MockRoutes))
			for _, rt := range input.MockRoutes {
				var srv models.MockServer
				if err := tx.Select("id", "user_id").Where("id = ? AND user_id = ?", rt.MockServerID, userID).First(&srv).Error; err != nil {
					result.Skipped++
					continue
				}
				validRoutes = append(validRoutes, rt)
			}
			if len(validRoutes) > 0 {
				res := tx.Clauses(onConflict).Create(&validRoutes)
				if res.Error != nil {
					return res.Error
				}
				result.MockRoutes = int(res.RowsAffected)
				result.Skipped += len(validRoutes) - int(res.RowsAffected)
			}
		}

		if len(input.Whiteboards) > 0 {
			// Re-validate ParentID. Create() bypasses the normal validParent
			// check, and ListShared's subtree walk has no user_id filter — a
			// board whose parentId points at someone else's (shared) folder would
			// be planted inside their subtree for every viewer. A parent is OK
			// only if it's in THIS batch or already owned by this user; otherwise
			// re-root the board to the top level (nil) rather than drop it.
			batchIDs := make(map[string]bool, len(input.Whiteboards))
			for i := range input.Whiteboards {
				batchIDs[input.Whiteboards[i].ID] = true
			}
			ownsParent := make(map[string]bool)
			for i := range input.Whiteboards {
				pid := input.Whiteboards[i].ParentID
				if pid == nil || *pid == "" {
					input.Whiteboards[i].ParentID = nil
					continue
				}
				if batchIDs[*pid] {
					continue
				}
				owned, checked := ownsParent[*pid]
				if !checked {
					var cnt int64
					tx.Model(&models.Whiteboard{}).Where("id = ? AND user_id = ?", *pid, userID).Count(&cnt)
					owned = cnt > 0
					ownsParent[*pid] = owned
				}
				if !owned {
					input.Whiteboards[i].ParentID = nil
				}
			}
			res := tx.Clauses(onConflict).Create(&input.Whiteboards)
			if res.Error != nil {
				return res.Error
			}
			result.Whiteboards = int(res.RowsAffected)
			result.Skipped += len(input.Whiteboards) - int(res.RowsAffected)
		}

		return nil
	})

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to push local data: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": result})
}

// ForwardHistory receives a single history record mirrored from an embedded
// BE. The UserID is forced to the authed user so a compromised forward token
// cannot plant rows under other accounts. Duplicates (by primary key) are
// silently skipped — the embedded BE retries on transient failures.
func (h *SyncHandler) ForwardHistory(c *gin.Context) {
	userID := c.GetString("userID")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	var in models.History
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !validUUIDs(in.ID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "history has invalid id"})
		return
	}
	in.UserID = userID

	res := h.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&in)
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to forward history"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"inserted": res.RowsAffected == 1}})
}

// ForwardTestRun upserts a single TestRun mirrored from an embedded BE.
// Verifies the associated TestPlan belongs to the authed user before writing.
func (h *SyncHandler) ForwardTestRun(c *gin.Context) {
	userID := c.GetString("userID")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	var in models.TestRun
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !validUUIDs(in.ID, in.TestPlanID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "test run has invalid id or testPlanId"})
		return
	}

	var plan models.TestPlan
	if err := h.db.Select("id", "user_id").Where("id = ? AND user_id = ?", in.TestPlanID, userID).First(&plan).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "test plan not found"})
		return
	}
	in.UserID = userID

	// Upsert on primary key: if the run already exists (a second forward after
	// initial create), update the mutable fields. This lets the embedded BE
	// forward both the initial "running" state and the final "completed"
	// state through the same endpoint.
	res := h.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"status", "results", "started_at", "completed_at", "environment",
		}),
	}).Create(&in)
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to forward test run"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}
