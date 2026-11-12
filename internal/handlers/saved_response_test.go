package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"patchman-be/internal/database"
	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
)

func savedRespEnv(t *testing.T) (*gin.Engine, func(user, method, path string, body any) (int, map[string]any)) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := database.Init("sqlite", filepath.Join(t.TempDir(), "sr.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []models.User{{ID: "alice", Email: "alice@example.com", Name: "Alice"}, {ID: "bob", Email: "bob@example.com", Name: "Bob"}} {
		if err := db.Create(&u).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&models.Collection{ID: "col", UserID: "alice", Name: "API"}).Error; err != nil {
		t.Fatal(err)
	}
	parent := "col"
	if err := db.Create(&models.Collection{ID: "folder", UserID: "alice", Name: "Orders", ParentID: &parent, Type: "folder"}).Error; err != nil {
		t.Fatal(err)
	}
	h := NewSavedResponseHandler(db)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", c.GetHeader("X-User")); c.Next() })
	r.GET("/saved-responses", h.List)
	r.POST("/saved-responses", h.Create)
	r.GET("/saved-responses/:id", h.Get)
	r.PATCH("/saved-responses/:id", h.Rename)
	r.DELETE("/saved-responses/:id", h.Delete)
	call := func(user, method, path string, body any) (int, map[string]any) {
		var rd *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		} else {
			rd = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, rd)
		req.Header.Set("X-User", user)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	return r, call
}

func TestSavedResponses(t *testing.T) {
	_, call := savedRespEnv(t)
	body := map[string]any{
		"collectionId": "folder", "requestId": "req1", "name": "", "method": "post", "url": "https://api.example.com/orders",
		"request":  map[string]any{"method": "POST", "url": "{{base}}/orders", "headers": []any{map[string]any{"key": "X-T", "value": "1", "enabled": true}}},
		"sent":     map[string]any{"method": "POST", "url": "https://api.example.com/orders"},
		"response": map[string]any{"statusCode": 201, "statusText": "Created", "durationMs": 42, "size": 17, "headers": map[string]string{"Content-Type": "application/json"}, "body": `{"id":7}`},
	}
	code, res := call("alice", "POST", "/saved-responses", body)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, res)
	}
	id := res["data"].(map[string]any)["id"].(string)
	if n := res["data"].(map[string]any)["name"]; n != "POST https://api.example.com/orders" {
		t.Errorf("default name %v", n)
	}

	// Listing by root + folder ids finds it; bodies are not in the list.
	code, res = call("alice", "GET", "/saved-responses?collections=col,folder", nil)
	list := res["data"].([]any)
	if code != 200 || len(list) != 1 {
		t.Fatalf("list: %d %v", code, res)
	}
	if _, has := list[0].(map[string]any)["responseBody"]; has {
		t.Error("list must not carry bodies")
	}
	if list[0].(map[string]any)["authorName"] != "Alice" || list[0].(map[string]any)["canDelete"] != true {
		t.Errorf("list row %v", list[0])
	}

	// The detail carries request, sent and response.
	code, res = call("alice", "GET", "/saved-responses/"+id, nil)
	d := res["data"].(map[string]any)
	if code != 200 || d["responseBody"] != `{"id":7}` || d["request"].(map[string]any)["url"] != "{{base}}/orders" {
		t.Fatalf("get: %d %v", code, res)
	}

	// Bob has no access to the collection: invisible and not deletable.
	if _, res = call("bob", "GET", "/saved-responses?collections=col,folder", nil); len(res["data"].([]any)) != 0 {
		t.Errorf("bob sees %v", res)
	}
	if code, _ = call("bob", "GET", "/saved-responses/"+id, nil); code != http.StatusNotFound {
		t.Errorf("bob get: %d", code)
	}
	if code, _ = call("bob", "DELETE", "/saved-responses/"+id, nil); code != http.StatusNotFound {
		t.Errorf("bob delete: %d", code)
	}
	if code, _ = call("bob", "POST", "/saved-responses", body); code != http.StatusNotFound {
		t.Errorf("bob create: %d", code)
	}

	// Rename + delete by the author.
	if code, _ = call("alice", "PATCH", "/saved-responses/"+id, map[string]any{"name": "Order created"}); code != 200 {
		t.Errorf("rename: %d", code)
	}
	if code, _ = call("alice", "DELETE", "/saved-responses/"+id, nil); code != 200 {
		t.Errorf("delete: %d", code)
	}
	if _, res = call("alice", "GET", "/saved-responses?collections=col,folder", nil); len(res["data"].([]any)) != 0 {
		t.Errorf("still listed after delete: %v", res)
	}
}

