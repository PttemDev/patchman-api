// Package forwarder mirrors selected writes from an embedded BE to a remote
// BE. It's used when a desktop user runs executions locally but keeps their
// data-of-record on the remote server (Phase 4.6 of the offline plan).
//
// Writes are fired asynchronously with a short timeout. Failures are logged
// but never propagated — a flaky network shouldn't break a local send/run.
// This is best-effort mirroring, not a durable queue; building a persistent
// outbox is deferred until we actually see drops in practice.
package forwarder

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"patchman-be/internal/models"
)

// Client mirrors writes to an upstream BE. Zero value is an inert "noop"
// client — call New with a non-empty target/token to activate.
type Client struct {
	base    string
	token   string
	http    *http.Client
	enabled bool
}

// New constructs a Client. If either target or token is empty, the returned
// Client is disabled — all methods no-op, so callers can unconditionally
// invoke it without a nil check.
func New(target, token string) *Client {
	if target == "" || token == "" {
		return &Client{}
	}
	return &Client{
		base:    strings.TrimRight(target, "/"),
		token:   token,
		http:    &http.Client{Timeout: 10 * time.Second},
		enabled: true,
	}
}

// Enabled reports whether this client will actually forward writes. Useful
// for handlers that want to short-circuit payload construction.
func (c *Client) Enabled() bool {
	return c != nil && c.enabled
}

// ForwardHistory fires a POST /api/sync/forward/history in a background
// goroutine. Returns immediately; errors surface as log lines only.
func (c *Client) ForwardHistory(h models.History) {
	if !c.Enabled() {
		return
	}
	go c.postJSON("/api/sync/forward/history", h, "history")
}

// ForwardTestRun fires a POST /api/sync/forward/test-runs. Safe to call
// multiple times for the same run (e.g. initial create + post-completion
// update) — the remote endpoint upserts by primary key.
func (c *Client) ForwardTestRun(r models.TestRun) {
	// Folder runs carry no test plan, and the upstream endpoint is
	// plan-keyed (it rejects them) — keep them local instead of logging a
	// guaranteed failure.
	if !c.Enabled() || r.TestPlanID == "" {
		return
	}
	go c.postJSON("/api/sync/forward/test-runs", r, "test-run")
}

func (c *Client) postJSON(path string, body any, label string) {
	payload, err := json.Marshal(body)
	if err != nil {
		log.Printf("[forwarder] %s: marshal failed: %v", label, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(payload))
	if err != nil {
		log.Printf("[forwarder] %s: build request failed: %v", label, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		log.Printf("[forwarder] %s: POST %s failed: %v", label, path, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	log.Printf("[forwarder] %s: %s returned %d: %s", label, path, resp.StatusCode, trim(string(snippet)))
}

func trim(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

// Compile-time check that Client satisfies the forwarding surface. Tests
// can substitute a no-op satisfying the same interface.
var _ interface {
	ForwardHistory(models.History)
	ForwardTestRun(models.TestRun)
} = (*Client)(nil)
