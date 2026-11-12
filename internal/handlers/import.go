package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/realtime"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Postman Collection v2.1 JSON structures

type postmanCollection struct {
	Info postmanInfo   `json:"info"`
	Item []postmanItem `json:"item"`
}

type postmanInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Schema      string `json:"schema"`
	PostmanID   string `json:"_postman_id"`
}

type postmanItem struct {
	Name      string        `json:"name"`
	PostmanID string        `json:"_postman_id"`
	Item      []postmanItem `json:"item,omitempty"`    // present if folder
	Request   *postmanReq   `json:"request,omitempty"` // present if request
}

type postmanReq struct {
	Method string            `json:"method"`
	Header []postmanKV       `json:"header"`
	URL    postmanURL        `json:"url"`
	Body   *postmanBody      `json:"body"`
	Auth   *postmanAuth      `json:"auth"`
}

type postmanKV struct {
	Key      string `json:"key"`
	Value    string `json:"value"`
	Disabled bool   `json:"disabled"`
}

type postmanURL struct {
	Raw      string   `json:"raw"`
	Protocol string   `json:"protocol"`
	Host     []string `json:"host"`
	Path     []string `json:"path"`
	Query    []postmanKV `json:"query"`
}

type postmanBody struct {
	Mode    string          `json:"mode"` // raw, formdata, urlencoded, file, graphql
	Raw     string          `json:"raw"`
	Options *postmanBodyOpt `json:"options"`
}

type postmanBodyOpt struct {
	Raw struct {
		Language string `json:"language"`
	} `json:"raw"`
}

type postmanAuth struct {
	Type   string      `json:"type"`
	Bearer []postmanKV `json:"bearer"`
	Basic  []postmanKV `json:"basic"`
	APIKey []postmanKV `json:"apikey"`
}

// ImportPostman imports a Postman Collection v2.1 format
func (h *ImportHandler) ImportPostman(c *gin.Context) {
	userID := c.GetString("userID")

	var specBytes []byte
	var err error

	// Support both raw JSON body and multipart file upload
	contentType := c.ContentType()
	if strings.Contains(contentType, "multipart/form-data") {
		file, _, fileErr := c.Request.FormFile("file")
		if fileErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "No file provided"})
			return
		}
		defer file.Close()
		specBytes, err = io.ReadAll(file)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read uploaded file"})
			return
		}
	} else {
		specBytes, err = io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read request body"})
			return
		}
	}

	collection, count, err := h.importPostmanBytes(specBytes, userID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	notifyCollectionMutated(h.db, c, collection.ID, realtime.EventCollectionCreated)

	c.JSON(http.StatusCreated, gin.H{
		"data": gin.H{
			"collection": collection,
			"count":      count,
		},
	})
}

// ImportPostmanBulk imports multiple Postman Collection v2.1 files in one request.
// Accepts multipart form with repeated "files" field.
func (h *ImportHandler) ImportPostmanBulk(c *gin.Context) {
	userID := c.GetString("userID")

	form, err := c.MultipartForm()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to parse multipart form"})
		return
	}

	files := form.File["files"]
	if len(files) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No files provided"})
		return
	}

	type importedItem struct {
		Filename     string `json:"filename"`
		CollectionID string `json:"collectionId"`
		Name         string `json:"name"`
		RequestCount int    `json:"requestCount"`
	}
	type failedItem struct {
		Filename string `json:"filename"`
		Error    string `json:"error"`
	}

	imported := make([]importedItem, 0, len(files))
	failed := make([]failedItem, 0)

	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			failed = append(failed, failedItem{Filename: fh.Filename, Error: "failed to open file"})
			continue
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			failed = append(failed, failedItem{Filename: fh.Filename, Error: "failed to read file"})
			continue
		}

		col, count, err := h.importPostmanBytes(data, userID)
		if err != nil {
			failed = append(failed, failedItem{Filename: fh.Filename, Error: err.Error()})
			continue
		}
		imported = append(imported, importedItem{
			Filename:     fh.Filename,
			CollectionID: col.ID,
			Name:         col.Name,
			RequestCount: count,
		})
		notifyCollectionMutated(h.db, c, col.ID, realtime.EventCollectionCreated)
	}

	// Always 200 for a well-formed bulk request — per-file outcomes are in the body.
	// The request itself succeeded even if every file's content was invalid.
	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"imported": imported,
			"failed":   failed,
		},
	})
}

