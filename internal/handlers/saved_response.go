package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// SavedResponseHandler stores request/response pairs under collections.
// Anyone who can see the collection lists and opens them; the author or a
// collection editor may rename / delete. Bodies are capped so one huge
// download can't bloat the database.
type SavedResponseHandler struct {
	db *gorm.DB
}

func NewSavedResponseHandler(db *gorm.DB) *SavedResponseHandler {
	return &SavedResponseHandler{db: db}
}

const (
	savedResponseMaxBody = 5 << 20 // 5 MiB
	savedResponseMaxList = 500
)

type savedResponseInput struct {
	CollectionID string          `json:"collectionId" binding:"required"`
	RequestID    string          `json:"requestId"`
	Name         string          `json:"name"`
	Method       string          `json:"method"`
	URL          string          `json:"url"`
	Request      json.RawMessage `json:"request"`
	Sent         json.RawMessage `json:"sent"`
	Response     struct {
		StatusCode int               `json:"statusCode"`
		StatusText string            `json:"statusText"`
		DurationMs int64             `json:"durationMs"`
		Size       int64             `json:"size"`
		Headers    map[string]string `json:"headers"`
		Body       string            `json:"body"`
		BodyBase64 bool              `json:"bodyBase64"`
	} `json:"response"`
}

type savedResponseSummary struct {
	models.SavedResponse
	AuthorName string `json:"authorName,omitempty"`
	CanDelete  bool   `json:"canDelete"`
}

type savedResponseDetail struct {
	savedResponseSummary
	Request         json.RawMessage   `json:"request"`
	Sent            json.RawMessage   `json:"sent,omitempty"`
	ResponseHeaders map[string]string `json:"responseHeaders"`
	ResponseBody    string            `json:"responseBody"`
}

func (h *SavedResponseHandler) canDelete(row models.SavedResponse, userID string) bool {
	return row.UserID == userID || canEditCollection(h.db, row.CollectionID, userID)
}

