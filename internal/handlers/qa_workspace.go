package handlers

import (
	"net/http"
	"strings"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// QAWorkspaceHandler moves QA items between the personal space and a
// workspace (CR_QA_RFC O5).
type QAWorkspaceHandler struct{ db *gorm.DB }

func NewQAWorkspaceHandler(db *gorm.DB) *QAWorkspaceHandler { return &QAWorkspaceHandler{db: db} }

type qaMoveInput struct {
	Kind string `json:"kind" binding:"required"` // case | folder | plan | defect | requirement | sharedStep | loadTest
	ID   string `json:"id" binding:"required"`
	// WorkspaceID: the target workspace; "" moves the item back to personal.
	WorkspaceID string `json:"workspaceId"`
}

// Move — POST /api/qa/move. Only the item's creator may move it, and only
// into a workspace they can edit. A folder takes its subfolders and their
// cases along (a case moved alone leaves its folder); a plan takes its runs.
func (h *QAWorkspaceHandler) Move(c *gin.Context) {
	userID := c.GetString("userID")
	var in qaMoveInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var target *string
	if ws := strings.TrimSpace(in.WorkspaceID); ws != "" {
		switch r := qaWorkspaceRole(h.db, ws, userID); {
		case r == NoRole:
			c.JSON(http.StatusNotFound, gin.H{"error": "Workspace not found"})
			return
		case !r.AtLeast(EditorRole):
			c.JSON(http.StatusForbidden, gin.H{"error": qaForbidden(EditorRole)})
			return
		}
		target = &ws
	}

	var row qaOwned
	var model any
	switch in.Kind {
	case "case":
		var x models.TestCase
		row, model = &x, &x
	case "folder":
		var x models.TestCaseFolder
		row, model = &x, &x
	case "plan":
		var x models.TestPlan
		row, model = &x, &x
	case "defect":
		var x models.Defect
		row, model = &x, &x
	case "requirement":
		var x models.Requirement
		row, model = &x, &x
	case "sharedStep":
		var x models.SharedStep
		row, model = &x, &x
	case "loadTest":
		var x models.LoadTest
		row, model = &x, &x
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown kind"})
		return
	}
	if err := h.db.Where("id = ?", in.ID).First(model).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}
	if owner, _ := row.QAOwner(); owner != userID {
		if qaRowRole(h.db, userID, row) == NoRole {
			c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		} else {
			c.JSON(http.StatusForbidden, gin.H{"error": "Only its creator can move this item"})
		}
		return
	}

	value := nullableUpdate(target)
	moved := 0
	err := h.db.Transaction(func(tx *gorm.DB) error {
		switch in.Kind {
		case "folder":
			ids, err := qaFolderTree(tx, in.ID)
			if err != nil {
				return err
			}
			if err := tx.Model(&models.TestCaseFolder{}).Where("id IN ?", ids).Update("workspace_id", value).Error; err != nil {
				return err
			}
			res := tx.Model(&models.TestCase{}).Where("folder_id IN ?", ids).Update("workspace_id", value)
			moved = len(ids) + int(res.RowsAffected)
			return res.Error
		case "case":
			// Leaves a folder of the old scope: file it at the root.
			moved = 1
			return tx.Model(model).Updates(map[string]any{"workspace_id": value, "folder_id": gorm.Expr("NULL")}).Error
		case "plan":
			if err := tx.Model(model).Update("workspace_id", value).Error; err != nil {
				return err
			}
			res := tx.Model(&models.TestRun{}).Where("test_plan_id = ?", in.ID).Update("workspace_id", value)
			moved = 1 + int(res.RowsAffected)
			return res.Error
		default:
			moved = 1
			return tx.Model(model).Update("workspace_id", value).Error
		}
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to move"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"moved": moved, "workspaceId": target}})
}

// qaFolderTree: a folder and every folder under it.
func qaFolderTree(db *gorm.DB, rootID string) ([]string, error) {
	ids := []string{rootID}
	frontier := []string{rootID}
	for len(frontier) > 0 && len(ids) < 10000 {
		var kids []string
		if err := db.Model(&models.TestCaseFolder{}).Where("parent_id IN ?", frontier).Pluck("id", &kids).Error; err != nil {
			return nil, err
		}
		ids = append(ids, kids...)
		frontier = kids
	}
	return ids, nil
}
