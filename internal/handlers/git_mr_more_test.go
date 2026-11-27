package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// A fake GitLab for the MR-page parity routes (detail, note edit/delete,
// commits, auto-merge cancel, awards, pickers). Paths and fields follow the
// GitLab REST docs; not checked against a live instance (doğrulanmadı).
func fakeMRParityGitLab(t *testing.T) (*httptest.Server, func() []mrFakeCall) {
	var mu sync.Mutex
	var calls []mrFakeCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		mu.Lock()
		calls = append(calls, mrFakeCall{r.Method, r.URL.Path, r.URL.RawQuery, body})
		mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, "/api/v4/projects/7")
		switch {
		case r.URL.Path == "/api/v4/user":
			_, _ = w.Write([]byte(`{"username":"ada"}`))
		case p == "/merge_requests/4" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"iid":4,"title":"Add greet","state":"opened","source_branch":"feat","target_branch":"main",
"source_project_id":7,"target_project_id":7,"author":{"name":"Ada","username":"ada"},
"assignees":[{"id":5,"name":"Bo","username":"bo"}],"reviewers":[],"labels":["bug","ui"],
"milestone":{"id":9,"title":"v1"},"merge_when_pipeline_succeeds":true,"squash":true,"force_remove_source_branch":true,
"references":{"full":"grp/app!4"},"user":{"can_merge":true},"changes_count":"3","user_notes_count":2,
"diff_refs":{"base_sha":"b","head_sha":"h","start_sha":"s"}}`))
		case p == "/merge_requests/4/discussions":
			_, _ = w.Write([]byte(`[{"id":"d1","notes":[{"id":11,"body":"mine","author":{"name":"Ada","username":"ada"},"created_at":"t1","updated_at":"t2"},{"id":12,"body":"theirs","author":{"name":"Bo","username":"bo"},"created_at":"t1","updated_at":"t1"}]},{"id":"d2","notes":[{"id":13,"body":"added 1 commit","system":true,"author":{"name":"Ada","username":"ada"}}]}]`))
		case p == "/merge_requests/4/commits":
			_, _ = w.Write([]byte(`[{"id":"abc123","short_id":"abc","title":"Add greet","message":"Add greet\n","author_name":"Ada"}]`))
		case p == "/merge_requests/4/award_emoji" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[{"id":31,"name":"thumbsup","user":{"username":"ada","name":"Ada"}},{"id":32,"name":"thumbsup","user":{"username":"bo","name":"Bo"}},{"id":33,"name":"rocket","user":{"username":"bo","name":"Bo"}}]`))
		case p == "/labels":
			_, _ = w.Write([]byte(`[{"name":"bug","color":"#d9534f"}]`))
		case p == "/milestones":
			_, _ = w.Write([]byte(`[{"id":9,"title":"v1"}]`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	return srv, func() []mrFakeCall { mu.Lock(); defer mu.Unlock(); return append([]mrFakeCall(nil), calls...) }
}

func TestMRParityGitLab(t *testing.T) {
	gl, calls := fakeMRParityGitLab(t)
	defer gl.Close()
	h := mrRig(t, gl, "read")
	last := func() mrFakeCall { cs := calls(); return cs[len(cs)-1] }

	// Detail: people, labels, milestone, merge settings, reference, me.
	code, out := callMR(h.MergeRequest, "owner", "GET", "project=7&iid=4", nil)
	d := data(out)
	if code != 200 || d["me"] != "ada" || d["reference"] != "grp/app!4" || d["autoMerge"] != true || d["squash"] != true ||
		d["removeSourceBranch"] != true || d["canMerge"] != true || d["fork"] != false {
		t.Fatalf("detail: %d %v", code, out)
	}
	if !strings.Contains(toJSON(d["assignees"]), `"username":"bo"`) || toJSON(d["reviewers"]) != "[]" || toJSON(d["labels"]) != `["bug","ui"]` ||
		!strings.Contains(toJSON(d["milestone"]), `"title":"v1"`) {
		t.Fatalf("detail people: %v", d)
	}

	// Discussions: canEdit only on my notes; system notes only with ?system=1.
	_, out = callMR(h.MRDiscussions, "owner", "GET", "project=7&iid=4", nil)
	list, _ := out["data"].([]any)
	if len(list) != 1 {
		t.Fatalf("system note leaked: %v", out)
	}
	notes := list[0].(map[string]any)["notes"].([]any)
	if notes[0].(map[string]any)["canEdit"] != true || notes[0].(map[string]any)["edited"] != true || notes[1].(map[string]any)["canEdit"] != false {
		t.Fatalf("canEdit: %v", notes)
	}
	_, out = callMR(h.MRDiscussions, "owner", "GET", "project=7&iid=4&system=1", nil)
	if list, _ = out["data"].([]any); len(list) != 2 || list[1].(map[string]any)["system"] != true {
		t.Fatalf("system=1: %v", out)
	}

	// Edit + delete a note; the read grantee can do neither; ids are checked.
	if code, _ = callMR(h.UpdateMRNote, "owner", "POST", "project=7&iid=4&note=11&discussion=d1", map[string]any{"body": "fixed"}); code != 200 {
		t.Fatalf("note update: %d", code)
	}
	if c := last(); c.Method != "PUT" || !strings.HasSuffix(c.Path, "/merge_requests/4/notes/11") || c.Body["body"] != "fixed" {
		t.Fatalf("note update call: %+v", c)
	}
	if code, _ = callMR(h.DeleteMRNote, "owner", "POST", "project=7&iid=4&note=11", nil); code != 200 {
		t.Fatalf("note delete: %d", code)
	}
	if c := last(); c.Method != "DELETE" || !strings.HasSuffix(c.Path, "/notes/11") {
		t.Fatalf("note delete call: %+v", c)
	}
	if code, _ = callMR(h.UpdateMRNote, "u2", "POST", "project=7&iid=4&note=11", map[string]any{"body": "x"}); code != http.StatusForbidden {
		t.Fatalf("read grant edited a note: %d", code)
	}
	if code, _ = callMR(h.DeleteMRNote, "u2", "POST", "project=7&iid=4&note=11", nil); code != http.StatusForbidden {
		t.Fatalf("read grant deleted a note: %d", code)
	}
	if code, _ = callMR(h.UpdateMRNote, "owner", "POST", "project=7&iid=4&note=../x", map[string]any{"body": "x"}); code != 400 {
		t.Fatalf("bad note id: %d", code)
	}
	if code, _ = callMR(h.UpdateMRNote, "owner", "POST", "project=7&iid=4&note=11", map[string]any{"body": " "}); code != 400 {
		t.Fatalf("empty body: %d", code)
	}

	// Commits.
	code, out = callMR(h.MRCommits, "u2", "GET", "project=7&iid=4", nil)
	if cs, _ := out["data"].([]any); code != 200 || len(cs) != 1 || cs[0].(map[string]any)["sha"] != "abc123" {
		t.Fatalf("commits: %d %v", code, out)
	}

	// Pipelines: ?limit= reaches per_page (capped at 50).
	callMR(h.MRPipelines, "owner", "GET", "project=7&iid=4&limit=500", nil)
	if c := last(); !strings.Contains(c.Query, "per_page=50") {
		t.Fatalf("pipelines limit: %+v", c)
	}

	// Update: labels, milestone, merge settings pass through.
	callMR(h.UpdateMR, "owner", "POST", "project=7&iid=4", map[string]any{"labels": "bug,ui", "milestoneId": 0, "squash": false, "removeSourceBranch": true, "reviewerIds": []int{}})
	if b := last().Body; b["labels"] != "bug,ui" || b["milestone_id"] != float64(0) || b["squash"] != false || b["remove_source_branch"] != true || toJSON(b["reviewer_ids"]) != "[]" {
		t.Fatalf("update body: %v", b)
	}

	// Cancel auto-merge.
	if code, _ = callMR(h.CancelAutoMergeMR, "owner", "POST", "project=7&iid=4", nil); code != 200 || !strings.HasSuffix(last().Path, "/cancel_merge_when_pipeline_succeeds") {
		t.Fatalf("cancel auto-merge: %d %+v", code, last())
	}
	if code, _ = callMR(h.CancelAutoMergeMR, "u2", "POST", "project=7&iid=4", nil); code != http.StatusForbidden {
		t.Fatalf("read grant cancelled auto-merge: %d", code)
	}

	// Awards: grouped, "mine" from /user; remove deletes my award only.
	_, out = callMR(h.MRAwards, "owner", "GET", "project=7&iid=4", nil)
	aw, _ := out["data"].([]any)
	if len(aw) != 2 || aw[0].(map[string]any)["count"].(float64) != 2 || aw[0].(map[string]any)["mine"] != true || aw[1].(map[string]any)["mine"] != false {
		t.Fatalf("awards: %v", out)
	}
	if code, _ = callMR(h.ToggleMRAward, "owner", "POST", "project=7&iid=4", map[string]any{"name": "rocket", "on": true}); code != 200 || last().Body["name"] != "rocket" {
		t.Fatalf("award add: %d %+v", code, last())
	}
	if code, _ = callMR(h.ToggleMRAward, "owner", "POST", "project=7&iid=4", map[string]any{"name": "thumbsup", "on": false}); code != 200 ||
		last().Method != "DELETE" || !strings.HasSuffix(last().Path, "/award_emoji/31") {
		t.Fatalf("award remove: %d %+v", code, last())
	}
	if code, _ = callMR(h.ToggleMRAward, "owner", "POST", "project=7&iid=4", map[string]any{"name": "custom_emoji", "on": true}); code != 400 {
		t.Fatalf("unknown award: %d", code)
	}
	if code, _ = callMR(h.ToggleMRAward, "u2", "POST", "project=7&iid=4", map[string]any{"name": "rocket", "on": true}); code != http.StatusForbidden {
		t.Fatalf("read grant awarded: %d", code)
	}

	// Pickers.
	if _, out = callMR(h.ProjectLabels, "u2", "GET", "project=7", nil); !strings.Contains(toJSON(out), `"color":"#d9534f"`) {
		t.Fatalf("labels: %v", out)
	}
	if _, out = callMR(h.ProjectMilestones, "u2", "GET", "project=7", nil); !strings.Contains(toJSON(out), `"title":"v1"`) || !strings.Contains(last().Query, "state=active") {
		t.Fatalf("milestones: %v", out)
	}
	// A stranger sees nothing.
	if code, _ = callMR(h.MRCommits, "stranger", "GET", "project=7&iid=4", nil); code != http.StatusNotFound {
		t.Fatalf("stranger: %d", code)
	}
}

func TestMRParityGitHub(t *testing.T) {
	r := newGHPRRig(t)
	h := r.h
	last := func() ghCall { cs := r.calls(); return cs[len(cs)-1] }

	// Detail: reference, people, me.
	_, out := r.do("GET", "project=11&iid=1", nil, h.MergeRequest)
	d := data(out)
	if d["reference"] != "acme/api#1" || d["me"] != "octo" || d["autoMerge"] != false || !strings.Contains(toJSON(d["reviewers"]), `"username":"octo"`) || toJSON(d["labels"]) != "[]" {
		t.Fatalf("detail: %v", d)
	}

	// Discussions: canEdit = the token's user wrote it (issue comments) or
	// GitHub says viewerCanUpdate (thread comments).
	_, out = r.do("GET", "project=11&iid=1", nil, h.MRDiscussions)
	canEdit := map[float64]any{}
	edited := map[float64]any{}
	for _, d := range out["data"].([]any) {
		for _, n := range d.(map[string]any)["notes"].([]any) {
			nm := n.(map[string]any)
			canEdit[nm["id"].(float64)] = nm["canEdit"]
			edited[nm["id"].(float64)] = nm["edited"]
		}
	}
	if canEdit[91] != false || canEdit[92] != true || canEdit[55] != false || canEdit[57] != true || edited[57] != true || edited[92] != true {
		t.Fatalf("discussions canEdit %v edited %v", canEdit, edited)
	}

	// Edit: the discussion prefix picks the API.
	if code, _ := r.do("POST", "project=11&iid=1&note=91&discussion=ic-91", map[string]any{"body": "edited"}, h.UpdateMRNote); code != 200 ||
		last().Method != "PATCH" || last().Path != "/repos/acme/api/issues/comments/91" || last().Body["body"] != "edited" {
		t.Fatalf("issue comment edit: %d %+v", code, last())
	}
	if code, _ := r.do("POST", "project=11&iid=1&note=55&discussion=PRRT_1", map[string]any{"body": "e"}, h.UpdateMRNote); code != 200 || last().Path != "/repos/acme/api/pulls/comments/55" {
		t.Fatalf("thread comment edit: %d %+v", code, last())
	}
	if code, _ := r.do("POST", "project=11&iid=1&note=55&discussion=rc-55", nil, h.DeleteMRNote); code != 200 || last().Method != "DELETE" || last().Path != "/repos/acme/api/pulls/comments/55" {
		t.Fatalf("review comment delete: %d %+v", code, last())
	}
	if code, _ := r.do("POST", "project=11&iid=1&note=202&discussion=rv-202", map[string]any{"body": "x"}, h.UpdateMRNote); code != 400 {
		t.Fatalf("review summary must not be editable: %d", code)
	}
	if code, _ := r.do("POST", "project=11&iid=1&note=91", map[string]any{"body": "x"}, h.UpdateMRNote); code != 400 {
		t.Fatalf("GitHub needs the discussion: %d", code)
	}

	// Commits newest first.
	_, out = r.do("GET", "project=11&iid=1", nil, h.MRCommits)
	if cs, _ := out["data"].([]any); len(cs) != 2 || cs[0].(map[string]any)["title"] != "second" {
		t.Fatalf("commits: %v", out)
	}

	// Cancel auto-merge → GraphQL.
	if code, _ := r.do("POST", "project=11&iid=1", nil, h.CancelAutoMergeMR); code != 200 || !strings.Contains(last().Body["query"].(string), "disablePullRequestAutoMerge") {
		t.Fatalf("cancel auto-merge: %d %+v", code, last())
	}

	// Awards ↔ reactions.
	_, out = r.do("GET", "project=11&iid=1", nil, h.MRAwards)
	aw, _ := out["data"].([]any)
	if len(aw) != 2 || aw[0].(map[string]any)["name"] != "thumbsup" || aw[0].(map[string]any)["mine"] != true || aw[1].(map[string]any)["name"] != "heart" {
		t.Fatalf("reactions: %v", out)
	}
	if code, _ := r.do("POST", "project=11&iid=1", map[string]any{"name": "tada", "on": true}, h.ToggleMRAward); code != 200 || last().Body["content"] != "hooray" {
		t.Fatalf("reaction add: %d %+v", code, last())
	}
	if code, _ := r.do("POST", "project=11&iid=1", map[string]any{"name": "thumbsup", "on": false}, h.ToggleMRAward); code != 200 || last().Path != "/repos/acme/api/issues/1/reactions/70" {
		t.Fatalf("reaction remove: %d %+v", code, last())
	}

	// Update: labels PUT (replace), milestone PATCH, reviewers set (octo removed, ada added).
	if code, out := r.do("POST", "project=11&iid=1", map[string]any{"labels": "bug, ui", "milestoneId": 3, "reviewerIds": []int{7}}, h.UpdateMR); code != 200 {
		t.Fatalf("update: %d %v", code, out)
	}
	var sawDel, sawAdd, sawLabels, sawMs bool
	for _, c := range r.calls() {
		switch {
		case c.Method == "DELETE" && c.Path == "/repos/acme/api/pulls/1/requested_reviewers" && toJSON(c.Body["reviewers"]) == `["octo"]`:
			sawDel = true
		case c.Method == "POST" && c.Path == "/repos/acme/api/pulls/1/requested_reviewers" && toJSON(c.Body["reviewers"]) == `["ada"]`:
			sawAdd = true
		case c.Method == "PUT" && c.Path == "/repos/acme/api/issues/1/labels" && toJSON(c.Body["labels"]) == `["bug","ui"]`:
			sawLabels = true
		case c.Method == "PATCH" && c.Path == "/repos/acme/api/issues/1" && c.Body["milestone"] == float64(3):
			sawMs = true
		}
	}
	if !sawDel || !sawAdd || !sawLabels || !sawMs {
		t.Fatalf("update calls del=%v add=%v labels=%v ms=%v: %+v", sawDel, sawAdd, sawLabels, sawMs, r.calls())
	}

	// Merge method rebase; a rebase writes no commit message.
	if code, _ := r.do("POST", "project=11&iid=1", map[string]any{"mergeMethod": "rebase", "mergeCommitMessage": "x"}, h.MergeMR); code != 200 {
		t.Fatalf("rebase merge: %d", code)
	}
	var mb map[string]any
	for _, c := range r.calls() {
		if c.Path == "/repos/acme/api/pulls/1/merge" {
			mb = c.Body
		}
	}
	if mb["merge_method"] != "rebase" || mb["commit_title"] != nil {
		t.Fatalf("rebase body: %v", mb)
	}
	if code, _ := r.do("POST", "project=11&iid=1", map[string]any{"mergeMethod": "octopus"}, h.MergeMR); code != 400 {
		t.Fatalf("bad merge method: %d", code)
	}

	// Pickers.
	if _, out = r.do("GET", "project=11", nil, h.ProjectLabels); !strings.Contains(toJSON(out), `"color":"#d73a4a"`) {
		t.Fatalf("labels: %v", out)
	}
	if _, out = r.do("GET", "project=11", nil, h.ProjectMilestones); !strings.Contains(toJSON(out), `"id":3`) {
		t.Fatalf("milestones: %v", out)
	}
}
