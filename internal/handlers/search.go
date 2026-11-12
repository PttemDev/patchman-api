package handlers

import (
	"net/http"
	"strings"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type SearchHandler struct {
	db *gorm.DB
}

func NewSearchHandler(db *gorm.DB) *SearchHandler {
	return &SearchHandler{db: db}
}

type searchResult struct {
	Type         string `json:"type"`         // collection, request, environment, testcase, history
	ID           string `json:"id"`
	Title        string `json:"title"`
	Subtitle     string `json:"subtitle"`
	MatchedField string `json:"matchedField"`
}

const maxResultsPerType = 20

// Search handles GET /api/search?q=keyword&type=all
// Searches across collections, requests, environments, test cases, and history.
// type param can be: all, collection, request, environment, testcase, history
func (h *SearchHandler) Search(c *gin.Context) {
	userID := c.GetString("userID")

	query := strings.TrimSpace(c.Query("q"))
	if query == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Search query (q) is required"})
		return
	}

	searchType := c.DefaultQuery("type", "all")
	// LOWER(col) LIKE lower(pattern) instead of ILIKE: ILIKE is Postgres-only,
	// and on the desktop's SQLite every search query failed (silently empty).
	pattern := "%" + strings.ToLower(query) + "%"

	var results []searchResult

	if searchType == "all" || searchType == "collection" {
		results = append(results, h.searchCollections(userID, pattern)...)
	}
	if searchType == "all" || searchType == "request" {
		results = append(results, h.searchRequests(userID, pattern)...)
	}
	if searchType == "all" || searchType == "environment" {
		results = append(results, h.searchEnvironments(userID, pattern)...)
	}
	if searchType == "all" || searchType == "testcase" {
		results = append(results, h.searchTestCases(userID, pattern)...)
	}
	if searchType == "all" || searchType == "history" {
		results = append(results, h.searchHistory(userID, pattern)...)
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"results": results,
		"query":   query,
		"total":   len(results),
	}})
}

func (h *SearchHandler) searchCollections(userID, pattern string) []searchResult {
	var collections []models.Collection
	h.db.Where("user_id = ? AND (LOWER(name) LIKE ? OR LOWER(description) LIKE ?)", userID, pattern, pattern).
		Limit(maxResultsPerType).
		Order("updated_at DESC").
		Find(&collections)

	var results []searchResult
	for _, c := range collections {
		matchedField := "name"
		if !ilike(c.Name, pattern) {
			matchedField = "description"
		}
		results = append(results, searchResult{
			Type:         "collection",
			ID:           c.ID,
			Title:        c.Name,
			Subtitle:     truncate(c.Description, 100),
			MatchedField: matchedField,
		})
	}
	return results
}

func (h *SearchHandler) searchRequests(userID, pattern string) []searchResult {
	// Requests don't have user_id directly; join through collection
	var requests []struct {
		models.SavedRequest
		CollectionName string
	}

	h.db.Table("saved_requests").
		Select("saved_requests.*, collections.name as collection_name").
		Joins("JOIN collections ON collections.id = saved_requests.collection_id").
		Where("collections.user_id = ? AND (LOWER(saved_requests.name) LIKE ? OR LOWER(saved_requests.url) LIKE ?)", userID, pattern, pattern).
		Limit(maxResultsPerType).
		Order("saved_requests.updated_at DESC").
		Find(&requests)

	var results []searchResult
	for _, r := range requests {
		matchedField := "name"
		if !ilike(r.Name, pattern) {
			matchedField = "url"
		}
		subtitle := r.Method + " " + r.URL
		if r.CollectionName != "" {
			subtitle = r.CollectionName + " - " + subtitle
		}
		results = append(results, searchResult{
			Type:         "request",
			ID:           r.ID,
			Title:        r.Name,
			Subtitle:     truncate(subtitle, 120),
			MatchedField: matchedField,
		})
	}
	return results
}

func (h *SearchHandler) searchEnvironments(userID, pattern string) []searchResult {
	var environments []models.Environment
	h.db.Where("user_id = ? AND LOWER(name) LIKE ?", userID, pattern).
		Limit(maxResultsPerType).
		Order("updated_at DESC").
		Find(&environments)

	var results []searchResult
	for _, e := range environments {
		subtitle := "Environment"
		if e.IsActive {
			subtitle = "Active Environment"
		}
		results = append(results, searchResult{
			Type:         "environment",
			ID:           e.ID,
			Title:        e.Name,
			Subtitle:     subtitle,
			MatchedField: "name",
		})
	}
	return results
}

func (h *SearchHandler) searchTestCases(userID, pattern string) []searchResult {
	var testCases []models.TestCase
	h.db.Where("user_id = ? AND (LOWER(title) LIKE ? OR LOWER(description) LIKE ?)", userID, pattern, pattern).
		Limit(maxResultsPerType).
		Order("updated_at DESC").
		Find(&testCases)

	var results []searchResult
	for _, tc := range testCases {
		matchedField := "title"
		if !ilike(tc.Title, pattern) {
			matchedField = "description"
		}
		subtitle := tc.Priority + " | " + tc.Status
		if tc.Type != "" {
			subtitle = tc.Type + " | " + subtitle
		}
		results = append(results, searchResult{
			Type:         "testcase",
			ID:           tc.ID,
			Title:        tc.Title,
			Subtitle:     truncate(subtitle, 100),
			MatchedField: matchedField,
		})
	}
	return results
}

func (h *SearchHandler) searchHistory(userID, pattern string) []searchResult {
	var histories []models.History
	h.db.Where("user_id = ? AND LOWER(url) LIKE ?", userID, pattern).
		Limit(maxResultsPerType).
		Order("created_at DESC").
		Find(&histories)

	var results []searchResult
	for _, hist := range histories {
		results = append(results, searchResult{
			Type:         "history",
			ID:           hist.ID,
			Title:        hist.Method + " " + truncate(hist.URL, 80),
			Subtitle:     formatStatusCode(hist.StatusCode),
			MatchedField: "url",
		})
	}
	return results
}

// ilike performs a case-insensitive match check (mirrors SQL ILIKE with % wildcards).
func ilike(value, pattern string) bool {
	// pattern is "%keyword%", extract the keyword
	keyword := strings.TrimPrefix(pattern, "%")
	keyword = strings.TrimSuffix(keyword, "%")
	return strings.Contains(strings.ToLower(value), strings.ToLower(keyword))
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func formatStatusCode(code int) string {
	if code == 0 {
		return "No response"
	}
	switch {
	case code >= 200 && code < 300:
		return "Success (" + intToStr(code) + ")"
	case code >= 300 && code < 400:
		return "Redirect (" + intToStr(code) + ")"
	case code >= 400 && code < 500:
		return "Client Error (" + intToStr(code) + ")"
	case code >= 500:
		return "Server Error (" + intToStr(code) + ")"
	default:
		return "Status " + intToStr(code)
	}
}

func intToStr(n int) string {
	// Simple int to string without importing strconv for just this
	if n == 0 {
		return "0"
	}
	s := ""
	negative := false
	if n < 0 {
		negative = true
		n = -n
	}
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	if negative {
		s = "-" + s
	}
	return s
}
