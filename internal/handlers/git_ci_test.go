package handlers

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"patchman-be/internal/config"
	"patchman-be/internal/models"
	"patchman-be/internal/services"
)

// fakeGitLab answers the CI/CD endpoints for project team/wms (group team).
// A read_api token ("glpat-read") gets insufficient_scope on writes.
type fakeGitLab struct {
	mu    sync.Mutex
	posts []string
}

func (f *fakeGitLab) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.EscapedPath()
	write := func(code int, body string) { w.WriteHeader(code); _, _ = io.WriteString(w, body) }
	if r.Method == http.MethodPost {
		f.mu.Lock()
		f.posts = append(f.posts, p)
		f.mu.Unlock()
		if r.Header.Get("Authorization") == "Bearer glpat-read" {
			write(403, `{"error":"insufficient_scope","error_description":"The request requires higher privileges than provided by the access token."}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(p, "/pipeline") && !bytes.Contains(b, []byte(`"ref":"main"`)) {
			write(400, `{"message":"bad ref"}`)
			return
		}
		write(201, `{"id":99,"status":"created","web_url":"https://gl/p/99"}`)
		return
	}
	switch p {
	case "/api/v4/projects/team%2Fwms":
		write(200, `{"path_with_namespace":"team/wms","namespace":{"kind":"group","full_path":"team"}}`)
	case "/api/v4/projects/team%2Fwms/pipelines":
		write(200, `[{"id":11,"iid":3,"status":"failed","source":"schedule","ref":"main","sha":"abc","web_url":"https://gl/p/11","created_at":"2026-11-11T01:00:00Z"}]`)
	case "/api/v4/projects/team%2Fwms/pipelines/11":
		write(200, `{"id":11,"project_id":42,"status":"failed","ref":"main","duration":95.5,"user":{"name":"Ada"},"web_url":"https://gl/team/wms/-/pipelines/11"}`)
	case "/api/v4/projects/team%2Fwms/pipelines/11/jobs":
		write(200, `[{"id":6,"name":"deploy","stage":"deploy","status":"manual"},{"id":5,"name":"test","stage":"test","status":"failed","failure_reason":"script_failure"},{"id":4,"name":"build","stage":"build","status":"success","duration":30}]`)
	case "/api/v4/projects/team%2Fwms/pipelines/11/bridges":
		// Trigger jobs: a child pipeline (same project), a multi-project one
		// and a manual one not started yet.
		write(200, `[{"id":9,"name":"zz-other","stage":"deploy","status":"running","downstream_pipeline":{"id":31,"project_id":77,"ref":"main","status":"running","web_url":"https://gl/team/other/-/pipelines/31"}},
			{"id":8,"name":"ingresses","stage":"ingress","status":"manual","web_url":"https://gl/team/wms/-/jobs/8"},
			{"id":7,"name":"adminapi","stage":"deploy","status":"success","downstream_pipeline":{"id":21,"project_id":42,"ref":"main","status":"success","web_url":"https://gl/team/wms/-/pipelines/21"}}]`)
	case "/api/v4/projects/team%2Fwms/pipelines/12":
		write(200, `{"id":12,"status":"success","ref":"main"}`)
	case "/api/v4/projects/team%2Fwms/pipelines/12/jobs":
		write(200, `[{"id":40,"name":"build","stage":"build","status":"success"}]`)
	case "/api/v4/projects/team%2Fwms/pipelines/12/bridges":
		write(403, `{"message":"403 Forbidden"}`)
	case "/api/v4/projects/team%2Fwms/repository/branches":
		// 100 feature branches on page 1 (GitLab sorts upper case first),
		// the default branch on page 2.
		if r.URL.Query().Get("page") == "2" {
			write(200, `[{"name":"main","default":true,"commit":{"id":"m1"}}]`)
			return
		}
		var b strings.Builder
		b.WriteString("[")
		for i := 0; i < 100; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"name":"PIM-%d","commit":{"id":"c%d"}}`, 1000+i, i)
		}
		b.WriteString("]")
		w.Header().Set("X-Next-Page", "2")
		write(200, b.String())
	case "/api/v4/projects/team%2Fwms/jobs/5":
		write(200, `{"id":5,"name":"test","stage":"test","status":"failed","failure_reason":"script_failure"}`)
	case "/api/v4/projects/team%2Fwms/jobs/5/trace":
		var b strings.Builder
		for i := 1; i <= 50; i++ {
			b.WriteString("line " + string(rune('0'+i%10)) + "\n")
		}
		b.WriteString("section_start:1700000000:step_script\r\x1b[0K\x1b[32;1mRunning tests\x1b[0;m\n")
		b.WriteString("Downloading 10%\rDownloading 100%\n")
		b.WriteString("\x1b[31;1mERROR: 2 tests failed\x1b[0;m\n")
		b.WriteString("section_end:1700000099:step_script\r\x1b[0K\n")
		write(200, b.String())
	case "/api/v4/projects/team%2Fwms/pipeline_schedules":
		write(200, `[{"id":3,"description":"Nightly sync","ref":"refs/heads/main","cron":"0 3 * * *","cron_timezone":"Europe/Istanbul","next_run_at":"2026-11-12T00:00:00Z","active":true,"owner":{"name":"Ada"}}]`)
	case "/api/v4/groups/team/variables":
		write(403, `{"message":"403 Forbidden"}`)
	case "/api/v4/projects/team%2Fwms/variables":
		write(200, `[{"key":"DB_PASS","value":"s3cret","masked":true,"protected":true,"environment_scope":"*"},
			{"key":"SYNC_CRON","value":"0 3 * * *","masked":false,"environment_scope":"*"},
			{"key":"API_KEY","value":"","masked":true,"hidden":true,"environment_scope":"prod"}]`)
	case "/api/v4/projects/team%2Fwms/variables/DB_PASS":
		write(200, `{"key":"DB_PASS","value":"s3cret","masked":true}`)
	default:
		write(404, `{"message":"404 Not Found"}`)
	}
}

