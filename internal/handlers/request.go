package handlers

import (
	"net/http"

	"patchman-be/internal/models"
	"patchman-be/internal/realtime"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type RequestHandler struct {
	db *gorm.DB
}

func NewRequestHandler(db *gorm.DB) *RequestHandler {
	return &RequestHandler{db: db}
}

type createRequestInput struct {
	ID               string `json:"id"`
	CollectionID     string `json:"collectionId" binding:"required"`
	Name             string `json:"name" binding:"required"`
	Method           string `json:"method"`
	URL              string `json:"url"`
	Headers          string `json:"headers"`
	Params           string `json:"params"`
	PathParams       string `json:"pathParams"`
	Body             string `json:"body"`
	Auth             string `json:"auth"`
	PreRequestScript string `json:"preRequestScript"`
	TestScript       string `json:"testScript"`
	SortOrder        int    `json:"sortOrder"`
}

type updateRequestInput struct {
	Name             *string `json:"name"`
	Method           *string `json:"method"`
	URL              *string `json:"url"`
	Headers          *string `json:"headers"`
	Params           *string `json:"params"`
	PathParams       *string `json:"pathParams"`
	Body             *string `json:"body"`
	Auth             *string `json:"auth"`
	PreRequestScript *string `json:"preRequestScript"`
	TestScript       *string `json:"testScript"`
	SortOrder        *int    `json:"sortOrder"`
	CollectionID     *string `json:"collectionId"`
}

func (h *RequestHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")

	var input createRequestInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Verify user has *edit* rights on the collection — public viewers
	// must not be able to create new requests on a public collection.
	if !canEditCollection(h.db, input.CollectionID, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You need editor access to add requests to this collection"})
		return
	}

	method := input.Method
	if method == "" {
		method = "GET"
	}

	newID, ok := pickID(input.ID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid id format"})
		return
	}

	req := models.SavedRequest{
		ID:               newID,
		CollectionID:     input.CollectionID,
		Name:             input.Name,
		Method:           method,
		URL:              input.URL,
		Headers:          input.Headers,
		Params:           input.Params,
		PathParams:       input.PathParams,
		Body:             input.Body,
		Auth:             input.Auth,
		PreRequestScript: input.PreRequestScript,
		TestScript:       input.TestScript,
		SortOrder:        input.SortOrder,
	}

	if err := h.db.Create(&req).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create request"})
		return
	}

	notifyCollectionMutated(h.db, c, req.CollectionID, realtime.EventRequestCreated)

	c.JSON(http.StatusCreated, gin.H{"data": req})
}

func (h *RequestHandler) Get(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var req models.SavedRequest
	if err := h.db.First(&req, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Request not found"})
		return
	}

	if !canAccessCollection(h.db, req.CollectionID, userID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Request not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": req})
}

func (h *RequestHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var req models.SavedRequest
	if err := h.db.First(&req, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Request not found"})
		return
	}

	if !canEditCollection(h.db, req.CollectionID, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You need editor access to modify this request"})
		return
	}

	var input updateRequestInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	updates := map[string]interface{}{}
	if input.Name != nil {
		updates["name"] = *input.Name
	}
	if input.Method != nil {
		updates["method"] = *input.Method
	}
	if input.URL != nil {
		updates["url"] = *input.URL
	}
	if input.Headers != nil {
		updates["headers"] = *input.Headers
	}
	if input.Params != nil {
		updates["params"] = *input.Params
	}
	if input.PathParams != nil {
		updates["path_params"] = *input.PathParams
	}
	if input.Body != nil {
		updates["body"] = *input.Body
	}
	if input.Auth != nil {
		updates["auth"] = *input.Auth
	}
	if input.PreRequestScript != nil {
		updates["pre_request_script"] = *input.PreRequestScript
	}
	if input.TestScript != nil {
		updates["test_script"] = *input.TestScript
	}
	if input.SortOrder != nil {
		updates["sort_order"] = *input.SortOrder
	}
	if input.CollectionID != nil {
		if !canEditCollection(h.db, *input.CollectionID, userID) {
			c.JSON(http.StatusForbidden, gin.H{"error": "No editor access to target collection"})
			return
		}
		updates["collection_id"] = *input.CollectionID
	}

	if err := h.db.Model(&req).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update request"})
		return
	}

	h.db.First(&req, "id = ?", id)
	notifyCollectionMutated(h.db, c, req.CollectionID, realtime.EventRequestUpdated)
	c.JSON(http.StatusOK, gin.H{"data": req})
}

func (h *RequestHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var req models.SavedRequest
	if err := h.db.First(&req, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Request not found"})
		return
	}

	if !canEditCollection(h.db, req.CollectionID, userID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "You need editor access to delete this request"})
		return
	}

	if err := h.db.Delete(&req).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete request"})
		return
	}

	notifyCollectionMutated(h.db, c, req.CollectionID, realtime.EventRequestDeleted)

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Request deleted"}})
}
