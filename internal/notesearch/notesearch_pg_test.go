package notesearch

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"patchman-be/internal/models"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Runs against a real Postgres when PATCHMAN_TEST_PG_DSN is set, e.g.
// docker run -e POSTGRES_PASSWORD=x -p 15433:5432 postgres:16-alpine
// PATCHMAN_TEST_PG_DSN="host=127.0.0.1 port=15433 user=postgres password=x dbname=postgres sslmode=disable"
func TestPostgresSearch(t *testing.T) {
	dsn := os.Getenv("PATCHMAN_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set PATCHMAN_TEST_PG_DSN to run against Postgres")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	db.Exec("DROP TABLE IF EXISTS notes")
	if err := db.AutoMigrate(&models.Note{}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // idempotent on every boot
		if err := Ensure(db); err != nil {
			t.Fatalf("ensure #%d: %v", i+1, err)
		}
	}
	if err := db.AutoMigrate(&models.Note{}); err != nil { // AutoMigrate must leave the generated column alone
		t.Fatalf("re-migrate: %v", err)
	}
	db.Create(&[]models.Note{
		{ID: "a", UserID: "u", Title: "Kafka consumer lag", Body: "Check lag with the tool.", Version: 1},
		{ID: "b", UserID: "u", Title: "Random", Body: "Something about kafka.", Version: 1},
		{ID: "c", UserID: "u", Title: "Ödeme servisi", Body: "Çağrı akışı", Version: 1},
	})
	for q, want := range map[string][]string{
		"kafka": {"a", "b"}, "kaf": {"a", "b"}, "odeme": {"c"}, "cagri akisi": {"c"}, "ÇAĞRI": {"c"}, "nothing": nil,
	} {
		ids, err := Candidates(db, q, 10)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if len(ids) != len(want) || (len(want) > 0 && ids[0] != want[0]) {
			t.Errorf("%q → %v, want %v", q, ids, want)
		}
	}
	// A body near the 1 MB cap must still save (the indexed prefix is capped).
	// Unique words (to_tsvector's limit is about distinct lexemes): uncapped,
	// this body fails with "string is too long for tsvector".
	var sb strings.Builder
	for i := 0; sb.Len() < 1_000_000; i++ {
		sb.WriteString("w" + strconv.Itoa(i) + " ")
	}
	big := sb.String()
	if err := db.Create(&models.Note{ID: "big", UserID: "u", Title: "Big", Body: big, Version: 1}).Error; err != nil {
		t.Fatalf("big body insert: %v", err)
	}
	db.Delete(&models.Note{}, "id = ?", "a") // soft delete
	if ids, _ := Candidates(db, "kafka", 10); len(ids) != 1 || ids[0] != "b" {
		t.Errorf("after delete → %v", ids)
	}
}

// A search column from before card summaries is replaced on boot, and a
// summary hit ranks above a body-only hit.
func TestPostgresSummaryUpgrade(t *testing.T) {
	dsn := os.Getenv("PATCHMAN_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set PATCHMAN_TEST_PG_DSN to run against Postgres")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	db.Exec("DROP TABLE IF EXISTS notes")
	if err := db.AutoMigrate(&models.Note{}); err != nil {
		t.Fatal(err)
	}
	// The 1.45 column: title A, body B, no summary.
	if err := db.Exec(`ALTER TABLE notes ADD COLUMN search_tsv tsvector GENERATED ALWAYS AS (
		setweight(to_tsvector('simple', coalesce(title, '')), 'A') ||
		setweight(to_tsvector('simple', coalesce(body, '')), 'B')) STORED`).Error; err != nil {
		t.Fatal(err)
	}
	db.Exec(`CREATE INDEX idx_notes_search_tsv ON notes USING GIN (search_tsv)`)
	db.Create(&[]models.Note{
		{ID: "body", UserID: "u", Title: "Runbook", Body: "Mentions rotation once among many other words here.", Version: 1},
		{ID: "card", UserID: "u", Title: "Tokens", Summary: "Refresh token rotation is single-use.", Body: "Details.", Version: 1},
	})
	for i := 0; i < 2; i++ {
		if err := Ensure(db); err != nil {
			t.Fatalf("ensure #%d: %v", i+1, err)
		}
	}
	var expr string
	db.Raw(`SELECT coalesce(generation_expression, '') FROM information_schema.columns
		WHERE table_name = 'notes' AND column_name = 'search_tsv'`).Scan(&expr)
	if !strings.Contains(expr, "summary") {
		t.Fatalf("column not upgraded: %s", expr)
	}
	ids, err := Candidates(db, "rotation", 10)
	if err != nil || len(ids) != 2 || ids[0] != "card" {
		t.Errorf("rotation → %v, %v (want the card first)", ids, err)
	}
}

// Recall widens to any term ranked by rarity (title counting double), finds
// versions as one lexeme and ignores stop-word-only questions.
func TestPostgresRecallCandidates(t *testing.T) {
	dsn := os.Getenv("PATCHMAN_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set PATCHMAN_TEST_PG_DSN to run against Postgres")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	db.Exec("DROP TABLE IF EXISTS notes")
	if err := db.AutoMigrate(&models.Note{}); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(db); err != nil {
		t.Fatal(err)
	}
	notes := []models.Note{
		{ID: "monaco", UserID: "u", Title: "Web bundle: Monaco lazy-loaded", Body: "The entry chunk shrank once the editor loaded lazily.", Version: 1},
		{ID: "stale", UserID: "u", Title: "Stale-chunk reload (1.43.1)", Body: "Use lazyWithReload for the bundle.", Version: 1},
		{ID: "urls", UserID: "u", Title: "Deployment URLs", Body: "Prod FE and BE behind the ingress.", Version: 1},
		{ID: "gate", UserID: "u", Title: "macOS notarization", Body: "Gatekeeper blocks a DMG that is signed but not notarized.", Version: 1},
	}
	for i := 0; i < 30; i++ { // common words everywhere: they must not outrank rare ones
		notes = append(notes, models.Note{ID: "filler" + strconv.Itoa(i), UserID: "u", Title: "Filler " + strconv.Itoa(i), Body: "prod bundle notes", Version: 1})
	}
	db.Create(&notes)
	for q, want := range map[string]string{
		"Monaco bundle size":             "monaco",
		"1.43.1":                         "stale",
		"prod API URL":                   "urls",
		"DMG Gatekeeper uyarısı veriyor": "gate",
	} {
		ids, err := RecallCandidates(db, q, 20)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if len(ids) == 0 || ids[0] != want {
			t.Errorf("%q → %v, want %s first", q, ids, want)
		}
	}
	if ids, _ := RecallCandidates(db, "how do the", 20); len(ids) != 0 {
		t.Errorf("stop words only (none of them in one note) → %v", ids)
	}
}
