// Package apm reads the live service map from observability backends
// (Coroot, Prometheus, Jaeger, Kiali, SigNoz, Datadog) on behalf of a saved
// ObservabilityConnection. Each backend is a Provider; this file holds the
// shared HTTP client, credentials and error mapping.
package apm

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
	clientIdleTimeout = 10 * time.Minute
	authBackoff       = 60 * time.Second
	maxBody           = 64 << 20
)

// Secrets is the decrypted SecretsEnc object.
type Secrets struct {
	Password string `json:"password,omitempty"`
	Token    string `json:"token,omitempty"`
	APIKey   string `json:"apiKey,omitempty"`
	AppKey   string `json:"appKey,omitempty"`
	Session  string `json:"session,omitempty"`
}

// Has lists which secrets are set (for the DTO; never the values).
func (s Secrets) Has() []string {
	var out []string
	for _, p := range []struct {
		n, v string
	}{{"password", s.Password}, {"token", s.Token}, {"apiKey", s.APIKey}, {"appKey", s.AppKey}, {"session", s.Session}} {
		if p.v != "" {
			out = append(out, p.n)
		}
	}
	return out
}

// Registry pools one *http.Client per connection plus the sign-in session
// of a "login" connection (Coroot) and a failed-login backoff.
type Registry struct {
	cipher *services.TokenCipher

	mu    sync.Mutex
	conns map[string]*connState
}

type connState struct {
	client   *http.Client
	lastUsed time.Time

	authMu        sync.Mutex
	session       string
	authFailedAt  time.Time
	authFailedErr error
}

func NewRegistry(cipher *services.TokenCipher) *Registry {
	r := &Registry{cipher: cipher, conns: map[string]*connState{}}
	go r.reapLoop()
	return r
}

// Close drops all cached state for a connection (edit, delete).
func (r *Registry) Close(connID string) {
	r.mu.Lock()
	delete(r.conns, connID)
	r.mu.Unlock()
}

func (r *Registry) reapLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-clientIdleTimeout)
		r.mu.Lock()
		for id, st := range r.conns {
			if st.lastUsed.Before(cutoff) {
				delete(r.conns, id)
			}
		}
		r.mu.Unlock()
	}
}

// EncryptSecrets / DecryptSecrets convert between Secrets and SecretsEnc.
func EncryptSecrets(c *services.TokenCipher, s Secrets) (string, error) {
	if s == (Secrets{}) {
		return "", nil
	}
	b, _ := json.Marshal(s)
	return c.Encrypt(string(b))
}

func DecryptSecrets(c *services.TokenCipher, enc string) (Secrets, error) {
	var s Secrets
	if enc == "" {
		return s, nil
	}
	p, err := c.Decrypt(enc)
	if err != nil {
		return s, err
	}
	err = json.Unmarshal([]byte(p), &s)
	return s, err
}

func ParseOptions(raw string) map[string]any {
	out := map[string]any{}
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return out
}

func optString(conn models.ObservabilityConnection, key string) string {
	v, _ := ParseOptions(conn.OptionsJSON)[key].(string)
	return strings.TrimSpace(v)
}

func buildClient(conn models.ObservabilityConnection) (*http.Client, error) {
	opts := ParseOptions(conn.OptionsJSON)
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if b, ok := opts["insecureSkipVerify"].(bool); ok && b {
		tlsCfg.InsecureSkipVerify = true
	}
	if ca, ok := opts["caCert"].(string); ok && strings.TrimSpace(ca) != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(ca)) {
			return nil, fmt.Errorf("caCert is not valid PEM")
		}
		tlsCfg.RootCAs = pool
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			TLSClientConfig:     tlsCfg,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		// A wrong URL (UI path behind an SSO proxy) redirects to a login
		// page; following it would turn that into a confusing 200.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// BaseURL validates and normalizes a connection URL (no trailing slash).
func BaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("url must be an http(s) address, e.g. https://coroot.example.com")
	}
	u.RawQuery, u.Fragment = "", ""
	return strings.TrimRight(u.String(), "/"), nil
}

// APIError is a backend answer PatchMan turns into a readable message.
// Code: auth | forbidden | not_found | upstream | unsupported.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string { return e.Message }

