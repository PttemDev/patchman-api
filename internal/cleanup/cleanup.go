package cleanup

import (
	"log"
	"time"

	"gorm.io/gorm"
)

// high-growth tables and their date columns
var targets = []struct {
	Table  string
	Column string
}{
	{"audit_logs", "created_at"},
	{"activity_feeds", "created_at"},
	{"histories", "created_at"},
	{"test_runs", "created_at"},
	{"test_case_versions", "created_at"},
	{"j_unit_import_results", "created_at"},
	{"load_tests", "created_at"},
}

// Start launches a background goroutine that purges rows older than
// the given retention period. It runs once immediately on boot, then
// repeats every 24 hours. Safe to call once from main().
func Start(db *gorm.DB, retention time.Duration) {
	go func() {
		run(db, retention)

		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			run(db, retention)
		}
	}()
}

func run(db *gorm.DB, retention time.Duration) {
	cutoff := time.Now().Add(-retention)
	log.Printf("[cleanup] purging records older than %s (before %s)", retention, cutoff.Format(time.RFC3339))

	for _, t := range targets {
		res := db.Exec("DELETE FROM "+t.Table+" WHERE "+t.Column+" < ?", cutoff)
		if res.Error != nil {
			log.Printf("[cleanup] %s: error: %v", t.Table, res.Error)
		} else if res.RowsAffected > 0 {
			log.Printf("[cleanup] %s: deleted %d rows", t.Table, res.RowsAffected)
		}
	}

	log.Println("[cleanup] done")
}
