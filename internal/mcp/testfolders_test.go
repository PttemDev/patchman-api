package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"patchman-be/internal/mcpscope"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestTestFolderToolsRouting pins each test-folder tool to its REST route,
// method and body (asserted against the echo engine).
func TestTestFolderToolsRouting(t *testing.T) {
	cs := connect(t, echoEngine())

	cases := []struct {
		tool       string
		args       map[string]any
		method     string
		path       string
		query      string
		bodyHas    []string
		bodyHasNot []string
	}{
		{tool: "list_test_folders", args: map[string]any{}, method: "GET", path: "/api/test-folders"},
		{tool: "create_test_folder", args: map[string]any{"name": "Checkout"}, method: "POST", path: "/api/test-folders",
			bodyHas: []string{`"name":"Checkout"`}, bodyHasNot: []string{"parentId"}},
		{tool: "create_test_folder", args: map[string]any{"name": "Pay", "parentId": "f1"}, method: "POST", path: "/api/test-folders",
			bodyHas: []string{`"parentId":"f1"`}},
		{tool: "rename_test_folder", args: map[string]any{"id": "f1", "name": "New"}, method: "PUT", path: "/api/test-folders/f1",
			bodyHas: []string{`"name":"New"`}},
		{tool: "delete_test_folder", args: map[string]any{"id": "f1"}, method: "DELETE", path: "/api/test-folders/f1", query: ""},
		{tool: "delete_test_folder", args: map[string]any{"id": "f1", "moveContentsToParent": true}, method: "DELETE", path: "/api/test-folders/f1", query: "moveToParent=true"},
		{tool: "move_test_case", args: map[string]any{"id": "c1", "folderId": "f2"}, method: "PUT", path: "/api/test-cases/c1/move",
			bodyHas: []string{`"folderId":"f2"`}},
		{tool: "move_test_case", args: map[string]any{"id": "c1"}, method: "PUT", path: "/api/test-cases/c1/move",
			bodyHas: []string{`"folderId":null`}},
		{tool: "run_test_folder", args: map[string]any{"id": "f1", "environmentId": "e1", "recursive": false}, method: "POST", path: "/api/test-folders/f1/execute",
			bodyHas: []string{`"environmentId":"e1"`, `"recursive":false`}},
		{tool: "run_test_folder", args: map[string]any{"id": "f1"}, method: "POST", path: "/api/test-folders/f1/execute",
			bodyHasNot: []string{"environmentId", "recursive"}},
		{tool: "create_test_case", args: map[string]any{"title": "T", "folderId": "f9"}, method: "POST", path: "/api/test-cases",
			bodyHas: []string{`"folderId":"f9"`}},
	}
	for _, tc := range cases {
		res := call(t, cs, tc.tool, tc.args)
		if res.IsError {
			t.Fatalf("%s: unexpected tool error: %s", tc.tool, text(t, res))
		}
		e := decode(t, res)
		if e.Method != tc.method || e.Path != tc.path {
			t.Errorf("%s: got %s %s, want %s %s", tc.tool, e.Method, e.Path, tc.method, tc.path)
		}
		if e.Query != tc.query {
			t.Errorf("%s: query = %q, want %q", tc.tool, e.Query, tc.query)
		}
		for _, want := range tc.bodyHas {
			if !strings.Contains(e.Body, want) {
				t.Errorf("%s: body %q missing %q", tc.tool, e.Body, want)
			}
		}
		for _, bad := range tc.bodyHasNot {
			if strings.Contains(e.Body, bad) {
				t.Errorf("%s: body %q must not contain %q", tc.tool, e.Body, bad)
			}
		}
	}
}

// TestTestFolderToolsScoped: the folder tools live in the testcases group.
func TestTestFolderToolsScoped(t *testing.T) {
	only := mcpscope.GroupTestCases
	cs := connectScoped(t, echoEngine(), mcpscope.Parse(&only))
	got := toolNames(t, cs)
	for _, want := range []string{"list_test_folders", "create_test_folder", "rename_test_folder", "delete_test_folder", "move_test_case", "run_test_folder"} {
		if !got[want] {
			t.Errorf("%s missing under scope %q", want, only)
		}
	}
	other := mcpscope.GroupDB
	if toolNames(t, connectScoped(t, echoEngine(), mcpscope.Parse(&other)))["run_test_folder"] {
		t.Error("run_test_folder must not be exposed without the testcases scope")
	}
}

func toolNames(t *testing.T, cs *mcpsdk.ClientSession) map[string]bool {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	got := map[string]bool{}
	for _, tl := range res.Tools {
		got[tl.Name] = true
	}
	return got
}

// TestCompactFolderRun keeps the run id + summary and only the failing steps.
func TestCompactFolderRun(t *testing.T) {
	raw := `{"data":{"testRun":{"id":"run-1"},"folder":{"id":"f1","name":"Checkout"},"overallStatus":"failed","totalDuration":42,
	"summary":{"total":2,"passed":1,"failed":1,"skipped":1},"skipped":[{"testCaseId":"c3","title":"Manual","folderId":"f1"}],
	"testCases":[
	 {"testCaseId":"c1","title":"ok","folderId":"f1","status":"passed","duration":10,"result":{"summary":{"total":1,"passed":1,"failed":0,"skipped":0},
	   "steps":[{"stepNo":1,"type":"request","status":"passed","request":{"method":"GET","url":"http://x"},"response":{"statusCode":200,"body":"BIGBODY"}}]}},
	 {"testCaseId":"c2","title":"bad","folderId":"f1","status":"failed","duration":32,"result":{"summary":{"total":2,"passed":1,"failed":1,"skipped":0},
	   "steps":[{"stepNo":1,"type":"request","status":"passed","response":{"statusCode":200,"body":"BIGBODY"}},
	            {"stepNo":2,"type":"assertion","status":"failed","assertions":[{"type":"status","expected":"201","actual":"200","passed":false},{"type":"x","expected":"a","actual":"a","passed":true}]}]}}]}}`
	out := compactFolderRun(raw)
	if strings.Contains(out, "BIGBODY") {
		t.Errorf("response bodies must be dropped: %s", out)
	}
	var got struct {
		RunID     string `json:"runId"`
		Summary   struct{ Total, Passed, Failed, Skipped int }
		Skipped   []map[string]string
		TestCases []struct {
			TestCaseID  string `json:"testCaseId"`
			Status      string
			FailedSteps []struct {
				StepNo           int `json:"stepNo"`
				FailedAssertions []map[string]string
			} `json:"failedSteps"`
		} `json:"testCases"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("compact output not JSON: %v (%s)", err, out)
	}
	if got.RunID != "run-1" || got.Summary.Total != 2 || got.Summary.Skipped != 1 || len(got.Skipped) != 1 {
		t.Errorf("run id / summary / skipped lost: %+v", got)
	}
	if len(got.TestCases) != 2 || len(got.TestCases[0].FailedSteps) != 0 {
		t.Fatalf("passing case should list no failed steps: %+v", got.TestCases)
	}
	fs := got.TestCases[1].FailedSteps
	if len(fs) != 1 || fs[0].StepNo != 2 || len(fs[0].FailedAssertions) != 1 || fs[0].FailedAssertions[0]["expected"] != "201" {
		t.Errorf("failed step detail wrong: %+v", fs)
	}

	// Unparseable / error-shaped bodies pass through untouched.
	if compactFolderRun(`{"error":"boom"}`) != `{"error":"boom"}` {
		t.Error("non-run body must pass through")
	}
}
