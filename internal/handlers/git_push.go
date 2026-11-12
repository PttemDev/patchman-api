package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"gorm.io/gorm"
)

// ----------------------------------------------------------------------
// Push entry point + DTOs
// ----------------------------------------------------------------------

type pushInput struct {
	// DryRun returns the patched JSON + diff without touching GitLab. The
	// FE uses this to populate the diff preview, then sends the same input
	// without DryRun to actually commit.
	DryRun bool `json:"dryRun"`

	CommitMessage string `json:"commitMessage"`
	MRTitle       string `json:"mrTitle"`
	MRDescription string `json:"mrDescription"`
	// Optional override; defaults to the binding's branch.
	TargetBranch string `json:"targetBranch"`
	// Skip the MR step and commit straight to TargetBranch. Off by default.
	DirectCommit bool `json:"directCommit"`
}

type pushOutput struct {
	DryRun     bool   `json:"dryRun"`
	NoChanges  bool   `json:"noChanges,omitempty"`
	OriginalJS string `json:"originalJs,omitempty"`
	PatchedJS  string `json:"patchedJs,omitempty"`
	Diff       string `json:"diff,omitempty"`

	// Set on real (non-dry) pushes.
	BranchName string `json:"branchName,omitempty"`
	CommitID   string `json:"commitId,omitempty"`
	MRURL      string `json:"mrUrl,omitempty"`
}

// Push patches the bound rawSnapshot with the user's current edits and
// either returns the diff (DryRun) or commits + opens an MR.
func (h *GitIntegrationHandler) Push(c *gin.Context) {
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
	if err := h.db.Where("collection_id = ? AND provider = ?", collectionID, "gitlab").First(&binding).Error; err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "collection is not bound to a GitLab source"})
		return
	}

	var in pushInput
	_ = c.ShouldBindJSON(&in)

	access, err := h.validAccessToken(userID)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": fmt.Sprintf("gitlab not linked: %v", err)})
		return
	}

	// One-time backfill for collections imported before _postman_id was
	// captured. Without this, every DB row looks "new" to the patcher and
	// the diff shows the entire file rebuilt. Idempotent: rows with a
	// non-empty GitItemID are left alone.
	h.backfillGitItemIDs(col.ID, &binding)

	// Build the patched JSON from the user's current DB state.
	patched, original, err := h.buildPatchedSnapshot(&col, &binding)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("build patched snapshot: %v", err)})
		return
	}

	out := pushOutput{
		DryRun:     in.DryRun,
		OriginalJS: original,
		PatchedJS:  patched,
		Diff:       unifiedDiff(original, patched, 3),
	}
	if original == patched {
		out.NoChanges = true
		c.JSON(http.StatusOK, gin.H{"data": out})
		return
	}

	if in.DryRun {
		c.JSON(http.StatusOK, gin.H{"data": out})
		return
	}

	// Conflict check — the file may have moved on the remote since import.
	currentBlobSha, err := h.fetchBlobSha(access, binding.ProjectID, binding.FilePath, binding.Branch)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("read remote blob sha: %v", err)})
		return
	}
	if currentBlobSha != "" && binding.LastSeenBlobSha != "" && currentBlobSha != binding.LastSeenBlobSha {
		c.JSON(http.StatusConflict, gin.H{"error": "remote has changed since you last pulled — refresh from GitLab and try again"})
		return
	}

	targetBranch := in.TargetBranch
	if targetBranch == "" {
		targetBranch = binding.Branch
	}

	commitMessage := in.CommitMessage
	if commitMessage == "" {
		commitMessage = "PatchMan: update " + binding.FilePath
	}

	branchName := targetBranch
	if !in.DirectCommit {
		username := h.usernameForUser(userID)
		branchName = fmt.Sprintf("patchman/%s-%d", slugify(username), time.Now().Unix())
		if err := h.createBranch(access, binding.ProjectID, branchName, binding.LastSeenCommitSha); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("create branch: %v", err)})
			return
		}
	}

	commitID, err := h.commitFile(access, binding.ProjectID, branchName, binding.FilePath, patched, commitMessage)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("commit: %v", err)})
		return
	}
	out.BranchName = branchName
	out.CommitID = commitID

	if !in.DirectCommit {
		title := in.MRTitle
		if title == "" {
			title = "PatchMan: " + col.Name
		}
		desc := in.MRDescription
		if desc == "" {
			desc = "Pushed from PatchMan."
		}
		mrURL, err := h.openMR(access, binding.ProjectID, branchName, targetBranch, title, desc)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("open MR: %v", err)})
			return
		}
		out.MRURL = mrURL
	}

	// Update binding to the just-pushed state so the next push doesn't
	// see a phantom remote move. blobSha is read from the commit response
	// where available; otherwise we re-query.
	if newBlobSha, err := h.fetchBlobSha(access, binding.ProjectID, binding.FilePath, branchName); err == nil {
		binding.LastSeenBlobSha = newBlobSha
	}
	binding.LastSeenCommitSha = commitID
	binding.RawSnapshot = patched
	_ = h.db.Save(&binding).Error

	c.JSON(http.StatusOK, gin.H{"data": out})
}

