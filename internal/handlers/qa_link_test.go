package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"patchman-be/internal/database"
	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
)

// Project ↔ Jira links follow the QA access rules: personal links are the
// owner's; workspace links are listed by members, edited by editors and
// deleted only by the creator or an admin.
func TestQALinks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.Init("sqlite", filepath.Join(t.TempDir(), "qa.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"alice", "bob", "dave"} {
		db.Create(&models.User{ID: id, Email: id + "@example.com", Name: id})
	}
	teamID := "team-1"
	db.Create(&models.Team{ID: teamID, Name: "QA", CreatedBy: "alice"})
	db.Create(&models.TeamMember{TeamID: teamID, UserID: "alice", Role: "admin"})
	db.Create(&models.TeamMember{TeamID: teamID, UserID: "bob", Role: "member"})
	db.Create(&models.Workspace{ID: "ws-1", Name: "Team", OwnerID: "alice", TeamID: &teamID, Type: "team"})

	h := NewQALinkHandler(db)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", c.GetHeader("X-User")); c.Next() })
	r.GET("/qa/links", h.List)
	r.POST("/qa/links", h.Create)
	r.PUT("/qa/links/:id", h.Update)
	r.DELETE("/qa/links/:id", h.Delete)
	call := func(user, method, path string, body any) (int, map[string]any) {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(b))
		req.Header.Set("X-User", user)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	count := func(res map[string]any) int {
		if arr, ok := res["data"].([]any); ok {
			return len(arr)
		}
		return -1
	}
	link := map[string]any{"gitConnectionId": "g1", "gitProject": "team/api", "jiraConnectionId": "j1", "jiraProjectKey": "mer"}

	// Validation.
	for _, bad := range []map[string]any{
		{"gitProject": "x"},
		{"gitConnectionId": "g1", "gitProject": "x", "jiraConnectionId": "j1", "jiraProjectKey": "1BAD"},
		{"gitConnectionId": "g1", "gitProject": "x", "jiraProjectKey": "MER"},
		{"gitConnectionId": "g1", "gitProject": "x", "keyPattern": "(["},
	} {
		if code, _ := call("alice", "POST", "/qa/links", bad); code != http.StatusBadRequest {
			t.Fatalf("bad %v: %d", bad, code)
		}
	}

	_, res := call("alice", "POST", "/qa/links", link)
	personal := res["data"].(map[string]any)
	if personal["jiraProjectKey"] != "MER" {
		t.Fatalf("key not uppercased: %v", personal)
	}
	code, res := call("alice", "POST", "/qa/links?workspaceId=ws-1", link)
	if code != http.StatusCreated {
		t.Fatalf("workspace create %d %v", code, res)
	}
	shared := res["data"].(map[string]any)["id"].(string)
	_, res = call("bob", "POST", "/qa/links?workspaceId=ws-1", map[string]any{"gitConnectionId": "g1", "gitProject": "team/web"})
	bobs := res["data"].(map[string]any)["id"].(string)

	if _, res := call("alice", "GET", "/qa/links", nil); count(res) != 1 {
		t.Fatalf("alice personal: %v", res)
	}
	if _, res := call("bob", "GET", "/qa/links?workspaceId=ws-1", nil); count(res) != 2 {
		t.Fatalf("bob workspace: %v", res)
	}
	if _, res := call("bob", "GET", "/qa/links", nil); count(res) != 0 {
		t.Fatalf("bob personal: %v", res)
	}
	if code, _ := call("dave", "GET", "/qa/links?workspaceId=ws-1", nil); code != http.StatusNotFound {
		t.Fatalf("dave list: %d", code)
	}
	if code, _ := call("bob", "PUT", "/qa/links/"+personal["id"].(string), map[string]any{"gitProject": "x"}); code != http.StatusNotFound {
		t.Fatalf("bob edits personal: %d", code)
	}
	if code, res := call("bob", "PUT", "/qa/links/"+shared, map[string]any{"keyPattern": `MER-\d+`}); code != http.StatusOK {
		t.Fatalf("bob edits shared: %d %v", code, res)
	}
	if code, _ := call("bob", "DELETE", "/qa/links/"+shared, nil); code != http.StatusForbidden {
		t.Fatalf("bob deletes alice's: %d", code)
	}
	if code, _ := call("bob", "DELETE", "/qa/links/"+bobs, nil); code != http.StatusOK {
		t.Fatalf("bob deletes own: %d", code)
	}
	if code, _ := call("alice", "DELETE", "/qa/links/"+shared, nil); code != http.StatusOK {
		t.Fatalf("alice deletes: %d", code)
	}
}
