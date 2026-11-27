package handlers

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"patchman-be/internal/config"
	"patchman-be/internal/database"
	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
)

// Fake GitLab for the radar: project 7 has four open MRs and one merged;
// project 8 pages forever (truncation). Field names follow the GitLab REST
// docs (doğrulanmadı live).
func fakeRadarGitLab(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		q := r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case p == "/api/v4/user" && strings.Contains(r.Header.Get("Authorization"), "bob"):
			_, _ = w.Write([]byte(`{"username":"bo"}`))
		case p == "/api/v4/user":
			_, _ = w.Write([]byte(`{"username":"ada"}`))
		case p == "/api/v4/projects/7/merge_requests" && q.Get("state") == "opened":
			w.Header().Set("X-Total", "4")
			w.Header().Set("X-Next-Page", "")
			_, _ = w.Write([]byte(`[
{"iid":1,"title":"MER-1 fix login","state":"opened","source_branch":"fix","sha":"s1","updated_at":"2026-11-25T10:00:00Z","has_conflicts":true,"author":{"username":"ada","name":"Ada"},"reviewers":[{"username":"bo","name":"Bo"}]},
{"iid":2,"title":"Cart","state":"opened","source_branch":"feature/MER-2-cart","sha":"s2","updated_at":"2026-11-25T09:00:00Z","blocking_discussions_resolved":true,"author":{"username":"bo","name":"Bo"},"reviewers":[{"username":"ada","name":"Ada"}]},
{"iid":3,"title":"no key here","state":"opened","source_branch":"misc","sha":"s3","updated_at":"2026-11-25T08:00:00Z","author":{"username":"bo"}},
{"iid":4,"title":"MER-999 ghost","state":"opened","source_branch":"g","sha":"s4","updated_at":"2026-11-25T07:00:00Z","author":{"username":"bo"}}]`))
		case p == "/api/v4/projects/7/merge_requests" && q.Get("state") == "merged":
			_, _ = w.Write([]byte(`[{"iid":9,"title":"MER-2 early","state":"merged","source_branch":"x","sha":"s9","updated_at":"2026-11-24T10:00:00Z","merged_at":"2026-11-24T10:00:00Z","author":{"username":"bo"}}]`))
		case p == "/api/v4/projects/8/merge_requests" && q.Get("state") == "opened":
			w.Header().Set("X-Total", "250")
			w.Header().Set("X-Next-Page", "x")
			_, _ = w.Write([]byte(`[{"iid":` + q.Get("page") + `,"title":"page","state":"opened","sha":"p` + q.Get("page") + `","updated_at":"2026-11-25T10:00:00Z","author":{"username":"bo"}}]`))
		case strings.HasSuffix(p, "/approvals"):
			_, _ = w.Write([]byte(`{"approvals_left":1,"approved_by":[]}`))
		case strings.HasSuffix(p, "/diffs"):
			switch {
			case strings.Contains(p, "/merge_requests/2/"):
				_, _ = w.Write([]byte(`[{"new_path":"a.go","old_path":"a.go"},{"new_path":"shared.go","old_path":"shared.go"}]`))
			case strings.Contains(p, "/merge_requests/3/"):
				_, _ = w.Write([]byte(`[{"new_path":"shared.go","old_path":"shared.go"},{"new_path":"shared_test.go","old_path":"shared_test.go"}]`))
			default:
				_, _ = w.Write([]byte(`[]`))
			}
		case strings.HasSuffix(p, "/commits"):
			if strings.Contains(p, "/merge_requests/3/") {
				_, _ = w.Write([]byte(`[{"message":"x\n\nCo-Authored-By: Claude <noreply@anthropic.com>"}]`))
			} else {
				_, _ = w.Write([]byte(`[{"message":"plain"}]`))
			}
		case strings.HasPrefix(p, "/api/v4/projects/7/merge_requests/1"):
			_, _ = w.Write([]byte(`{"changes_count":"12","head_pipeline":{"status":"failed"}}`))
		case strings.Contains(p, "/merge_requests/"):
			_, _ = w.Write([]byte(`{"changes_count":"3","head_pipeline":{"status":"success"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

// Fake GitHub: o/r has one open PR with failing checks.
func fakeRadarGitHub(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case p == "/user":
			_, _ = w.Write([]byte(`{"login":"ada"}`))
		case p == "/repos/o/r/pulls" && r.URL.Query().Get("state") == "open":
			_, _ = w.Write([]byte(`[{"number":5,"title":"MER-2 gh side","state":"open","html_url":"u","updated_at":"2026-11-25T10:00:00Z","user":{"login":"ada"},"requested_reviewers":[],"head":{"ref":"h","sha":"g5"},"base":{"ref":"main"}}]`))
		case p == "/repos/o/r/pulls":
			_, _ = w.Write([]byte(`[]`))
		case p == "/repos/o/r/pulls/5":
			_, _ = w.Write([]byte(`{"number":5,"state":"open","mergeable":true,"mergeable_state":"unstable","changed_files":2,"head":{"sha":"g5"}}`))
		case p == "/repos/o/r/pulls/5/reviews":
			_, _ = w.Write([]byte(`[{"state":"APPROVED","user":{"login":"bo"}}]`))
		case p == "/repos/o/r/pulls/5/files":
			_, _ = w.Write([]byte(`[{"filename":"x.go"}]`))
		case p == "/repos/o/r/pulls/5/commits":
			_, _ = w.Write([]byte(`[{"commit":{"message":"m"}}]`))
		default:
			http.NotFound(w, r)
		}
	}))
}

// Fake Jira Data Center: MER-1 done, MER-2 in test; any other key makes
// the whole `key in (…)` JQL fail with 400, as Jira does (doğrulanmadı on
// Cloud).
func fakeRadarJira(t *testing.T, searches *int32) *httptest.Server {
	issues := map[string]string{
		"MER-1": `{"key":"MER-1","fields":{"summary":"Login","status":{"name":"Done","statusCategory":{"key":"done"}},"assignee":{"displayName":"Ada"}}}`,
		"MER-2": `{"key":"MER-2","fields":{"summary":"Cart","status":{"name":"In Test","statusCategory":{"key":"indeterminate"}},"assignee":{"displayName":"Ada"}}}`,
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/rest/api/2/search" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(searches, 1)
		jql := r.URL.Query().Get("jql")
		if strings.HasPrefix(jql, "assignee = currentUser()") {
			_, _ = w.Write([]byte(`{"startAt":0,"total":1,"issues":[` + issues["MER-2"] + `]}`))
			return
		}
		inner := strings.TrimSuffix(strings.TrimPrefix(jql, "key in ("), ")")
		out := []string{}
		for _, k := range strings.Split(inner, ",") {
			body, ok := issues[k]
			if !ok {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"errorMessages":["An issue with key '` + k + `' does not exist for field 'key'."]}`))
				return
			}
			out = append(out, body)
		}
		_, _ = w.Write([]byte(`{"startAt":0,"total":` + itoa(len(out)) + `,"issues":[` + strings.Join(out, ",") + `]}`))
	}))
}

