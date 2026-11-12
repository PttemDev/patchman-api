// Package notesearch is full-text search over Notes, portable across the two
// databases PatchMan runs on:
//
//   - Postgres (hosted): a generated tsvector column on notes ('simple' config,
//     title above a card's summary above body) with a GIN index. The column is
//     kept OUT of the GORM model, so AutoMigrate never touches it.
//   - SQLite (desktop embedded BE): a standalone FTS5 table (unicode61 with
//     diacritics folding, so "odeme" finds "Ödeme"), kept in step by Index /
//     Remove on every write. FTS5 finds rows fast only by MATCH or rowid, so
//     notes_fts_map gives each note a stable FTS rowid; looking a note up by
//     its (unindexed) note_id column would scan the whole index per save.
//
// Ranking happens in SQL; permissions are filtered by the caller (a note is
// visible through grants on any ancestor, which SQL can't cheaply express).
package notesearch

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"gorm.io/gorm"
)

func isPostgres(db *gorm.DB) bool { return db.Dialector.Name() == "postgres" }

// Turkish (and a few Latin) letters folded to ASCII, so "cagri" finds "Çağrı".
// FTS5's remove_diacritics handles ç/ğ/ö/ş/ü but not the dotless ı, and
// Postgres' 'simple' config folds nothing — so both sides fold the same way:
// Go (Fold) for SQLite and the query terms, SQL translate() for Postgres.
// Every mapping is one rune to one rune, so offsets survive folding.
const (
	foldFrom = "ÇĞİIÖŞÜÂÎÛçğıöşüâîû"
	foldTo   = "CGIIOSUAIUcgiosuaiu"
)

var folder = func() *strings.Replacer {
	from, to := []rune(foldFrom), []rune(foldTo)
	pairs := make([]string, 0, 2*len(from))
	for i := range from {
		pairs = append(pairs, string(from[i]), string(to[i]))
	}
	return strings.NewReplacer(pairs...)
}()

// Fold maps the letters above to ASCII (case preserved).
func Fold(s string) string { return folder.Replace(s) }

