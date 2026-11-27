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

// Fix check (CR_QA_P4_RFC §1): a review posted at h1, the MR is now at h2.
func TestQAVerifyFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetQACaches()
	var mu sync.Mutex
	var calls []string
	var compareURL string
	head, compareFail := "h2", false
	getHead := func() string { mu.Lock(); defer mu.Unlock(); return head }
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		rec := func() {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			calls = append(calls, r.Method+" "+r.URL.RequestURI()+" "+string(b))
			mu.Unlock()
		}
		switch {
		case p == "/api/v4/user":
			_, _ = w.Write([]byte(`{"username":"ada"}`))
		case p == "/api/v4/projects/7/merge_requests" && r.URL.Query().Get("state") == "opened":
			w.Header().Set("X-Total", "1")
			_, _ = w.Write([]byte(`[{"iid":1,"title":"MER-1 login","state":"opened","source_branch":"f","sha":"` + getHead() + `","updated_at":"2026-11-27T10:00:00Z","author":{"username":"bo"},"reviewers":[{"username":"ada"}]}]`))
		case p == "/api/v4/projects/7/merge_requests":
			_, _ = w.Write([]byte(`[]`))
		case p == "/api/v4/projects/7/merge_requests/1" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"iid":1,"title":"MER-1 login","state":"opened","source_branch":"f","target_branch":"main","web_url":"u","sha":"` + getHead() + `","author":{"username":"bo"},"reviewers":[{"username":"ada"}],"diff_refs":{"base_sha":"b1","start_sha":"s1","head_sha":"` + getHead() + `"}}`))
		case p == "/api/v4/projects/7/repository/compare":
			mu.Lock()
			compareURL = r.URL.RawQuery
			fail := compareFail
			mu.Unlock()
			if fail {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"404 Commit Not Found"}`))
				return
			}
			_, _ = w.Write([]byte(`{"diffs":[{"old_path":"a.go","new_path":"a.go","diff":"@@ -2 +2 @@\n-DB_PASSWORD=Sup3rS3cret!\n+pw := os.Getenv(\"DB\")\n"}]}`))
		case p == "/api/v4/projects/7/merge_requests/1/discussions/d1" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"id":"d1","notes":[{"resolvable":true,"resolved":false}]}`))
		case strings.HasPrefix(p, "/api/v4/projects/7/merge_requests/1/discussions/") && r.Method != http.MethodGet:
			rec()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"n1"}`))
		case strings.HasSuffix(p, "/approvals"):
			_, _ = w.Write([]byte(`{"approvals_left":1,"approved_by":[]}`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer gl.Close()

	db, err := database.Init("sqlite", filepath.Join(t.TempDir(), "ver.db"), "")
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
	ws := "ws-1"
	db.Create(&models.QAProjectLink{ID: "l1", UserID: "alice", WorkspaceID: &ws, GitConnectionID: "gl1", GitProject: "7"})
	db.Create(&models.QASettings{ID: "s1", UserID: "alice", WorkspaceID: &ws, ReviewGuide: "Money is decimal, never float."})
	rev := models.MRReview{ID: "r1", WorkspaceID: &ws, LinkID: "l1", GitConnectionID: "gl1", Provider: "gitlab", Project: "7", IID: 1, HeadSHA: "h1",
		Title: "MER-1 login", Status: "posted", ClaimedBy: "alice", AuthorUsername: "bo", ReviewerUsernames: "ada"}
	setReviewFindings(&rev, []models.ReviewFinding{
		{ID: "f1", Severity: "major", File: "a.go", Line: 2, Title: "secret in code", Body: "password committed", Status: "posted", NoteID: "d1", Inline: true},
		{ID: "f2", Severity: "major", File: "b.go", Line: 9, Title: "no timeout", Body: "http call without timeout", Status: "posted", NoteID: "d2", Inline: true},
		{ID: "f3", Severity: "minor", Title: "naming", Body: "rename", Status: "pending"},
	})
	db.Create(&rev)

	h := NewQARadarHandler(db, NewGitRemoteHandler(db, &config.Config{}, NewGitConnectionHandler(db, cipher), nil), NewJiraConnectionHandler(db, cipher))
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", c.GetHeader("X-User")); c.Next() })
	r.GET("/qa/review-queue", h.ReviewQueue)
	r.GET("/qa/verify-context", h.VerifyContext)
	r.GET("/qa/verifications", h.ListVerifications)
	r.POST("/qa/verifications/:id/submit", h.SubmitVerification)
	r.POST("/qa/verifications/:id/apply", h.ApplyVerification)
	r.GET("/qa/mr-context", h.MRContext)
	r.GET("/qa/radar", h.Radar)
	r.GET("/qa/inbox", h.Inbox)
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

	// Radar: the MR shows unverifiedFixes; alice's inbox has a verify item.
	_, rad := do("alice", "GET", "/qa/radar?workspaceId=ws-1", "")
	if js := toJSON(rad); !strings.Contains(js, `"unverifiedFixes"`) || !strings.Contains(js, `"openFindings":2`) {
		t.Fatalf("radar: %s", js)
	}
	_, inb := do("alice", "GET", "/qa/inbox?workspaceId=ws-1", "")
	if js := toJSON(inb); !strings.Contains(js, `"kind":"verify"`) {
		t.Fatalf("inbox: %s", js)
	}
	resetQACaches()

	// Queue: a verify item with 2 open findings (f3 was never posted).
	_, q := do("alice", "GET", "/qa/review-queue?workspaceId=ws-1", "")
	var verify map[string]any
	for _, it := range q["items"].([]any) {
		if m := it.(map[string]any); m["kind"] == "verify" {
			verify = m
		}
	}
	if verify == nil || verify["openFindings"].(float64) != 2 || verify["reason"] != "reviewer" {
		t.Fatalf("queue verify item: %v", q)
	}

	// A rebased branch (compare fails): findings still come, with a note.
	mu.Lock()
	compareFail = true
	mu.Unlock()
	if code, cn := do("alice", "GET", "/qa/verify-context?linkId=l1&iid=1", ""); code != 200 || cn["compareNote"] == nil || len(cn["findings"].([]any)) != 2 {
		t.Fatalf("compare failure: %d %v", code, cn)
	}
	mu.Lock()
	compareFail = false
	mu.Unlock()

	// Context: compare h1→h2, redacted diff, open findings, team guide, claim.
	code, ctx := do("alice", "GET", "/qa/verify-context?linkId=l1&iid=1&claim=1", "")
	if code != 200 {
		t.Fatalf("verify-context %d %v", code, ctx)
	}
	if !strings.Contains(compareURL, "from=h1") || !strings.Contains(compareURL, "to=h2") {
		t.Fatalf("compare range: %s", compareURL)
	}
	js := toJSON(ctx)
	if strings.Contains(js, "Sup3rS3cret") {
		t.Fatalf("secret reached the agent: %s", js)
	}
	if !strings.Contains(js, "Money is decimal") {
		t.Fatalf("team guide missing: %s", js)
	}
	if fs := ctx["findings"].([]any); len(fs) != 2 || fs[0].(map[string]any)["threadResolved"] != false {
		t.Fatalf("findings: %v", fs)
	}
	ver := ctx["verification"].(map[string]any)
	if ver["claimed"] != true {
		t.Fatalf("claim: %v", ver)
	}
	vid := ver["id"].(string)

	// The MR review context carries the team guide too.
	if _, mc := do("alice", "GET", "/qa/mr-context?linkId=l1&iid=1", ""); mc["teamGuide"] != "Money is decimal, never float." {
		t.Fatalf("mr-context teamGuide: %v", mc["teamGuide"])
	}

	// Only the claimer submits; bad verdicts and unposted findings are refused.
	if code, _ := do("bob", "POST", "/qa/verifications/"+vid+"/submit", `{"verdicts":[]}`); code != http.StatusForbidden {
		t.Fatalf("bob submits: %d", code)
	}
	if code, _ := do("alice", "POST", "/qa/verifications/"+vid+"/submit", `{"verdicts":[{"findingId":"f1","verdict":"maybe"}]}`); code != http.StatusBadRequest {
		t.Fatalf("bad verdict: %d", code)
	}
	if code, _ := do("alice", "POST", "/qa/verifications/"+vid+"/submit", `{"verdicts":[{"findingId":"f3","verdict":"fixed"}]}`); code != http.StatusBadRequest {
		t.Fatalf("unposted finding: %d", code)
	}
	code, sub := do("alice", "POST", "/qa/verifications/"+vid+"/submit",
		`{"summary":"one fixed","model":"claude-x","verdicts":[{"findingId":"f1","verdict":"fixed","evidence":"a.go:2 reads env; was ghp_abcdefghijklmnopqrstuvwxyz0123456789"},{"findingId":"f2","verdict":"not_fixed","evidence":"b.go:9 unchanged"}]}`)
	if code != 200 || sub["status"] != "submitted" || strings.Contains(toJSON(sub), "ghp_abc") {
		t.Fatalf("submit %d %v", code, sub)
	}

	// A second claim at the same head is refused (one check per commit).
	if _, again := do("alice", "GET", "/qa/verify-context?linkId=l1&iid=1&claim=1", ""); again["verification"].(map[string]any)["claimed"] != false {
		t.Fatalf("second claim: %v", again["verification"])
	}

	// Submitted, not applied: alice's inbox has an "apply" item under its own
	// ref, and the queue no longer offers a fix check (no loop).
	resetQACaches()
	_, inb2 := do("alice", "GET", "/qa/inbox?workspaceId=ws-1", "")
	if js := toJSON(inb2); !strings.Contains(js, `"ref":"mr:l1:1#verify"`) || !strings.Contains(js, `"reason":"apply 2"`) {
		t.Fatalf("inbox after submit: %s", js)
	}
	_, q2 := do("alice", "GET", "/qa/review-queue?workspaceId=ws-1", "")
	if strings.Contains(toJSON(q2), `"kind":"verify"`) {
		t.Fatalf("queue still offers the check: %v", q2)
	}

	// New commits after the check: applying is refused.
	mu.Lock()
	head = "h3"
	mu.Unlock()
	if code, _ := do("alice", "POST", "/qa/verifications/"+vid+"/apply", `{"resolve":["f1"]}`); code != http.StatusConflict {
		t.Fatalf("stale apply: %d", code)
	}
	mu.Lock()
	head = "h2"
	mu.Unlock()

	// Apply: bob may not; alice resolves f1 and replies on f2.
	if code, _ := do("bob", "POST", "/qa/verifications/"+vid+"/apply", `{"resolve":["f1"],"reply":["f2"]}`); code != http.StatusForbidden {
		t.Fatalf("bob applies: %d", code)
	}
	// Wrong actions for the verdicts send nothing (no resolve on not_fixed,
	// no "still open" on fixed).
	if _, ap0 := do("alice", "POST", "/qa/verifications/"+vid+"/apply", `{"resolve":["f2"],"reply":["f1"]}`); ap0["sent"].(float64) != 0 {
		t.Fatalf("mismatched apply sent: %v", ap0)
	}
	code, ap := do("alice", "POST", "/qa/verifications/"+vid+"/apply", `{"resolve":["f1"],"reply":["f2"]}`)
	if code != 200 || ap["sent"].(float64) != 2 {
		t.Fatalf("apply %d %v", code, ap)
	}
	mu.Lock()
	got := strings.Join(calls, "\n")
	mu.Unlock()
	if !strings.Contains(got, "POST /api/v4/projects/7/merge_requests/1/discussions/d1/notes") || !strings.Contains(got, "PUT /api/v4/projects/7/merge_requests/1/discussions/d1?resolved=true") {
		t.Fatalf("f1 not replied+resolved:\n%s", got)
	}
	if !strings.Contains(got, "discussions/d2/notes") || strings.Contains(got, "d2?resolved=true") || !strings.Contains(got, "Still open") {
		t.Fatalf("f2 should get a reply only:\n%s", got)
	}
	// Re-apply sends nothing new.
	do("alice", "POST", "/qa/verifications/"+vid+"/apply", `{"resolve":["f1"],"reply":["f2"]}`)
	mu.Lock()
	n := len(calls)
	mu.Unlock()
	if n != 3 {
		t.Fatalf("re-apply sent again: %d calls", n)
	}

	// f1 is resolved, f2 stays open (not fixed).
	var after models.MRReview
	db.First(&after, "id = ?", "r1")
	for _, f := range reviewFindings(&after) {
		if f.ID == "f1" && (f.FindingOpen() || !f.Resolved) || f.ID == "f2" && (!f.FindingOpen() || f.Verdict != "not_fixed") {
			t.Fatalf("finding state: %+v", f)
		}
	}
	_, l := do("alice", "GET", "/qa/verifications?linkId=l1&iid=1", "")
	_ = l
}
