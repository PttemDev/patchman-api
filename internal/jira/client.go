// Package jira is a small read-only client for the Jira REST API, used by
// the Jira connection (CR_QA_P0_PLAN.md P0.b / O1). It knows two flavors:
//
//   - Cloud: base path /rest/api/3, Basic auth with email:apiToken. Search
//     uses the cursor endpoint GET /rest/api/3/search/jql (nextPageToken),
//     because the old /rest/api/3/search is deprecated on Cloud.
//   - Data Center: base path /rest/api/2, "Authorization: Bearer <PAT>".
//     Search uses GET /rest/api/2/search with startAt paging.
//
// Calls: Myself, ServerInfo, Fields, Search, Issue. Nothing writes.
//
// Upstream shapes here come from Atlassian's public docs as remembered, not
// from a live response; each one is marked "doğrulanmadı" until checked
// against a real Cloud and DC instance.
package jira

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	FlavorCloud      = "cloud"
	FlavorDataCenter = "datacenter"

	AuthEmailToken = "email_token"
	AuthPAT        = "pat"

	// maxBody caps one upstream answer; a single issue with every field or a
	// 100-issue search page stays well under it.
	maxBody = 16 << 20
	// MaxLimit caps a search page (Jira's own cap is 100 on both flavors —
	// doğrulanmadı for Cloud /search/jql).
	MaxLimit     = 100
	DefaultLimit = 25
	// maxText caps a rendered description / AC field in the API answer.
	maxText = 100_000
)

// Config is what a client needs: the base URL (no /rest path), the flavor,
// and the credential (email + API token for Cloud, PAT for Data Center).
type Config struct {
	BaseURL string
	Flavor  string
	Email   string
	Secret  string
	// HTTP overrides the client (tests); nil = a default with timeouts and
	// no redirects.
	HTTP *http.Client
}

type Client struct {
	base   string
	flavor string
	auth   string
	http   *http.Client
}

var defaultHTTP = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 25 * time.Second,
	},
	// A URL behind an SSO proxy that redirects to a login page must not turn
	// into a misleading 200, and a redirect must not carry the credential to
	// another host.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// New validates the config and builds a client.
func New(cfg Config) (*Client, error) {
	base, err := BaseURL(cfg.BaseURL)
	if err != nil {
		return nil, &APIError{Code: "bad_request", Message: err.Error()}
	}
	flavor := NormalizeFlavor(cfg.Flavor)
	secret := strings.TrimSpace(cfg.Secret)
	if secret == "" {
		return nil, &APIError{Code: "auth", Message: "The connection has no API token / personal access token."}
	}
	auth := ""
	if flavor == FlavorCloud {
		email := strings.TrimSpace(cfg.Email)
		if email == "" {
			return nil, &APIError{Code: "auth", Message: "Jira Cloud needs the account email."}
		}
		auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(email+":"+secret))
	} else {
		auth = "Bearer " + secret
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = defaultHTTP
	}
	return &Client{base: base, flavor: flavor, auth: auth, http: hc}, nil
}

// NormalizeFlavor maps anything but "datacenter" (and its spellings) to cloud.
func NormalizeFlavor(f string) string {
	switch strings.ToLower(strings.TrimSpace(f)) {
	case "datacenter", "data_center", "dc", "server":
		return FlavorDataCenter
	}
	return FlavorCloud
}

// AuthKindFor is the only auth kind each flavor takes.
func AuthKindFor(flavor string) string {
	if NormalizeFlavor(flavor) == FlavorDataCenter {
		return AuthPAT
	}
	return AuthEmailToken
}

func (c *Client) apiPath() string {
	if c.flavor == FlavorDataCenter {
		return "/rest/api/2"
	}
	return "/rest/api/3"
}

// Flavor reports the client's flavor.
func (c *Client) Flavor() string { return c.flavor }

// Base reports the normalized base URL.
func (c *Client) Base() string { return c.base }