// ----------------------------------------------------------------------
// Patch logic — applies the user's current DB state on top of rawSnapshot
// ----------------------------------------------------------------------

// buildPatchedSnapshot starts from the byte-original rawSnapshot and applies
// path-targeted mutations using sjson. The result preserves every byte we
// did not touch — same key order, same whitespace, same tab-vs-space — so
// the GitLab MR diff shows only the user's actual changes instead of a
// reformat-the-world rewrite.
func (h *GitIntegrationHandler) buildPatchedSnapshot(root *models.Collection, binding *models.CollectionGitSource) (string, string, error) {
	original := binding.RawSnapshot
	patched := original

	tree, err := h.loadCollectionTree(root.ID)
	if err != nil {
		return "", "", err
	}

	// Top-level: rename the collection if the user changed it.
	snapName := gjson.Get(patched, "info.name").String()
	if root.Name != "" && snapName != root.Name {
		if next, setErr := sjson.Set(patched, "info.name", root.Name); setErr == nil {
			patched = next
		}
	}
	// Description is opt-in: only push when the user has put something in
	// the field. Empty descriptions don't override an existing snapshot
	// description (we cannot tell "user blanked it" from "user never set
	// it" with the current model).
	if root.Description != "" {
		snapDesc := gjson.Get(patched, "info.description").String()
		if snapDesc != root.Description {
			if next, setErr := sjson.Set(patched, "info.description", root.Description); setErr == nil {
				patched = next
			}
		}
	}

	patched, err = patchItemArrayRaw(patched, "item", tree)
	if err != nil {
		return "", "", err
	}
	return patched, original, nil
}

// patchItemArrayRaw reconciles the snapshot's item array at parentPath with
// the desired tree. Mutations are applied in an order that keeps array
// indices stable: recurse into matched folders first, edit fields on matched
// requests, delete unmatched items in reverse-index order, then append new
// items at the end.
//
// This intentionally does NOT reorder existing items. A reorder would force
// us to rebuild the whole array (defeating the diff-minimisation goal).
// Reorder support can land later as a separate pass driven by sortOrder.
func patchItemArrayRaw(j string, parentPath string, desired []*collectionNode) (string, error) {
	items := gjson.Get(j, parentPath).Array()
	matched := make([]int, len(desired))
	for i := range matched {
		matched[i] = -1
	}
	used := make([]bool, len(items))

	// Phase 1: id-match.
	for i, node := range desired {
		if node.GitItemID == "" {
			continue
		}
		for k, item := range items {
			if used[k] {
				continue
			}
			if item.Get("_postman_id").String() == node.GitItemID {
				matched[i] = k
				used[k] = true
				break
			}
		}
	}
	// Phase 2: name+kind match. Catches collections whose Postman files
	// don't carry _postman_id (so GitItemID would be empty even after
	// re-import) and cases where the user added an item with a unique
	// name — its old "match" is a name twin in the snapshot, not a
	// position twin.
	for i, node := range desired {
		if matched[i] >= 0 {
			continue
		}
		for k, item := range items {
			if used[k] {
				continue
			}
			isFolder := item.Get("item").Exists()
			if isFolder != node.IsFolder {
				continue
			}
			if item.Get("name").String() == node.Name {
				matched[i] = k
				used[k] = true
				break
			}
		}
	}

	// 1) Recurse into matched folders. Inner mutations don't shift this
	//    level's indices.
	for i, k := range matched {
		if k < 0 {
			continue
		}
		node := desired[i]
		if !node.IsFolder {
			continue
		}
		childPath := fmt.Sprintf("%s.%d.item", parentPath, k)
		next, err := patchItemArrayRaw(j, childPath, node.Children)
		if err != nil {
			return j, err
		}
		j = next
	}

	// 2) Field-edit matched, edited items.
	for i, k := range matched {
		if k < 0 {
			continue
		}
		node := desired[i]
		itemPath := fmt.Sprintf("%s.%d", parentPath, k)
		if node.IsFolder {
			if !node.unedited() && items[k].Get("name").String() != node.Name {
				if next, err := sjson.Set(j, itemPath+".name", node.Name); err == nil {
					j = next
				}
			}
			continue
		}
		if node.unedited() {
			continue
		}
		next, err := patchRequestRaw(j, itemPath, node)
		if err != nil {
			return j, err
		}
		j = next
	}

	// 3) Delete unmatched snapshot items, reverse order so earlier indices
	//    stay valid.
	var toDelete []int
	for k := range items {
		if !used[k] {
			toDelete = append(toDelete, k)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(toDelete)))
	for _, k := range toDelete {
		next, err := sjson.Delete(j, fmt.Sprintf("%s.%d", parentPath, k))
		if err != nil {
			return j, err
		}
		j = next
	}

	// 4) Append new desired items at the end of the parent array.
	for i, node := range desired {
		if matched[i] >= 0 {
			continue
		}
		newJSON, err := json.Marshal(buildNewItem(node))
		if err != nil {
			return j, err
		}
		next, err := sjson.SetRaw(j, parentPath+".-1", string(newJSON))
		if err != nil {
			return j, err
		}
		j = next
	}

	return j, nil
}

