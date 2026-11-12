package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"patchman-be/internal/models"
	"patchman-be/internal/notecards"
	"patchman-be/internal/notesearch"
	"patchman-be/internal/realtime"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Agent memory: knowledge cards inside Notes (type "card"), typed links
// between notes (note_links), recall in one call and remember through a
// queue. Cards live in a memory folder (notes.memory): the caller's own
// "Agent Memory", or one a team shared with them. Each card records the
// agent that wrote it. The pure parts are in package notecards.

const agentMemoryFolder = "Agent Memory"

// StartCardWorkers runs the remember queue and the recall use counter, each
// as a single goroutine fed by a channel — the only writers of their rows,
// so nothing needs a lock. Called once at boot (tests drive processInbox
// directly instead).
func (h *NoteHandler) StartCardWorkers() {
	h.kick = make(chan struct{}, 1)
	h.uses = make(chan []string, 64)
	// A crash mid-item leaves it "processing"; hand it back to the queue.
	h.db.Model(&models.NoteInbox{}).Where("status = ?", "processing").Update("status", "pending")
	h.backfillBodyLinks()
	go func() {
		for {
			h.processInbox()
			select {
			case <-h.kick:
			case <-time.After(30 * time.Second):
			}
		}
	}()
	go func() {
		pending := map[string]float64{}
		flush := time.NewTicker(2 * time.Second)
		for {
			select {
			case ids := <-h.uses:
				for _, id := range ids {
					pending[id]++
				}
			case <-flush.C:
				for id, n := range pending {
					h.db.Model(&models.Note{}).Where("id = ?", id).UpdateColumn("use_count", gorm.Expr("use_count + ?", n))
				}
				pending = map[string]float64{}
			}
		}
	}()
}

// ── links ────────────────────────────────────────────────────────────────

// syncBodyLinks mirrors a body's [text](note:<id>) links into note_links
// (source "body"; "related" unless the link title names a type), replacing
// the previous set. A link the same pair already has with another source is
// left alone.
func syncBodyLinks(db *gorm.DB, noteID, body, actor string) {
	db.Where("src_note_id = ? AND source = ?", noteID, "body").Delete(&models.NoteLink{})
	for _, l := range notecards.BodyNoteLinks(body) {
		if l.ID == noteID {
			continue
		}
		db.Clauses(clause.OnConflict{DoNothing: true}).Create(&models.NoteLink{
			ID: uuid.New().String(), SrcNoteID: noteID, DstNoteID: l.ID, Type: l.Type,
			Weight: 1, Source: "body", CreatedBy: actor,
		})
	}
}

// backfillBodyLinks fills note_links from existing bodies once (the first
// boot with the table), so backlinks keep working for older notes.
func (h *NoteHandler) backfillBodyLinks() {
	var n int64
	h.db.Model(&models.NoteLink{}).Where("source = ?", "body").Count(&n)
	if n > 0 {
		return
	}
	var rows []struct{ ID, Body, UpdatedBy string }
	h.db.Table("notes").Select("id, body, updated_by").Where("deleted_at IS NULL AND body LIKE ?", "%note:%").Scan(&rows)
	for _, r := range rows {
		syncBodyLinks(h.db, r.ID, r.Body, r.UpdatedBy)
	}
}

// resolveNoteRef turns a note reference — full id, short id (with or
// without '#') — into a full id the caller can read.
func (h *NoteHandler) resolveNoteRef(userID, ref string) (string, string) {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "#")
	if !notecards.IsShortID(ref) {
		return ref, ""
	}
	var ids []string
	h.db.Model(&models.Note{}).Where("id LIKE ?", "%"+ref).Limit(20).Pluck("id", &ids)
	var readable []string
	for _, id := range ids {
		if canReadNote(h.db, userID, id) {
			readable = append(readable, id)
		}
	}
	switch len(readable) {
	case 0:
		return "", "Note not found"
	case 1:
		return readable[0], ""
	}
	return "", "ambiguous id, use one of: " + strings.Join(readable, ", ")
}

type linkInput struct {
	Dst    string   `json:"dst"`  // note id, short id or patchman:// URI
	Type   string   `json:"type"` // one of notecards.LinkTypes
	Weight *float64 `json:"weight"`
}

// AddLink adds a typed edge from a note. An agent (MCP) linking from a person's
// note doesn't change it: the edge becomes a suggestion. A "supersedes" edge
// marks its target stale (or suggests that, for a person's note).
func (h *NoteHandler) AddLink(c *gin.Context) {
	userID := c.GetString("userID")
	var in linkInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	src, msg := h.resolveNoteRef(userID, c.Param("id"))
	if msg != "" {
		c.JSON(http.StatusNotFound, gin.H{"error": msg})
		return
	}
	var srcNote models.Note
	if h.db.Where("id = ?", src).First(&srcNote).Error != nil || !canReadNote(h.db, userID, src) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Note not found"})
		return
	}
	weight := 1.0
	if in.Weight != nil && *in.Weight > 0 && *in.Weight <= 1 {
		weight = *in.Weight
	}
	r := h.addLink(c, userID, noteSource(c) == noteSourceMCP, noteAgent(c), srcNote, in.Dst, in.Type, weight)
	if r.err != "" {
		c.JSON(r.status, gin.H{"error": r.err})
		return
	}
	c.JSON(r.status, gin.H{"data": r.data})
}

