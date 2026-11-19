package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"patchman-be/internal/models"
)

func TestNormalizeTestCaseFields(t *testing.T) {
	for in, want := range map[string]string{"": "functional", "manual": "functional", "Automated": "functional", "security": "security", " Integration ": "integration"} {
		if got := normalizeTestCaseType(in); got != want {
			t.Errorf("type %q = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{`["web", "checkout"]`: "web,checkout", "web, checkout ,": "web,checkout", "": "", `[1,"a"]`: "a", `[broken`: "[broken"} {
		if got := normalizeTestCaseTags(in); got != want {
			t.Errorf("tags %q = %q, want %q", in, got, want)
		}
	}

	steps := `[{"action":"open the page","expected":"form shown"},` +
		`{"request":{"method":"GET","url":"/x"}},` +
		`{"stepNo":7,"assertions":[{"type":"status","operator":"equals","value":"200"}]},` +
		`{"extractions":[{"variable":"tok","source":"body"}]},{"waitMs":500},{"type":"wait","waitMs":1}]`
	var got []map[string]any
	if err := json.Unmarshal([]byte(normalizeTestCaseSteps(steps)), &got); err != nil {
		t.Fatal(err)
	}
	wantTypes := []any{nil, "request", "assertion", "extract", "wait", "wait"}
	wantNo := []float64{1, 2, 7, 4, 5, 6}
	for i, st := range got {
		if st["type"] != wantTypes[i] || st["stepNo"] != wantNo[i] {
			t.Errorf("step %d = type %v no %v, want %v %v", i, st["type"], st["stepNo"], wantTypes[i], wantNo[i])
		}
	}
	typed := `[{"stepNo":1,"type":"wait","waitMs":1}]`
	if normalizeTestCaseSteps(typed) != typed || normalizeTestCaseSteps("not json") != "not json" {
		t.Error("well-formed or non-array steps must be left byte-identical")
	}
}

// The MCP create_test_case → REST path: an agent's JSON-array tags, legacy
// type and untyped steps are stored in the shape the editor reads.
func TestCreateTestCaseNormalizes(t *testing.T) {
	db := adminUsageTestDB(t)
	if err := db.AutoMigrate(&models.TestCaseFolder{}, &models.TestCaseVersion{}); err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	h := NewTestCaseHandler(db)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", "u1"); c.Next() })
	r.POST("/tc", h.Create)
	r.PUT("/tc/:id", h.Update)
	body, _ := json.Marshal(map[string]string{"title": "Login", "type": "automated", "tags": `["web","checkout"]`,
		"steps": `[{"request":{"method":"GET","url":"/login"}}]`})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/tc", bytes.NewReader(body)))
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	var out struct{ Data models.TestCase }
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	tc := out.Data
	if tc.Type != "functional" || tc.Tags != "web,checkout" || !bytes.Contains([]byte(tc.Steps), []byte(`"type":"request"`)) {
		t.Fatalf("stored %+v", tc)
	}
	body, _ = json.Marshal(map[string]string{"tags": `["api"]`, "type": "manual"})
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PUT", "/tc/"+tc.ID, bytes.NewReader(body)))
	var got models.TestCase
	db.First(&got, "id = ?", tc.ID)
	if w.Code != http.StatusOK || got.Tags != "api" || got.Type != "functional" {
		t.Fatalf("update = %d, stored tags %q type %q", w.Code, got.Tags, got.Type)
	}
}

// ReassignCollection used to update saved_requests.user_id, a column that
// doesn't exist, so it failed with 500 on every real schema.
func TestReassignCollectionMovesTree(t *testing.T) {
	db := adminUsageTestDB(t)
	if err := db.AutoMigrate(&models.CollectionPublicGrant{}); err != nil {
		t.Fatal(err)
	}
	for _, u := range []models.User{{ID: "old", Email: "old@x.test"}, {ID: "new", Email: "new@x.test"}} {
		if err := db.Create(&u).Error; err != nil {
			t.Fatal(err)
		}
	}
	child := "root"
	db.Create(&models.Collection{ID: "root", UserID: "old", Name: "Root"})
	db.Create(&models.Collection{ID: "child", UserID: "old", Name: "Child", ParentID: &child})
	db.Create(&models.SavedRequest{ID: "r1", CollectionID: "child", Name: "Get", Method: "GET", URL: "/x"})
	old := "old"
	db.Create(&models.CollectionPublicGrant{ID: "g1", CollectionID: "root", Role: "editor", SubjectKind: "user", SubjectID: &old})

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.PUT("/collections/:id/owner", NewAdminHandler(db, nil).ReassignCollection)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PUT", "/collections/root/owner", bytes.NewReader([]byte(`{"newOwnerId":"new"}`))))
	if w.Code != http.StatusOK {
		t.Fatalf("reassign = %d %s", w.Code, w.Body)
	}
	var n int64
	db.Model(&models.Collection{}).Where("user_id = ?", "new").Count(&n)
	var g models.CollectionPublicGrant
	db.First(&g, "id = ?", "g1")
	if n != 2 || g.SubjectID == nil || *g.SubjectID != "new" {
		t.Fatalf("collections owned by new = %d, grant subject %v", n, g.SubjectID)
	}
}

// A manual step (no type) is skipped and the run goes on; before, it errored
// with "Unknown step type" and aborted every step after it.
func TestExecuteSkipsManualSteps(t *testing.T) {
	res := executeTestCaseSteps(`[{"stepNo":1,"action":"look","expected":"ok"},{"stepNo":2,"type":"wait","waitMs":1},{"stepNo":3,"type":"bogus"}]`, nil)
	if len(res.Steps) != 3 || res.Steps[0].Status != "skipped" || res.Steps[0].Type != "manual" || res.Steps[1].Status != "passed" || res.Steps[2].Status != "error" {
		t.Fatalf("steps = %+v", res.Steps)
	}
	if res.Summary.Skipped != 1 || res.Summary.Passed != 1 || res.Summary.Failed != 1 {
		t.Fatalf("summary = %+v", res.Summary)
	}
}
