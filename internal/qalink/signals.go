package qalink

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MR Radar signals (CR_QA_P0_PLAN P0.c). Pure functions over facts the
// radar fetched; `now` is passed in. A fact the radar did not compute is
// nil, and a signal that needs it is skipped (never reported as false). A
// threshold left empty turns its signal off (O6). Signals describe an MR,
// never a person: nothing here aggregates per author.

// Signal kinds.
const (
	SigAwaitingReview = "awaitingReview"
	SigStuck          = "stuck"
	SigLarge          = "large"
	SigNoTests        = "noTests"
	SigNoJira         = "noJira"
	SigJiraMismatch   = "jiraMismatch"
	SigAIAssisted     = "aiAssisted"
	SigCollision      = "collision"
	// SigUnverifiedFixes: posted findings and new commits, but no fix check yet (P4.a).
	SigUnverifiedFixes = "unverifiedFixes"
)

// AllSignals in display order.
var AllSignals = []string{SigStuck, SigUnverifiedFixes, SigAwaitingReview, SigNoJira, SigJiraMismatch, SigNoTests, SigLarge, SigCollision, SigAIAssisted}

// MRFacts is what the radar knows about one MR.
type MRFacts struct {
	State               string // opened | merged
	Draft               bool
	HasConflicts        bool
	DetailedMergeStatus string // GitLab detailed_merge_status ("" unknown)
	Pipeline            string // success | failed | running | … ("" unknown)
	UnresolvedThreads   *bool
	ApprovalsGiven      *int
	ChangedFiles        *int
	Paths               []string // nil = not fetched
	AIAssisted          *bool
	UpdatedAt           time.Time
	// Jira: JiraLinked = the link names a Jira project; JiraKey = the key
	// found in the MR ("" none); Jira = the issue (nil = not looked up or
	// not found, JiraMissing says which).
	JiraLinked  bool
	JiraKey     string
	Jira        *JiraFacts
	JiraMissing bool
}

// JiraFacts is the linked issue's state.
type JiraFacts struct {
	Status         string
	StatusCategory string // new | indeterminate | done
}

// Settings are the workspace's radar thresholds.
type Settings struct {
	SLAHours       int
	LargeFiles     int
	SourcePatterns []string
	TestPatterns   []string
	Disabled       map[string]bool
}

// Signal is one finding with a short machine-readable reason.
type Signal struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason,omitempty"`
}

func (s Settings) on(kind string) bool { return !s.Disabled[kind] }

// Signals evaluates every per-MR signal (collision is cross-MR: Collisions).
func Signals(mr MRFacts, s Settings, now time.Time) []Signal {
	out := []Signal{}
	add := func(kind, reason string) {
		if s.on(kind) {
			out = append(out, Signal{Kind: kind, Reason: reason})
		}
	}
	open := mr.State == "opened"
	if open {
		if r := stuckReasons(mr); len(r) > 0 {
			add(SigStuck, strings.Join(r, ","))
		}
		if s.SLAHours > 0 && !mr.Draft && mr.ApprovalsGiven != nil && *mr.ApprovalsGiven == 0 && !mr.UpdatedAt.IsZero() &&
			now.Sub(mr.UpdatedAt) >= time.Duration(s.SLAHours)*time.Hour {
			add(SigAwaitingReview, "idle>"+strconv.Itoa(s.SLAHours)+"h")
		}
		if s.LargeFiles > 0 && mr.ChangedFiles != nil && *mr.ChangedFiles >= s.LargeFiles {
			add(SigLarge, strconv.Itoa(*mr.ChangedFiles)+" files")
		}
		if len(s.TestPatterns) > 0 && mr.Paths != nil && len(mr.Paths) > 0 && noTests(mr.Paths, s.SourcePatterns, s.TestPatterns) {
			add(SigNoTests, "")
		}
		if mr.AIAssisted != nil && *mr.AIAssisted {
			add(SigAIAssisted, "trailer")
		}
	}
	if mr.JiraLinked {
		switch {
		case mr.JiraKey == "":
			if open {
				add(SigNoJira, "no key")
			}
		case mr.JiraMissing:
			add(SigNoJira, mr.JiraKey+" not found")
		case mr.Jira != nil:
			done := mr.Jira.StatusCategory == "done"
			if open && done {
				add(SigJiraMismatch, "jira done, MR open")
			} else if mr.State == "merged" && !done && mr.Jira.StatusCategory != "" {
				add(SigJiraMismatch, "MR merged, jira "+mr.Jira.Status)
			}
		}
	}
	return out
}