func ciTestHandler(t *testing.T, pat string) (*GitRemoteHandler, *fakeGitLab, *gorm.DB) {
	t.Helper()
	gl := &fakeGitLab{}
	srv := httptest.NewServer(gl)
	t.Cleanup(srv.Close)
	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+pat+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.GitConnection{}, &models.GitConnectionGrant{}, &models.GitActivityHistory{}); err != nil {
		t.Fatal(err)
	}
	db.Exec("CREATE TABLE IF NOT EXISTS team_members (team_id TEXT, user_id TEXT)")
	cipher, _ := services.NewTokenCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32)))
	enc, _ := cipher.Encrypt(pat)
	db.Create(&models.GitConnection{ID: "c1", UserID: "owner", Name: "gl", BaseURL: srv.URL, AuthKind: "pat", CredentialEnc: enc})
	db.Create(&models.GitConnectionGrant{ConnectionID: "c1", SubjectKind: "user", SubjectID: "reader", Role: "read"})
	return NewGitRemoteHandler(db, &config.Config{}, NewGitConnectionHandler(db, cipher), nil), gl, db
}

func ciCall(t *testing.T, fn gin.HandlerFunc, user, method, query string, body any) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, "/x?"+query, rdr)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "c1"}}
	c.Set("userID", user)
	fn(c)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestGitCIReads(t *testing.T) {
	h, _, _ := ciTestHandler(t, "glpat-full")
	code, out := ciCall(t, h.Pipelines, "reader", "GET", "project=team/wms", nil)
	if code != 200 || !strings.Contains(mustJSON(out), `"source":"schedule"`) {
		t.Fatalf("pipelines = %d %v", code, out)
	}
	code, out = ciCall(t, h.Pipeline, "reader", "GET", "project=team/wms&pipeline=11", nil)
	d := out["data"].(map[string]any)
	if code != 200 || mustJSON(d["stages"]) != `["build","test","deploy","ingress"]` || len(d["jobs"].([]any)) != 6 {
		t.Fatalf("pipeline = %d %v", code, out)
	}
	code, out = ciCall(t, h.JobLog, "reader", "GET", "project=team/wms&job=5&lines=20", nil)
	d = out["data"].(map[string]any)
	text := d["text"].(string)
	if code != 200 || d["truncated"] != true || strings.Contains(text, "\x1b") || strings.Contains(text, "section_") ||
		!strings.Contains(text, "Running tests") || !strings.Contains(text, "\nDownloading 100%\n") || !strings.HasSuffix(strings.TrimSpace(text), "ERROR: 2 tests failed") {
		t.Fatalf("job log = %d %q", code, text)
	}
	code, out = ciCall(t, h.PipelineSchedules, "reader", "GET", "project=team/wms", nil)
	if code != 200 || !strings.Contains(mustJSON(out), `"cron":"0 3 * * *"`) || !strings.Contains(mustJSON(out), `"ref":"main"`) {
		t.Fatalf("schedules = %d %v", code, out)
	}
}