// linkResult is what adding an edge did: an HTTP status with either data or
// an error message.
type linkResult struct {
	status int
	data   gin.H
	err    string
}

// addLink is the one place the link rules live — for the API, an accepted
// suggestion and the card splitter alike. fromAgent: the edge is an agent's
// idea (named agent, if known), so a person's note only gets a suggestion.
func (h *NoteHandler) addLink(c *gin.Context, actor string, fromAgent bool, agent string, srcNote models.Note, dstRef, typ string, weight float64) linkResult {
	src := srcNote.ID
	if !canEditNote(h.db, actor, src) {
		return linkResult{status: http.StatusForbidden, err: "You have read-only access to this note"}
	}
	typ = strings.TrimSpace(typ)
	if !notecards.ValidLinkType(typ) {
		return linkResult{status: http.StatusBadRequest, err: notecards.LinkTypeError(typ).Error()}
	}
	link := models.NoteLink{ID: uuid.New().String(), SrcNoteID: src, Type: typ, Weight: weight, CreatedBy: actor, Source: "human"}
	if fromAgent {
		link.Source = "agent"
	}
	var dstNote models.Note
	dstRef = strings.TrimSpace(dstRef)
	if notecards.IsEntityURI(dstRef) {
		link.DstURI = dstRef
	} else {
		dst, msg := h.resolveNoteRef(actor, dstRef)
		if msg != "" || dst == "" || h.db.Where("id = ?", dst).First(&dstNote).Error != nil || !canReadNote(h.db, actor, dst) {
			return linkResult{status: http.StatusBadRequest, err: "dst must be a note you can read or a patchman:// URI" + suffix(msg)}
		}
		if dst == src {
			return linkResult{status: http.StatusBadRequest, err: "a note can't link to itself"}
		}
		link.DstNoteID = dst
	}
	var exists int64
	h.db.Model(&models.NoteLink{}).Where("src_note_id = ? AND dst_note_id = ? AND dst_uri = ? AND type = ?",
		src, link.DstNoteID, link.DstURI, link.Type).Count(&exists)
	if exists > 0 {
		return linkResult{status: http.StatusOK, data: gin.H{"ok": true, "hint": "edge exists"}}
	}
	// A person's note is changed only by a person.
	if fromAgent && srcNote.Source != "agent" {
		h.suggest(srcNote, "link", actor, agent, gin.H{"dst": firstNonEmpty(link.DstNoteID, link.DstURI), "type": link.Type})
		return linkResult{status: http.StatusAccepted, data: gin.H{"suggested": true, "hint": "written by a person — added to the suggestions for them to accept"}}
	}
	if err := h.db.Create(&link).Error; err != nil {
		return linkResult{status: http.StatusInternalServerError, err: "save link"}
	}
	hint := ""
	if link.Type == "supersedes" && link.DstNoteID != "" {
		if fromAgent && dstNote.Source != "agent" || !canEditNote(h.db, actor, dstNote.ID) {
			h.suggest(dstNote, "supersede", actor, agent, gin.H{"by": src})
			hint = "the superseded note was written by a person — marking it stale is suggested to them"
		} else {
			h.db.Model(&models.Note{}).Where("id = ?", dstNote.ID).Updates(map[string]any{"stale": true, "superseded_by": src})
			notifyNote(h.db, c, dstNote.ID, realtime.EventNoteUpdated)
		}
	}
	notifyNote(h.db, c, src, realtime.EventNoteUpdated)
	out := gin.H{"ok": true, "id": link.ID}
	if hint != "" {
		out["hint"] = hint
	}
	return linkResult{status: http.StatusCreated, data: out}
}

// DeleteLink removes an edge: DELETE /notes/:id/links?dst=&type=.
func (h *NoteHandler) DeleteLink(c *gin.Context) {
	userID := c.GetString("userID")
	src, msg := h.resolveNoteRef(userID, c.Param("id"))
	if msg != "" || !canEditNote(h.db, userID, src) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Note not found"})
		return
	}
	dst := strings.TrimSpace(c.Query("dst"))
	q := h.db.Where("src_note_id = ? AND type = ?", src, c.Query("type"))
	if notecards.IsEntityURI(dst) {
		q = q.Where("dst_uri = ?", dst)
	} else {
		full, _ := h.resolveNoteRef(userID, dst)
		q = q.Where("dst_note_id = ?", full)
	}
	res := q.Delete(&models.NoteLink{})
	notifyNote(h.db, c, src, realtime.EventNoteUpdated)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"deleted": res.RowsAffected}})
}

