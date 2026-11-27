package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"patchman-be/internal/jira"
	"patchman-be/internal/models"
	"patchman-be/internal/qalink"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// QARadarHandler — MR Radar and Inbox (CR_QA_P0_PLAN P0.c). It reads the
// open MRs of every Git project linked in a QA space, matches them to Jira
// issues by key and computes signals. Read-only to GitLab / GitHub / Jira.
//
// Invariants:
//   - every remote call uses the caller's own access (loadAccessible on each
//     link's Git and Jira connection, then the connection's token or the
//     caller's linked account); a link the caller can't use is reported with
//     a status, never filled from someone else's access;
//   - one link failing never fails the reply (per-link status, 200);
//   - lists are never cut silently: a link says how many MRs it shows of how
//     many there are;
//   - facts not fetched are null, not false.
type QARadarHandler struct {
	db   *gorm.DB
	git  *GitRemoteHandler
	jira *JiraConnectionHandler
	now  func() time.Time
}

func NewQARadarHandler(db *gorm.DB, git *GitRemoteHandler, jiraH *JiraConnectionHandler) *QARadarHandler {
	return &QARadarHandler{db: db, git: git, jira: jiraH, now: time.Now}
}

const (
	radarMaxPages     = 3 // ×100 open MRs per project
	radarConcurrency  = 6
	radarDeadline     = 40 * time.Second
	radarResultTTL    = 60 * time.Second
	radarJiraChunk    = 50
	radarPathsPerMR   = 300
	radarDetailMaxAge = 24 * time.Hour
)

// ── payload ───────────────────────────────────────────────────────────

type radarUser struct {
	Username string `json:"username"`
	Name     string `json:"name,omitempty"`
}

type radarJira struct {
	Key            string `json:"key"`
	Summary        string `json:"summary,omitempty"`
	Status         string `json:"status,omitempty"`
	StatusCategory string `json:"statusCategory,omitempty"`
	Assignee       string `json:"assignee,omitempty"`
	URL            string `json:"url,omitempty"`
	Missing        bool   `json:"missing,omitempty"`
}

type radarMR struct {
	Ref                 string          `json:"ref"` // mr:<linkId>:<iid>
	LinkID              string          `json:"linkId"`
	GitConnectionID     string          `json:"gitConnectionId"`
	Provider            string          `json:"provider"`
	Project             string          `json:"project"`
	IID                 int64           `json:"iid"`
	Title               string          `json:"title"`
	State               string          `json:"state"` // opened | merged
	Author              radarUser       `json:"author"`
	Reviewers           []radarUser     `json:"reviewers"`
	Assignees           []radarUser     `json:"assignees"`
	ApprovedBy          []string        `json:"approvedBy"`
	SourceBranch        string          `json:"sourceBranch"`
	TargetBranch        string          `json:"targetBranch"`
	WebURL              string          `json:"webUrl"`
	HeadSHA             string          `json:"headSha"`
	CreatedAt           string          `json:"createdAt"`
	UpdatedAt           string          `json:"updatedAt"`
	MergedAt            string          `json:"mergedAt,omitempty"`
	Draft               bool            `json:"draft"`
	HasConflicts        bool            `json:"hasConflicts"`
	DetailedMergeStatus string          `json:"detailedMergeStatus,omitempty"`
	Pipeline            *string         `json:"pipeline"`
	ApprovalsGiven      *int            `json:"approvalsGiven"`
	ApprovalsLeft       *int            `json:"approvalsLeft"`
	UnresolvedThreads   *bool           `json:"unresolvedThreads"`
	ChangedFiles        *int            `json:"changedFiles"`
	AIAssisted          *bool           `json:"aiAssisted"`
	Notes               int             `json:"notes"`
	JiraKey             string          `json:"jiraKey,omitempty"`
	Jira                *radarJira      `json:"jira"`
	Signals             []qalink.Signal `json:"signals"`
	// Review: the AI review at the current head (nil = none).
	Review *radarReview `json:"review"`
	// OpenFindings: posted findings of an earlier review still waiting for a
	// fix check at this head (0 = none, or already checked here) — P4.a.
	OpenFindings int `json:"openFindings"`
	// VerifyID: the fix check at this head, if any.
	VerifyID string `json:"verifyId,omitempty"`
	// VerifyPending: verdicts of that check nobody applied yet (submitted only).
	VerifyPending int      `json:"verifyPending,omitempty"`
	verifyOwners  []string // user ids: who posted / claimed the source review
	CollidesWith  []string `json:"collidesWith"`
	paths         []string // nil = not fetched
	description   string   // key extraction only
	link          *models.QAProjectLink
}

type radarReview struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Pending int    `json:"pending"` // findings waiting for a decision
	Posted  int    `json:"posted"`
}

type radarLink struct {
	ID              string `json:"id"`
	GitConnectionID string `json:"gitConnectionId"`
	GitProject      string `json:"gitProject"`
	Provider        string `json:"provider,omitempty"`
	JiraProjectKey  string `json:"jiraProjectKey,omitempty"`
	// Status: ok | noAccess (caller can't use the Git connection) |
	// needsToken (no token / GitLab not linked) | error.
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	JiraStatus string `json:"jiraStatus,omitempty"` // ok | noAccess | error | "" (no Jira project)
	JiraError  string `json:"jiraError,omitempty"`
	Shown      int    `json:"shown"`
	Total      *int   `json:"total"` // nil = unknown
	Truncated  bool   `json:"truncated"`
	// Incomplete: MRs whose details couldn't all be read (timeout, error);
	// their unread facts are null.
	Incomplete int `json:"incomplete"`
	// Personal: the Git identity is the caller's own (their OAuth token, or
	// a connection they own), so "mine" / Inbox items can be derived. A
	// connection's stored token shared by grant belongs to its owner.
	Personal bool `json:"personal"`
	// Mapped: the caller reads this link through their own connection(s).
	Mapped bool `json:"mapped"`
	// MergedTruncated: more merged MRs exist in the look-back than were read.
	MergedTruncated bool `json:"mergedTruncated,omitempty"`
}