// Ensure creates the search structures. Idempotent; runs on every boot after
// AutoMigrate.
func Ensure(db *gorm.DB) error {
	if isPostgres(db) {
		// Only a prefix of the body is indexed: to_tsvector errors past ~1 MB
		// of lexemes, and in a STORED generated column that would fail the
		// note's INSERT/UPDATE itself. ADD COLUMN IF NOT EXISTS never updates
		// an existing column, so a column from before the summary existed is
		// dropped (its index goes with it) and re-added once.
		var expr string
		db.Raw(`SELECT coalesce(generation_expression, '') FROM information_schema.columns
			WHERE table_name = 'notes' AND column_name = 'search_tsv'`).Scan(&expr)
		if expr != "" && !strings.Contains(expr, "summary") {
			if err := db.Exec(`ALTER TABLE notes DROP COLUMN search_tsv`).Error; err != nil {
				return fmt.Errorf("notes search column upgrade: %w", err)
			}
		}
		if err := db.Exec(`ALTER TABLE notes ADD COLUMN IF NOT EXISTS search_tsv tsvector GENERATED ALWAYS AS (
			setweight(to_tsvector('simple', translate(coalesce(title, ''), '` + foldFrom + `', '` + foldTo + `')), 'A') ||
			setweight(to_tsvector('simple', translate(coalesce(summary, ''), '` + foldFrom + `', '` + foldTo + `')), 'B') ||
			setweight(to_tsvector('simple', translate(left(coalesce(body, ''), 250000), '` + foldFrom + `', '` + foldTo + `')), 'C')) STORED`).Error; err != nil {
			return fmt.Errorf("notes search column: %w", err)
		}
		return db.Exec(`CREATE INDEX IF NOT EXISTS idx_notes_search_tsv ON notes USING GIN (search_tsv)`).Error
	}
	// An index from before the summary column or the rowid map is rebuilt
	// (the backfill below refills it).
	var cols []string
	db.Raw(`SELECT name FROM pragma_table_info('notes_fts')`).Scan(&cols)
	var mapped int64
	db.Raw(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'notes_fts_map'`).Scan(&mapped)
	if len(cols) > 0 && (!containsString(cols, "summary") || mapped == 0) {
		if err := db.Exec(`DROP TABLE notes_fts`).Error; err != nil {
			return fmt.Errorf("notes fts upgrade: %w", err)
		}
	}
	if err := db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS notes_fts USING fts5(
		note_id UNINDEXED, title, summary, body, tokenize='unicode61 remove_diacritics 2')`).Error; err != nil {
		return fmt.Errorf("notes fts: %w", err)
	}
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS notes_fts_map (
		rid INTEGER PRIMARY KEY AUTOINCREMENT, note_id TEXT NOT NULL UNIQUE)`).Error; err != nil {
		return fmt.Errorf("notes fts map: %w", err)
	}
	// Backfill when the index drifted (first boot after the feature lands, or
	// a DB restored from elsewhere).
	var notes, indexed int64
	db.Table("notes").Where("deleted_at IS NULL").Count(&notes)
	db.Raw(`SELECT COUNT(*) FROM notes_fts`).Scan(&indexed)
	if notes != indexed {
		if err := db.Exec(`DELETE FROM notes_fts`).Error; err != nil {
			return err
		}
		if err := db.Exec(`DELETE FROM notes_fts_map`).Error; err != nil {
			return err
		}
		var rows []struct{ ID, Title, Summary, Body string }
		if err := db.Table("notes").Select("id, title, summary, body").Where("deleted_at IS NULL").Scan(&rows).Error; err != nil {
			return err
		}
		return db.Transaction(func(tx *gorm.DB) error {
			for _, r := range rows {
				Index(tx, r.ID, r.Title, r.Summary, r.Body)
			}
			return nil
		})
	}
	return nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Index (re)writes one note's entry. No-op on Postgres (generated column).
func Index(db *gorm.DB, id, title, summary, body string) {
	if isPostgres(db) {
		return
	}
	db.Exec(`INSERT OR IGNORE INTO notes_fts_map(note_id) VALUES (?)`, id)
	var rid int64
	if db.Raw(`SELECT rid FROM notes_fts_map WHERE note_id = ?`, id).Scan(&rid).Error != nil || rid == 0 {
		return
	}
	db.Exec(`DELETE FROM notes_fts WHERE rowid = ?`, rid)
	db.Exec(`INSERT INTO notes_fts(rowid, note_id, title, summary, body) VALUES (?, ?, ?, ?, ?)`, rid, id, Fold(title), Fold(summary), Fold(body))
}

// Remove drops entries (soft delete). No-op on Postgres.
func Remove(db *gorm.DB, ids []string) {
	if isPostgres(db) || len(ids) == 0 {
		return
	}
	var rids []int64
	db.Raw(`SELECT rid FROM notes_fts_map WHERE note_id IN ?`, ids).Scan(&rids)
	if len(rids) > 0 {
		db.Exec(`DELETE FROM notes_fts WHERE rowid IN ?`, rids)
	}
	db.Exec(`DELETE FROM notes_fts_map WHERE note_id IN ?`, ids)
}

// Terms turns a free-text query into at most 8 search terms: runs of letters
// and digits, lower-cased, at least 2 characters. A dot between two digits
// stays inside the term, so a version or address ("1.43.1") is one term —
// Postgres indexes it as one lexeme, and FTS5 reads the quoted term as the
// phrase "1 43 1". Everything else (quotes, operators like NEAR/OR/-, SQL
// punctuation) is dropped, so the terms are safe to splice into FTS5 MATCH /
// to_tsquery syntax.
func Terms(q string) []string {
	var out []string
	seen := map[string]bool{}
	var b strings.Builder
	flush := func() {
		t := strings.ToLower(Fold(b.String()))
		b.Reset()
		if utf8.RuneCountInString(t) < 2 || seen[t] || len(out) >= 8 {
			return
		}
		seen[t] = true
		out = append(out, t)
	}
	rs := []rune(q)
	for i, r := range rs {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '.' && i > 0 && i+1 < len(rs) && unicode.IsDigit(rs[i-1]) && unicode.IsDigit(rs[i+1]) && isNumeric(b.String()):
			b.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// isNumeric: s is digits and dots only (a number being read).
func isNumeric(s string) bool {
	for _, r := range s {
		if r != '.' && !unicode.IsDigit(r) {
			return false
		}
	}
	return s != ""
}

// stopWords carry no topic in a recall question ("how do I …", "… nasıl
// yapılır"). Recall drops them; the search box keeps every word.
var stopWords = func() map[string]bool {
	m := map[string]bool{}
	// Only words that are never a topic or an identifier on their own (no
	// "var", "all", "not", "only" — those name things in code).
	for _, w := range strings.Fields(`a an the and or but of to in on at by for from with into about as is are was were be been
		do does did have has had i me my we our you your it its this that these those
		what which who when where why how
		ve veya ile ama fakat ya da de ki bu bir icin gibi daha mi mu ne nasil neden nerede hangi`) {
		m[Fold(w)] = true
	}
	return m
}()

// RecallTerms are a question's search terms without stop words — all of
// them when the question is nothing but stop words (then they are the topic).
func RecallTerms(q string) []string {
	all := Terms(q)
	var out []string
	for _, t := range all {
		if !stopWords[t] {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return all
	}
	return out
}

// onlyStopWords: every term of q is a stop word (recall then matches all of
// them, as plain search does, and never widens).
func onlyStopWords(q string) bool {
	all := Terms(q)
	for _, t := range all {
		if !stopWords[t] {
			return false
		}
	}
	return len(all) > 0
}

// recallAndEnough: when the all-terms query finds this many notes, recall
// doesn't widen to any-term.
const recallAndEnough = 8

// RecallCandidates is search for agent recall, where questions are written
// in natural language: the notes matching ALL terms come first (as in
// Candidates), and when those are few, notes matching ANY term follow,
// ranked by how rare the matched terms are (title matches count double) —
// so one extra word in a question no longer empties the answer, while a
// question whose every word is common still finds nothing.
func RecallCandidates(db *gorm.DB, q string, limit int) ([]string, error) {
	return RecallCandidatesOf(db, q, limit, nil)
}

// RecallCandidatesOf is RecallCandidates over the notes of these owners
// only (tree owners, notes.user_id); nil means everyone's.
func RecallCandidatesOf(db *gorm.DB, q string, limit int, owners []string) ([]string, error) {
	if owners != nil && len(owners) == 0 {
		return nil, nil
	}
	terms := RecallTerms(q)
	if len(terms) == 0 {
		return titleCandidates(db, q, limit, owners) // no usable terms: title substring
	}
	and, err := candidates(db, terms, limit, owners)
	if err != nil || len(terms) < 2 || len(and) >= recallAndEnough || onlyStopWords(q) {
		return and, err
	}
	or, err := anyCandidates(db, terms, limit, owners)
	if err != nil {
		return and, nil // the precise part still stands
	}
	seen := make(map[string]bool, len(and))
	for _, id := range and {
		seen[id] = true
	}
	for _, id := range or {
		if len(and) >= limit {
			break
		}
		if !seen[id] {
			seen[id] = true
			and = append(and, id)
		}
	}
	return and, nil
}

// prefixable: a term worth matching as a prefix too ("url" → "urls") —
// words of 3+ letters, not numbers.
func prefixable(t string) bool {
	return utf8.RuneCountInString(t) >= 3 && !isNumeric(t)
}

// anyCandidates ranks notes matching any term. Postgres: score = Σ idf(term)
// over matched terms, a title match counting twice; ts_rank_cd breaks ties.
// SQLite: FTS5's bm25 already weighs rare terms up.
func anyCandidates(db *gorm.DB, terms []string, limit int, owners []string) ([]string, error) {
	var ids []string
	ownSQL, ownArgs := ownerClause(owners, "n.")
	if !isPostgres(db) {
		parts := make([]string, len(terms))
		for i, t := range terms {
			parts[i] = `"` + t + `"`
			if prefixable(t) {
				parts[i] += "*"
			}
		}
		args := append([]any{}, ownArgs...)
		args = append(args, strings.Join(parts, " OR "), limit)
		err := db.Raw(`SELECT f.note_id FROM notes_fts f
			JOIN notes n ON n.id = f.note_id AND n.deleted_at IS NULL`+ownSQL+`
			WHERE notes_fts MATCH ?
			ORDER BY bm25(notes_fts, 0.0, 10.0, 5.0, 1.0), f.note_id
			LIMIT ?`, args...).Scan(&ids).Error
		return ids, err
	}
	pgOwnSQL, pgOwnArgs := ownerClause(owners, "")
	qs := make([]string, len(terms))
	for i, t := range terms {
		qs[i] = t
		if prefixable(t) {
			qs[i] += ":*"
		}
	}
	// One round trip for every term's match flags, then corpus frequencies.
	cols := []string{"id", "ts_rank_cd(search_tsv, to_tsquery('simple', ?)) AS rank"}
	args := []any{strings.Join(qs, " | ")}
	for i, t := range qs {
		inTitle := t + ":A" // weight A = the title
		if strings.HasSuffix(t, ":*") {
			inTitle = t + "A"
		}
		cols = append(cols,
			fmt.Sprintf("(search_tsv @@ to_tsquery('simple', ?)) AS m%d", i),
			fmt.Sprintf("(search_tsv @@ to_tsquery('simple', ?)) AS t%d", i))
		args = append(args, t, inTitle)
	}
	args = append(args, strings.Join(qs, " | "))
	args = append(args, pgOwnArgs...)
	args = append(args, 200)
	rows, err := db.Raw(`SELECT `+strings.Join(cols, ", ")+` FROM notes
		WHERE deleted_at IS NULL AND search_tsv @@ to_tsquery('simple', ?)`+pgOwnSQL+`
		ORDER BY rank DESC, id ASC LIMIT ?`, args...).Rows()
	if err != nil {
		return nil, err
	}
	type cand struct {
		id    string
		rank  float64
		match []bool
		title []bool
	}
	var cands []cand
	for rows.Next() {
		c := cand{match: make([]bool, len(qs)), title: make([]bool, len(qs))}
		dest := []any{&c.id, &c.rank}
		for i := range qs {
			dest = append(dest, &c.match[i], &c.title[i])
		}
		if err := rows.Scan(dest...); err != nil {
			rows.Close()
			return nil, err
		}
		cands = append(cands, c)
	}
	rows.Close()
	idf, err := termIDF(db, qs)
	if err != nil {
		return nil, err
	}
	score := func(c cand) float64 {
		s := 0.0
		for i := range qs {
			if c.match[i] {
				s += idf[i]
			}
			if c.title[i] {
				s += idf[i]
			}
		}
		return s
	}
	sort.SliceStable(cands, func(i, j int) bool {
		si, sj := score(cands[i]), score(cands[j])
		if si != sj {
			return si > sj
		}
		if cands[i].rank != cands[j].rank {
			return cands[i].rank > cands[j].rank
		}
		return cands[i].id < cands[j].id
	})
	for _, c := range cands {
		if len(ids) >= limit {
			break
		}
		ids = append(ids, c.id)
	}
	return ids, nil
}

// termIDF is ln(1 + N/df) per tsquery term, df counted up to a cap so a
// term found in nearly every note costs a bounded scan.
func termIDF(db *gorm.DB, qs []string) ([]float64, error) {
	const dfCap = 5000
	var total float64
	db.Raw(`SELECT reltuples FROM pg_class WHERE relname = 'notes'`).Scan(&total)
	if total < 1000 { // estimate missing or too rough for a small table
		var n int64
		db.Table("notes").Where("deleted_at IS NULL").Count(&n)
		total = float64(n)
	}
	cols := make([]string, len(qs))
	args := make([]any, len(qs))
	for i, t := range qs {
		cols[i] = fmt.Sprintf(`(SELECT count(*) FROM (SELECT 1 FROM notes WHERE deleted_at IS NULL AND search_tsv @@ to_tsquery('simple', ?) LIMIT %d) s%d)`, dfCap, i)
		args[i] = t
	}
	row := db.Raw(`SELECT `+strings.Join(cols, ", "), args...).Row()
	dfs := make([]int64, len(qs))
	dest := make([]any, len(qs))
	for i := range dfs {
		dest[i] = &dfs[i]
	}
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	out := make([]float64, len(qs))
	for i, df := range dfs {
		if df < 1 {
			df = 1
		}
		out[i] = math.Log(1 + math.Max(total, float64(df))/float64(df))
	}
	return out, nil
}

// Candidates returns note ids matching q, best first (up to limit). All terms
// must match; the last one also matches as a prefix (search-as-you-type). When
// the query has no usable terms (one-letter / CJK-only input) it falls back to
// a title substring match.
func Candidates(db *gorm.DB, q string, limit int) ([]string, error) {
	terms := Terms(q)
	if len(terms) == 0 {
		return titleCandidates(db, q, limit, nil)
	}
	return candidates(db, terms, limit, nil)
}

// titleCandidates: a title substring match, for queries without usable terms.
func titleCandidates(db *gorm.DB, q string, limit int, owners []string) ([]string, error) {
	var ids []string
	like := strings.TrimSpace(q)
	if like == "" {
		return nil, nil
	}
	tx := db.Table("notes").Select("id").Where("deleted_at IS NULL AND LOWER(title) LIKE ?", "%"+strings.ToLower(like)+"%")
	if owners != nil {
		tx = tx.Where("user_id IN ?", owners)
	}
	err := tx.Order("updated_at DESC").Limit(limit).Scan(&ids).Error
	return ids, err
}

// ownerClause is " AND <prefix>user_id IN ?" for an owner filter ("" for none).
func ownerClause(owners []string, prefix string) (string, []any) {
	if owners == nil {
		return "", nil
	}
	return " AND " + prefix + "user_id IN ?", []any{owners}
}

// candidates: notes matching every term (the last one as a prefix too).
func candidates(db *gorm.DB, terms []string, limit int, owners []string) ([]string, error) {
	var ids []string
	if isPostgres(db) {
		ownSQL, ownArgs := ownerClause(owners, "")
		parts := make([]string, len(terms))
		for i, t := range terms {
			parts[i] = t
			if i == len(terms)-1 {
				parts[i] += ":*"
			}
		}
		tsq := strings.Join(parts, " & ")
		args := append([]any{tsq}, ownArgs...)
		args = append(args, tsq, limit)
		err := db.Raw(`SELECT id FROM notes
			WHERE deleted_at IS NULL AND search_tsv @@ to_tsquery('simple', ?)`+ownSQL+`
			ORDER BY ts_rank_cd(search_tsv, to_tsquery('simple', ?)) DESC, updated_at DESC, id ASC
			LIMIT ?`, args...).Scan(&ids).Error
		return ids, err
	}
	ownSQL, ownArgs := ownerClause(owners, "n.")
	parts := make([]string, len(terms))
	for i, t := range terms {
		parts[i] = `"` + t + `"`
		if i == len(terms)-1 {
			parts[i] += "*"
		}
	}
	// bm25 weights follow column order: note_id (unindexed), title, summary, body.
	args := append([]any{}, ownArgs...)
	args = append(args, strings.Join(parts, " "), limit)
	err := db.Raw(`SELECT f.note_id FROM notes_fts f
		JOIN notes n ON n.id = f.note_id AND n.deleted_at IS NULL`+ownSQL+`
		WHERE notes_fts MATCH ?
		ORDER BY bm25(notes_fts, 0.0, 10.0, 5.0, 1.0), f.note_id
		LIMIT ?`, args...).Scan(&ids).Error
	return ids, err
}

// Snippet returns a short plain-text excerpt of body around the first match
// of any term (or the start of the body), with markdown noise trimmed.
func Snippet(body string, terms []string, width int) string {
	text := plain(body)
	if text == "" {
		return ""
	}
	lower := strings.ToLower(Fold(text)) // same rune count as text
	at := -1
	for _, t := range terms {
		if i := strings.Index(lower, t); i >= 0 && (at < 0 || i < at) {
			at = i
		}
	}
	runes := []rune(text)
	start := 0
	if at > 0 {
		// Fold keeps rune counts, so a rune offset in `lower` is one in text.
		start = utf8.RuneCountInString(lower[:at]) - width/3
		if start < 0 {
			start = 0
		}
	}
	end := start + width
	if end > len(runes) {
		end = len(runes)
	}
	out := strings.TrimSpace(string(runes[start:end]))
	if start > 0 {
		out = "…" + out
	}
	if end < len(runes) {
		out += "…"
	}
	return out
}

// plain flattens markdown enough for a one-line excerpt: drops code fences,
// heading/list/quote markers and link targets, collapses whitespace.
func plain(md string) string {
	var b strings.Builder
	inFence := false
	for _, line := range strings.Split(md, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence || t == "" {
			continue
		}
		t = strings.TrimLeft(t, "#>*-+ \t")
		b.WriteString(t)
		b.WriteByte(' ')
	}
	s := b.String()
	// [text](target) → text
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == ']' && i+1 < len(s) && s[i+1] == '(' {
			if j := strings.IndexByte(s[i+1:], ')'); j >= 0 {
				i += j + 1
				continue
			}
		}
		if s[i] == '[' || (s[i] == '!' && i+1 < len(s) && s[i+1] == '[') || s[i] == '*' || s[i] == '`' {
			continue
		}
		out.WriteByte(s[i])
	}
	return strings.Join(strings.Fields(out.String()), " ")
}
