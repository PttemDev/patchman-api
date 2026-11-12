package handlers

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/forwarder"
	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// Types: step definitions (parsed from TestCase.Steps JSON)
// ---------------------------------------------------------------------------

type executableStep struct {
	StepNo      int               `json:"stepNo"`
	Type        string            `json:"type"` // request | assertion | extract | wait
	Action      string            `json:"action"`
	Expected    string            `json:"expected"`
	Request     *stepRequest      `json:"request,omitempty"`
	Assertions  []stepAssertion   `json:"assertions,omitempty"`
	Extractions []stepExtraction  `json:"extractions,omitempty"`
	WaitMs      int               `json:"waitMs,omitempty"`
}

type stepRequest struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

type stepAssertion struct {
	Type     string `json:"type"`               // status | body_contains | json_path | response_time | header
	Operator string `json:"operator"`           // equals | not_equals | contains | not_contains | exists | not_empty | greater_than | less_than | matches
	Value    string `json:"value,omitempty"`
	Path     string `json:"path,omitempty"`     // for json_path
	Key      string `json:"key,omitempty"`      // for header
}

type stepExtraction struct {
	Variable string `json:"variable"`
	Source   string `json:"source"` // json_path | header | body | status
	Path     string `json:"path,omitempty"`
	Key      string `json:"key,omitempty"`
}

// ---------------------------------------------------------------------------
// Types: execution results
// ---------------------------------------------------------------------------

type stepResultResponse struct {
	StatusCode int               `json:"statusCode"`
	Body       string            `json:"body"`
	Headers    map[string]string `json:"headers"`
	Duration   int64             `json:"duration"`
}

