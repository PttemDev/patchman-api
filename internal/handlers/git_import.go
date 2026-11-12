package handlers

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
)

// gitImportInput accepts either a full GitLab URL (preferred — matches what
// teams send each other) or its parsed pieces. URL takes precedence when both
// are sent.
type gitImportInput struct {
	URL       string `json:"url"`
	ProjectID string `json:"projectId"`
	Branch    string `json:"branch"`
	FilePath  string `json:"filePath"`
}

// Import fetches a Postman v2.1 collection file from GitLab using the user's
// linked OAuth token, hands it to the existing Postman import pipeline, and
// records a CollectionGitSource binding so the resulting collection can be
// pushed back later.
func (h *GitIntegrationHandler) Import(c *gin.Context) {
	userID := c.GetString("userID")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "auth required"})
		return
	}

	var in gitImportInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	projectID, branch, filePath, err := h.resolveGitLabRef(in)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	access, err := h.validAccessToken(userID)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": fmt.Sprintf("gitlab not linked: %v", err)})
		return
	}

	file, err := h.fetchRepoFile(access, projectID, filePath, branch)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("fetch file: %v", err)})
		return
	}

	// Validate the payload looks like a Postman v2.1 collection before we
	// commit anything to the DB. Cheaper to fail early than half-import.
	if err := looksLikePostmanV21(file.content); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}

	imp := NewImportHandler(h.db)
	collection, count, err := imp.ImportPostmanBytes(file.content, userID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Bind the collection to its remote source so push knows where to write.
	binding := models.CollectionGitSource{
		CollectionID:      collection.ID,
		Provider:          "gitlab",
		BaseURL:           h.cfg.GitLabBaseURL,
		ProjectID:         projectID,
		FilePath:          filePath,
		Branch:            branch,
		LastSeenCommitSha: file.commitID,
		LastSeenBlobSha:   file.blobID,
		RawSnapshot:       string(file.content),
	}
	if err := h.db.Save(&binding).Error; err != nil {
		// Best-effort: don't fail the import if we can't bind — the
		// collection itself has already landed and is usable.
		c.JSON(http.StatusOK, gin.H{"data": gin.H{
			"collection":  collection,
			"count":       count,
			"binding":     nil,
			"bindingErr":  err.Error(),
			"projectId":   projectID,
			"branch":      branch,
			"filePath":    filePath,
		}})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"data": gin.H{
		"collection": collection,
		"count":      count,
		"binding":    binding,
	}})
}

// Refresh pulls the latest version of the bound file from GitLab and
// re-imports it in place under the existing collection. Used by the
// "Update from Source" dialog when a collection has a GitLab binding.
func (h *GitIntegrationHandler) Refresh(c *gin.Context) {
	userID := c.GetString("userID")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "auth required"})
		return
	}
	collectionID := c.Param("id")

	// Verify the user owns the collection before doing any GitLab work.
	var col models.Collection
	if err := h.db.Where("id = ? AND user_id = ?", collectionID, userID).First(&col).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "collection not found"})
		return
	}

	var binding models.CollectionGitSource
	if err := h.db.Where("collection_id = ? AND provider = ?", collectionID, "gitlab").First(&binding).Error; err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "collection is not bound to a GitLab source"})
		return
	}

	access, err := h.validAccessToken(userID)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": fmt.Sprintf("gitlab not linked: %v", err)})
		return
	}

	file, err := h.fetchRepoFile(access, binding.ProjectID, binding.FilePath, binding.Branch)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("fetch file: %v", err)})
		return
	}
	if err := looksLikePostmanV21(file.content); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}

	imp := NewImportHandler(h.db)
	count, err := imp.ReimportPostmanBytes(collectionID, userID, file.content)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Update the binding so the next push doesn't see a phantom remote
	// move. The rawSnapshot replaces the in-memory baseline.
	binding.LastSeenCommitSha = file.commitID
	binding.LastSeenBlobSha = file.blobID
	binding.RawSnapshot = string(file.content)
	if err := h.db.Save(&binding).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "persist binding update"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"count":   count,
		"binding": binding,
	}})
}

// Binding returns the GitLab source binding for a collection, or null. Used
// by the FE to decide whether to show the GitLab tab as default in the
// Update dialog and to pre-fill the URL field.
func (h *GitIntegrationHandler) Binding(c *gin.Context) {
	userID := c.GetString("userID")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "auth required"})
		return
	}
	collectionID := c.Param("id")

	var col models.Collection
	if err := h.db.Where("id = ? AND user_id = ?", collectionID, userID).First(&col).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "collection not found"})
		return
	}

	var binding models.CollectionGitSource
	err := h.db.Where("collection_id = ? AND provider = ?", collectionID, "gitlab").First(&binding).Error
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"data": nil})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": binding})
}

// ----------------------------------------------------------------------
// URL parsing
// ----------------------------------------------------------------------

