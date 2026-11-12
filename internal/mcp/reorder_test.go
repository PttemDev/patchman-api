package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"patchman-be/internal/mcpscope"
	"patchman-be/internal/middleware"

	"github.com/gin-gonic/gin"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// orderStore is a tiny in-memory stand-in for the collection/request REST
// endpoints the reorder tools use. Lists come back the way the real handlers
// order them (sort_order ASC, then creation), PUTs set sortOrder and are
// recorded, and ids in `forbidden` answer 403 like a viewer-only share.
type orderStore struct {
	mu        sync.Mutex
	roots     []*fakeItem
	children  map[string][]*fakeItem // parent id → child collections/folders
	requests  map[string][]*fakeItem // collection id → saved requests
	forbidden map[string]bool
	puts      []fakePut
	lastQuery string
}

type fakeItem struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	SortOrder int    `json:"sortOrder"`
	created   int
}

type fakePut struct {
	Path string
	Body string
}

func items(spec ...any) []*fakeItem {
	var out []*fakeItem
	for i := 0; i < len(spec); i += 2 {
		out = append(out, &fakeItem{ID: spec[i].(string), Name: "name-" + spec[i].(string), SortOrder: spec[i+1].(int), created: len(out)})
	}
	return out
}

func displayed(list []*fakeItem) []*fakeItem {
	out := append([]*fakeItem(nil), list...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SortOrder != out[j].SortOrder {
			return out[i].SortOrder < out[j].SortOrder
		}
		return out[i].created < out[j].created
	})
	return out
}

func ids(list []*fakeItem) string {
	var s []string
	for _, it := range displayed(list) {
		s = append(s, it.ID)
	}
	return strings.Join(s, ",")
}

func (st *orderStore) engine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	api.Use(middleware.JWTAuth(testSecret, 7*24*time.Hour))
	api.GET("/collections", func(c *gin.Context) {
		st.mu.Lock()
		defer st.mu.Unlock()
		st.lastQuery = c.Request.URL.RawQuery
		c.JSON(http.StatusOK, gin.H{"data": displayed(st.roots)})
	})
	api.GET("/collections/:id/export", func(c *gin.Context) {
		st.mu.Lock()
		defer st.mu.Unlock()
		kids, ok := st.children[c.Param("id")]
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": "Collection not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"collection": gin.H{"id": c.Param("id"), "children": displayed(kids)}}})
	})
	api.GET("/collections/:id/requests", func(c *gin.Context) {
		st.mu.Lock()
		defer st.mu.Unlock()
		reqs, ok := st.requests[c.Param("id")]
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": "Collection not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": displayed(reqs)})
	})
	put := func(lists func() [][]*fakeItem) gin.HandlerFunc {
		return func(c *gin.Context) {
			st.mu.Lock()
			defer st.mu.Unlock()
			raw, _ := c.GetRawData()
			st.puts = append(st.puts, fakePut{Path: c.Request.URL.Path, Body: string(raw)})
			id := c.Param("id")
			if st.forbidden[id] {
				c.JSON(http.StatusForbidden, gin.H{"error": "You need editor access to update this collection"})
				return
			}
			var in struct {
				SortOrder *int `json:"sortOrder"`
			}
			_ = json.Unmarshal(raw, &in)
			for _, l := range lists() {
				for _, it := range l {
					if it.ID == id {
						if in.SortOrder != nil {
							it.SortOrder = *in.SortOrder
						}
						c.JSON(http.StatusOK, gin.H{"data": it})
						return
					}
				}
			}
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		}
	}
	api.PUT("/collections/:id", put(func() [][]*fakeItem {
		out := [][]*fakeItem{st.roots}
		for _, k := range st.children {
			out = append(out, k)
		}
		return out
	}))
	api.PUT("/requests/:id", put(func() [][]*fakeItem {
		var out [][]*fakeItem
		for _, r := range st.requests {
			out = append(out, r)
		}
		return out
	}))
	return r
}

func decodeReorder(t *testing.T, res *mcpsdk.CallToolResult) reorderResult {
	t.Helper()
	var out struct {
		Data reorderResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(text(t, res)), &out); err != nil {
		t.Fatalf("decode reorder result: %v (raw=%q)", err, text(t, res))
	}
	return out.Data
}

