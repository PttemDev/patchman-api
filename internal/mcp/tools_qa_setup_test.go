package mcp

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Setup changes are the user's decision: asked in the client when it can
// ask, left as a proposal when it can't, applied directly only when the user
// turned the question off. A token passed by the agent never comes back.
func TestQASetupDecisions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var mu sync.Mutex
	var calls []string
	ask := `{"data":{"ask":true}}`
	e := gin.New()
	e.Any("/*p", func(c *gin.Context) {
		b, _ := io.ReadAll(c.Request.Body)
		mu.Lock()
		calls = append(calls, c.Request.Method+" "+c.Request.URL.Path+" "+string(b))
		mu.Unlock()
		switch c.Request.URL.Path {
		case "/api/qa/agent-setup-policy":
			c.String(http.StatusOK, ask)
		case "/api/qa/proposals":
			c.String(http.StatusCreated, `{"data":{"id":"p1","status":"pending"}}`)
		default:
			c.String(http.StatusOK, `{"data":{"id":"x1","gitProject":"grp/shop"}}`)
		}
	})
	last := func() string { mu.Lock(); defer mu.Unlock(); return calls[len(calls)-1] }
	args := map[string]any{"gitConnectionId": "g1", "gitProject": "grp/shop", "jiraProjectKey": "mer"}

	// No elicitation: a proposal, nothing applied.
	cs := connect(t, e)
	out := text(t, call(t, cs, "qa_link_save", args))
	if !strings.Contains(out, "Waiting for the user") || !strings.Contains(last(), "POST /api/qa/proposals") || !strings.Contains(last(), `"path":"/qa/links"`) {
		t.Fatalf("proposal: %s | %s", out, last())
	}

	// Elicitation: declined → nothing; accepted → applied.
	answer := "decline"
	cs2 := connectWith(t, e, &mcpsdk.ClientOptions{ElicitationHandler: func(_ context.Context, r *mcpsdk.ElicitRequest) (*mcpsdk.ElicitResult, error) {
		if !strings.Contains(r.Params.Message, "Link grp/shop to Jira MER") {
			t.Errorf("message: %s", r.Params.Message)
		}
		if answer == "accept" {
			return &mcpsdk.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
		}
		return &mcpsdk.ElicitResult{Action: "decline"}, nil
	}})
	if res := call(t, cs2, "qa_link_save", args); !res.IsError {
		t.Fatalf("declined but applied: %s", text(t, res))
	}
	answer = "accept"
	if out := text(t, call(t, cs2, "qa_link_save", args)); !strings.HasPrefix(out, "Done:") || !strings.Contains(last(), "POST /api/qa/links") {
		t.Fatalf("accepted: %s | %s", out, last())
	}

	// Question turned off: applied directly; the token is not echoed.
	ask = `{"data":{"ask":false}}`
	out = text(t, call(t, cs, "jira_connection_create", map[string]any{"name": "j", "baseUrl": "https://acme.atlassian.net", "token": "SECRET-TOKEN-123"}))
	if !strings.HasPrefix(out, "Done:") || strings.Contains(out, "SECRET-TOKEN-123") || !strings.Contains(last(), "POST /api/jira-connections") {
		t.Fatalf("direct: %s | %s", out, last())
	}
}
