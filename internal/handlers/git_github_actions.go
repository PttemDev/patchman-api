package handlers

// GitHub Actions behind the CI/CD view (provider phase 4), in the GitLab
// shapes GitCIPanel renders:
//   - pipeline = workflow run (iid = run number), job = workflow job; GitHub
//     has no stages, so a reusable-workflow job "caller / callee" goes in stage
//     "caller" and everything else in "jobs".
//   - status words: queued/waiting → pending, in_progress → running,
//     failure/timed_out/startup_failure → failed, cancelled → canceled,
//     action_required → manual (waiting for an approval on GitHub).
//   - schedules = workflows with an `on: schedule` cron (read-only; "Run"
//     dispatches the workflow when it has workflow_dispatch).
//   - variables: repository / organization / environment variables (values
//     readable) + secrets (names only — GitHub never returns a secret value).
//   - run pipeline = workflow_dispatch on a chosen workflow; key=value lines
//     become the workflow's inputs.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
)

func ghRunStatus(status, conclusion string) string {
	switch status {
	case "queued", "waiting", "requested", "pending":
		return "pending"
	case "in_progress":
		return "running"
	}
	switch conclusion {
	case "success":
		return "success"
	case "failure", "timed_out", "startup_failure":
		return "failed"
	case "cancelled", "stale":
		return "canceled"
	case "skipped", "neutral":
		return "skipped"
	case "action_required":
		return "manual"
	}
	return status
}

// ghRunStatusFilter maps the FE's GitLab status filter to GitHub's query.
func ghRunStatusFilter(s string) (status, event string) {
	switch s {
	case "running":
		return "in_progress", ""
	case "pending":
		return "queued", ""
	case "success":
		return "success", ""
	case "failed":
		return "failure", ""
	case "canceled":
		return "cancelled", ""
	case "manual":
		return "action_required", ""
	case "scheduled":
		return "", "schedule"
	}
	return "", ""
}

func ghDuration(start, end string) *float64 {
	a, err1 := time.Parse(time.RFC3339, start)
	b, err2 := time.Parse(time.RFC3339, end)
	if err1 != nil || err2 != nil || b.Before(a) {
		return nil
	}
	d := b.Sub(a).Seconds()
	return &d
}

type ghRun struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	DisplayTitle string `json:"display_title"`
	RunNumber    int64  `json:"run_number"`
	RunAttempt   int    `json:"run_attempt"`
	Event        string `json:"event"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	HeadBranch   string `json:"head_branch"`
	HeadSha      string `json:"head_sha"`
	HTMLURL      string `json:"html_url"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	RunStartedAt string `json:"run_started_at"`
	WorkflowID   int64  `json:"workflow_id"`
	Path         string `json:"path"`
	Actor        *struct {
		Login string `json:"login"`
	} `json:"actor"`
}

func (r ghRun) dto() gin.H {
	st := ghRunStatus(r.Status, r.Conclusion)
	out := gin.H{
		"id": r.ID, "iid": r.RunNumber, "status": st, "source": r.Event, "ref": r.HeadBranch, "sha": r.HeadSha,
		"tag": false, "webUrl": r.HTMLURL, "createdAt": r.CreatedAt, "updatedAt": r.UpdatedAt,
		"name": r.Name, "title": r.DisplayTitle, "workflowId": r.WorkflowID,
	}
	if r.RunStartedAt != "" {
		out["startedAt"] = r.RunStartedAt
	}
	if r.Status == "completed" {
		out["finishedAt"] = r.UpdatedAt
		if d := ghDuration(r.RunStartedAt, r.UpdatedAt); d != nil {
			out["duration"] = *d
		}
	}
	if r.Actor != nil {
		out["user"] = r.Actor.Login
	}
	if r.RunAttempt > 1 {
		out["attempt"] = r.RunAttempt
	}
	return out
}