type radarResult struct {
	GeneratedAt string            `json:"generatedAt"`
	Links       []radarLink       `json:"links"`
	MRs         []*radarMR        `json:"mrs"`    // open
	Merged      []*radarMR        `json:"merged"` // recently merged, only those with a signal
	Settings    models.QASettings `json:"settings"`
	Signals     []string          `json:"signalKinds"`
	Me          map[string]string `json:"me"`
	mergedAll   []*radarMR        // git connection id → the caller's own username there
	jiraConns   map[string]bool   // Jira connection ids the caller can use
	links       []models.QAProjectLink
}

// ── handlers ──────────────────────────────────────────────────────────

// Radar — GET /api/qa/radar?workspaceId=&refresh=1.
func (h *QARadarHandler) Radar(c *gin.Context) {
	res, ok := h.result(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

// result resolves the scope and returns the (cached) radar.
func (h *QARadarHandler) result(c *gin.Context) (*radarResult, bool) {
	userID := c.GetString("userID")
	scope, ok := qaListScope(h.db, c, userID)
	if !ok {
		return nil, false
	}
	ws := qaWorkspaceParam(c)
	key := userID + "|" + ws
	if c.Query("refresh") != "1" {
		if v, ok := radarResults.Load(key); ok {
			if e := v.(radarCached); h.now().Sub(e.at) < radarResultTTL {
				return e.res, true
			}
		}
	}
	var links []models.QAProjectLink
	if err := scope.Order("git_project ASC").Find(&links).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load links"})
		return nil, false
	}
	var wsp *string
	if ws != "" {
		wsp = &ws
	}
	settings := loadQASettings(h.db, userID, wsp)
	ctx, cancel := context.WithTimeout(c.Request.Context(), radarDeadline)
	defer cancel()
	res := h.compute(ctx, userID, links, settings, c.Query("refresh") == "1")
	radarResults.Store(key, radarCached{res: res, at: h.now()})
	return res, true
}

type radarCached struct {
	res *radarResult
	at  time.Time
}

var radarResults sync.Map // userID|workspace → radarCached

// radarAuditEvery: the background poll would otherwise write an audit row
// per connection every two minutes per user. An explicit refresh is always
// audited; a poll at most this often.
const radarAuditEvery = 30 * time.Minute

var radarAuditAt sync.Map // git|jira + user + connection → time

func radarShouldAudit(key string, refresh bool) bool {
	now := time.Now()
	if !refresh {
		if v, ok := radarAuditAt.Load(key); ok && now.Sub(v.(time.Time)) < radarAuditEvery {
			return false
		}
	}
	radarAuditAt.Store(key, now)
	return true
}

// ── compute ───────────────────────────────────────────────────────────

func (h *QARadarHandler) compute(ctx context.Context, userID string, links []models.QAProjectLink, settings models.QASettings, refresh bool) *radarResult {
	now := h.now()
	sig := radarSettings(settings)
	res := &radarResult{
		GeneratedAt: now.UTC().Format(time.RFC3339),
		Links:       make([]radarLink, len(links)),
		MRs:         []*radarMR{},
		Merged:      []*radarMR{},
		Settings:    settings,
		Signals:     qalink.AllSignals,
		Me:          map[string]string{},
		jiraConns:   map[string]bool{},
		links:       links,
	}
	mergedDays := settings.MergedDays
	if mergedDays <= 0 {
		mergedDays = 7
	}
	opts := fetchOpts{
		needPaths:   !sig.Disabled[qalink.SigCollision] || (len(sig.TestPatterns) > 0 && !sig.Disabled[qalink.SigNoTests]),
		needCommit:  !sig.Disabled[qalink.SigAIAssisted],
		mergedSince: now.Add(-time.Duration(mergedDays) * 24 * time.Hour),
	}

	type linkOut struct {
		open, merged []*radarMR
	}
	outs := make([]linkOut, len(links))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3)
	audited := map[string][]string{}
	// A person's own-connection mappings replace the link's connections for
	// them (QALinkConnectionMap); links are copied so the shared rows stay.
	maps := qaLinkMaps(h.db, userID, links)
	eff := make([]models.QAProjectLink, len(links))
	for i, l := range links {
		eff[i] = l
		if m, ok := maps[l.ID]; ok {
			if m.GitConnectionID != "" {
				eff[i].GitConnectionID = m.GitConnectionID
			}
			if m.JiraConnectionID != "" && l.JiraProjectKey != "" {
				eff[i].JiraConnectionID = m.JiraConnectionID
			}
		}
	}
	links = eff
	res.links = eff
	for i := range links {
		l := &links[i]
		_, mapped := maps[l.ID]
		res.Links[i] = radarLink{ID: l.ID, GitConnectionID: l.GitConnectionID, GitProject: l.GitProject, JiraProjectKey: l.JiraProjectKey, Mapped: mapped}
		wg.Add(1)
		go func(i int, l *models.QAProjectLink) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rl := &res.Links[i]
			conn, _, err := h.git.conns.loadAccessible(l.GitConnectionID, userID)
			if err != nil {
				rl.Status, rl.Error = "noAccess", "You can't use this link's Git connection"
				return
			}
			rl.Provider = conn.Provider
			if rl.Provider == "" {
				rl.Provider = "gitlab"
			}
			token, base, err := h.git.credsFor(conn, userID)
			if err != nil {
				rl.Status, rl.Error = "needsToken", err.Error()
				return
			}
			f := &radarFetcher{h: h.git, conn: conn, token: token, glBase: h.git.glBase(base), opts: opts}
			// "Me" only when the token speaks for the caller: their own
			// linked account, or a connection they own. A shared PAT is the
			// owner's identity — using it would hand the owner's reviews to
			// whoever reads the link.
			me := ""
			if conn.UserID == userID || !h.git.usesStoredCredential(conn) {
				rl.Personal = true
				me = f.me(ctx)
			}
			var open, merged []*radarMR
			var ferr error
			if isGitHub(conn) {
				open, merged, ferr = f.github(ctx, l, rl)
			} else {
				open, merged, ferr = f.gitlab(ctx, l, rl)
			}
			mu.Lock()
			if me != "" {
				res.Me[conn.ID] = me
			}
			audited[conn.ID] = append(audited[conn.ID], l.GitProject)
			mu.Unlock()
			if ferr != nil {
				rl.Status, rl.Error = "error", ferr.Error()
			} else {
				rl.Status = "ok"
			}
			for _, m := range append(append([]*radarMR{}, open...), merged...) {
				m.link = l
				m.LinkID = l.ID
				m.GitConnectionID = l.GitConnectionID
				m.Project = l.GitProject
				m.Ref = "mr:" + l.ID + ":" + strconv.FormatInt(m.IID, 10)
				if l.JiraProjectKey != "" {
					m.JiraKey = qalink.ExtractJiraKey(m.Title, m.SourceBranch, m.description, []string{l.JiraProjectKey}, l.KeyPattern)
				}
			}
			outs[i] = linkOut{open: open, merged: merged}
		}(i, l)
	}
	wg.Wait()
	for connID, projects := range audited {
		if radarShouldAudit("git|"+userID+"|"+connID, refresh) {
			h.git.conns.audit(userID, connID, "qa_radar", true, gin.H{"projects": projects})
		}
	}

	all := []*radarMR{}
	for _, o := range outs {
		all = append(all, o.open...)
		all = append(all, o.merged...)
	}
	h.attachJira(ctx, userID, links, all, res, refresh)

	// Collisions among open MRs of the same project.
	open := []*radarMR{}
	for _, o := range outs {
		open = append(open, o.open...)
	}
	groups := make([]string, len(open))
	paths := make([][]string, len(open))
	for i, m := range open {
		groups[i] = m.GitConnectionID + "|" + m.Project
		paths[i] = m.paths
	}
	coll := qalink.Collisions(groups, paths)

	for i, m := range open {
		m.Signals = qalink.Signals(facts(m), sig, now)
		m.CollidesWith = []string{}
		if others := coll[i]; len(others) > 0 && !sig.Disabled[qalink.SigCollision] {
			for _, j := range others {
				m.CollidesWith = append(m.CollidesWith, open[j].Ref)
			}
			m.Signals = append(m.Signals, qalink.Signal{Kind: qalink.SigCollision, Reason: strconv.Itoa(len(others)) + " MR"})
		}
		res.MRs = append(res.MRs, m)
	}
	for _, o := range outs {
		res.mergedAll = append(res.mergedAll, o.merged...)
		for _, m := range o.merged {
			m.Signals = qalink.Signals(facts(m), sig, now)
			m.CollidesWith = []string{}
			if len(m.Signals) > 0 {
				res.Merged = append(res.Merged, m)
			}
		}
	}
	sort.SliceStable(res.MRs, func(a, b int) bool { return res.MRs[a].UpdatedAt > res.MRs[b].UpdatedAt })
	h.attachReviews(res.MRs)
	if !sig.Disabled[qalink.SigUnverifiedFixes] {
		for _, m := range res.MRs {
			if m.OpenFindings > 0 {
				m.Signals = append(m.Signals, qalink.Signal{Kind: qalink.SigUnverifiedFixes, Reason: strconv.Itoa(m.OpenFindings) + " findings"})
			}
		}
	}
	h.snapshot(userID, settings.WorkspaceID, res, now)
	return res
}

