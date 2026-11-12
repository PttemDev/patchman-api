package handlers

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// GitLab CI/CD over the Git connection: pipelines and their jobs, job logs,
// pipeline schedules (cron), CI/CD variables — and, with a write grant and a
// token with the api scope, running / retrying / cancelling pipelines, playing
// manual jobs and schedules. Reads need read_api; GitLab adds its own roles
// on top (variables: Maintainer).
//
// CI/CD variables are secrets more often than not, so:
//   - listing them needs a WRITE grant on the connection (a read-only grantee
//     can't read the owner's project secrets through a shared token);
//   - the list never carries values, except — on request (values=unmasked,
//     what the MCP tool's includeValues sends) — those of variables that are
//     neither masked nor hidden;
//   - a single value (masked ones too) is revealed one at a time
//     (ci_variable), and that is audited.

// ciError writes a GitLab failure as a readable 502 (never 401/403, which the
// FE treats as its own session failing).
func ciError(c *gin.Context, status int, body []byte, action string) {
	c.JSON(http.StatusBadGateway, gin.H{"error": ciErrorMessage(status, body, action)})
}

// ciErrorMessage is the readable text for a GitLab failure.
func ciErrorMessage(status int, body []byte, action string) string {
	text := strings.TrimSpace(string(body))
	var msg string
	switch {
	case status == http.StatusUnauthorized:
		msg = "GitLab didn't accept the token (expired or revoked)."
	case status == http.StatusForbidden && strings.Contains(text, "insufficient_scope"):
		msg = "This token can only read (read_api). " + action + " needs a token with the api scope."
	case status == http.StatusForbidden:
		msg = "Your GitLab role on this project doesn't allow this (" + action + ")."
	case status == http.StatusNotFound:
		msg = "Not found on GitLab — or your role there can't see it."
	default:
		msg = fmt.Sprintf("gitlab %d: %s", status, clipText(text, 300))
	}
	return msg
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ciGET is getJSON with ciError's messages.
func (h *GitRemoteHandler) ciGET(c *gin.Context, token, path string, q url.Values, out any, action string) (http.Header, bool) {
	body, hdr, status, err := h.gitlabGET(c, token, path, q)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return nil, false
	}
	if status/100 != 2 {
		ciError(c, status, body, action)
		return nil, false
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "decode gitlab response: " + err.Error()})
			return nil, false
		}
	}
	return hdr, true
}

func requirePositive(c *gin.Context, name string) (string, bool) {
	raw := strings.TrimSpace(c.Query(name))
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": name + " must be a positive integer"})
		return "", false
	}
	return strconv.FormatInt(n, 10), true
}

type glUser struct {
	Name     string `json:"name"`
	Username string `json:"username"`
}

