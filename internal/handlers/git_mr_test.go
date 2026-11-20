package handlers

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"patchman-be/internal/config"
	"patchman-be/internal/middleware"
	"patchman-be/internal/models"
	"patchman-be/internal/services"
)

type mrFakeCall struct {
	Method, Path, Query string
	Body                map[string]any
}

// A fake GitLab that records every call and answers per path.
func fakeMRGitLab(t *testing.T) (*httptest.Server, func() []mrFakeCall) {
	var mu sync.Mutex
	var calls []mrFakeCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		mu.Lock()
		calls = append(calls, mrFakeCall{r.Method, r.URL.Path, r.URL.RawQuery, body})
		mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, "/api/v4")
		switch {
		case p == "/user":
			_, _ = w.Write([]byte(`{"username":"ada"}`))
		case strings.HasSuffix(p, "/merge_requests") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[{"iid":3,"title":"Draft: Add greet","state":"opened","draft":true,"user_notes_count":2,"author":{"name":"Ada"}}]`))
		case strings.HasSuffix(p, "/merge_requests") && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"iid":4,"web_url":"https://gl/mr/4","state":"opened","title":"x"}`))
		case strings.HasSuffix(p, "/merge_requests/4") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"iid":4,"title":"Add greet"}`))
		case strings.HasSuffix(p, "/discussions") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[{"id":"d1","notes":[{"id":1,"body":"nit","resolvable":true,"resolved":false,"author":{"name":"Bo","username":"bo"},"position":{"new_path":"a.go","old_path":"a.go","new_line":7}}]},{"id":"d2","notes":[{"id":2,"body":"added 1 commit","system":true}]}]`))
		case strings.HasSuffix(p, "/merge"):
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = w.Write([]byte(`{"message":"Branch cannot be merged"}`))
		default:
			_, _ = w.Write([]byte(`{"id":"ok"}`))
		}
	}))
	return srv, func() []mrFakeCall { mu.Lock(); defer mu.Unlock(); return append([]mrFakeCall(nil), calls...) }
}

func mrRig(t *testing.T, gl *httptest.Server, role string) *GitRemoteHandler {
	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.GitConnection{}, &models.GitConnectionGrant{}, &models.GitActivityHistory{}); err != nil {
		t.Fatal(err)
	}
	db.Exec("CREATE TABLE IF NOT EXISTS team_members (team_id TEXT, user_id TEXT)")
	cipher, _ := services.NewTokenCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32)))
	enc, _ := cipher.Encrypt("glpat-x")
	db.Create(&models.GitConnection{ID: "c1", UserID: "owner", Name: "gl", BaseURL: gl.URL, AuthKind: "pat", CredentialEnc: enc})
	if role != "" {
		db.Create(&models.GitConnectionGrant{ConnectionID: "c1", SubjectKind: "user", SubjectID: "u2", Role: role})
	}
	return NewGitRemoteHandler(db, &config.Config{}, NewGitConnectionHandler(db, cipher), nil)
}