func facts(m *radarMR) qalink.MRFacts {
	f := qalink.MRFacts{
		State: m.State, Draft: m.Draft, HasConflicts: m.HasConflicts, DetailedMergeStatus: m.DetailedMergeStatus,
		UnresolvedThreads: m.UnresolvedThreads, ApprovalsGiven: m.ApprovalsGiven, ChangedFiles: m.ChangedFiles,
		Paths: m.paths, AIAssisted: m.AIAssisted, JiraKey: m.JiraKey,
	}
	if m.Pipeline != nil {
		f.Pipeline = *m.Pipeline
	}
	if t, err := time.Parse(time.RFC3339, m.UpdatedAt); err == nil {
		f.UpdatedAt = t
	}
	if m.link != nil && m.link.JiraProjectKey != "" {
		f.JiraLinked = true
	}
	if m.Jira != nil {
		if m.Jira.Missing {
			f.JiraMissing = true
		} else {
			f.Jira = &qalink.JiraFacts{Status: m.Jira.Status, StatusCategory: m.Jira.StatusCategory}
		}
	} else if f.JiraLinked && m.JiraKey != "" {
		// Jira not reachable for this caller: the Jira signals stay off.
		f.JiraLinked = false
	}
	return f
}

// ── Jira ──────────────────────────────────────────────────────────────

func (h *QARadarHandler) attachJira(ctx context.Context, userID string, links []models.QAProjectLink, mrs []*radarMR, res *radarResult, refresh bool) {
	byConn := map[string][]*radarMR{}
	for _, m := range mrs {
		if m.JiraKey != "" && m.link != nil && m.link.JiraConnectionID != "" {
			byConn[m.link.JiraConnectionID] = append(byConn[m.link.JiraConnectionID], m)
		}
	}
	setStatus := func(connID, status, msg string) {
		for i := range res.Links {
			if links[i].JiraConnectionID == connID && links[i].JiraProjectKey != "" {
				res.Links[i].JiraStatus, res.Links[i].JiraError = status, msg
			}
		}
	}
	for connID := range jiraConnIDs(links) {
		conn, role, err := h.jira.loadAccessible(connID, userID)
		if err != nil {
			setStatus(connID, "noAccess", "You can't use this link's Jira connection")
			continue
		}
		cl, err := h.jira.client(*conn)
		if err != nil {
			setStatus(connID, "error", err.Error())
			continue
		}
		res.jiraConns[connID] = true
		group := byConn[connID]
		keys := []string{}
		seen := map[string]bool{}
		for _, m := range group {
			if !seen[m.JiraKey] {
				seen[m.JiraKey] = true
				keys = append(keys, m.JiraKey)
			}
		}
		issues, missing, err := batchJiraIssues(ctx, cl, keys)
		if err != nil {
			setStatus(connID, "error", err.Error())
			continue
		}
		setStatus(connID, "ok", "")
		if len(keys) > 0 && radarShouldAudit("jira|"+userID+"|"+connID, refresh) {
			h.jira.auditRadar(conn, userID, role, len(keys))
		}
		for _, m := range group {
			if row, ok := issues[m.JiraKey]; ok {
				m.Jira = &radarJira{Key: row.Key, Summary: row.Summary, Status: row.Status, StatusCategory: row.StatusCategory, Assignee: row.Assignee, URL: cl.IssueWebURL(row.Key)}
			} else if missing[m.JiraKey] {
				m.Jira = &radarJira{Key: m.JiraKey, Missing: true}
			}
		}
	}
}