func (h *GitRemoteHandler) ghPipelines(c *gin.Context, userID string, conn *models.GitConnection, token, project string) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	q := url.Values{"per_page": {"30"}}
	if v := strings.TrimSpace(c.Query("ref")); v != "" {
		q.Set("branch", v)
	}
	if v := strings.TrimSpace(c.Query("page")); v != "" {
		q.Set("page", v)
	}
	st, ev := ghRunStatusFilter(strings.TrimSpace(c.Query("status")))
	if st != "" {
		q.Set("status", st)
	}
	if v := strings.TrimSpace(c.Query("source")); v != "" {
		ev = v
	}
	if ev != "" {
		q.Set("event", ev)
	}
	var raw struct {
		WorkflowRuns []ghRun `json:"workflow_runs"`
	}
	hdr, ok := h.ghJSON(c, conn, token, ghRepoPath(info)+"/actions/runs", q, &raw)
	if !ok {
		return
	}
	out := make([]gin.H, 0, len(raw.WorkflowRuns))
	for _, r := range raw.WorkflowRuns {
		out = append(out, r.dto())
	}
	h.conns.audit(userID, conn.ID, "pipelines", true, gin.H{"project": project})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"pipelines": out, "nextPage": ghNextPage(hdr)}})
}

type ghJob struct {
	ID          int64  `json:"id"`
	RunID       int64  `json:"run_id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	CreatedAt   string `json:"created_at"`
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at"`
	HTMLURL     string `json:"html_url"`
	RunnerName  string `json:"runner_name"`
	Steps       []struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		Number     int    `json:"number"`
	} `json:"steps"`
}

func ghJobStage(name string) string {
	if before, _, ok := strings.Cut(name, " / "); ok && strings.TrimSpace(before) != "" {
		return strings.TrimSpace(before)
	}
	return "jobs"
}

func (j ghJob) dto() gin.H {
	job := gin.H{"id": j.ID, "kind": "job", "name": j.Name, "stage": ghJobStage(j.Name), "status": ghRunStatus(j.Status, j.Conclusion),
		"allowFailure": false, "createdAt": j.CreatedAt, "webUrl": j.HTMLURL}
	if j.StartedAt != "" {
		job["startedAt"] = j.StartedAt
	}
	if j.CompletedAt != "" {
		job["finishedAt"] = j.CompletedAt
		if d := ghDuration(j.StartedAt, j.CompletedAt); d != nil {
			job["duration"] = *d
		}
	}
	if j.RunnerName != "" {
		job["runner"] = j.RunnerName
	}
	if j.Conclusion == "failure" {
		for _, s := range j.Steps {
			if s.Conclusion == "failure" {
				job["failureReason"] = "step failed: " + s.Name
				break
			}
		}
	}
	return job
}

func (h *GitRemoteHandler) ghPipeline(c *gin.Context, userID string, conn *models.GitConnection, token, project, pid string) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	var run ghRun
	if _, ok := h.ghJSON(c, conn, token, ghRepoPath(info)+"/actions/runs/"+pid, nil, &run); !ok {
		return
	}
	var entries []ciEntry
	for page := 1; page <= 5; page++ {
		var raw struct {
			Jobs []ghJob `json:"jobs"`
		}
		hdr, ok := h.ghJSON(c, conn, token, ghRepoPath(info)+"/actions/runs/"+pid+"/jobs", url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}, "filter": {"latest"}}, &raw)
		if !ok {
			return
		}
		for _, j := range raw.Jobs {
			entries = append(entries, ciEntry{id: j.ID, stage: ghJobStage(j.Name), name: j.Name, dto: j.dto()})
		}
		if ghNextPage(hdr) == 0 {
			break
		}
	}
	stages, jobs := orderCIEntries(entries)
	h.conns.audit(userID, conn.ID, "pipeline", true, gin.H{"project": project, "pipeline": pid})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"pipeline": run.dto(), "stages": stages, "jobs": jobs}})
}