// ImportPostmanBytes is the exported wrapper around importPostmanBytes. Used
// by the GitLab integration import path so it can reuse the existing parser
// without duplicating the model-mapping logic.
func (h *ImportHandler) ImportPostmanBytes(specBytes []byte, userID string) (models.Collection, int, error) {
	return h.importPostmanBytes(specBytes, userID)
}

// ReimportPostmanBytes wipes an existing collection's requests + child
// folders and replays a Postman v2.1 payload into the same collection ID.
// Returns the new request count. Used by the GitLab "refresh from source"
// path so the collection identity (and any bindings/shares/activity) stays
// intact across resyncs.
func (h *ImportHandler) ReimportPostmanBytes(collectionID, userID string, specBytes []byte) (int, error) {
	var postmanColl postmanCollection
	if err := json.Unmarshal(specBytes, &postmanColl); err != nil {
		return 0, fmt.Errorf("failed to parse Postman collection: %v", err)
	}

	// Wipe requests + child folders under this collection. Mirrors the
	// CollectionHandler.Sync behaviour so refresh paths produce the same
	// shape as a fresh import.
	anchors := snapshotSavedResponses(h.db, collectionID)
	defer restoreSavedResponses(h.db, collectionID, anchors)
	h.db.Where("collection_id = ?", collectionID).Delete(&models.SavedRequest{})
	h.deleteCollectionChildren(collectionID)

	count := 0
	h.processPostmanItems(postmanColl.Item, collectionID, userID, &count)

	now := time.Now()
	updates := map[string]any{"last_synced": &now}
	if postmanColl.Info.Name != "" {
		updates["name"] = postmanColl.Info.Name
	}
	h.db.Model(&models.Collection{}).Where("id = ?", collectionID).Updates(updates)

	// Reimport wipes + replays the tree; recipients need to refetch
	// just like the Sync handler does. Background callers (GitLab webhook)
	// have no gin context — pass nil so the event still goes out without
	// an origin stamp.
	notifyCollectionMutated(h.db, nil, collectionID, realtime.EventCollectionUpdated)

	return count, nil
}

// deleteCollectionChildren is a small dup of CollectionHandler's private
// helper. Cheaper to copy 6 lines than to plumb a CollectionHandler reference
// through the GitLab integration just for this.
func (h *ImportHandler) deleteCollectionChildren(collectionID string) {
	var children []models.Collection
	h.db.Where("parent_id = ?", collectionID).Find(&children)
	for _, child := range children {
		h.db.Where("collection_id = ?", child.ID).Delete(&models.SavedRequest{})
		h.deleteCollectionChildren(child.ID)
		h.db.Delete(&child)
	}
}

// importPostmanBytes parses a single Postman v2.1 payload, creates the collection
// tree, and returns the created collection plus the imported request count.
func (h *ImportHandler) importPostmanBytes(specBytes []byte, userID string) (models.Collection, int, error) {
	var postmanColl postmanCollection
	if err := json.Unmarshal(specBytes, &postmanColl); err != nil {
		return models.Collection{}, 0, fmt.Errorf("failed to parse Postman collection: %v", err)
	}

	if postmanColl.Info.Name == "" {
		postmanColl.Info.Name = "Imported Postman Collection"
	}

	now := time.Now()
	collectionID := uuid.New().String()
	collection := models.Collection{
		ID:          collectionID,
		UserID:      userID,
		Name:        postmanColl.Info.Name,
		Description: postmanColl.Info.Description,
		Type:        "collection",
		SourceType:  "postman",
		LastSynced:  &now,
		GitItemID:   postmanColl.Info.PostmanID,
	}

	if err := h.db.Create(&collection).Error; err != nil {
		return models.Collection{}, 0, fmt.Errorf("failed to create collection")
	}

	requestCount := 0
	h.processPostmanItems(postmanColl.Item, collectionID, userID, &requestCount)

	h.db.Preload("Children").First(&collection, "id = ?", collectionID)
	return collection, requestCount, nil
}

