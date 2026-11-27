package handlers

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"patchman-be/internal/config"
	"patchman-be/internal/models"
	"patchman-be/internal/services"
)

type ghCall struct {
	Method, Path string
	Body         map[string]any
}

// fakeGitHubPR answers the pull-request REST + GraphQL calls and records the writes.
func fakeGitHubPR(t *testing.T) (*httptest.Server, func() []ghCall) {
	var mu sync.Mutex
	calls := []ghCall{}
	pull := func(n int, state string, merged bool, user string, extra string) string {
		mergedAt := "null"
		if merged {
			mergedAt = `"2026-11-18T10:00:00Z"`
		}
		return `{"id":9` + itoa(n) + `,"node_id":"PR_node` + itoa(n) + `","number":` + itoa(n) + `,"title":"PR ` + itoa(n) + `","body":"desc","state":"` + state + `","draft":false,
"merged":` + boolS(merged) + `,"merged_at":` + mergedAt + `,"html_url":"https://github.com/acme/api/pull/` + itoa(n) + `","created_at":"2026-11-17T10:00:00Z","updated_at":"2026-11-18T10:00:00Z",
"user":{"id":1,"login":"` + user + `"},"assignees":[],"requested_reviewers":[{"id":2,"login":"octo"}],
"head":{"ref":"feature","sha":"h1","repo":{"full_name":"acme/api"}},"base":{"ref":"main","sha":"b1","repo":{"full_name":"acme/api"}}` + extra + `}`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghp_test" {
			w.WriteHeader(401)
			return
		}
		var body map[string]any
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			_ = json.Unmarshal(raw, &body)
		}
		p := r.URL.Path
		if r.Method != http.MethodGet {
			mu.Lock()
			calls = append(calls, ghCall{r.Method, p, body})
			mu.Unlock()
		}
		switch {
		case p == "/graphql":
			q, _ := body["query"].(string)
			switch {
			case strings.Contains(q, "reviewThreads"):
				_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":"PRRT_1","isResolved":false,"path":"x.go","line":7,"originalLine":7,"diffSide":"RIGHT","comments":{"nodes":[{"databaseId":55,"body":"nit","createdAt":"2026-11-18T11:00:00Z","author":{"login":"ada"}},{"databaseId":57,"body":"mine","createdAt":"2026-11-18T11:05:00Z","lastEditedAt":"2026-11-18T11:06:00Z","viewerCanUpdate":true,"author":{"login":"octo"}}]}},{"id":"PRRT_2","isResolved":true,"path":"y.go","line":null,"originalLine":3,"diffSide":"LEFT","comments":{"nodes":[{"databaseId":56,"body":"old","createdAt":"2026-11-18T11:00:00Z","author":null}]}}]}}}}}`))
			case strings.Contains(q, "convertPullRequestToDraft"), strings.Contains(q, "resolveReviewThread"), strings.Contains(q, "enablePullRequestAutoMerge"), strings.Contains(q, "updatePullRequestBranch"), strings.Contains(q, "disablePullRequestAutoMerge"):
				_, _ = w.Write([]byte(`{"data":{}}`))
			case strings.Contains(q, "addPullRequestReviewThreadReply"):
				_, _ = w.Write([]byte(`{"data":{"addPullRequestReviewThreadReply":{"comment":{"databaseId":77}}}}`))
			default:
				_, _ = w.Write([]byte(`{"errors":[{"message":"unknown query"}]}`))
			}
		case p == "/user":
			_, _ = w.Write([]byte(`{"login":"octo"}`))
		case p == "/repositories/11" || p == "/repos/acme/api":
			_, _ = w.Write([]byte(`{"full_name":"acme/api","default_branch":"main"}`))
		case p == "/repos/acme/api/pulls" && r.Method == "GET":
			if r.URL.Query().Get("state") == "closed" {
				_, _ = w.Write([]byte(`[` + pull(3, "closed", true, "ada", "") + `,` + pull(4, "closed", false, "ada", "") + `]`))
				return
			}
			_, _ = w.Write([]byte(`[` + pull(1, "open", false, "ada", "") + `,` + pull(2, "open", false, "octo", "") + `]`))
		case p == "/repos/acme/api/pulls" && r.Method == "POST":
			_, _ = w.Write([]byte(pull(5, "open", false, "octo", "")))
		case p == "/repos/acme/api/pulls/1":
			if r.Method == "PATCH" {
				_, _ = w.Write([]byte(pull(1, "closed", false, "ada", "")))
				return
			}
			_, _ = w.Write([]byte(pull(1, "open", false, "ada", `,"mergeable":true,"mergeable_state":"behind"`)))
		case p == "/repos/acme/api/pulls/2":
			_, _ = w.Write([]byte(pull(2, "open", false, "octo", `,"mergeable":null,"mergeable_state":"unknown"`)))
		case p == "/repos/acme/api/pulls/1/files":
			_, _ = w.Write([]byte(`[{"filename":"x.go","status":"modified","patch":"@@ -1 +1 @@\n-a\n+b","additions":1,"deletions":1}]`))
		case p == "/repos/acme/api/issues/1/comments":
			if r.Method == "POST" {
				_, _ = w.Write([]byte(`{"id":900}`))
				return
			}
			_, _ = w.Write([]byte(`[{"id":91,"body":"looks good","created_at":"2026-11-18T09:00:00Z","user":{"login":"ada"}},{"id":92,"body":"from me","created_at":"2026-11-18T09:30:00Z","updated_at":"2026-11-18T09:31:00Z","user":{"login":"octo"}}]`))
		case p == "/repos/acme/api/pulls/1/reviews":
			if r.Method == "POST" {
				_, _ = w.Write([]byte(`{"id":300,"state":"APPROVED"}`))
				return
			}
			_, _ = w.Write([]byte(`[{"id":201,"state":"APPROVED","body":"","user":{"login":"bob"}},{"id":202,"state":"COMMENTED","body":"hm","submitted_at":"2026-11-18T10:30:00Z","user":{"login":"bob"}},{"id":203,"state":"APPROVED","body":"","user":{"login":"octo"}}]`))
		case p == "/repos/acme/api/pulls/1/reviews/203/dismissals":
			_, _ = w.Write([]byte(`{}`))
		case p == "/repos/acme/api/pulls/1/comments":
			_, _ = w.Write([]byte(`{"id":501}`))
		case p == "/repos/acme/api/pulls/1/merge":
			_, _ = w.Write([]byte(`{"merged":true}`))
		case p == "/repos/acme/api/git/refs/heads/feature":
			w.WriteHeader(204)
		case p == "/repos/acme/api/commits/h1/check-runs":
			_, _ = w.Write([]byte(`{"total_count":2,"check_runs":[{"status":"completed","conclusion":"success"},{"status":"in_progress"}]}`))
		case p == "/repos/acme/api/commits/h1/status":
			_, _ = w.Write([]byte(`{"state":"pending","total_count":0}`))
		case p == "/repos/acme/api/collaborators":
			_, _ = w.Write([]byte(`[{"id":2,"login":"octo"},{"id":7,"login":"ada"}]`))
		case p == "/repos/acme/api/pulls/5/requested_reviewers":
			_, _ = w.Write([]byte(`{}`))
		case p == "/repos/acme/api/issues/comments/91", p == "/repos/acme/api/pulls/comments/55":
			if r.Method == "DELETE" {
				w.WriteHeader(204)
				return
			}
			_, _ = w.Write([]byte(`{"id":91}`))
		case p == "/repos/acme/api/pulls/1/commits":
			_, _ = w.Write([]byte(`[{"sha":"c1aaaaaaaaaa","commit":{"message":"first\n\nbody","author":{"name":"Ada"}}},{"sha":"c2bbbbbbbbbb","commit":{"message":"second","author":{"name":"Ada"}}}]`))
		case p == "/repos/acme/api/issues/1/reactions":
			if r.Method == "POST" {
				_, _ = w.Write([]byte(`{"id":71}`))
				return
			}
			_, _ = w.Write([]byte(`[{"id":70,"content":"+1","user":{"login":"octo"}},{"id":72,"content":"+1","user":{"login":"ada"}},{"id":73,"content":"heart","user":{"login":"ada"}}]`))
		case p == "/repos/acme/api/issues/1/reactions/70":
			w.WriteHeader(204)
		case p == "/repos/acme/api/labels":
			_, _ = w.Write([]byte(`[{"name":"bug","color":"d73a4a"}]`))
		case p == "/repos/acme/api/milestones":
			_, _ = w.Write([]byte(`[{"number":3,"title":"v2"}]`))
		case p == "/repos/acme/api/issues/1/labels", p == "/repos/acme/api/issues/1", p == "/repos/acme/api/pulls/1/requested_reviewers", p == "/repos/acme/api/issues/1/assignees":
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	return srv, func() []ghCall { mu.Lock(); defer mu.Unlock(); return append([]ghCall(nil), calls...) }
}