type linkView struct {
	Type   string `json:"type"`
	NoteID string `json:"noteId,omitempty"`
	Title  string `json:"title,omitempty"`
	Icon   string `json:"icon,omitempty"`
	URI    string `json:"uri,omitempty"`
	Source string `json:"source"`
	Stale  bool   `json:"stale,omitempty"`
}

// Links lists a note's typed edges both ways, only towards notes the caller
// can read.
func (h *NoteHandler) Links(c *gin.Context) {
	userID := c.GetString("userID")
	id, msg := h.resolveNoteRef(userID, c.Param("id"))
	if msg != "" || !canReadNote(h.db, userID, id) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Note not found"})
		return
	}
	var rows []models.NoteLink
	h.db.Where("src_note_id = ? OR dst_note_id = ?", id, id).Order("created_at ASC").Limit(500).Find(&rows)
	out, in := []linkView{}, []linkView{}
	for _, l := range rows {
		other := l.DstNoteID
		if l.DstNoteID == id {
			other = l.SrcNoteID
		}
		v := linkView{Type: l.Type, Source: l.Source}
		if l.SrcNoteID == id && l.DstURI != "" {
			v.URI = l.DstURI
			out = append(out, v)
			continue
		}
		var n models.Note
		if h.db.Select("id", "title", "icon", "stale").Where("id = ?", other).First(&n).Error != nil || !canReadNote(h.db, userID, other) {
			continue
		}
		v.NoteID, v.Title, v.Icon, v.Stale = n.ID, n.Title, n.Icon, n.Stale
		if l.SrcNoteID == id {
			out = append(out, v)
		} else {
			in = append(in, v)
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"outgoing": out, "incoming": in}})
}

