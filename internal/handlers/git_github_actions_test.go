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
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"patchman-be/internal/config"
	"patchman-be/internal/models"
	"patchman-be/internal/services"
)

const ciWorkflowYAML = `name: Nightly
on:
  schedule:
    - cron: '0 3 * * *'   # nightly
  workflow_dispatch:
    inputs:
      target:
        description: where
        required: false
      dry_run:
        type: boolean
jobs:
  build:
    runs-on: ubuntu-latest
`

func fakeGitHubActions(t *testing.T) (*httptest.Server, func() []ghCall) {
	var mu sync.Mutex
	calls := []ghCall{}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/blob/log" { // the signed log URL: no API token may arrive here
			if r.Header.Get("Authorization") != "" {
				t.Errorf("token forwarded to the log blob")
			}
			_, _ = w.Write([]byte("2026-11-18T10:00:00.1234567Z Run make\n2026-11-18T10:00:01.0000000Z ok\n"))
			return
		}
		if r.Header.Get("Authorization") != "Bearer ghp_test" {
			w.WriteHeader(401)
			return
		}
		var body map[string]any
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			_ = json.Unmarshal(raw, &body)
		}
		if r.Method != http.MethodGet {
			mu.Lock()
			calls = append(calls, ghCall{r.Method, p, body})
			mu.Unlock()
		}
		run := `{"id":700,"name":"CI","display_title":"Fix","run_number":42,"run_attempt":1,"event":"push","status":"completed","conclusion":"failure","head_branch":"main","head_sha":"s1","html_url":"https://github.com/acme/api/actions/runs/700","created_at":"2026-11-18T10:00:00Z","updated_at":"2026-11-18T10:05:00Z","run_started_at":"2026-11-18T10:00:30Z","workflow_id":5,"actor":{"login":"ada"}}`
		q := r.URL.Query()
		switch {
		case p == "/repositories/11" || p == "/repos/acme/api":
			_, _ = w.Write([]byte(`{"full_name":"acme/api","default_branch":"main"}`))
		case p == "/repos/acme/api/actions/runs":
			if q.Get("status") != "failure" || q.Get("branch") != "main" {
				t.Errorf("run filter: %v", q)
			}
			_, _ = w.Write([]byte(`{"total_count":1,"workflow_runs":[` + run + `]}`))
		case p == "/repos/acme/api/actions/runs/700":
			_, _ = w.Write([]byte(run))
		case p == "/repos/acme/api/actions/runs/700/jobs":
			_, _ = w.Write([]byte(`{"jobs":[{"id":2,"name":"deploy / prod","status":"completed","conclusion":"success","started_at":"2026-11-18T10:01:00Z","completed_at":"2026-11-18T10:02:00Z"},{"id":1,"name":"build","status":"completed","conclusion":"failure","started_at":"2026-11-18T10:00:30Z","completed_at":"2026-11-18T10:01:00Z","steps":[{"name":"Checkout","conclusion":"success"},{"name":"Test","conclusion":"failure"}]}]}`))
		case p == "/repos/acme/api/actions/jobs/1":
			_, _ = w.Write([]byte(`{"id":1,"name":"build","status":"completed","conclusion":"failure"}`))
		case p == "/repos/acme/api/actions/jobs/1/logs":
			http.Redirect(w, r, srv.URL+"/blob/log", http.StatusFound)
		case p == "/repos/acme/api/actions/workflows":
			_, _ = w.Write([]byte(`{"workflows":[{"id":5,"name":"Nightly","path":".github/workflows/nightly.yml","state":"active"},{"id":6,"name":"CodeQL","path":"dynamic/codeql","state":"active"}]}`))
		case p == "/repos/acme/api/contents/.github/workflows/nightly.yml":
			_, _ = w.Write([]byte(`{"content":"` + base64.StdEncoding.EncodeToString([]byte(ciWorkflowYAML)) + `"}`))
		case p == "/repos/acme/api/actions/workflows/5/dispatches":
			w.WriteHeader(204)
		case p == "/repos/acme/api/actions/workflows/5/runs":
			_, _ = w.Write([]byte(`{"workflow_runs":[{"id":701,"status":"queued","created_at":"` + time.Now().UTC().Format(time.RFC3339) + `","html_url":"u"}]}`))
		case strings.HasSuffix(p, "/rerun-failed-jobs") || strings.HasSuffix(p, "/cancel") || p == "/repos/acme/api/actions/jobs/1/rerun":
			w.WriteHeader(201)
		case p == "/repos/acme/api/actions/organization-variables":
			_, _ = w.Write([]byte(`{"variables":[{"name":"ORG_V","value":"o"}]}`))
		case p == "/repos/acme/api/actions/organization-secrets":
			_, _ = w.Write([]byte(`{"secrets":[{"name":"ORG_S"}]}`))
		case p == "/repos/acme/api/actions/variables":
			_, _ = w.Write([]byte(`{"variables":[{"name":"REGION","value":"eu"}]}`))
		case p == "/repos/acme/api/actions/secrets":
			_, _ = w.Write([]byte(`{"secrets":[{"name":"TOKEN"}]}`))
		case p == "/repos/acme/api/actions/variables/REGION":
			_, _ = w.Write([]byte(`{"name":"REGION","value":"eu"}`))
		case p == "/repos/acme/api/environments":
			_, _ = w.Write([]byte(`{"environments":[{"name":"prod"}]}`))
		case p == "/repos/acme/api/environments/prod/variables":
			_, _ = w.Write([]byte(`{"variables":[{"name":"URL","value":"https://x"}]}`))
		case p == "/repos/acme/api/environments/prod/secrets":
			_, _ = w.Write([]byte(`{"secrets":[]}`))
		case p == "/repos/acme/api/environments/prod/variables/URL":
			_, _ = w.Write([]byte(`{"name":"URL","value":"https://x"}`))
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	return srv, func() []ghCall { mu.Lock(); defer mu.Unlock(); return append([]ghCall(nil), calls...) }
}