// GitHub log lines start with an RFC 3339 timestamp; the view has its own.
var ghLogStamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z ?`)

func (h *GitRemoteHandler) ghJobLog(c *gin.Context, userID string, conn *models.GitConnection, token, project, job string, n int) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	var meta ghJob
	if _, ok := h.ghJSON(c, conn, token, ghRepoPath(info)+"/actions/jobs/"+job, nil, &meta); !ok {
		return
	}
	// The logs endpoint redirects to a short-lived signed URL.
	req, err := http.NewRequest(http.MethodGet, h.ghBase(conn)+ghRepoPath(info)+"/actions/jobs/"+job+"/logs", nil)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	// Never forward the token to the signed blob URL, even on the same host.
	client := *h.http
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		r.Header.Del("Authorization")
		if len(via) >= 5 {
			return http.ErrUseLastResponse
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	text, lines, total, truncated := "", 0, 0, false
	switch {
	case resp.StatusCode/100 == 2:
		ls, tot, tr := tailLog(resp.Body, n, 64<<20)
		for i, l := range ls {
			ls[i] = ghLogStamp.ReplaceAllString(l, "")
		}
		text, lines, total, truncated = strings.Join(ls, "\n"), len(ls), tot, tr
	case resp.StatusCode == http.StatusNotFound && meta.Status != "completed":
		text = "The log is available once the job finishes (GitHub publishes job logs at the end)."
	default:
		b := make([]byte, 4096)
		k, _ := resp.Body.Read(b)
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("github %d: %s", resp.StatusCode, ghMessage(b[:k]))})
		return
	}
	h.conns.audit(userID, conn.ID, "job_log", true, gin.H{"project": project, "job": job})
	jdto := meta.dto()
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"job":  gin.H{"id": meta.ID, "name": meta.Name, "stage": jdto["stage"], "status": jdto["status"], "failureReason": jdto["failureReason"], "webUrl": meta.HTMLURL},
		"text": text, "lines": lines, "totalLines": total, "truncated": truncated,
	}})
}

// ── workflows (schedules + dispatch) ───────────────────────────────────────

type ghWorkflow struct {
	ID           int64    `json:"id"`
	Name         string   `json:"name"`
	Path         string   `json:"path"`
	State        string   `json:"state"`
	HTMLURL      string   `json:"html_url"`
	UpdatedAt    string   `json:"updated_at"`
	Crons        []string `json:"crons"`
	Dispatchable bool     `json:"dispatchable"`
	Inputs       []string `json:"inputs,omitempty"`
}

var (
	ghCronRE     = regexp.MustCompile(`(?m)^\s*-\s*cron\s*:\s*['"]?([^'"#\n]+?)['"]?\s*(#.*)?$`)
	ghDispatchRE = regexp.MustCompile(`(?m)(^|[\s\[,{])workflow_dispatch\b`)
)

// ghWorkflowTriggers reads a workflow file's crons, whether it can be
// dispatched and its dispatch input names. A light text scan, not a YAML
// parser: enough for the common `on:` shapes.
func ghWorkflowTriggers(yml string) (crons []string, dispatch bool, inputs []string) {
	for _, m := range ghCronRE.FindAllStringSubmatch(yml, -1) {
		crons = append(crons, strings.TrimSpace(m[1]))
	}
	dispatch = ghDispatchRE.MatchString(yml)
	if dispatch {
		lines := strings.Split(yml, "\n")
		for i, l := range lines {
			if strings.TrimSpace(l) != "inputs:" {
				continue
			}
			ind := len(l) - len(strings.TrimLeft(l, " "))
			child := -1
			for _, nl := range lines[i+1:] {
				t := strings.TrimSpace(nl)
				if t == "" || strings.HasPrefix(t, "#") {
					continue
				}
				ni := len(nl) - len(strings.TrimLeft(nl, " "))
				if ni <= ind {
					break
				}
				if child < 0 {
					child = ni
				}
				if ni == child && strings.HasSuffix(t, ":") {
					inputs = append(inputs, strings.TrimSuffix(t, ":"))
				}
			}
			break
		}
	}
	return crons, dispatch, inputs
}

func (h *GitRemoteHandler) ghWorkflows(c *gin.Context, conn *models.GitConnection, token string, info ghRepoInfo) ([]ghWorkflow, bool) {
	key := "wf|" + conn.ID + "|" + info.FullName
	if v, ok := ghRepoCache.Load(key); ok {
		if e := v.(ghWorkflowsCached); time.Since(e.at) < 2*time.Minute {
			return e.list, true
		}
	}
	var raw struct {
		Workflows []ghWorkflow `json:"workflows"`
	}
	if _, ok := h.ghJSON(c, conn, token, ghRepoPath(info)+"/actions/workflows", url.Values{"per_page": {"100"}}, &raw); !ok {
		return nil, false
	}
	out := raw.Workflows
	for i := range out {
		if !strings.HasPrefix(out[i].Path, ".github/workflows/") {
			continue // dynamic workflows (Dependabot, CodeQL default setup) have no file
		}
		var f struct {
			Content string `json:"content"`
		}
		body, _, status, err := h.ghGET(conn, token, ghRepoPath(info)+"/contents/"+ghContentsPath(out[i].Path), nil)
		if err != nil || status != 200 || json.Unmarshal(body, &f) != nil {
			continue
		}
		yml, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
		if err != nil {
			continue
		}
		out[i].Crons, out[i].Dispatchable, out[i].Inputs = ghWorkflowTriggers(string(yml))
	}
	ghRepoCache.Store(key, ghWorkflowsCached{list: out, at: time.Now()})
	return out, true
}