type glPipeline struct {
	ID             int64    `json:"id"`
	IID            int64    `json:"iid"`
	ProjectID      int64    `json:"project_id"`
	Status         string   `json:"status"`
	Source         string   `json:"source"`
	Ref            string   `json:"ref"`
	SHA            string   `json:"sha"`
	Tag            bool     `json:"tag"`
	WebURL         string   `json:"web_url"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
	StartedAt      string   `json:"started_at"`
	FinishedAt     string   `json:"finished_at"`
	Duration       *float64 `json:"duration"`
	QueuedDuration *float64 `json:"queued_duration"`
	User           *glUser  `json:"user"`
	YAMLErrors     string   `json:"yaml_errors"`
}

func (p glPipeline) dto() gin.H {
	out := gin.H{
		"id": p.ID, "iid": p.IID, "status": p.Status, "source": p.Source, "ref": p.Ref, "sha": p.SHA,
		"tag": p.Tag, "webUrl": p.WebURL, "createdAt": p.CreatedAt, "updatedAt": p.UpdatedAt,
	}
	if p.StartedAt != "" {
		out["startedAt"] = p.StartedAt
	}
	if p.FinishedAt != "" {
		out["finishedAt"] = p.FinishedAt
	}
	if p.Duration != nil {
		out["duration"] = *p.Duration
	}
	if p.User != nil {
		out["user"] = p.User.Name
	}
	if p.YAMLErrors != "" {
		out["yamlErrors"] = p.YAMLErrors
	}
	return out
}

// Pipelines lists a project's pipelines, newest first.
// GET .../pipelines?project=&ref=&status=&source=&page=
func (h *GitRemoteHandler) Pipelines(c *gin.Context) {
	userID, conn, token, ok := h.auth(c)
	if !ok {
		return
	}
	project, ok := requireProject(c)
	if !ok {
		return
	}
	q := url.Values{}
	q.Set("per_page", "30")
	q.Set("order_by", "id")
	q.Set("sort", "desc")
	for _, k := range []string{"ref", "status", "source", "page"} {
		if v := strings.TrimSpace(c.Query(k)); v != "" {
			q.Set(k, v)
		}
	}
	var raw []glPipeline
	hdr, ok := h.ciGET(c, token, projectPath(project)+"/pipelines", q, &raw, "Listing pipelines")
	if !ok {
		return
	}
	out := make([]gin.H, 0, len(raw))
	for _, p := range raw {
		out = append(out, p.dto())
	}
	h.conns.audit(userID, conn.ID, "pipelines", true, gin.H{"project": project})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"pipelines": out, "nextPage": headerInt(hdr, "X-Next-Page")}})
}

type glJob struct {
	ID            int64    `json:"id"`
	Name          string   `json:"name"`
	Stage         string   `json:"stage"`
	Status        string   `json:"status"`
	AllowFailure  bool     `json:"allow_failure"`
	FailureReason string   `json:"failure_reason"`
	CreatedAt     string   `json:"created_at"`
	StartedAt     string   `json:"started_at"`
	FinishedAt    string   `json:"finished_at"`
	Duration      *float64 `json:"duration"`
	WebURL        string   `json:"web_url"`
	User          *glUser  `json:"user"`
	Runner        *struct {
		Description string `json:"description"`
	} `json:"runner"`
}

// Pipeline returns one pipeline with its jobs (stages in order).
// GET .../pipeline?project=&pipeline=
func (h *GitRemoteHandler) Pipeline(c *gin.Context) {
	userID, conn, token, ok := h.auth(c)
	if !ok {
		return
	}
	project, ok := requireProject(c)
	if !ok {
		return
	}
	pid, ok := requirePositive(c, "pipeline")
	if !ok {
		return
	}
	var p glPipeline
	if _, ok := h.ciGET(c, token, projectPath(project)+"/pipelines/"+pid, nil, &p, "Reading the pipeline"); !ok {
		return
	}
	var entries []ciEntry
	q := url.Values{}
	q.Set("per_page", "100")
	for page := 1; page <= 5; page++ {
		q.Set("page", strconv.Itoa(page))
		var raw []glJob
		hdr, ok := h.ciGET(c, token, projectPath(project)+"/pipelines/"+pid+"/jobs", q, &raw, "Listing the pipeline's jobs")
		if !ok {
			return
		}
		for _, j := range raw {
			job := gin.H{"id": j.ID, "kind": "job", "name": j.Name, "stage": j.Stage, "status": j.Status, "allowFailure": j.AllowFailure,
				"createdAt": j.CreatedAt, "webUrl": j.WebURL}
			if j.FailureReason != "" {
				job["failureReason"] = j.FailureReason
			}
			if j.StartedAt != "" {
				job["startedAt"] = j.StartedAt
			}
			if j.FinishedAt != "" {
				job["finishedAt"] = j.FinishedAt
			}
			if j.Duration != nil {
				job["duration"] = *j.Duration
			}
			if j.Runner != nil && j.Runner.Description != "" {
				job["runner"] = j.Runner.Description
			}
			entries = append(entries, ciEntry{id: j.ID, stage: j.Stage, name: j.Name, dto: job})
		}
		if headerInt(hdr, "X-Next-Page") == 0 {
			break
		}
	}
	// Trigger jobs (`trigger:` — child / multi-project pipelines) are not in
	// /jobs; GitLab lists them as bridges. A failure here keeps the jobs and
	// says why instead of failing the whole pipeline view.
	bridges, bridgesErr := h.pipelineBridges(c, token, project, pid, p)
	entries = append(entries, bridges...)
	stages, jobs := orderCIEntries(entries)
	data := gin.H{"pipeline": p.dto(), "stages": stages, "jobs": jobs}
	if bridgesErr != "" {
		data["bridgesError"] = bridgesErr
	}
	h.conns.audit(userID, conn.ID, "pipeline", true, gin.H{"project": project, "pipeline": pid})
	c.JSON(http.StatusOK, gin.H{"data": data})
}

// ciEntry is a job or trigger job on its way into the pipeline response.
type ciEntry struct {
	id    int64
	stage string
	name  string
	dto   gin.H
}

// orderCIEntries puts stages in pipeline order and jobs by name within a
// stage, as GitLab's pipeline graph does. Jobs and trigger jobs share one id
// sequence, created stage by stage, so a stage's smallest id gives its place.
func orderCIEntries(entries []ciEntry) ([]string, []gin.H) {
	first := map[string]int64{}
	for _, e := range entries {
		if v, ok := first[e.stage]; !ok || e.id < v {
			first[e.stage] = e.id
		}
	}
	stages := make([]string, 0, len(first))
	for s := range first {
		stages = append(stages, s)
	}
	sort.Slice(stages, func(i, j int) bool { return first[stages[i]] < first[stages[j]] })
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.stage != b.stage {
			return first[a.stage] < first[b.stage]
		}
		if a.name != b.name {
			return a.name < b.name
		}
		return a.id < b.id
	})
	jobs := make([]gin.H, 0, len(entries))
	for _, e := range entries {
		jobs = append(jobs, e.dto)
	}
	return stages, jobs
}

type glBridge struct {
	ID            int64    `json:"id"`
	Name          string   `json:"name"`
	Stage         string   `json:"stage"`
	Status        string   `json:"status"`
	AllowFailure  bool     `json:"allow_failure"`
	FailureReason string   `json:"failure_reason"`
	CreatedAt     string   `json:"created_at"`
	StartedAt     string   `json:"started_at"`
	FinishedAt    string   `json:"finished_at"`
	Duration      *float64 `json:"duration"`
	WebURL        string   `json:"web_url"`
	Downstream    *struct {
		ID        int64  `json:"id"`
		ProjectID int64  `json:"project_id"`
		Ref       string `json:"ref"`
		Status    string `json:"status"`
		WebURL    string `json:"web_url"`
	} `json:"downstream_pipeline"`
}

// pipelineBridges lists a pipeline's trigger jobs with their downstream
// pipelines. errMsg is set (and the list possibly partial) when GitLab
// refused.
func (h *GitRemoteHandler) pipelineBridges(c *gin.Context, token, project, pid string, parent glPipeline) (out []ciEntry, errMsg string) {
	q := url.Values{}
	q.Set("per_page", "100")
	for page := 1; page <= 5; page++ {
		q.Set("page", strconv.Itoa(page))
		body, hdr, status, err := h.gitlabGET(c, token, projectPath(project)+"/pipelines/"+pid+"/bridges", q)
		if err != nil {
			return out, err.Error()
		}
		if status/100 != 2 {
			return out, ciErrorMessage(status, body, "Listing the trigger jobs")
		}
		var raw []glBridge
		if err := json.Unmarshal(body, &raw); err != nil {
			return out, "decode gitlab response: " + err.Error()
		}
		for _, b := range raw {
			job := gin.H{"id": b.ID, "kind": "bridge", "name": b.Name, "stage": b.Stage, "status": b.Status, "allowFailure": b.AllowFailure,
				"createdAt": b.CreatedAt, "webUrl": b.WebURL}
			if b.FailureReason != "" {
				job["failureReason"] = b.FailureReason
			}
			if b.StartedAt != "" {
				job["startedAt"] = b.StartedAt
			}
			if b.FinishedAt != "" {
				job["finishedAt"] = b.FinishedAt
			}
			if b.Duration != nil {
				job["duration"] = *b.Duration
			}
			if d := b.Downstream; d != nil && d.ID > 0 {
				job["downstream"] = gin.H{"id": d.ID, "status": d.Status, "ref": d.Ref, "webUrl": d.WebURL,
					"sameProject": sameProjectPipeline(parent.ProjectID, d.ProjectID, parent.WebURL, d.WebURL)}
			}
			out = append(out, ciEntry{id: b.ID, stage: b.Stage, name: b.Name, dto: job})
		}
		if headerInt(hdr, "X-Next-Page") == 0 {
			break
		}
	}
	return out, ""
}

// sameProjectPipeline says whether a downstream pipeline lives in the parent's
// project (a child pipeline, openable in the panel) rather than another one
// (multi-project, opened in GitLab). Project ids decide when GitLab sends
// both; otherwise the web URLs' project part (before "/-/pipelines/") does.
func sameProjectPipeline(parentID, childID int64, parentURL, childURL string) bool {
	if parentID > 0 && childID > 0 {
		return parentID == childID
	}
	pp, _, okP := strings.Cut(parentURL, "/-/pipelines/")
	cp, _, okC := strings.Cut(childURL, "/-/pipelines/")
	return okP && okC && pp == cp
}

var (
	ansiRE    = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")
	sectionRE = regexp.MustCompile(`section_(start|end):\d+:[A-Za-z0-9_.\-]+(\[[^\]]*\])?`)
)

// cleanLogLine drops ANSI colours, GitLab's collapsible-section markers and
// everything a carriage return overwrote (progress bars).
func cleanLogLine(line string) string {
	line = ansiRE.ReplaceAllString(line, "")
	line = sectionRE.ReplaceAllString(line, "")
	line = strings.TrimRight(line, "\r")
	if i := strings.LastIndexByte(line, '\r'); i >= 0 {
		line = line[i+1:]
	}
	return line
}

// tailLog keeps the last n cleaned lines of a job log, reading at most
// limit bytes; truncated says lines were dropped.
func tailLog(r io.Reader, n int, limit int64) (lines []string, total int, truncated bool) {
	sc := bufio.NewScanner(io.LimitReader(r, limit))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	ring := make([]string, 0, n)
	for sc.Scan() {
		total++
		l := cleanLogLine(sc.Text())
		if len(ring) < n {
			ring = append(ring, l)
		} else {
			copy(ring, ring[1:])
			ring[n-1] = l
		}
	}
	return ring, total, total > len(ring)
}

// JobLog returns the end of a job's log as plain text (colours stripped).
// GET .../job_log?project=&job=&lines=300
func (h *GitRemoteHandler) JobLog(c *gin.Context) {
	userID, conn, token, ok := h.auth(c)
	if !ok {
		return
	}
	project, ok := requireProject(c)
	if !ok {
		return
	}
	job, ok := requirePositive(c, "job")
	if !ok {
		return
	}
	n, _ := strconv.Atoi(c.DefaultQuery("lines", "300"))
	if n < 20 {
		n = 20
	}
	if n > 3000 {
		n = 3000
	}
	var meta glJob
	if _, ok := h.ciGET(c, token, projectPath(project)+"/jobs/"+job, nil, &meta, "Reading the job"); !ok {
		return
	}
	req, err := http.NewRequest(http.MethodGet, h.apiBase(c)+projectPath(project)+"/jobs/"+job+"/trace", nil)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.http.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		ciError(c, resp.StatusCode, b, "Reading the job log")
		return
	}
	lines, total, truncated := tailLog(resp.Body, n, 64<<20)
	h.conns.audit(userID, conn.ID, "job_log", true, gin.H{"project": project, "job": job})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"job":  gin.H{"id": meta.ID, "name": meta.Name, "stage": meta.Stage, "status": meta.Status, "failureReason": meta.FailureReason, "webUrl": meta.WebURL},
		"text": strings.Join(lines, "\n"), "lines": len(lines), "totalLines": total, "truncated": truncated,
	}})
}

// PipelineSchedules lists a project's scheduled pipelines (cron).
// GET .../pipeline_schedules?project=
func (h *GitRemoteHandler) PipelineSchedules(c *gin.Context) {
	userID, conn, token, ok := h.auth(c)
	if !ok {
		return
	}
	project, ok := requireProject(c)
	if !ok {
		return
	}
	q := url.Values{}
	q.Set("per_page", "100")
	var raw []struct {
		ID           int64   `json:"id"`
		Description  string  `json:"description"`
		Ref          string  `json:"ref"`
		Cron         string  `json:"cron"`
		CronTimezone string  `json:"cron_timezone"`
		NextRunAt    string  `json:"next_run_at"`
		Active       bool    `json:"active"`
		CreatedAt    string  `json:"created_at"`
		UpdatedAt    string  `json:"updated_at"`
		Owner        *glUser `json:"owner"`
	}
	if _, ok := h.ciGET(c, token, projectPath(project)+"/pipeline_schedules", q, &raw, "Listing pipeline schedules"); !ok {
		return
	}
	out := make([]gin.H, 0, len(raw))
	for _, s := range raw {
		row := gin.H{"id": s.ID, "description": s.Description, "ref": strings.TrimPrefix(s.Ref, "refs/heads/"), "cron": s.Cron,
			"timezone": s.CronTimezone, "nextRunAt": s.NextRunAt, "active": s.Active, "updatedAt": s.UpdatedAt}
		if s.Owner != nil {
			row["owner"] = s.Owner.Name
		}
		out = append(out, row)
	}
	h.conns.audit(userID, conn.ID, "pipeline_schedules", true, gin.H{"project": project})
	c.JSON(http.StatusOK, gin.H{"data": out})
}

type glVariable struct {
	Key              string `json:"key"`
	Value            string `json:"value"`
	VariableType     string `json:"variable_type"`
	Protected        bool   `json:"protected"`
	Masked           bool   `json:"masked"`
	Hidden           bool   `json:"hidden"`
	Raw              bool   `json:"raw"`
	EnvironmentScope string `json:"environment_scope"`
	Description      string `json:"description"`
}

// dto never carries the value unless asked for an unmasked, unhidden one.
func (v glVariable) dto(withUnmasked bool) gin.H {
	out := gin.H{"key": v.Key, "type": v.VariableType, "protected": v.Protected, "masked": v.Masked, "hidden": v.Hidden,
		"raw": v.Raw, "environmentScope": v.EnvironmentScope}
	if v.Description != "" {
		out["description"] = v.Description
	}
	if withUnmasked && !v.Masked && !v.Hidden {
		out["value"] = v.Value
	}
	return out
}

// listVariables pages through a variables endpoint (100 per page, ≤ 10 pages).
func (h *GitRemoteHandler) listVariables(c *gin.Context, token, path string) ([]glVariable, int, []byte, error) {
	var all []glVariable
	q := url.Values{}
	q.Set("per_page", "100")
	for page := 1; page <= 10; page++ {
		q.Set("page", strconv.Itoa(page))
		body, hdr, status, err := h.gitlabGET(c, token, path, q)
		if err != nil {
			return nil, 0, nil, err
		}
		if status/100 != 2 {
			return nil, status, body, nil
		}
		var raw []glVariable
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, 0, nil, err
		}
		all = append(all, raw...)
		if headerInt(hdr, "X-Next-Page") == 0 {
			break
		}
	}
	return all, http.StatusOK, nil, nil
}

// variablesBlock is one level's variables, or why they couldn't be read.
func variablesBlock(level, path string, vars []glVariable, status int, body []byte, withUnmasked bool) gin.H {
	out := gin.H{"level": level, "path": path}
	if status != http.StatusOK {
		switch {
		case status == http.StatusForbidden && strings.Contains(string(body), "insufficient_scope"):
			out["error"] = "This token can't read CI/CD variables (it needs read_api or api)."
		case status == http.StatusForbidden || status == http.StatusNotFound || status == http.StatusUnauthorized:
			if level == "group" {
				out["error"] = "Reading this group's CI/CD variables needs the Maintainer or Owner role on the group (a GitLab rule)."
			} else {
				out["error"] = "Reading CI/CD variables needs the Maintainer role on this project (a GitLab rule)."
			}
		default:
			out["error"] = fmt.Sprintf("gitlab %d: %s", status, clipText(strings.TrimSpace(string(body)), 200))
		}
		return out
	}
	list := make([]gin.H, 0, len(vars))
	for _, v := range vars {
		list = append(list, v.dto(withUnmasked))
	}
	out["variables"] = list
	return out
}

// CIVariables lists the project's CI/CD variables and those it inherits from
// its groups (outermost group first). Needs a write grant on the connection.
// GET .../ci_variables?project=[&values=unmasked]
func (h *GitRemoteHandler) CIVariables(c *gin.Context) {
	userID, conn, token, ok := h.authWrite(c)
	if !ok {
		return
	}
	project, ok := requireProject(c)
	if !ok {
		return
	}
	withUnmasked := c.Query("values") == "unmasked"
	var proj struct {
		PathWithNamespace string `json:"path_with_namespace"`
		Namespace         struct {
			Kind     string `json:"kind"`
			FullPath string `json:"full_path"`
		} `json:"namespace"`
	}
	if _, ok := h.ciGET(c, token, projectPath(project), nil, &proj, "Reading the project"); !ok {
		return
	}
	blocks := []gin.H{}
	if proj.Namespace.Kind == "group" && proj.Namespace.FullPath != "" {
		parts := strings.Split(proj.Namespace.FullPath, "/")
		for i := range parts {
			gp := strings.Join(parts[:i+1], "/")
			vars, status, body, err := h.listVariables(c, token, "/groups/"+url.PathEscape(gp)+"/variables")
			if err != nil {
				c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
				return
			}
			blocks = append(blocks, variablesBlock("group", gp, vars, status, body, withUnmasked))
		}
	}
	vars, status, body, err := h.listVariables(c, token, projectPath(project)+"/variables")
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	blocks = append(blocks, variablesBlock("project", proj.PathWithNamespace, vars, status, body, withUnmasked))
	h.conns.audit(userID, conn.ID, "ci_variables", true, gin.H{"project": project, "values": withUnmasked})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"levels": blocks, "valuesIncluded": withUnmasked}})
}

// CIVariable reveals one variable's value (masked ones too — GitLab shows them
// to Maintainers; hidden ones it never returns). Audited.
// GET .../ci_variable?project=&key=&scope=&group=
func (h *GitRemoteHandler) CIVariable(c *gin.Context) {
	userID, conn, token, ok := h.authWrite(c)
	if !ok {
		return
	}
	project, ok := requireProject(c)
	if !ok {
		return
	}
	key := strings.TrimSpace(c.Query("key"))
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key is required"})
		return
	}
	path := projectPath(project) + "/variables/" + url.PathEscape(key)
	if g := strings.TrimSpace(c.Query("group")); g != "" {
		path = "/groups/" + url.PathEscape(g) + "/variables/" + url.PathEscape(key)
	}
	q := url.Values{}
	if s := c.Query("scope"); s != "" {
		q.Set("filter[environment_scope]", s)
	}
	var v glVariable
	if _, ok := h.ciGET(c, token, path, q, &v, "Reading a CI/CD variable"); !ok {
		return
	}
	h.conns.audit(userID, conn.ID, "ci_variable_reveal", true, gin.H{"project": project, "group": c.Query("group"), "key": key, "scope": c.Query("scope")})
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"key": v.Key, "value": v.Value, "hidden": v.Hidden}})
}

// ── writes (write grant + a token with the api scope) ──────────────────

func (h *GitRemoteHandler) ciWrite(c *gin.Context, auditAction, action, path string, body any, ids gin.H) {
	userID, conn, token, ok := h.authWrite(c)
	if !ok {
		return
	}
	resp, status, err := h.gitlabDo(c, token, http.MethodPost, path, body)
	if err != nil {
		h.conns.audit(userID, conn.ID, auditAction, false, ids)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	if status/100 != 2 {
		ids["status"] = status
		h.conns.audit(userID, conn.ID, auditAction, false, ids)
		ciError(c, status, resp, action)
		return
	}
	h.conns.audit(userID, conn.ID, auditAction, true, ids)
	var out struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
		WebURL string `json:"web_url"`
	}
	_ = json.Unmarshal(resp, &out)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true, "id": out.ID, "status": out.Status, "webUrl": out.WebURL}})
}

// RetryPipeline retries a pipeline's failed jobs. POST .../pipeline_retry?project=&pipeline=
func (h *GitRemoteHandler) RetryPipeline(c *gin.Context) {
	project, ok := requireProject(c)
	if !ok {
		return
	}
	pid, ok := requirePositive(c, "pipeline")
	if !ok {
		return
	}
	h.ciWrite(c, "pipeline_retry", "Retrying a pipeline", projectPath(project)+"/pipelines/"+pid+"/retry", nil, gin.H{"project": project, "pipeline": pid})
}

// CancelPipeline cancels a running pipeline. POST .../pipeline_cancel?project=&pipeline=
func (h *GitRemoteHandler) CancelPipeline(c *gin.Context) {
	project, ok := requireProject(c)
	if !ok {
		return
	}
	pid, ok := requirePositive(c, "pipeline")
	if !ok {
		return
	}
	h.ciWrite(c, "pipeline_cancel", "Cancelling a pipeline", projectPath(project)+"/pipelines/"+pid+"/cancel", nil, gin.H{"project": project, "pipeline": pid})
}

// RunPipeline starts a pipeline on a ref, optionally with variables.
// POST .../pipeline_run?project=  body {ref, variables: [{key, value}]}
func (h *GitRemoteHandler) RunPipeline(c *gin.Context) {
	project, ok := requireProject(c)
	if !ok {
		return
	}
	var in struct {
		Ref       string `json:"ref"`
		Variables []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"variables"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || strings.TrimSpace(in.Ref) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ref is required"})
		return
	}
	body := gin.H{"ref": strings.TrimSpace(in.Ref)}
	if len(in.Variables) > 0 {
		vars := make([]gin.H, 0, len(in.Variables))
		for _, v := range in.Variables {
			if k := strings.TrimSpace(v.Key); k != "" {
				vars = append(vars, gin.H{"key": k, "value": v.Value, "variable_type": "env_var"})
			}
		}
		body["variables"] = vars
	}
	h.ciWrite(c, "pipeline_run", "Running a pipeline", projectPath(project)+"/pipeline", body, gin.H{"project": project, "ref": in.Ref})
}

// PlayJob starts a manual job. POST .../job_play?project=&job=
func (h *GitRemoteHandler) PlayJob(c *gin.Context) {
	project, ok := requireProject(c)
	if !ok {
		return
	}
	job, ok := requirePositive(c, "job")
	if !ok {
		return
	}
	h.ciWrite(c, "job_play", "Running a manual job", projectPath(project)+"/jobs/"+job+"/play", nil, gin.H{"project": project, "job": job})
}

// RetryJob retries one job. POST .../job_retry?project=&job=
func (h *GitRemoteHandler) RetryJob(c *gin.Context) {
	project, ok := requireProject(c)
	if !ok {
		return
	}
	job, ok := requirePositive(c, "job")
	if !ok {
		return
	}
	h.ciWrite(c, "job_retry", "Retrying a job", projectPath(project)+"/jobs/"+job+"/retry", nil, gin.H{"project": project, "job": job})
}

// PlaySchedule runs a scheduled pipeline now. POST .../schedule_play?project=&schedule=
func (h *GitRemoteHandler) PlaySchedule(c *gin.Context) {
	project, ok := requireProject(c)
	if !ok {
		return
	}
	sid, ok := requirePositive(c, "schedule")
	if !ok {
		return
	}
	h.ciWrite(c, "schedule_play", "Running a scheduled pipeline", projectPath(project)+"/pipeline_schedules/"+sid+"/play", nil, gin.H{"project": project, "schedule": sid})
}