// List: GET /saved-responses?collections=id1,id2 or ?request=id — newest
// first, no bodies. request lists one saved request's responses wherever
// they were filed (collections is then ignored).
func (h *SavedResponseHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	rid := strings.TrimSpace(c.Query("request"))
	var ids []string
	for _, id := range strings.Split(c.Query("collections"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
		if len(ids) >= 300 {
			break
		}
	}
	out := []savedResponseSummary{}
	if len(ids) == 0 && rid == "" {
		c.JSON(http.StatusOK, gin.H{"data": out})
		return
	}
	// Access is decided on each collection's root; cache per root.
	allowedRoot := map[string]bool{}
	canSee := func(id string) bool {
		root := rootCollectionID(h.db, id)
		ok, seen := allowedRoot[root]
		if !seen {
			ok = ResolveCollectionRole(h.db, userID, root).AtLeast(ViewerRole)
			allowedRoot[root] = ok
		}
		return ok
	}
	q := h.db.Omit("request_json", "sent_json", "response_headers", "response_body")
	if rid != "" {
		q = q.Where("request_id = ?", rid)
	} else {
		var visible []string
		for _, id := range ids {
			if canSee(id) {
				visible = append(visible, id)
			}
		}
		if len(visible) == 0 {
			c.JSON(http.StatusOK, gin.H{"data": out})
			return
		}
		q = q.Where("collection_id IN ?", visible)
	}
	var found []models.SavedResponse
	if err := q.Order("created_at DESC").Limit(savedResponseMaxList).Find(&found).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list saved responses"})
		return
	}
	rows := found
	if rid != "" {
		rows = rows[:0]
		for _, r := range found {
			if canSee(r.CollectionID) {
				rows = append(rows, r)
			}
		}
	}
	names := h.authorNames(rows)
	editable := map[string]bool{}
	for _, r := range rows {
		del := r.UserID == userID
		if !del {
			root := rootCollectionID(h.db, r.CollectionID)
			v, seen := editable[root]
			if !seen {
				v = ResolveCollectionRole(h.db, userID, root).AtLeast(EditorRole)
				editable[root] = v
			}
			del = v
		}
		out = append(out, savedResponseSummary{SavedResponse: r, AuthorName: names[r.UserID], CanDelete: del})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *SavedResponseHandler) authorNames(rows []models.SavedResponse) map[string]string {
	ids := map[string]bool{}
	for _, r := range rows {
		ids[r.UserID] = true
	}
	list := make([]string, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	out := map[string]string{}
	if len(list) == 0 {
		return out
	}
	var users []struct{ ID, Email, Name string }
	h.db.Table("users").Select("id, email, name").Where("id IN ?", list).Scan(&users)
	for _, u := range users {
		if u.Name != "" {
			out[u.ID] = u.Name
		} else {
			out[u.ID] = u.Email
		}
	}
	return out
}

func (h *SavedResponseHandler) load(c *gin.Context) (*models.SavedResponse, bool) {
	var row models.SavedResponse
	if err := h.db.First(&row, "id = ?", c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Saved response not found"})
		return nil, false
	}
	if !canAccessCollection(h.db, row.CollectionID, c.GetString("userID")) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Saved response not found"})
		return nil, false
	}
	return &row, true
}

// Get: GET /saved-responses/:id — the full pair.
func (h *SavedResponseHandler) Get(c *gin.Context) {
	row, ok := h.load(c)
	if !ok {
		return
	}
	headers := map[string]string{}
	_ = json.Unmarshal([]byte(row.ResponseHeaders), &headers)
	req := json.RawMessage(row.RequestJSON)
	if len(req) == 0 {
		req = json.RawMessage("{}")
	}
	var sent json.RawMessage
	if row.SentJSON != "" {
		sent = json.RawMessage(row.SentJSON)
	}
	userID := c.GetString("userID")
	c.JSON(http.StatusOK, gin.H{"data": savedResponseDetail{
		savedResponseSummary: savedResponseSummary{SavedResponse: *row, AuthorName: h.authorNames([]models.SavedResponse{*row})[row.UserID], CanDelete: h.canDelete(*row, userID)},
		Request:              req, Sent: sent, ResponseHeaders: headers, ResponseBody: row.ResponseBody,
	}})
}

// Create: POST /saved-responses.
func (h *SavedResponseHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")
	var in savedResponseInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !canAccessCollection(h.db, in.CollectionID, userID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Collection not found"})
		return
	}
	method := strings.ToUpper(strings.TrimSpace(in.Method))
	if method == "" {
		method = "GET"
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = strings.TrimSpace(method + " " + in.URL)
	}
	if r := []rune(name); len(r) > 250 {
		name = string(r[:250])
	}
	body := in.Response.Body
	truncated := false
	if len(body) > savedResponseMaxBody {
		body = body[:savedResponseMaxBody]
		truncated = true
	}
	hdrs, _ := json.Marshal(in.Response.Headers)
	row := models.SavedResponse{
		ID: uuid.New().String(), UserID: userID, CollectionID: in.CollectionID, RequestID: in.RequestID,
		Name: name, Method: method, URL: in.URL,
		RequestJSON: string(in.Request), SentJSON: string(in.Sent),
		StatusCode: in.Response.StatusCode, StatusText: in.Response.StatusText, DurationMs: in.Response.DurationMs,
		ResponseSize: in.Response.Size, ResponseHeaders: string(hdrs), ResponseBody: body,
		BodyBase64: in.Response.BodyBase64, Truncated: truncated, CreatedAt: time.Now(),
	}
	if len(row.RequestJSON) > savedResponseMaxBody {
		c.JSON(http.StatusBadRequest, gin.H{"error": "request too large to save"})
		return
	}
	if err := h.db.Create(&row).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save response"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": savedResponseSummary{SavedResponse: row, CanDelete: true}})
}

// Rename: PATCH /saved-responses/:id {name}.
func (h *SavedResponseHandler) Rename(c *gin.Context) {
	row, ok := h.load(c)
	if !ok {
		return
	}
	if !h.canDelete(*row, c.GetString("userID")) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the author or a collection editor can rename it"})
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || strings.TrimSpace(in.Name) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	name := strings.TrimSpace(in.Name)
	if r := []rune(name); len(r) > 250 {
		name = string(r[:250])
	}
	if err := h.db.Model(row).Update("name", name).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "rename"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"name": name}})
}

// Delete: DELETE /saved-responses/:id.
func (h *SavedResponseHandler) Delete(c *gin.Context) {
	row, ok := h.load(c)
	if !ok {
		return
	}
	if !h.canDelete(*row, c.GetString("userID")) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the author or a collection editor can delete it"})
		return
	}
	if err := h.db.Delete(row).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"deleted": true}})
}

// ── keeping saved responses attached when collections change ─────────