func callMR(h func(*gin.Context), user, method, query string, body any) (int, map[string]any) {
	gin.SetMode(gin.TestMode)
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, "/x?"+query, rd)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "c1"}}
	c.Set("userID", user)
	h(c)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestMRWorkflowAgainstFakeGitLab(t *testing.T) {
	gl, calls := fakeMRGitLab(t)
	defer gl.Close()
	h := mrRig(t, gl, "read")

	// List: "waiting for my review" resolves my username first.
	code, out := callMR(h.MergeRequests, "owner", "GET", "project=7&mine=review&search=greet", nil)
	if code != 200 || !strings.Contains(toJSON(out), `"draft":true`) || !strings.Contains(toJSON(out), `"notes":2`) {
		t.Fatalf("list: %d %v", code, out)
	}
	last := calls()[len(calls())-1]
	if !strings.Contains(last.Query, "reviewer_username=ada") || !strings.Contains(last.Query, "search=greet") {
		t.Fatalf("list query: %s", last.Query)
	}

	// Create: draft prefixes the title, options pass through.
	code, out = callMR(h.CreateMR, "owner", "POST", "project=7", map[string]any{
		"sourceBranch": "feature/x", "targetBranch": "main", "title": "Add greet", "draft": true, "squash": true, "reviewerIds": []int{5},
	})
	if code != 200 || data(out)["iid"].(float64) != 4 {
		t.Fatalf("create: %d %v", code, out)
	}
	cb := calls()[len(calls())-1].Body
	if cb["title"] != "Draft: Add greet" || cb["squash"] != true || cb["reviewer_ids"] == nil {
		t.Fatalf("create body: %v", cb)
	}

	// A read grant cannot write.
	if code, _ := callMR(h.CreateMR, "u2", "POST", "project=7", map[string]any{"sourceBranch": "a", "targetBranch": "b", "title": "t"}); code != http.StatusForbidden {
		t.Fatalf("read grant created an MR: %d", code)
	}

	// Mark ready: draft=false strips the prefix from the current title.
	if code, out := callMR(h.UpdateMR, "owner", "POST", "project=7&iid=4", map[string]any{"draft": false}); code != 200 {
		t.Fatalf("update: %d %v", code, out)
	}
	if b := calls()[len(calls())-1].Body; b["title"] != "Add greet" {
		t.Fatalf("ready title: %v", b)
	}

	// Discussions: system notes dropped, inline position kept.
	code, out = callMR(h.MRDiscussions, "owner", "GET", "project=7&iid=4", nil)
	list, _ := out["data"].([]any)
	if code != 200 || len(list) != 1 || !strings.Contains(toJSON(list[0]), `"newLine":7`) {
		t.Fatalf("discussions: %d %v", code, out)
	}

	// Inline comment carries GitLab's position shape.
	nl := 12
	code, _ = callMR(h.CreateMRDiscussion, "owner", "POST", "project=7&iid=4", map[string]any{
		"body": "why?", "position": map[string]any{"baseSha": "b", "startSha": "s", "headSha": "h", "newPath": "a.go", "oldPath": "a.go", "newLine": nl},
	})
	pos, _ := calls()[len(calls())-1].Body["position"].(map[string]any)
	if code != 200 || pos["position_type"] != "text" || pos["new_line"].(float64) != 12 || pos["head_sha"] != "h" {
		t.Fatalf("inline: %d %v", code, pos)
	}
	if code, _ := callMR(h.CreateMRDiscussion, "owner", "POST", "project=7&iid=4", map[string]any{"body": "x", "position": map[string]any{"newPath": "a.go"}}); code != 400 {
		t.Fatal("position without refs must be refused")
	}

	// Resolve + reply.
	if code, _ := callMR(h.ResolveMRDiscussion, "owner", "POST", "project=7&iid=4&discussion=d1", map[string]any{"resolved": true}); code != 200 {
		t.Fatal("resolve")
	}
	if c := calls()[len(calls())-1]; c.Method != "PUT" || c.Query != "resolved=true" || !strings.HasSuffix(c.Path, "/discussions/d1") {
		t.Fatalf("resolve call: %+v", c)
	}
	if code, _ := callMR(h.ReplyMRDiscussion, "owner", "POST", "project=7&iid=4&discussion=d1", map[string]any{"body": "done"}); code != 200 {
		t.Fatal("reply")
	}

	// Merge: GitLab's refusal comes back readable.
	code, out = callMR(h.MergeMR, "owner", "POST", "project=7&iid=4", map[string]any{"squash": true, "sha": "abc"})
	if code != http.StatusBadGateway || !strings.Contains(toJSON(out), "Branch cannot be merged") {
		t.Fatalf("merge refusal: %d %v", code, out)
	}
	if b := calls()[len(calls())-1].Body; b["sha"] != "abc" || b["squash"] != true {
		t.Fatalf("merge body: %v", b)
	}
	// An edited message goes to merge_commit_message (or squash_commit_message when squashing).
	callMR(h.MergeMR, "owner", "POST", "project=7&iid=4", map[string]any{"mergeCommitMessage": "Merge branch 'a' into 'main'\n\nWhy", "squashCommitMessage": "ignored"})
	if b := calls()[len(calls())-1].Body; b["merge_commit_message"] != "Merge branch 'a' into 'main'\n\nWhy" || b["squash_commit_message"] != nil {
		t.Fatalf("merge message body: %v", b)
	}
	callMR(h.MergeMR, "owner", "POST", "project=7&iid=4", map[string]any{"squash": true, "squashCommitMessage": "One commit", "mergeCommitMessage": "ignored"})
	if b := calls()[len(calls())-1].Body; b["squash_commit_message"] != "One commit" || b["merge_commit_message"] != nil {
		t.Fatalf("squash message body: %v", b)
	}
	if !middleware.IsSensitiveRoute("POST", "/api/git/connections/c1/merge_request_merge") {
		t.Fatal("merge must need step-up")
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
