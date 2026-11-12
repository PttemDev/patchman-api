package services

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/dop251/goja"
)

// ScriptEngine executes JavaScript scripts in a sandboxed goja runtime,
// providing a Postman-compatible pm.* API for pre-request and test scripts.
type ScriptEngine struct{}

// ScriptContext holds all the data exchanged between Go and the JS runtime.
type ScriptContext struct {
	// Request info (available in both pre-request and test scripts)
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`

	// Response info (only available in test scripts)
	StatusCode      int               `json:"statusCode"`
	ResponseBody    string            `json:"responseBody"`
	ResponseHeaders map[string]string `json:"responseHeaders"`
	ResponseTime    int               `json:"responseTime"`

	// Environment variables (can be read/written)
	EnvVars map[string]string `json:"envVars"`

	// Console output captured from console.log
	ConsoleOutput []string `json:"consoleOutput"`

	// Test results (only for test scripts)
	TestResults []TestResult `json:"testResults"`
}

// TestResult represents the outcome of a single pm.test() call.
type TestResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Error  string `json:"error,omitempty"`
}

// NewScriptEngine creates a new ScriptEngine.
func NewScriptEngine() *ScriptEngine {
	return &ScriptEngine{}
}

// Execute runs a JavaScript script with a Postman-compatible API.
// The script has access to pm.request, pm.response, pm.environment,
// pm.variables, pm.test, pm.expect, and console.log.
func (e *ScriptEngine) Execute(script string, ctx *ScriptContext) error {
	if script == "" {
		return nil
	}

	// Initialise nil maps so the JS side never sees undefined where it expects an object.
	if ctx.Headers == nil {
		ctx.Headers = make(map[string]string)
	}
	if ctx.ResponseHeaders == nil {
		ctx.ResponseHeaders = make(map[string]string)
	}
	if ctx.EnvVars == nil {
		ctx.EnvVars = make(map[string]string)
	}

	vm := goja.New()

	// Impose a 10-second execution timeout to prevent infinite loops.
	timer := time.AfterFunc(10*time.Second, func() {
		vm.Interrupt("script execution timed out (10s limit)")
	})
	defer timer.Stop()

	// ------------------------------------------------------------------
	// console.log
	// ------------------------------------------------------------------
	consoleObj := vm.NewObject()
	_ = consoleObj.Set("log", func(call goja.FunctionCall) goja.Value {
		parts := make([]string, 0, len(call.Arguments))
		for _, arg := range call.Arguments {
			parts = append(parts, formatValue(vm, arg))
		}
		ctx.ConsoleOutput = append(ctx.ConsoleOutput, strings.Join(parts, " "))
		return goja.Undefined()
	})
	_ = consoleObj.Set("warn", func(call goja.FunctionCall) goja.Value {
		parts := make([]string, 0, len(call.Arguments))
		for _, arg := range call.Arguments {
			parts = append(parts, formatValue(vm, arg))
		}
		ctx.ConsoleOutput = append(ctx.ConsoleOutput, "[warn] "+strings.Join(parts, " "))
		return goja.Undefined()
	})
	_ = consoleObj.Set("error", func(call goja.FunctionCall) goja.Value {
		parts := make([]string, 0, len(call.Arguments))
		for _, arg := range call.Arguments {
			parts = append(parts, formatValue(vm, arg))
		}
		ctx.ConsoleOutput = append(ctx.ConsoleOutput, "[error] "+strings.Join(parts, " "))
		return goja.Undefined()
	})
	_ = vm.Set("console", consoleObj)

	// ------------------------------------------------------------------
	// pm object
	// ------------------------------------------------------------------
	pmObj := vm.NewObject()

	// --- pm.request ---
	if err := e.setupRequest(vm, pmObj, ctx); err != nil {
		return fmt.Errorf("failed to setup pm.request: %w", err)
	}

	// --- pm.response (only when we have response data) ---
	if err := e.setupResponse(vm, pmObj, ctx); err != nil {
		return fmt.Errorf("failed to setup pm.response: %w", err)
	}

	// --- pm.environment ---
	envObj := vm.NewObject()
	_ = envObj.Set("get", func(call goja.FunctionCall) goja.Value {
		key := call.Argument(0).String()
		if val, ok := ctx.EnvVars[key]; ok {
			return vm.ToValue(val)
		}
		return goja.Undefined()
	})
	_ = envObj.Set("set", func(call goja.FunctionCall) goja.Value {
		key := call.Argument(0).String()
		val := call.Argument(1).String()
		ctx.EnvVars[key] = val
		return goja.Undefined()
	})
	_ = envObj.Set("unset", func(call goja.FunctionCall) goja.Value {
		key := call.Argument(0).String()
		delete(ctx.EnvVars, key)
		return goja.Undefined()
	})
	_ = envObj.Set("has", func(call goja.FunctionCall) goja.Value {
		key := call.Argument(0).String()
		_, ok := ctx.EnvVars[key]
		return vm.ToValue(ok)
	})
	_ = pmObj.Set("environment", envObj)

	// --- pm.variables (aliases pm.environment for now) ---
	_ = pmObj.Set("variables", envObj)

	// --- pm.test ---
	_ = pmObj.Set("test", func(call goja.FunctionCall) goja.Value {
		name := call.Argument(0).String()
		fn, ok := goja.AssertFunction(call.Argument(1))
		if !ok {
			ctx.TestResults = append(ctx.TestResults, TestResult{
				Name:   name,
				Passed: false,
				Error:  "second argument to pm.test must be a function",
			})
			return goja.Undefined()
		}

		_, err := fn(goja.Undefined())
		if err != nil {
			ctx.TestResults = append(ctx.TestResults, TestResult{
				Name:   name,
				Passed: false,
				Error:  err.Error(),
			})
		} else {
			ctx.TestResults = append(ctx.TestResults, TestResult{
				Name:   name,
				Passed: true,
			})
		}
		return goja.Undefined()
	})

	// --- pm.expect ---
	_ = pmObj.Set("expect", func(call goja.FunctionCall) goja.Value {
		actual := call.Argument(0)
		return e.buildExpectChain(vm, actual, ctx)
	})

	_ = vm.Set("pm", pmObj)

	// ------------------------------------------------------------------
	// Execute the script
	// ------------------------------------------------------------------
	_, err := vm.RunString(script)
	if err != nil {
		return fmt.Errorf("script error: %w", err)
	}

	return nil
}

// setupRequest creates the pm.request object on pmObj.
func (e *ScriptEngine) setupRequest(vm *goja.Runtime, pmObj *goja.Object, ctx *ScriptContext) error {
	reqObj := vm.NewObject()
	_ = reqObj.Set("method", ctx.Method)
	_ = reqObj.Set("url", ctx.URL)
	_ = reqObj.Set("body", ctx.Body)

	// pm.request.headers as an object with get/set helpers
	headersObj := vm.NewObject()
	_ = headersObj.Set("get", func(call goja.FunctionCall) goja.Value {
		key := call.Argument(0).String()
		// Case-insensitive header lookup
		for k, v := range ctx.Headers {
			if strings.EqualFold(k, key) {
				return vm.ToValue(v)
			}
		}
		return goja.Undefined()
	})
	_ = headersObj.Set("set", func(call goja.FunctionCall) goja.Value {
		key := call.Argument(0).String()
		val := call.Argument(1).String()
		ctx.Headers[key] = val
		return goja.Undefined()
	})
	_ = headersObj.Set("has", func(call goja.FunctionCall) goja.Value {
		key := call.Argument(0).String()
		for k := range ctx.Headers {
			if strings.EqualFold(k, key) {
				return vm.ToValue(true)
			}
		}
		return vm.ToValue(false)
	})
	_ = headersObj.Set("remove", func(call goja.FunctionCall) goja.Value {
		key := call.Argument(0).String()
		for k := range ctx.Headers {
			if strings.EqualFold(k, key) {
				delete(ctx.Headers, k)
				break
			}
		}
		return goja.Undefined()
	})
	// Also expose raw header entries for iteration
	for k, v := range ctx.Headers {
		_ = headersObj.Set(k, v)
	}
	_ = reqObj.Set("headers", headersObj)

	_ = pmObj.Set("request", reqObj)
	return nil
}

// setupResponse creates the pm.response object on pmObj.
func (e *ScriptEngine) setupResponse(vm *goja.Runtime, pmObj *goja.Object, ctx *ScriptContext) error {
	respObj := vm.NewObject()
	_ = respObj.Set("code", ctx.StatusCode)
	_ = respObj.Set("statusCode", ctx.StatusCode)
	_ = respObj.Set("status", statusText(ctx.StatusCode))
	_ = respObj.Set("responseTime", ctx.ResponseTime)
	_ = respObj.Set("time", ctx.ResponseTime)

	// pm.response.headers
	respHeadersObj := vm.NewObject()
	_ = respHeadersObj.Set("get", func(call goja.FunctionCall) goja.Value {
		key := call.Argument(0).String()
		for k, v := range ctx.ResponseHeaders {
			if strings.EqualFold(k, key) {
				return vm.ToValue(v)
			}
		}
		return goja.Undefined()
	})
	_ = respHeadersObj.Set("has", func(call goja.FunctionCall) goja.Value {
		key := call.Argument(0).String()
		for k := range ctx.ResponseHeaders {
			if strings.EqualFold(k, key) {
				return vm.ToValue(true)
			}
		}
		return vm.ToValue(false)
	})
	for k, v := range ctx.ResponseHeaders {
		_ = respHeadersObj.Set(k, v)
	}
	_ = respObj.Set("headers", respHeadersObj)

	// pm.response.text()
	_ = respObj.Set("text", func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(ctx.ResponseBody)
	})

	// pm.response.json()
	_ = respObj.Set("json", func(call goja.FunctionCall) goja.Value {
		var parsed interface{}
		if err := json.Unmarshal([]byte(ctx.ResponseBody), &parsed); err != nil {
			panic(vm.NewGoError(fmt.Errorf("failed to parse response body as JSON: %w", err)))
		}
		return vm.ToValue(parsed)
	})

	_ = pmObj.Set("response", respObj)
	return nil
}

// buildExpectChain returns a JS object that supports Postman-style chaining:
//
//	pm.expect(value).to.equal(x)
//	pm.expect(value).to.be.a("string")
//	pm.expect(value).to.have.property("name")
//	pm.expect(value).to.include("substr")
//	pm.expect(value).to.be.above(5)
//	pm.expect(value).to.be.below(10)
//	pm.expect(value).to.have.status(200)
//	pm.expect(value).to.have.header("Content-Type")
//	pm.expect(value).to.have.jsonBody("path.to.field")
//	pm.expect(value).to.be.ok  (getter, status 200-299)
func (e *ScriptEngine) buildExpectChain(vm *goja.Runtime, actual goja.Value, ctx *ScriptContext) goja.Value {
	chain := vm.NewObject()
	toObj := vm.NewObject()
	beObj := vm.NewObject()
	haveObj := vm.NewObject()
	notObj := vm.NewObject()
	notBeObj := vm.NewObject()
	notHaveObj := vm.NewObject()

	// Helper to get the Go value from goja
	goActual := func() interface{} {
		return actual.Export()
	}

	// ---- to.equal(expected) ----
	_ = toObj.Set("equal", func(call goja.FunctionCall) goja.Value {
		expected := call.Argument(0).Export()
		act := goActual()
		if !deepEqual(act, expected) {
			panic(vm.NewGoError(fmt.Errorf("expected %v to equal %v", act, expected)))
		}
		return chain
	})

	// ---- to.include(value) ----
	_ = toObj.Set("include", func(call goja.FunctionCall) goja.Value {
		expected := call.Argument(0).Export()
		act := goActual()
		if !includes(act, expected) {
			panic(vm.NewGoError(fmt.Errorf("expected %v to include %v", act, expected)))
		}
		return chain
	})

	// ---- to.be ----
	// to.be.a(type)
	_ = beObj.Set("a", func(call goja.FunctionCall) goja.Value {
		expectedType := call.Argument(0).String()
		act := goActual()
		if !isType(act, expectedType) {
			panic(vm.NewGoError(fmt.Errorf("expected %v to be a %s", act, expectedType)))
		}
		return chain
	})
	_ = beObj.Set("an", func(call goja.FunctionCall) goja.Value {
		expectedType := call.Argument(0).String()
		act := goActual()
		if !isType(act, expectedType) {
			panic(vm.NewGoError(fmt.Errorf("expected %v to be an %s", act, expectedType)))
		}
		return chain
	})

	// to.be.above(n)
	_ = beObj.Set("above", func(call goja.FunctionCall) goja.Value {
		threshold := call.Argument(0).ToFloat()
		actNum := toFloat64(goActual())
		if actNum <= threshold {
			panic(vm.NewGoError(fmt.Errorf("expected %v to be above %v", actNum, threshold)))
		}
		return chain
	})

	// to.be.below(n)
	_ = beObj.Set("below", func(call goja.FunctionCall) goja.Value {
		threshold := call.Argument(0).ToFloat()
		actNum := toFloat64(goActual())
		if actNum >= threshold {
			panic(vm.NewGoError(fmt.Errorf("expected %v to be below %v", actNum, threshold)))
		}
		return chain
	})

	// to.be.ok (property/getter) - status 200-299
	// We define it via DefineAccessorProperty so accessing .ok without () works.
	_ = beObj.Set("oneOf", func(call goja.FunctionCall) goja.Value {
		listVal := call.Argument(0)
		listExport := listVal.Export()
		arr, ok := listExport.([]interface{})
		if !ok {
			panic(vm.NewGoError(fmt.Errorf("oneOf expects an array")))
		}
		act := goActual()
		for _, item := range arr {
			if deepEqual(act, item) {
				return chain
			}
		}
		panic(vm.NewGoError(fmt.Errorf("expected %v to be one of %v", act, arr)))
	})

	// to.be.ok as a getter property so `pm.expect(...).to.be.ok` works without ()
	okGetter := vm.ToValue(func(call goja.FunctionCall) goja.Value {
		code := ctx.StatusCode
		if code < 200 || code > 299 {
			panic(vm.NewGoError(fmt.Errorf("expected status code %d to be in range 200-299", code)))
		}
		return chain
	})
	_ = beObj.DefineAccessorProperty("ok", okGetter, nil, goja.FLAG_TRUE, goja.FLAG_TRUE)

	_ = toObj.Set("be", beObj)

	// ---- to.have ----
	// to.have.property(name)
	_ = haveObj.Set("property", func(call goja.FunctionCall) goja.Value {
		propName := call.Argument(0).String()
		act := goActual()
		actMap, ok := act.(map[string]interface{})
		if !ok {
			panic(vm.NewGoError(fmt.Errorf("expected %v to be an object with property '%s'", act, propName)))
		}
		if _, exists := actMap[propName]; !exists {
			panic(vm.NewGoError(fmt.Errorf("expected object to have property '%s'", propName)))
		}
		return chain
	})

	// to.have.status(code)
	_ = haveObj.Set("status", func(call goja.FunctionCall) goja.Value {
		expected := int(call.Argument(0).ToInteger())
		if ctx.StatusCode != expected {
			panic(vm.NewGoError(fmt.Errorf("expected status code %d but got %d", expected, ctx.StatusCode)))
		}
		return chain
	})

	// to.have.header(name)
	_ = haveObj.Set("header", func(call goja.FunctionCall) goja.Value {
		headerName := call.Argument(0).String()
		found := false
		for k := range ctx.ResponseHeaders {
			if strings.EqualFold(k, headerName) {
				found = true
				break
			}
		}
		if !found {
			panic(vm.NewGoError(fmt.Errorf("expected response to have header '%s'", headerName)))
		}
		return chain
	})

	// to.have.jsonBody(path?)
	_ = haveObj.Set("jsonBody", func(call goja.FunctionCall) goja.Value {
		var parsed interface{}
		if err := json.Unmarshal([]byte(ctx.ResponseBody), &parsed); err != nil {
			panic(vm.NewGoError(fmt.Errorf("expected response body to be valid JSON: %v", err)))
		}

		// If a path is provided, navigate to it
		if len(call.Arguments) > 0 && !goja.IsUndefined(call.Argument(0)) {
			path := call.Argument(0).String()
			result, err := navigateJSONPath(parsed, path)
			if err != nil {
				panic(vm.NewGoError(err))
			}
			return vm.ToValue(result)
		}

		return vm.ToValue(parsed)
	})

	// to.have.lengthOf(n)
	_ = haveObj.Set("lengthOf", func(call goja.FunctionCall) goja.Value {
		expected := int(call.Argument(0).ToInteger())
		act := goActual()
		length := getLength(act)
		if length != expected {
			panic(vm.NewGoError(fmt.Errorf("expected length %d but got %d", expected, length)))
		}
		return chain
	})

	_ = toObj.Set("have", haveObj)

	// ---- to.not ----
	// Negated versions of key assertions
	_ = notObj.Set("equal", func(call goja.FunctionCall) goja.Value {
		expected := call.Argument(0).Export()
		act := goActual()
		if deepEqual(act, expected) {
			panic(vm.NewGoError(fmt.Errorf("expected %v to not equal %v", act, expected)))
		}
		return chain
	})
	_ = notObj.Set("include", func(call goja.FunctionCall) goja.Value {
		expected := call.Argument(0).Export()
		act := goActual()
		if includes(act, expected) {
			panic(vm.NewGoError(fmt.Errorf("expected %v to not include %v", act, expected)))
		}
		return chain
	})

	// not.be.*
	_ = notBeObj.Set("a", func(call goja.FunctionCall) goja.Value {
		expectedType := call.Argument(0).String()
		act := goActual()
		if isType(act, expectedType) {
			panic(vm.NewGoError(fmt.Errorf("expected %v to not be a %s", act, expectedType)))
		}
		return chain
	})
	_ = notBeObj.Set("an", func(call goja.FunctionCall) goja.Value {
		expectedType := call.Argument(0).String()
		act := goActual()
		if isType(act, expectedType) {
			panic(vm.NewGoError(fmt.Errorf("expected %v to not be an %s", act, expectedType)))
		}
		return chain
	})
	_ = notObj.Set("be", notBeObj)

	// not.have.*
	_ = notHaveObj.Set("property", func(call goja.FunctionCall) goja.Value {
		propName := call.Argument(0).String()
		act := goActual()
		actMap, ok := act.(map[string]interface{})
		if ok {
			if _, exists := actMap[propName]; exists {
				panic(vm.NewGoError(fmt.Errorf("expected object to not have property '%s'", propName)))
			}
		}
		return chain
	})
	_ = notHaveObj.Set("header", func(call goja.FunctionCall) goja.Value {
		headerName := call.Argument(0).String()
		for k := range ctx.ResponseHeaders {
			if strings.EqualFold(k, headerName) {
				panic(vm.NewGoError(fmt.Errorf("expected response to not have header '%s'", headerName)))
			}
		}
		return chain
	})
	_ = notObj.Set("have", notHaveObj)

	_ = toObj.Set("not", notObj)

	// Wire up chain: chain.to = toObj
	_ = chain.Set("to", toObj)

	return chain
}

// -------------------------------------------------------------------------
// Helpers
// -------------------------------------------------------------------------

// formatValue converts a goja.Value to a human-readable string for console output.
func formatValue(vm *goja.Runtime, val goja.Value) string {
	if val == nil || goja.IsUndefined(val) {
		return "undefined"
	}
	if goja.IsNull(val) {
		return "null"
	}
	exported := val.Export()
	switch v := exported.(type) {
	case string:
		return v
	case map[string]interface{}, []interface{}:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// deepEqual compares two values for equality, handling numeric type mismatches.
func deepEqual(a, b interface{}) bool {
	// Handle numeric comparisons: goja may export numbers as int64 or float64
	af, aIsNum := toNumber(a)
	bf, bIsNum := toNumber(b)
	if aIsNum && bIsNum {
		return af == bf
	}
	return reflect.DeepEqual(a, b)
}

// toNumber attempts to convert an interface{} to float64.
func toNumber(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// toFloat64 converts a value to float64, panicking on failure.
func toFloat64(v interface{}) float64 {
	f, ok := toNumber(v)
	if !ok {
		return 0
	}
	return f
}

// isType checks if a value matches a named type (string, number, boolean, object, array).
func isType(val interface{}, typeName string) bool {
	switch strings.ToLower(typeName) {
	case "string":
		_, ok := val.(string)
		return ok
	case "number":
		_, ok := toNumber(val)
		return ok
	case "boolean", "bool":
		_, ok := val.(bool)
		return ok
	case "object":
		_, ok := val.(map[string]interface{})
		return ok
	case "array":
		_, ok := val.([]interface{})
		return ok
	case "undefined":
		return val == nil
	case "null":
		return val == nil
	default:
		return false
	}
}

// includes checks whether `haystack` contains `needle`.
// Works for strings (substring) and arrays (element equality).
func includes(haystack, needle interface{}) bool {
	// String containment
	if hs, ok := haystack.(string); ok {
		if ns, ok := needle.(string); ok {
			return strings.Contains(hs, ns)
		}
	}
	// Array containment
	if arr, ok := haystack.([]interface{}); ok {
		for _, item := range arr {
			if deepEqual(item, needle) {
				return true
			}
		}
	}
	return false
}

// getLength returns the length of a string or array, or -1 for unsupported types.
func getLength(val interface{}) int {
	switch v := val.(type) {
	case string:
		return len(v)
	case []interface{}:
		return len(v)
	case map[string]interface{}:
		return len(v)
	default:
		return -1
	}
}

// navigateJSONPath navigates a dot-separated path in a parsed JSON structure.
// Example: "data.users.0.name" navigates into nested objects/arrays.
func navigateJSONPath(data interface{}, path string) (interface{}, error) {
	if path == "" {
		return data, nil
	}

	parts := strings.Split(path, ".")
	current := data

	for _, part := range parts {
		switch v := current.(type) {
		case map[string]interface{}:
			val, ok := v[part]
			if !ok {
				return nil, fmt.Errorf("property '%s' not found in JSON body at path '%s'", part, path)
			}
			current = val
		case []interface{}:
			// Try to parse as array index
			var idx int
			if _, err := fmt.Sscanf(part, "%d", &idx); err != nil {
				return nil, fmt.Errorf("expected numeric index but got '%s' at path '%s'", part, path)
			}
			if idx < 0 || idx >= len(v) {
				return nil, fmt.Errorf("index %d out of bounds (length %d) at path '%s'", idx, len(v), path)
			}
			current = v[idx]
		default:
			return nil, fmt.Errorf("cannot navigate into %T at path '%s'", current, path)
		}
	}

	return current, nil
}

// statusText returns a human-readable status text for common HTTP status codes.
func statusText(code int) string {
	switch code {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 204:
		return "No Content"
	case 301:
		return "Moved Permanently"
	case 302:
		return "Found"
	case 304:
		return "Not Modified"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 403:
		return "Forbidden"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 409:
		return "Conflict"
	case 422:
		return "Unprocessable Entity"
	case 429:
		return "Too Many Requests"
	case 500:
		return "Internal Server Error"
	case 502:
		return "Bad Gateway"
	case 503:
		return "Service Unavailable"
	default:
		return fmt.Sprintf("%d", code)
	}
}
