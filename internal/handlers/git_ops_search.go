package handlers

import (
	"context"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Code search over the local clone (GIT_IDE_PLAN §F, §G): Find in Files
// (git grep, working tree incl. untracked, binary skipped), file finder
// (ls-files, fuzzy ranked) and a regex-based symbol index for Search
// Everywhere and Go to Declaration. Every search is capped and time-boxed.

const (
	grepTimeout  = 10 * time.Second
	grepMaxHits  = 2000
	grepMaxLine  = 400
	fileMaxHits  = 60
	symbolMaxHit = 200
)

type grepHit struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Col  int    `json:"col"`
	Text string `json:"text"`
}

// runGrep runs git grep with -z output and parses path\0line\0col\0text.
func (h *GitOpsHandler) runGrep(ctx context.Context, dir string, args []string, limit int) ([]grepHit, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, grepTimeout)
	defer cancel()
	base := []string{"-C", dir, "-c", "core.quotepath=false", "grep", "-n", "--column", "-I", "-z", "--untracked", "--no-color"}
	out, stderr, err := h.runGit(ctx, nil, append(base, args...)...)
	if err != nil {
		// Exit 1 = no match.
		if strings.TrimSpace(stderr) == "" {
			return []grepHit{}, false, nil
		}
		return nil, false, err
	}
	hits := []grepHit{}
	truncated := false
	for _, l := range strings.Split(out, "\n") {
		if l == "" {
			continue
		}
		f := strings.SplitN(l, "\x00", 4)
		if len(f) < 4 {
			continue
		}
		ln, _ := strconv.Atoi(f[1])
		col, _ := strconv.Atoi(f[2])
		text := f[3]
		if len(text) > grepMaxLine {
			text = text[:grepMaxLine] + "…"
		}
		hits = append(hits, grepHit{Path: f[0], Line: ln, Col: col, Text: text})
		if len(hits) >= limit {
			truncated = true
			break
		}
	}
	return hits, truncated, nil
}

// Grep — GET /git/repos/:repoId/grep?q=&regex=1&case=1&word=1&glob=
func (h *GitOpsHandler) Grep(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	q := c.Query("q")
	if strings.TrimSpace(q) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "q is required"})
		return
	}
	args := []string{}
	if isTrue(c.Query("regex")) {
		args = append(args, "-E")
	} else {
		args = append(args, "-F")
	}
	if !isTrue(c.Query("case")) {
		args = append(args, "-i")
	}
	if isTrue(c.Query("word")) {
		args = append(args, "-w")
	}
	args = append(args, "-e", q, "--")
	for _, g := range strings.Split(c.Query("glob"), ",") {
		if g = strings.TrimSpace(g); g != "" {
			if strings.HasPrefix(g, "!") {
				args = append(args, ":(exclude,glob)"+strings.TrimPrefix(g, "!"))
			} else {
				if !strings.Contains(g, "/") {
					g = "**/" + g
				}
				args = append(args, ":(glob)"+g)
			}
		}
	}
	hits, truncated, err := h.runGrep(c.Request.Context(), repo.LocalPath, args, grepMaxHits)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "search: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"hits": hits, "truncated": truncated}})
}

