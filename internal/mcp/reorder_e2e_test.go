package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"patchman-be/internal/handlers"
	"patchman-be/internal/mcp"
	"patchman-be/internal/mcpscope"
	"patchman-be/internal/middleware"
	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"gorm.io/gorm"
)

// TestReorderAgainstRealHandlers runs the reorder tools through the real
// collection/request handlers on SQLite: the list shapes they parse, the
// sortOrder PUTs, the display order list_collections / list_collection_requests
// report afterwards, and that someone without access is refused.
func TestReorderAgainstRealHandlers(t *testing.T) {
	dsn := fmt.Sprintf("file:reorder_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.User{}, &models.Collection{}, &models.SavedRequest{}, &models.CollectionShare{},
		&models.CollectionPublicGrant{}, &models.Workspace{}, &models.Team{}, &models.TeamMember{}); err != nil {
		t.Fatal(err)
	}
	const owner, stranger, secret = "u1", "u2", "s"
	base := time.Now().Add(-time.Hour)
	parent := "P"
	col := func(id, user string, parentID *string, i int) {
		// Every row starts at sortOrder 0, like rows created before any reorder.
		db.Create(&models.Collection{ID: id, UserID: user, Name: "n" + id, ParentID: parentID, Type: "folder",
			Visibility: "private", CreatedAt: base.Add(time.Duration(i) * time.Second)})
	}
	col("P", owner, nil, 0)
	col("Q", owner, nil, 1)
	for i, id := range []string{"W", "X", "Y", "Z"} {
		col(id, owner, &parent, 2+i)
	}
	for i, id := range []string{"r1", "r2", "r3"} {
		db.Create(&models.SavedRequest{ID: id, CollectionID: "P", Name: id, Method: "GET", URL: "x", SortOrder: i,
			CreatedAt: base.Add(time.Duration(i) * time.Second)})
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	api.Use(middleware.JWTAuth(secret, time.Hour))
	ch, rh := handlers.NewCollectionHandler(db), handlers.NewRequestHandler(db)
	api.GET("/collections", ch.List)
	api.PUT("/collections/:id", ch.Update)
	api.GET("/collections/:id/requests", ch.ListRequests)
	api.GET("/collections/:id/export", ch.Export)
	api.PUT("/requests/:id", rh.Update)

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
		return res.IsError, res.Content[0].(*mcpsdk.TextContent).Text
	}
	listedIDs := func(cs *mcpsdk.ClientSession, name string, args any) string {
		_, out := call(cs, name, args)
		var res struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("%s: %v (%s)", name, err, out)
		}
		var ids []string
		for _, d := range res.Data {
			ids = append(ids, d.ID)
		}
		return strings.Join(ids, ",")
	}
	children := func() string {
		var kids []models.Collection
		db.Where("parent_id = ?", "P").Order("sort_order ASC, created_at ASC").Find(&kids)
		var ids []string
		for _, k := range kids {
			ids = append(ids, k.ID)
		}
		return strings.Join(ids, ",")
	}

	cs := connectAs(owner)
	if isErr, out := call(cs, "reorder_collections", map[string]any{"parentId": "P", "ids": []string{"Z", "X"}}); isErr || children() != "Z,X,W,Y" {
		t.Fatalf("nested reorder: %s → %s, want Z,X,W,Y", out, children())
	}
	if isErr, out := call(cs, "reorder_collections", map[string]any{"ids": []string{"Q"}, "workspaceId": "__none__"}); isErr {
		t.Fatalf("top-level reorder: %s", out)
	}
	if got := listedIDs(cs, "list_collections", map[string]any{}); got != "Q,P" {
		t.Errorf("list_collections order = %s, want Q,P", got)
	}
	if isErr, out := call(cs, "reorder_requests", map[string]any{"collectionId": "P", "ids": []string{"r3", "r1"}}); isErr {
		t.Fatalf("request reorder: %s", out)
	}
	if got := listedIDs(cs, "list_collection_requests", map[string]any{"id": "P"}); got != "r3,r1,r2" {
		t.Errorf("list_collection_requests order = %s, want r3,r1,r2", got)
	}

	if isErr, out := call(connectAs(stranger), "reorder_collections", map[string]any{"parentId": "P", "ids": []string{"W"}}); !isErr || children() != "Z,X,W,Y" {
		t.Errorf("a user without access must be refused and change nothing: %s → %s", out, children())
	}
}
