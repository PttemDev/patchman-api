// Package notecards holds the pure parts of agent memory — knowledge cards
// inside Notes: link types, short ids, link extraction, the secret filter,
// how a remembered text becomes a card, and how recall prints its answer.
// Everything that touches the database or permissions lives in the notes
// handler; nothing here keeps state, so there is nothing to lock.
package notecards

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// LinkTypes are the edge types a note link may carry, in the order shown in
// errors and pickers.
var LinkTypes = []string{"depends_on", "part_of", "contradicts", "supersedes", "example_of", "related", "refers_to"}

// ValidLinkType reports whether t is one of LinkTypes.
func ValidLinkType(t string) bool {
	for _, v := range LinkTypes {
		if v == t {
			return true
		}
	}
	return false
}

// LinkTypeError is the one-line answer to a bad link type.
func LinkTypeError(t string) error {
	return fmt.Errorf("type %q is not a link type; type must be one of: %s", t, strings.Join(LinkTypes, ", "))
}

// ShortID is the id recall prints: the last 8 hex characters of the UUID —
// random in both UUID versions, so collisions are rare and resolvable.
func ShortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[len(id)-8:]
}

var shortIDRE = regexp.MustCompile(`^[0-9a-f]{8}$`)

// IsShortID reports whether s looks like a ShortID (after an optional '#').
func IsShortID(s string) bool { return shortIDRE.MatchString(strings.TrimPrefix(s, "#")) }

var bodyLinkRE = regexp.MustCompile(`note:([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})(?:#[^\s)"]*)?(?:\s+"([a-z_]+)")?`)

// BodyLink is a [text](note:<id>) link in a markdown body. A card's links may
// carry their type as the link title: [text](note:<id> "depends_on").
type BodyLink struct {
	ID   string
	Type string // "related" unless the title names a link type
}