func jiraConnIDs(links []models.QAProjectLink) map[string]bool {
	out := map[string]bool{}
	for _, l := range links {
		if l.JiraConnectionID != "" && l.JiraProjectKey != "" {
			out[l.JiraConnectionID] = true
		}
	}
	return out
}

// jiraSearcher is the part of the Jira client the batch needs.
type jiraSearcher interface {
	Search(ctx context.Context, req jira.SearchRequest) (*jira.SearchResult, error)
}

// batchJiraIssues looks issues up with `key in (…)` in chunks. A key that
// doesn't exist makes Jira reject the whole JQL (400), so a rejected chunk
// is split in halves until the bad keys are isolated — they come back in
// missing. Invalid-looking keys never reach the JQL.
func batchJiraIssues(ctx context.Context, cl jiraSearcher, keys []string) (map[string]jira.IssueRow, map[string]bool, error) {
	found := map[string]jira.IssueRow{}
	missing := map[string]bool{}
	valid := []string{}
	for _, k := range keys {
		if jira.ValidKey(k) {
			valid = append(valid, k)
		} else {
			missing[k] = true
		}
	}
	var run func(chunk []string) error
	run = func(chunk []string) error {
		if len(chunk) == 0 {
			return nil
		}
		jql := "key in (" + strings.Join(chunk, ",") + ")"
		r, err := cl.Search(ctx, jira.SearchRequest{JQL: jql, Limit: len(chunk)})
		if err != nil {
			if ae, ok := jira.AsAPIError(err); ok && ae.Status == http.StatusBadRequest {
				if len(chunk) == 1 {
					missing[chunk[0]] = true
					return nil
				}
				mid := len(chunk) / 2
				if err := run(chunk[:mid]); err != nil {
					return err
				}
				return run(chunk[mid:])
			}
			return err
		}
		got := map[string]bool{}
		for _, row := range r.Issues {
			found[strings.ToUpper(row.Key)] = row
			got[strings.ToUpper(row.Key)] = true
		}
		// A moved issue answers under its new key; the old one is then
		// absent from the reply (doğrulanmadı) — report it as missing.
		for _, k := range chunk {
			if !got[k] {
				missing[k] = true
			}
		}
		return nil
	}
	for i := 0; i < len(valid); i += radarJiraChunk {
		end := i + radarJiraChunk
		if end > len(valid) {
			end = len(valid)
		}
		if err := run(valid[i:end]); err != nil {
			return nil, nil, err
		}
	}
	return found, missing, nil
}

// ── fetching ──────────────────────────────────────────────────────────

type fetchOpts struct {
	needPaths   bool
	needCommit  bool
	mergedSince time.Time
}

type radarFetcher struct {
	h      *GitRemoteHandler
	conn   *models.GitConnection
	token  string
	glBase string
	opts   fetchOpts
	errs   int // failed calls (not 404) on this copy; per-MR copies count per MR
}

// detailCache holds per-MR facts keyed by connection|project|iid|updatedAt
// (or |sha for paths and commits). An entry is only served after the
// caller's own list call returned that MR, so sharing it across callers
// reveals nothing new.
var detailCache sync.Map

type detailEntry struct {
	v   any
	at  time.Time
	ttl time.Duration
}

// radarVolatileTTL bounds facts that can change without the MR's
// updated_at moving (pipeline status, approvals, mergeable state — not
// verified either way on live GitLab / GitHub): about one poll.
const radarVolatileTTL = 2 * time.Minute

var detailSweptAt atomic.Int64

func cacheGet(key string) (any, bool) {
	if v, ok := detailCache.Load(key); ok {
		e := v.(detailEntry)
		if time.Since(e.at) < e.ttl {
			return e.v, true
		}
		detailCache.Delete(key)
	}
	return nil, false
}

// cachePut keeps SHA-keyed facts (paths, commits) for a day.
func cachePut(key string, v any) { cachePutTTL(key, v, radarDetailMaxAge) }

// cachePutShort keeps volatile facts for radarVolatileTTL.
func cachePutShort(key string, v any) { cachePutTTL(key, v, radarVolatileTTL) }

func cachePutTTL(key string, v any, ttl time.Duration) {
	now := time.Now()
	detailCache.Store(key, detailEntry{v: v, at: now, ttl: ttl})
	// Sweep expired and superseded entries at most every 5 minutes so a
	// long-running server doesn't grow the cache without bound.
	if last := detailSweptAt.Load(); now.Unix()-last >= 300 && detailSweptAt.CompareAndSwap(last, now.Unix()) {
		detailCache.Range(func(k, val any) bool {
			if e := val.(detailEntry); now.Sub(e.at) >= e.ttl {
				detailCache.Delete(k)
			}
			return true
		})
	}
}

// get runs one GET with the deadline; out is decoded on 2xx.
func (f *radarFetcher) get(ctx context.Context, endpoint string, out any) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	if isGitHub(f.conn) {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	resp, err := f.h.http.Do(req)
	if err != nil {
		f.errs++
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode/100 != 2 {
		if resp.StatusCode != http.StatusNotFound {
			f.errs++
		}
		return resp.Header, &remoteErr{status: resp.StatusCode, msg: remoteMessage(body)}
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return resp.Header, fmt.Errorf("decode: %w", err)
		}
	}
	return resp.Header, nil
}

type remoteErr struct {
	status int
	msg    string
}

func (e *remoteErr) Error() string {
	if e.msg == "" {
		return fmt.Sprintf("remote %d", e.status)
	}
	return fmt.Sprintf("remote %d: %s", e.status, e.msg)
}