func AsAPIError(err error) (*APIError, bool) {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// Client is a connection ready to call its backend.
type Client struct {
	Conn     models.ObservabilityConnection
	Provider Provider
	Base     string
	Secrets  Secrets

	reg *Registry
	st  *connState
}

// Client resolves the provider, decrypts the secrets and pools the HTTP
// client. Unsaved connections (Test before create) get a throwaway state.
func (r *Registry) Client(conn models.ObservabilityConnection) (*Client, error) {
	p := Get(conn.Provider)
	if p == nil {
		return nil, fmt.Errorf("unknown provider %q", conn.Provider)
	}
	base, err := BaseURL(conn.URL)
	if err != nil {
		return nil, err
	}
	sec, err := DecryptSecrets(r.cipher, conn.SecretsEnc)
	if err != nil {
		return nil, fmt.Errorf("decrypt credentials")
	}
	var st *connState
	if conn.ID != "" {
		r.mu.Lock()
		st = r.conns[conn.ID]
		r.mu.Unlock()
	}
	if st == nil {
		cl, err := buildClient(conn)
		if err != nil {
			return nil, err
		}
		st = &connState{client: cl}
		if conn.ID != "" {
			r.mu.Lock()
			if existing, ok := r.conns[conn.ID]; ok {
				st = existing
			} else {
				r.conns[conn.ID] = st
			}
			r.mu.Unlock()
		}
	}
	r.mu.Lock()
	st.lastUsed = time.Now()
	r.mu.Unlock()
	return &Client{Conn: conn, Provider: p, Base: base, Secrets: sec, reg: r, st: st}, nil
}

// HTTP exposes the pooled client (providers' Login).
func (c *Client) HTTP() *http.Client { return c.st.client }

// authorize adds the connection's credentials to req.
func (c *Client) authorize(ctx context.Context, req *http.Request, fresh bool) error {
	switch c.Conn.AuthKind {
	case "", "none":
	case "basic":
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.Conn.Username+":"+c.Secrets.Password)))
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+c.Secrets.Token)
	case "apikey":
		name := optString(c.Conn, "headerName")
		if name == "" {
			name = c.Provider.APIKeyHeader()
		}
		v := c.Secrets.APIKey
		if strings.EqualFold(name, "Authorization") && !strings.Contains(v, " ") {
			v = "Bearer " + v
		}
		req.Header.Set(name, v)
	case "session":
		req.AddCookie(&http.Cookie{Name: c.Provider.SessionCookie(), Value: c.Secrets.Session})
	case "datadog":
		req.Header.Set("DD-API-KEY", c.Secrets.APIKey)
		req.Header.Set("DD-APPLICATION-KEY", c.Secrets.AppKey)
	case "login":
		c.st.authMu.Lock()
		defer c.st.authMu.Unlock()
		if fresh || c.st.session == "" {
			if !c.st.authFailedAt.IsZero() && time.Since(c.st.authFailedAt) < authBackoff && !fresh {
				return c.st.authFailedErr
			}
			s, err := c.Provider.Login(ctx, c)
			if err != nil {
				c.st.authFailedAt, c.st.authFailedErr = time.Now(), err
				return err
			}
			c.st.session, c.st.authFailedAt, c.st.authFailedErr = s, time.Time{}, nil
		}
		req.AddCookie(&http.Cookie{Name: c.Provider.SessionCookie(), Value: c.st.session})
	default:
		return fmt.Errorf("unsupported auth kind %q", c.Conn.AuthKind)
	}
	return nil
}

// Do sends an authenticated request (path relative to the base URL) and
// decodes a JSON answer into out (nil = discard). A "login" connection
// whose session expired signs in again once.
func (c *Client) Do(ctx context.Context, method, path string, q url.Values, body any, out any) error {
	b, err := c.DoRaw(ctx, method, path, q, body)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return &APIError{http.StatusBadGateway, "upstream", fmt.Sprintf("%s's answer was not JSON — check the URL", c.Provider.Title())}
	}
	return nil
}

// DoRaw is Do without decoding.
func (c *Client) DoRaw(ctx context.Context, method, path string, q url.Values, body any) ([]byte, error) {
	return c.DoRawWith(ctx, method, path, q, body, nil)
}

// DoRawWith is DoRaw with extra request headers (Coroot's project key on
// its Prometheus endpoints, next to the connection's own sign-in).
func (c *Client) DoRawWith(ctx context.Context, method, path string, q url.Values, body any, headers map[string]string) ([]byte, error) {
	u := c.Base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	for attempt := 0; attempt < 2; attempt++ {
		var rd io.Reader
		if payload != nil {
			rd = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json, */*")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if err := c.authorize(ctx, req, attempt > 0); err != nil {
			return nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := c.st.client.Do(req)
		if err != nil {
			return nil, err
		}
		b, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		resp.Body.Close()
		if rerr != nil {
			return nil, rerr
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 && c.Conn.AuthKind == "login" {
			continue // the session expired: sign in again
		}
		if resp.StatusCode >= 300 {
			return nil, c.httpErr(resp.StatusCode, b)
		}
		return b, nil
	}
	return nil, c.httpErr(http.StatusUnauthorized, nil)
}

func (c *Client) httpErr(status int, body []byte) *APIError {
	title := c.Provider.Title()
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	switch status {
	case http.StatusUnauthorized:
		switch c.Conn.AuthKind {
		case "session":
			return &APIError{status, "auth", title + " rejected the session cookie — it has expired or was signed out. Paste a fresh one."}
		case "login", "basic":
			return &APIError{status, "auth", title + " rejected the user name or password."}
		case "none", "":
			return &APIError{status, "auth", title + " needs credentials."}
		}
		return &APIError{status, "auth", title + " rejected the API key / token."}
	case http.StatusForbidden:
		return &APIError{status, "forbidden", "This " + title + " user is not allowed to view that."}
	case http.StatusNotFound:
		return &APIError{status, "not_found", "Not found on " + title + suffix(msg)}
	case http.StatusFound, http.StatusMovedPermanently, http.StatusSeeOther, http.StatusTemporaryRedirect:
		return &APIError{status, "upstream", title + " answered with a redirect — check the URL (it should be the API address, not a login proxy)."}
	}
	return &APIError{status, "upstream", fmt.Sprintf("%s answered %d%s", title, status, suffix(msg))}
}

func suffix(msg string) string {
	if msg == "" {
		return ""
	}
	return ": " + msg
}