// patchRequestRaw mutates a single request item's fields in place. The path
// points at the item itself (not the request sub-object).
func patchRequestRaw(j, itemPath string, node *collectionNode) (string, error) {
	r := node.Request
	if r == nil {
		return j, nil
	}
	var err error
	if next, e := sjson.Set(j, itemPath+".name", node.Name); e == nil {
		j = next
	} else {
		err = e
	}
	if next, e := sjson.Set(j, itemPath+".request.method", strings.ToUpper(r.Method)); e == nil {
		j = next
	} else {
		err = e
	}

	// URL — only overwrite the raw form. Leaving host/path/query alone
	// keeps Postman's parsed view intact for unchanged URLs and is harmless
	// for changed ones (Postman re-parses raw on next open).
	if next, e := sjson.Set(j, itemPath+".request.url.raw", r.URL); e == nil {
		j = next
	} else {
		err = e
	}

	headers := decodeHeaderJSON(r.Headers)
	if next, e := sjson.Set(j, itemPath+".request.header", headers); e == nil {
		j = next
	} else {
		err = e
	}

	body := decodeBodyJSON(r.Body)
	if body != nil {
		if next, e := sjson.Set(j, itemPath+".request.body", body); e == nil {
			j = next
		} else {
			err = e
		}
	} else if gjson.Get(j, itemPath+".request.body").Exists() {
		if next, e := sjson.Delete(j, itemPath+".request.body"); e == nil {
			j = next
		} else {
			err = e
		}
	}

	auth := decodeAuthJSON(r.Auth)
	if auth != nil {
		if next, e := sjson.Set(j, itemPath+".request.auth", auth); e == nil {
			j = next
		} else {
			err = e
		}
	} else if gjson.Get(j, itemPath+".request.auth").Exists() {
		if next, e := sjson.Delete(j, itemPath+".request.auth"); e == nil {
			j = next
		} else {
			err = e
		}
	}

	return j, err
}

// collectionNode is the internal flat shape we walk during patching. It
// distinguishes folders (with children) from requests (with method/url/etc).
type collectionNode struct {
	IsFolder  bool
	GitItemID string
	Name      string
	SortOrder int
	CreatedAt time.Time
	UpdatedAt time.Time
	// Folder
	Children []*collectionNode
	// Request
	Request *models.SavedRequest
}

// unedited reports whether the row was untouched after import. GORM stamps
// CreatedAt and UpdatedAt to the same time on Create; any subsequent Save
// bumps UpdatedAt. Tolerance of 1 second covers clock skew + the small gap
// between the GORM Create call and the actual DB write.
func (n *collectionNode) unedited() bool {
	if n.CreatedAt.IsZero() || n.UpdatedAt.IsZero() {
		return false
	}
	return n.UpdatedAt.Sub(n.CreatedAt) < time.Second
}