// collectionSubtree returns id and the ids of every folder below it.
func collectionSubtree(db *gorm.DB, id string) []string {
	out := []string{id}
	for i := 0; i < len(out) && len(out) < 10000; i++ {
		var kids []string
		db.Table("collections").Where("parent_id = ?", out[i]).Pluck("id", &kids)
		out = append(out, kids...)
	}
	return out
}

// rehomeSavedResponsesOnDelete runs before a collection or folder is deleted:
// a folder's saved responses (its sub-folders' too) move up to its parent so
// nothing is lost; deleting a whole collection deletes them with it.
func rehomeSavedResponsesOnDelete(db *gorm.DB, c models.Collection) {
	ids := collectionSubtree(db, c.ID)
	q := db.Model(&models.SavedResponse{}).Where("collection_id IN ?", ids)
	if c.ParentID != nil && *c.ParentID != "" {
		q.Update("collection_id", *c.ParentID)
		return
	}
	db.Where("collection_id IN ?", ids).Delete(&models.SavedResponse{})
}

// savedResponseAnchor remembers where a saved response sat — folder path
// from the root and its request's name + method — so it can be re-attached
// after a re-import rebuilds the tree with new ids.
type savedResponseAnchor struct {
	ID, Path, ReqName, ReqMethod string
}

// folderPaths maps every folder under root (root itself = "") to its path.
func folderPaths(db *gorm.DB, rootID string) map[string]string {
	paths := map[string]string{rootID: ""}
	queue := []string{rootID}
	for len(queue) > 0 && len(paths) < 10000 {
		id := queue[0]
		queue = queue[1:]
		var kids []struct{ ID, Name string }
		db.Table("collections").Select("id, name").Where("parent_id = ?", id).Scan(&kids)
		for _, k := range kids {
			paths[k.ID] = paths[id] + "/" + k.Name
			queue = append(queue, k.ID)
		}
	}
	return paths
}

// snapshotSavedResponses records the anchors of every saved response under
// root before a sync / re-import wipes its folders and requests.
func snapshotSavedResponses(db *gorm.DB, rootID string) []savedResponseAnchor {
	paths := folderPaths(db, rootID)
	ids := make([]string, 0, len(paths))
	for id := range paths {
		ids = append(ids, id)
	}
	var rows []struct{ ID, CollectionID, RequestID string }
	db.Model(&models.SavedResponse{}).Select("id, collection_id, request_id").Where("collection_id IN ?", ids).Scan(&rows)
	if len(rows) == 0 {
		return nil
	}
	reqIDs := []string{}
	for _, r := range rows {
		if r.RequestID != "" {
			reqIDs = append(reqIDs, r.RequestID)
		}
	}
	reqs := map[string]struct{ Name, Method string }{}
	if len(reqIDs) > 0 {
		var rr []struct{ ID, Name, Method string }
		db.Table("saved_requests").Select("id, name, method").Where("id IN ?", reqIDs).Scan(&rr)
		for _, r := range rr {
			reqs[r.ID] = struct{ Name, Method string }{r.Name, r.Method}
		}
	}
	out := make([]savedResponseAnchor, 0, len(rows))
	for _, r := range rows {
		a := savedResponseAnchor{ID: r.ID, Path: paths[r.CollectionID]}
		if q, ok := reqs[r.RequestID]; ok {
			a.ReqName, a.ReqMethod = q.Name, strings.ToUpper(q.Method)
		}
		out = append(out, a)
	}
	return out
}

// restoreSavedResponses re-attaches anchored saved responses to the rebuilt
// tree: the folder with the same path (else the root) and, in it, the
// request with the same name + method (else no request link).
func restoreSavedResponses(db *gorm.DB, rootID string, anchors []savedResponseAnchor) {
	if len(anchors) == 0 {
		return
	}
	byPath := map[string]string{}
	for id, p := range folderPaths(db, rootID) {
		byPath[p] = id
	}
	for _, a := range anchors {
		folder, ok := byPath[a.Path]
		if !ok {
			folder = rootID
		}
		reqID := ""
		if a.ReqName != "" {
			var r struct{ ID string }
			db.Table("saved_requests").Select("id").
				Where("collection_id = ? AND name = ? AND UPPER(method) = ?", folder, a.ReqName, a.ReqMethod).
				Limit(1).Scan(&r)
			reqID = r.ID
		}
		db.Model(&models.SavedResponse{}).Where("id = ?", a.ID).
			Updates(map[string]any{"collection_id": folder, "request_id": reqID})
	}
}
