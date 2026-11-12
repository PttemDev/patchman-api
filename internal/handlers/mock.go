package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type MockHandler struct {
	db *gorm.DB
}

func NewMockHandler(db *gorm.DB) *MockHandler {
	return &MockHandler{db: db}
}

// userMockPrefix returns the per-user namespace segment that gets prepended
// to every mock prefix so two users editing the same suffix don't collide
// on the global uniqueIndex. Derived from the email's local part:
//
//	jane.doe@example.com → "jane.doe"
//
// Embedded mode users (no email) get "" — they're alone on the device, no
// collisions possible.
func (h *MockHandler) userMockPrefix(userID string) string {
	var user models.User
	if err := h.db.Select("email").First(&user, "id = ?", userID).Error; err != nil {
		return ""
	}
	at := strings.IndexByte(user.Email, '@')
	if at <= 0 {
		return ""
	}
	return sanitizeMockSlug(user.Email[:at])
}

// sanitizeMockSlug enforces the same character set the catch-all router
// accepts. Anything outside [a-z0-9.-_] becomes '-' so an email like
// "Müge.Kurt+test" still produces a usable prefix.
func sanitizeMockSlug(s string) string {
	s = strings.ToLower(s)
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'a' && c <= 'z'), (c >= '0' && c <= '9'), c == '.', c == '-', c == '_':
			out = append(out, c)
		default:
			out = append(out, '-')
		}
	}
	return strings.Trim(string(out), "-.")
}

// applyUserMockPrefix prepends the user's namespace to a user-supplied
// suffix. Idempotent — if the suffix is already namespaced for this user
// (e.g. on update where we round-trip the stored value), the namespace
// isn't doubled.
func (h *MockHandler) applyUserMockPrefix(userID, suffix string) string {
	ns := h.userMockPrefix(userID)
	suffix = strings.Trim(suffix, "/")
	if ns == "" {
		return suffix
	}
	if suffix == ns || strings.HasPrefix(suffix, ns+"/") {
		return suffix
	}
	return ns + "/" + suffix
}

// --- Mock Servers ---

type createMockServerInput struct {
	ID     string `json:"id"`
	Name   string `json:"name" binding:"required"`
	Prefix string `json:"prefix" binding:"required"`
}

type updateMockServerInput struct {
	Name     *string `json:"name"`
	Prefix   *string `json:"prefix"`
	IsActive *bool   `json:"isActive"`
}

func (h *MockHandler) ListServers(c *gin.Context) {
	userID := c.GetString("userID")

	var servers []models.MockServer
	if err := h.db.Where("user_id = ?", userID).
		Preload("Routes").
		Order("created_at DESC").
		Find(&servers).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch mock servers"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": servers})
}

func (h *MockHandler) CreateServer(c *gin.Context) {
	userID := c.GetString("userID")

	var input createMockServerInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Normalize prefix: strip slashes so it's a clean slug, then prepend
	// the user's namespace so two users using the same suffix don't
	// collide on the global uniqueIndex.
	suffix := strings.Trim(input.Prefix, "/")
	if suffix == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Prefix cannot be empty"})
		return
	}
	prefix := h.applyUserMockPrefix(userID, suffix)

	// Check prefix uniqueness — still required for embedded mode where the
	// namespace is empty.
	var existing models.MockServer
	if err := h.db.Where("prefix = ?", prefix).First(&existing).Error; err == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "Prefix already in use"})
		return
	}

	newID, ok := pickID(input.ID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid id format"})
		return
	}

	server := models.MockServer{
		ID:       newID,
		UserID:   userID,
		Name:     input.Name,
		Prefix:   prefix,
		IsActive: true,
	}

	if err := h.db.Create(&server).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create mock server"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"data": server})
}