// BaseURL normalizes the Jira URL: https by default, trailing slash and any
// pasted /rest, /browse, /jira/software… path cut off. A URL with a user or
// password in it is refused: Go's transport errors echo the URL, and the
// credential belongs in the credential fields.
func BaseURL(raw string) (string, error) {
	u := strings.TrimSpace(raw)
	if u == "" {
		return "", fmt.Errorf("Jira URL is required")
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "https://" + u
	}
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return "", fmt.Errorf("invalid Jira URL")
	}
	if p.User != nil {
		return "", fmt.Errorf("put the user and token in their own fields, not in the Jira URL")
	}
	// Data Center may live under a context path (https://example.com/jira),
	// so cut at the first known Jira segment wherever it sits.
	path := strings.TrimRight(p.Path, "/")
	cutAt := len(path)
	for _, seg := range []string{"/rest", "/browse", "/secure", "/projects", "/issues", "/jira/software", "/jira/core", "/jira/servicedesk", "/jira/your-work", "/plugins", "/login.jsp"} {
		if i := strings.Index(path+"/", seg+"/"); i >= 0 && i < cutAt {
			cutAt = i
		}
	}
	p.Path = path[:cutAt]
	p.RawQuery, p.Fragment, p.RawPath = "", "", ""
	return strings.TrimRight(p.String(), "/"), nil
}

var keyRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*-[0-9]+$`)

// ValidKey reports whether s looks like an issue key (ABC-123). Keys are
// validated before they go into a path, so a crafted key can't reach other
// REST resources with the stored credential.
func ValidKey(s string) bool { return keyRe.MatchString(s) }

var fieldIDRe = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,64}$`)

// ValidFieldID reports whether s looks like a Jira field id
// (customfield_10042, summary, …).
func ValidFieldID(s string) bool { return fieldIDRe.MatchString(s) }

// IssueWebURL is the browser link of an issue.
func (c *Client) IssueWebURL(key string) string { return c.base + "/browse/" + url.PathEscape(key) }

// ── errors ──────────────────────────────────────────────────────────

// APIError is a Jira failure with a readable message and a machine code:
// auth | forbidden | not_found | bad_query | bad_request | rate_limited |
// unreachable | upstream. It never carries the credential.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string { return e.Message }

// AsAPIError unwraps an *APIError.
func AsAPIError(err error) (*APIError, bool) {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// Jira error body: {"errorMessages":["…"],"errors":{"jql":"…"}} on both
// flavors (doğrulanmadı for /search/jql).
func newAPIError(status int, body []byte) *APIError {
	var b struct {
		ErrorMessages []string          `json:"errorMessages"`
		Errors        map[string]string `json:"errors"`
		Message       string            `json:"message"`
	}
	_ = json.Unmarshal(body, &b)
	parts := append([]string{}, b.ErrorMessages...)
	for k, v := range b.Errors {
		parts = append(parts, k+": "+v)
	}
	if b.Message != "" {
		parts = append(parts, b.Message)
	}
	reason := strings.TrimSpace(strings.Join(parts, "; "))
	e := &APIError{Status: status, Code: "upstream"}
	switch {
	case status == http.StatusUnauthorized:
		e.Code = "auth"
		e.Message = "Jira rejected the credentials (401): check the email and API token (Cloud) or the personal access token (Data Center)."
	case status == http.StatusForbidden:
		e.Code = "forbidden"
		e.Message = "This Jira user may not do that (403)" + suffix(reason)
	case status == http.StatusNotFound:
		e.Code = "not_found"
		e.Message = "Not found on Jira (404)" + suffix(reason)
	case status == http.StatusBadRequest:
		e.Code = "bad_query"
		e.Message = "Jira refused the request" + suffix(reason)
	case status == http.StatusTooManyRequests:
		e.Code = "rate_limited"
		e.Message = "Jira is rate limiting these calls (429); try again shortly."
	case status >= 300 && status < 400:
		e.Code = "auth"
		e.Message = "Jira answered with a redirect — the URL probably points at a login page or proxy, not the Jira site."
	default:
		e.Message = fmt.Sprintf("Jira answered %d", status) + suffix(reason)
	}
	return e
}

func suffix(reason string) string {
	if reason == "" {
		return "."
	}
	if len(reason) > 300 {
		reason = reason[:300] + "…"
	}
	return ": " + reason
}

// ── transport ───────────────────────────────────────────────────────

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	u := c.base + c.apiPath() + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return &APIError{Code: "bad_request", Message: "invalid Jira request"}
	}
	req.Header.Set("Authorization", c.auth)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return &APIError{Code: "unreachable", Message: "Jira did not answer in time."}
		}
		// The error text names the URL (base + path, never the credential:
		// userinfo is refused in BaseURL).
		return &APIError{Code: "unreachable", Message: "Jira is not reachable: " + err.Error()}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return &APIError{Code: "unreachable", Message: "reading Jira's answer failed"}
	}
	if len(raw) > maxBody {
		return &APIError{Code: "upstream", Message: "Jira's answer is too large; narrow the query."}
	}
	if resp.StatusCode/100 != 2 {
		return newAPIError(resp.StatusCode, raw)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		if trimmed := strings.TrimSpace(string(raw)); strings.HasPrefix(trimmed, "<") {
			return &APIError{Code: "upstream", Message: "Jira answered with an HTML page — check that the URL is the Jira site and the flavor (Cloud / Data Center) is right."}
		}
		return &APIError{Code: "upstream", Message: "Jira's answer could not be read"}
	}
	return nil
}

