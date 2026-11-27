// Package jiratest is a fake Jira (Cloud or Data Center) for tests. It
// answers only the calls PatchMan makes, checks the auth header shape of
// its flavor, and serves the rich-text shapes of that flavor: ADF documents
// on Cloud (v3), plain strings on Data Center (v2). The shapes follow
// Atlassian's docs as remembered (doğrulanmadı against a live site): a fake
// only proves what was assumed.
package jiratest

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
)

const (
	Email    = "ada@example.com"
	APIToken = "cloud-token-1"
	PAT      = "dc-pat-1"
	ACField  = "customfield_10042"
	Sprint   = "customfield_10020"
)

// Server is a running fake.
type Server struct {
	*httptest.Server
	Flavor string

	mu    sync.Mutex
	Paths []string // every path + query seen
}

// Seen returns a copy of the requests seen.
func (s *Server) Seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.Paths...)
}

// New starts a fake of the given flavor ("cloud" | "datacenter").
func New(flavor string) *Server {
	s := &Server{Flavor: flavor}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.Paths = append(s.Paths, r.URL.Path+"?"+r.URL.RawQuery)
	s.mu.Unlock()
	write := func(code int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
	cloud := s.Flavor == "cloud"
	want := "Bearer " + PAT
	prefix := "/rest/api/2"
	if cloud {
		want = "Basic " + base64.StdEncoding.EncodeToString([]byte(Email+":"+APIToken))
		prefix = "/rest/api/3"
	}
	if r.Header.Get("Authorization") != want {
		write(401, `{"errorMessages":["You are not authenticated."],"errors":{}}`)
		return
	}
	if r.Method != http.MethodGet {
		write(405, `{"errorMessages":["read-only fake"]}`)
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, prefix)
	if !ok {
		write(404, `{"errorMessages":["wrong API version"]}`)
		return
	}
	q := r.URL.Query()
	switch {
	case path == "/myself":
		if cloud {
			write(200, `{"accountId":"5b10ac8d82e05b22cc7d4ef5","displayName":"Ada Lovelace","emailAddress":"ada@example.com","active":true}`)
		} else {
			write(200, `{"name":"ada","key":"JIRAUSER10100","displayName":"Ada (DC)","emailAddress":"ada@example.com","active":true}`)
		}
	case path == "/serverInfo":
		if cloud {
			write(200, `{"baseUrl":"https://x.atlassian.net","version":"1001.0.0-SNAPSHOT","deploymentType":"Cloud","serverTitle":"Jira"}`)
		} else {
			write(200, `{"baseUrl":"https://jira.example.com","version":"9.12.4","deploymentType":"Server","serverTitle":"Jira"}`)
		}
	case path == "/field":
		write(200, `[
			{"id":"summary","name":"Summary","custom":false,"schema":{"type":"string","system":"summary"}},
			{"id":"`+ACField+`","name":"Acceptance Criteria","custom":true,"schema":{"type":"string","custom":"com.atlassian.jira.plugin.system.customfieldtypes:textarea","customId":10042}},
			{"id":"`+Sprint+`","name":"Sprint","custom":true,"schema":{"type":"array","items":"json","custom":"com.pyxis.greenhopper.jira:gh-sprint","customId":10020}}]`)
	case cloud && path == "/search/jql":
		s.cloudSearch(q, write)
	case !cloud && path == "/search":
		s.dcSearch(q, write)
	case strings.HasPrefix(path, "/issue/"):
		key := strings.TrimPrefix(path, "/issue/")
		if key != "MER-936" {
			write(404, `{"errorMessages":["Issue does not exist or you do not have permission to see it."],"errors":{}}`)
			return
		}
		write(200, issueBody(cloud, q.Get("fields")))
	default:
		write(404, `{"errorMessages":["not found"]}`)
	}
}

// Five issues MER-1 … MER-5, two per page.
func row(n int) string {
	return `{"id":"1000` + strconv.Itoa(n) + `","key":"MER-` + strconv.Itoa(n) + `","fields":{"summary":"Issue ` + strconv.Itoa(n) +
		`","status":{"name":"In Test"},"issuetype":{"name":"Story"},"priority":{"name":"High"},"assignee":{"displayName":"Ada Lovelace"},"updated":"2026-11-20T10:00:00.000+0000"}}`
}

func rows(from, to int) string {
	parts := []string{}
	for i := from; i <= to && i <= 5; i++ {
		parts = append(parts, row(i))
	}
	return strings.Join(parts, ",")
}

func (s *Server) cloudSearch(q map[string][]string, write func(int, string)) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	if strings.Contains(get("jql"), "((") {
		write(400, `{"errorMessages":["Error in the JQL Query: Expecting ')' before the end of the query."],"errors":{}}`)
		return
	}
	if get("fields") == "" {
		// Cloud /search/jql answers ids only without fields (doğrulanmadı).
		write(200, `{"issues":[{"id":"10001"}],"isLast":true}`)
		return
	}
	switch get("nextPageToken") {
	case "":
		write(200, `{"issues":[`+rows(1, 2)+`],"nextPageToken":"tok-p2","isLast":false}`)
	case "tok-p2":
		write(200, `{"issues":[`+rows(3, 4)+`],"nextPageToken":"tok-p3","isLast":false}`)
	case "tok-p3":
		write(200, `{"issues":[`+rows(5, 5)+`],"isLast":true}`)
	default:
		write(400, `{"errorMessages":["Invalid nextPageToken"]}`)
	}
}