// LinksByURI lists the readable notes linked to a PatchMan entity
// (GET /notes/links/by-uri?uri=patchman://…), for the entity screens.
func (h *NoteHandler) LinksByURI(c *gin.Context) {
	userID := c.GetString("userID")
	uri := strings.TrimSpace(c.Query("uri"))
	out := []gin.H{}
	if !notecards.IsEntityURI(uri) {
		c.JSON(http.StatusOK, gin.H{"data": out})
		return
	}
	var rows []models.NoteLink
	h.db.Where("dst_uri = ?", uri).Order("created_at DESC").Limit(200).Find(&rows)
	for _, l := range rows {
		var n models.Note
		if h.db.Select("id", "title", "icon", "type", "summary", "stale").Where("id = ?", l.SrcNoteID).First(&n).Error != nil || !canReadNote(h.db, userID, n.ID) {
			continue
		}
		out = append(out, gin.H{"id": n.ID, "title": n.Title, "icon": n.Icon, "type": n.Type, "summary": n.Summary, "stale": n.Stale, "linkType": l.Type})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// ── suggestions ──────────────────────────────────────────────────────────

// suggest files a change for a person to accept, on the note's folder.
func (h *NoteHandler) suggest(n models.Note, kind, actor, agent string, payload gin.H) {
	b, _ := json.Marshal(payload)
	// Filed on the memory folder (a project card's too, so one inbox covers
	// a memory); outside agent memory, on the note's own folder.
	folder := h.memoryRoot(n.ParentID)
	if folder == "" && n.ParentID != nil {
		folder = *n.ParentID
	}
	h.db.Create(&models.NoteSuggestion{
		ID: uuid.New().String(), FolderID: folder, NoteID: n.ID, Kind: kind,
		Payload: string(b), Status: "open", CreatedBy: actor, Agent: agent,
	})
	notifyNote(h.db, nil, n.ID, realtime.EventNoteSuggestion)
}

// ── recall ───────────────────────────────────────────────────────────────

// Recall answers a question from the notes in one call: full-text hits
// ranked, the best three widened by one hop of links, printed within a token
// budget as plain lines (see notecards.Render). Stale notes are left out and
// named in a hint when a hit supersedes them.
func (h *NoteHandler) Recall(c *gin.Context) {
	userID := c.GetString("userID")
	q := strings.TrimSpace(c.Query("q"))
	budget, _ := strconv.Atoi(c.DefaultQuery("budget", "800"))
	if budget < 100 {
		budget = 100
	}
	if budget > 2000 {
		budget = 2000
	}
	project := notecards.ProjectKey(firstNonEmpty(c.Query("project"), noteProject(c)))
	// from: answer only from one person's notes (what they shared with the
	// caller — or the caller's own), "ask Ekrem's memory".
	var owners []string
	if from := strings.TrimSpace(c.Query("from")); from != "" {
		people := h.peopleNamed(from)
		if len(people) == 0 {
			c.JSON(http.StatusOK, gin.H{"data": gin.H{"text": "No one called \"" + from + "\" — check the name or email.", "ids": []string{}}})
			return
		}
		owners = make([]string, 0, len(people))
		for _, p := range people {
			owners = append(owners, p.ID)
		}
	}
	text, ids := h.recall(userID, q, budget, c.Query("scope"), project, owners)
	// Only the best hit is shown in full — that is the "use" counted.
	if len(ids) > 0 && h.uses != nil {
		select {
		case h.uses <- ids[:1]:
		default:
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"text": text, "ids": ids}})
}

// scopeFilter: "" or "all" for every readable note, "personal" or "team"
// for one kind of memory. project (a ProjectKey, "" if unknown) is the one
// the agent works in: its cards rank a little higher, and other projects'
// hits are tagged with their project.
func (h *NoteHandler) recall(userID, q string, budget int, scopeFilter, project string, owners []string) (string, []string) {
	if q == "" {
		return notecards.NoMatch, nil
	}
	cands, err := notesearch.RecallCandidatesOf(h.db, q, 200, owners)
	if err != nil {
		return notecards.NoMatch, nil
	}
	terms := notesearch.RecallTerms(q)
	projects := newProjectResolver(h.db)
	uses := map[string]float64{}
	hitProjects := map[string]string{}
	cols := []string{"id", "user_id", "parent_id", "type", "title", "summary", "body", "stale", "superseded_by", "use_count"}
	readable := map[string]bool{}
	canRead := func(n models.Note) bool {
		if v, ok := readable[n.ID]; ok {
			return v
		}
		v := n.UserID == userID || canReadNote(h.db, userID, n.ID)
		readable[n.ID] = v
		return v
	}
	scopes := newScopeResolver(h.db)
	if scopeFilter == "all" {
		scopeFilter = ""
	}
	ownerOK := map[string]bool{}
	for _, o := range owners {
		ownerOK[o] = true
	}
	inScope := func(n models.Note) bool {
		if owners != nil && !ownerOK[n.UserID] {
			return false
		}
		return scopeFilter == "" || scopes.scope(userID, n.ID).Kind == scopeFilter
	}
	toHit := func(n models.Note, score float64) notecards.Hit {
		line := n.Summary
		if line == "" {
			line = notesearch.Snippet(n.Body, terms, 140)
		}
		hit := notecards.Hit{ID: n.ID, Title: n.Title, Line: line, Body: n.Body, Scope: scopes.scope(userID, n.ID).Label(), Score: score}
		if p := projects.of(n.ParentID); p.key != "" {
			if p.key == project {
				hit.Score *= projectBoost
			} else {
				hit.Project = p.title
			}
			hitProjects[n.ID] = p.key
		}
		uses[n.ID] = n.UseCount
		return hit
	}
	var hits []notecards.Hit
	var hints []string
	seen := map[string]bool{}
	for rank, id := range cands {
		if len(hits) >= 30 {
			break
		}
		var n models.Note
		if h.db.Select(cols).Where("id = ?", id).First(&n).Error != nil || n.Type == "folder" || !canRead(n) || !inScope(n) {
			continue
		}
		if n.Stale {
			if n.SupersededBy != nil {
				hints = append(hints, "#"+notecards.ShortID(n.ID)+" is stale, superseded by #"+notecards.ShortID(*n.SupersededBy)+".")
			}
			continue
		}
		seen[n.ID] = true
		hits = append(hits, toHit(n, 1/float64(61+rank)))
	}
	// How often a note was recalled only breaks exact ties: as a score bonus
	// it reordered answers between two identical questions.
	byScore := func(list []notecards.Hit) func(i, j int) bool {
		return func(i, j int) bool {
			if list[i].Score != list[j].Score {
				return list[i].Score > list[j].Score
			}
			if uses[list[i].ID] != uses[list[j].ID] {
				return uses[list[i].ID] > uses[list[j].ID]
			}
			return list[i].ID < list[j].ID
		}
	}
	sort.SliceStable(hits, byScore(hits))

	// One hop out from the best three.
	top := len(hits)
	if top > 3 {
		top = 3
	}
	for i := 0; i < top; i++ {
		var links []models.NoteLink
		h.db.Where("src_note_id = ? OR dst_note_id = ?", hits[i].ID, hits[i].ID).Order("created_at ASC, id ASC").Limit(20).Find(&links)
		for _, l := range links {
			out := l.SrcNoteID == hits[i].ID
			if out && l.DstURI != "" {
				hits[i].Rels = append(hits[i].Rels, notecards.Rel{Out: true, Type: l.Type, Title: l.DstURI})
				continue
			}
			other := l.DstNoteID
			if !out {
				other = l.SrcNoteID
			}
			var n models.Note
			if h.db.Select(cols).Where("id = ?", other).First(&n).Error != nil || n.Type == "folder" || !canRead(n) {
				continue
			}
			hits[i].Rels = append(hits[i].Rels, notecards.Rel{Out: out, Type: l.Type, ID: n.ID, Title: n.Title})
			if l.Type == "contradicts" {
				hints = append(hints, "#"+notecards.ShortID(hits[i].ID)+" and #"+notecards.ShortID(n.ID)+" contradict; check which still holds.")
			}
			if !seen[n.ID] && !n.Stale && inScope(n) {
				seen[n.ID] = true
				hits = append(hits, toHit(n, hits[i].Score*l.Weight*0.5))
			}
		}
	}
	rest := hits[min(top, len(hits)):]
	sort.SliceStable(rest, byScore(rest))
	// Without a known project, a project tag only helps when the answer
	// spans several projects.
	if project == "" {
		distinct := map[string]bool{}
		for _, p := range hitProjects {
			distinct[p] = true
		}
		if len(distinct) < 2 {
			for i := range hits {
				hits[i].Project = ""
			}
		}
	}
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.ID)
	}
	return notecards.RenderFor(hits, budget, dedupe(hints), terms), ids
}