func TestQARadarAndInbox(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetQACaches()
	gl := fakeRadarGitLab(t)
	defer gl.Close()
	gh := fakeRadarGitHub(t)
	defer gh.Close()
	SetGitHubAPIBase(gh.URL)
	defer SetGitHubAPIBase("")
	var searches int32
	jr := fakeRadarJira(t, &searches)
	defer jr.Close()

	db, err := database.Init("sqlite", filepath.Join(t.TempDir(), "radar.db"), "")
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
	db.Create(&models.GitConnection{ID: "gh1", UserID: "alice", Name: "gh", Provider: "github", BaseURL: "https://github.com", AuthKind: "pat", CredentialEnc: enc})
	db.Create(&models.JiraConnection{ID: "j1", UserID: "alice", Name: "jira", BaseURL: jr.URL, Flavor: "datacenter", AuthKind: "pat", CredentialEnc: enc})
	ws := "ws-1"
	db.Create(&models.QAProjectLink{ID: "l1", UserID: "alice", WorkspaceID: &ws, GitConnectionID: "gl1", GitProject: "7", JiraConnectionID: "j1", JiraProjectKey: "MER"})
	db.Create(&models.QAProjectLink{ID: "l2", UserID: "alice", WorkspaceID: &ws, GitConnectionID: "gl1", GitProject: "8"})
	db.Create(&models.QAProjectLink{ID: "l3", UserID: "alice", WorkspaceID: &ws, GitConnectionID: "gh1", GitProject: "o/r", JiraConnectionID: "j1", JiraProjectKey: "MER"})
	db.Create(&models.QASettings{ID: "s1", UserID: "alice", WorkspaceID: &ws, TestPatterns: "**/*_test.go", TestStatuses: "In Test", SLAHours: 0})

	gitConns := NewGitConnectionHandler(db, cipher)
	h := NewQARadarHandler(db, NewGitRemoteHandler(db, &config.Config{}, gitConns, nil), NewJiraConnectionHandler(db, cipher))
	h.now = func() time.Time { return time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC) }
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", c.GetHeader("X-User")); c.Next() })
	r.GET("/qa/radar", h.Radar)
	r.GET("/qa/inbox", h.Inbox)
	r.POST("/qa/inbox/snooze", NewQASettingsHandler(db).Snooze)
	r.POST("/qa/links", NewQALinkHandler(db).Create)
	r.PUT("/qa/links/:id/mine", h.PutMine)
	get := func(user, path string) (int, map[string]any) {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("X-User", user)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		d, _ := out["data"].(map[string]any)
		return w.Code, d
	}

	code, d := get("alice", "/qa/radar?workspaceId=ws-1&refresh=1")
	if code != 200 {
		t.Fatalf("radar %d %v", code, d)
	}
	mrs := map[string]map[string]any{}
	for _, x := range d["mrs"].([]any) {
		m := x.(map[string]any)
		mrs[m["ref"].(string)] = m
	}
	sigs := func(ref string) string {
		m, ok := mrs[ref]
		if !ok {
			t.Fatalf("missing %s in %v", ref, keys(mrs))
		}
		out := []string{}
		for _, s := range m["signals"].([]any) {
			out = append(out, s.(map[string]any)["kind"].(string))
		}
		return strings.Join(out, ",")
	}
	want := map[string]string{
		"mr:l1:1": "stuck,jiraMismatch", // conflict + failed pipeline; Jira done while open
		"mr:l1:2": "noTests,collision",  // a.go without tests; shares shared.go with !3
		"mr:l1:3": "aiAssisted,noJira,collision",
		"mr:l1:4": "noJira",        // MER-999 doesn't exist
		"mr:l3:5": "stuck,noTests", // GitHub failing checks; x.go without a test
	}
	for ref, w := range want {
		if got := sigs(ref); got != w {
			t.Errorf("%s: signals %q, want %q", ref, got, w)
		}
	}
	if j := mrs["mr:l1:2"]["jira"].(map[string]any); j["status"] != "In Test" || j["statusCategory"] != "indeterminate" {
		t.Errorf("jira attach: %v", j)
	}
	if j := mrs["mr:l1:4"]["jira"].(map[string]any); j["missing"] != true {
		t.Errorf("missing key: %v", j)
	}
	if mrs["mr:l3:5"]["approvalsGiven"].(float64) != 1 {
		t.Errorf("gh approvals: %v", mrs["mr:l3:5"])
	}
	merged := d["merged"].([]any)
	if len(merged) != 1 || merged[0].(map[string]any)["ref"] != "mr:l1:9" {
		t.Errorf("merged mismatch: %v", merged)
	}
	links := d["links"].([]any)
	for _, x := range links {
		l := x.(map[string]any)
		switch l["id"] {
		case "l2":
			if l["truncated"] != true || l["shown"].(float64) != 3 || l["total"].(float64) != 250 {
				t.Errorf("truncation not reported: %v", l)
			}
		default:
			if l["status"] != "ok" {
				t.Errorf("link %v", l)
			}
		}
	}

	// Inbox for alice (= ada on GitLab / GitHub): review !2, stuck !1 and
	// the GitHub PR (her own), test issue MER-2.
	_, in := get("alice", "/qa/inbox?workspaceId=ws-1")
	got := []string{}
	for _, x := range in["items"].([]any) {
		it := x.(map[string]any)
		got = append(got, it["kind"].(string)+":"+it["ref"].(string))
	}
	joined := strings.Join(got, " ")
	for _, w := range []string{"review:mr:l1:2", "stuck:mr:l1:1", "stuck:mr:l3:5", "test:jira:j1:MER-2"} {
		if !strings.Contains(joined, w) {
			t.Errorf("inbox missing %s: %v", w, got)
		}
	}

	// Snooze the review until its head changes.
	req := httptest.NewRequest("POST", "/qa/inbox/snooze", strings.NewReader(`{"itemRef":"mr:l1:2@s2","hours":4}`))
	req.Header.Set("X-User", "alice")
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(httptest.NewRecorder(), req)
	_, in = get("alice", "/qa/inbox?workspaceId=ws-1")
	if in["snoozed"].(float64) != 1 || strings.Contains(toJSON(in["items"]), `"kind":"review"`) {
		t.Errorf("snooze: %v", in)
	}

	// bob is in the workspace but has no grant on alice's connections: he
	// sees the links with noAccess and none of the MRs.
	_, d = get("bob", "/qa/radar?workspaceId=ws-1&refresh=1")
	if n := len(d["mrs"].([]any)); n != 0 {
		t.Fatalf("bob sees %d MRs through alice's access", n)
	}
	for _, x := range d["links"].([]any) {
		if l := x.(map[string]any); l["status"] != "noAccess" {
			t.Errorf("bob link status %v", l)
		}
	}

	// Granted read access, bob sees the MRs — but the connections' stored
	// tokens are alice's identity, so none of alice's reviews, stuck MRs or
	// Jira test queue become bob's Inbox.
	db.Create(&models.GitConnectionGrant{ConnectionID: "gl1", SubjectKind: "user", SubjectID: "bob", Role: "read"})
	db.Create(&models.GitConnectionGrant{ConnectionID: "gh1", SubjectKind: "user", SubjectID: "bob", Role: "read"})
	db.Create(&models.JiraConnectionGrant{ID: "jg1", ConnectionID: "j1", SubjectKind: "user", SubjectID: "bob", Role: "read", GrantedBy: "alice"})
	_, d = get("bob", "/qa/radar?workspaceId=ws-1&refresh=1")
	if n := len(d["mrs"].([]any)); n != 8 {
		t.Fatalf("bob with grants sees %d MRs, want 8", n)
	}
	if me := d["me"].(map[string]any); len(me) != 0 {
		t.Fatalf("bob got an identity from alice's tokens: %v", me)
	}
	for _, x := range d["links"].([]any) {
		if l := x.(map[string]any); l["status"] != "ok" || l["personal"] != false {
			t.Errorf("bob granted link %v", l)
		}
	}
	_, in = get("bob", "/qa/inbox?workspaceId=ws-1&refresh=1")
	if n := len(in["items"].([]any)); n != 0 || in["jiraSkipped"].(float64) != 1 {
		t.Fatalf("bob's inbox leaks alice's identity: %v", in)
	}

	// bob maps link l1 to his own GitLab and Jira connections: now the
	// radar knows he is "bo", so !1 (bo is its reviewer) is his to review,
	// and his own Jira credential gives him a test queue.
	encBob, _ := cipher.Encrypt("tok-bob")
	db.Create(&models.GitConnection{ID: "glb", UserID: "bob", Name: "bob gl", Provider: "gitlab", BaseURL: gl.URL, AuthKind: "pat", CredentialEnc: encBob})
	db.Create(&models.JiraConnection{ID: "jb", UserID: "bob", Name: "bob jira", BaseURL: jr.URL, Flavor: "datacenter", AuthKind: "pat", CredentialEnc: encBob})
	put := func(user, path, body string) int {
		req := httptest.NewRequest("PUT", path, strings.NewReader(body))
		req.Header.Set("X-User", user)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	if code := put("bob", "/qa/links/l1/mine", `{"gitConnectionId":"gl1"}`); code != 200 {
		t.Fatalf("map to a granted connection: %d", code)
	}
	if code := put("bob", "/qa/links/l1/mine", `{"gitConnectionId":"nope"}`); code != 400 {
		t.Fatalf("map to an unusable connection must fail: %d", code)
	}
	if code := put("bob", "/qa/links/l1/mine", `{"gitConnectionId":"glb","jiraConnectionId":"jb"}`); code != 200 {
		t.Fatalf("map own: %d", code)
	}
	_, in = get("bob", "/qa/inbox?workspaceId=ws-1&refresh=1")
	joined = toJSON(in["items"])
	if !strings.Contains(joined, `"ref":"mr:l1:1"`) || !strings.Contains(joined, `"kind":"test"`) || in["jiraSkipped"].(float64) != 1 { // l3 still reads alice's Jira
		t.Fatalf("bob's own inbox after mapping: %v", in)
	}
	for _, x := range in["links"].([]any) {
		if l := x.(map[string]any); l["id"] == "l1" && (l["personal"] != true || l["mapped"] != true || l["gitConnectionId"] != "glb") {
			t.Errorf("mapped link %v", l)
		}
	}
	// alice is unaffected by bob's mapping.
	_, d = get("alice", "/qa/radar?workspaceId=ws-1&refresh=1")
	for _, x := range d["links"].([]any) {
		if l := x.(map[string]any); l["id"] == "l1" && (l["mapped"] != false || l["gitConnectionId"] != "gl1") {
			t.Errorf("alice sees bob's mapping: %v", l)
		}
	}
	// Removing the mapping (both empty) goes back to the link's connections.
	if code := put("bob", "/qa/links/l1/mine", `{}`); code != 200 {
		t.Fatalf("unmap: %d", code)
	}

	// Cached: a second radar within the TTL doesn't hit Jira again.
	get("alice", "/qa/radar?workspaceId=ws-1") // warm (bob's unmapping dropped the space's cache)
	before := atomic.LoadInt32(&searches)
	get("alice", "/qa/radar?workspaceId=ws-1")
	if atomic.LoadInt32(&searches) != before {
		t.Errorf("radar not cached")
	}
}