// BodyNoteLinks returns the distinct (note, type) links of a markdown body,
// in order of first appearance.
func BodyNoteLinks(body string) []BodyLink {
	var out []BodyLink
	seen := map[BodyLink]bool{}
	for _, m := range bodyLinkRE.FindAllStringSubmatch(body, -1) {
		l := BodyLink{ID: m[1], Type: "related"}
		if ValidLinkType(m[2]) && m[2] != "refers_to" {
			l.Type = m[2]
		}
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	return out
}

var uriRE = regexp.MustCompile(`patchman://[^\s,;)"'<>]+`)

// ContextURIs returns the distinct patchman:// entity URIs in a remember()
// context string.
func ContextURIs(context string) []string {
	var out []string
	seen := map[string]bool{}
	for _, u := range uriRE.FindAllString(context, -1) {
		u = strings.TrimRight(u, ".")
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

// IsEntityURI reports whether s addresses a PatchMan entity.
func IsEntityURI(s string) bool {
	return strings.HasPrefix(s, "patchman://") && len(s) > len("patchman://")
}

var secretPatterns = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"AWS access key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"GitLab token", regexp.MustCompile(`\bglpat-[0-9A-Za-z_\-]{20,}`)},
	{"GitHub token", regexp.MustCompile(`\bgh[pousr]_[0-9A-Za-z]{30,}`)},
	{"Slack token", regexp.MustCompile(`\bxox[abprs]-[0-9A-Za-z-]{10,}`)},
	{"API key", regexp.MustCompile(`\bsk-(ant-)?[0-9A-Za-z_\-]{20,}`)},
	{"JWT", regexp.MustCompile(`\beyJ[0-9A-Za-z_-]{10,}\.[0-9A-Za-z_-]{10,}\.[0-9A-Za-z_-]{10,}`)},
}

// keyValueRE finds "name: value" / NAME=value pairs whose name says it holds
// a credential — DB_PASSWORD=…, JWT_SECRET=…, Pass: …, token: …, Şifre: … —
// and urlCredRE a password inside a URL (scheme://user:pass@host). Only a
// value that looks like a real secret counts (see secretValue), so
// "token: string" or "user:pass@host" placeholders in docs pass.
var (
	keyValueRE = regexp.MustCompile(`(?i)(?:^|[^\pL\pN])[\pL\pN_-]*?(pass(?:word|wd)?|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|enc(?:ryption)?[_-]?key|credentials?|şifre|sifre|parola)[\pL\pN_-]*\s*[:=]\s*['"]?([^\s'"]+)`)
	urlCredRE  = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s/:@]+:([^\s/@]+)@`)
)

var headerNameRE = regexp.MustCompile(`^[A-Za-z][a-z]*(-[A-Za-z][a-z]*)+$`)

// placeholderValues are what docs write instead of a secret.
var placeholderValues = map[string]bool{
	"password": true, "passwd": true, "pass": true, "secret": true, "token": true, "changeme": true, "example": true,
	"redacted": true, "hidden": true, "masked": true, "none": true, "null": true, "true": true, "false": true, "string": true,
}

// secretValue: v looks like a real credential — 6+ characters, not a
// placeholder (…, ***, <x>, ${X}, {{x}}, $(cmd)), and either mixing two kinds
// of character (lower, upper, digit, symbol other than - _ . /) or 24+ long.
func secretValue(v string) bool {
	v = strings.TrimRight(v, ",;)]}`.")
	if utf8.RuneCountInString(v) < 6 || placeholderValues[strings.ToLower(v)] {
		return false
	}
	if strings.ContainsAny(v[:1], "$<{%*.`") || strings.Contains(v, "...") || strings.Contains(v, "…") || strings.Contains(v, "***") {
		return false
	}
	if headerNameRE.MatchString(v) { // X-Api-Key, X-Refreshed-Token: a name
		return false
	}
	var lower, upper, digit, symbol bool
	for _, r := range v {
		switch {
		case unicode.IsLower(r):
			lower = true
		case unicode.IsUpper(r):
			upper = true
		case unicode.IsDigit(r):
			digit = true
		case !strings.ContainsRune("-_./", r):
			symbol = true
		}
	}
	kinds := 0
	for _, k := range []bool{lower, upper, digit, symbol} {
		if k {
			kinds++
		}
	}
	return kinds >= 2 || utf8.RuneCountInString(v) >= 24
}

// SecretKind names the kind of credential text seems to contain, or "" —
// memories are shared and long-lived, so they never hold one.
func SecretKind(text string) string {
	for _, p := range secretPatterns {
		if p.re.MatchString(text) {
			return p.kind
		}
	}
	for _, m := range keyValueRE.FindAllStringSubmatch(text, -1) {
		if secretValue(m[2]) {
			return "password or key"
		}
	}
	for _, m := range urlCredRE.FindAllStringSubmatch(text, -1) {
		if secretValue(m[1]) {
			return "password in a URL"
		}
	}
	return ""
}

// DeriveCard turns remembered text into a card without a model: a first
// line followed by more text is a heading (title) over a body, and the
// summary is the body's first sentence; a single paragraph gives its first
// words as the title and its first sentence as the summary. A summary that
// only repeats the title is dropped.
func DeriveCard(text string) (title, summary, body string) {
	t := strings.TrimSpace(text)
	first, rest, _ := strings.Cut(t, "\n")
	first = strings.TrimSpace(strings.TrimLeft(first, "#-* \t"))
	rest = strings.TrimSpace(rest)
	if rest != "" {
		title = clip(first, 120)
		summary = clip(FirstSentence(rest), 200)
		body = rest
		// A first paragraph that is just the summary sentence would show twice
		// (summary line + body); the body keeps the rest.
		if para, more, ok := strings.Cut(rest, "\n\n"); ok && strings.TrimSpace(more) != "" &&
			strings.Join(strings.Fields(para), " ") == summary {
			body = strings.TrimSpace(more)
		}
	} else {
		title = clip(firstWords(first, 10), 120)
		summary = clip(FirstSentence(first), 200)
		body = t
	}
	title, summary = PlainWiki(title), PlainWiki(summary)
	if strings.EqualFold(strings.TrimSpace(summary), strings.TrimSpace(title)) {
		summary = ""
	}
	return title, summary, body
}

// PlainWiki shows [[Target|label]] / [[Target]] as its label (one-line
// fields like a title or summary have no links).
func PlainWiki(s string) string {
	return wikiLinkRE.ReplaceAllStringFunc(s, func(m string) string {
		inner := strings.TrimSuffix(strings.TrimPrefix(m, "[["), "]]")
		if _, label, ok := strings.Cut(inner, "|"); ok && strings.TrimSpace(label) != "" {
			return strings.TrimSpace(label)
		}
		return strings.TrimSpace(inner)
	})
}

// ReplaceWiki rewrites each [[Target]] / [[Target|label]] in body with what
// to(target, label) returns ("" keeps it as written).
func ReplaceWiki(body string, to func(target, label string) string) string {
	return wikiLinkRE.ReplaceAllStringFunc(body, func(m string) string {
		inner := strings.TrimSuffix(strings.TrimPrefix(m, "[["), "]]")
		target, label, _ := strings.Cut(inner, "|")
		target = strings.TrimSpace(target)
		if strings.TrimSpace(label) == "" {
			label = target
		}
		if r := to(target, strings.TrimSpace(label)); r != "" {
			return r
		}
		return m
	})
}

// FirstSentence returns text up to and including its first sentence end
// (. ! ? followed by a space or the end), on one line.
func FirstSentence(text string) string {
	s := strings.Join(strings.Fields(text), " ")
	for i := 0; i < len(s); i++ {
		if (s[i] == '.' || s[i] == '!' || s[i] == '?') && (i+1 == len(s) || s[i+1] == ' ') {
			return s[:i+1]
		}
	}
	return s
}

func firstWords(s string, n int) string {
	f := strings.Fields(s)
	if len(f) <= n {
		return strings.Join(f, " ")
	}
	return strings.Join(f[:n], " ") + "…"
}

func clip(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	cut := string(r[:max-1])
	if i := strings.LastIndexByte(cut, ' '); i > max/2 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut) + "…"
}

