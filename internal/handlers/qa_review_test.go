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

// A fake GitLab for the AI review flow: MR !1 at head h1 with a diff that
// contains a secret; posted discussions are recorded.
func fakeReviewGitLab(t *testing.T) (*httptest.Server, func() []map[string]any) {
	var mu sync.Mutex
	var posts []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case p == "/api/v4/user" && strings.Contains(r.Header.Get("Authorization"), "bob"):
			_, _ = w.Write([]byte(`{"username":"stranger"}`))
		case p == "/api/v4/user":
			_, _ = w.Write([]byte(`{"username":"ada"}`))
		case p == "/api/v4/projects/7/merge_requests" && r.URL.Query().Get("state") == "opened":
			w.Header().Set("X-Total", "1")
			_, _ = w.Write([]byte(`[{"iid":1,"title":"MER-1 login","state":"opened","source_branch":"f","sha":"h1","updated_at":"2026-11-25T10:00:00Z","author":{"username":"bo"},"reviewers":[{"username":"ada"}]}]`))
		case p == "/api/v4/projects/7/merge_requests":
			_, _ = w.Write([]byte(`[]`))
		case p == "/api/v4/projects/7/merge_requests/1" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"iid":1,"title":"MER-1 login","description":"fix","state":"opened","source_branch":"f","target_branch":"main","web_url":"u","sha":"h1","author":{"username":"bo"},"reviewers":[{"username":"ada"}],"diff_refs":{"base_sha":"b1","start_sha":"s1","head_sha":"h1"}}`))
		case p == "/api/v4/projects/7/merge_requests/1/diffs":
			_, _ = w.Write([]byte(`[{"old_path":"a.go","new_path":"a.go","diff":"@@ -1 +1,2 @@\n+DB_PASSWORD=Sup3rS3cret!\n+x := 1\n"}]`))
		case p == "/api/v4/projects/7/merge_requests/1/commits":
			_, _ = w.Write([]byte(`[{"message":"login fix"}]`))
		case p == "/api/v4/projects/7/merge_requests/1/discussions" && r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			mu.Lock()
			posts = append(posts, m)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"d` + itoa(len(posts)) + `"}`))
		case strings.HasSuffix(p, "/approvals"):
			_, _ = w.Write([]byte(`{"approvals_left":1,"approved_by":[]}`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	return srv, func() []map[string]any { mu.Lock(); defer mu.Unlock(); return append([]map[string]any(nil), posts...) }
}

func TestQAReviewFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetQACaches()
	gl, posts := fakeReviewGitLab(t)
	defer gl.Close()
	db, err := database.Init("sqlite", filepath.Join(t.TempDir(), "rev.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"alice", "bob", "carol"} {
		db.Create(&models.User{ID: id, Email: id + "@example.com", Name: id})
	}
	teamID := "team-1"
	db.Create(&models.Team{ID: teamID, Name: "QA", CreatedBy: "alice"})
	db.Create(&models.TeamMember{TeamID: teamID, UserID: "alice", Role: "admin"})
	db.Create(&models.TeamMember{TeamID: teamID, UserID: "bob", Role: "member"})
	db.Create(&models.TeamMember{TeamID: teamID, UserID: "carol", Role: "member"})
	db.Create(&models.Workspace{ID: "ws-1", Name: "Team", OwnerID: "alice", TeamID: &teamID, Type: "team"})
	cipher, _ := services.NewTokenCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32)))
	enc, _ := cipher.Encrypt("tok")
	encBob, _ := cipher.Encrypt("tok-bob")
	db.Create(&models.GitConnection{ID: "gl1", UserID: "alice", Name: "gl", Provider: "gitlab", BaseURL: gl.URL, AuthKind: "pat", CredentialEnc: enc})
	db.Create(&models.GitConnection{ID: "glb", UserID: "bob", Name: "gl", Provider: "gitlab", BaseURL: gl.URL, AuthKind: "pat", CredentialEnc: encBob})
	ws := "ws-1"
	db.Create(&models.QAProjectLink{ID: "l1", UserID: "alice", WorkspaceID: &ws, GitConnectionID: "gl1", GitProject: "7"})
	db.Create(&models.QALinkConnectionMap{ID: "m1", LinkID: "l1", UserID: "bob", GitConnectionID: "glb"})

	h := NewQARadarHandler(db, NewGitRemoteHandler(db, &config.Config{}, NewGitConnectionHandler(db, cipher), nil), NewJiraConnectionHandler(db, cipher))
	wh := NewQAWebhookHandler(db)
	lh := NewQALinkHandler(db)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", c.GetHeader("X-User")); c.Next() })
	r.GET("/qa/review-queue", h.ReviewQueue)
	r.POST("/qa/reviews/claim", h.Claim)
	r.GET("/qa/mr-context", h.MRContext)
	r.POST("/qa/reviews/:id/submit", h.Submit)
	r.PATCH("/qa/reviews/:id/findings/:fid", h.Decide)
	r.POST("/qa/reviews/:id/post", h.Post)
	r.GET("/qa/reviews", h.ListReviews)
	r.GET("/qa/reviews/:id", h.GetReview)
	r.POST("/qa/links/:id/webhook", lh.CreateWebhookSecret)
	r.POST("/hook/:linkId", wh.GitLab)
	do := func(user, method, path, body string, hdr ...string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-User", user)
		req.Header.Set("Content-Type", "application/json")
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
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

	// Queue: !1 is alice's to review.
	_, q := do("alice", "GET", "/qa/review-queue?workspaceId=ws-1", "")
	items := q["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["reason"] != "reviewer" {
		t.Fatalf("queue: %v", q)
	}

	// Context with claim: diff redacted, review claimed.
	code, ctx := do("alice", "GET", "/qa/mr-context?linkId=l1&iid=1&claim=1", "")
	if code != 200 {
		t.Fatalf("context %d %v", code, ctx)
	}
	if b, _ := json.Marshal(ctx); strings.Contains(string(b), "Sup3rS3cret") || ctx["redactions"].(float64) < 1 {
		t.Fatalf("secret reached the agent: %s", b)
	}
	rev := ctx["review"].(map[string]any)
	if rev["claimed"] != true {
		t.Fatalf("claim: %v", rev)
	}
	id := rev["id"].(string)

	// bob can't take a fresh claim.
	if code, b := do("bob", "POST", "/qa/reviews/claim", `{"linkId":"l1","iid":1}`); code != http.StatusConflict {
		t.Log(b)
		t.Fatalf("double claim: %d", code)
	}
	// Only the claimer submits.
	if code, _ := do("bob", "POST", "/qa/reviews/"+id+"/submit", `{"findings":[]}`); code != http.StatusForbidden {
		t.Fatalf("bob submit: %d", code)
	}
	code, sub := do("alice", "POST", "/qa/reviews/"+id+"/submit",
		`{"summary":"two issues","model":"claude-x","findings":[{"severity":"minor","title":"naming","body":"rename x"},{"severity":"major","file":"a.go","line":2,"title":"secret in code","body":"token ghp_abcdefghijklmnopqrstuvwxyz0123456789 committed","suggestion":"x := 1"}]}`)
	if code != 200 || sub["status"] != "draft" {
		t.Fatalf("submit %d %v", code, sub)
	}
	fs := sub["findings"].([]any)
	if fs[0].(map[string]any)["severity"] != "major" || strings.Contains(toJSON(fs), "ghp_abc") {
		t.Fatalf("findings not sorted / not redacted: %v", fs)
	}
	majorID := fs[0].(map[string]any)["id"].(string)

	// The MR is no longer in the queue.
	_, q = do("alice", "GET", "/qa/review-queue?workspaceId=ws-1", "")
	if n := len(q["items"].([]any)); n != 0 {
		t.Fatalf("reviewed MR still queued: %v", q)
	}

	// bob is neither author, reviewer nor owner: no decisions.
	if code, _ := do("bob", "PATCH", "/qa/reviews/"+id+"/findings/"+majorID, `{"status":"approved"}`); code != http.StatusForbidden {
		t.Fatalf("bob decides: %d", code)
	}
	if code, _ := do("alice", "PATCH", "/qa/reviews/"+id+"/findings/"+majorID, `{"status":"approved"}`); code != 200 {
		t.Fatalf("approve: %d", code)
	}
	// Post: summary + the approved finding inline; the pending one stays.
	code, pr := do("alice", "POST", "/qa/reviews/"+id+"/post", "")
	if code != 200 || pr["posted"].(float64) != 1 {
		t.Fatalf("post %d %v", code, pr)
	}
	ps := posts()
	if len(ps) != 2 {
		t.Fatalf("posted %d comments: %v", len(ps), ps)
	}
	pos, _ := ps[1]["position"].(map[string]any)
	if pos == nil || pos["new_path"] != "a.go" || pos["head_sha"] != "h1" || !strings.Contains(ps[1]["body"].(string), "[major] secret in code") {
		t.Fatalf("inline post: %v", ps[1])
	}
	// Posting again doesn't duplicate.
	do("alice", "POST", "/qa/reviews/"+id+"/post", "")
	if len(posts()) != 2 {
		t.Fatalf("re-post duplicated: %d", len(posts()))
	}

	// carol is in the workspace but can't use the Git connection: she sees
	// no reviews (they hold code and titles from the project).
	if _, l := do("carol", "GET", "/qa/reviews?workspaceId=ws-1", ""); toJSON(l) != "{}" && len(asList(l)) != 0 {
		t.Fatalf("carol lists reviews: %v", l)
	}
	if code, _ := do("carol", "GET", "/qa/reviews/"+id, ""); code != http.StatusForbidden {
		t.Fatalf("carol reads a review: %d", code)
	}
	// A write grant on alice's stored token doesn't let carol post as alice.
	db.Create(&models.GitConnectionGrant{ConnectionID: "gl1", SubjectKind: "user", SubjectID: "carol", Role: "write"})
	if code, _ := do("carol", "POST", "/qa/reviews/"+id+"/post", ""); code != http.StatusForbidden {
		t.Fatalf("carol posts under alice's token: %d", code)
	}

	// Webhook: bad token 401, good token 200.
	_, sec := do("alice", "POST", "/qa/links/l1/webhook", "")
	secret := sec["secret"].(string)
	if code, _ := do("", "POST", "/hook/l1", `{}`, "X-Gitlab-Token", "nope"); code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", code)
	}
	if code, _ := do("", "POST", "/hook/l1", `{"object_kind":"merge_request","object_attributes":{"iid":1,"last_commit":{"id":"h2"}}}`, "X-Gitlab-Token", secret); code != 200 {
		t.Fatalf("good token: %d", code)
	}
	_, list := do("alice", "GET", "/qa/reviews?workspaceId=ws-1", "")
	_ = list
}

func asList(m map[string]any) []any {
	if v, ok := m["data"].([]any); ok {
		return v
	}
	return nil
}