func (h *GitIntegrationHandler) loadCollectionTree(rootID string) ([]*collectionNode, error) {
	// Load every folder + request once and build the tree in memory. Cheaper
	// than recursive queries and the trees are small.
	var folders []models.Collection
	if err := h.db.Where("parent_id = ? OR (id = ? AND type = 'folder')", rootID, rootID).Find(&folders).Error; err != nil {
		return nil, err
	}
	// Recurse: we need ALL descendants. Easiest with a BFS.
	allFolders := []models.Collection{}
	queue := []string{rootID}
	for len(queue) > 0 {
		var current []string
		for _, p := range queue {
			var kids []models.Collection
			h.db.Where("parent_id = ?", p).Find(&kids)
			allFolders = append(allFolders, kids...)
			for _, k := range kids {
				current = append(current, k.ID)
			}
		}
		queue = current
	}
	// Index folders by parent for tree assembly.
	byParent := map[string][]*collectionNode{}
	folderByID := map[string]*collectionNode{}
	for i := range allFolders {
		f := allFolders[i]
		node := &collectionNode{
			IsFolder:  true,
			GitItemID: f.GitItemID,
			Name:      f.Name,
			SortOrder: f.SortOrder,
			CreatedAt: f.CreatedAt,
			UpdatedAt: f.UpdatedAt,
		}
		folderByID[f.ID] = node
		parent := ""
		if f.ParentID != nil {
			parent = *f.ParentID
		}
		byParent[parent] = append(byParent[parent], node)
		// Pair node with the underlying collection for child lookup later.
		nodeFolderID[node] = f.ID
	}

	var allReqs []models.SavedRequest
	collectionIDs := []string{rootID}
	for id := range folderByID {
		collectionIDs = append(collectionIDs, id)
	}
	if err := h.db.Where("collection_id IN ?", collectionIDs).Find(&allReqs).Error; err != nil {
		return nil, err
	}
	for i := range allReqs {
		r := allReqs[i]
		node := &collectionNode{
			IsFolder:  false,
			GitItemID: r.GitItemID,
			Name:      r.Name,
			SortOrder: r.SortOrder,
			CreatedAt: r.CreatedAt,
			UpdatedAt: r.UpdatedAt,
			Request:   &allReqs[i],
		}
		byParent[r.CollectionID] = append(byParent[r.CollectionID], node)
	}

	// Assemble children for folders, recursively.
	for node, fid := range nodeFolderID {
		node.Children = byParent[fid]
		sortNodes(node.Children)
	}
	rootChildren := byParent[rootID]
	sortNodes(rootChildren)
	return rootChildren, nil
}

// nodeFolderID maps a folder collectionNode to its DB id during one tree
// build — kept in a function-local var that loadCollectionTree resets.
var nodeFolderID = map[*collectionNode]string{}

// backfillGitItemIDs walks the snapshot tree alongside the DB tree (matched
// by sortOrder + name + folder/request type) and copies missing _postman_id
// values into DB rows. Necessary for collections imported before the import
// path captured _postman_id.
//
// On the second push the rows already carry GitItemID and this is a no-op.
func (h *GitIntegrationHandler) backfillGitItemIDs(rootID string, binding *models.CollectionGitSource) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(binding.RawSnapshot), &doc); err != nil {
		return
	}
	items, _ := doc["item"].([]any)

	// Backfill the root collection's GitItemID from info._postman_id.
	if info, ok := doc["info"].(map[string]any); ok {
		if pid, _ := info["_postman_id"].(string); pid != "" {
			h.db.Model(&models.Collection{}).
				Where("id = ? AND (git_item_id IS NULL OR git_item_id = '')", rootID).
				UpdateColumn("git_item_id", pid)
		}
	}

	h.backfillLevel(rootID, items)
}

