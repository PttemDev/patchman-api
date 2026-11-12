package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type RunnerHandler struct {
	db *gorm.DB
}

func NewRunnerHandler(db *gorm.DB) *RunnerHandler {
	return &RunnerHandler{db: db}
}

type runCollectionInput struct {
	CollectionID  string `json:"collectionId" binding:"required"`
	EnvironmentID string `json:"environmentId"`
	Delay         int    `json:"delay"`      // ms between requests
	Iterations    int    `json:"iterations"`  // number of times to repeat
}

type runnerRequestResult struct {
	RequestID  string `json:"requestId"`
	Name       string `json:"name"`
	Method     string `json:"method"`
	URL        string `json:"url"`
	StatusCode int    `json:"statusCode"`
	Duration   int64  `json:"duration"`
	Iteration  int    `json:"iteration"`
	Passed     bool   `json:"passed"`
	Error      string `json:"error,omitempty"`
}

type runSummary struct {
	Total      int   `json:"total"`
	Passed     int   `json:"passed"`
	Failed     int   `json:"failed"`
	Duration   int64 `json:"duration"` // total ms
	Iterations int   `json:"iterations"`
}

// Run executes all requests in a collection sequentially
func (h *RunnerHandler) Run(c *gin.Context) {
	userID := c.GetString("userID")

	var input runCollectionInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Verify collection belongs to user
	var collection models.Collection
	if err := h.db.Where("id = ? AND user_id = ?", input.CollectionID, userID).First(&collection).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Collection not found"})
		return
	}

	// Load environment variables if provided
	envVars := make(map[string]string)
	if input.EnvironmentID != "" {
		var env models.Environment
		if err := h.db.Where("id = ? AND user_id = ?", input.EnvironmentID, userID).First(&env).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Environment not found"})
			return
		}
		envVars = parseEnvironmentVariables(env.Variables)
	}

	// Collect all requests recursively from the collection and sub-folders
	allRequests := h.collectRequests(input.CollectionID)
	if len(allRequests) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"data": gin.H{
				"results": []runnerRequestResult{},
				"summary": runSummary{
					Total:      0,
					Passed:     0,
					Failed:     0,
					Duration:   0,
					Iterations: 0,
				},
			},
		})
		return
	}

	iterations := input.Iterations
	if iterations < 1 {
		iterations = 1
	}

	delay := time.Duration(input.Delay) * time.Millisecond

	var results []runnerRequestResult
	totalStart := time.Now()
	passed := 0
	failed := 0

	for iter := 1; iter <= iterations; iter++ {
		for i, savedReq := range allRequests {
			// Apply delay between requests (not before the first one)
			if delay > 0 && (iter > 1 || i > 0) {
				time.Sleep(delay)
			}

			result := h.runRequest(savedReq, envVars, iter)
			if result.Passed {
				passed++
			} else {
				failed++
			}
			results = append(results, result)
		}
	}

	totalDuration := time.Since(totalStart).Milliseconds()

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"results": results,
			"summary": runSummary{
				Total:      len(results),
				Passed:     passed,
				Failed:     failed,
				Duration:   totalDuration,
				Iterations: iterations,
			},
		},
	})
}

// collectRequests loads all SavedRequests from a collection and its sub-folders recursively
func (h *RunnerHandler) collectRequests(collectionID string) []models.SavedRequest {
	var requests []models.SavedRequest
	h.db.Where("collection_id = ?", collectionID).
		Order("sort_order ASC, created_at ASC").
		Find(&requests)

	// Find child folders/collections and collect their requests too
	var children []models.Collection
	h.db.Where("parent_id = ?", collectionID).
		Order("sort_order ASC, created_at ASC").
		Find(&children)

	for _, child := range children {
		childRequests := h.collectRequests(child.ID)
		requests = append(requests, childRequests...)
	}

	return requests
}

// runRequest executes a single saved request with environment variable substitution
func (h *RunnerHandler) runRequest(savedReq models.SavedRequest, envVars map[string]string, iteration int) runnerRequestResult {
	result := runnerRequestResult{
		RequestID: savedReq.ID,
		Name:      savedReq.Name,
		Method:    savedReq.Method,
		Iteration: iteration,
	}

	// Substitute env vars in URL using the shared substituteVariables function
	url := substituteVariables(savedReq.URL, envVars)
	result.URL = url

	// Build proxy request
	proxyReq := &services.ProxyRequest{
		Method: savedReq.Method,
		URL:    url,
	}

	// Parse and substitute headers
	if savedReq.Headers != "" {
		var headerList []struct {
			Key     string `json:"key"`
			Value   string `json:"value"`
			Enabled bool   `json:"enabled"`
		}
		if err := json.Unmarshal([]byte(savedReq.Headers), &headerList); err == nil {
			proxyReq.Headers = make(map[string]string)
			for _, h := range headerList {
				if h.Enabled {
					proxyReq.Headers[substituteVariables(h.Key, envVars)] = substituteVariables(h.Value, envVars)
				}
			}
		}
	}

	// Parse and substitute params
	if savedReq.Params != "" {
		var paramList []struct {
			Key     string `json:"key"`
			Value   string `json:"value"`
			Enabled bool   `json:"enabled"`
		}
		if err := json.Unmarshal([]byte(savedReq.Params), &paramList); err == nil {
			proxyReq.Params = make(map[string]string)
			for _, p := range paramList {
				if p.Enabled {
					proxyReq.Params[substituteVariables(p.Key, envVars)] = substituteVariables(p.Value, envVars)
				}
			}
		}
	}

	// Parse and substitute body
	if savedReq.Body != "" {
		var body services.RequestBody
		if err := json.Unmarshal([]byte(savedReq.Body), &body); err == nil {
			if body.Type != "none" && body.Type != "" {
				body.Content = substituteVariables(body.Content, envVars)
				proxyReq.Body = &body
			}
		}
	}

	// Parse and substitute auth
	if savedReq.Auth != "" {
		var auth services.RequestAuth
		if err := json.Unmarshal([]byte(savedReq.Auth), &auth); err == nil {
			if auth.Type != "none" && auth.Type != "" {
				auth.Token = substituteVariables(auth.Token, envVars)
				auth.Username = substituteVariables(auth.Username, envVars)
				auth.Password = substituteVariables(auth.Password, envVars)
				auth.Key = substituteVariables(auth.Key, envVars)
				auth.Value = substituteVariables(auth.Value, envVars)
				proxyReq.Auth = &auth
			}
		}
	}

	// Execute the request
	resp, err := services.ExecuteRequest(proxyReq)
	if err != nil {
		result.Passed = false
		result.Error = err.Error()
		return result
	}

	result.StatusCode = resp.StatusCode
	result.Duration = resp.Duration
	// Consider 2xx and 3xx as passed
	result.Passed = resp.StatusCode >= 200 && resp.StatusCode < 400

	return result
}