// processPostmanItems recursively processes Postman items (folders and requests)
func (h *ImportHandler) processPostmanItems(items []postmanItem, parentID string, userID string, count *int) {
	for i, item := range items {
		if item.Item != nil && len(item.Item) > 0 {
			// This is a folder
			folderID := uuid.New().String()
			folder := models.Collection{
				ID:        folderID,
				UserID:    userID,
				Name:      item.Name,
				ParentID:  &parentID,
				Type:      "folder",
				SortOrder: i,
				GitItemID: item.PostmanID,
			}
			h.db.Create(&folder)

			// Recurse into sub-items
			h.processPostmanItems(item.Item, folderID, userID, count)
		}

		if item.Request != nil {
			// This is a request
			req := h.convertPostmanRequest(item, parentID, i)
			req.GitItemID = item.PostmanID
			h.db.Create(&req)
			*count++
		}
	}
}

// convertPostmanRequest converts a Postman request item into a PatchMan SavedRequest
func (h *ImportHandler) convertPostmanRequest(item postmanItem, collectionID string, sortOrder int) models.SavedRequest {
	pmReq := item.Request

	method := pmReq.Method
	if method == "" {
		method = "GET"
	}

	// Build URL - prefer raw URL
	reqURL := pmReq.URL.Raw
	if reqURL == "" {
		// Reconstruct from parts
		var parts []string
		protocol := pmReq.URL.Protocol
		if protocol == "" {
			protocol = "https"
		}
		host := strings.Join(pmReq.URL.Host, ".")
		path := strings.Join(pmReq.URL.Path, "/")
		if host != "" {
			parts = append(parts, protocol+"://"+host)
			if path != "" {
				parts = append(parts, path)
			}
			reqURL = strings.Join(parts, "/")
		}
	}

	// Convert headers
	headers := make([]map[string]interface{}, 0)
	for _, h := range pmReq.Header {
		headers = append(headers, map[string]interface{}{
			"key":     h.Key,
			"value":   h.Value,
			"enabled": !h.Disabled,
		})
	}
	headersJSON, _ := json.Marshal(headers)

	// Convert query params
	params := make([]map[string]interface{}, 0)
	for _, q := range pmReq.URL.Query {
		params = append(params, map[string]interface{}{
			"key":     q.Key,
			"value":   q.Value,
			"enabled": !q.Disabled,
		})
	}
	paramsJSON, _ := json.Marshal(params)

	// Convert body
	bodyType := "none"
	bodyContent := ""
	if pmReq.Body != nil {
		switch pmReq.Body.Mode {
		case "raw":
			bodyContent = pmReq.Body.Raw
			// Determine content type from options
			if pmReq.Body.Options != nil {
				switch pmReq.Body.Options.Raw.Language {
				case "json":
					bodyType = "json"
				case "xml":
					bodyType = "xml"
				default:
					bodyType = "raw"
				}
			} else {
				bodyType = "raw"
			}
		case "urlencoded":
			bodyType = "form"
		case "formdata":
			bodyType = "form"
		case "graphql":
			bodyType = "graphql"
			bodyContent = pmReq.Body.Raw
		default:
			bodyType = "raw"
			bodyContent = pmReq.Body.Raw
		}
	}
	bodyJSON, _ := json.Marshal(map[string]string{"type": bodyType, "content": bodyContent})

	// Convert auth
	authData := map[string]string{"type": "none"}
	if pmReq.Auth != nil {
		switch pmReq.Auth.Type {
		case "bearer":
			token := getPostmanKVValue(pmReq.Auth.Bearer, "token")
			authData = map[string]string{"type": "bearer", "token": token}
		case "basic":
			username := getPostmanKVValue(pmReq.Auth.Basic, "username")
			password := getPostmanKVValue(pmReq.Auth.Basic, "password")
			authData = map[string]string{"type": "basic", "username": username, "password": password}
		case "apikey":
			key := getPostmanKVValue(pmReq.Auth.APIKey, "key")
			value := getPostmanKVValue(pmReq.Auth.APIKey, "value")
			addTo := getPostmanKVValue(pmReq.Auth.APIKey, "in")
			if addTo == "" {
				addTo = "header"
			}
			authData = map[string]string{"type": "apikey", "key": key, "value": value, "addTo": addTo}
		}
	}
	authJSON, _ := json.Marshal(authData)

	return models.SavedRequest{
		ID:           uuid.New().String(),
		CollectionID: collectionID,
		Name:         item.Name,
		Method:       strings.ToUpper(method),
		URL:          reqURL,
		Headers:      string(headersJSON),
		Params:       string(paramsJSON),
		Body:         string(bodyJSON),
		Auth:         string(authJSON),
		SortOrder:    sortOrder,
	}
}

