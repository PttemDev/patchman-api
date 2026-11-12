package grafanaquery

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"patchman-be/internal/models"
)

// SearchHit is one folder or dashboard from /api/search. FolderUID is the
// parent folder ("" = General / root), which is enough to build the tree on
// every Grafana version — nested folders (10.3+) come back from the same
// search with their parent set (verified on 11.6 and 13.2), so no per-folder
// walk is needed.
type SearchHit struct {
	UID       string   `json:"uid"`
	Title     string   `json:"title"`
	Type      string   `json:"type"` // dash-db | dash-folder
	URL       string   `json:"url,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	FolderUID string   `json:"folderUid,omitempty"`
	Starred   bool     `json:"isStarred,omitempty"`
}

type SearchResult struct {
	Folders    []SearchHit `json:"folders"`
	Dashboards []SearchHit `json:"dashboards"`
}

const searchLimit = "5000"

// Search lists folders and dashboards (filtered by title when query is set).
// Folders are always listed in full so a match deep in the tree still shows
// its path.
func (r *Registry) Search(ctx context.Context, conn models.GrafanaConnection, query string) (*SearchResult, error) {
	out := &SearchResult{Folders: []SearchHit{}, Dashboards: []SearchHit{}}
	var folders []SearchHit
	if err := r.DoJSON(ctx, conn, http.MethodGet, "/api/search", url.Values{"type": {"dash-folder"}, "limit": {searchLimit}}, nil, &folders); err != nil {
		return nil, err
	}
	out.Folders = append(out.Folders, folders...)
	q := url.Values{"type": {"dash-db"}, "limit": {searchLimit}}
	if s := strings.TrimSpace(query); s != "" {
		q.Set("query", s)
	}
	var dashes []SearchHit
	if err := r.DoJSON(ctx, conn, http.MethodGet, "/api/search", q, nil, &dashes); err != nil {
		return nil, err
	}
	out.Dashboards = append(out.Dashboards, dashes...)
	return out, nil
}