func (h *GitIntegrationHandler) backfillLevel(parentID string, snapshotItems []any) {
	// DB children of this parent, ordered the way they were imported.
	var folders []models.Collection
	h.db.Where("parent_id = ?", parentID).Order("sort_order asc").Find(&folders)
	var requests []models.SavedRequest
	h.db.Where("collection_id = ?", parentID).Order("sort_order asc").Find(&requests)

	folderIdx := 0
	requestIdx := 0
	for _, raw := range snapshotItems {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		pid, _ := m["_postman_id"].(string)
		if _, isFolder := m["item"]; isFolder {
			if folderIdx < len(folders) {
				f := &folders[folderIdx]
				if pid != "" && f.GitItemID == "" {
					h.db.Model(f).UpdateColumn("git_item_id", pid)
					f.GitItemID = pid
				}
				// Recurse into the folder's children.
				if children, ok := m["item"].([]any); ok {
					h.backfillLevel(f.ID, children)
				}
				folderIdx++
			}
		} else if _, isRequest := m["request"]; isRequest {
			if requestIdx < len(requests) {
				r := &requests[requestIdx]
				if pid != "" && r.GitItemID == "" {
					h.db.Model(r).UpdateColumn("git_item_id", pid)
				}
				requestIdx++
			}
		}
	}
}

func sortNodes(ns []*collectionNode) {
	sort.SliceStable(ns, func(i, j int) bool { return ns[i].SortOrder < ns[j].SortOrder })
}

// patchSnapshotRoot mutates `doc` (a parsed Postman v2.1 JSON) so that its
// info.name / info.description / item[] reflect `root` and `tree`.
func patchSnapshotRoot(doc map[string]any, root *models.Collection, tree []*collectionNode) {
	info, _ := doc["info"].(map[string]any)
	if info == nil {
		info = map[string]any{}
		doc["info"] = info
	}
	info["name"] = root.Name
	if root.Description != "" {
		info["description"] = root.Description
	}

	items, _ := doc["item"].([]any)
	doc["item"] = patchItemArray(items, tree)
}

// patchItemArray walks an item array from the snapshot and reconciles it
// with the desired tree. Order in the result follows `desired`.
//
// Match strategy, in order:
//  1. _postman_id equality — robust, survives reorders/renames
//  2. Positional fallback — for snapshot items missing _postman_id (some
//     hand-curated Postman files omit them), pair with the desired node at
//     the same index if neither has been claimed
//  3. New item — the desired node has no claim on any snapshot item,
//     append a freshly-built one
//
// Without (2) every push of a no-_postman_id collection rebuilds every
// item and produces a giant useless diff.
func patchItemArray(items []any, desired []*collectionNode) []any {
	type itemRef struct {
		idx int
		m   map[string]any
	}
	existingByID := map[string]itemRef{}
	for i, raw := range items {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["_postman_id"].(string)
		if id != "" {
			existingByID[id] = itemRef{idx: i, m: m}
		}
	}

	claimed := make([]bool, len(items))
	out := make([]any, 0, len(desired))

	// Phase 1: id-match — the only fully reliable strategy. Survives
	// reorders, renames, sibling additions/deletions.
	idMatched := make([]map[string]any, len(desired))
	for nodeIdx, node := range desired {
		if node.GitItemID == "" {
			continue
		}
		if ref, ok := existingByID[node.GitItemID]; ok {
			idMatched[nodeIdx] = ref.m
			claimed[ref.idx] = true
		}
	}

	// Phase 2: build a name+kind index of *unclaimed* snapshot items so
	// nodes without a usable GitItemID (legacy imports from Postman files
	// that omit _postman_id, or freshly-added rows) can find their twin
	// without falling back to position. Position is dangerous: inserting
	// or deleting a sibling shifts every later node, causing every shifted
	// slot to look "rewritten" in the diff.
	remaining := map[string]int{}
	for i, raw := range items {
		if claimed[i] {
			continue
		}
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		if name == "" {
			continue
		}
		key := nameKey(name, m)
		// First write wins — duplicates are rare and ambiguity-skewing
		// either way; sticking to first keeps it deterministic.
		if _, exists := remaining[key]; !exists {
			remaining[key] = i
		}
	}

	for nodeIdx, node := range desired {
		existing := idMatched[nodeIdx]
		if existing == nil {
			kind := "req"
			if node.IsFolder {
				kind = "fold"
			}
			key := kind + ":" + node.Name
			if i, ok := remaining[key]; ok {
				if m, ok := items[i].(map[string]any); ok && itemKindMatches(m, node) {
					existing = m
					claimed[i] = true
					delete(remaining, key)
				}
			}
		}
		if existing == nil {
			out = append(out, buildNewItem(node))
			continue
		}
		out = append(out, patchExistingItem(existing, node))
	}
	return out
}