func itoa(n int) string { return strconv.Itoa(n) }
func boolS(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

type ghPRRig struct {
	h     *GitRemoteHandler
	calls func() []ghCall
	glHit *int
}

func newGHPRRig(t *testing.T) *ghPRRig {
	t.Helper()
	ghRepoCache.Range(func(k, _ any) bool { ghRepoCache.Delete(k); return true })
	srv, calls := fakeGitHubPR(t)
	t.Cleanup(srv.Close)
	gitHubAPIBaseOverride = srv.URL
	t.Cleanup(func() { gitHubAPIBaseOverride = "" })
	// A GitLab that must never be asked anything for a GitHub connection.
	hits := 0
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		t.Errorf("GitLab was called for a GitHub connection: %s %s (Authorization %q)", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		w.WriteHeader(500)
	}))
	t.Cleanup(gl.Close)
	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.GitConnection{}, &models.GitConnectionGrant{}, &models.GitActivityHistory{}); err != nil {
		t.Fatal(err)
	}
	cipher, _ := services.NewTokenCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)))
	enc, _ := cipher.Encrypt("ghp_test")
	db.Create(&models.GitConnection{ID: "gh1", UserID: "u1", Name: "gh", Provider: "github", BaseURL: "https://github.com", AuthKind: "pat", CredentialEnc: enc})
	h := NewGitRemoteHandler(db, &config.Config{GitLabBaseURL: gl.URL}, NewGitConnectionHandler(db, cipher), nil)
	gin.SetMode(gin.TestMode)
	return &ghPRRig{h: h, calls: calls, glHit: &hits}
}