type ghWorkflowsCached struct {
	list []ghWorkflow
	at   time.Time
}

// Workflows — GET .../workflows?project= (GitHub only): the run dialog's
// workflow picker.
func (h *GitRemoteHandler) Workflows(c *gin.Context) {
	_, conn, token, ok := h.auth(c)
	if !ok {
		return
	}
	project, ok := requireProject(c)
	if !ok {
		return
	}
	if !isGitHub(conn) {
		c.JSON(http.StatusOK, gin.H{"data": []any{}})
		return
	}
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	wfs, ok := h.ghWorkflows(c, conn, token, info)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": wfs})
}

func (h *GitRemoteHandler) ghPipelineSchedules(c *gin.Context, userID string, conn *models.GitConnection, token, project string) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	wfs, ok := h.ghWorkflows(c, conn, token, info)
	if !ok {
		return
	}
	out := []gin.H{}
	for _, w := range wfs {
		if len(w.Crons) == 0 {
			continue
		}
		out = append(out, gin.H{"id": w.ID, "description": w.Name, "ref": info.DefaultBranch, "cron": strings.Join(w.Crons, " · "),
			"timezone": "UTC", "nextRunAt": "", "active": w.State == "active", "updatedAt": w.UpdatedAt,
			"dispatchable": w.Dispatchable, "webUrl": w.HTMLURL, "readOnly": true})
	}
	h.conns.audit(userID, conn.ID, "pipeline_schedules", true, gin.H{"project": project})
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// ghDispatch runs workflow_dispatch; GitHub answers 204 without the run, so
// the newest run of that workflow on the ref is looked up for the FE.
func (h *GitRemoteHandler) ghDispatch(c *gin.Context, userID string, conn *models.GitConnection, token, project, workflow, ref string, inputs map[string]string, op string) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	if ref == "" {
		ref = info.DefaultBranch
	}
	body := gin.H{"ref": ref}
	if len(inputs) > 0 {
		body["inputs"] = inputs
	}
	before := time.Now().Add(-5 * time.Second)
	if _, ok := h.ghWrite(c, conn, token, userID, op, http.MethodPost, ghRepoPath(info)+"/actions/workflows/"+url.PathEscape(workflow)+"/dispatches", body,
		gin.H{"project": project, "workflow": workflow, "ref": ref}); !ok {
		return
	}
	// Best effort: the run shows up a moment after the dispatch.
	var id int64
	var webURL, status string
	for i := 0; i < 4 && id == 0; i++ {
		if i > 0 {
			time.Sleep(time.Duration(i) * 700 * time.Millisecond)
		}
		var raw struct {
			WorkflowRuns []ghRun `json:"workflow_runs"`
		}
		b, _, st, err := h.ghGET(conn, token, ghRepoPath(info)+"/actions/workflows/"+url.PathEscape(workflow)+"/runs", url.Values{"per_page": {"5"}, "branch": {ref}, "event": {"workflow_dispatch"}})
		if err != nil || st != 200 || json.Unmarshal(b, &raw) != nil {
			break
		}
		for _, r := range raw.WorkflowRuns {
			if t, err := time.Parse(time.RFC3339, r.CreatedAt); err == nil && t.After(before) {
				id, webURL, status = r.ID, r.HTMLURL, ghRunStatus(r.Status, r.Conclusion)
				break
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true, "id": id, "status": status, "webUrl": webURL}})
}

// ── variables ───────────────────────────────────────────────────────────────