func (h *MockHandler) UpdateServer(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var server models.MockServer
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&server).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Mock server not found"})
		return
	}

	var input updateMockServerInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	updates := map[string]interface{}{}
	if input.Name != nil {
		updates["name"] = *input.Name
	}
	if input.Prefix != nil {
		normalized := h.applyUserMockPrefix(userID, *input.Prefix)
		// Check uniqueness
		var existing models.MockServer
		if err := h.db.Where("prefix = ? AND id != ?", normalized, id).First(&existing).Error; err == nil {
			c.JSON(http.StatusConflict, gin.H{"error": "Prefix already in use"})
			return
		}
		updates["prefix"] = normalized
	}
	if input.IsActive != nil {
		updates["is_active"] = *input.IsActive
	}

	if err := h.db.Model(&server).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update mock server"})
		return
	}

	h.db.First(&server, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{"data": server})
}

func (h *MockHandler) DeleteServer(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var server models.MockServer
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&server).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Mock server not found"})
		return
	}

	// Delete all routes
	h.db.Where("mock_server_id = ?", id).Delete(&models.MockRoute{})

	if err := h.db.Delete(&server).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete mock server"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Mock server deleted"}})
}

// --- Mock Routes ---

type createMockRouteInput struct {
	ID              string `json:"id"`
	Method          string `json:"method" binding:"required"`
	Path            string `json:"path" binding:"required"`
	StatusCode      int    `json:"statusCode"`
	ResponseHeaders string `json:"responseHeaders"`
	ResponseBody    string `json:"responseBody"`
	Delay           int    `json:"delay"`
}

type updateMockRouteInput struct {
	Method          *string `json:"method"`
	Path            *string `json:"path"`
	StatusCode      *int    `json:"statusCode"`
	ResponseHeaders *string `json:"responseHeaders"`
	ResponseBody    *string `json:"responseBody"`
	Delay           *int    `json:"delay"`
	IsActive        *bool   `json:"isActive"`
}

func (h *MockHandler) ListRoutes(c *gin.Context) {
	userID := c.GetString("userID")
	serverID := c.Param("id")

	// Verify server belongs to user
	var server models.MockServer
	if err := h.db.Where("id = ? AND user_id = ?", serverID, userID).First(&server).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Mock server not found"})
		return
	}

	var routes []models.MockRoute
	if err := h.db.Where("mock_server_id = ?", serverID).
		Order("created_at ASC").
		Find(&routes).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch routes"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": routes})
}

func (h *MockHandler) CreateRoute(c *gin.Context) {
	userID := c.GetString("userID")
	serverID := c.Param("id")

	// Verify server belongs to user
	var server models.MockServer
	if err := h.db.Where("id = ? AND user_id = ?", serverID, userID).First(&server).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Mock server not found"})
		return
	}

	var input createMockRouteInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	statusCode := input.StatusCode
	if statusCode == 0 {
		statusCode = 200
	}

	newID, ok := pickID(input.ID)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid id format"})
		return
	}

	route := models.MockRoute{
		ID:              newID,
		MockServerID:    serverID,
		Method:          strings.ToUpper(input.Method),
		Path:            input.Path,
		StatusCode:      statusCode,
		ResponseHeaders: input.ResponseHeaders,
		ResponseBody:    input.ResponseBody,
		Delay:           input.Delay,
		IsActive:        true,
	}

	if err := h.db.Create(&route).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create route"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"data": route})
}

func (h *MockHandler) UpdateRoute(c *gin.Context) {
	id := c.Param("id")

	var route models.MockRoute
	if err := h.db.First(&route, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Mock route not found"})
		return
	}

	// Verify ownership through server
	userID := c.GetString("userID")
	var server models.MockServer
	if err := h.db.Where("id = ? AND user_id = ?", route.MockServerID, userID).First(&server).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Mock route not found"})
		return
	}

	var input updateMockRouteInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	updates := map[string]interface{}{}
	if input.Method != nil {
		updates["method"] = strings.ToUpper(*input.Method)
	}
	if input.Path != nil {
		updates["path"] = *input.Path
	}
	if input.StatusCode != nil {
		updates["status_code"] = *input.StatusCode
	}
	if input.ResponseHeaders != nil {
		updates["response_headers"] = *input.ResponseHeaders
	}
	if input.ResponseBody != nil {
		updates["response_body"] = *input.ResponseBody
	}
	if input.Delay != nil {
		updates["delay"] = *input.Delay
	}
	if input.IsActive != nil {
		updates["is_active"] = *input.IsActive
	}

	if err := h.db.Model(&route).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update route"})
		return
	}

	h.db.First(&route, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{"data": route})
}