// Files — GET /git/repos/:repoId/files?q=. Tracked + untracked (not ignored)
// paths ranked by a subsequence match on the name, then the path.
func (h *GitOpsHandler) Files(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), grepTimeout)
	defer cancel()
	out, stderr, err := h.runGit(ctx, nil, "-C", repo.LocalPath, "-c", "core.quotepath=false", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "git ls-files: " + gitErrMsg(stderr, err)})
		return
	}
	q := strings.ToLower(strings.TrimSpace(c.Query("q")))
	type scored struct {
		path  string
		score int
	}
	seen := map[string]bool{}
	var list []scored
	for _, p := range strings.Split(out, "\x00") {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		if q == "" {
			list = append(list, scored{p, len(p)})
			continue
		}
		if s, ok := fuzzyScore(q, p); ok {
			list = append(list, scored{p, s})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].score != list[j].score {
			return list[i].score < list[j].score
		}
		return list[i].path < list[j].path
	})
	if len(list) > fileMaxHits {
		list = list[:fileMaxHits]
	}
	res := make([]string, len(list))
	for i, s := range list {
		res[i] = s.path
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

// fuzzyScore: q must be a subsequence of the path. Lower is better: a match
// inside the base name, contiguous runs and early starts win.
func fuzzyScore(q, path string) (int, bool) {
	lp := strings.ToLower(path)
	base := strings.ToLower(filepath.Base(path))
	if i := strings.Index(base, q); i >= 0 {
		return i + len(base) - len(q), true
	}
	if i := strings.Index(lp, q); i >= 0 {
		return 100 + i, true
	}
	score, pos, last := 200, 0, -2
	for _, r := range q {
		i := strings.IndexRune(lp[pos:], r)
		if i < 0 {
			return 0, false
		}
		at := pos + i
		if at != last+1 {
			score += 10
		}
		score += i
		last, pos = at, at+1
	}
	return score, true
}

// Definition patterns per language family. NAME is replaced by the escaped
// identifier (or an identifier-prefix pattern for prefix search).
var symbolPatterns = []struct {
	kind, lang, re string
	exts           []string
}{
	{"func", "go", `^[[:space:]]*func[[:space:]]+(\([^)]*\)[[:space:]]*)?NAME[[:space:]]*[(\[]`, []string{"go"}},
	{"type", "go", `^[[:space:]]*(type[[:space:]]+)?NAME[[:space:]]+(struct|interface|func|map|chan|\[|[A-Za-z*])`, []string{"go"}},
	{"var", "go", `^[[:space:]]*(var|const)[[:space:]]+NAME([^A-Za-z0-9_]|$)`, []string{"go"}},
	{"func", "ts", `(function\*?|def|fn)[[:space:]]+NAME([^A-Za-z0-9_]|$)`, []string{"ts", "tsx", "js", "jsx", "mjs", "cjs", "py", "rs"}},
	{"class", "ts", `(class|interface|enum|type|struct|trait|record|object|module|mod)[[:space:]]+NAME([^A-Za-z0-9_]|$)`, []string{"ts", "tsx", "js", "jsx", "py", "rs", "java", "kt", "cs", "scala", "swift", "go"}},
	{"const", "ts", `(const|let|var|export[[:space:]]+const)[[:space:]]+NAME[[:space:]]*[=:]`, []string{"ts", "tsx", "js", "jsx", "mjs", "cjs"}},
	{"method", "ts", `^[[:space:]]*(public |private |protected |static |async |readonly |override |abstract |get |set )*NAME[[:space:]]*(<[^>]*>)?\([^)]*\)[[:space:]]*(:[^={]*)?\{`, []string{"ts", "tsx", "js", "jsx"}},
	{"method", "java", `^[[:space:]]*((public|private|protected|internal|static|final|abstract|override|virtual|async|suspend|fun)[[:space:]]+)+[A-Za-z0-9_<>,.?\[\] ]*NAME[[:space:]]*\(`, []string{"java", "kt", "cs", "scala"}},
}

type symbolHit struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

var identRe = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

// Symbols — GET /git/repos/:repoId/symbols?q=&exact=1. Definitions whose
// name is q (exact) or starts with q.
func (h *GitOpsHandler) Symbols(c *gin.Context) {
	repo, ok := h.resolveRepo(c)
	if !ok {
		return
	}
	q := strings.TrimSpace(c.Query("q"))
	if !identRe.MatchString(q) {
		c.JSON(http.StatusOK, gin.H{"data": []symbolHit{}})
		return
	}
	exact := isTrue(c.Query("exact"))
	name := regexp.QuoteMeta(q)
	name = strings.ReplaceAll(name, "$", `\$`)
	if !exact {
		name += `[A-Za-z0-9_$]*`
	}
	nameRe := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(q) + `[A-Za-z0-9_$]*`)
	if exact {
		nameRe = regexp.MustCompile(`\b` + regexp.QuoteMeta(q) + `\b`)
	}
	seen := map[string]bool{}
	out := []symbolHit{}
	for _, p := range symbolPatterns {
		args := []string{"-E"}
		if !exact {
			args = append(args, "-i")
		}
		args = append(args, "-e", strings.ReplaceAll(p.re, "NAME", name), "--")
		for _, e := range p.exts {
			args = append(args, ":(glob)**/*."+e)
		}
		hits, _, err := h.runGrep(c.Request.Context(), repo.LocalPath, args, symbolMaxHit)
		if err != nil {
			continue
		}
		for _, hit := range hits {
			key := hit.Path + ":" + strconv.Itoa(hit.Line)
			if seen[key] {
				continue
			}
			seen[key] = true
			nm := nameRe.FindString(hit.Text)
			if nm == "" {
				nm = q
			}
			out = append(out, symbolHit{Name: nm, Kind: p.kind, Path: hit.Path, Line: hit.Line, Text: strings.TrimSpace(hit.Text)})
		}
		if len(out) >= symbolMaxHit {
			break
		}
	}
	// Exact name first, then shorter names, then path.
	sort.SliceStable(out, func(i, j int) bool {
		ei, ej := strings.EqualFold(out[i].Name, q), strings.EqualFold(out[j].Name, q)
		if ei != ej {
			return ei
		}
		if len(out[i].Name) != len(out[j].Name) {
			return len(out[i].Name) < len(out[j].Name)
		}
		return out[i].Path < out[j].Path
	})
	c.JSON(http.StatusOK, gin.H{"data": out})
}