func TestGitCIVariablesKeepSecrets(t *testing.T) {
	h, _, db := ciTestHandler(t, "glpat-full")
	// A read-only grantee can't read CI/CD variables through the shared token.
	if code, _ := ciCall(t, h.CIVariables, "reader", "GET", "project=team/wms", nil); code != 403 {
		t.Errorf("reader got variables: %d", code)
	}
	code, out := ciCall(t, h.CIVariables, "owner", "GET", "project=team/wms", nil)
	js := mustJSON(out)
	if code != 200 || strings.Contains(js, "s3cret") || strings.Contains(js, `"value"`) || !strings.Contains(js, "Maintainer or Owner role on the group") {
		t.Fatalf("variables = %d %s", code, js)
	}
	// includeValues (values=unmasked): only the unmasked, unhidden value.
	_, out = ciCall(t, h.CIVariables, "owner", "GET", "project=team/wms&values=unmasked", nil)
	js = mustJSON(out)
	if strings.Contains(js, "s3cret") || !strings.Contains(js, `"value":"0 3 * * *"`) {
		t.Fatalf("unmasked values = %s", js)
	}
	// One value on request, audited.
	code, out = ciCall(t, h.CIVariable, "owner", "GET", "project=team/wms&key=DB_PASS&scope=*", nil)
	if code != 200 || out["data"].(map[string]any)["value"] != "s3cret" {
		t.Fatalf("reveal = %d %v", code, out)
	}
	var n int64
	for i := 0; i < 50 && n == 0; i++ { // the audit row is written asynchronously
		time.Sleep(10 * time.Millisecond)
		db.Model(&models.GitActivityHistory{}).Where("op = ?", "ci_variable_reveal").Count(&n)
	}
	if n != 1 {
		t.Errorf("reveal audits = %d", n)
	}
}

