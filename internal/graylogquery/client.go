// Package graylogquery talks to a Graylog server on behalf of a saved
// GraylogConnection through its REST API (<url>/api): identity and version,
// streams, log search with a message-count histogram, and the dashboards /
// saved searches list. PatchMan only reads.
//
// Search goes through the Views API (`POST /api/views/search/sync`) — what
// Graylog's own UI uses, present from 3.x through 6.x — because it returns
// complete messages (every field) and the histogram in one round trip; the
// newer Scripting API returns chosen columns only.
package graylogquery

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/services"
)

const (
	AuthToken = "token"
	AuthBasic = "basic"

	maxBody     = 32 << 20
	idleTimeout = 10 * time.Minute
)

// Registry pools one *http.Client per connection, rebuilt when the row
// changes (UpdatedAt) and dropped after a while unused.
type Registry struct {
	cipher *services.TokenCipher

	mu      sync.Mutex
	clients map[string]*clientState
}

type clientState struct {
	client   *http.Client
	stamp    time.Time
	lastUsed time.Time
}

func NewRegistry(cipher *services.TokenCipher) *Registry {
	r := &Registry{cipher: cipher, clients: map[string]*clientState{}}
	go r.reapLoop()
	return r
}

// Close drops a connection's cached client (edit, delete).
func (r *Registry) Close(connID string) {
	r.mu.Lock()
	delete(r.clients, connID)
	r.mu.Unlock()
}

func (r *Registry) reapLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-idleTimeout)
		r.mu.Lock()
		for id, st := range r.clients {
			if st.lastUsed.Before(cutoff) {
				delete(r.clients, id)
			}
		}
		r.mu.Unlock()
	}
}

func (r *Registry) httpClient(conn models.GraylogConnection) (*http.Client, error) {
	if conn.ID != "" {
		r.mu.Lock()
		if st, ok := r.clients[conn.ID]; ok && st.stamp.Equal(conn.UpdatedAt) {
			st.lastUsed = time.Now()
			r.mu.Unlock()
			return st.client, nil
		}
		r.mu.Unlock()
	}
	cfg, err := tlsConfig(conn)
	if err != nil {
		return nil, err
	}
	cl := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			TLSClientConfig:     cfg,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		// A URL behind an SSO proxy that redirects to a login page must not
		// turn into a misleading 200.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if conn.ID != "" {
		r.mu.Lock()
		r.clients[conn.ID] = &clientState{client: cl, stamp: conn.UpdatedAt, lastUsed: time.Now()}
		r.mu.Unlock()
	}
	return cl, nil
}

// Options parses the connection's OptionsJSON.
func Options(conn models.GraylogConnection) map[string]any {
	out := map[string]any{}
	if strings.TrimSpace(conn.OptionsJSON) != "" {
		_ = json.Unmarshal([]byte(conn.OptionsJSON), &out)
	}
	return out
}

func tlsConfig(conn models.GraylogConnection) (*tls.Config, error) {
	opts := Options(conn)
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if b, _ := opts["insecureSkipVerify"].(bool); b {
		cfg.InsecureSkipVerify = true
	}
	if ca, _ := opts["caCert"].(string); strings.TrimSpace(ca) != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(ca)) {
			return nil, fmt.Errorf("caCert is not valid PEM")
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// BaseURL normalizes the Graylog web URL: scheme defaults to http (Graylog's
// default listener is plain :9000), and a trailing slash or /api (people
// paste the API browser URL) is dropped, as is a pasted UI page path.
func BaseURL(raw string) (string, error) {
	u := strings.TrimSpace(raw)
	if u == "" {
		return "", fmt.Errorf("Graylog URL is required")
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "http://" + u
	}
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return "", fmt.Errorf("invalid Graylog URL")
	}
	// Cut at the first Graylog API / UI segment, wherever it sits (Graylog
	// can live under a sub-path, e.g. https://example.com/graylog).
	path := strings.TrimRight(p.Path, "/")
	cutAt := len(path)
	for _, seg := range []string{"/api", "/api-browser", "/search", "/dashboards", "/streams", "/system", "/alerts", "/welcome"} {
		if i := strings.Index(path+"/", seg+"/"); i >= 0 && i < cutAt {
			cutAt = i
		}
	}
	p.Path = path[:cutAt]
	p.RawQuery, p.Fragment, p.RawPath = "", "", ""
	return strings.TrimRight(p.String(), "/"), nil
}