func remoteMessage(body []byte) string {
	var m struct {
		Message any    `json:"message"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(body, &m)
	s := m.Error
	if v, ok := m.Message.(string); ok && v != "" {
		s = v
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func (f *radarFetcher) me(ctx context.Context) string {
	sum := sha256.Sum256([]byte(f.token))
	key := "me|" + f.conn.ID + "|" + hex.EncodeToString(sum[:8])
	if v, ok := cacheGet(key); ok {
		return v.(string)
	}
	var u struct {
		Username string `json:"username"`
		Login    string `json:"login"`
	}
	endpoint := f.glBase + "/user"
	if isGitHub(f.conn) {
		endpoint = f.h.ghBase(f.conn) + "/user"
	}
	if _, err := f.get(ctx, endpoint, &u); err != nil {
		return ""
	}
	name := u.Username
	if name == "" {
		name = u.Login
	}
	cachePut(key, name)
	return name
}

// parallel runs fn over items with bounded concurrency.
func parallel[T any](items []T, fn func(T)) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, radarConcurrency)
	for _, it := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func(it T) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(it)
		}(it)
	}
	wg.Wait()
}

// enrichAll runs fn per MR on its own fetcher copy and returns how many
// MRs had a failed call (their facts stay null): reported per link so a
// partial read is never silent.
func (f *radarFetcher) enrichAll(mrs []*radarMR, fn func(*radarFetcher, *radarMR)) int {
	var bad atomic.Int32
	parallel(mrs, func(m *radarMR) {
		mf := *f
		mf.errs = 0
		fn(&mf, m)
		if mf.errs > 0 {
			bad.Add(1)
		}
	})
	return int(bad.Load())
}

func intp(n int) *int       { return &n }
func boolp(b bool) *bool    { return &b }
func strp(s string) *string { return &s }

// ── GitLab ────────────────────────────────────────────────────────────

type glRadarUser struct {
	Username string `json:"username"`
	Name     string `json:"name"`
}

type glRadarMR struct {
	IID                         int64         `json:"iid"`
	Title                       string        `json:"title"`
	Description                 string        `json:"description"`
	State                       string        `json:"state"`
	SourceBranch                string        `json:"source_branch"`
	TargetBranch                string        `json:"target_branch"`
	WebURL                      string        `json:"web_url"`
	SHA                         string        `json:"sha"`
	CreatedAt                   string        `json:"created_at"`
	UpdatedAt                   string        `json:"updated_at"`
	MergedAt                    *string       `json:"merged_at"`
	Draft                       bool          `json:"draft"`
	HasConflicts                bool          `json:"has_conflicts"`
	DetailedMergeStatus         string        `json:"detailed_merge_status"`
	BlockingDiscussionsResolved *bool         `json:"blocking_discussions_resolved"`
	UserNotesCount              int           `json:"user_notes_count"`
	Author                      glRadarUser   `json:"author"`
	Reviewers                   []glRadarUser `json:"reviewers"`
	Assignees                   []glRadarUser `json:"assignees"`
}

func glRadarUsers(us []glRadarUser) []radarUser {
	out := make([]radarUser, 0, len(us))
	for _, u := range us {
		out = append(out, radarUser{Username: u.Username, Name: u.Name})
	}
	return out
}

func (m glRadarMR) toRadar() *radarMR {
	r := &radarMR{
		Provider: "gitlab", IID: m.IID, Title: m.Title, State: m.State, SourceBranch: m.SourceBranch,
		TargetBranch: m.TargetBranch, WebURL: m.WebURL, HeadSHA: m.SHA, CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt, Draft: m.Draft, HasConflicts: m.HasConflicts, DetailedMergeStatus: m.DetailedMergeStatus,
		Notes: m.UserNotesCount, Author: radarUser{Username: m.Author.Username, Name: m.Author.Name},
		Reviewers: glRadarUsers(m.Reviewers), Assignees: glRadarUsers(m.Assignees), ApprovedBy: []string{},
		description: m.Description,
	}
	if m.MergedAt != nil {
		r.MergedAt = *m.MergedAt
	}
	if m.BlockingDiscussionsResolved != nil {
		r.UnresolvedThreads = boolp(!*m.BlockingDiscussionsResolved)
	}
	return r
}

func (f *radarFetcher) gitlab(ctx context.Context, l *models.QAProjectLink, rl *radarLink) (open, merged []*radarMR, err error) {
	base := f.glBase + projectPath(l.GitProject) + "/merge_requests"
	open = []*radarMR{}
	for page := 1; page <= radarMaxPages; page++ {
		q := url.Values{"state": {"opened"}, "per_page": {"100"}, "page": {strconv.Itoa(page)}, "order_by": {"updated_at"}}
		var raw []glRadarMR
		hdr, gerr := f.get(ctx, base+"?"+q.Encode(), &raw)
		if gerr != nil {
			return nil, nil, gerr
		}
		for _, m := range raw {
			open = append(open, m.toRadar())
		}
		if t := hdr.Get("X-Total"); t != "" {
			if n, perr := strconv.Atoi(t); perr == nil {
				rl.Total = intp(n)
			}
		}
		next := strings.TrimSpace(hdr.Get("X-Next-Page"))
		if next == "" || len(raw) == 0 {
			break
		}
		if page == radarMaxPages {
			rl.Truncated = true
		}
	}
	rl.Shown = len(open)
	if rl.Total == nil && !rl.Truncated {
		rl.Total = intp(len(open))
	}
	rl.Incomplete = f.enrichAll(open, func(mf *radarFetcher, m *radarMR) { mf.glEnrich(ctx, l, m) })

	merged = []*radarMR{}
	{
		q := url.Values{"state": {"merged"}, "per_page": {"100"}, "updated_after": {f.opts.mergedSince.UTC().Format(time.RFC3339)}, "order_by": {"updated_at"}}
		var raw []glRadarMR
		hdr, gerr := f.get(ctx, base+"?"+q.Encode(), &raw)
		if gerr == nil {
			for _, m := range raw {
				merged = append(merged, m.toRadar())
			}
			if strings.TrimSpace(hdr.Get("X-Next-Page")) != "" {
				rl.MergedTruncated = true
			}
		}
	}
	return open, merged, nil
}

func (f *radarFetcher) glEnrich(ctx context.Context, l *models.QAProjectLink, m *radarMR) {
	mrBase := f.glBase + projectPath(l.GitProject) + "/merge_requests/" + strconv.FormatInt(m.IID, 10)
	key := "gl|" + f.conn.ID + "|" + l.GitProject + "|" + strconv.FormatInt(m.IID, 10)

	type glDetail struct {
		Pipeline     *string
		ChangedFiles *int
	}
	dk := key + "|d|" + m.UpdatedAt
	if v, ok := cacheGet(dk); ok {
		d := v.(glDetail)
		m.Pipeline, m.ChangedFiles = d.Pipeline, d.ChangedFiles
	} else {
		var raw struct {
			ChangesCount string `json:"changes_count"`
			HeadPipeline *struct {
				Status string `json:"status"`
			} `json:"head_pipeline"`
		}
		if _, err := f.get(ctx, mrBase, &raw); err == nil {
			d := glDetail{}
			if raw.HeadPipeline != nil && raw.HeadPipeline.Status != "" {
				d.Pipeline = strp(raw.HeadPipeline.Status)
			}
			if n, perr := strconv.Atoi(strings.TrimSuffix(raw.ChangesCount, "+")); perr == nil {
				d.ChangedFiles = intp(n)
			}
			cachePutShort(dk, d)
			m.Pipeline, m.ChangedFiles = d.Pipeline, d.ChangedFiles
		}
	}

	type glAppr struct {
		Left  *int
		Given int
		By    []string
	}
	ak := key + "|a|" + m.UpdatedAt
	if v, ok := cacheGet(ak); ok {
		a := v.(glAppr)
		m.ApprovalsLeft, m.ApprovalsGiven, m.ApprovedBy = a.Left, intp(a.Given), a.By
	} else {
		var raw struct {
			ApprovalsLeft *int `json:"approvals_left"`
			ApprovedBy    []struct {
				User glRadarUser `json:"user"`
			} `json:"approved_by"`
		}
		if _, err := f.get(ctx, mrBase+"/approvals", &raw); err == nil {
			a := glAppr{Left: raw.ApprovalsLeft, Given: len(raw.ApprovedBy), By: []string{}}
			for _, u := range raw.ApprovedBy {
				a.By = append(a.By, u.User.Username)
			}
			cachePutShort(ak, a)
			m.ApprovalsLeft, m.ApprovalsGiven, m.ApprovedBy = a.Left, intp(a.Given), a.By
		}
	}

	if f.opts.needPaths && m.HeadSHA != "" {
		pk := key + "|p|" + m.HeadSHA
		if v, ok := cacheGet(pk); ok {
			m.paths = v.([]string)
		} else if ps, ok := f.glPaths(ctx, mrBase); ok {
			cachePut(pk, ps)
			m.paths = ps
		}
	}
	if f.opts.needCommit && m.HeadSHA != "" {
		ck := key + "|c|" + m.HeadSHA
		if v, ok := cacheGet(ck); ok {
			m.AIAssisted = boolp(v.(bool))
		} else {
			var raw []struct {
				Message string `json:"message"`
			}
			if _, err := f.get(ctx, mrBase+"/commits?per_page=100", &raw); err == nil {
				msgs := make([]string, 0, len(raw))
				for _, c := range raw {
					msgs = append(msgs, c.Message)
				}
				ai := qalink.AIAssisted(msgs)
				cachePut(ck, ai)
				m.AIAssisted = boolp(ai)
			}
		}
	}
}

// glPaths lists an MR's changed paths (/diffs, GitLab 15.7+; /changes as a
// fallback). Capped; ok=false when the paths couldn't be read in full.
func (f *radarFetcher) glPaths(ctx context.Context, mrBase string) ([]string, bool) {
	paths := []string{}
	add := func(newP, oldP string) {
		if newP != "" {
			paths = append(paths, newP)
		}
		if oldP != "" && oldP != newP {
			paths = append(paths, oldP)
		}
	}
	for page := 1; page <= 3; page++ {
		var raw []struct {
			NewPath string `json:"new_path"`
			OldPath string `json:"old_path"`
		}
		hdr, err := f.get(ctx, mrBase+"/diffs?per_page=100&page="+strconv.Itoa(page), &raw)
		if err != nil {
			if re, ok := err.(*remoteErr); ok && re.status == http.StatusNotFound && page == 1 {
				var ch struct {
					Changes []struct {
						NewPath string `json:"new_path"`
						OldPath string `json:"old_path"`
					} `json:"changes"`
				}
				if _, err := f.get(ctx, mrBase+"/changes", &ch); err != nil {
					return nil, false
				}
				for _, c := range ch.Changes {
					add(c.NewPath, c.OldPath)
				}
				return capPaths(paths), true
			}
			return nil, false
		}
		for _, d := range raw {
			add(d.NewPath, d.OldPath)
		}
		if strings.TrimSpace(hdr.Get("X-Next-Page")) == "" {
			return capPaths(paths), true
		}
	}
	return capPaths(paths), true
}

func capPaths(p []string) []string {
	if len(p) > radarPathsPerMR {
		return p[:radarPathsPerMR]
	}
	return p
}

// ── GitHub ────────────────────────────────────────────────────────────

func (f *radarFetcher) ghRepoPath(ctx context.Context, project string) (string, error) {
	if _, err := strconv.ParseInt(project, 10, 64); err != nil {
		if strings.Count(project, "/") != 1 || strings.ContainsAny(project, " ?#") {
			return "", fmt.Errorf("project must be a GitHub repository id or owner/name")
		}
		return "/repos/" + project, nil
	}
	key := "ghrepo|" + f.conn.ID + "|" + project
	if v, ok := cacheGet(key); ok {
		return v.(string), nil
	}
	var info ghRepoInfo
	if _, err := f.get(ctx, f.h.ghBase(f.conn)+"/repositories/"+project, &info); err != nil {
		return "", err
	}
	p := "/repos/" + info.FullName
	cachePut(key, p)
	return p, nil
}

func ghUsers(us []ghUser) []radarUser {
	out := make([]radarUser, 0, len(us))
	for _, u := range us {
		out = append(out, radarUser{Username: u.Login})
	}
	return out
}

func (p ghPull) toRadar() *radarMR {
	r := &radarMR{
		Provider: "github", IID: p.Number, Title: p.Title, State: p.glState(), SourceBranch: p.Head.Ref,
		TargetBranch: p.Base.Ref, WebURL: p.HTMLURL, HeadSHA: p.Head.Sha, CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt, Draft: p.Draft, HasConflicts: p.hasConflicts(), Notes: p.Comments + p.ReviewComments,
		Author: radarUser{Username: p.User.Login}, Reviewers: ghUsers(p.Reviewers), Assignees: ghUsers(p.Assignees),
		ApprovedBy: []string{}, description: p.Body,
	}
	if p.MergedAt != nil {
		r.MergedAt = *p.MergedAt
	}
	return r
}

func (f *radarFetcher) github(ctx context.Context, l *models.QAProjectLink, rl *radarLink) (open, merged []*radarMR, err error) {
	repo, err := f.ghRepoPath(ctx, l.GitProject)
	if err != nil {
		return nil, nil, err
	}
	base := f.h.ghBase(f.conn) + repo
	open = []*radarMR{}
	for page := 1; page <= radarMaxPages; page++ {
		var raw []ghPull
		hdr, gerr := f.get(ctx, base+"/pulls?state=open&per_page=100&sort=updated&direction=desc&page="+strconv.Itoa(page), &raw)
		if gerr != nil {
			return nil, nil, gerr
		}
		for _, p := range raw {
			open = append(open, p.toRadar())
		}
		if ghNextPage(hdr) == 0 || len(raw) == 0 {
			break
		}
		if page == radarMaxPages {
			rl.Truncated = true
		}
	}
	rl.Shown = len(open)
	if !rl.Truncated {
		rl.Total = intp(len(open)) // GitHub sends no total; complete when not truncated
	}
	rl.Incomplete = f.enrichAll(open, func(mf *radarFetcher, m *radarMR) { mf.ghEnrich(ctx, base, l, m) })

	merged = []*radarMR{}
	{
		var raw []ghPull
		hdr, gerr := f.get(ctx, base+"/pulls?state=closed&per_page=100&sort=updated&direction=desc", &raw)
		if gerr == nil {
			for _, p := range raw {
				if p.MergedAt == nil {
					continue
				}
				if t, perr := time.Parse(time.RFC3339, *p.MergedAt); perr == nil && t.Before(f.opts.mergedSince) {
					continue
				}
				merged = append(merged, p.toRadar())
			}
			if ghNextPage(hdr) != 0 && len(raw) > 0 {
				if t, perr := time.Parse(time.RFC3339, raw[len(raw)-1].UpdatedAt); perr == nil && t.After(f.opts.mergedSince) {
					rl.MergedTruncated = true
				}
			}
		}
	}
	return open, merged, nil
}

func (f *radarFetcher) ghEnrich(ctx context.Context, base string, l *models.QAProjectLink, m *radarMR) {
	pr := base + "/pulls/" + strconv.FormatInt(m.IID, 10)
	key := "gh|" + f.conn.ID + "|" + l.GitProject + "|" + strconv.FormatInt(m.IID, 10)

	type ghDetail struct {
		Pipeline     *string
		ChangedFiles *int
		Conflicts    bool
		Status       string
	}
	dk := key + "|d|" + m.UpdatedAt
	apply := func(d ghDetail) {
		m.Pipeline, m.ChangedFiles = d.Pipeline, d.ChangedFiles
		if d.Conflicts {
			m.HasConflicts = true
		}
		m.DetailedMergeStatus = d.Status
	}
	if v, ok := cacheGet(dk); ok {
		apply(v.(ghDetail))
	} else {
		var p ghPull
		if _, err := f.get(ctx, pr, &p); err == nil {
			d := ghDetail{ChangedFiles: intp(p.ChangedFiles), Conflicts: p.hasConflicts()}
			switch p.MergeableState {
			case "unstable":
				d.Pipeline = strp("failed") // non-passing commit status / checks
			case "behind":
				d.Status = "need_rebase"
			case "dirty":
				d.Status = "conflict"
			}
			// mergeable_state is computed lazily by GitHub ("unknown" at
			// first); don't cache an unknown answer.
			if p.MergeableState != "" && p.MergeableState != "unknown" {
				cachePutShort(dk, d)
			}
			apply(d)
		}
	}

	rk := key + "|r|" + m.UpdatedAt
	if v, ok := cacheGet(rk); ok {
		by := v.([]string)
		m.ApprovedBy, m.ApprovalsGiven = by, intp(len(by))
	} else {
		var raw []struct {
			State string `json:"state"`
			User  ghUser `json:"user"`
		}
		if _, err := f.get(ctx, pr+"/reviews?per_page=100", &raw); err == nil {
			last := map[string]string{}
			order := []string{}
			for _, r := range raw {
				if r.State == "COMMENTED" {
					continue
				}
				if _, ok := last[r.User.Login]; !ok {
					order = append(order, r.User.Login)
				}
				last[r.User.Login] = r.State
			}
			by := []string{}
			for _, u := range order {
				if last[u] == "APPROVED" {
					by = append(by, u)
				}
			}
			cachePutShort(rk, by)
			m.ApprovedBy, m.ApprovalsGiven = by, intp(len(by))
		}
	}

	if f.opts.needPaths && m.HeadSHA != "" {
		pk := key + "|p|" + m.HeadSHA
		if v, ok := cacheGet(pk); ok {
			m.paths = v.([]string)
		} else {
			paths := []string{}
			complete := true
			for page := 1; page <= 3; page++ {
				var raw []struct {
					Filename         string `json:"filename"`
					PreviousFilename string `json:"previous_filename"`
				}
				hdr, err := f.get(ctx, pr+"/files?per_page=100&page="+strconv.Itoa(page), &raw)
				if err != nil {
					complete = false
					break
				}
				for _, fl := range raw {
					paths = append(paths, fl.Filename)
					if fl.PreviousFilename != "" {
						paths = append(paths, fl.PreviousFilename)
					}
				}
				if ghNextPage(hdr) == 0 {
					break
				}
			}
			if complete {
				paths = capPaths(paths)
				cachePut(pk, paths)
				m.paths = paths
			}
		}
	}
	if f.opts.needCommit && m.HeadSHA != "" {
		ck := key + "|c|" + m.HeadSHA
		if v, ok := cacheGet(ck); ok {
			m.AIAssisted = boolp(v.(bool))
		} else {
			var raw []struct {
				Commit struct {
					Message string `json:"message"`
				} `json:"commit"`
			}
			if _, err := f.get(ctx, pr+"/commits?per_page=100", &raw); err == nil {
				msgs := make([]string, 0, len(raw))
				for _, c := range raw {
					msgs = append(msgs, c.Commit.Message)
				}
				ai := qalink.AIAssisted(msgs)
				cachePut(ck, ai)
				m.AIAssisted = boolp(ai)
			}
		}
	}
}

// attachReviews marks each open MR with its AI review at the current head.
func (h *QARadarHandler) attachReviews(mrs []*radarMR) {
	if len(mrs) == 0 {
		return
	}
	linkIDs := map[string]bool{}
	for _, m := range mrs {
		linkIDs[m.LinkID] = true
	}
	ids := make([]string, 0, len(linkIDs))
	for id := range linkIDs {
		ids = append(ids, id)
	}
	var rows []models.MRReview
	h.db.Where("link_id IN ? AND status <> ?", ids, "discarded").Find(&rows)
	by := map[string]*models.MRReview{}
	for i := range rows {
		by[rows[i].LinkID+"|"+strconv.FormatInt(rows[i].IID, 10)+"|"+rows[i].HeadSHA] = &rows[i]
	}
	for _, m := range mrs {
		if r, ok := by[m.LinkID+"|"+strconv.FormatInt(m.IID, 10)+"|"+m.HeadSHA]; ok {
			rv := &radarReview{ID: r.ID, Status: r.Status}
			for _, f := range reviewFindings(r) {
				switch f.Status {
				case "pending", "approved":
					rv.Pending++
				case "posted":
					rv.Posted++
				}
			}
			m.Review = rv
		}
	}
	// Fix checks (P4.a): posted findings of an older review, and whether this
	// head was already checked.
	var vrows []models.MRVerification
	h.db.Where("link_id IN ? AND status <> ?", ids, "claimed").Find(&vrows)
	checked := map[string]*models.MRVerification{}
	for i := range vrows {
		v := &vrows[i]
		checked[v.LinkID+"|"+strconv.FormatInt(v.IID, 10)+"|"+v.HeadSHA] = v
	}
	for _, m := range mrs {
		key := m.LinkID + "|" + strconv.FormatInt(m.IID, 10)
		if v, ok := checked[key+"|"+m.HeadSHA]; ok {
			m.VerifyID = v.ID
			if v.Status == "submitted" {
				var vs []models.FindingVerdict
				_ = json.Unmarshal([]byte(v.VerdictsJSON), &vs)
				for _, x := range vs {
					if x.Applied == "" {
						m.VerifyPending++
					}
				}
				m.verifyOwners = append(m.verifyOwners, v.ClaimedBy)
				for i := range rows {
					if rows[i].ID == v.ReviewID {
						m.verifyOwners = append(m.verifyOwners, rows[i].PostedBy, rows[i].ClaimedBy)
					}
				}
			}
			continue
		}
		for i := range rows {
			r := &rows[i]
			if r.Status != "posted" || r.LinkID != m.LinkID || r.IID != m.IID || r.HeadSHA == m.HeadSHA {
				continue
			}
			n := 0
			for _, f := range reviewFindings(r) {
				if f.FindingOpen() {
					n++
				}
			}
			if n > 0 {
				m.OpenFindings += n
				m.verifyOwners = append(m.verifyOwners, r.PostedBy, r.ClaimedBy)
			}
		}
	}
}

// leadTimes: hours from creation to merge of the merged MRs.
func leadTimes(mrs []*radarMR) []float64 {
	out := []float64{}
	for _, m := range mrs {
		c, err1 := time.Parse(time.RFC3339, m.CreatedAt)
		d, err2 := time.Parse(time.RFC3339, m.MergedAt)
		if err1 == nil && err2 == nil && d.After(c) {
			out = append(out, d.Sub(c).Hours())
		}
	}
	sort.Float64s(out)
	return out
}

func pctHours(sorted []float64, p float64) *float64 {
	if len(sorted) == 0 {
		return nil
	}
	i := int(p * float64(len(sorted)-1))
	v := float64(int(sorted[i]*10+0.5)) / 10
	return &v
}

func qaSpaceKey(userID string, ws *string) string {
	if ws != nil && *ws != "" {
		return "ws:" + *ws
	}
	return "user:" + userID
}

// snapshot records the space's counts for today once (Reports' trend).
func (h *QARadarHandler) snapshot(userID string, ws *string, res *radarResult, now time.Time) {
	// Only a complete view counts: a member who can't read every link would
	// record a partial (too low) day for the whole team.
	if len(res.Links) == 0 {
		return
	}
	for _, l := range res.Links {
		if l.Status != "ok" || l.Truncated || l.Incomplete > 0 {
			return
		}
	}
	day := now.UTC().Format("2006-01-02")
	key := qaSpaceKey(userID, ws)
	var n int64
	h.db.Model(&models.QARadarSnapshot{}).Where("space_key = ? AND day = ?", key, day).Count(&n)
	if n > 0 {
		return
	}
	counts := map[string]int{}
	for _, m := range res.MRs {
		for _, s := range m.Signals {
			counts[s.Kind]++
		}
	}
	b, _ := json.Marshal(counts)
	lt := leadTimes(res.mergedAll)
	_ = h.db.Create(&models.QARadarSnapshot{ID: uuid.NewString(), SpaceKey: key, Day: day, OpenMRs: len(res.MRs),
		SignalsJSON: string(b), Merged: len(res.mergedAll), LeadMedianH: pctHours(lt, 0.5), LeadP90H: pctHours(lt, 0.9)}).Error
}