func (s *Server) dcSearch(q map[string][]string, write func(int, string)) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	if strings.Contains(get("jql"), "((") {
		write(400, `{"errorMessages":["Error in the JQL Query: Expecting ')' before the end of the query."],"errors":{}}`)
		return
	}
	start, _ := strconv.Atoi(get("startAt"))
	max, _ := strconv.Atoi(get("maxResults"))
	if max <= 0 {
		max = 50
	}
	if max > 2 {
		max = 2
	}
	write(200, `{"startAt":`+strconv.Itoa(start)+`,"maxResults":`+strconv.Itoa(max)+`,"total":5,"issues":[`+rows(start+1, start+max)+`]}`)
}

// LongDescription is long enough to be truncated by compact MCP replies.
var LongDescription = strings.Repeat("The order list must page correctly. ", 200)

func issueBody(cloud bool, fields string) string {
	has := func(f string) bool {
		for _, x := range strings.Split(fields, ",") {
			if x == f {
				return true
			}
		}
		return false
	}
	var desc, ac, sprint string
	if cloud {
		desc = `{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"` + LongDescription + `"}]}]}`
		ac = `{"type":"doc","version":1,"content":[
			{"type":"heading","attrs":{"level":3},"content":[{"type":"text","text":"Given a merchant"}]},
			{"type":"bulletList","content":[
				{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"lists orders "},{"type":"text","text":"by date","marks":[{"type":"strong"}]}]}]},
				{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"sees 20 per page"}]}]}]}]}`
		sprint = `[{"id":37,"name":"Sprint 12","state":"active","boardId":1}]`
	} else {
		desc = `"` + LongDescription + `"`
		ac = `"h3. Given a merchant\n* lists orders *by date*\n* sees 20 per page"`
		sprint = `["com.atlassian.greenhopper.service.sprint.Sprint@6f2b[id=37,rapidViewId=1,state=ACTIVE,name=Sprint 12,startDate=2026-11-17T09:00:00.000Z]"]`
	}
	f := []string{
		`"summary":"Order list paging"`,
		`"status":{"name":"In Test","statusCategory":{"name":"In Progress"}}`,
		`"issuetype":{"name":"Story"}`,
		`"priority":{"name":"High"}`,
		`"assignee":{"displayName":"Ada Lovelace"}`,
		`"reporter":null`,
		`"labels":["orders","paging"]`,
		`"created":"2026-11-10T10:00:00.000+0000"`,
		`"updated":"2026-11-20T10:00:00.000+0000"`,
		`"description":` + desc,
		`"issuelinks":[{"type":{"name":"Blocks","inward":"is blocked by","outward":"blocks"},"outwardIssue":{"key":"MER-937","fields":{"summary":"Export orders","status":{"name":"To Do"}}}},
			{"type":{"name":"Relates","inward":"relates to","outward":"relates to"},"inwardIssue":{"key":"MER-12","fields":{"summary":"Order API","status":{"name":"Done"}}}}]`,
		`"subtasks":[{"key":"MER-940","fields":{"summary":"API tests","status":{"name":"Done"}}}]`,
	}
	if has(ACField) {
		f = append(f, `"`+ACField+`":`+ac)
	}
	if has(Sprint) {
		f = append(f, `"`+Sprint+`":`+sprint)
	}
	return `{"id":"10936","key":"MER-936","fields":{` + strings.Join(f, ",") + `}}`
}