func newGHActionsRig(t *testing.T) (*ghPRRig, func() []ghCall) {
	t.Helper()
	ghRepoCache.Range(func(k, _ any) bool { ghRepoCache.Delete(k); return true })
	srv, calls := fakeGitHubActions(t)
	t.Cleanup(srv.Close)
	gitHubAPIBaseOverride = srv.URL
	t.Cleanup(func() { gitHubAPIBaseOverride = "" })
	hits := 0
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		t.Errorf("GitLab was called for a GitHub connection: %s %s", r.Method, r.URL.Path)
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
	return &ghPRRig{h: h, glHit: &hits}, calls
}

func TestGitHubActionsReads(t *testing.T) {
	r, _ := newGHActionsRig(t)
	h := r.h

	_, out := r.do("GET", "project=11&ref=main&status=failed", nil, h.Pipelines)
	ps := out["data"].(map[string]any)["pipelines"].([]any)
	p := ps[0].(map[string]any)
	if p["status"] != "failed" || p["iid"].(float64) != 42 || p["ref"] != "main" || p["user"] != "ada" || p["duration"].(float64) != 270 {
		t.Errorf("run: %v", p)
	}

	_, out = r.do("GET", "project=11&pipeline=700", nil, h.Pipeline)
	d := out["data"].(map[string]any)
	st := d["stages"].([]any)
	jobs := d["jobs"].([]any)
	if len(st) != 2 || st[0] != "jobs" || st[1] != "deploy" {
		t.Errorf("stages: %v", st)
	}
	if j := jobs[0].(map[string]any); j["name"] != "build" || j["status"] != "failed" || j["failureReason"] != "step failed: Test" || j["duration"].(float64) != 30 {
		t.Errorf("job: %v", j)
	}

	code, out := r.do("GET", "project=11&job=1&lines=50", nil, h.JobLog)
	if code != 200 || out["data"].(map[string]any)["text"] != "Run make\nok" {
		t.Errorf("log (timestamps stripped, redirect followed): %d %v", code, out)
	}

	_, out = r.do("GET", "project=11", nil, h.PipelineSchedules)
	sc := out["data"].([]any)
	if len(sc) != 1 || sc[0].(map[string]any)["cron"] != "0 3 * * *" || sc[0].(map[string]any)["dispatchable"] != true || sc[0].(map[string]any)["ref"] != "main" {
		t.Errorf("schedules: %v", out)
	}
	_, out = r.do("GET", "project=11", nil, h.Workflows)
	wf := out["data"].([]any)[0].(map[string]any)
	if ins := wf["inputs"].([]any); len(ins) != 2 || ins[0] != "target" || ins[1] != "dry_run" {
		t.Errorf("workflow inputs: %v", wf)
	}

	_, out = r.do("GET", "project=11&values=unmasked", nil, h.CIVariables)
	lv := out["data"].(map[string]any)["levels"].([]any)
	if len(lv) != 3 || lv[0].(map[string]any)["level"] != "organization" || lv[1].(map[string]any)["level"] != "repository" || lv[2].(map[string]any)["path"] != "prod" {
		t.Fatalf("levels: %v", lv)
	}
	repo := lv[1].(map[string]any)["variables"].([]any)
	if repo[0].(map[string]any)["key"] != "REGION" || repo[0].(map[string]any)["value"] != "eu" || repo[1].(map[string]any)["hidden"] != true || repo[1].(map[string]any)["value"] != nil {
		t.Errorf("repo vars (secret without value): %v", repo)
	}
	if _, out = r.do("GET", "project=11&key=REGION", nil, h.CIVariable); out["data"].(map[string]any)["value"] != "eu" {
		t.Errorf("reveal: %v", out)
	}
	if _, out = r.do("GET", "project=11&key=URL&scope=prod", nil, h.CIVariable); out["data"].(map[string]any)["value"] != "https://x" {
		t.Errorf("env reveal: %v", out)
	}
	if _, out = r.do("GET", "project=11&key=ORG_V&group=acme", nil, h.CIVariable); out["data"].(map[string]any)["value"] != "o" {
		t.Errorf("org reveal: %v", out)
	}
	if code, _ = r.do("GET", "project=11&key=TOKEN", nil, h.CIVariable); code != 404 {
		t.Errorf("a secret never reveals: %d", code)
	}
}

