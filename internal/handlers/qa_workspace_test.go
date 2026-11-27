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

// Workspace sharing of QA items (CR_QA_RFC O5): owner alice shares into a
// team workspace; admin carol and member bob work in it; stranger dave sees
// nothing; personal items stay private.
func TestQAWorkspaceAccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.Init("sqlite", filepath.Join(t.TempDir(), "qa.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"alice", "bob", "carol", "dave"} {
		db.Create(&models.User{ID: id, Email: id + "@example.com", Name: id})
	}
	teamID := "team-1"
	if err := db.Create(&models.Team{ID: teamID, Name: "QA", CreatedBy: "alice"}).Error; err != nil {
		t.Fatal(err)
	}
	db.Create(&models.TeamMember{TeamID: teamID, UserID: "alice", Role: "admin"})
	db.Create(&models.TeamMember{TeamID: teamID, UserID: "carol", Role: "admin"})
	db.Create(&models.TeamMember{TeamID: teamID, UserID: "bob", Role: "member"})
	db.Create(&models.Workspace{ID: "ws-1", Name: "Team", OwnerID: "alice", TeamID: &teamID, Type: "team"})

	ch, ph, dh := NewTestCaseHandler(db), NewTestPlanHandler(db), NewDefectHandler(db)
	fh, mh := NewTestCaseFolderHandler(db), NewQAWorkspaceHandler(db)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", c.GetHeader("X-User")); c.Next() })
	r.GET("/test-cases", ch.List)
	r.POST("/test-cases", ch.Create)
	r.GET("/test-cases/:id", ch.Get)
	r.PUT("/test-cases/:id", ch.Update)
	r.DELETE("/test-cases/:id", ch.Delete)
	r.GET("/test-plans", ph.List)
	r.POST("/test-plans", ph.Create)
	r.POST("/defects", dh.Create)
	r.GET("/defects", dh.List)
	r.POST("/test-folders", fh.Create)
	r.POST("/qa/move", mh.Move)
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
	idOf := func(res map[string]any) string { return res["data"].(map[string]any)["id"].(string) }
	count := func(res map[string]any) int {
		if arr, ok := res["data"].([]any); ok {
			return len(arr)
		}
		return -1
	}

	// alice: one personal case, one shared case.
	_, res := call("alice", "POST", "/test-cases", map[string]any{"title": "personal"})
	personal := idOf(res)
	code, res := call("alice", "POST", "/test-cases?workspaceId=ws-1", map[string]any{"title": "shared"})
	if code != http.StatusCreated {
		t.Fatalf("create in workspace: %d %v", code, res)
	}
	shared := idOf(res)

	// Lists: personal shows only personal; the workspace list shows the shared one.
	if _, res := call("alice", "GET", "/test-cases", nil); count(res) != 1 {
		t.Fatalf("alice personal list: %v", res)
	}
	for _, u := range []string{"alice", "bob", "carol"} {
		if _, res := call(u, "GET", "/test-cases?workspaceId=ws-1", nil); count(res) != 1 {
			t.Fatalf("%s workspace list: %v", u, res)
		}
	}
	if code, _ := call("dave", "GET", "/test-cases?workspaceId=ws-1", nil); code != http.StatusNotFound {
		t.Fatalf("stranger lists the workspace: %d", code)
	}

	// Rows: bob (member) reads and edits the shared case, can't delete it;
	// carol (admin) can; nobody else sees alice's personal case.
	if code, _ := call("bob", "GET", "/test-cases/"+shared, nil); code != http.StatusOK {
		t.Fatalf("member get: %d", code)
	}
	if code, _ := call("bob", "PUT", "/test-cases/"+shared, map[string]any{"title": "edited by bob"}); code != http.StatusOK {
		t.Fatalf("member update: %d", code)
	}
	if code, _ := call("bob", "DELETE", "/test-cases/"+shared, nil); code != http.StatusForbidden {
		t.Fatalf("member delete of another's case: %d", code)
	}
	for _, u := range []string{"bob", "carol", "dave"} {
		if code, _ := call(u, "GET", "/test-cases/"+personal, nil); code != http.StatusNotFound {
			t.Fatalf("%s reads alice's personal case: %d", u, code)
		}
	}
	if code, _ := call("dave", "GET", "/test-cases/"+shared, nil); code != http.StatusNotFound {
		t.Fatalf("stranger reads shared case: %d", code)
	}
	if code, _ := call("dave", "POST", "/test-cases?workspaceId=ws-1", map[string]any{"title": "x"}); code != http.StatusNotFound {
		t.Fatalf("stranger creates in workspace: %d", code)
	}

	// Members create plans / defects there too; a member's own item is theirs to delete.
	if code, _ := call("bob", "POST", "/test-plans?workspaceId=ws-1", map[string]any{"name": "Sprint"}); code != http.StatusCreated {
		t.Fatalf("member creates plan: %d", code)
	}
	if code, _ := call("bob", "POST", "/defects?workspaceId=ws-1", map[string]any{"title": "Bug", "severity": "minor"}); code != http.StatusCreated {
		t.Fatalf("member creates defect: %d", code)
	}
	_, res = call("bob", "POST", "/test-cases?workspaceId=ws-1", map[string]any{"title": "bob's"})
	if code, _ := call("bob", "DELETE", "/test-cases/"+idOf(res), nil); code != http.StatusOK {
		t.Fatalf("member deletes own case: %d", code)
	}
	if code, _ := call("carol", "DELETE", "/test-cases/"+shared, nil); code != http.StatusOK {
		t.Fatalf("admin deletes: %d", code)
	}

	// Move: alice moves her personal folder (with its case) into the workspace.
	_, res = call("alice", "POST", "/test-folders", map[string]any{"name": "Checkout"})
	folder := idOf(res)
	code, res = call("alice", "POST", "/test-cases", map[string]any{"title": "in folder", "folderId": folder})
	if code != http.StatusCreated {
		t.Fatalf("case in folder: %d %v", code, res)
	}
	inFolder := idOf(res)
	if code, _ := call("bob", "POST", "/qa/move", map[string]any{"kind": "folder", "id": folder, "workspaceId": "ws-1"}); code != http.StatusNotFound {
		t.Fatalf("non-creator moves a personal folder: %d", code)
	}
	code, res = call("alice", "POST", "/qa/move", map[string]any{"kind": "folder", "id": folder, "workspaceId": "ws-1"})
	if code != http.StatusOK || res["data"].(map[string]any)["moved"].(float64) != 2 {
		t.Fatalf("move folder: %d %v", code, res)
	}
	if code, _ := call("bob", "GET", "/test-cases/"+inFolder, nil); code != http.StatusOK {
		t.Fatalf("moved case not visible to member: %d", code)
	}
	if code, _ := call("alice", "POST", "/test-cases", map[string]any{"title": "personal into shared folder", "folderId": folder}); code != http.StatusNotFound {
		t.Fatalf("a personal case must not be filed in a workspace folder: %d", code)
	}
	if code, _ := call("alice", "POST", "/qa/move", map[string]any{"kind": "case", "id": personal, "workspaceId": "ws-x"}); code != http.StatusNotFound {
		t.Fatalf("move into unknown workspace: %d", code)
	}
}