type ghVar struct {
	Name      string `json:"name"`
	Value     string `json:"value"`
	UpdatedAt string `json:"updated_at"`
}

func ghVarBlock(level, path, scope string, vars []ghVar, secrets []ghVar, withValues bool, errMsg string) gin.H {
	out := gin.H{"level": level, "path": path}
	if errMsg != "" {
		out["error"] = errMsg
		return out
	}
	list := make([]gin.H, 0, len(vars)+len(secrets))
	for _, v := range vars {
		row := gin.H{"key": v.Name, "type": "env_var", "protected": false, "masked": false, "hidden": false, "raw": true, "environmentScope": scope}
		if withValues {
			row["value"] = v.Value
		}
		list = append(list, row)
	}
	for _, s := range secrets {
		list = append(list, gin.H{"key": s.Name, "type": "secret", "protected": false, "masked": true, "hidden": true, "raw": true, "environmentScope": scope,
			"description": "Secret — GitHub never returns its value"})
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i]["key"].(string) < list[j]["key"].(string) })
	out["variables"] = list
	return out
}

func ghVarsErr(status int, body []byte, what string) string {
	switch status {
	case http.StatusForbidden, http.StatusNotFound, http.StatusUnauthorized:
		return "Reading " + what + " needs a token with admin (or Actions variables / secrets read) access to this repository."
	}
	return fmt.Sprintf("github %d: %s", status, clipText(ghMessage(body), 200))
}

// ghList fetches name lists like {"variables":[...]} / {"secrets":[...]}.
func (h *GitRemoteHandler) ghList(conn *models.GitConnection, token, path, field string) ([]ghVar, int, []byte) {
	body, _, status, err := h.ghGET(conn, token, path, url.Values{"per_page": {"30"}})
	if err != nil {
		return nil, http.StatusBadGateway, []byte(err.Error())
	}
	if status != 200 {
		return nil, status, body
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(body, &m)
	var out []ghVar
	_ = json.Unmarshal(m[field], &out)
	return out, 200, nil
}

func (h *GitRemoteHandler) ghCIVariables(c *gin.Context, userID string, conn *models.GitConnection, token, project string, withValues bool) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	rp := ghRepoPath(info)
	owner, _ := ghOwnerName(info)
	blocks := []gin.H{}
	// Organization variables / secrets this repository can use.
	ov, st1, b1 := h.ghList(conn, token, rp+"/actions/organization-variables", "variables")
	os, st2, _ := h.ghList(conn, token, rp+"/actions/organization-secrets", "secrets")
	if st1 == 200 || st2 == 200 {
		if len(ov)+len(os) > 0 {
			blocks = append(blocks, ghVarBlock("organization", owner, "", ov, os, withValues, ""))
		}
	} else if st1 != http.StatusNotFound {
		blocks = append(blocks, ghVarBlock("organization", owner, "", nil, nil, false, ghVarsErr(st1, b1, "organization variables")))
	}
	// Repository.
	rv, st3, b3 := h.ghList(conn, token, rp+"/actions/variables", "variables")
	rs, st4, b4 := h.ghList(conn, token, rp+"/actions/secrets", "secrets")
	errMsg := ""
	if st3 != 200 && st4 != 200 {
		errMsg = ghVarsErr(st3, b3, "Actions variables")
		if st3 == 200 || len(b4) == 0 {
			errMsg = ghVarsErr(st4, b4, "Actions secrets")
		}
	}
	blocks = append(blocks, ghVarBlock("repository", info.FullName, "", rv, rs, withValues, errMsg))
	// Environments (first 10).
	var envs struct {
		Environments []struct {
			Name string `json:"name"`
		} `json:"environments"`
	}
	if b, _, st, err := h.ghGET(conn, token, rp+"/environments", url.Values{"per_page": {"10"}}); err == nil && st == 200 && json.Unmarshal(b, &envs) == nil {
		for _, e := range envs.Environments {
			ep := rp + "/environments/" + url.PathEscape(e.Name)
			ev, sa, _ := h.ghList(conn, token, ep+"/variables", "variables")
			es, sb, _ := h.ghList(conn, token, ep+"/secrets", "secrets")
			if (sa == 200 || sb == 200) && len(ev)+len(es) > 0 {
				blocks = append(blocks, ghVarBlock("environment", e.Name, e.Name, ev, es, withValues, ""))
			}
		}
	}
	h.conns.audit(userID, conn.ID, "ci_variables", true, gin.H{"project": project, "values": withValues})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"levels": blocks, "valuesIncluded": withValues}})
}