func nameKey(name string, m map[string]any) string {
	if _, isFolder := m["item"]; isFolder {
		return "fold:" + name
	}
	return "req:" + name
}

// itemKindMatches checks that a snapshot item is the same flavour as a
// desired node — folder vs request. Postman v2.1 folders carry an `item`
// array; requests carry a `request` object. Mixing them in the positional
// fallback would produce nonsense.
func itemKindMatches(m map[string]any, node *collectionNode) bool {
	_, hasItem := m["item"]
	_, hasReq := m["request"]
	if node.IsFolder {
		return hasItem
	}
	return hasReq
}

// patchExistingItem mutates an existing snapshot item to match a desired
// node, preserving keys we don't model. Crucially, when the row was never
// edited after import (unedited()) we leave the snapshot item byte-identical
// — that's how `Push` produces a clean, focused diff instead of re-emitting
// the whole file every time.
func patchExistingItem(item map[string]any, node *collectionNode) map[string]any {
	if node.IsFolder {
		// Folder name only changes if the folder row itself was edited;
		// children get reconciled regardless because edits inside them
		// must propagate up.
		if !node.unedited() {
			item["name"] = node.Name
		}
		children, _ := item["item"].([]any)
		item["item"] = patchItemArray(children, node.Children)
		return item
	}
	// Request. If untouched, return the snapshot item exactly as-is so
	// fields we don't model (event, description, response, etc.) — and
	// even fields we DO model but format differently — survive verbatim.
	if node.unedited() {
		return item
	}
	item["name"] = node.Name
	req, _ := item["request"].(map[string]any)
	if req == nil {
		req = map[string]any{}
	}
	patchRequestFields(req, node.Request)
	item["request"] = req
	return item
}

// buildNewItem creates a fresh Postman item for a node that did not exist
// in the snapshot (i.e. the user added it after import).
func buildNewItem(node *collectionNode) map[string]any {
	id := node.GitItemID
	if id == "" {
		id = uuid.NewString()
	}
	if node.IsFolder {
		children := make([]any, 0, len(node.Children))
		for _, ch := range node.Children {
			children = append(children, buildNewItem(ch))
		}
		return map[string]any{
			"name":         node.Name,
			"_postman_id":  id,
			"item":         children,
		}
	}
	req := map[string]any{}
	patchRequestFields(req, node.Request)
	return map[string]any{
		"name":        node.Name,
		"_postman_id": id,
		"request":     req,
	}
}

// patchRequestFields applies the SavedRequest fields onto a postman
// `request` map. This is the lossiest part of the round-trip — we replace
// fields we model and leave the rest alone.
func patchRequestFields(req map[string]any, r *models.SavedRequest) {
	if r == nil {
		return
	}
	req["method"] = strings.ToUpper(r.Method)

	// URL — write the raw form. Postman accepts both `"url": "https://…"`
	// and the parsed object form. Whichever was present we replace with the
	// raw string + a normalized object so editors agree.
	req["url"] = map[string]any{"raw": r.URL}

	// Headers — JSON-encoded array of {key,value,enabled,description}.
	req["header"] = decodeHeaderJSON(r.Headers)

	// Body — only emit if non-empty so we don't synthesise an empty body
	// where there was none.
	if body := decodeBodyJSON(r.Body); body != nil {
		req["body"] = body
	} else {
		delete(req, "body")
	}

	// Auth — same: only emit if user picked something other than 'none'.
	if auth := decodeAuthJSON(r.Auth); auth != nil {
		req["auth"] = auth
	} else {
		delete(req, "auth")
	}
}

func decodeHeaderJSON(s string) []any {
	if strings.TrimSpace(s) == "" {
		return []any{}
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(s), &arr); err != nil {
		return []any{}
	}
	out := make([]any, 0, len(arr))
	for _, h := range arr {
		key, _ := h["key"].(string)
		if key == "" {
			continue
		}
		enabled := true
		if v, ok := h["enabled"].(bool); ok {
			enabled = v
		}
		entry := map[string]any{
			"key":   key,
			"value": fmt.Sprint(h["value"]),
		}
		if !enabled {
			entry["disabled"] = true
		}
		if d, ok := h["description"].(string); ok && d != "" {
			entry["description"] = d
		}
		out = append(out, entry)
	}
	return out
}

