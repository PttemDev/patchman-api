package mcp_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"patchman-be/internal/handlers"
	"patchman-be/internal/jira/jiratest"
	"patchman-be/internal/mcp"
	"patchman-be/internal/mcpscope"
	"patchman-be/internal/middleware"
	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"gorm.io/gorm"
)

// TestJiraToolsAgainstRealHandlers runs list_jira_connections, jira_search
// and jira_issue through the real Jira handlers and a fake Jira Cloud: the
// replies are compact text, the AC field is rendered, a long description is
// cut with a note, list counts are honest ("showing N", next cursor), the
// token never appears, and a stranger is refused.
func TestJiraToolsAgainstRealHandlers(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:jiramcp_%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.JiraConnection{}, &models.JiraConnectionGrant{}, &models.JiraActivityHistory{}); err != nil {
		t.Fatal(err)
	}
	db.Exec("CREATE TABLE IF NOT EXISTS team_members (team_id TEXT, user_id TEXT)")
	db.Exec("CREATE TABLE IF NOT EXISTS users (id TEXT, email TEXT, name TEXT)")
	cipher, _ := services.NewTokenCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)))
	fake := jiratest.New("cloud")
	t.Cleanup(fake.Close)
	enc, _ := cipher.Encrypt(jiratest.APIToken)
	opts, _ := json.Marshal(map[string]any{"acFieldId": jiratest.ACField, "defaultJql": "project = MER"})
	db.Create(&models.JiraConnection{ID: "j1", UserID: "owner", Name: "acme", BaseURL: fake.URL, Flavor: "cloud",
		AuthKind: "email_token", Email: jiratest.Email, CredentialEnc: enc, OptionsJSON: string(opts)})

	const secret = "s"
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	api.Use(middleware.JWTAuth(secret, time.Hour))
	jh := handlers.NewJiraConnectionHandler(db, cipher)
	api.GET("/jira-connections", jh.List)
	api.GET("/jira-connections/:id/search", jh.Search)
	api.GET("/jira-connections/:id/issue/:key", jh.Issue)

	connectAs := func(user string) *mcpsdk.ClientSession {
		ct, st := mcpsdk.NewInMemoryTransports()
		if _, err := mcp.BuildServer(r, secret, user, mcpscope.Parse(nil)).Connect(context.Background(), st, nil); err != nil {
			t.Fatal(err)
		}
		cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "t", Version: "0"}, nil).Connect(context.Background(), ct, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cs.Close() })
		return cs
	}
	call := func(cs *mcpsdk.ClientSession, name string, args any) (bool, string) {
		res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		txt := res.Content[0].(*mcpsdk.TextContent).Text
		if strings.Contains(txt, jiratest.APIToken) {
			t.Fatalf("%s leaks the token: %s", name, txt)
		}
		return res.IsError, txt
	}
	owner := connectAs("owner")

	if isErr, out := call(owner, "list_jira_connections", map[string]any{}); isErr || !strings.Contains(out, `"id":"j1"`) || !strings.Contains(out, `"hasSecret":true`) {
		t.Fatalf("list = %v %s", isErr, out)
	}

	isErr, out := call(owner, "jira_search", map[string]any{"id": "j1", "limit": 2})
	if isErr || !strings.Contains(out, "Showing 2 issues; more match (total not reported by Jira Cloud). Next page: next=\"tok-p2\".") ||
		!strings.Contains(out, "JQL: project = MER") || !strings.Contains(out, "MER-1  [In Test]  Story  High  @Ada Lovelace  Issue 1") {
		t.Fatalf("search = %s", out)
	}
	if _, out := call(owner, "jira_search", map[string]any{"id": "j1", "next": "tok-p3"}); !strings.Contains(out, "Showing 1 issues (last page).") {
		t.Fatalf("last page = %s", out)
	}

	isErr, out = call(owner, "jira_issue", map[string]any{"id": "j1", "key": "MER-936"})
	if isErr {
		t.Fatalf("issue error: %s", out)
	}
	for _, want := range []string{
		"MER-936: Order list paging",
		"Status: In Test (In Progress) | Type: Story | Priority: High",
		"Sprint: Sprint 12 (active)",
		"Acceptance criteria (" + jiratest.ACField + "):\nGiven a merchant\n- lists orders by date\n- sees 20 per page",
		"(truncated: showing 2000 of ",
		"Links (2):\n- blocks MER-937 [To Do] Export orders\n- relates to MER-12 [Done] Order API",
		"Subtasks (1):\n- MER-940 [Done] API tests",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("issue lacks %q:\n%s", want, out)
		}
	}
	if len(out) > 4000 {
		t.Fatalf("issue reply not compact: %d bytes", len(out))
	}
	// A crafted key is refused by the BE before Jira is called.
	before := len(fake.Seen())
	if isErr, _ := call(owner, "jira_issue", map[string]any{"id": "j1", "key": "../../myself"}); !isErr || len(fake.Seen()) != before {
		t.Fatal("crafted key reached Jira")
	}

	stranger := connectAs("stranger")
	if isErr, _ := call(stranger, "jira_issue", map[string]any{"id": "j1", "key": "MER-936"}); !isErr {
		t.Fatal("stranger read an issue")
	}
	if _, out := call(stranger, "list_jira_connections", map[string]any{}); strings.Contains(out, "j1") {
		t.Fatalf("stranger lists the connection: %s", out)
	}

	// The tools sit in the jira group; list_jira_connections in connections.
	only := "jira"
	ct, st := mcpsdk.NewInMemoryTransports()
	_, _ = mcp.BuildServer(r, secret, "owner", mcpscope.Parse(&only)).Connect(context.Background(), st, nil)
	cs, _ := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "t", Version: "0"}, nil).Connect(context.Background(), ct, nil)
	defer cs.Close()
	tools, _ := cs.ListTools(context.Background(), nil)
	names := []string{}
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
		if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
			t.Fatalf("%s must be annotated read-only", tl.Name)
		}
	}
	if strings.Join(names, ",") != "jira_issue,jira_search" && strings.Join(names, ",") != "jira_search,jira_issue" {
		t.Fatalf("jira group tools = %v", names)
	}
}