func resultIDs(order []orderItem) string {
	var s []string
	for _, it := range order {
		s = append(s, it.ID)
	}
	return strings.Join(s, ",")
}

// A successful top-level reorder: the listed ids come first, and only the
// rows whose position changed are written — position 0 included.
func TestReorderCollectionsTopLevel(t *testing.T) {
	st := &orderStore{roots: items("A", 0, "B", 1, "C", 2, "D", 3)}
	cs := connect(t, st.engine())
	res := call(t, cs, "reorder_collections", map[string]any{"ids": []string{"B", "A"}, "workspaceId": "ws1"})
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", text(t, res))
	}
	if st.lastQuery != "workspaceId=ws1" {
		t.Errorf("list query = %q, want workspaceId=ws1", st.lastQuery)
	}
	if got := ids(st.roots); got != "B,A,C,D" {
		t.Errorf("order after reorder = %s, want B,A,C,D", got)
	}
	if len(st.puts) != 2 {
		t.Fatalf("PUTs = %+v, want 2 (only B and A moved)", st.puts)
	}
	if st.puts[0].Path != "/api/collections/B" || !strings.Contains(st.puts[0].Body, `"sortOrder":0`) {
		t.Errorf("first PUT = %+v, want /api/collections/B with sortOrder 0", st.puts[0])
	}
	out := decodeReorder(t, res)
	if resultIDs(out.Order) != "B,A,C,D" || out.Unchanged != 2 || len(out.Updated) != 2 || len(out.Failed) != 0 {
		t.Errorf("result = %+v", out)
	}
}

// Top level without a workspace is refused before anything is read or
// written: the unfiltered list spans every workspace, and renumbering it
// would reshuffle shared team workspaces for everyone.
func TestReorderCollectionsTopLevelNeedsWorkspace(t *testing.T) {
	st := &orderStore{roots: items("A", 0, "B", 1)}
	cs := connect(t, st.engine())
	res := call(t, cs, "reorder_collections", map[string]any{"ids": []string{"B", "A"}})
	if !res.IsError || !strings.Contains(text(t, res), "workspaceId is required") {
		t.Fatalf("want a workspaceId error, got: %s", text(t, res))
	}
	if len(st.puts) != 0 || ids(st.roots) != "A,B" {
		t.Fatalf("nothing may change: puts=%+v order=%s", st.puts, ids(st.roots))
	}
}

// An id that isn't a child of the parent is rejected before anything is
// written, and the answer names the current children so the agent can retry.
func TestReorderCollectionsRejectsUnknownID(t *testing.T) {
	st := &orderStore{children: map[string][]*fakeItem{"P": items("A", 0, "B", 1)}}
	cs := connect(t, st.engine())
	res := call(t, cs, "reorder_collections", map[string]any{"parentId": "P", "ids": []string{"B", "X"}})
	if !res.IsError {
		t.Fatal("expected a tool error for an unknown id")
	}
	msg := text(t, res)
	if !strings.Contains(msg, "X") || !strings.Contains(msg, `A \"name-A\"`) {
		t.Errorf("error should name the unknown id and list the current children: %s", msg)
	}
	if len(st.puts) != 0 {
		t.Errorf("nothing should be written on a rejected reorder, got %+v", st.puts)
	}

	dup := call(t, cs, "reorder_collections", map[string]any{"parentId": "P", "ids": []string{"A", "A"}})
	if !dup.IsError || !strings.Contains(text(t, dup), "more than once") {
		t.Errorf("duplicate ids should be rejected: %s", text(t, dup))
	}
	empty := call(t, cs, "reorder_collections", map[string]any{"parentId": "P", "ids": []string{}})
	if !empty.IsError {
		t.Errorf("empty ids should be rejected: %s", text(t, empty))
	}
	if len(st.puts) != 0 {
		t.Errorf("nothing should be written on a rejected reorder, got %+v", st.puts)
	}
}