type assertionResult struct {
	Type     string `json:"type"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
	Passed   bool   `json:"passed"`
}

type extractionResult struct {
	Variable string `json:"variable"`
	Value    string `json:"value"`
}

type stepResult struct {
	StepNo      int                 `json:"stepNo"`
	Type        string              `json:"type"`
	Status      string              `json:"status"` // passed | failed | error | skipped
	Duration    int64               `json:"duration"`
	Request     *stepRequest        `json:"request,omitempty"`
	Response    *stepResultResponse `json:"response,omitempty"`
	Assertions  []assertionResult   `json:"assertions,omitempty"`
	Extractions []extractionResult  `json:"extractions,omitempty"`
	Error       string              `json:"error,omitempty"`
}

type executionSummary struct {
	Total   int `json:"total"`
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
}

type executionResult struct {
	Status    string            `json:"status"` // passed | failed | error
	Duration  int64             `json:"duration"`
	Steps     []stepResult      `json:"steps"`
	Variables map[string]string `json:"variables"`
	Summary   executionSummary  `json:"summary"`
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

type TestExecutorHandler struct {
	db  *gorm.DB
	fwd *forwarder.Client
}

func NewTestExecutorHandler(db *gorm.DB) *TestExecutorHandler {
	return &TestExecutorHandler{db: db}
}

// WithForwarder attaches an upstream mirror for TestRun writes. See
// ProxyHandler.WithForwarder for the same rationale.
func (h *TestExecutorHandler) WithForwarder(fwd *forwarder.Client) *TestExecutorHandler {
	h.fwd = fwd
	return h
}

type executeInput struct {
	EnvironmentID string `json:"environmentId"`
}

// ExecuteTestCase handles POST /api/test-cases/:id/execute
func (h *TestExecutorHandler) ExecuteTestCase(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")

	var tc models.TestCase
	if err := h.db.Where("id = ? AND user_id = ?", id, userID).First(&tc).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Test case not found"})
		return
	}

	var input executeInput
	// Allow empty body
	if err := c.ShouldBindJSON(&input); err != nil {
		input = executeInput{}
	}

	// Load environment variables (owned or shared env — see resolveRunEnvVars)
	envVars, ok := resolveRunEnvVars(h.db, userID, input.EnvironmentID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "Environment not found"})
		return
	}

	result := executeTestCaseSteps(tc.Steps, envVars)
	c.JSON(http.StatusOK, gin.H{"data": result})
}

// ExecuteTestPlan handles POST /api/test-plans/:id/execute
func (h *TestExecutorHandler) ExecuteTestPlan(c *gin.Context) {
	userID := c.GetString("userID")
	planID := c.Param("id")

	var plan models.TestPlan
	if err := h.db.Where("id = ? AND user_id = ?", planID, userID).First(&plan).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Test plan not found"})
		return
	}

	var input executeInput
	if err := c.ShouldBindJSON(&input); err != nil {
		input = executeInput{}
	}

	// Load environment variables (owned or shared env — see resolveRunEnvVars)
	envVars, ok := resolveRunEnvVars(h.db, userID, input.EnvironmentID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "Environment not found"})
		return
	}

	// Parse test case IDs from plan
	var testCaseIDs []string
	if plan.TestCaseIDs != "" {
		if err := json.Unmarshal([]byte(plan.TestCaseIDs), &testCaseIDs); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to parse test case IDs from plan"})
			return
		}
	}

	if len(testCaseIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Test plan has no test cases"})
		return
	}

	// Create a TestRun record
	now := time.Now()
	run := models.TestRun{
		ID:          uuid.New().String(),
		TestPlanID:  planID,
		UserID:      userID,
		Environment: input.EnvironmentID,
		Status:      "running",
		Results:     "[]",
		StartedAt:   now,
	}
	if err := h.db.Create(&run).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create test run"})
		return
	}

	// Mirror the "running" state upstream so the remote BE has a row even if
	// the local executor crashes before completion.
	if h.fwd != nil {
		h.fwd.ForwardTestRun(run)
	}

	// Execute each test case sequentially
	type testCaseResult struct {
		TestCaseID string          `json:"testCaseId"`
		Title      string          `json:"title"`
		Status     string          `json:"status"`
		Duration   int64           `json:"duration"`
		Result     executionResult `json:"result"`
	}

	var tcResults []testCaseResult
	overallStatus := "passed"
	totalDuration := int64(0)

	for _, tcID := range testCaseIDs {
		var tc models.TestCase
		if err := h.db.Where("id = ? AND user_id = ?", tcID, userID).First(&tc).Error; err != nil {
			tcResults = append(tcResults, testCaseResult{
				TestCaseID: tcID,
				Title:      "",
				Status:     "error",
				Duration:   0,
				Result: executionResult{
					Status:    "error",
					Duration:  0,
					Steps:     []stepResult{},
					Variables: map[string]string{},
					Summary:   executionSummary{},
				},
			})
			overallStatus = "failed"
			continue
		}

		result := executeTestCaseSteps(tc.Steps, envVars)
		totalDuration += result.Duration

		tcResults = append(tcResults, testCaseResult{
			TestCaseID: tcID,
			Title:      tc.Title,
			Status:     result.Status,
			Duration:   result.Duration,
			Result:     result,
		})

		if result.Status != "passed" {
			overallStatus = "failed"
		}
	}

	// Update the TestRun with results
	resultsJSON, _ := json.Marshal(tcResults)
	completedAt := time.Now()
	h.db.Model(&run).Updates(map[string]interface{}{
		"status":       overallStatus,
		"results":      string(resultsJSON),
		"completed_at": &completedAt,
	})

	// Re-fetch the run
	h.db.First(&run, "id = ?", run.ID)

	// Forward the completed state upstream. The remote endpoint upserts on
	// id so this is idempotent with the earlier "running" forward.
	if h.fwd != nil {
		h.fwd.ForwardTestRun(run)
	}

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"testRun":      serializeTestRun(run),
			"testCases":    tcResults,
			"overallStatus": overallStatus,
			"totalDuration": totalDuration,
		},
	})
}

// ---------------------------------------------------------------------------
// Core execution engine
// ---------------------------------------------------------------------------

// executeTestCaseSteps parses the steps JSON and executes each step in order.
// envVars provides initial variable values from the environment. Extracted
// variables are merged in as execution progresses.
func executeTestCaseSteps(stepsJSON string, envVars map[string]string) executionResult {
	result := executionResult{
		Status:    "passed",
		Variables: make(map[string]string),
		Steps:     []stepResult{},
		Summary:   executionSummary{},
	}

	// Copy env vars into working variables
	for k, v := range envVars {
		result.Variables[k] = v
	}

	if stepsJSON == "" {
		return result
	}

	var steps []executableStep
	if err := json.Unmarshal([]byte(stepsJSON), &steps); err != nil {
		result.Status = "error"
		result.Steps = append(result.Steps, stepResult{
			StepNo: 0,
			Type:   "parse",
			Status: "error",
			Error:  fmt.Sprintf("Failed to parse steps JSON: %v", err),
		})
		result.Summary.Total = 1
		return result
	}

	result.Summary.Total = len(steps)

	start := time.Now()
	var lastResponse *stepResultResponse
	aborted := false

	for _, step := range steps {
		if aborted {
			sr := stepResult{
				StepNo: step.StepNo,
				Type:   step.Type,
				Status: "skipped",
			}
			result.Steps = append(result.Steps, sr)
			result.Summary.Skipped++
			continue
		}

		var sr stepResult

		switch step.Type {
		case "request":
			sr = executeRequestStep(step, result.Variables)
			if sr.Response != nil {
				lastResponse = sr.Response
			}
			// If the request step also has inline assertions, evaluate them
			if len(step.Assertions) > 0 && sr.Status != "error" && lastResponse != nil {
				sr.Assertions = evaluateAssertions(step.Assertions, lastResponse, result.Variables)
				allPassed := true
				for _, a := range sr.Assertions {
					if !a.Passed {
						allPassed = false
						break
					}
				}
				if !allPassed {
					sr.Status = "failed"
				}
			}
			// If the request step also has inline extractions, process them
			if len(step.Extractions) > 0 && sr.Status != "error" && lastResponse != nil {
				sr.Extractions = performExtractions(step.Extractions, lastResponse, result.Variables)
			}

		case "assertion":
			sr = executeAssertionStep(step, lastResponse, result.Variables)

		case "extract":
			sr = executeExtractionStep(step, lastResponse, result.Variables)

		case "wait":
			sr = executeWaitStep(step)

		default:
			sr = stepResult{
				StepNo: step.StepNo,
				Type:   step.Type,
				Status: "error",
				Error:  fmt.Sprintf("Unknown step type: %s", step.Type),
			}
		}

		result.Steps = append(result.Steps, sr)

		switch sr.Status {
		case "passed":
			result.Summary.Passed++
		case "failed":
			result.Summary.Failed++
			result.Status = "failed"
		case "error":
			result.Summary.Failed++
			result.Status = "failed"
			aborted = true
		case "skipped":
			result.Summary.Skipped++
		}
	}

	result.Duration = time.Since(start).Milliseconds()
	return result
}

// ---------------------------------------------------------------------------
// Step executors
// ---------------------------------------------------------------------------

func executeRequestStep(step executableStep, vars map[string]string) stepResult {
	sr := stepResult{
		StepNo: step.StepNo,
		Type:   "request",
	}

	if step.Request == nil {
		sr.Status = "error"
		sr.Error = "Request step missing 'request' field"
		return sr
	}

	reqStart := time.Now()

	// Substitute variables
	method := strings.ToUpper(substituteVariables(step.Request.Method, vars))
	rawURL := substituteVariables(step.Request.URL, vars)
	body := substituteVariables(step.Request.Body, vars)

	headers := make(map[string]string)
	for k, v := range step.Request.Headers {
		headers[substituteVariables(k, vars)] = substituteVariables(v, vars)
	}

	sr.Request = &stepRequest{
		Method:  method,
		URL:     rawURL,
		Headers: headers,
		Body:    body,
	}

	// Parse & validate URL
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		sr.Status = "error"
		sr.Error = fmt.Sprintf("Invalid URL: %v", err)
		sr.Duration = time.Since(reqStart).Milliseconds()
		return sr
	}
	if parsedURL.Scheme == "" {
		sr.Status = "error"
		sr.Error = fmt.Sprintf("URL missing scheme (http/https): %s", rawURL)
		sr.Duration = time.Since(reqStart).Milliseconds()
		return sr
	}

	// Build body reader
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}

	// Create HTTP request
	httpReq, err := http.NewRequest(method, parsedURL.String(), bodyReader)
	if err != nil {
		sr.Status = "error"
		sr.Error = fmt.Sprintf("Failed to create request: %v", err)
		sr.Duration = time.Since(reqStart).Milliseconds()
		return sr
	}

	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}

	if httpReq.Header.Get("Accept-Encoding") == "" {
		httpReq.Header.Set("Accept-Encoding", services.DefaultAcceptEncoding)
	}

	// Auto-set Content-Type for bodies that look like JSON
	if body != "" && httpReq.Header.Get("Content-Type") == "" {
		trimmed := strings.TrimSpace(body)
		if (strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}")) ||
			(strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]")) {
			httpReq.Header.Set("Content-Type", "application/json")
		}
	}

	// Execute
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
			DisableCompression: true,
			// Per-request transport is never reused/closed — disable
			// keep-alives so idle connections don't leak fds over time.
			DisableKeepAlives: true,
		},
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		sr.Status = "error"
		sr.Error = fmt.Sprintf("Request failed: %v", err)
		sr.Duration = time.Since(reqStart).Milliseconds()
		return sr
	}
	defer resp.Body.Close()

	respBody, err := services.DecodeResponseBody(resp, 0)
	if err != nil {
		sr.Status = "error"
		sr.Error = fmt.Sprintf("Failed to read response body: %v", err)
		sr.Duration = time.Since(reqStart).Milliseconds()
		return sr
	}

	duration := time.Since(reqStart).Milliseconds()

	respHeaders := make(map[string]string)
	for k, v := range resp.Header {
		respHeaders[k] = strings.Join(v, ", ")
	}

	sr.Response = &stepResultResponse{
		StatusCode: resp.StatusCode,
		Body:       string(respBody),
		Headers:    respHeaders,
		Duration:   duration,
	}
	sr.Duration = duration
	sr.Status = "passed"

	return sr
}

func executeAssertionStep(step executableStep, lastResponse *stepResultResponse, vars map[string]string) stepResult {
	sr := stepResult{
		StepNo: step.StepNo,
		Type:   "assertion",
	}

	if lastResponse == nil {
		sr.Status = "error"
		sr.Error = "No previous response to assert against"
		return sr
	}

	if len(step.Assertions) == 0 {
		sr.Status = "passed"
		return sr
	}

	start := time.Now()
	sr.Assertions = evaluateAssertions(step.Assertions, lastResponse, vars)
	sr.Duration = time.Since(start).Milliseconds()

	sr.Status = "passed"
	for _, a := range sr.Assertions {
		if !a.Passed {
			sr.Status = "failed"
			break
		}
	}

	return sr
}

func executeExtractionStep(step executableStep, lastResponse *stepResultResponse, vars map[string]string) stepResult {
	sr := stepResult{
		StepNo: step.StepNo,
		Type:   "extract",
	}

	if lastResponse == nil {
		sr.Status = "error"
		sr.Error = "No previous response to extract from"
		return sr
	}

	start := time.Now()
	sr.Extractions = performExtractions(step.Extractions, lastResponse, vars)
	sr.Duration = time.Since(start).Milliseconds()
	sr.Status = "passed"

	return sr
}

func executeWaitStep(step executableStep) stepResult {
	sr := stepResult{
		StepNo: step.StepNo,
		Type:   "wait",
	}

	waitMs := step.WaitMs
	if waitMs <= 0 {
		waitMs = 0
	}
	// Cap at 30 seconds to prevent abuse
	if waitMs > 30000 {
		waitMs = 30000
	}

	start := time.Now()
	if waitMs > 0 {
		time.Sleep(time.Duration(waitMs) * time.Millisecond)
	}
	sr.Duration = time.Since(start).Milliseconds()
	sr.Status = "passed"

	return sr
}

// ---------------------------------------------------------------------------
// Assertion evaluation
// ---------------------------------------------------------------------------

func evaluateAssertions(assertions []stepAssertion, resp *stepResultResponse, vars map[string]string) []assertionResult {
	var results []assertionResult

	for _, a := range assertions {
		ar := assertionResult{
			Type:     a.Type,
			Expected: substituteVariables(a.Value, vars),
		}

		switch a.Type {
		case "status":
			ar.Actual = strconv.Itoa(resp.StatusCode)
			ar.Passed = evaluateOperator(ar.Actual, a.Operator, ar.Expected)

		case "body_contains":
			ar.Actual = truncateForDisplay(resp.Body, 200)
			ar.Passed = evaluateOperator(resp.Body, a.Operator, ar.Expected)

		case "json_path":
			path := substituteVariables(a.Path, vars)
			val := resolveJSONPath(resp.Body, path)
			ar.Actual = val
			ar.Expected = substituteVariables(a.Value, vars)
			ar.Passed = evaluateOperator(val, a.Operator, ar.Expected)

		case "response_time":
			ar.Actual = strconv.FormatInt(resp.Duration, 10)
			ar.Passed = evaluateOperator(ar.Actual, a.Operator, ar.Expected)

		case "header":
			key := substituteVariables(a.Key, vars)
			headerVal := ""
			// Case-insensitive header lookup
			for k, v := range resp.Headers {
				if strings.EqualFold(k, key) {
					headerVal = v
					break
				}
			}
			ar.Actual = headerVal
			ar.Passed = evaluateOperator(headerVal, a.Operator, ar.Expected)

		default:
			ar.Actual = ""
			ar.Passed = false
		}

		results = append(results, ar)
	}

	return results
}

func evaluateOperator(actual, operator, expected string) bool {
	switch operator {
	case "equals":
		return actual == expected

	case "not_equals":
		return actual != expected

	case "contains":
		return strings.Contains(actual, expected)

	case "not_contains":
		return !strings.Contains(actual, expected)

	case "exists":
		return actual != "" && actual != "<nil>"

	case "not_empty":
		return strings.TrimSpace(actual) != ""

	case "greater_than":
		av, errA := strconv.ParseFloat(actual, 64)
		ev, errE := strconv.ParseFloat(expected, 64)
		if errA != nil || errE != nil {
			return false
		}
		return av > ev

	case "less_than":
		av, errA := strconv.ParseFloat(actual, 64)
		ev, errE := strconv.ParseFloat(expected, 64)
		if errA != nil || errE != nil {
			return false
		}
		return av < ev

	case "matches":
		re, err := regexp.Compile(expected)
		if err != nil {
			return false
		}
		return re.MatchString(actual)

	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// Variable extraction
// ---------------------------------------------------------------------------

func performExtractions(extractions []stepExtraction, resp *stepResultResponse, vars map[string]string) []extractionResult {
	var results []extractionResult

	for _, ext := range extractions {
		er := extractionResult{
			Variable: ext.Variable,
		}

		switch ext.Source {
		case "json_path":
			path := substituteVariables(ext.Path, vars)
			er.Value = resolveJSONPath(resp.Body, path)

		case "header":
			key := substituteVariables(ext.Key, vars)
			for k, v := range resp.Headers {
				if strings.EqualFold(k, key) {
					er.Value = v
					break
				}
			}

		case "body":
			er.Value = resp.Body

		case "status":
			er.Value = strconv.Itoa(resp.StatusCode)

		default:
			er.Value = ""
		}

		// Store the extracted variable for subsequent steps
		if ext.Variable != "" {
			vars[ext.Variable] = er.Value
		}

		results = append(results, er)
	}

	return results
}

// ---------------------------------------------------------------------------
// Simple JSON path resolver
// Supports:
//   $.field              -> top-level field
//   $.field.subfield     -> nested field
//   $.array[0]           -> array index
//   $.array[0].field     -> array element field
//   $.array.length       -> array length (count)
// ---------------------------------------------------------------------------

func resolveJSONPath(body string, path string) string {
	if body == "" || path == "" {
		return ""
	}

	// Strip leading "$." or "$"
	path = strings.TrimPrefix(path, "$.")
	path = strings.TrimPrefix(path, "$")
	if path == "" {
		return body
	}

	// Parse body into generic interface
	var data interface{}
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		return ""
	}

	// Tokenize path: split by "." but handle array indices like "array[0]"
	tokens := tokenizePath(path)
	current := data

	for _, token := range tokens {
		if current == nil {
			return ""
		}

		// Check for array index: e.g. "items[0]"
		if idx := strings.Index(token, "["); idx != -1 {
			fieldName := token[:idx]
			indexStr := strings.TrimSuffix(token[idx+1:], "]")
			arrayIndex, err := strconv.Atoi(indexStr)
			if err != nil {
				return ""
			}

			// Navigate to the field first (if field name is not empty)
			if fieldName != "" {
				obj, ok := current.(map[string]interface{})
				if !ok {
					return ""
				}
				current, ok = obj[fieldName]
				if !ok {
					return ""
				}
			}

			// Index into the array
			arr, ok := current.([]interface{})
			if !ok {
				return ""
			}
			if arrayIndex < 0 || arrayIndex >= len(arr) {
				return ""
			}
			current = arr[arrayIndex]
			continue
		}

		// Special "length" token for arrays
		if token == "length" {
			arr, ok := current.([]interface{})
			if ok {
				return strconv.Itoa(len(arr))
			}
			// For strings, return string length
			s, ok := current.(string)
			if ok {
				return strconv.Itoa(len(s))
			}
			return ""
		}

		// Regular field navigation
		obj, ok := current.(map[string]interface{})
		if !ok {
			return ""
		}
		current, ok = obj[token]
		if !ok {
			return "<nil>"
		}
	}

	return interfaceToString(current)
}

// tokenizePath splits a JSON path string by "." while keeping array indices intact.
// Example: "data[0].name" -> ["data[0]", "name"]
func tokenizePath(path string) []string {
	var tokens []string
	current := ""
	inBracket := false

	for _, ch := range path {
		switch {
		case ch == '[':
			inBracket = true
			current += string(ch)
		case ch == ']':
			inBracket = false
			current += string(ch)
		case ch == '.' && !inBracket:
			if current != "" {
				tokens = append(tokens, current)
			}
			current = ""
		default:
			current += string(ch)
		}
	}
	if current != "" {
		tokens = append(tokens, current)
	}

	return tokens
}

// interfaceToString converts a parsed JSON value to its string representation.
func interfaceToString(v interface{}) string {
	if v == nil {
		return "<nil>"
	}
	switch val := v.(type) {
	case string:
		return val
	case float64:
		// Render integers without decimals
		if val == float64(int64(val)) {
			return strconv.FormatInt(int64(val), 10)
		}
		return strconv.FormatFloat(val, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(val)
	case nil:
		return "<nil>"
	default:
		// For objects/arrays, marshal back to JSON
		b, err := json.Marshal(val)
		if err != nil {
			return fmt.Sprintf("%v", val)
		}
		return string(b)
	}
}

// truncateForDisplay shortens a string for display in assertion results.
func truncateForDisplay(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