func TestGitCIWrites(t *testing.T) {
	h, gl, _ := ciTestHandler(t, "glpat-full")
	if code, _ := ciCall(t, h.RetryPipeline, "reader", "POST", "project=team/wms&pipeline=11", nil); code != 403 {
		t.Errorf("read grantee retried: %d", code)
	}
	if code, out := ciCall(t, h.RunPipeline, "owner", "POST", "project=team/wms", map[string]any{"ref": "main", "variables": []map[string]string{{"key": "DRY", "value": "1"}}}); code != 200 || out["data"].(map[string]any)["id"] != float64(99) {
		t.Errorf("run = %d %v", code, out)
	}
	for _, f := range []gin.HandlerFunc{h.RetryPipeline, h.CancelPipeline} {
		if code, _ := ciCall(t, f, "owner", "POST", "project=team/wms&pipeline=11", nil); code != 200 {
			t.Errorf("pipeline write = %d", code)
		}
	}
	ciCall(t, h.PlayJob, "owner", "POST", "project=team/wms&job=6", nil)
	ciCall(t, h.PlaySchedule, "owner", "POST", "project=team/wms&schedule=3", nil)
	want := []string{"/api/v4/projects/team%2Fwms/pipeline", "/api/v4/projects/team%2Fwms/pipelines/11/retry", "/api/v4/projects/team%2Fwms/pipelines/11/cancel",
		"/api/v4/projects/team%2Fwms/jobs/6/play", "/api/v4/projects/team%2Fwms/pipeline_schedules/3/play"}
	if mustJSON(gl.posts) != mustJSON(want) {
		t.Errorf("posts = %v", gl.posts)
	}

	// A read_api token: a readable explanation, not a raw 403.
	h2, _, _ := ciTestHandler(t, "glpat-read")
	code, out := ciCall(t, h2.RetryPipeline, "owner", "POST", "project=team/wms&pipeline=11", nil)
	if code != 502 || !strings.Contains(out["error"].(string), "needs a token with the api scope") {
		t.Errorf("read_api write = %d %v", code, out)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// Trigger jobs (bridges) sit beside the jobs in pipeline order — stages by
// their first job id, jobs by name — with their downstream pipelines; GitLab's
// /jobs alone left them out (deploy / ingress stages missing).
func TestGitCIPipelineIncludesTriggerJobs(t *testing.T) {
	h, _, _ := ciTestHandler(t, "glpat-full")
	code, out := ciCall(t, h.Pipeline, "reader", "GET", "project=team/wms&pipeline=11", nil)
	if code != 200 {
		t.Fatalf("pipeline = %d %v", code, out)
	}
	d := out["data"].(map[string]any)
	var names []string
	byName := map[string]map[string]any{}
	for _, j := range d["jobs"].([]any) {
		m := j.(map[string]any)
		names = append(names, m["stage"].(string)+"/"+m["name"].(string))
		byName[m["name"].(string)] = m
	}
	if got := strings.Join(names, " "); got != "build/build test/test deploy/adminapi deploy/deploy deploy/zz-other ingress/ingresses" {
		t.Fatalf("job order = %s", got)
	}
	if _, has := d["bridgesError"]; has {
		t.Fatalf("unexpected bridgesError: %v", d["bridgesError"])
	}
	if byName["deploy"]["kind"] != "job" || byName["ingresses"]["kind"] != "bridge" || byName["ingresses"]["status"] != "manual" {
		t.Fatalf("kinds = %v / %v", byName["deploy"], byName["ingresses"])
	}
	child := byName["adminapi"]["downstream"].(map[string]any)
	if child["id"] != float64(21) || child["sameProject"] != true || child["status"] != "success" {
		t.Fatalf("child downstream = %v", child)
	}
	other := byName["zz-other"]["downstream"].(map[string]any)
	if other["sameProject"] != false || other["webUrl"] != "https://gl/team/other/-/pipelines/31" {
		t.Fatalf("multi-project downstream = %v", other)
	}
	if _, has := byName["ingresses"]["downstream"]; has {
		t.Fatalf("a manual trigger job that never ran has no downstream: %v", byName["ingresses"])
	}
}

// A refused /bridges keeps the jobs and says why.
func TestGitCIPipelineBridgesErrorKeepsJobs(t *testing.T) {
	h, _, _ := ciTestHandler(t, "glpat-full")
	code, out := ciCall(t, h.Pipeline, "reader", "GET", "project=team/wms&pipeline=12", nil)
	d, _ := out["data"].(map[string]any)
	if code != 200 || d == nil || len(d["jobs"].([]any)) != 1 || !strings.Contains(fmt.Sprint(d["bridgesError"]), "role") {
		t.Fatalf("pipeline 12 = %d %v", code, out)
	}
}

func TestSameProjectPipeline(t *testing.T) {
	cases := []struct {
		pid, cid int64
		pu, cu   string
		want     bool
	}{
		{42, 42, "", "", true},
		{42, 77, "https://gl/a/b/-/pipelines/1", "https://gl/a/b/-/pipelines/2", false}, // ids win
		{0, 77, "https://gl/a/b/-/pipelines/1", "https://gl/a/b/-/pipelines/2", true},
		{0, 0, "https://gl/a/b/-/pipelines/1", "https://gl/a/c/-/pipelines/2", false},
		{0, 0, "", "https://gl/a/c/-/pipelines/2", false},
	}
	for _, c := range cases {
		if got := sameProjectPipeline(c.pid, c.cid, c.pu, c.cu); got != c.want {
			t.Errorf("sameProjectPipeline(%d,%d,%q,%q) = %v", c.pid, c.cid, c.pu, c.cu, got)
		}
	}
}

// Branches come from every page, default first: one page of a repo full of
// feature branches used to leave out main.
func TestGitBranchesAllPagesDefaultFirst(t *testing.T) {
	h, _, _ := ciTestHandler(t, "glpat-full")
	code, out := ciCall(t, h.Branches, "reader", "GET", "project=team/wms", nil)
	bs, _ := out["data"].([]any)
	if code != 200 || len(bs) != 101 {
		t.Fatalf("branches = %d, %d items", code, len(bs))
	}
	first := bs[0].(map[string]any)
	if first["name"] != "main" || first["isDefault"] != true || bs[1].(map[string]any)["name"] != "PIM-1000" {
		t.Fatalf("order = %v, %v", first, bs[1])
	}
}