func (r *ghPRRig) do(method, q string, body any, fn gin.HandlerFunc) (int, map[string]any) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	c.Request = httptest.NewRequest(method, "/x?"+q, rd)
	if body != nil {
		c.Request.Header.Set("Content-Type", "application/json")
	}
	c.Params = gin.Params{{Key: "id", Value: "gh1"}}
	c.Set("userID", "u1")
	fn(c)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestGitHubPullRequestReads(t *testing.T) {
	r := newGHPRRig(t)
	h := r.h

	code, out := r.do("GET", "project=11&state=opened", nil, h.MergeRequests)
	l := out["data"].([]any)
	if code != 200 || len(l) != 2 || l[0].(map[string]any)["state"] != "opened" || l[0].(map[string]any)["sourceBranch"] != "feature" || l[0].(map[string]any)["author"] != "ada" {
		t.Fatalf("open list: %d %v", code, out)
	}
	if _, out = r.do("GET", "project=11&state=merged", nil, h.MergeRequests); len(out["data"].([]any)) != 1 || out["data"].([]any)[0].(map[string]any)["iid"].(float64) != 3 {
		t.Errorf("merged filter: %v", out)
	}
	if _, out = r.do("GET", "project=11&state=closed", nil, h.MergeRequests); len(out["data"].([]any)) != 1 || out["data"].([]any)[0].(map[string]any)["iid"].(float64) != 4 {
		t.Errorf("closed (not merged) filter: %v", out)
	}
	if _, out = r.do("GET", "project=11&state=opened&mine=created", nil, h.MergeRequests); len(out["data"].([]any)) != 1 || out["data"].([]any)[0].(map[string]any)["iid"].(float64) != 2 {
		t.Errorf("mine=created: %v", out)
	}
	if _, out = r.do("GET", "project=11&state=opened&mine=review", nil, h.MergeRequests); len(out["data"].([]any)) != 2 {
		t.Errorf("mine=review (octo requested on both): %v", out)
	}
	if _, out = r.do("GET", "project=11&state=opened&search=pr+2", nil, h.MergeRequests); len(out["data"].([]any)) != 1 {
		t.Errorf("search: %v", out)
	}

	_, out = r.do("GET", "project=11&iid=1", nil, h.MergeRequest)
	d := out["data"].(map[string]any)
	if d["mergeStatus"] != "need_rebase" || d["hasConflicts"] != false || d["sha"] != "h1" || d["diffRefs"].(map[string]any)["baseSha"] != "b1" || d["description"] != "desc" {
		t.Errorf("detail: %v", out)
	}
	if _, out = r.do("GET", "project=11&iid=2", nil, h.MergeRequest); out["data"].(map[string]any)["mergeStatus"] != "checking" || out["data"].(map[string]any)["hasConflicts"] != false {
		t.Errorf("mergeable null must read as checking, not a conflict: %v", out)
	}

	_, out = r.do("GET", "project=11&iid=1", nil, h.MergeRequestChanges)
	if diffs := out["data"].(map[string]any)["diffs"].([]any); len(diffs) != 1 || diffs[0].(map[string]any)["newPath"] != "x.go" {
		t.Errorf("changes: %v", out)
	}

	_, out = r.do("GET", "project=11&iid=1", nil, h.MRDiscussions)
	ds := out["data"].([]any)
	if len(ds) != 5 {
		t.Fatalf("discussions (2 comments + review summary + 2 threads): %v", out)
	}
	if ds[0].(map[string]any)["id"] != "ic-91" || ds[1].(map[string]any)["id"] != "ic-92" || ds[2].(map[string]any)["id"] != "rv-202" {
		t.Errorf("general threads in time order: %v", ds)
	}
	th := ds[3].(map[string]any)
	if th["id"] != "PRRT_1" || th["resolvable"] != true || th["position"].(map[string]any)["newLine"].(float64) != 7 {
		t.Errorf("line thread: %v", th)
	}
	old := ds[4].(map[string]any)
	if old["resolved"] != true || old["position"].(map[string]any)["oldLine"].(float64) != 3 || old["notes"].([]any)[0].(map[string]any)["author"] != "ghost" {
		t.Errorf("outdated LEFT thread from a deleted user: %v", old)
	}

	_, out = r.do("GET", "project=11&iid=1", nil, h.MRApprovals)
	a := out["data"].(map[string]any)
	if a["approved"] != true || len(a["approvedBy"].([]any)) != 2 || a["userHasApproved"] != true || a["userCanApprove"] != false {
		t.Errorf("approvals (bob's later COMMENTED keeps his approval): %v", a)
	}

	_, out = r.do("GET", "project=11&iid=1", nil, h.MRPipelines)
	if ps := out["data"].([]any); len(ps) != 1 || ps[0].(map[string]any)["status"] != "running" {
		t.Errorf("checks folded: %v", out)
	}

	_, out = r.do("GET", "project=11&query=ad", nil, h.ProjectMembers)
	if ms := out["data"].([]any); len(ms) != 1 || ms[0].(map[string]any)["username"] != "ada" || ms[0].(map[string]any)["id"].(float64) != 7 {
		t.Errorf("members: %v", out)
	}
	if _, out = r.do("GET", "project=11&iid=1", nil, h.MergeRequestNotes); out["data"].([]any)[0].(map[string]any)["body"] != "looks good" {
		t.Errorf("notes: %v", out)
	}
}