func stuckReasons(mr MRFacts) []string {
	r := []string{}
	if mr.HasConflicts || mr.DetailedMergeStatus == "conflict" {
		r = append(r, "conflict")
	}
	if mr.Pipeline == "failed" || mr.Pipeline == "canceled" {
		r = append(r, "pipeline")
	}
	if mr.UnresolvedThreads != nil && *mr.UnresolvedThreads {
		r = append(r, "threads")
	}
	if mr.DetailedMergeStatus == "need_rebase" {
		r = append(r, "rebase")
	}
	return r
}

// noTests: source files changed (any non-test file when no source patterns
// are set) and no test file changed.
func noTests(paths, source, tests []string) bool {
	src := compileGlobs(source)
	tst := compileGlobs(tests)
	changedSrc := false
	for _, p := range paths {
		if matchAny(tst, p) {
			return false
		}
		if len(src) == 0 || matchAny(src, p) {
			changedSrc = true
		}
	}
	return changedSrc
}

// Collisions maps an MR index to the other MR indexes that touch a shared
// file. groups[i] = the project of MR i (only MRs of one project collide);
// MRs whose paths are nil are skipped.
func Collisions(groups []string, paths [][]string) map[int][]int {
	owner := map[string][]int{} // project\x00path → MR indexes
	for i, ps := range paths {
		for _, p := range ps {
			k := groups[i] + "\x00" + p
			owner[k] = append(owner[k], i)
		}
	}
	set := map[int]map[int]bool{}
	for _, idx := range owner {
		if len(idx) < 2 {
			continue
		}
		for _, a := range idx {
			for _, b := range idx {
				if a != b {
					if set[a] == nil {
						set[a] = map[int]bool{}
					}
					set[a][b] = true
				}
			}
		}
	}
	out := map[int][]int{}
	for a, bs := range set {
		for b := range bs {
			out[a] = append(out[a], b)
		}
		sort.Ints(out[a])
	}
	return out
}

// aiTrailer matches commit trailers / footers AI coding tools add. A
// heuristic: shown as "possibly AI-assisted" (O10).
var aiTrailer = regexp.MustCompile(`(?im)^(co-authored-by:.*\b(claude|copilot|cursor|codex|gemini|chatgpt|openai|devin|aider|windsurf|codeium)\b|.*generated with \[?(claude code|cursor|copilot|aider)|🤖 generated with)`)

// AIAssisted reports whether any commit message carries such a trailer.
func AIAssisted(messages []string) bool {
	for _, m := range messages {
		if aiTrailer.MatchString(m) {
			return true
		}
	}
	return false
}

// ── globs ─────────────────────────────────────────────────────────────

// compileGlobs turns path globs into anchored regexps: ** = any depth,
// * = within a segment, ? = one char. A pattern without "/" matches the
// file name at any depth (like .gitignore). Invalid patterns are dropped.
func compileGlobs(pats []string) []*regexp.Regexp {
	out := []*regexp.Regexp{}
	for _, p := range pats {
		p = strings.TrimSpace(p)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		if !strings.Contains(p, "/") {
			p = "**/" + p
		}
		var b strings.Builder
		b.WriteString("^")
		for i := 0; i < len(p); i++ {
			ch := p[i]
			switch {
			case ch == '*' && i+1 < len(p) && p[i+1] == '*':
				i++
				if i+1 < len(p) && p[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			case ch == '*':
				b.WriteString("[^/]*")
			case ch == '?':
				b.WriteString("[^/]")
			default:
				b.WriteString(regexp.QuoteMeta(string(ch)))
			}
		}
		b.WriteString("$")
		if re, err := regexp.Compile(b.String()); err == nil {
			out = append(out, re)
		}
	}
	return out
}

func matchAny(res []*regexp.Regexp, p string) bool {
	for _, re := range res {
		if re.MatchString(p) {
			return true
		}
	}
	return false
}

// SplitPatterns splits a settings text (newlines or commas) into patterns.
func SplitPatterns(s string) []string {
	out := []string{}
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == ',' || r == '\r' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