// getPostmanKVValue extracts a value from a Postman KV array by key
func getPostmanKVValue(kvs []postmanKV, key string) string {
	for _, kv := range kvs {
		if kv.Key == key {
			return kv.Value
		}
	}
	return ""
}

type ImportHandler struct {
	db *gorm.DB
}

func NewImportHandler(db *gorm.DB) *ImportHandler {
	return &ImportHandler{db: db}
}

// OpenAPI/Swagger JSON structures
type openAPISpec struct {
	Info     openAPIInfo                       `json:"info"`
	Paths    map[string]map[string]openAPIOp   `json:"paths"`
	Servers  []openAPIServer                   `json:"servers"`            // OpenAPI 3.0
	Host     string                            `json:"host"`              // Swagger 2.0
	BasePath string                            `json:"basePath"`          // Swagger 2.0
	Schemes  []string                          `json:"schemes"`           // Swagger 2.0
}

type openAPIServer struct {
	URL string `json:"url"`
}

// baseURL extracts the base URL from an OpenAPI/Swagger spec
func (s *openAPISpec) baseURL() string {
	// OpenAPI 3.0: use first server URL
	if len(s.Servers) > 0 && s.Servers[0].URL != "" {
		u := s.Servers[0].URL
		return strings.TrimRight(u, "/")
	}
	// Swagger 2.0: build from host + basePath
	if s.Host != "" {
		scheme := "https"
		if len(s.Schemes) > 0 {
			scheme = s.Schemes[0]
		}
		base := scheme + "://" + s.Host
		if s.BasePath != "" && s.BasePath != "/" {
			base += s.BasePath
		}
		return strings.TrimRight(base, "/")
	}
	return ""
}

type openAPIInfo struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Version     string `json:"version"`
}

type openAPIOp struct {
	Summary     string                    `json:"summary"`
	OperationID string                    `json:"operationId"`
	Description string                    `json:"description"`
	Parameters  []openAPIParam            `json:"parameters"`
	// Tags groups operations under named buckets — most modern OpenAPI
	// specs (Swashbuckle, NSwag, FastAPI, NestJS, etc.) use this instead
	// of a directory layout, so the spec's "shape" lives entirely in
	// op.tags[]. We mirror that grouping into PatchMan folders so the
	// imported collection isn't a 240-row flat list.
	Tags []string `json:"tags"`
}

type openAPIParam struct {
	Name     string `json:"name"`
	In       string `json:"in"`    // query, header, path
	Required bool   `json:"required"`
}