func TestGitHubActionsWrites(t *testing.T) {
	r, calls := newGHActionsRig(t)
	h := r.h
	last := func() ghCall { cs := calls(); return cs[len(cs)-1] }

	if code, _ := r.do("POST", "project=11&pipeline=700", nil, h.RetryPipeline); code != 200 || last().Path != "/repos/acme/api/actions/runs/700/rerun-failed-jobs" {
		t.Errorf("retry a failed run → failed jobs: %d %+v", code, last())
	}
	if code, _ := r.do("POST", "project=11&pipeline=700", nil, h.CancelPipeline); code != 200 || last().Path != "/repos/acme/api/actions/runs/700/cancel" {
		t.Errorf("cancel: %+v", last())
	}
	if code, _ := r.do("POST", "project=11&job=1", nil, h.RetryJob); code != 200 || last().Path != "/repos/acme/api/actions/jobs/1/rerun" {
		t.Errorf("job rerun: %+v", last())
	}
	if code, _ := r.do("POST", "project=11&job=1", nil, h.PlayJob); code != 400 {
		t.Errorf("no manual jobs on GitHub: %d", code)
	}
	if code, _ := r.do("POST", "project=11", map[string]any{"ref": "main"}, h.RunPipeline); code != 400 {
		t.Errorf("run without a workflow: %d", code)
	}
	code, out := r.do("POST", "project=11", map[string]any{"ref": "main", "workflow": "5", "variables": []map[string]string{{"key": "target", "value": "eu"}}}, h.RunPipeline)
	if code != 200 || out["data"].(map[string]any)["id"].(float64) != 701 {
		t.Errorf("dispatch: %d %v", code, out)
	}
	if c := calls(); c[len(c)-1].Path != "/repos/acme/api/actions/workflows/5/dispatches" || c[len(c)-1].Body["inputs"].(map[string]any)["target"] != "eu" {
		t.Errorf("dispatch body: %+v", c[len(c)-1])
	}
	if code, _ := r.do("POST", "project=11&schedule=5", nil, h.PlaySchedule); code != 200 || last().Body["ref"] != "main" {
		t.Errorf("schedule play → dispatch on the default branch: %d %+v", code, last())
	}
	if *r.glHit != 0 {
		t.Errorf("GitLab hit")
	}
}

func TestGitHubWorkflowTriggers(t *testing.T) {
	crons, d, in := ghWorkflowTriggers("on: [push, workflow_dispatch]\n")
	if len(crons) != 0 || !d || len(in) != 0 {
		t.Errorf("inline list: %v %v %v", crons, d, in)
	}
	crons, d, _ = ghWorkflowTriggers("on:\n  schedule:\n    - cron: \"*/15 * * * *\"\n    - cron: 0 0 * * 0\n  push:\n")
	if len(crons) != 2 || crons[1] != "0 0 * * 0" || d {
		t.Errorf("two crons, not dispatchable: %v %v", crons, d)
	}
}
