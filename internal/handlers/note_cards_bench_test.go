package handlers

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/notesearch"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Recall latency at scale: 100k cards, 500k links, SQLite on disk.
// PATCHMAN_BENCH_RECALL=1 go test ./internal/handlers/ -run TestRecallScale -v
func TestRecallScale(t *testing.T) {
	if os.Getenv("PATCHMAN_BENCH_RECALL") == "" {
		t.Skip("set PATCHMAN_BENCH_RECALL=1")
	}
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "bench.db")+"?_pragma=journal_mode(WAL)"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Note{}, &models.NoteLink{}); err != nil {
		t.Fatal(err)
	}
	if err := notesearch.Ensure(db); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	vocab := make([]string, 5000)
	for i := range vocab {
		vocab[i] = fmt.Sprintf("term%04d", i)
	}
	words := func(n int) string {
		w := make([]string, n)
		for i := range w {
			w[i] = vocab[rng.Intn(len(vocab))]
		}
		return strings.Join(w, " ")
	}
	const nNotes, nLinks = 100_000, 500_000
	ids := make([]string, nNotes)
	start := time.Now()
	db.Create(&models.Note{ID: "root", UserID: "alice", Type: "folder", Title: "Agent Memory", Version: 1})
	root := "root"
	err = db.Transaction(func(tx *gorm.DB) error {
		batch := make([]models.Note, 0, 1000)
		for i := 0; i < nNotes; i++ {
			ids[i] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
			n := models.Note{ID: ids[i], UserID: "alice", ParentID: &root, Type: "card", Title: words(4),
				Summary: words(12), Body: words(60), Source: "agent", Version: 1}
			batch = append(batch, n)
			notesearch.Index(tx, n.ID, n.Title, n.Summary, n.Body)
			if len(batch) == cap(batch) {
				if err := tx.Create(&batch).Error; err != nil {
					return err
				}
				batch = batch[:0]
			}
		}
		links := make([]models.NoteLink, 0, 2000)
		for i := 0; i < nLinks; i++ {
			links = append(links, models.NoteLink{ID: fmt.Sprintf("l%d", i), SrcNoteID: ids[rng.Intn(nNotes)], DstNoteID: ids[rng.Intn(nNotes)],
				Type: "related", Weight: 1, Source: "agent"})
			if len(links) == cap(links) {
				if err := tx.Clauses().CreateInBatches(&links, 500).Error; err != nil && !strings.Contains(err.Error(), "UNIQUE") {
					return err
				}
				links = links[:0]
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("seeded %d notes + %d links in %s", nNotes, nLinks, time.Since(start).Round(time.Second))

	h := &NoteHandler{db: db}
	var lat []time.Duration
	for i := 0; i < 300; i++ {
		q := vocab[rng.Intn(len(vocab))]
		switch i % 3 {
		case 1:
			q += " " + vocab[rng.Intn(len(vocab))]
		case 2: // a natural-language question: stop words + an any-term widening
			q = "how do I fix the " + q + " and " + vocab[rng.Intn(len(vocab))] + " problem in " + vocab[rng.Intn(len(vocab))]
		}
		t0 := time.Now()
		text, _ := h.recall("alice", q, 800, "", "", nil)
		lat = append(lat, time.Since(t0))
		if len(text) > 800*4 {
			t.Fatalf("over budget: %d", len(text))
		}
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	t.Logf("recall p50 %s p95 %s max %s", lat[len(lat)/2], lat[len(lat)*95/100], lat[len(lat)-1])
}
