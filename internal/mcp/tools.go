package mcp

import (
	"context"
	"encoding/json"
	"net/url"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---- Collections -----------------------------------------------------------

type listCollectionsIn struct {
	WorkspaceID string `json:"workspaceId,omitempty" jsonschema:"optional workspace id to filter by"`
}
type createCollectionIn struct {
	Name        string `json:"name" jsonschema:"collection name"`
	Description string `json:"description,omitempty" jsonschema:"optional description"`
	ParentID    string `json:"parentId,omitempty" jsonschema:"parent collection/folder id; omit for a top-level collection"`
	Type        string `json:"type,omitempty" jsonschema:"'collection' (default) or 'folder'"`
	WorkspaceID string `json:"workspaceId,omitempty" jsonschema:"workspace id this collection belongs to"`
	BaseURL     string `json:"baseUrl,omitempty" jsonschema:"base URL prepended to relative request paths"`
}
type updateCollectionIn struct {
	ID          string `json:"id" jsonschema:"collection id to update"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	BaseURL     string `json:"baseUrl,omitempty"`
	ParentID    string `json:"parentId,omitempty" jsonschema:"move under this parent; empty string moves to root"`
	WorkspaceID string `json:"workspaceId,omitempty"`
}
type idIn struct {
	ID string `json:"id" jsonschema:"resource id"`
}

func registerCollectionTools(s *mcpsdk.Server, c *caller) {
	reg(s, "list_collections", "List the caller's collections (folders + collections tree, with their requests), in the order the app shows them: siblings by sortOrder, then creation time. Change the order with reorder_collections / reorder_requests.",
		func(in listCollectionsIn) (int, string) {
			p := "/api/collections"
			if in.WorkspaceID != "" {
				p += "?workspaceId=" + url.QueryEscape(in.WorkspaceID)
			}
			return c.call("GET", p, nil)
		})
	reg(s, "create_collection", "Create a new collection or folder.",
		func(in createCollectionIn) (int, string) { return c.call("POST", "/api/collections", in) })
	reg(s, "update_collection", "Update an existing collection's name/description/baseUrl/parent.",
		func(in updateCollectionIn) (int, string) {
			return c.call("PUT", "/api/collections/"+url.PathEscape(in.ID), in)
		})
	reg(s, "delete_collection", "Delete a collection (and its requests) by id.",
		func(in idIn) (int, string) {
			return c.call("DELETE", "/api/collections/"+url.PathEscape(in.ID), nil)
		})
	reg(s, "list_collection_requests", "List the saved requests inside a collection, in the order the app shows them (by sortOrder, then creation time).",
		func(in idIn) (int, string) {
			return c.call("GET", "/api/collections/"+url.PathEscape(in.ID)+"/requests", nil)
		})
	registerReorderTools(s, c)
}

// ---- Requests --------------------------------------------------------------

type createRequestIn struct {
	CollectionID     string `json:"collectionId" jsonschema:"id of the collection this request belongs to"`
	Name             string `json:"name" jsonschema:"request name"`
	Method           string `json:"method,omitempty" jsonschema:"HTTP method, e.g. GET/POST (default GET)"`
	URL              string `json:"url,omitempty" jsonschema:"request URL"`
	Headers          string `json:"headers,omitempty" jsonschema:"headers as a JSON string"`
	Params           string `json:"params,omitempty" jsonschema:"query params as a JSON string"`
	Body             string `json:"body,omitempty" jsonschema:"body as a JSON string {type, content}"`
	Auth             string `json:"auth,omitempty" jsonschema:"auth as a JSON string {type, ...}"`
	PreRequestScript string `json:"preRequestScript,omitempty"`
	TestScript       string `json:"testScript,omitempty"`
}
type updateRequestIn struct {
	ID               string `json:"id" jsonschema:"request id to update"`
	CollectionID     string `json:"collectionId,omitempty" jsonschema:"move request to this collection"`
	Name             string `json:"name,omitempty"`
	Method           string `json:"method,omitempty"`
	URL              string `json:"url,omitempty"`
	Headers          string `json:"headers,omitempty"`
	Params           string `json:"params,omitempty"`
	Body             string `json:"body,omitempty"`
	Auth             string `json:"auth,omitempty"`
	PreRequestScript string `json:"preRequestScript,omitempty"`
	TestScript       string `json:"testScript,omitempty"`
}

func registerRequestTools(s *mcpsdk.Server, c *caller) {
	reg(s, "get_request", "Get a single saved request by id.",
		func(in idIn) (int, string) { return c.call("GET", "/api/requests/"+url.PathEscape(in.ID), nil) })
	reg(s, "create_request", "Create a new saved HTTP request inside a collection.",
		func(in createRequestIn) (int, string) { return c.call("POST", "/api/requests", in) })
	reg(s, "update_request", "Update fields of an existing saved request.",
		func(in updateRequestIn) (int, string) {
			return c.call("PUT", "/api/requests/"+url.PathEscape(in.ID), in)
		})
	reg(s, "delete_request", "Delete a saved request by id.",
		func(in idIn) (int, string) { return c.call("DELETE", "/api/requests/"+url.PathEscape(in.ID), nil) })
}

// ---- Environments ----------------------------------------------------------

type createEnvironmentIn struct {
	Name        string `json:"name" jsonschema:"environment name"`
	Variables   string `json:"variables,omitempty" jsonschema:"variables as a JSON string array [{key,value,enabled,secret}]"`
	WorkspaceID string `json:"workspaceId,omitempty"`
}
type updateEnvironmentIn struct {
	ID          string `json:"id" jsonschema:"environment id to update"`
	Name        string `json:"name,omitempty"`
	Variables   string `json:"variables,omitempty" jsonschema:"variables as a JSON string"`
	WorkspaceID string `json:"workspaceId,omitempty"`
}

func registerEnvironmentTools(s *mcpsdk.Server, c *caller) {
	reg(s, "list_environments", "List the caller's environments.",
		func(_ struct{}) (int, string) { return c.call("GET", "/api/environments", nil) })
	reg(s, "create_environment", "Create a new environment.",
		func(in createEnvironmentIn) (int, string) { return c.call("POST", "/api/environments", in) })
	reg(s, "update_environment", "Update an environment's name/variables.",
		func(in updateEnvironmentIn) (int, string) {
			return c.call("PUT", "/api/environments/"+url.PathEscape(in.ID), in)
		})
	reg(s, "delete_environment", "Delete an environment by id.",
		func(in idIn) (int, string) {
			return c.call("DELETE", "/api/environments/"+url.PathEscape(in.ID), nil)
		})
}

// ---- Test cases ------------------------------------------------------------

type createTestCaseIn struct {
	Title       string `json:"title" jsonschema:"test case title"`
	Description string `json:"description,omitempty"`
	Priority    string `json:"priority,omitempty" jsonschema:"P0..P4 (default P2)"`
	Type        string `json:"type,omitempty" jsonschema:"manual or automated"`
	Tags        string `json:"tags,omitempty" jsonschema:"tags as a JSON array string"`
	Steps       string `json:"steps,omitempty" jsonschema:"steps as a JSON array string [{stepNo,action,expected}]"`
	Status      string `json:"status,omitempty" jsonschema:"draft/active/deprecated"`
	FolderID    string `json:"folderId,omitempty" jsonschema:"test folder id to file the case in (see list_test_folders); omit for the root"`
}
type updateTestCaseIn struct {
	ID          string `json:"id" jsonschema:"test case id to update"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Priority    string `json:"priority,omitempty"`
	Type        string `json:"type,omitempty"`
	Tags        string `json:"tags,omitempty"`
	Steps       string `json:"steps,omitempty"`
	Status      string `json:"status,omitempty"`
}

// Test-case folders. A folder nests under parentId (null = top level); a case
// sits in folderId (null = root). Folders are owner-only, like test cases.
type createTestFolderIn struct {
	Name     string `json:"name" jsonschema:"folder name"`
	ParentID string `json:"parentId,omitempty" jsonschema:"parent folder id for a subfolder; omit for a top-level folder"`
}
type renameTestFolderIn struct {
	ID   string `json:"id" jsonschema:"folder id to rename"`
	Name string `json:"name" jsonschema:"new folder name"`
}
type deleteTestFolderIn struct {
	ID                   string `json:"id" jsonschema:"folder id to delete"`
	MoveContentsToParent bool   `json:"moveContentsToParent,omitempty" jsonschema:"required to delete a non-empty folder: moves its test cases and subfolders to the deleted folder's parent (the root for a top-level folder). Test cases are never deleted."`
}
type moveTestCaseIn struct {
	ID       string `json:"id" jsonschema:"test case id to move"`
	FolderID string `json:"folderId,omitempty" jsonschema:"destination folder id; empty or omitted moves the case to the root"`
}
type runTestFolderIn struct {
	ID            string `json:"id" jsonschema:"folder id whose test cases to run"`
	EnvironmentID string `json:"environmentId,omitempty" jsonschema:"environment whose variables fill {{var}} placeholders (see list_environments); omit to run without one"`
	Recursive     *bool  `json:"recursive,omitempty" jsonschema:"include the cases of all subfolders (default true)"`
}

func registerTestCaseTools(s *mcpsdk.Server, c *caller) {
	reg(s, "list_test_cases", "List the caller's test cases. Each carries folderId (null = root; see list_test_folders).",
		func(_ struct{}) (int, string) { return c.call("GET", "/api/test-cases", nil) })
	reg(s, "get_test_case", "Get a single test case by id.",
		func(in idIn) (int, string) { return c.call("GET", "/api/test-cases/"+url.PathEscape(in.ID), nil) })
	reg(s, "create_test_case", "Create a new test case.",
		func(in createTestCaseIn) (int, string) { return c.call("POST", "/api/test-cases", in) })
	reg(s, "update_test_case", "Update an existing test case.",
		func(in updateTestCaseIn) (int, string) {
			return c.call("PUT", "/api/test-cases/"+url.PathEscape(in.ID), in)
		})
	reg(s, "delete_test_case", "Delete a test case by id.",
		func(in idIn) (int, string) {
			return c.call("DELETE", "/api/test-cases/"+url.PathEscape(in.ID), nil)
		})

	reg(s, "list_test_folders", "List the caller's test-case folders (flat; build the tree from parentId, null = top level). Test cases reference them by folderId.",
		func(_ struct{}) (int, string) { return c.call("GET", "/api/test-folders", nil) })
	reg(s, "create_test_folder", "Create a test-case folder, optionally inside another folder (parentId).",
		func(in createTestFolderIn) (int, string) {
			body := map[string]any{"name": in.Name}
			if in.ParentID != "" {
				body["parentId"] = in.ParentID
			}
			return c.call("POST", "/api/test-folders", body)
		})
	reg(s, "rename_test_folder", "Rename a test-case folder.",
		func(in renameTestFolderIn) (int, string) {
			return c.call("PUT", "/api/test-folders/"+url.PathEscape(in.ID), map[string]any{"name": in.Name})
		})
	reg(s, "delete_test_folder", "Delete a test-case folder. Refused (409, with counts) while it still holds test cases or subfolders unless moveContentsToParent=true, which moves them to the folder's parent first.",
		func(in deleteTestFolderIn) (int, string) {
			p := "/api/test-folders/" + url.PathEscape(in.ID)
			if in.MoveContentsToParent {
				p += "?moveToParent=true"
			}
			return c.call("DELETE", p, nil)
		})
	reg(s, "move_test_case", "Move a test case into a folder, or to the root when folderId is empty.",
		func(in moveTestCaseIn) (int, string) {
			var folder any // JSON null → root
			if in.FolderID != "" {
				folder = in.FolderID
			}
			return c.call("PUT", "/api/test-cases/"+url.PathEscape(in.ID)+"/move", map[string]any{"folderId": folder})
		})
	regCtx(s, "run_test_folder", "Run every executable test case in a folder (and its subfolders unless recursive=false), sequentially, with the given environment's variables. Cases with no executable steps are skipped and listed. Records a test run (runId) and returns its summary plus each case's status and the failing steps.",
		func(ctx context.Context, in runTestFolderIn) (int, string) {
			body := map[string]any{}
			if in.EnvironmentID != "" {
				body["environmentId"] = in.EnvironmentID
			}
			if in.Recursive != nil {
				body["recursive"] = *in.Recursive
			}
			code, raw := c.callCtx(ctx, "POST", "/api/test-folders/"+url.PathEscape(in.ID)+"/execute", body)
			if code >= 400 {
				return code, raw
			}
			return code, compactFolderRun(raw)
		})
}

// compactFolderRun trims a folder-run response for a model: the run id, the
// summary and, per case, its status plus only the steps that did not pass
// (with their error / failed assertions) — not every response body, which can
// be megabytes for a large folder. Falls back to the raw body if it doesn't
// parse.
func compactFolderRun(raw string) string {
	var resp struct {
		Data struct {
			TestRun struct {
				ID string `json:"id"`
			} `json:"testRun"`
			Folder        json.RawMessage `json:"folder"`
			OverallStatus string          `json:"overallStatus"`
			TotalDuration int64           `json:"totalDuration"`
			Summary       json.RawMessage `json:"summary"`
			Skipped       json.RawMessage `json:"skipped"`
			TestCases     []struct {
				TestCaseID string `json:"testCaseId"`
				Title      string `json:"title"`
				FolderID   string `json:"folderId"`
				Status     string `json:"status"`
				Duration   int64  `json:"duration"`
				Result     struct {
					Summary json.RawMessage `json:"summary"`
					Steps   []struct {
						StepNo  int    `json:"stepNo"`
						Type    string `json:"type"`
						Status  string `json:"status"`
						Error   string `json:"error,omitempty"`
						Request *struct {
							Method string `json:"method"`
							URL    string `json:"url"`
						} `json:"request,omitempty"`
						Response *struct {
							StatusCode int `json:"statusCode"`
						} `json:"response,omitempty"`
						Assertions []struct {
							Type     string `json:"type"`
							Expected string `json:"expected"`
							Actual   string `json:"actual"`
							Passed   bool   `json:"passed"`
						} `json:"assertions,omitempty"`
					} `json:"steps"`
				} `json:"result"`
			} `json:"testCases"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil || resp.Data.TestRun.ID == "" {
		return raw
	}
	type failedStep struct {
		StepNo     int    `json:"stepNo"`
		Type       string `json:"type"`
		Status     string `json:"status"`
		Request    string `json:"request,omitempty"`
		StatusCode int    `json:"statusCode,omitempty"`
		Error      string `json:"error,omitempty"`
		Failed     []any  `json:"failedAssertions,omitempty"`
	}
	type caseOut struct {
		TestCaseID  string          `json:"testCaseId"`
		Title       string          `json:"title"`
		FolderID    string          `json:"folderId,omitempty"`
		Status      string          `json:"status"`
		Duration    int64           `json:"durationMs"`
		Steps       json.RawMessage `json:"steps,omitempty"`
		FailedSteps []failedStep    `json:"failedSteps,omitempty"`
	}
	d := resp.Data
	cases := make([]caseOut, 0, len(d.TestCases))
	for _, tc := range d.TestCases {
		co := caseOut{TestCaseID: tc.TestCaseID, Title: tc.Title, FolderID: tc.FolderID, Status: tc.Status, Duration: tc.Duration, Steps: tc.Result.Summary}
		for _, st := range tc.Result.Steps {
			if st.Status == "passed" {
				continue
			}
			fs := failedStep{StepNo: st.StepNo, Type: st.Type, Status: st.Status, Error: st.Error}
			if st.Request != nil {
				fs.Request = st.Request.Method + " " + st.Request.URL
			}
			if st.Response != nil {
				fs.StatusCode = st.Response.StatusCode
			}
			for _, a := range st.Assertions {
				if !a.Passed {
					fs.Failed = append(fs.Failed, map[string]string{"type": a.Type, "expected": a.Expected, "actual": a.Actual})
				}
			}
			co.FailedSteps = append(co.FailedSteps, fs)
		}
		cases = append(cases, co)
	}
	out, err := json.Marshal(map[string]any{
		"runId":         d.TestRun.ID,
		"folder":        d.Folder,
		"overallStatus": d.OverallStatus,
		"totalDuration": d.TotalDuration,
		"summary":       d.Summary,
		"skipped":       d.Skipped,
		"testCases":     cases,
	})
	if err != nil {
		return raw
	}
	return string(out)
}

// ---- Mock servers & routes -------------------------------------------------

type createMockServerIn struct {
	Name   string `json:"name" jsonschema:"mock server name"`
	Prefix string `json:"prefix" jsonschema:"path prefix this mock server answers under"`
}
type updateMockServerIn struct {
	ID       string `json:"id" jsonschema:"mock server id"`
	Name     string `json:"name,omitempty"`
	Prefix   string `json:"prefix,omitempty"`
	IsActive *bool  `json:"isActive,omitempty"`
}
type createMockRouteIn struct {
	ServerID        string `json:"serverId" jsonschema:"mock server id this route belongs to"`
	Method          string `json:"method" jsonschema:"HTTP method to match"`
	Path            string `json:"path" jsonschema:"path to match (relative to the server prefix)"`
	StatusCode      int    `json:"statusCode,omitempty" jsonschema:"response status code (default 200)"`
	ResponseHeaders string `json:"responseHeaders,omitempty" jsonschema:"response headers as a JSON string"`
	ResponseBody    string `json:"responseBody,omitempty"`
	Delay           int    `json:"delay,omitempty" jsonschema:"artificial delay in ms"`
}
type updateMockRouteIn struct {
	ID              string `json:"id" jsonschema:"mock route id"`
	Method          string `json:"method,omitempty"`
	Path            string `json:"path,omitempty"`
	StatusCode      *int   `json:"statusCode,omitempty"`
	ResponseHeaders string `json:"responseHeaders,omitempty"`
	ResponseBody    string `json:"responseBody,omitempty"`
	Delay           *int   `json:"delay,omitempty"`
	IsActive        *bool  `json:"isActive,omitempty"`
}

func registerMockTools(s *mcpsdk.Server, c *caller) {
	reg(s, "list_mock_servers", "List the caller's mock servers.",
		func(_ struct{}) (int, string) { return c.call("GET", "/api/mock-servers", nil) })
	reg(s, "create_mock_server", "Create a new mock server.",
		func(in createMockServerIn) (int, string) { return c.call("POST", "/api/mock-servers", in) })
	reg(s, "update_mock_server", "Update a mock server (name/prefix/active).",
		func(in updateMockServerIn) (int, string) {
			return c.call("PUT", "/api/mock-servers/"+url.PathEscape(in.ID), in)
		})
	reg(s, "delete_mock_server", "Delete a mock server by id.",
		func(in idIn) (int, string) {
			return c.call("DELETE", "/api/mock-servers/"+url.PathEscape(in.ID), nil)
		})
	reg(s, "list_mock_routes", "List the routes of a mock server.",
		func(in idIn) (int, string) {
			return c.call("GET", "/api/mock-servers/"+url.PathEscape(in.ID)+"/routes", nil)
		})
	reg(s, "create_mock_route", "Create a route on a mock server.",
		func(in createMockRouteIn) (int, string) {
			return c.call("POST", "/api/mock-servers/"+url.PathEscape(in.ServerID)+"/routes", in)
		})
	reg(s, "update_mock_route", "Update a mock route.",
		func(in updateMockRouteIn) (int, string) {
			return c.call("PUT", "/api/mock-routes/"+url.PathEscape(in.ID), in)
		})
	reg(s, "delete_mock_route", "Delete a mock route by id.",
		func(in idIn) (int, string) {
			return c.call("DELETE", "/api/mock-routes/"+url.PathEscape(in.ID), nil)
		})
}