// gitLabBlobOrRawRE matches both /-/blob/ and /-/raw/ forms. We capture the
// repo path (group 1, "namespace/group/repo") and the ref+filePath tail
// (group 2). The trailing query string is ignored.
var gitLabBlobOrRawRE = regexp.MustCompile(`^https?://[^/]+/(.+?)/-/(?:blob|raw)/(.+)$`)

// resolveGitLabRef accepts either explicit pieces or a URL, and returns
// (projectId, branch, filePath). Branch is heuristic: we treat the first path
// segment after `/-/blob/` as the branch. That covers main/master/develop,
// the 99% case. Slash-bearing branches (`feature/foo`) need explicit fields.
func (h *GitIntegrationHandler) resolveGitLabRef(in gitImportInput) (string, string, string, error) {
	if in.URL == "" && in.ProjectID == "" {
		return "", "", "", errors.New("provide either url or projectId+branch+filePath")
	}
	if in.URL != "" {
		u, err := url.Parse(strings.TrimSpace(in.URL))
		if err != nil {
			return "", "", "", fmt.Errorf("invalid url: %v", err)
		}
		if expected := strings.TrimRight(strings.TrimPrefix(strings.TrimPrefix(h.cfg.GitLabBaseURL, "https://"), "http://"), "/"); expected != "" && u.Host != expected {
			return "", "", "", fmt.Errorf("url host %q is not the configured GitLab instance %q", u.Host, expected)
		}
		m := gitLabBlobOrRawRE.FindStringSubmatch(u.Scheme + "://" + u.Host + u.Path)
		if m == nil {
			return "", "", "", errors.New("URL does not look like a GitLab blob/raw link (expected /<group>/<repo>/-/blob/<branch>/<path>)")
		}
		project := m[1]
		tail := m[2]
		// First path segment of `tail` is the branch — see comment above.
		slash := strings.IndexByte(tail, '/')
		if slash <= 0 {
			return "", "", "", errors.New("URL is missing a file path after the branch")
		}
		branch := tail[:slash]
		path := tail[slash+1:]
		if path == "" {
			return "", "", "", errors.New("URL is missing a file path")
		}
		return project, branch, path, nil
	}
	if in.Branch == "" || in.FilePath == "" {
		return "", "", "", errors.New("branch and filePath are required when projectId is given without url")
	}
	return in.ProjectID, in.Branch, in.FilePath, nil
}

// ----------------------------------------------------------------------
// GitLab repository file fetch
// ----------------------------------------------------------------------

type repoFile struct {
	content   []byte
	commitID  string
	blobID    string
	filePath  string
	ref       string
	updatedAt time.Time
}

type repoFileResp struct {
	FilePath     string `json:"file_path"`
	BlobID       string `json:"blob_id"`
	CommitID     string `json:"commit_id"`
	LastCommitID string `json:"last_commit_id"`
	Encoding     string `json:"encoding"`
	Content      string `json:"content"`
	Ref          string `json:"ref"`
}

func (h *GitIntegrationHandler) fetchRepoFile(access, projectID, filePath, branch string) (*repoFile, error) {
	endpoint := fmt.Sprintf(
		"%s/api/v4/projects/%s/repository/files/%s?ref=%s",
		strings.TrimRight(h.cfg.GitLabBaseURL, "/"),
		url.PathEscape(projectID),
		url.PathEscape(filePath),
		url.QueryEscape(branch),
	)
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Accept", "application/json")
	resp, err := h.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("file not found at %s on branch %s", filePath, branch)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("gitlab files API %d: %s", resp.StatusCode, string(body))
	}
	var f repoFileResp
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("decode file response: %w", err)
	}
	if f.Encoding != "base64" {
		return nil, fmt.Errorf("unexpected encoding %q (want base64)", f.Encoding)
	}
	raw, err := base64.StdEncoding.DecodeString(f.Content)
	if err != nil {
		return nil, fmt.Errorf("decode base64 content: %w", err)
	}
	commitID := f.CommitID
	if commitID == "" {
		commitID = f.LastCommitID
	}
	return &repoFile{
		content:  raw,
		commitID: commitID,
		blobID:   f.BlobID,
		filePath: f.FilePath,
		ref:      f.Ref,
	}, nil
}

// ----------------------------------------------------------------------
// Cheap pre-flight to fail fast on non-Postman files
// ----------------------------------------------------------------------

func looksLikePostmanV21(b []byte) error {
	var probe struct {
		Info struct {
			Schema string `json:"schema"`
			Name   string `json:"name"`
		} `json:"info"`
		Item []json.RawMessage `json:"item"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return fmt.Errorf("file is not valid JSON: %v", err)
	}
	if probe.Info.Name == "" && len(probe.Item) == 0 {
		return errors.New("file does not look like a Postman v2.1 collection (missing info.name and item[])")
	}
	// Schema is informational; Postman docs say v2.1 lives at
	// schema.getpostman.com/json/collection/v2.1.0/. Accept anything that
	// has the shape — strict schema URL match is unnecessarily picky.
	return nil
}
