package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Writes (CR_QA_RFC P2): a comment on an issue and a new issue. Each is
// called only on a person's click in the CR + QA Hub. Cloud (/rest/api/3)
// takes Atlassian Document Format; Data Center (/rest/api/2) plain text.

func (c *Client) send(ctx context.Context, method, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return &APIError{Code: "bad_request", Message: "invalid Jira request"}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+c.apiPath()+path, bytes.NewReader(b))
	if err != nil {
		return &APIError{Code: "bad_request", Message: "invalid Jira request"}
	}
	req.Header.Set("Authorization", c.auth)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return &APIError{Code: "unreachable", Message: "Jira did not answer in time."}
		}
		return &APIError{Code: "unreachable", Message: "Jira is not reachable: " + err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode/100 != 2 {
		return newAPIError(resp.StatusCode, raw)
	}
	if out != nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, out)
	}
	return nil
}

// adf wraps plain text (paragraphs split on blank lines, lines kept) as an
// Atlassian Document Format doc for Jira Cloud.
func adf(text string) map[string]any {
	paras := []any{}
	for _, p := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n\n") {
		if strings.TrimSpace(p) == "" {
			continue
		}
		content := []any{}
		for i, line := range strings.Split(p, "\n") {
			if i > 0 {
				content = append(content, map[string]any{"type": "hardBreak"})
			}
			if line != "" {
				content = append(content, map[string]any{"type": "text", "text": line})
			}
		}
		paras = append(paras, map[string]any{"type": "paragraph", "content": content})
	}
	return map[string]any{"type": "doc", "version": 1, "content": paras}
}

func (c *Client) richBody(text string) any {
	if c.flavor == FlavorDataCenter {
		return text
	}
	return adf(text)
}

// AddComment adds a comment to an issue.
func (c *Client) AddComment(ctx context.Context, key, text string) error {
	if !ValidKey(key) {
		return &APIError{Code: "bad_request", Message: "invalid issue key"}
	}
	return c.send(ctx, http.MethodPost, "/issue/"+url.PathEscape(key)+"/comment", map[string]any{"body": c.richBody(text)}, nil)
}

// CreateIssue creates an issue and returns its key.
func (c *Client) CreateIssue(ctx context.Context, projectKey, issueType, summary, description string) (string, error) {
	if issueType == "" {
		issueType = "Bug"
	}
	fields := map[string]any{
		"project":   map[string]any{"key": projectKey},
		"issuetype": map[string]any{"name": issueType},
		"summary":   summary,
	}
	if strings.TrimSpace(description) != "" {
		fields["description"] = c.richBody(description)
	}
	var out struct {
		Key string `json:"key"`
	}
	if err := c.send(ctx, http.MethodPost, "/issue", map[string]any{"fields": fields}, &out); err != nil {
		return "", err
	}
	return out.Key, nil
}

// Project is a Jira project a connection can see.
type Project struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

// Projects lists projects matching query (key or name, case-insensitive),
// at most 50. Cloud searches server-side (/project/search); Data Center
// returns every visible project (/project) and is filtered here.
func (c *Client) Projects(ctx context.Context, query string) ([]Project, error) {
	query = strings.TrimSpace(query)
	out := []Project{}
	if c.flavor == FlavorDataCenter {
		var all []Project
		if err := c.get(ctx, "/project", nil, &all); err != nil {
			return nil, err
		}
		q := strings.ToLower(query)
		for _, p := range all {
			if q == "" || strings.Contains(strings.ToLower(p.Key), q) || strings.Contains(strings.ToLower(p.Name), q) {
				out = append(out, p)
				if len(out) == 50 {
					break
				}
			}
		}
		return out, nil
	}
	v := url.Values{"maxResults": {"50"}, "orderBy": {"key"}}
	if query != "" {
		v.Set("query", query)
	}
	var page struct {
		Values []Project `json:"values"`
	}
	if err := c.get(ctx, "/project/search", v, &page); err != nil {
		return nil, err
	}
	return append(out, page.Values...), nil
}