// ── remember ─────────────────────────────────────────────────────────────

type rememberInput struct {
	Text    string `json:"text"`
	Context string `json:"context"` // patchman:// URIs of what is open (linked as refers_to)
	Scope   string `json:"scope"`   // personal (default) | team
	Folder  string `json:"folderId"`
	Project string `json:"project"` // project name; MCP calls default to the agent's working directory
}

// rememberMaxRunes caps one memory (characters, not bytes: Turkish text
// isn't cut at half the length); rememberGoodRunes is the size a card reads
// best at — past it the memory is kept but the agent is told to split.
const (
	rememberMaxRunes  = 10000
	rememberGoodRunes = 4000
)

// rememberWait is how long remember() waits for the worker to file the card
// so it can answer with it; a slower filing (AI splitting) answers "queued".
const rememberWait = 3 * time.Second

// Remember queues text to become a card and answers at once; the worker
// files it in the caller's memory folder (or the team's).
func (h *NoteHandler) Remember(c *gin.Context) {
	userID := c.GetString("userID")
	var in rememberInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	in.Text = strings.TrimSpace(in.Text)
	if in.Text == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "text is required"})
		return
	}
	if n := utf8.RuneCountInString(in.Text); n > rememberMaxRunes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": fmt.Sprintf("Too long for one memory (%d characters, %d max). Split it into several remember calls — one fact, decision or root cause each — "+
			"or put the long text in a page with create_note and remember a short card that links to it with [[Page title]].", n, rememberMaxRunes)})
		return
	}
	if kind := notecards.SecretKind(in.Text + " " + in.Context); kind != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "This looks like it contains a secret (" + kind + "). Memories are shared and kept — leave the secret out and remember the rest."})
		return
	}
	folder, status, msg := h.memoryFolder(userID, in.Scope, in.Folder)
	if msg != "" {
		c.JSON(status, gin.H{"error": msg})
		return
	}
	// A memory root files the card under the project's folder; a project
	// folder passed as folderId already is one.
	if key := notecards.ProjectKey(firstNonEmpty(in.Project, noteProject(c))); key != "" && folder.Memory {
		pf, err := h.projectFolder(folder, key, firstNonEmpty(in.Project, noteProject(c)), userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "create project folder"})
			return
		}
		folder = pf
	}
	item := models.NoteInbox{ID: uuid.New().String(), UserID: userID, FolderID: folder.ID, Text: in.Text, Context: in.Context, Agent: noteAgent(c), Status: "pending"}
	done := make(chan struct{})
	h.waiters.Store(item.ID, done)
	defer h.waiters.Delete(item.ID)
	if err := h.db.Create(&item).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "queue memory"})
		return
	}
	if h.kick != nil {
		select {
		case h.kick <- struct{}{}:
		default:
		}
		select {
		case <-done:
		case <-time.After(rememberWait):
		case <-c.Request.Context().Done():
		}
	}
	h.db.Where("id = ?", item.ID).First(&item)
	out := gin.H{"inboxId": item.ID, "folderId": folder.ID, "status": item.Status}
	if n := utf8.RuneCountInString(in.Text); n > rememberGoodRunes {
		out["hint"] = fmt.Sprintf("Saved, but at %d characters it is long for one card (%d reads best): next time split it into one fact, decision or root cause per remember, or put the details in a page (create_note) and link it with [[Page title]].", n, rememberGoodRunes)
	}
	if folder.Project != "" {
		out["project"] = folder.Title
	}
	if item.Status == "done" && item.NoteID != "" {
		var n models.Note
		h.db.Select("id", "title").Where("id = ?", item.NoteID).First(&n)
		out["card"] = gin.H{"id": n.ID, "short": "#" + notecards.ShortID(n.ID), "title": n.Title}
		out["outcome"] = item.Outcome
		c.JSON(http.StatusOK, gin.H{"data": out})
		return
	}
	out["queued"] = true
	c.JSON(http.StatusAccepted, gin.H{"data": out})
}

