package errorreport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// PostStatus describes the outcome of a forward attempt so the caller
// can distinguish "not configured" (silent OK) from "deduped" (silent OK)
// from "Slack rejected it" (admin-visible error).
type PostStatus int

const (
	StatusPosted     PostStatus = iota // forwarded to Slack OK
	StatusDeduped                      // skipped — same signature within the dedupe window
	StatusNotConfigured                 // skipped — webhook URL is unset
	StatusFailed                        // Slack returned non-2xx or transport failed
)

// Reporter forwards Payloads to a Slack incoming webhook. Construct one
// per process; safe for concurrent Post calls.
type Reporter struct {
	webhookURL string
	client     *http.Client
	dd         *dedupe
}

// New returns a Reporter that posts to the given webhook URL. Pass an
// empty URL when the env var is unset — Post then short-circuits with
// StatusNotConfigured so the FE call still gets a 204.
func New(webhookURL string) *Reporter {
	return &Reporter{
		webhookURL: webhookURL,
		client:     &http.Client{Timeout: 5 * time.Second},
		// 5-minute window covers a refresh-loop bug spamming the same
		// crash. 256 cap keeps the map bounded under high traffic.
		dd: newDedupe(5*time.Minute, 256),
	}
}

// Configured reports whether a webhook URL is set. Handlers can use this
// to short-circuit the test-integration endpoint with a clear error
// instead of attempting a no-op POST.
func (r *Reporter) Configured() bool {
	return r != nil && r.webhookURL != ""
}

// Post forwards a single payload to Slack. Returns the dispositional
// status and a non-nil error only when StatusFailed is returned.
//
// Dedupe runs even when the webhook isn't configured, on purpose — that
// way enabling the integration mid-incident doesn't suddenly flush a
// minute's worth of buffered repeats.
func (r *Reporter) Post(ctx context.Context, p *Payload) (PostStatus, error) {
	if r == nil {
		return StatusNotConfigured, nil
	}
	if !r.dd.Allow(p.Signature(), time.Now()) {
		return StatusDeduped, nil
	}
	if r.webhookURL == "" {
		return StatusNotConfigured, nil
	}

	body, err := json.Marshal(buildSlackMessage(p))
	if err != nil {
		return StatusFailed, fmt.Errorf("marshal slack payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.webhookURL, bytes.NewReader(body))
	if err != nil {
		return StatusFailed, fmt.Errorf("build slack request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return StatusFailed, fmt.Errorf("post to slack: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return StatusFailed, fmt.Errorf("slack returned %d: %s", resp.StatusCode, string(respBody))
	}

	return StatusPosted, nil
}

// PostRaw bypasses dedupe — used by the admin "Test integration" button
// so the operator gets feedback even if a real crash with the same
// signature was just received.
func (r *Reporter) PostRaw(ctx context.Context, p *Payload) (PostStatus, error) {
	if r == nil || r.webhookURL == "" {
		return StatusNotConfigured, errors.New("Slack webhook URL is not configured on the server")
	}

	body, err := json.Marshal(buildSlackMessage(p))
	if err != nil {
		return StatusFailed, fmt.Errorf("marshal slack payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.webhookURL, bytes.NewReader(body))
	if err != nil {
		return StatusFailed, fmt.Errorf("build slack request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return StatusFailed, fmt.Errorf("post to slack: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return StatusFailed, fmt.Errorf("slack returned %d: %s", resp.StatusCode, string(respBody))
	}

	return StatusPosted, nil
}