func TestGitHubPullRequestWrites(t *testing.T) {
	r := newGHPRRig(t)
	h := r.h
	last := func() ghCall { cs := r.calls(); return cs[len(cs)-1] }

	code, out := r.do("POST", "project=11", map[string]any{"sourceBranch": "feature", "targetBranch": "main", "title": "Draft: Add x", "draft": true, "reviewerIds": []int{7}}, h.CreateMR)
	if code != 200 || out["data"].(map[string]any)["iid"].(float64) != 5 || out["data"].(map[string]any)["state"] != "opened" {
		t.Fatalf("create: %d %v", code, out)
	}
	cs := r.calls()
	if cs[0].Path != "/repos/acme/api/pulls" || cs[0].Body["head"] != "feature" || cs[0].Body["draft"] != true || cs[0].Body["title"] != "Add x" {
		t.Errorf("create body: %+v", cs[0])
	}
	if l := last(); l.Path != "/repos/acme/api/pulls/5/requested_reviewers" || l.Body["reviewers"].([]any)[0] != "ada" {
		t.Errorf("reviewer ids → logins: %+v", l)
	}

	if code, out = r.do("POST", "project=11&iid=1", map[string]any{"stateEvent": "close"}, h.UpdateMR); code != 200 || last().Body["state"] != "closed" || out["data"].(map[string]any)["state"] != "closed" {
		t.Errorf("close: %d %v %+v", code, out, last())
	}
	if code, _ = r.do("POST", "project=11&iid=1", map[string]any{"draft": true}, h.UpdateMR); code != 200 || !strings.Contains(last().Body["query"].(string), "convertPullRequestToDraft") || last().Body["variables"].(map[string]any)["id"] != "PR_node1" {
		t.Errorf("draft via GraphQL: %d %+v", code, last())
	}

	if code, _ = r.do("POST", "project=11&iid=1", map[string]any{"squash": true, "removeSourceBranch": true, "sha": "h1", "squashCommitMessage": "Add x\n\nbody"}, h.MergeMR); code != 200 {
		t.Fatalf("merge: %d", code)
	}
	cs = r.calls()
	m, del := cs[len(cs)-2], cs[len(cs)-1]
	if m.Path != "/repos/acme/api/pulls/1/merge" || m.Body["merge_method"] != "squash" || m.Body["commit_title"] != "Add x" || m.Body["commit_message"] != "body" || m.Body["sha"] != "h1" {
		t.Errorf("merge body: %+v", m)
	}
	if del.Method != "DELETE" || del.Path != "/repos/acme/api/git/refs/heads/feature" {
		t.Errorf("source branch delete: %+v", del)
	}
	if code, out = r.do("POST", "project=11&iid=1", map[string]any{"mergeWhenPipelineSucceeds": true}, h.MergeMR); code != 200 || out["data"].(map[string]any)["autoMerge"] != true || !strings.Contains(last().Body["query"].(string), "enablePullRequestAutoMerge") {
		t.Errorf("auto-merge: %d %v", code, out)
	}

	if code, _ = r.do("POST", "project=11&iid=1", nil, h.RebaseMR); code != 200 || !strings.Contains(last().Body["query"].(string), "REBASE") {
		t.Errorf("rebase: %+v", last())
	}
	if code, _ = r.do("POST", "project=11&iid=1", nil, h.ApproveMR); code != 200 || last().Body["event"] != "APPROVE" {
		t.Errorf("approve: %+v", last())
	}
	if code, _ = r.do("POST", "project=11&iid=1", nil, h.UnapproveMR); code != 200 || last().Path != "/repos/acme/api/pulls/1/reviews/203/dismissals" {
		t.Errorf("unapprove dismisses my approval: %d %+v", code, last())
	}

	if code, out = r.do("POST", "project=11&iid=1", map[string]any{"body": "hi"}, h.CreateMRDiscussion); code != 200 || out["data"].(map[string]any)["id"] != "ic-900" {
		t.Errorf("general comment: %d %v", code, out)
	}
	nl := 7
	if code, _ = r.do("POST", "project=11&iid=1", map[string]any{"body": "nit", "position": map[string]any{"headSha": "h1", "baseSha": "b1", "startSha": "b1", "newPath": "x.go", "oldPath": "x.go", "newLine": nl}}, h.CreateMRDiscussion); code != 200 {
		t.Errorf("line comment: %d", code)
	}
	if l := last(); l.Path != "/repos/acme/api/pulls/1/comments" || l.Body["side"] != "RIGHT" || l.Body["line"].(float64) != 7 || l.Body["commit_id"] != "h1" {
		t.Errorf("line comment body: %+v", l)
	}
	if code, out = r.do("POST", "project=11&iid=1&discussion=PRRT_1", map[string]any{"body": "fixed"}, h.ReplyMRDiscussion); code != 200 || out["data"].(map[string]any)["id"].(float64) != 77 {
		t.Errorf("thread reply: %d %v", code, out)
	}
	if code, _ = r.do("POST", "project=11&iid=1&discussion=PRRT_1", map[string]any{"resolved": true}, h.ResolveMRDiscussion); code != 200 || !strings.Contains(last().Body["query"].(string), "resolveReviewThread") {
		t.Errorf("resolve: %+v", last())
	}
	if code, _ = r.do("POST", "project=11&iid=1&discussion=ic-91", map[string]any{"resolved": true}, h.ResolveMRDiscussion); code != 400 {
		t.Errorf("conversation comments aren't resolvable: %d", code)
	}
	if code, _ = r.do("POST", "project=11&iid=1&discussion=ic-91", map[string]any{"body": "re"}, h.ReplyMRDiscussion); code != 200 || last().Path != "/repos/acme/api/issues/1/comments" {
		t.Errorf("reply to a conversation comment: %+v", last())
	}
	if code, _ = r.do("POST", "project=11&iid=1", map[string]any{"body": "note"}, h.CreateMRNote); code != 200 {
		t.Errorf("note: %d", code)
	}
	if *r.glHit != 0 {
		t.Errorf("GitLab hit %d times", *r.glHit)
	}
}

