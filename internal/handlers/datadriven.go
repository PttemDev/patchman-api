package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type DataDrivenHandler struct {
	db *gorm.DB
}

func NewDataDrivenHandler(db *gorm.DB) *DataDrivenHandler {
	return &DataDrivenHandler{db: db}
}

type dataDrivenInput struct {
	CollectionID  string              `json:"collectionId" binding:"required"`
	EnvironmentID string              `json:"environmentId"`
	Data          []map[string]string `json:"data" binding:"required"`
	Delay         int                 `json:"delay"` // milliseconds between iterations
}

type iterationResult struct {
	RowIndex  int                     `json:"rowIndex"`
	Variables map[string]string       `json:"variables"`
	Results   []runnerRequestResult   `json:"results"`
}

type dataDrivenSummary struct {
	TotalIterations int   `json:"totalIterations"`
	TotalRequests   int   `json:"totalRequests"`
	TotalPassed     int   `json:"totalPassed"`
	TotalFailed     int   `json:"totalFailed"`
	TotalErrors     int   `json:"totalErrors"`
	TotalDuration   int64 `json:"totalDuration"`
}

// Run handles POST /api/runner/data-driven
// For each row in the data array, it runs every request in the collection
// with {{variable}} placeholders replaced by row values.
func (h *DataDrivenHandler) Run(c *gin.Context) {
	userID := c.GetString("userID")

	var input dataDrivenInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if len(input.Data) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Data array must contain at least one row"})
		return
	}

	// Verify collection belongs to user
	var collection models.Collection
	if err := h.db.Where("id = ? AND user_id = ?", input.CollectionID, userID).First(&collection).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Collection not found"})
		return
	}

	// Load all requests in the collection (including child folders)
	requests := h.loadCollectionRequests(input.CollectionID)
	if len(requests) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Collection has no requests"})
		return
	}

	// Load environment variables if provided
	envVars := make(map[string]string)
	if input.EnvironmentID != "" {
		var env models.Environment
		if err := h.db.Where("id = ? AND user_id = ?", input.EnvironmentID, userID).First(&env).Error; err == nil {
			envVars = parseEnvironmentVariables(env.Variables)
		}
	}

	// Run iterations
	var iterations []iterationResult
	summary := dataDrivenSummary{
		TotalIterations: len(input.Data),
	}

	startTime := time.Now()

	for rowIdx, row := range input.Data {
		// Merge env vars with row data (row overrides env)
		merged := make(map[string]string)
		for k, v := range envVars {
			merged[k] = v
		}
		for k, v := range row {
			merged[k] = v
		}

		iteration := iterationResult{
			RowIndex:  rowIdx,
			Variables: merged,
		}

		for _, req := range requests {
			result := h.executeRequest(req, merged, userID, rowIdx)
			iteration.Results = append(iteration.Results, result)

			summary.TotalRequests++
			if result.Error != "" {
				summary.TotalErrors++
			} else if result.Passed {
				summary.TotalPassed++
			} else {
				summary.TotalFailed++
			}
		}

		iterations = append(iterations, iteration)

		// Apply delay between iterations (but not after the last one)
		if input.Delay > 0 && rowIdx < len(input.Data)-1 {
			time.Sleep(time.Duration(input.Delay) * time.Millisecond)
		}
	}

	summary.TotalDuration = time.Since(startTime).Milliseconds()

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"iterations": iterations,
			"summary":    summary,
		},
	})
}

// loadCollectionRequests loads all requests in a collection and its child folders.
func (h *DataDrivenHandler) loadCollectionRequests(collectionID string) []models.SavedRequest {
	var requests []models.SavedRequest
	h.db.Where("collection_id = ?", collectionID).
		Order("sort_order ASC, created_at ASC").
		Find(&requests)

	// Also load from child folders
	var children []models.Collection
	h.db.Where("parent_id = ?", collectionID).Find(&children)
	for _, child := range children {
		childRequests := h.loadCollectionRequests(child.ID)
		requests = append(requests, childRequests...)
	}

	return requests
}