func keys(m map[string]map[string]any) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A new link shows up at once, though an empty radar was cached a moment
// before (the first thing a user does after linking).
func TestQARadarLinkInvalidatesCache(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetQACaches()
	gl := fakeRadarGitLab(t)
	defer gl.Close()
	db, err := database.Init("sqlite", filepath.Join(t.TempDir(), "inv.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	db.Create(&models.User{ID: "alice", Email: "alice@example.com", Name: "alice"})
	cipher, _ := services.NewTokenCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32)))
	enc, _ := cipher.Encrypt("tok")
	db.Create(&models.GitConnection{ID: "gl1", UserID: "alice", Name: "gl", Provider: "gitlab", BaseURL: gl.URL, AuthKind: "pat", CredentialEnc: enc})
	h := NewQARadarHandler(db, NewGitRemoteHandler(db, &config.Config{}, NewGitConnectionHandler(db, cipher), nil), NewJiraConnectionHandler(db, cipher))
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", "alice"); c.Next() })
	r.GET("/qa/radar", h.Radar)
	r.POST("/qa/links", NewQALinkHandler(db).Create)
	do := func(method, path, body string) map[string]any {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		d, _ := out["data"].(map[string]any)
		return d
	}
	if d := do("GET", "/qa/radar", ""); len(d["links"].([]any)) != 0 {
		t.Fatalf("expected empty: %v", d)
	}
	do("POST", "/qa/links", `{"gitConnectionId":"gl1","gitProject":"7"}`)
	if d := do("GET", "/qa/radar", ""); len(d["links"].([]any)) != 1 || len(d["mrs"].([]any)) != 4 {
		t.Fatalf("stale radar after linking: %v", d)
	}
}