func decodeBodyJSON(s string) map[string]any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var b map[string]any
	if err := json.Unmarshal([]byte(s), &b); err != nil {
		return nil
	}
	t, _ := b["type"].(string)
	content, _ := b["content"].(string)
	switch t {
	case "", "none":
		return nil
	case "json", "xml", "raw":
		out := map[string]any{"mode": "raw", "raw": content}
		if t == "json" {
			out["options"] = map[string]any{"raw": map[string]any{"language": "json"}}
		} else if t == "xml" {
			out["options"] = map[string]any{"raw": map[string]any{"language": "xml"}}
		}
		return out
	case "graphql":
		return map[string]any{"mode": "graphql", "graphql": map[string]any{"query": content}}
	case "form":
		// `content` is a JSON-encoded array of FormField {key,value,type}
		var fields []map[string]any
		if err := json.Unmarshal([]byte(content), &fields); err != nil {
			return nil
		}
		formdata := make([]any, 0, len(fields))
		for _, f := range fields {
			kv := map[string]any{"key": fmt.Sprint(f["key"]), "value": fmt.Sprint(f["value"]), "type": "text"}
			if t, _ := f["type"].(string); t == "file" {
				kv["type"] = "file"
			}
			formdata = append(formdata, kv)
		}
		return map[string]any{"mode": "formdata", "formdata": formdata}
	default:
		return map[string]any{"mode": "raw", "raw": content}
	}
}

func decodeAuthJSON(s string) map[string]any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var a map[string]any
	if err := json.Unmarshal([]byte(s), &a); err != nil {
		return nil
	}
	t, _ := a["type"].(string)
	if t == "" || t == "none" {
		return nil
	}
	out := map[string]any{"type": t}
	switch t {
	case "bearer":
		out["bearer"] = []any{map[string]any{"key": "token", "value": a["token"], "type": "string"}}
	case "basic":
		out["basic"] = []any{
			map[string]any{"key": "username", "value": a["username"], "type": "string"},
			map[string]any{"key": "password", "value": a["password"], "type": "string"},
		}
	case "apikey":
		out["apikey"] = []any{
			map[string]any{"key": "key", "value": a["key"], "type": "string"},
			map[string]any{"key": "value", "value": a["value"], "type": "string"},
			map[string]any{"key": "in", "value": a["addTo"], "type": "string"},
		}
	}
	return out
}

// ----------------------------------------------------------------------
// GitLab API — branch / commit / MR / blob sha
// ----------------------------------------------------------------------

func (h *GitIntegrationHandler) fetchBlobSha(access, projectID, filePath, branch string) (string, error) {
	endpoint := fmt.Sprintf(
		"%s/api/v4/projects/%s/repository/files/%s?ref=%s",
		strings.TrimRight(h.cfg.GitLabBaseURL, "/"),
		url.PathEscape(projectID),
		url.PathEscape(filePath),
		url.QueryEscape(branch),
	)
	req, _ := http.NewRequest("HEAD", endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := h.http.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil // file does not exist on this branch yet
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("blob sha %d", resp.StatusCode)
	}
	return resp.Header.Get("X-Gitlab-Blob-Id"), nil
}

func (h *GitIntegrationHandler) createBranch(access, projectID, branch, ref string) error {
	endpoint := fmt.Sprintf("%s/api/v4/projects/%s/repository/branches",
		strings.TrimRight(h.cfg.GitLabBaseURL, "/"),
		url.PathEscape(projectID),
	)
	form := url.Values{}
	form.Set("branch", branch)
	form.Set("ref", ref)
	req, _ := http.NewRequest("POST", endpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := h.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%d %s", resp.StatusCode, string(body))
	}
	return nil
}

type commitAction struct {
	Action   string `json:"action"`    // create|update|delete|move
	FilePath string `json:"file_path"`
	Content  string `json:"content,omitempty"`
}

type commitRequest struct {
	Branch        string         `json:"branch"`
	CommitMessage string         `json:"commit_message"`
	Actions       []commitAction `json:"actions"`
}

type commitResponse struct {
	ID string `json:"id"`
}