// executeRequest runs a single saved request with variable substitution
// and saves the result to history.
func (h *DataDrivenHandler) executeRequest(req models.SavedRequest, vars map[string]string, userID string, rowIndex int) runnerRequestResult {
	result := runnerRequestResult{
		RequestID: req.ID,
		Name:      req.Name,
		Method:    req.Method,
		Iteration: rowIndex,
	}

	// Substitute variables in URL
	resolvedURL := substituteVariables(req.URL, vars)
	result.URL = resolvedURL

	// Build proxy request
	proxyReq := services.ProxyRequest{
		Method: req.Method,
		URL:    resolvedURL,
	}

	// Parse and substitute headers
	if req.Headers != "" {
		var headerList []struct {
			Key     string `json:"key"`
			Value   string `json:"value"`
			Enabled bool   `json:"enabled"`
		}
		if err := json.Unmarshal([]byte(req.Headers), &headerList); err == nil {
			proxyReq.Headers = make(map[string]string)
			for _, hdr := range headerList {
				if hdr.Enabled {
					proxyReq.Headers[substituteVariables(hdr.Key, vars)] = substituteVariables(hdr.Value, vars)
				}
			}
		}
	}

	// Parse and substitute params
	if req.Params != "" {
		var paramList []struct {
			Key     string `json:"key"`
			Value   string `json:"value"`
			Enabled bool   `json:"enabled"`
		}
		if err := json.Unmarshal([]byte(req.Params), &paramList); err == nil {
			proxyReq.Params = make(map[string]string)
			for _, p := range paramList {
				if p.Enabled {
					proxyReq.Params[substituteVariables(p.Key, vars)] = substituteVariables(p.Value, vars)
				}
			}
		}
	}

	// Parse and substitute body
	if req.Body != "" {
		var body services.RequestBody
		if err := json.Unmarshal([]byte(req.Body), &body); err == nil {
			body.Content = substituteVariables(body.Content, vars)
			proxyReq.Body = &body
		}
	}

	// Parse and substitute auth
	if req.Auth != "" {
		var auth services.RequestAuth
		if err := json.Unmarshal([]byte(req.Auth), &auth); err == nil {
			auth.Token = substituteVariables(auth.Token, vars)
			auth.Username = substituteVariables(auth.Username, vars)
			auth.Password = substituteVariables(auth.Password, vars)
			auth.Key = substituteVariables(auth.Key, vars)
			auth.Value = substituteVariables(auth.Value, vars)
			proxyReq.Auth = &auth
		}
	}

	// Execute request
	resp, err := services.ExecuteRequest(&proxyReq)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	result.StatusCode = resp.StatusCode
	result.Duration = resp.Duration
	result.Passed = resp.StatusCode >= 200 && resp.StatusCode < 400

	// Save to history
	reqHeadersJSON, _ := json.Marshal(proxyReq.Headers)
	respHeadersJSON, _ := json.Marshal(resp.Headers)

	var reqBody string
	if proxyReq.Body != nil {
		reqBody = proxyReq.Body.Content
	}

	history := models.History{
		ID:              uuid.New().String(),
		UserID:          userID,
		Method:          proxyReq.Method,
		URL:             resolvedURL,
		StatusCode:      resp.StatusCode,
		Duration:        resp.Duration,
		RequestHeaders:  string(reqHeadersJSON),
		RequestBody:     reqBody,
		ResponseHeaders: string(respHeadersJSON),
		ResponseBody:    resp.Body,
		ResponseSize:    resp.Size,
	}
	h.db.Create(&history)

	return result
}

// substituteVariables replaces all {{variable}} placeholders in a string
// with corresponding values from the vars map.
// This is a package-level utility used by both RunnerHandler and DataDrivenHandler.
func substituteVariables(s string, vars map[string]string) string {
	for key, value := range vars {
		s = strings.ReplaceAll(s, "{{"+key+"}}", value)
	}
	return s
}

// parseEnvironmentVariables parses the JSON variables string from an Environment
// into a simple key-value map, including only enabled variables.
// This is a package-level utility used by both RunnerHandler and DataDrivenHandler.
func parseEnvironmentVariables(variablesJSON string) map[string]string {
	result := make(map[string]string)
	if variablesJSON == "" {
		return result
	}

	var vars []struct {
		Key     string `json:"key"`
		Value   string `json:"value"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal([]byte(variablesJSON), &vars); err != nil {
		return result
	}

	for _, v := range vars {
		if v.Enabled {
			result[v.Key] = v.Value
		}
	}
	return result
}