// Rel is one link shown under a recall hit.
type Rel struct {
	Out   bool   // true: hit → other; false: other → hit
	Type  string // link type
	ID    string // other note's id ("" for an entity URI)
	Title string // other note's title, or the entity URI
}

// Hit is one note recall returns.
type Hit struct {
	ID    string
	Title string
	Line  string // summary, or a body snippet for a note without one
	Body  string // markdown body (shown for the best hit only)
	Scope string // whose memory it is, when not the caller's own ("team: Backend")
	// Project is the project the note belongs to, when it isn't the one the
	// agent is working in ("" then).
	Project string
	Score   float64
	Rels    []Rel
}

// NoMatch is recall's answer when nothing fits.
const NoMatch = "No match. If you learn this, remember() it."

// Render prints recall's answer within budget tokens (≈ 4 characters each):
// one "#id title | summary" line per hit with its links indented under it,
// then the best hit's body between --- lines (at most half the budget),
// then any hint: lines. Lines that would overflow the budget are left out.
func Render(hits []Hit, budget int, hints []string) string {
	return RenderFor(hits, budget, hints, nil)
}

// RenderFor is Render for a question's search terms. The best hit comes
// first with its links and its body (up to half the budget — room for it is
// kept before the other hits are listed, so a long hit list can't crowd the
// answer out); a body too long for its room is shown as its paragraphs that
// match the terms (in order, gaps marked …) rather than its head. Then the
// other hits, one line each, and the hint: lines (their room kept too).
func RenderFor(hits []Hit, budget int, hints []string, terms []string) string {
	if len(hits) == 0 {
		return NoMatch
	}
	limit := budget * 4
	var hintLines []string
	hintRoom := 0
	for _, h := range hints {
		line := "hint: " + h
		if hintRoom+len(line)+1 > limit/5 {
			break
		}
		hintLines = append(hintLines, line)
		hintRoom += len(line) + 1
	}
	var b strings.Builder
	write := func(line string, reserve int) bool {
		if b.Len()+len(line)+1+reserve > limit {
			return false
		}
		b.WriteString(line)
		b.WriteByte('\n')
		return true
	}
	writeHit := func(h Hit, reserve int) bool {
		if !write(hitHead(h), reserve) {
			return false
		}
		for i, r := range h.Rels {
			if i == 4 {
				break
			}
			arrow := "←"
			if r.Out {
				arrow = "→"
			}
			target := r.Title
			if r.ID != "" {
				target = "#" + ShortID(r.ID) + " " + r.Title
			}
			write("  "+arrow+" "+r.Type+" "+target, reserve)
		}
		return true
	}
	writeHit(hits[0], hintRoom)
	if body := strings.TrimSpace(hits[0].Body); body != "" && body != hits[0].Line {
		room := limit - b.Len() - hintRoom - 16
		if half := limit / 2; room > half {
			room = half
		}
		if room > 80 {
			if len(body) > room {
				body = Excerpt(body, terms, room)
			}
			b.WriteString("---\n" + body + "\n---\n")
		}
	}
	for _, h := range hits[1:] {
		if !writeHit(h, hintRoom) {
			break
		}
	}
	for _, line := range hintLines {
		write(line, 0)
	}
	return strings.TrimRight(b.String(), "\n")
}

