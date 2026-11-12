package handlers

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func wbPublicTestHandler(t *testing.T) (*WhiteboardSharingHandler, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:wbpublic?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.Whiteboard{}, &models.WhiteboardPublicLink{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db.Exec("DELETE FROM whiteboards")
	db.Exec("DELETE FROM whiteboard_public_links")
	return NewWhiteboardSharingHandler(db), db
}

func wbNode(db *gorm.DB, id, parent, typ, vis string) {
	var p *string
	if parent != "" {
		p = &parent
	}
	db.Create(&models.Whiteboard{ID: id, UserID: "owner", ParentID: p, Type: typ, Name: id, SceneJSON: `{"elements":["` + id + `"]}`, Visibility: vis})
}

func wbLink(db *gorm.DB, raw, boardID, role string) {
	db.Create(&models.WhiteboardPublicLink{ID: raw, WhiteboardID: boardID, UserID: "owner", TokenHash: wbHashToken(raw), Token: raw, Prefix: raw[:4], Role: role})
}

func wbPublicCall(t *testing.T, fn gin.HandlerFunc, method string, params gin.Params, body string) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = params
	c.Request = httptest.NewRequest(method, "/", bytes.NewReader([]byte(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	fn(c)
	var resp struct {
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w.Code, resp.Data
}

func TestPublicFolderLink(t *testing.T) {
	h, db := wbPublicTestHandler(t)
	// shared/            ← public folder with a link
	//   a                 board
	//   sub/              folder (private — inherits the share)
	//     deep            board
	// outside             board, not under the folder
	wbNode(db, "shared", "", "folder", "public")
	wbNode(db, "a", "shared", "whiteboard", "private")
	wbNode(db, "sub", "shared", "folder", "private")
	wbNode(db, "deep", "sub", "whiteboard", "private")
	wbNode(db, "outside", "", "whiteboard", "public")
	wbLink(db, "view-tok", "shared", "viewer")
	wbLink(db, "edit-tok", "shared", "editor")

	tok := func(v string) gin.Params { return gin.Params{{Key: "token", Value: v}} }
	board := func(v, id string) gin.Params { return gin.Params{{Key: "token", Value: v}, {Key: "boardId", Value: id}} }

	// The folder link lists the whole subtree, no scenes.
	code, data := wbPublicCall(t, h.PublicGet, "GET", tok("view-tok"), "")
	if code != 200 || data["type"] != "folder" {
		t.Fatalf("folder get = %d %v", code, data)
	}
	items, _ := data["items"].([]any)
	names := map[string]bool{}
	for _, it := range items {
		m := it.(map[string]any)
		names[m["id"].(string)] = true
		if _, has := m["scene"]; has {
			t.Errorf("folder listing leaked a scene for %v", m["id"])
		}
	}
	if len(items) != 3 || !names["a"] || !names["sub"] || !names["deep"] || names["outside"] {
		t.Errorf("folder items = %v", names)
	}

	// Boards under the folder open at any depth; the scene comes back.
	for _, id := range []string{"a", "deep"} {
		code, data = wbPublicCall(t, h.PublicGetBoard, "GET", board("view-tok", id), "")
		if code != 200 || data["scene"] != `{"elements":["`+id+`"]}` || data["role"] != "viewer" {
			t.Errorf("board %s = %d %v", id, code, data)
		}
	}
	// A board outside the folder, a subfolder, or the folder itself is not a board here.
	for _, id := range []string{"outside", "sub", "shared", "missing"} {
		if code, _ = wbPublicCall(t, h.PublicGetBoard, "GET", board("view-tok", id), ""); code != 404 {
			t.Errorf("board %s under folder link = %d, want 404", id, code)
		}
	}

	// Viewer link can't write; editor link can, only inside the folder.
	if code, _ = wbPublicCall(t, h.PublicUpdateBoard, "PATCH", board("view-tok", "deep"), `{"scene":"x"}`); code != 403 {
		t.Errorf("viewer PATCH = %d, want 403", code)
	}
	if code, _ = wbPublicCall(t, h.PublicUpdateBoard, "PATCH", board("edit-tok", "deep"), `{"scene":"{\"v\":2}"}`); code != 200 {
		t.Errorf("editor PATCH = %d, want 200", code)
	}
	var deep models.Whiteboard
	db.First(&deep, "id = ?", "deep")
	if deep.SceneJSON != `{"v":2}` {
		t.Errorf("deep scene = %q", deep.SceneJSON)
	}
	if code, _ = wbPublicCall(t, h.PublicUpdateBoard, "PATCH", board("edit-tok", "outside"), `{"scene":"x"}`); code != 404 {
		t.Errorf("editor PATCH outside = %d, want 404", code)
	}
	// The folder row itself has no scene to save.
	if code, _ = wbPublicCall(t, h.PublicUpdate, "PATCH", tok("edit-tok"), `{"scene":"x"}`); code != 404 {
		t.Errorf("PATCH folder link root = %d, want 404", code)
	}

	// Unsharing the folder kills the link for everything under it.
	db.Model(&models.Whiteboard{}).Where("id = ?", "shared").Update("visibility", "private")
	if code, _ = wbPublicCall(t, h.PublicGetBoard, "GET", board("view-tok", "a"), ""); code != 404 {
		t.Errorf("board after unshare = %d, want 404", code)
	}
}

func TestPublicBoardLinkUnchanged(t *testing.T) {
	h, db := wbPublicTestHandler(t)
	wbNode(db, "solo", "", "whiteboard", "public")
	wbNode(db, "other", "", "whiteboard", "public")
	wbLink(db, "solo-tok", "solo", "editor")

	code, data := wbPublicCall(t, h.PublicGet, "GET", gin.Params{{Key: "token", Value: "solo-tok"}}, "")
	if code != 200 || data["type"] != "whiteboard" || data["scene"] != `{"elements":["solo"]}` {
		t.Fatalf("board link get = %d %v", code, data)
	}
	if code, _ = wbPublicCall(t, h.PublicUpdate, "PATCH", gin.Params{{Key: "token", Value: "solo-tok"}}, `{"scene":"{}"}`); code != 200 {
		t.Errorf("board link PATCH = %d", code)
	}
	// A board link never reaches another board by id.
	if code, _ = wbPublicCall(t, h.PublicGetBoard, "GET", gin.Params{{Key: "token", Value: "solo-tok"}, {Key: "boardId", Value: "other"}}, ""); code != 404 {
		t.Errorf("board link → other board = %d, want 404", code)
	}
}
