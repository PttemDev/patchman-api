package notesearch

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"patchman-be/internal/models"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// An index from before card summaries is rebuilt with a summary column and
// refilled, and a summary hit ranks above a body-only hit.
func TestSQLiteSummaryUpgradeAndWeight(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:summary_upgrade?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Note{}); err != nil {
		t.Fatal(err)
	}
	// The 1.45 index shape.
	db.Exec(`CREATE VIRTUAL TABLE notes_fts USING fts5(note_id UNINDEXED, title, body, tokenize='unicode61 remove_diacritics 2')`)
	db.Create(&[]models.Note{
		{ID: "body", UserID: "u", Title: "Runbook", Body: "Mentions rotation once among many other words here.", Version: 1},
		{ID: "card", UserID: "u", Title: "Tokens", Summary: "Refresh token rotation is single-use.", Body: "Details.", Version: 1},
	})
	for i := 0; i < 2; i++ {
		if err := Ensure(db); err != nil {
			t.Fatalf("ensure #%d: %v", i+1, err)
		}
	}
	var cols []string
	db.Raw(`SELECT name FROM pragma_table_info('notes_fts')`).Scan(&cols)
	if !containsString(cols, "summary") {
		t.Fatalf("fts columns = %v", cols)
	}
	ids, err := Candidates(db, "rotation", 10)
	if err != nil || len(ids) != 2 || ids[0] != "card" {
		t.Errorf("rotation → %v, %v (want the card first)", ids, err)
	}

	// Re-indexing replaces the note's one entry; Remove drops it and its rowid.
	Index(db, "card", "Tokens", "Refresh token rotation is single-use.", "Details, edited.")
	Index(db, "card", "Tokens", "Refresh token rotation is single-use.", "Details, edited again.")
	var entries, mapped int64
	db.Raw(`SELECT COUNT(*) FROM notes_fts`).Scan(&entries)
	db.Raw(`SELECT COUNT(*) FROM notes_fts_map`).Scan(&mapped)
	if entries != 2 || mapped != 2 {
		t.Errorf("after re-index: %d entries, %d mapped", entries, mapped)
	}
	Remove(db, []string{"card"})
	if ids, _ := Candidates(db, "rotation", 10); len(ids) != 1 || ids[0] != "body" {
		t.Errorf("after remove → %v", ids)
	}
	db.Raw(`SELECT COUNT(*) FROM notes_fts_map`).Scan(&mapped)
	if mapped != 1 {
		t.Errorf("map rows after remove = %d", mapped)
	}
}

// Saving a note costs the same with a large index: entries are found by
// rowid, not by scanning note_id.
func TestSQLiteIndexCostFlat(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	db, err := gorm.Open(sqlite.Open("file:index_cost?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	db.AutoMigrate(&models.Note{})
	if err := Ensure(db); err != nil {
		t.Fatal(err)
	}
	timeIndex := func() time.Duration {
		t0 := time.Now()
		for i := 0; i < 50; i++ {
			Index(db, fmt.Sprintf("n%d", i), "title", "summary", "body text")
		}
		return time.Since(t0)
	}
	small := timeIndex()
	db.Transaction(func(tx *gorm.DB) error {
		for i := 50; i < 40000; i++ {
			Index(tx, fmt.Sprintf("n%d", i), "title", "summary", "body text with a few more words")
		}
		return nil
	})
	large := timeIndex()
	if large > 4*small+20*time.Millisecond {
		t.Errorf("50 saves took %s with 50 notes but %s with 40k", small, large)
	}
}

func TestTermsKeepVersionsAndDropStopWords(t *testing.T) {
	if got := Terms("fix in 1.43.1, see v2 and 192.168.1.10."); !reflect.DeepEqual(got, []string{"fix", "in", "1.43.1", "see", "v2", "and", "192.168.1.10"}) {
		t.Errorf("Terms = %v", got)
	}
	if got := RecallTerms("how do I release the desktop build"); !reflect.DeepEqual(got, []string{"release", "desktop", "build"}) {
		t.Errorf("RecallTerms = %v", got)
	}
	if got := RecallTerms("var"); !reflect.DeepEqual(got, []string{"var"}) { // an identifier, not a stop word
		t.Errorf("RecallTerms(var) = %v", got)
	}
	if got := RecallTerms("how is"); !reflect.DeepEqual(got, []string{"how", "is"}) {
		t.Errorf("only stop words should stay the topic: %v", got)
	}
}