// hitHead is a hit's "#id title | summary [tags]" line.
func hitHead(h Hit) string {
	head := "#" + ShortID(h.ID) + " " + h.Title
	if h.Line != "" {
		head += " | " + h.Line
	}
	var tags []string
	if h.Project != "" {
		tags = append(tags, "project: "+h.Project)
	}
	if h.Scope != "" {
		tags = append(tags, h.Scope)
	}
	if len(tags) > 0 {
		head += " [" + strings.Join(tags, " · ") + "]"
	}
	return head
}

// clipBytes cuts s to at most n bytes on a rune boundary.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Excerpt fits body into room bytes: the paragraphs that match the most
// terms (ties: earlier first), shown in their original order with "…" where
// paragraphs were left out; the head of the body when nothing matches.
// terms are lower-case and folded like notesearch.Terms.
func Excerpt(body string, terms []string, room int) string {
	if len(body) <= room {
		return body
	}
	paras := strings.Split(body, "\n\n")
	type scored struct{ i, n int }
	var order []scored
	for i, p := range paras {
		lp := strings.ToLower(fold(p))
		n := 0
		for _, t := range terms {
			if strings.Contains(lp, t) {
				n++
			}
		}
		if n > 0 {
			order = append(order, scored{i, n})
		}
	}
	if len(order) == 0 {
		return clipBytes(body, room) + "\n…"
	}
	sort.SliceStable(order, func(a, b int) bool { return order[a].n > order[b].n })
	keep := map[int]bool{}
	used := 0
	for _, o := range order {
		cost := len(paras[o.i]) + 4
		if used+cost > room {
			if used == 0 { // the best paragraph alone is too long: clip it
				return clipBytes(paras[o.i], room) + "\n…"
			}
			continue
		}
		keep[o.i] = true
		used += cost
	}
	var b strings.Builder
	last := -1
	for i, p := range paras {
		if !keep[i] {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		if i != last+1 {
			b.WriteString("…\n\n")
		}
		b.WriteString(p)
		last = i
	}
	if last != len(paras)-1 {
		b.WriteString("\n\n…")
	}
	return b.String()
}

// fold maps Turkish letters to ASCII like notesearch.Fold (kept here so this
// package stays free of database code).
var fold = strings.NewReplacer("Ç", "C", "Ğ", "G", "İ", "I", "I", "I", "Ö", "O", "Ş", "S", "Ü", "U", "Â", "A", "Î", "I", "Û", "U",
	"ç", "c", "ğ", "g", "ı", "i", "ö", "o", "ş", "s", "ü", "u", "â", "a", "î", "i", "û", "u").Replace

var wikiLinkRE = regexp.MustCompile(`\[\[([^\[\]\n|]{1,120})(?:\|[^\]\n]*)?\]\]`)

// WikiLinks returns the distinct [[Target]] names in a remembered text
// (Obsidian style, "[[Target|label]]" too): note titles or short ids
// ("[[#abcd1234]]") that remember links the new card to.
func WikiLinks(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range wikiLinkRE.FindAllStringSubmatch(text, -1) {
		name := strings.TrimSpace(m[1])
		if name != "" && !seen[strings.ToLower(name)] {
			seen[strings.ToLower(name)] = true
			out = append(out, name)
		}
	}
	return out
}

// ProjectKey normalizes a project name (a directory or repo name) into the
// key cards are grouped by: lower-case letters, digits, '.', '_' and '-',
// at most 64 characters; "" when nothing usable is left.
func ProjectKey(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(fold(strings.TrimSpace(name))) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	k := strings.Trim(b.String(), "-.")
	if len(k) > 64 {
		k = strings.Trim(k[:64], "-.")
	}
	return k
}