// ── errors ──────────────────────────────────────────────────────────

// APIError is a Graylog failure with a readable message and a machine code:
// auth | forbidden | not_found | bad_query | bad_request | unreachable |
// upstream.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string { return e.Message }

// AsAPIError unwraps an *APIError.
func AsAPIError(err error) (*APIError, bool) {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

func newAPIError(status int, body []byte) *APIError {
	var b struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	}
	_ = json.Unmarshal(body, &b)
	reason := strings.TrimSpace(b.Message)
	e := &APIError{Status: status, Code: "upstream"}
	switch {
	case status == http.StatusUnauthorized:
		e.Code = "auth"
		e.Message = "Graylog rejected the credentials (401): check the access token, or the username and password."
	case status == http.StatusForbidden:
		e.Code = "forbidden"
		e.Message = "This Graylog user may not do that (403)" + suffix(reason)
	case status == http.StatusNotFound:
		e.Code = "not_found"
		e.Message = "Not found on Graylog" + suffix(reason)
	case status == http.StatusBadRequest:
		e.Code = "bad_request"
		e.Message = "Graylog refused the request" + suffix(reason)
	case status >= 300 && status < 400:
		e.Code = "auth"
		e.Message = "Graylog answered with a redirect — the URL probably points at a login page or proxy, not the Graylog server."
	default:
		e.Message = fmt.Sprintf("Graylog answered %d", status) + suffix(reason)
	}
	return e
}

func suffix(reason string) string {
	if reason == "" {
		return "."
	}
	if len(reason) > 300 {
		reason = reason[:300] + "…"
	}
	return ": " + reason
}

// ── transport ───────────────────────────────────────────────────────

func (r *Registry) authHeader(conn models.GraylogConnection) (string, error) {
	secret := ""
	if conn.SecretEnc != "" {
		s, err := r.cipher.Decrypt(conn.SecretEnc)
		if err != nil {
			return "", fmt.Errorf("decrypt secret: %w", err)
		}
		secret = s
	}
	user, pass := conn.Username, secret
	if conn.AuthKind != AuthBasic {
		// An access token is sent as the username with the literal password
		// "token".
		user, pass = secret, "token"
	}
	if user == "" {
		return "", &APIError{Code: "auth", Message: "The connection has no access token / username."}
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass)), nil
}

func (r *Registry) do(ctx context.Context, conn models.GraylogConnection, method, path string, body, out any) error {
	base, err := BaseURL(conn.URL)
	if err != nil {
		return &APIError{Code: "bad_request", Message: err.Error()}
	}
	cl, err := r.httpClient(conn)
	if err != nil {
		return err
	}
	auth, err := r.authHeader(conn)
	if err != nil {
		return err
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+"/api"+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/json")
	// Graylog refuses non-GET API calls without X-Requested-By (CSRF guard).
	req.Header.Set("X-Requested-By", "PatchMan")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := cl.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return &APIError{Code: "unreachable", Message: "Graylog did not answer in time."}
		}
		return &APIError{Code: "unreachable", Message: "Graylog is not reachable: " + err.Error()}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return &APIError{Code: "unreachable", Message: "reading Graylog's answer: " + err.Error()}
	}
	if resp.StatusCode/100 != 2 {
		return newAPIError(resp.StatusCode, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("<")) {
				return &APIError{Code: "upstream", Message: "Graylog answered with an HTML page — check that the URL is the Graylog server (the API lives under /api)."}
			}
			return fmt.Errorf("decode Graylog response: %w", err)
		}
	}
	return nil
}
