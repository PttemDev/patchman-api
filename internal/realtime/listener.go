package realtime

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
)

// StartListener boots the Postgres LISTEN loop in a background goroutine.
// One goroutine per pod, holding a dedicated pgx connection (LISTEN ties
// up a connection for its lifetime, so we can't reuse GORM's pool).
//
// dsn is the same Postgres connection string the rest of the app uses.
// Pass an empty string (or call from a non-Postgres deployment) and the
// function logs + returns — single-pod paths broadcast locally via
// Notify directly and don't need a listener.
//
// The goroutine is intentionally never cancelled in the current design:
// the BE process owns it for its entire lifetime. If we add graceful
// shutdown later, expose a context arg here.
func StartListener(dsn string) {
	if hub == nil {
		log.Printf("[realtime] StartListener called before Init; refusing")
		return
	}
	if dsn == "" {
		log.Printf("[realtime] StartListener: empty DSN, skipping (single-pod mode)")
		return
	}
	go runListener(dsn)
}

func runListener(dsn string) {
	// Re-connect loop. pgx connections die on network blips; we want to
	// come back online quickly without crashing the BE pod. Exponential
	// backoff up to 30s caps log noise during an outage.
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		if err := listenOnce(dsn); err != nil {
			log.Printf("[realtime] listener error: %v; retry in %s", err, backoff)
			time.Sleep(backoff)
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		// listenOnce returned nil — that means a clean close, which
		// shouldn't happen in normal operation. Treat it as an outage
		// and reconnect.
		backoff = time.Second
	}
}

func listenOnce(dsn string) error {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	// LISTEN itself can't take a placeholder so we hard-code the
	// channel name (safe — it's a package constant, not user input).
	if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
		return err
	}
	log.Printf("[realtime] listening on Postgres channel %q", notifyChannel)
	// Reset backoff once we're listening successfully — caller sees nil
	// only if the loop exits cleanly, which won't happen here.

	for {
		notif, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		var ev Event
		if err := json.Unmarshal([]byte(notif.Payload), &ev); err != nil {
			log.Printf("[realtime] bad notification payload: %v (raw=%q)", err, notif.Payload)
			continue
		}
		hub.Broadcast(ev)
	}
}
