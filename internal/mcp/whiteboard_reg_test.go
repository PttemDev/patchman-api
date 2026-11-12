package mcp

import (
	"context"
	"testing"
	"time"

	"patchman-be/internal/mcpscope"

	"github.com/gin-gonic/gin"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestWhiteboardToolsRegister exercises the real BuildServer path (not the REST
// layer the live curl hit): a bad jsonschema tag or reg[In] mismatch on any of
// the new whiteboard tools would panic here at registration / list.
func TestWhiteboardToolsRegister(t *testing.T) {
	gin.SetMode(gin.TestMode)
	scope := "whiteboards"
	srv := BuildServer(gin.New(), "test-secret", "user-1", mcpscope.Parse(&scope))

	ct, st := mcpsdk.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() { _ = srv.Run(ctx, st) }()

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	sess, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sess.Close()

	res, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	want := map[string]bool{
		"list_whiteboards": false, "get_whiteboard": false, "create_whiteboard": false,
		"update_whiteboard": false, "delete_whiteboard": false, "draw_on_whiteboard": false,
	}
	for _, tool := range res.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
		}
		// Scope was whiteboards-only → nothing else should be present.
		if _, ok := want[tool.Name]; !ok {
			t.Errorf("unexpected tool outside whiteboards scope: %s", tool.Name)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("whiteboard tool not registered: %s", name)
		}
	}
}