// ── calls ───────────────────────────────────────────────────────────

// User is the signed-in account. Cloud has accountId; DC has name / key
// (doğrulanmadı: DC 9.x+ still returns name).
type User struct {
	AccountID    string `json:"accountId,omitempty"`
	Name         string `json:"name,omitempty"`
	Key          string `json:"key,omitempty"`
	DisplayName  string `json:"displayName"`
	EmailAddress string `json:"emailAddress,omitempty"`
	Active       bool   `json:"active"`
}

// Myself returns the account the credential signs in as.
func (c *Client) Myself(ctx context.Context) (*User, error) {
	var u User
	if err := c.get(ctx, "/myself", nil, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// ServerInfo — deploymentType "Cloud" or "Server" / "DataCenter"
// (doğrulanmadı for DC 9.x).
type ServerInfo struct {
	Version        string `json:"version"`
	DeploymentType string `json:"deploymentType"`
	ServerTitle    string `json:"serverTitle"`
}

func (c *Client) ServerInfo(ctx context.Context) (*ServerInfo, error) {
	var s ServerInfo
	if err := c.get(ctx, "/serverInfo", nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Field is a Jira field (system or custom). SchemaType is schema.type
// ("string", "array", "option"…), SchemaCustom the plugin key for custom
// fields (e.g. com.atlassian.jira.plugin.system.customfieldtypes:textarea).
type Field struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Custom       bool   `json:"custom"`
	SchemaType   string `json:"schemaType,omitempty"`
	SchemaCustom string `json:"schemaCustom,omitempty"`
}

// SprintSchema is the custom field type of Jira Software's sprint field
// (doğrulanmadı on a live site, long-standing key).
const SprintSchema = "com.pyxis.greenhopper.jira:gh-sprint"

// Fields lists every field the user can see: GET /field (array on both
// flavors).
func (c *Client) Fields(ctx context.Context) ([]Field, error) {
	var raw []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Custom bool   `json:"custom"`
		Schema *struct {
			Type   string `json:"type"`
			Custom string `json:"custom"`
		} `json:"schema"`
	}
	if err := c.get(ctx, "/field", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Field, 0, len(raw))
	for _, f := range raw {
		fl := Field{ID: f.ID, Name: f.Name, Custom: f.Custom}
		if f.Schema != nil {
			fl.SchemaType, fl.SchemaCustom = f.Schema.Type, f.Schema.Custom
		}
		out = append(out, fl)
	}
	return out, nil
}

// SprintFieldID picks the sprint field from a field list ("" when absent).
func SprintFieldID(fields []Field) string {
	for _, f := range fields {
		if f.SchemaCustom == SprintSchema {
			return f.ID
		}
	}
	return ""
}

// SearchRequest is one page of a JQL search. Next is the cursor the
// previous page returned: Cloud's nextPageToken, or DC's next startAt.
type SearchRequest struct {
	JQL   string
	Limit int
	Next  string
}

// IssueRow is one search hit.
type IssueRow struct {
	Key     string `json:"key"`
	Summary string `json:"summary"`
	Status  string `json:"status"`
	// StatusCategory is Jira's fixed category key: new | indeterminate | done.
	StatusCategory string `json:"statusCategory,omitempty"`
	IssueType      string `json:"issueType"`
	Priority       string `json:"priority,omitempty"`
	Assignee       string `json:"assignee,omitempty"`
	Updated        string `json:"updated,omitempty"`
}

// SearchResult — Total is nil on Cloud (/search/jql returns no total;
// doğrulanmadı), Next is "" on the last page.
type SearchResult struct {
	Issues []IssueRow `json:"issues"`
	Total  *int       `json:"total"`
	Next   string     `json:"next"`
	IsLast bool       `json:"isLast"`
}

var searchFields = []string{"summary", "status", "issuetype", "priority", "assignee", "updated"}

type rawIssue struct {
	ID     string                     `json:"id"`
	Key    string                     `json:"key"`
	Fields map[string]json.RawMessage `json:"fields"`
}

// Search runs one page of a JQL search.
func (c *Client) Search(ctx context.Context, req SearchRequest) (*SearchResult, error) {
	jql := strings.TrimSpace(req.JQL)
	if jql == "" {
		return nil, &APIError{Code: "bad_request", Message: "JQL is required"}
	}
	limit := req.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	q := url.Values{}
	q.Set("jql", jql)
	q.Set("maxResults", strconv.Itoa(limit))
	// Cloud /search/jql returns only issue ids unless fields are named
	// (doğrulanmadı); both flavors get the same explicit list.
	q.Set("fields", strings.Join(searchFields, ","))
	var page struct {
		Issues        []rawIssue `json:"issues"`
		Total         *int       `json:"total"`
		StartAt       int        `json:"startAt"`
		NextPageToken string     `json:"nextPageToken"`
		IsLast        *bool      `json:"isLast"`
	}
	path := "/search/jql"
	startAt := 0
	if c.flavor == FlavorDataCenter {
		path = "/search"
		if n := strings.TrimSpace(req.Next); n != "" {
			v, err := strconv.Atoi(n)
			if err != nil || v < 0 {
				return nil, &APIError{Code: "bad_request", Message: "next must be the number a previous Data Center page returned"}
			}
			startAt = v
		}
		q.Set("startAt", strconv.Itoa(startAt))
	} else if n := strings.TrimSpace(req.Next); n != "" {
		q.Set("nextPageToken", n)
	}
	if err := c.get(ctx, path, q, &page); err != nil {
		return nil, err
	}
	out := &SearchResult{Issues: make([]IssueRow, 0, len(page.Issues))}
	for _, ri := range page.Issues {
		out.Issues = append(out.Issues, rowOf(ri))
	}
	if c.flavor == FlavorDataCenter {
		out.Total = page.Total
		end := startAt + len(page.Issues)
		if page.Total != nil && end < *page.Total && len(page.Issues) > 0 {
			out.Next = strconv.Itoa(end)
		}
	} else {
		// Cloud: no total; the cursor decides (doğrulanmadı: isLast may be
		// absent, then a missing token means the last page).
		out.Next = page.NextPageToken
		if page.IsLast != nil && *page.IsLast {
			out.Next = ""
		}
	}
	out.IsLast = out.Next == ""
	return out, nil
}

func rowOf(ri rawIssue) IssueRow {
	f := ri.Fields
	return IssueRow{
		Key:            ri.Key,
		Summary:        strField(f["summary"]),
		Status:         nameField(f["status"]),
		StatusCategory: statusCategory(f["status"]),
		IssueType:      nameField(f["issuetype"]),
		Priority:       nameField(f["priority"]),
		Assignee:       userField(f["assignee"]),
		Updated:        strField(f["updated"]),
	}
}

// Link is one issue link, seen from this issue ("blocks", "is blocked by").
type Link struct {
	Type    string `json:"type"`
	Key     string `json:"key"`
	Summary string `json:"summary,omitempty"`
	Status  string `json:"status,omitempty"`
}

// SubTask is a subtask of an issue.
type SubTask struct {
	Key     string `json:"key"`
	Summary string `json:"summary"`
	Status  string `json:"status"`
}

// Issue is a normalized issue: rich-text fields (v3 ADF documents or v2
// wiki strings) rendered as plain text.
type Issue struct {
	Key                         string    `json:"key"`
	ID                          string    `json:"id"`
	Summary                     string    `json:"summary"`
	Status                      string    `json:"status"`
	StatusCategory              string    `json:"statusCategory,omitempty"`
	IssueType                   string    `json:"issueType"`
	Priority                    string    `json:"priority,omitempty"`
	Assignee                    string    `json:"assignee,omitempty"`
	Reporter                    string    `json:"reporter,omitempty"`
	Labels                      []string  `json:"labels"`
	Sprints                     []string  `json:"sprints"`
	Created                     string    `json:"created,omitempty"`
	Updated                     string    `json:"updated,omitempty"`
	Description                 string    `json:"description"`
	DescriptionTruncated        bool      `json:"descriptionTruncated,omitempty"`
	AcceptanceFieldID           string    `json:"acceptanceFieldId,omitempty"`
	AcceptanceCriteria          *string   `json:"acceptanceCriteria"`
	AcceptanceCriteriaTruncated bool      `json:"acceptanceCriteriaTruncated,omitempty"`
	Links                       []Link    `json:"links"`
	Subtasks                    []SubTask `json:"subtasks"`
	WebURL                      string    `json:"webUrl"`
}

// IssueOptions names the extra fields to read: the acceptance-criteria
// field (configured per connection) and the sprint field (from Fields).
type IssueOptions struct {
	AcceptanceFieldID string
	SprintFieldID     string
}

var issueFields = []string{"summary", "status", "issuetype", "priority", "assignee", "reporter", "labels",
	"created", "updated", "description", "issuelinks", "subtasks"}

// Issue reads one issue with an explicit field list.
func (c *Client) Issue(ctx context.Context, key string, opt IssueOptions) (*Issue, error) {
	key = strings.TrimSpace(key)
	if !ValidKey(key) {
		return nil, &APIError{Code: "bad_request", Message: "invalid issue key (expected e.g. ABC-123)"}
	}
	fields := append([]string{}, issueFields...)
	if opt.AcceptanceFieldID != "" && ValidFieldID(opt.AcceptanceFieldID) {
		fields = append(fields, opt.AcceptanceFieldID)
	}
	if opt.SprintFieldID != "" && ValidFieldID(opt.SprintFieldID) {
		fields = append(fields, opt.SprintFieldID)
	}
	q := url.Values{}
	q.Set("fields", strings.Join(fields, ","))
	var ri rawIssue
	if err := c.get(ctx, "/issue/"+url.PathEscape(strings.ToUpper(key)), q, &ri); err != nil {
		return nil, err
	}
	f := ri.Fields
	out := &Issue{
		Key: ri.Key, ID: ri.ID,
		Summary:   strField(f["summary"]),
		Status:    nameField(f["status"]),
		IssueType: nameField(f["issuetype"]),
		Priority:  nameField(f["priority"]),
		Assignee:  userField(f["assignee"]),
		Reporter:  userField(f["reporter"]),
		Labels:    stringList(f["labels"]),
		Sprints:   []string{},
		Created:   strField(f["created"]),
		Updated:   strField(f["updated"]),
		Links:     []Link{},
		Subtasks:  []SubTask{},
		WebURL:    c.IssueWebURL(ri.Key),
	}
	var sc struct {
		StatusCategory struct {
			Name string `json:"name"`
		} `json:"statusCategory"`
	}
	if json.Unmarshal(f["status"], &sc) == nil {
		out.StatusCategory = sc.StatusCategory.Name
	}
	out.Description, out.DescriptionTruncated = capText(c.richText(f["description"]))
	if opt.AcceptanceFieldID != "" {
		out.AcceptanceFieldID = opt.AcceptanceFieldID
		if raw, ok := f[opt.AcceptanceFieldID]; ok && !isNull(raw) {
			txt, cut := capText(c.richText(raw))
			out.AcceptanceCriteria, out.AcceptanceCriteriaTruncated = &txt, cut
		}
	}
	if opt.SprintFieldID != "" {
		out.Sprints = sprintNames(f[opt.SprintFieldID])
	}
	var links []struct {
		Type struct {
			Inward  string `json:"inward"`
			Outward string `json:"outward"`
		} `json:"type"`
		InwardIssue  *rawIssue `json:"inwardIssue"`
		OutwardIssue *rawIssue `json:"outwardIssue"`
	}
	_ = json.Unmarshal(f["issuelinks"], &links)
	for _, l := range links {
		switch {
		case l.OutwardIssue != nil:
			out.Links = append(out.Links, Link{Type: l.Type.Outward, Key: l.OutwardIssue.Key,
				Summary: strField(l.OutwardIssue.Fields["summary"]), Status: nameField(l.OutwardIssue.Fields["status"])})
		case l.InwardIssue != nil:
			out.Links = append(out.Links, Link{Type: l.Type.Inward, Key: l.InwardIssue.Key,
				Summary: strField(l.InwardIssue.Fields["summary"]), Status: nameField(l.InwardIssue.Fields["status"])})
		}
	}
	var subs []rawIssue
	_ = json.Unmarshal(f["subtasks"], &subs)
	for _, s := range subs {
		out.Subtasks = append(out.Subtasks, SubTask{Key: s.Key, Summary: strField(s.Fields["summary"]), Status: nameField(s.Fields["status"])})
	}
	return out, nil
}

// richText renders a rich-text field: ADF on Cloud (v3), wiki markup
// strings on Data Center (v2).
func (c *Client) richText(raw json.RawMessage) string {
	if c.flavor == FlavorDataCenter {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return WikiText(s)
		}
	}
	return FieldText(raw)
}

func capText(s string) (string, bool) {
	if len(s) <= maxText {
		return s, false
	}
	cut := maxText
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut], true
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// ── field value helpers ─────────────────────────────────────────────

func isNull(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}

// statusCategory reads status.statusCategory.key ("" when absent).
func statusCategory(raw json.RawMessage) string {
	var v struct {
		StatusCategory struct {
			Key string `json:"key"`
		} `json:"statusCategory"`
	}
	if isNull(raw) || json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return v.StatusCategory.Key
}

func strField(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// nameField reads {name} (status, issuetype, priority).
func nameField(raw json.RawMessage) string {
	var v struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &v) == nil {
		return v.Name
	}
	return ""
}

// userField reads a user's display name.
func userField(raw json.RawMessage) string {
	var v struct {
		DisplayName string `json:"displayName"`
		Name        string `json:"name"`
	}
	if json.Unmarshal(raw, &v) == nil {
		if v.DisplayName != "" {
			return v.DisplayName
		}
		return v.Name
	}
	return ""
}

func stringList(raw json.RawMessage) []string {
	var xs []string
	if json.Unmarshal(raw, &xs) == nil && xs != nil {
		return xs
	}
	return []string{}
}

// sprintNames reads the sprint field: Cloud and newer DC give objects with
// name; older DC gives strings like
// "com.atlassian.greenhopper.service.sprint.Sprint@1a2b[id=1,…,name=Sprint 4,…]"
// (both doğrulanmadı on a live site).
var sprintNameRe = regexp.MustCompile(`name=([^,\]]*)`)

func sprintNames(raw json.RawMessage) []string {
	out := []string{}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return out
	}
	for _, it := range items {
		var obj struct {
			Name  string `json:"name"`
			State string `json:"state"`
		}
		if json.Unmarshal(it, &obj) == nil && obj.Name != "" {
			n := obj.Name
			if obj.State != "" {
				n += " (" + strings.ToLower(obj.State) + ")"
			}
			out = append(out, n)
			continue
		}
		var s string
		if json.Unmarshal(it, &s) == nil {
			if m := sprintNameRe.FindStringSubmatch(s); m != nil {
				out = append(out, m[1])
			} else if s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}