func (h *GitIntegrationHandler) commitFile(access, projectID, branch, filePath, content, message string) (string, error) {
	endpoint := fmt.Sprintf("%s/api/v4/projects/%s/repository/commits",
		strings.TrimRight(h.cfg.GitLabBaseURL, "/"),
		url.PathEscape(projectID),
	)
	body, _ := json.Marshal(commitRequest{
		Branch:        branch,
		CommitMessage: message,
		Actions:       []commitAction{{Action: "update", FilePath: filePath, Content: content}},
	})
	req, _ := http.NewRequest("POST", endpoint, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("%d %s", resp.StatusCode, string(respBody))
	}
	var cr commitResponse
	if err := json.Unmarshal(respBody, &cr); err != nil {
		return "", err
	}
	return cr.ID, nil
}

type mrRequest struct {
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	Title        string `json:"title"`
	Description  string `json:"description,omitempty"`
}

type mrResponse struct {
	WebURL string `json:"web_url"`
}

func (h *GitIntegrationHandler) openMR(access, projectID, source, target, title, desc string) (string, error) {
	endpoint := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests",
		strings.TrimRight(h.cfg.GitLabBaseURL, "/"),
		url.PathEscape(projectID),
	)
	body, _ := json.Marshal(mrRequest{SourceBranch: source, TargetBranch: target, Title: title, Description: desc})
	req, _ := http.NewRequest("POST", endpoint, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("%d %s", resp.StatusCode, string(respBody))
	}
	var mr mrResponse
	if err := json.Unmarshal(respBody, &mr); err != nil {
		return "", err
	}
	return mr.WebURL, nil
}

// ----------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------

func (h *GitIntegrationHandler) usernameForUser(userID string) string {
	var integ models.UserGitIntegration
	if err := h.db.Where("user_id = ? AND provider = ?", userID, "gitlab").First(&integ).Error; err == nil {
		if integ.GitLabUsername != "" {
			return integ.GitLabUsername
		}
	}
	var u models.User
	if err := h.db.Where("id = ?", userID).First(&u).Error; err == nil {
		// Prefer the local part of the email as a slug source.
		if at := strings.IndexByte(u.Email, '@'); at > 0 {
			return u.Email[:at]
		}
		return u.Name
	}
	return "user"
}

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	out := make([]byte, 0, len(s))
	prevDash := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			out = append(out, c)
			prevDash = false
		} else if !prevDash {
			out = append(out, '-')
			prevDash = true
		}
	}
	return strings.Trim(string(out), "-")
}

// unifiedDiff produces a small unified diff (rough — line-based, no
// minimum-edit detection). Good enough for the FE preview pane; we are not
// trying to be `git diff`.
func unifiedDiff(a, b string, ctx int) string {
	if a == b {
		return ""
	}
	la := strings.Split(a, "\n")
	lb := strings.Split(b, "\n")
	// Naive shared-prefix / shared-suffix trim, then mark middle as
	// changed. Sufficient for human review of patch JSON.
	prefix := 0
	for prefix < len(la) && prefix < len(lb) && la[prefix] == lb[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(la)-prefix && suffix < len(lb)-prefix && la[len(la)-1-suffix] == lb[len(lb)-1-suffix] {
		suffix++
	}
	var buf bytes.Buffer
	startA := max0(prefix - ctx)
	startB := max0(prefix - ctx)
	endA := min(len(la), len(la)-suffix+ctx)
	endB := min(len(lb), len(lb)-suffix+ctx)
	fmt.Fprintf(&buf, "@@ -%d,%d +%d,%d @@\n", startA+1, endA-startA, startB+1, endB-startB)
	for i := startA; i < prefix; i++ {
		buf.WriteString(" " + la[i] + "\n")
	}
	for i := prefix; i < len(la)-suffix; i++ {
		buf.WriteString("-" + la[i] + "\n")
	}
	for i := prefix; i < len(lb)-suffix; i++ {
		buf.WriteString("+" + lb[i] + "\n")
	}
	for i := len(la) - suffix; i < endA; i++ {
		buf.WriteString(" " + la[i] + "\n")
	}
	return buf.String()
}

func max0(x int) int {
	if x < 0 {
		return 0
	}
	return x
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// _ keeps gorm import for some files that compile in the same package; the
// integration handler structurally depends on *gorm.DB via h.db.
var _ = gorm.ErrRecordNotFound
var _ = errors.New