func (h *GitRemoteHandler) ghCIVariable(c *gin.Context, userID string, conn *models.GitConnection, token, project, key, org, scope string) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	rp := ghRepoPath(info)
	var path string
	switch {
	case org != "":
		// Organization variables readable through the repo list only.
		vars, st, b := h.ghList(conn, token, rp+"/actions/organization-variables", "variables")
		if st != 200 {
			c.JSON(http.StatusBadGateway, gin.H{"error": ghVarsErr(st, b, "organization variables")})
			return
		}
		for _, v := range vars {
			if v.Name == key {
				h.conns.audit(userID, conn.ID, "ci_variable_reveal", true, gin.H{"project": project, "group": org, "key": key})
				c.JSON(http.StatusOK, gin.H{"data": gin.H{"key": v.Name, "value": v.Value, "hidden": false}})
				return
			}
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "No organization variable " + key + " (secrets never reveal their value)."})
		return
	case scope != "" && scope != "*":
		path = rp + "/environments/" + url.PathEscape(scope) + "/variables/" + url.PathEscape(key)
	default:
		path = rp + "/actions/variables/" + url.PathEscape(key)
	}
	var v ghVar
	body, _, status, err := h.ghGET(conn, token, path, nil)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	if status == http.StatusNotFound {
		c.JSON(http.StatusNotFound, gin.H{"error": "No variable " + key + " — if it is a secret, GitHub never returns its value."})
		return
	}
	if status != 200 || json.Unmarshal(body, &v) != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": ghVarsErr(status, body, "Actions variables")})
		return
	}
	h.conns.audit(userID, conn.ID, "ci_variable_reveal", true, gin.H{"project": project, "key": key, "scope": scope})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"key": v.Name, "value": v.Value, "hidden": false}})
}

// ── writes ──────────────────────────────────────────────────────────────────

func (h *GitRemoteHandler) ghCIPost(c *gin.Context, userID string, conn *models.GitConnection, token, op, path string, detail gin.H, id int64) {
	if _, ok := h.ghWrite(c, conn, token, userID, op, http.MethodPost, path, nil, detail); !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true, "id": id, "status": "pending"}})
}

// ghRetryPipeline re-runs the failed jobs of a failed / cancelled run, or the
// whole run otherwise (what GitLab's retry does).
func (h *GitRemoteHandler) ghRetryPipeline(c *gin.Context, userID string, conn *models.GitConnection, token, project, pid string) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return
	}
	var run ghRun
	if _, ok := h.ghJSON(c, conn, token, ghRepoPath(info)+"/actions/runs/"+pid, nil, &run); !ok {
		return
	}
	suffix := "/rerun"
	switch run.Conclusion {
	case "failure", "cancelled", "timed_out", "startup_failure":
		suffix = "/rerun-failed-jobs"
	}
	id, _ := strconv.ParseInt(pid, 10, 64)
	h.ghCIPost(c, userID, conn, token, "pipeline_retry", ghRepoPath(info)+"/actions/runs/"+pid+suffix, gin.H{"project": project, "pipeline": pid}, id)
}

func (h *GitRemoteHandler) ghRepoPathOrFail(c *gin.Context, conn *models.GitConnection, token, project string) (string, bool) {
	info, ok := h.ghRepo(c, conn, token, project)
	if !ok {
		return "", false
	}
	return ghRepoPath(info), true
}

// ghWriteAuth: for a GitHub connection, the write-checked auth (writing the
// error itself; ok=false then). isGH=false leaves the request to the GitLab path.
func (h *GitRemoteHandler) ghWriteAuth(c *gin.Context) (userID string, conn *models.GitConnection, token string, isGH, ok bool) {
	cn, _, err := h.conns.loadAccessible(c.Param("id"), c.GetString("userID"))
	if err != nil || !isGitHub(cn) {
		return "", nil, "", false, false
	}
	userID, conn, token, ok = h.authWrite(c)
	return userID, conn, token, true, ok
}