// Every merge-request handler, called for a GitHub connection, must answer
// from GitHub — never send the GitHub token to the GitLab API.
func TestGitHubConnectionNeverReachesGitLab(t *testing.T) {
	r := newGHPRRig(t)
	h := r.h
	for name, fn := range map[string]gin.HandlerFunc{
		"MergeRequests": h.MergeRequests, "MergeRequest": h.MergeRequest, "MergeRequestChanges": h.MergeRequestChanges,
		"MergeRequestNotes": h.MergeRequestNotes, "CreateMRNote": h.CreateMRNote, "ApproveMR": h.ApproveMR,
		"UnapproveMR": h.UnapproveMR, "CreateMR": h.CreateMR, "UpdateMR": h.UpdateMR, "MergeMR": h.MergeMR,
		"RebaseMR": h.RebaseMR, "MRDiscussions": h.MRDiscussions, "CreateMRDiscussion": h.CreateMRDiscussion,
		"ReplyMRDiscussion": h.ReplyMRDiscussion, "ResolveMRDiscussion": h.ResolveMRDiscussion,
		"MRApprovals": h.MRApprovals, "MRPipelines": h.MRPipelines, "ProjectMembers": h.ProjectMembers,
		"UpdateMRNote": h.UpdateMRNote, "DeleteMRNote": h.DeleteMRNote, "MRCommits": h.MRCommits,
		"CancelAutoMergeMR": h.CancelAutoMergeMR, "MRAwards": h.MRAwards, "ToggleMRAward": h.ToggleMRAward,
		"ProjectLabels": h.ProjectLabels, "ProjectMilestones": h.ProjectMilestones,
	} {
		before := *r.glHit
		r.do("POST", "project=11&iid=1&discussion=PRRT_1", map[string]any{"body": "x", "title": "t", "sourceBranch": "a", "targetBranch": "b"}, fn)
		if *r.glHit != before {
			t.Errorf("%s reached GitLab", name)
		}
	}
}

func TestOpenGitHubPullOnPush(t *testing.T) {
	srv, calls := fakeGitHubPR(t)
	defer srv.Close()
	gitHubAPIBaseOverride = srv.URL
	defer func() { gitHubAPIBaseOverride = "" }()
	conn := &models.GitConnection{Provider: "github", BaseURL: "https://github.com"}
	u, err := openGitHubPull(t.Context(), conn, "11", "ghp_test", "patchman/x", "main", "Add x", "d")
	if err != nil || u != "https://github.com/acme/api/pull/5" {
		t.Fatalf("pr url %q %v", u, err)
	}
	if c := calls()[0]; c.Path != "/repos/acme/api/pulls" || c.Body["head"] != "patchman/x" || c.Body["base"] != "main" {
		t.Errorf("pull body %+v", c)
	}
	if _, err := openGitHubPull(t.Context(), conn, "local:abc", "ghp_test", "a", "b", "t", ""); err == nil {
		t.Error("a stand-alone folder has no GitHub repo")
	}
}