// memoryFolder finds (or, for personal memory, creates) the folder a
// remembered card goes to: an explicit folder the caller can edit, a team
// memory (their chosen default, or the only one), or their personal memory.
func (h *NoteHandler) memoryFolder(userID, scope, folderID string) (models.Note, int, string) {
	var f models.Note
	if folderID != "" {
		if h.db.Where("id = ? AND type = ?", folderID, "folder").First(&f).Error != nil || !canEditNote(h.db, userID, f.ID) {
			return f, http.StatusForbidden, "You can't add cards to that folder"
		}
		if !f.Memory && h.memoryRoot(&f.ID) == "" {
			return f, http.StatusBadRequest, "That folder isn't an agent memory — pass a folder id from memory_folders (or one of its project folders)"
		}
		return f, 0, ""
	}
	if scope == "team" {
		cands, r := h.teamMemories(userID)
		def := h.memorySettings(userID).TeamFolderID
		for _, cnd := range cands {
			if cnd.ID == def {
				return cnd, 0, ""
			}
		}
		switch len(cands) {
		case 0:
			return f, http.StatusBadRequest, "No team memory is shared with you yet. Share a memory folder with your team (as editors) first."
		case 1:
			return cands[0], 0, ""
		}
		var opts []string
		for _, cnd := range cands {
			opts = append(opts, cnd.ID+" ("+cnd.Title+", "+r.scope(userID, cnd.ID).Label()+")")
		}
		return f, http.StatusBadRequest, "Several team memories are shared with you; pass folderId, one of: " + strings.Join(opts, "; ") +
			". A default can be set in Notes & Brain → Agent memory settings."
	}
	r := newScopeResolver(h.db)
	var own []models.Note
	h.db.Where("user_id = ? AND type = ? AND memory = ?", userID, "folder", true).Order("created_at ASC").Find(&own)
	for _, o := range own {
		if r.scope(userID, o.ID).Kind == "personal" {
			return o, 0, ""
		}
	}
	f = models.Note{
		ID: uuid.New().String(), UserID: userID, Type: "folder", Title: agentMemoryFolder, Memory: true,
		Slug: uniqueNoteSlug(h.db, userID, nil, agentMemoryFolder, ""), Visibility: "private", Version: 1,
		CreatedBy: userID, UpdatedBy: userID,
	}
	if err := h.db.Create(&f).Error; err != nil {
		return f, http.StatusInternalServerError, "create memory folder"
	}
	notesearch.Index(h.db, f.ID, f.Title, "", "")
	notifyNote(h.db, nil, f.ID, realtime.EventNoteCreated)
	return f, 0, ""
}

// projectFolder is the folder under a memory that holds one project's cards
// (found by its key, created on first use with the project's name as title).
func (h *NoteHandler) projectFolder(memory models.Note, key, name, actor string) (models.Note, error) {
	h.projectMu.Lock()
	defer h.projectMu.Unlock()
	var f models.Note
	if h.db.Where("parent_id = ? AND type = ? AND project = ?", memory.ID, "folder", key).First(&f).Error == nil {
		return f, nil
	}
	title := strings.TrimSpace(name)
	if title == "" || utf8.RuneCountInString(title) > 120 {
		title = key
	}
	var maxOrder *int
	h.db.Model(&models.Note{}).Where("parent_id = ?", memory.ID).Select("MAX(sort_order)").Scan(&maxOrder)
	order := 0
	if maxOrder != nil {
		order = *maxOrder + 1
	}
	pid := memory.ID
	f = models.Note{
		ID: uuid.New().String(), UserID: memory.UserID, WorkspaceID: memory.WorkspaceID, ParentID: &pid, Type: "folder",
		// Source "agent": agents keep it up to date and may link projects
		// to each other directly (a person's note only gets suggestions).
		SortOrder: order, Title: title, Project: key, Source: "agent", Slug: uniqueNoteSlug(h.db, memory.UserID, &pid, title, ""),
		Visibility: "private", Version: 1, CreatedBy: actor, UpdatedBy: actor,
	}
	if err := h.db.Create(&f).Error; err != nil {
		// Another backend made it first (unique parent + project).
		var other models.Note
		if h.db.Where("parent_id = ? AND type = ? AND project = ?", memory.ID, "folder", key).First(&other).Error == nil {
			return other, nil
		}
		return f, err
	}
	notesearch.Index(h.db, f.ID, f.Title, "", "")
	notifyNote(h.db, nil, f.ID, realtime.EventNoteCreated)
	return f, nil
}

