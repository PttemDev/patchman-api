package handlers

import (
	"net/http"

	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// ScriptHandler handles script execution requests.
type ScriptHandler struct {
	db *gorm.DB
}

// NewScriptHandler creates a new ScriptHandler.
func NewScriptHandler(db *gorm.DB) *ScriptHandler {
	return &ScriptHandler{db: db}
}

// executeScriptInput is the JSON body for POST /api/script/execute.
type executeScriptInput struct {
	Script  string              `json:"script" binding:"required"`
	Type    string              `json:"type" binding:"required,oneof=pre-request test"`
	Context scriptContextInput  `json:"context"`
}

// scriptContextInput holds the request/response context passed from the frontend.
type scriptContextInput struct {
	Method          string            `json:"method"`
	URL             string            `json:"url"`
	Headers         map[string]string `json:"headers"`
	Body            string            `json:"body"`
	StatusCode      int               `json:"statusCode"`
	ResponseBody    string            `json:"responseBody"`
	ResponseHeaders map[string]string `json:"responseHeaders"`
	ResponseTime    int               `json:"responseTime"`
	EnvVars         map[string]string `json:"envVars"`
}

// executeScriptOutput is the JSON response from POST /api/script/execute.
type executeScriptOutput struct {
	ConsoleOutput  []string            `json:"consoleOutput"`
	TestResults    []services.TestResult `json:"testResults"`
	UpdatedEnvVars map[string]string   `json:"updatedEnvVars"`
	Error          string              `json:"error,omitempty"`
	// For pre-request scripts, return potentially modified request fields
	UpdatedHeaders map[string]string `json:"updatedHeaders,omitempty"`
}

// Execute runs a JavaScript script with a Postman-compatible pm.* API.
//
// POST /api/script/execute
//
//	Body: { script, type, context }
//	Response: { consoleOutput, testResults, updatedEnvVars, error? }
func (h *ScriptHandler) Execute(c *gin.Context) {
	var input executeScriptInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Build the script context from input
	ctx := &services.ScriptContext{
		Method:          input.Context.Method,
		URL:             input.Context.URL,
		Headers:         input.Context.Headers,
		Body:            input.Context.Body,
		StatusCode:      input.Context.StatusCode,
		ResponseBody:    input.Context.ResponseBody,
		ResponseHeaders: input.Context.ResponseHeaders,
		ResponseTime:    input.Context.ResponseTime,
		EnvVars:         input.Context.EnvVars,
	}

	// Ensure slices/maps are initialised so JSON output is [] / {} not null
	if ctx.Headers == nil {
		ctx.Headers = make(map[string]string)
	}
	if ctx.ResponseHeaders == nil {
		ctx.ResponseHeaders = make(map[string]string)
	}
	if ctx.EnvVars == nil {
		ctx.EnvVars = make(map[string]string)
	}
	ctx.ConsoleOutput = make([]string, 0)
	ctx.TestResults = make([]services.TestResult, 0)

	engine := services.NewScriptEngine()
	err := engine.Execute(input.Script, ctx)

	output := executeScriptOutput{
		ConsoleOutput:  ctx.ConsoleOutput,
		TestResults:    ctx.TestResults,
		UpdatedEnvVars: ctx.EnvVars,
		UpdatedHeaders: ctx.Headers,
	}

	// Ensure non-nil slices for clean JSON
	if output.ConsoleOutput == nil {
		output.ConsoleOutput = make([]string, 0)
	}
	if output.TestResults == nil {
		output.TestResults = make([]services.TestResult, 0)
	}
	if output.UpdatedEnvVars == nil {
		output.UpdatedEnvVars = make(map[string]string)
	}

	if err != nil {
		output.Error = err.Error()
		// Still return 200 with the error in the body so the frontend can
		// show console output and partial test results alongside the error.
		c.JSON(http.StatusOK, gin.H{"data": output})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": output})
}