func TestSavedResponsesStayAttached(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dbPath := filepath.Join(t.TempDir(), "sr2.db")
	db, err := database.Init("sqlite", dbPath, "")
	if err != nil {
		t.Fatal(err)
	}
	root := "root"
	mk := func(id, name string, parent *string) {
		if err := db.Create(&models.Collection{ID: id, UserID: "alice", Name: name, ParentID: parent, Type: "folder"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	mk(root, "API", nil)
	mk("orders", "Orders", &root)
	ordersID := "orders"
	mk("deep", "Archive", &ordersID)
	if err := db.Create(&models.SavedRequest{ID: "req-get", CollectionID: "orders", Name: "Get order", Method: "GET", URL: "/orders/1"}).Error; err != nil {
		t.Fatal(err)
	}
	sr := func(id, col, req string) {
		if err := db.Create(&models.SavedResponse{ID: id, UserID: "alice", CollectionID: col, RequestID: req, Name: id, Method: "GET", URL: "/x"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	sr("a", "orders", "req-get")
	sr("b", "deep", "")
	sr("c", "orders", "")
	get := func(id string) models.SavedResponse {
		var r models.SavedResponse
		db.First(&r, "id = ?", id)
		return r
	}

	// Request filter on the list.
	h := NewSavedResponseHandler(db)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", "alice"); c.Next() })
	r.GET("/saved-responses", h.List)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/saved-responses?collections=orders,deep&request=req-get", nil))
	var out struct{ Data []map[string]any }
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Data) != 1 || out.Data[0]["id"] != "a" {
		t.Fatalf("request filter: %s", w.Body.String())
	}
	// A response of that request filed into another folder is listed too;
	// one whose collection the caller cannot see is not.
	sr("a2", "deep", "req-get")
	if err := db.Create(&models.Collection{ID: "foreign", UserID: "bob", Name: "Bob's"}).Error; err != nil {
		t.Fatal(err)
	}
	sr("hidden", "foreign", "req-get")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/saved-responses?request=req-get", nil))
	out.Data = nil
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Data) != 2 {
		t.Fatalf("request filter across folders: %s", w.Body.String())
	}
	db.Where("id IN ?", []string{"a2", "hidden"}).Delete(&models.SavedResponse{})
	db.Where("id = ?", "foreign").Delete(&models.Collection{})

	// Re-sync: the tree is wiped and rebuilt with new ids; saved responses
	// follow the folder path and the request name + method.
	anchors := snapshotSavedResponses(db, root)
	db.Where("collection_id IN ?", []string{"orders", "deep"}).Delete(&models.SavedRequest{})
	db.Where("id IN ?", []string{"orders", "deep"}).Delete(&models.Collection{})
	mk("orders2", "Orders", &root)
	if err := db.Create(&models.SavedRequest{ID: "req-get2", CollectionID: "orders2", Name: "Get order", Method: "get", URL: "/orders/1"}).Error; err != nil {
		t.Fatal(err)
	}
	restoreSavedResponses(db, root, anchors)
	if a := get("a"); a.CollectionID != "orders2" || a.RequestID != "req-get2" {
		t.Errorf("a after resync: %+v", a)
	}
	if b := get("b"); b.CollectionID != root {
		t.Errorf("b (its folder vanished) should land on the root: %+v", b)
	}
	if c := get("c"); c.CollectionID != "orders2" || c.RequestID != "" {
		t.Errorf("c after resync: %+v", c)
	}

	// Deleting a folder moves its saved responses up to the parent.
	var orders2 models.Collection
	db.First(&orders2, "id = ?", "orders2")
	rehomeSavedResponsesOnDelete(db, orders2)
	if a := get("a"); a.CollectionID != root {
		t.Errorf("folder delete should move a to the root: %+v", a)
	}
	// Deleting the whole collection deletes them.
	var rootRow models.Collection
	db.First(&rootRow, "id = ?", root)
	rehomeSavedResponsesOnDelete(db, rootRow)
	var n int64
	db.Model(&models.SavedResponse{}).Count(&n)
	if n != 0 {
		t.Errorf("collection delete left %d saved responses", n)
	}

	// Startup drops saved responses whose collection no longer exists.
	sr("orphan", "gone", "")
	sr("kept", root, "")
	sqlDB, _ := db.DB()
	_ = sqlDB.Close()
	db2, err := database.Init("sqlite", dbPath, "")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	db2.Model(&models.SavedResponse{}).Pluck("id", &ids)
	if len(ids) != 1 || ids[0] != "kept" {
		t.Errorf("after startup cleanup: %v", ids)
	}
}
