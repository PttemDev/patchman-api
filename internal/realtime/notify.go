package realtime

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"gorm.io/gorm"
)

// Notify is the single entry point mutation handlers call after a
// successful write. It does two things:
//
//   - Cross-pod path (Postgres): emit `NOTIFY patchman_events, <json>`.
//     Every pod's listener (see listener.go) is LISTENing on that channel
//     and will fan the event out to its own local hub.
//   - Single-pod path (SQLite / embedded / non-postgres dialect): skip
//     the NOTIFY round-trip and call hub.Broadcast directly. SQLite has
//     no LISTEN/NOTIFY, and the embedded desktop sidecar is always a
//     single process anyway.
//
// `db` is the GORM connection — we pull the dialect off it to decide
// which path to take. Passing the db (rather than caching a flag) keeps
// the package free of hidden boot-time state.
//
// This is best-effort: an error emitting NOTIFY is logged and swallowed.
// The mutation already succeeded; the user got their write. Missing a
// realtime event degrades to "client needs to refresh manually" — which
// is exactly the polling fallback we'd ship anyway if WS were broken.
func Notify(db *gorm.DB, ev Event) {
	if hub == nil {
		// Init() wasn't called. In tests this is normal; in prod it
		// means a misconfiguration. Log once-per-call so the operator
		// can spot it without flooding.
		log.Printf("[realtime] Notify called before Init; dropping event type=%s", ev.Type)
		return
	}

	// Postgres path: emit pg_notify so all pods' listeners pick it up.
	if db != nil && db.Dialector != nil && db.Dialector.Name() == "postgres" {
		payload, err := json.Marshal(ev)
		if err != nil {
			log.Printf("[realtime] marshal failed type=%s: %v", ev.Type, err)
			return
		}
		// pg_notify(channel, payload) — preferred over `NOTIFY channel,
		// 'literal'` because it accepts a $1 placeholder, so we don't
		// have to escape JSON quotes by hand.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := db.WithContext(ctx).
			Exec("SELECT pg_notify(?, ?)", notifyChannel, string(payload)).Error; err != nil {
			log.Printf("[realtime] pg_notify failed type=%s: %v", ev.Type, err)
			// Fall back to local-only broadcast so this pod's clients
			// still see the event even when cross-pod fan-out fails.
			hub.Broadcast(ev)
		}
		return
	}

	// Non-Postgres path (SQLite embedded etc.) — broadcast locally only.
	// There is exactly one process so "locally only" == "everyone".
	hub.Broadcast(ev)
}