// memoryRoot is the memory folder a node sits in (the node itself when it
// is one), or "" outside agent memory.
func (h *NoteHandler) memoryRoot(id *string) string {
	for i := 0; i < 32 && id != nil && *id != ""; i++ {
		var n struct {
			ParentID *string
			Memory   bool
		}
		if h.db.Table("notes").Select("parent_id, memory").Where("id = ? AND deleted_at IS NULL", *id).Scan(&n).Error != nil {
			return ""
		}
		if n.Memory {
			return *id
		}
		id = n.ParentID
	}
	return ""
}

type person struct{ ID, Name, Email string }

// peopleNamed finds users by a name or email fragment (case- and
// Turkish-letter-insensitive): an exact email or full name wins, else every
// partial match.
func (h *NoteHandler) peopleNamed(q string) []person {
	norm := func(s string) string { return strings.ToLower(notesearch.Fold(strings.TrimSpace(s))) }
	want := norm(q)
	var all []person
	h.db.Table("users").Select("id, name, email").Scan(&all)
	var exact, partial []person
	for _, p := range all {
		name, email := norm(p.Name), norm(p.Email)
		local, _, _ := strings.Cut(email, "@")
		switch {
		case want == name || want == email || want == local:
			exact = append(exact, p)
		case strings.Contains(name, want) || strings.Contains(email, want):
			partial = append(partial, p)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return partial
}

// projectBoost lifts the cards of the project the agent works in.
const projectBoost = 1.3

type projectInfo struct{ key, title string }

// projectResolver finds the project a note is filed under — the nearest
// folder with a project key above it, stopping at the memory folder —
// memoized per folder.
type projectResolver struct {
	db   *gorm.DB
	memo map[string]projectInfo
}

func newProjectResolver(db *gorm.DB) *projectResolver {
	return &projectResolver{db: db, memo: map[string]projectInfo{}}
}

func (r *projectResolver) of(parentID *string) projectInfo {
	if parentID == nil {
		return projectInfo{}
	}
	var chain []string
	var out projectInfo
	id := *parentID
	for i := 0; i < 16 && id != ""; i++ {
		if p, ok := r.memo[id]; ok {
			out = p
			break
		}
		chain = append(chain, id)
		var n struct {
			ParentID *string
			Project  string
			Title    string
			Memory   bool
		}
		if r.db.Table("notes").Select("parent_id, project, title, memory").Where("id = ?", id).Scan(&n).Error != nil {
			break
		}
		if n.Project != "" {
			out = projectInfo{n.Project, n.Title}
			break
		}
		if n.Memory || n.ParentID == nil {
			break
		}
		id = *n.ParentID
	}
	for _, c := range chain {
		r.memo[c] = out
	}
	return out
}

// processInbox turns every pending remember() into a card, oldest first.
// Each item is claimed with a conditional update, so a second worker (or a
// re-run) can't process it twice; failures retry up to three times.
func (h *NoteHandler) processInbox() {
	for {
		var item models.NoteInbox
		if h.db.Where("status = ?", "pending").Order("created_at ASC").First(&item).Error != nil {
			return
		}
		res := h.db.Model(&models.NoteInbox{}).Where("id = ? AND status = ?", item.ID, "pending").
			Updates(map[string]any{"status": "processing", "attempts": gorm.Expr("attempts + 1")})
		if res.RowsAffected == 0 {
			continue
		}
		noteID, outcome, err := h.rememberOne(item)
		if err != nil {
			status := "pending"
			if item.Attempts+1 >= 3 {
				status = "failed"
			}
			h.db.Model(&models.NoteInbox{}).Where("id = ?", item.ID).Updates(map[string]any{"status": status, "error": err.Error()})
		} else {
			h.db.Model(&models.NoteInbox{}).Where("id = ?", item.ID).Updates(map[string]any{"status": "done", "note_id": noteID, "outcome": outcome, "error": ""})
		}
		if w, ok := h.waiters.LoadAndDelete(item.ID); ok {
			close(w.(chan struct{}))
		}
	}
}

// rememberOne files one memory. With an AI provider set up, the text is
// split into cards first (note_atomize.go); any failure there falls back to
// one card per call. A card in the same folder with the same title or text
// is not duplicated: a merge suggestion is filed for a person to fold the
// new text in (nothing at all when the text is identical).
func (h *NoteHandler) rememberOne(item models.NoteInbox) (string, string, error) {
	var folder models.Note
	if err := h.db.Where("id = ?", item.FolderID).First(&folder).Error; err != nil {
		return "", "", err
	}
	if !canEditNote(h.db, item.UserID, folder.ID) {
		return "", "", errString("no longer allowed to write to the memory folder")
	}
	if cards, known, err := h.atomize(item, folder); err == nil {
		id, err := h.fileCards(item, folder, cards, known)
		return id, "split", err
	}
	title, summary, body := notecards.DeriveCard(item.Text)
	if id, outcome := h.duplicateCard(item, folder, title, body); outcome != "" {
		return id, outcome, nil
	}
	n, err := h.createCard(item, folder, title, summary, body)
	if err != nil {
		return "", "", err
	}
	return n.ID, "created", nil
}

// duplicateCard reports whether the folder already has this card: the same
// text ("duplicate": nothing to do) or the same title ("merge": a merge is
// suggested); "" when it is new.
func (h *NoteHandler) duplicateCard(item models.NoteInbox, folder models.Note, title, body string) (string, string) {
	norm := func(s string) string { return strings.ToLower(strings.Join(strings.Fields(notesearch.Fold(s)), " ")) }
	var siblings []models.Note
	h.db.Select("id", "parent_id", "title", "body", "source").Where("parent_id = ? AND type = ?", folder.ID, "card").Find(&siblings)
	for _, s := range siblings {
		if norm(s.Body) == norm(body) {
			return s.ID, "duplicate"
		}
		if norm(s.Title) == norm(title) {
			h.suggest(s, "merge", item.UserID, item.Agent, gin.H{"text": body, "inboxId": item.ID})
			return s.ID, "merge"
		}
	}
	return "", ""
}

// createCard writes one agent card into the folder, with its revision,
// search entry, body links and refers_to links to the remembered context.
func (h *NoteHandler) createCard(item models.NoteInbox, folder models.Note, title, summary, body string) (models.Note, error) {
	var maxOrder *int
	h.db.Model(&models.Note{}).Where("parent_id = ?", folder.ID).Select("MAX(sort_order)").Scan(&maxOrder)
	order := 0
	if maxOrder != nil {
		order = *maxOrder + 1
	}
	pid := folder.ID
	body = h.resolveWikiLinks(item.UserID, folder.ID, body)
	n := models.Note{
		ID: uuid.New().String(), UserID: folder.UserID, WorkspaceID: folder.WorkspaceID, ParentID: &pid, Type: "card",
		SortOrder: order, Title: title, Slug: uniqueNoteSlug(h.db, folder.UserID, &pid, title, ""), Body: body,
		Summary: summary, Source: "agent", Agent: item.Agent, Confidence: 0.8, Visibility: "private", Version: 1,
		CreatedBy: item.UserID, UpdatedBy: item.UserID,
	}
	if err := h.db.Create(&n).Error; err != nil {
		return n, err
	}
	recordNoteRevision(h.db, &n, item.UserID, noteSourceMCP)
	notesearch.Index(h.db, n.ID, n.Title, n.Summary, n.Body)
	syncBodyLinks(h.db, n.ID, n.Body, item.UserID)
	for _, uri := range notecards.ContextURIs(item.Context) {
		h.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&models.NoteLink{
			ID: uuid.New().String(), SrcNoteID: n.ID, DstURI: uri, Type: "refers_to", Weight: 1, Source: "agent", CreatedBy: item.UserID,
		})
	}
	notifyNote(h.db, nil, n.ID, realtime.EventNoteCreated)
	return n, nil
}

// resolveWikiLinks turns [[Title]] / [[#shortid]] in a remembered card's
// body into real note links ([Title](note:<id>)) to the notes they name —
// readable ones, a card in the same folder first — so they render as links
// and become "related" edges like any body link. A name nothing matches stays
// as written.
func (h *NoteHandler) resolveWikiLinks(userID, folderID, body string) string {
	return notecards.ReplaceWiki(body, func(target, label string) string {
		dst := ""
		if notecards.IsShortID(target) {
			dst, _ = h.resolveNoteRef(userID, target)
		} else {
			var cands []models.Note
			h.db.Select("id", "parent_id").Where("deleted_at IS NULL AND type <> ? AND LOWER(title) = ?", "folder", strings.ToLower(target)).
				Limit(20).Find(&cands)
			sort.SliceStable(cands, func(i, j int) bool {
				return ptrIs(cands[i].ParentID, folderID) && !ptrIs(cands[j].ParentID, folderID)
			})
			for _, c := range cands {
				if canReadNote(h.db, userID, c.ID) {
					dst = c.ID
					break
				}
			}
		}
		if dst == "" {
			return ""
		}
		return "[" + strings.ReplaceAll(label, "]", "") + "](note:" + dst + ")"
	})
}

func ptrIs(p *string, v string) bool { return p != nil && *p == v }

// InboxStatus reports what became of a remember() call (GET /notes/inbox/:id).
func (h *NoteHandler) InboxStatus(c *gin.Context) {
	userID := c.GetString("userID")
	var item models.NoteInbox
	if h.db.Where("id = ? AND user_id = ?", c.Param("id"), userID).First(&item).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"status": item.Status, "noteId": item.NoteID, "error": item.Error}})
}

// ── small helpers ────────────────────────────────────────────────────────

type errString string

func (e errString) Error() string { return string(e) }

func suffix(msg string) string {
	if msg == "" {
		return ""
	}
	return " (" + msg + ")"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func dedupe(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range list {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