func (h *ImportHandler) ImportSwagger(c *gin.Context) {
	userID := c.GetString("userID")

	var specBytes []byte
	var err error
	var urlField string

	// Three input shapes, in priority order:
	//
	//  1. multipart/form-data file upload    — drag-and-drop a .json
	//  2. multipart form `url` field         — server fetches the URL
	//  3. raw JSON body (Content-Type: application/json) — the FE has
	//     already resolved a Scalar/Swagger UI/Redoc page through
	//     /api/import/resolve-spec and is sending the JSON straight
	//     here. Bundling parse+folder+request creation server-side
	//     means the FE makes ONE request instead of fanning out 200+
	//     `POST /api/requests` calls. The optional `?sourceUrl=…`
	//     query lets the FE record where the JSON came from so the
	//     Update Collection dialog can pre-fill that URL on next open.
	//
	// We also accept a `?workspaceId=…` query to match the
	// FE-parsing path: imports land in the workspace the user is
	// currently looking at, not the BE's default fallback.
	contentType := c.ContentType()
	if strings.Contains(contentType, "multipart/form-data") {
		file, _, fileErr := c.Request.FormFile("file")
		urlField = c.PostForm("url")
		switch {
		case fileErr == nil && file != nil:
			defer file.Close()
			specBytes, err = io.ReadAll(file)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read uploaded file"})
				return
			}
		case urlField != "":
			resp, fetchErr := http.Get(urlField)
			if fetchErr != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Failed to fetch URL: %v", fetchErr)})
				return
			}
			defer resp.Body.Close()
			specBytes, err = io.ReadAll(resp.Body)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read response from URL"})
				return
			}
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "Provide either a file upload or a URL"})
			return
		}
	} else {
		// Raw JSON body (or anything else with a body) — read the spec
		// directly. Optional ?sourceUrl=… stamps where the user typed
		// it from on the FE so Update Collection can pre-fill.
		specBytes, err = io.ReadAll(c.Request.Body)
		if err != nil || len(specBytes) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Empty request body — send the OpenAPI JSON, a multipart file, or a `url` field"})
			return
		}
		urlField = c.Query("sourceUrl")
	}

	// Parse JSON
	var spec openAPISpec
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Failed to parse OpenAPI spec: %v", err)})
		return
	}

	if spec.Info.Title == "" {
		spec.Info.Title = "Imported API"
	}

	// Create collection
	now := time.Now()
	collectionID := uuid.New().String()

	// Resolve workspace the same way collection.Create does so a
	// JSON-body import lands in the workspace the FE currently shows,
	// not the user's oldest workspace.
	workspaceID := c.Query("workspaceId")
	if workspaceID == "__none__" {
		workspaceID = ""
	} else if workspaceID == "" {
		var ws models.Workspace
		if err := h.db.
			Joins("JOIN team_members ON team_members.workspace_id = workspaces.id").
			Where("team_members.user_id = ?", userID).
			Order("workspaces.created_at ASC").
			First(&ws).Error; err == nil {
			workspaceID = ws.ID
		}
	}

	collection := models.Collection{
		ID:          collectionID,
		UserID:      userID,
		WorkspaceID: workspaceID,
		Name:        spec.Info.Title,
		Description: spec.Info.Description,
		Type:        "collection",
		BaseURL:     spec.baseURL(),
		SourceURL:   urlField,
		SourceType:  "swagger",
		LastSynced:  &now,
	}

	if err := h.db.Create(&collection).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create collection"})
		return
	}

	// Create requests grouped by tag — see buildRequestsGroupedByTag for
	// rationale. Modern OpenAPI specs (Swashbuckle .NET, NSwag, FastAPI,
	// NestJS, etc.) carry their hierarchy in op.tags, not in directories,
	// so a flat import would dump 200+ rows at the root of the collection.
	count := buildRequestsGroupedByTag(h.db, spec, collectionID)

	// Load created collection with requests + child folders
	h.db.Preload("Children").First(&collection, "id = ?", collectionID)
	var requests []models.SavedRequest
	h.db.Where("collection_id = ?", collectionID).Order("sort_order ASC").Find(&requests)

	notifyCollectionMutated(h.db, c, collectionID, realtime.EventCollectionCreated)

	c.JSON(http.StatusCreated, gin.H{
		"data": gin.H{
			"collection": collection,
			"requests":   requests,
			"count":      count,
		},
	})
}
