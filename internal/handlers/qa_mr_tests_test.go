package handlers

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"patchman-be/internal/config"
	"patchman-be/internal/database"
	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
)

func TestQAMRTestsAndJiraWrites(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetQACaches()
	gl, posts := fakeReviewGitLab(t)
	defer gl.Close()
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) }))
	defer app.Close()
	var jmu sync.Mutex
	var jiraCalls []string
	jr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		jmu.Lock()
		jiraCalls = append(jiraCalls, r.Method+" "+r.URL.Path+" "+string(b))
		jmu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"9","key":"MER-500"}`))
	}))
	defer jr.Close()

	db, err := database.Init("sqlite", filepath.Join(t.TempDir(), "mrt.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"alice", "bob"} {
		db.Create(&models.User{ID: id, Email: id + "@example.com", Name: id})
	}
	teamID := "team-1"
	db.Create(&models.Team{ID: teamID, Name: "QA", CreatedBy: "alice"})
	db.Create(&models.TeamMember{TeamID: teamID, UserID: "alice", Role: "admin"})
	db.Create(&models.TeamMember{TeamID: teamID, UserID: "bob", Role: "member"})
	db.Create(&models.Workspace{ID: "ws-1", Name: "Team", OwnerID: "alice", TeamID: &teamID, Type: "team"})
	cipher, _ := services.NewTokenCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32)))
	enc, _ := cipher.Encrypt("tok")
	db.Create(&models.GitConnection{ID: "gl1", UserID: "alice", Name: "gl", Provider: "gitlab", BaseURL: gl.URL, AuthKind: "pat", CredentialEnc: enc})
	db.Create(&models.JiraConnection{ID: "j1", UserID: "alice", Name: "jira", BaseURL: jr.URL, Flavor: "datacenter", AuthKind: "pat", CredentialEnc: enc})
	ws := "ws-1"
	db.Create(&models.QAProjectLink{ID: "l1", UserID: "alice", WorkspaceID: &ws, GitConnectionID: "gl1", GitProject: "7", JiraConnectionID: "j1", JiraProjectKey: "MER"})
	steps := `[{"stepNo":1,"type":"request","request":{"method":"GET","url":"` + app.URL + `/x"}},{"stepNo":2,"type":"assertion","assertions":[{"type":"status","operator":"equals","value":"200"}]}]`
	db.Create(&models.TestCase{ID: "tc1", UserID: "alice", WorkspaceID: &ws, Title: "login works", Steps: steps, Status: "active", MRRef: "l1:1"})
	db.Create(&models.TestCase{ID: "tc2", UserID: "alice", WorkspaceID: &ws, Title: "manual check", Steps: `[{"stepNo":1,"action":"look","expected":"ok"}]`, Status: "draft", JiraKey: "MER-1"})
	db.Create(&models.TestCase{ID: "tc3", UserID: "dave", Title: "someone else's", Steps: steps, MRRef: "l1:1"})
	// An agent's draft (executable, but not reviewed yet) must not run.
	db.Create(&models.TestCase{ID: "tc4", UserID: "alice", WorkspaceID: &ws, Title: "agent draft", Steps: steps, Status: "draft", MRRef: "l1:1"})
	db.Create(&models.Defect{ID: "d1", UserID: "alice", WorkspaceID: &ws, Title: "Login 500", Description: "boom", StepsToReproduce: "1. login", Severity: "major", Priority: "P1"})

	h := NewQARadarHandler(db, NewGitRemoteHandler(db, &config.Config{}, NewGitConnectionHandler(db, cipher), nil), NewJiraConnectionHandler(db, cipher))
	ex := NewTestExecutorHandler(db)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", c.GetHeader("X-User")); c.Next() })
	r.GET("/qa/mr-tests", h.MRTests)
	r.POST("/qa/mr-tests/run", h.RunMRTests)
	r.POST("/qa/mr-tests/post", h.PostMRTestResults)
	r.POST("/qa/defects/:id/jira", h.DefectToJira)
	r.POST("/test-cases/:id/execute", ex.ExecuteTestCase)
	do := func(user, method, path, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-User", user)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		d, _ := out["data"].(map[string]any)
		if d == nil {
			d = out
		}
		return w.Code, d
	}

	// Linked cases: tc1 by MR ref, tc2 by Jira key; tc3 is outside the space.
	_, d := do("alice", "GET", "/qa/mr-tests?linkId=l1&iid=1", "")
	if n := len(d["cases"].([]any)); n != 3 || d["jiraKey"] != "MER-1" {
		t.Fatalf("cases: %v", d)
	}
	// A member runs them: one passes, the manual one is skipped.
	code, d := do("bob", "POST", "/qa/mr-tests/run", `{"linkId":"l1","iid":1}`)
	if code != 403 { // bob has no grant on alice's Git connection
		t.Fatalf("bob without connection access: %d", code)
	}
	code, d = do("alice", "POST", "/qa/mr-tests/run", `{"linkId":"l1","iid":1}`)
	sum, _ := d["summary"].(map[string]any)
	if code != 200 || sum["passed"].(float64) != 1 || sum["drafts"].(float64) != 2 || sum["total"].(float64) != 1 {
		t.Fatalf("run %d %v", code, d)
	}
	run := d["testRun"].(map[string]any)
	if run["mrRef"] != "l1:1" || run["jiraKey"] != "MER-1" || run["workspaceId"] != "ws-1" {
		t.Fatalf("run tags: %v", run)
	}
	runID := run["id"].(string)
	_, d = do("alice", "GET", "/qa/mr-tests?linkId=l1&iid=1", "")
	if n := len(d["runs"].([]any)); n != 1 {
		t.Fatalf("runs: %v", d)
	}

	// Results on the MR and on Jira.
	before := len(posts())
	if code, d := do("alice", "POST", "/qa/mr-tests/post", `{"linkId":"l1","iid":1,"runId":"`+runID+`","target":"mr"}`); code != 200 {
		t.Fatalf("post mr %d %v", code, d)
	}
	if ps := posts(); len(ps) != before+1 || !strings.Contains(ps[len(ps)-1]["body"].(string), "1 passed, 0 failed") {
		t.Fatalf("mr comment: %v", ps)
	}
	if code, d := do("alice", "POST", "/qa/mr-tests/post", `{"linkId":"l1","iid":1,"runId":"`+runID+`","target":"jira"}`); code != 200 {
		t.Fatalf("post jira %d %v", code, d)
	}
	// Defect → Jira issue.
	code, d = do("alice", "POST", "/qa/defects/d1/jira", `{"jiraConnectionId":"j1","projectKey":"mer"}`)
	if code != 200 || d["key"] != "MER-500" {
		t.Fatalf("defect→jira %d %v", code, d)
	}
	var def models.Defect
	db.First(&def, "id = ?", "d1")
	if def.JiraKey != "MER-500" {
		t.Fatalf("key not stored: %v", def)
	}
	if code, _ := do("alice", "POST", "/qa/defects/d1/jira", `{"jiraConnectionId":"j1","projectKey":"MER"}`); code != http.StatusConflict {
		t.Fatalf("second create: %d", code)
	}
	jmu.Lock()
	calls := strings.Join(jiraCalls, "\n")
	jmu.Unlock()
	if !strings.Contains(calls, "POST /rest/api/2/issue/MER-1/comment") || !strings.Contains(calls, "POST /rest/api/2/issue ") || !strings.Contains(calls, "Steps to reproduce") {
		t.Fatalf("jira calls:\n%s", calls)
	}

	// P0.b fix: a workspace member can execute a shared case.
	if code, d := do("bob", "POST", "/test-cases/tc1/execute", `{}`); code != 200 {
		t.Fatalf("member executes shared case: %d %v", code, d)
	}
}
