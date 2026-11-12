package errorreport

import (
	"sync"
	"time"
)

// dedupe drops repeated reports of the same signature within a window.
// The map is bounded by trimming entries on every Add — at low traffic
// this stays tiny, and at high traffic the trim amortises out.
type dedupe struct {
	mu     sync.Mutex
	seen   map[string]time.Time
	window time.Duration
	cap    int
}

func newDedupe(window time.Duration, cap int) *dedupe {
	return &dedupe{
		seen:   make(map[string]time.Time, cap),
		window: window,
		cap:    cap,
	}
}

// Allow returns true if the signature has not been seen in the dedupe
// window. If true, the signature is recorded; subsequent calls with the
// same signature return false until the window passes.
func (d *dedupe) Allow(signature string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if last, ok := d.seen[signature]; ok && now.Sub(last) < d.window {
		return false
	}

	d.seen[signature] = now

	if len(d.seen) > d.cap {
		cutoff := now.Add(-d.window)
		for k, t := range d.seen {
			if t.Before(cutoff) {
				delete(d.seen, k)
			}
		}
	}

	return true
}