// Children left out keep their relative order after the listed ones. All
// siblings start tied at sortOrder 0 (older rows): Z already sits at 0 so it
// isn't written, yet it ends first because everyone else gets a distinct
// position after it.
func TestReorderCollectionsUnlistedKeepOrderAfter(t *testing.T) {
	st := &orderStore{children: map[string][]*fakeItem{"P": items("W", 0, "X", 0, "Y", 0, "Z", 0)}}
	cs := connect(t, st.engine())
	res := call(t, cs, "reorder_collections", map[string]any{"parentId": "P", "ids": []string{"Z", "X"}})
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", text(t, res))
	}
	if got := ids(st.children["P"]); got != "Z,X,W,Y" {
		t.Errorf("order after reorder = %s, want Z,X,W,Y", got)
	}
	for _, p := range st.puts {
		if p.Path == "/api/collections/Z" {
			t.Errorf("Z already had sortOrder 0 and should not be written: %+v", st.puts)
		}
	}
	if out := decodeReorder(t, res); resultIDs(out.Order) != "Z,X,W,Y" {
		t.Errorf("result order = %s, want Z,X,W,Y", resultIDs(out.Order))
	}
}

func TestReorderRequests(t *testing.T) {
	st := &orderStore{requests: map[string][]*fakeItem{"c1": items("r1", 0, "r2", 1, "r3", 2)}}
	cs := connect(t, st.engine())
	res := call(t, cs, "reorder_requests", map[string]any{"collectionId": "c1", "ids": []string{"r3"}})
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", text(t, res))
	}
	if got := ids(st.requests["c1"]); got != "r3,r1,r2" {
		t.Errorf("order after reorder = %s, want r3,r1,r2", got)
	}
	for _, p := range st.puts {
		if !strings.HasPrefix(p.Path, "/api/requests/") {
			t.Errorf("request reorder should PUT /api/requests/:id, got %s", p.Path)
		}
	}

	st.puts = nil
	bad := call(t, cs, "reorder_requests", map[string]any{"collectionId": "c1", "ids": []string{"r9"}})
	if !bad.IsError || !strings.Contains(text(t, bad), "r9") {
		t.Errorf("unknown request id should be rejected: %s", text(t, bad))
	}
	if len(st.puts) != 0 {
		t.Errorf("nothing should be written on a rejected reorder, got %+v", st.puts)
	}
}

// A write the REST layer refuses is reported per id; the other moves still
// happen and the answer is a tool error that says so.
func TestReorderReportsPartialFailure(t *testing.T) {
	st := &orderStore{roots: items("A", 0, "B", 1, "C", 2), forbidden: map[string]bool{"B": true}}
	cs := connect(t, st.engine())
	res := call(t, cs, "reorder_collections", map[string]any{"ids": []string{"B", "C", "A"}, "workspaceId": "ws1"})
	if !res.IsError {
		t.Fatal("a refused write should make the answer a tool error")
	}
	out := decodeReorder(t, res)
	if len(out.Failed) != 1 || out.Failed[0].ID != "B" || out.Failed[0].Status != http.StatusForbidden || !strings.Contains(out.Failed[0].Error, "editor access") {
		t.Errorf("failed = %+v, want B with 403 and the backend's message", out.Failed)
	}
	if strings.Join(out.Updated, ",") != "C,A" || out.Note == "" {
		t.Errorf("updated = %v note = %q, want C,A and a note", out.Updated, out.Note)
	}
}

// Both reorder tools sit in the collections permission group: a token scoped
// to collections gets them, one without it doesn't.
func TestReorderToolsInCollectionsGroup(t *testing.T) {
	for _, tc := range []struct {
		scope string
		want  bool
	}{{"collections", true}, {"requests,db", false}} {
		scope := tc.scope
		cs := connectScoped(t, echoEngine(), mcpscope.Parse(&scope))
		res, err := cs.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		got := map[string]bool{}
		for _, tl := range res.Tools {
			got[tl.Name] = true
		}
		for _, name := range []string{"reorder_collections", "reorder_requests"} {
			if got[name] != tc.want {
				t.Errorf("scope %q: %s present = %v, want %v", tc.scope, name, got[name], tc.want)
			}
		}
	}
}