func (h *MockHandler) DeleteRoute(c *gin.Context) {
	id := c.Param("id")

	var route models.MockRoute
	if err := h.db.First(&route, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Mock route not found"})
		return
	}

	// Verify ownership through server
	userID := c.GetString("userID")
	var server models.MockServer
	if err := h.db.Where("id = ? AND user_id = ?", route.MockServerID, userID).First(&server).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Mock route not found"})
		return
	}

	if err := h.db.Delete(&route).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete route"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Mock route deleted"}})
}

// --- Mock Catch-All Handler ---

type MockCatchAllHandler struct {
	db *gorm.DB
}

func NewMockCatchAllHandler(db *gorm.DB) *MockCatchAllHandler {
	return &MockCatchAllHandler{db: db}
}

func (h *MockCatchAllHandler) Handle(c *gin.Context) {
	full := strings.TrimPrefix(c.Param("fullpath"), "/")

	// Walk path segments from longest to shortest looking for a registered
	// prefix. Necessary because user-namespaced prefixes carry slashes
	// (`jane.doe/test`) and Gin's `:prefix` only captures one segment.
	segments := strings.Split(full, "/")
	var server models.MockServer
	var path string
	found := false
	for i := len(segments); i > 0; i-- {
		candidate := strings.Join(segments[:i], "/")
		// Also probe legacy single-segment forms ("/foo", "foo") so older
		// mock servers created before the namespace requirement still work.
		if err := h.db.Where("prefix IN (?, ?, ?) AND is_active = ?",
			candidate, "/"+candidate, candidate, true).First(&server).Error; err == nil {
			path = "/" + strings.Join(segments[i:], "/")
			if path == "/" {
				path = ""
			}
			found = true
			break
		}
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "Mock server not found"})
		return
	}

	// Find matching route
	method := strings.ToUpper(c.Request.Method)
	var route models.MockRoute
	if err := h.db.Where("mock_server_id = ? AND method = ? AND path = ? AND is_active = ?",
		server.ID, method, path, true).First(&route).Error; err != nil {
		// Try without leading slash
		cleanPath := strings.TrimPrefix(path, "/")
		if err := h.db.Where("mock_server_id = ? AND method = ? AND path = ? AND is_active = ?",
			server.ID, method, cleanPath, true).First(&route).Error; err != nil {
			// Also try with leading slash
			withSlash := "/" + cleanPath
			if err := h.db.Where("mock_server_id = ? AND method = ? AND path = ? AND is_active = ?",
				server.ID, method, withSlash, true).First(&route).Error; err != nil {
				c.JSON(http.StatusNotFound, gin.H{"error": "No matching mock route found"})
				return
			}
		}
	}

	// Apply delay if configured
	if route.Delay > 0 {
		time.Sleep(time.Duration(route.Delay) * time.Millisecond)
	}

	// Set response headers
	if route.ResponseHeaders != "" {
		var headers map[string]string
		if err := json.Unmarshal([]byte(route.ResponseHeaders), &headers); err == nil {
			for k, v := range headers {
				c.Header(k, v)
			}
		}
	}

	// Return response
	statusCode := route.StatusCode
	if statusCode == 0 {
		statusCode = 200
	}

	// Try to detect if response is JSON
	body := route.ResponseBody
	if len(body) > 0 && (body[0] == '{' || body[0] == '[') {
		c.Data(statusCode, "application/json", []byte(body))
	} else {
		c.Data(statusCode, "text/plain", []byte(body))
	}
}
