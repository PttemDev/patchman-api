// Package grafanaquery talks to Grafana's HTTP API on behalf of a saved
// GrafanaConnection: auth (token / basic with form-login fallback / rotated
// session), version-aware dashboard + datasource access, template variable
// interpolation and data queries. Dashboards are rendered natively by the FE
// from what this package returns — Grafana's UI is never proxied.
package grafanaquery

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

// SessionStore persists a rotated session cookie (session auth kind). The
// handler implements it (encrypt + UPDATE); the registry stays DB-free.
type SessionStore interface {
	SaveSession(connID, cookie string) error
}

// Registry pools one *http.Client per connection plus the per-connection
// auth state that must outlive a single request: the form-login session of a
// basic connection whose Grafana refuses HTTP Basic, the latest rotated
// cookie of a session connection, a failed-login backoff (so a wrong
// password can't trip Grafana's brute-force lockout) and the cached server
// info (version, datasources).
type Registry struct {
	cipher *services.TokenCipher
	store  SessionStore

	mu    sync.Mutex
	conns map[string]*connState
}

type connState struct {
	client   *http.Client
	lastUsed time.Time

	authMu sync.Mutex // serializes login / rotation for this connection
	// basic kind: "" = try HTTP Basic, "form" = use the form-login cookie.
	basicMode  string
	formCookie string
	// session kind: newest cookie (after rotation), overriding the DB copy
	// the request carried. Cleared by Close (edit / re-login).
	liveCookie string
	rotatedAt  time.Time
	// failed login backoff
	authFailedAt  time.Time
	authFailedErr error

	info   *ServerInfo
	infoAt time.Time
	// base is the Grafana root found by probing when the saved URL isn't
	// Grafana's root (Percona PMM serves Grafana under /graph). Own lock:
	// rootURL runs inside login / rotation, which hold authMu.
	baseMu      sync.Mutex
	base        string
	rootChecked bool
}

const (
	clientIdleTimeout = 10 * time.Minute
	infoTTL           = 5 * time.Minute
	authBackoff       = 60 * time.Second
	maxBody           = 64 << 20
	defaultCookieName = "grafana_session"
	// testIDPrefix marks the temporary state of an unsaved connection's Test.
	testIDPrefix = "test:"
)

func NewRegistry(cipher *services.TokenCipher, store SessionStore) *Registry {
	r := &Registry{cipher: cipher, store: store, conns: map[string]*connState{}}
	go r.reapLoop()
	return r
}

// SetStore wires the session persister after construction (the handler that
// implements it needs the registry first).
func (r *Registry) SetStore(s SessionStore) { r.store = s }

// Close drops all cached state for a connection (edit, delete, re-login).
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

// state returns (creating) the per-connection state. Unsaved connections
// (Test before create: empty ID) get a throwaway state.
func (r *Registry) state(conn models.GrafanaConnection) (*connState, error) {
	if conn.ID != "" {
		r.mu.Lock()
		if st, ok := r.conns[conn.ID]; ok {
			st.lastUsed = time.Now()
			r.mu.Unlock()
			return st, nil
		}
		r.mu.Unlock()
	}
	cl, err := buildClient(conn)
	if err != nil {
		return nil, err
	}
	st := &connState{client: cl, lastUsed: time.Now()}
	if conn.ID != "" {
		r.mu.Lock()
		if existing, ok := r.conns[conn.ID]; ok {
			st = existing
		} else {
			r.conns[conn.ID] = st
		}
		r.mu.Unlock()
	}
	return st, nil
}

func buildClient(conn models.GrafanaConnection) (*http.Client, error) {
	opts := parseOptions(conn.OptionsJSON)
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
		// Never follow redirects: an unauthenticated Grafana answers API calls
		// with 401, but a misconfigured URL (UI path, SSO proxy) 302s to a
		// login page, and following it would turn that into a confusing 200.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

func parseOptions(raw string) map[string]any {
	out := map[string]any{}
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return out
}

func optString(conn models.GrafanaConnection, key string) string {
	v, _ := parseOptions(conn.OptionsJSON)[key].(string)
	return strings.TrimSpace(v)
}

func cookieName(conn models.GrafanaConnection) string {
	if n := optString(conn, "cookieName"); n != "" {
		return n
	}
	return defaultCookieName
}

// rootURL is where API calls go: the probed root when one was found,
// otherwise the saved URL.
func (r *Registry) rootURL(conn models.GrafanaConnection, st *connState) (string, error) {
	st.baseMu.Lock()
	b := st.base
	st.baseMu.Unlock()
	if b != "" {
		return b, nil
	}
	return BaseURL(conn)
}

// BaseURL is the connection URL without a trailing slash.
func BaseURL(conn models.GrafanaConnection) (string, error) {
	u := strings.TrimRight(strings.TrimSpace(conn.URL), "/")
	if u == "" {
		return "", fmt.Errorf("Grafana URL is required")
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "https://" + u
	}
	if _, err := url.Parse(u); err != nil {
		return "", fmt.Errorf("invalid Grafana URL: %w", err)
	}
	return u, nil
}

func (r *Registry) decrypt(enc string) (string, error) {
	if enc == "" {
		return "", nil
	}
	if r.cipher == nil {
		return "", fmt.Errorf("no cipher configured")
	}
	return r.cipher.Decrypt(enc)
}

// ── errors ──────────────────────────────────────────────────────────

// APIError is a non-2xx answer from Grafana, with a message made for humans.
type APIError struct {
	Status    int
	Message   string
	MessageID string
	// Code is a machine-readable hint for the FE: auth | session_expired |
	// forbidden | not_found | upstream.
	Code string
	// body is the raw answer (e.g. /api/ds/query's per-ref errors on 400).
	body []byte
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

func grafanaMessage(body []byte) (msg, msgID string) {
	var m struct {
		Message   string `json:"message"`
		MessageID string `json:"messageId"`
		Error     string `json:"error"`
	}
	if json.Unmarshal(body, &m) == nil {
		msg = m.Message
		if msg == "" {
			msg = m.Error
		}
		return msg, m.MessageID
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s, ""
}

func newAPIError(conn models.GrafanaConnection, status int, body []byte) *APIError {
	msg, id := grafanaMessage(body)
	e := &APIError{Status: status, MessageID: id, Code: "upstream", body: body}
	switch status {
	case http.StatusUnauthorized:
		e.Code = "auth"
		switch conn.AuthKind {
		case "token":
			e.Message = "Grafana rejected the token (401). Check that the service-account token is valid and not expired."
		case "basic":
			e.Message = "Grafana rejected the username/password (401). If you sign in to Grafana with SSO (GitLab, Google, OAuth) you have no Grafana password — use a service-account token or the session option instead."
		case "session":
			e.Code = "session_expired"
			e.Message = "The Grafana session has expired. Sign in to Grafana again and update the session."
		case "none":
			e.Message = "This Grafana requires sign-in (401) — anonymous access isn't enabled. Use a service-account token, username/password or a session instead."
		default:
			e.Message = "Grafana rejected the credentials (401)."
		}
	case http.StatusForbidden:
		e.Code = "forbidden"
		e.Message = "Grafana denied access (403)"
		if msg != "" {
			e.Message += ": " + msg
		}
	case http.StatusNotFound:
		e.Code = "not_found"
		e.Message = "Not found in Grafana (404)"
		if msg != "" {
			e.Message += ": " + msg
		}
	case http.StatusFound, http.StatusMovedPermanently, http.StatusSeeOther, http.StatusTemporaryRedirect:
		e.Code = "auth"
		e.Message = "Grafana redirected the API call (probably to a login page). Check the URL — it must be Grafana's root URL (including any sub path) — and that no SSO proxy sits in front of the API."
	default:
		e.Message = fmt.Sprintf("Grafana returned %d", status)
		if msg != "" {
			e.Message += ": " + msg
		}
	}
	return e
}

// ── request core ────────────────────────────────────────────────────

// Response is a raw Grafana answer (2xx only — non-2xx become *APIError).
type Response struct {
	Status int
	Header http.Header
	Body   []byte
	// sentCookie is the session cookie this request carried — rotation and
	// re-login compare it with the current one so parallel 401s share a
	// single rotation instead of each rotating the token again.
	sentCookie string
}

// Do performs one authenticated API call. path is relative to the Grafana
// root ("/api/search"); query may be nil; body is JSON (nil for none). It
// owns every auth detail: Bearer/Basic headers, the form-login fallback, the
// session cookie + its rotation, the org header and the failed-login backoff.
func (r *Registry) Do(ctx context.Context, conn models.GrafanaConnection, method, path string, query url.Values, body []byte) (*Response, error) {
	st, err := r.state(conn)
	if err != nil {
		return nil, err
	}
	if err := st.backoffErr(); err != nil {
		return nil, err
	}
	resp, err := r.send(ctx, conn, st, method, path, query, body)
	if err != nil {
		return nil, err
	}
	if resp.Status == http.StatusUnauthorized {
		retry, aerr := r.recoverAuth(ctx, conn, st, resp)
		if aerr != nil {
			return nil, aerr
		}
		if retry {
			resp, err = r.send(ctx, conn, st, method, path, query, body)
			if err != nil {
				return nil, err
			}
		}
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return nil, newAPIError(conn, resp.Status, resp.Body)
	}
	return resp, nil
}

// DoJSON is Do + json.Unmarshal into out.
func (r *Registry) DoJSON(ctx context.Context, conn models.GrafanaConnection, method, path string, query url.Values, body any, out any) error {
	var raw []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		raw = b
	}
	resp, err := r.Do(ctx, conn, method, path, query, raw)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Body, out); err != nil {
		return fmt.Errorf("decode Grafana %s response: %w", path, err)
	}
	return nil
}

func (st *connState) backoffErr() error {
	st.authMu.Lock()
	defer st.authMu.Unlock()
	if st.authFailedErr != nil && time.Since(st.authFailedAt) < authBackoff {
		return st.authFailedErr
	}
	return nil
}

func (st *connState) failAuth(err error) error {
	st.authFailedAt = time.Now()
	st.authFailedErr = err
	return err
}

func (r *Registry) send(ctx context.Context, conn models.GrafanaConnection, st *connState, method, path string, query url.Values, body []byte) (*Response, error) {
	base, err := r.rootURL(conn, st)
	if err != nil {
		return nil, err
	}
	u := base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if org := optOrgID(conn); org != "" {
		req.Header.Set("X-Grafana-Org-Id", org)
	}
	if err := r.applyAuth(conn, st, req); err != nil {
		return nil, err
	}
	sent := ""
	if c, cerr := req.Cookie(cookieName(conn)); cerr == nil {
		sent = c.Value
	}
	res, err := st.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Grafana unreachable: %w", err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("read Grafana response: %w", err)
	}
	if len(b) > maxBody {
		return nil, fmt.Errorf("Grafana response larger than %d MB — narrow the time range or query", maxBody>>20)
	}
	r.captureCookie(conn, st, res)
	return &Response{Status: res.StatusCode, Header: res.Header, Body: b, sentCookie: sent}, nil
}

func optOrgID(conn models.GrafanaConnection) string {
	switch v := parseOptions(conn.OptionsJSON)["orgId"].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		if v > 0 {
			return fmt.Sprintf("%d", int64(v))
		}
	}
	return ""
}

func (r *Registry) applyAuth(conn models.GrafanaConnection, st *connState, req *http.Request) error {
	switch conn.AuthKind {
	case "none":
		// Anonymous access (auth.anonymous) / public dashboards: no credential.
		return nil
	case "token":
		tok, err := r.decrypt(conn.TokenEnc)
		if err != nil {
			return fmt.Errorf("decrypt token: %w", err)
		}
		if tok == "" {
			return &APIError{Status: 401, Code: "auth", Message: "No token saved for this connection."}
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	case "basic":
		st.authMu.Lock()
		mode, cookie := st.basicMode, st.formCookie
		st.authMu.Unlock()
		if mode == "form" && cookie != "" {
			req.AddCookie(&http.Cookie{Name: cookieName(conn), Value: cookie})
			return nil
		}
		pass, err := r.decrypt(conn.PasswordEnc)
		if err != nil {
			return fmt.Errorf("decrypt password: %w", err)
		}
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(conn.Username+":"+pass)))
	case "session":
		cookie, err := r.sessionCookie(conn, st)
		if err != nil {
			return err
		}
		if cookie == "" {
			return &APIError{Status: 401, Code: "session_expired", Message: "No Grafana session saved — sign in to Grafana and save the session."}
		}
		req.AddCookie(&http.Cookie{Name: cookieName(conn), Value: cookie})
	default:
		return fmt.Errorf("unsupported auth kind %q", conn.AuthKind)
	}
	return nil
}

// sessionCookie is the newest session cookie: a value rotated in this
// process wins over the DB copy the request loaded — unless the row was
// rotated later (another backend replica, or a fresh sign-in saved on the
// row), in which case the DB copy is adopted and the stale live value
// dropped, so replicas don't keep rotating each other's prev token.
func (r *Registry) sessionCookie(conn models.GrafanaConnection, st *connState) (string, error) {
	st.authMu.Lock()
	live := st.liveCookie
	if live != "" && conn.SessionRotatedAt != nil && conn.SessionRotatedAt.After(st.rotatedAt.Add(time.Second)) {
		st.liveCookie, live = "", ""
	}
	st.authMu.Unlock()
	if live != "" {
		return live, nil
	}
	v, err := r.decrypt(conn.SessionEnc)
	if err != nil {
		return "", fmt.Errorf("decrypt session: %w", err)
	}
	return normalizeCookieValue(v, cookieName(conn)), nil
}

// normalizeCookieValue accepts a bare value, "grafana_session=value" or a
// whole Cookie header pasted from DevTools, and returns just the value.
func normalizeCookieValue(v, name string) string {
	v = strings.TrimSpace(v)
	if !strings.Contains(v, "=") {
		return v
	}
	for _, part := range strings.Split(v, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 && strings.EqualFold(strings.TrimSpace(kv[0]), name) {
			return strings.TrimSpace(kv[1])
		}
	}
	return v
}

// captureCookie keeps a server-side rotated session (Grafana < 10 rotates in
// its auth middleware and answers with Set-Cookie on any request).
func (r *Registry) captureCookie(conn models.GrafanaConnection, st *connState, res *http.Response) {
	if conn.AuthKind != "session" && conn.AuthKind != "basic" {
		return
	}
	name := cookieName(conn)
	for _, c := range res.Cookies() {
		if c.Name != name || c.Value == "" || c.MaxAge < 0 {
			continue
		}
		st.authMu.Lock()
		if conn.AuthKind == "basic" {
			if st.basicMode == "form" {
				st.formCookie = c.Value
			}
			st.authMu.Unlock()
			continue
		}
		changed := st.liveCookie != c.Value
		st.liveCookie = c.Value
		st.rotatedAt = time.Now()
		st.authMu.Unlock()
		if changed {
			r.persist(conn, c.Value)
		}
	}
}

func (r *Registry) persist(conn models.GrafanaConnection, cookie string) {
	if r.store == nil || conn.ID == "" || strings.HasPrefix(conn.ID, testIDPrefix) {
		return
	}
	_ = r.store.SaveSession(conn.ID, cookie)
}

// recoverAuth reacts to a 401. Returns retry=true when the request should be
// sent again with fresh auth.
//   - session: rotate (Grafana 10+ answers 401 session.token.rotate when the
//     client must rotate); another goroutine may already have rotated — then
//     just retry with its cookie.
//   - basic: HTTP Basic refused → form login (POST /login) once; a form
//     session that expired → log in again.
func (r *Registry) recoverAuth(ctx context.Context, conn models.GrafanaConnection, st *connState, resp *Response) (bool, error) {
	switch conn.AuthKind {
	case "session":
		return r.rotateSession(ctx, conn, st, resp)
	case "basic":
		return r.formLogin(ctx, conn, st, resp)
	}
	return false, nil
}

func (r *Registry) rotateSession(ctx context.Context, conn models.GrafanaConnection, st *connState, resp *Response) (bool, error) {
	before := resp.sentCookie
	st.authMu.Lock()
	defer st.authMu.Unlock()
	// Someone rotated while we waited for the lock → just retry with theirs.
	if st.liveCookie != "" && st.liveCookie != before {
		return true, nil
	}
	newCookie, ok := r.callRotate(ctx, conn, st, before)
	if !ok {
		return false, newAPIError(conn, http.StatusUnauthorized, nil)
	}
	st.liveCookie = newCookie
	st.rotatedAt = time.Now()
	r.persist(conn, newCookie)
	return true, nil
}

// callRotate posts /api/user/auth-tokens/rotate with the current cookie and
// returns the new cookie value. Caller holds st.authMu.
func (r *Registry) callRotate(ctx context.Context, conn models.GrafanaConnection, st *connState, cookie string) (string, bool) {
	base, err := r.rootURL(conn, st)
	if err != nil || cookie == "" {
		return "", false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/user/auth-tokens/rotate", nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("Accept", "application/json")
	if org := optOrgID(conn); org != "" {
		req.Header.Set("X-Grafana-Org-Id", org)
	}
	req.AddCookie(&http.Cookie{Name: cookieName(conn), Value: cookie})
	res, err := st.client.Do(req)
	if err != nil {
		return "", false
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", false
	}
	for _, c := range res.Cookies() {
		if c.Name == cookieName(conn) && c.Value != "" && c.MaxAge >= 0 {
			return c.Value, true
		}
	}
	return "", false
}

func (r *Registry) formLogin(ctx context.Context, conn models.GrafanaConnection, st *connState, resp *Response) (bool, error) {
	st.authMu.Lock()
	defer st.authMu.Unlock()
	// Another request already logged in / rotated since this one was sent.
	if st.basicMode == "form" && st.formCookie != "" && st.formCookie != resp.sentCookie {
		return true, nil
	}
	// A form session first tries a rotation (Grafana 10+ client rotation),
	// then a fresh login.
	if st.basicMode == "form" && st.formCookie != "" {
		if c, ok := r.callRotate(ctx, conn, st, st.formCookie); ok {
			st.formCookie = c
			return true, nil
		}
	}
	pass, err := r.decrypt(conn.PasswordEnc)
	if err != nil {
		return false, fmt.Errorf("decrypt password: %w", err)
	}
	cookie, lerr := r.postLogin(ctx, conn, st, conn.Username, pass)
	if lerr != nil {
		return false, st.failAuth(lerr)
	}
	st.basicMode = "form"
	st.formCookie = cookie
	return true, nil
}

// postLogin signs in through Grafana's login form API and returns the
// session cookie value.
func (r *Registry) postLogin(ctx context.Context, conn models.GrafanaConnection, st *connState, user, pass string) (string, error) {
	base, err := r.rootURL(conn, st)
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]string{"user": user, "password": pass})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := st.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("Grafana unreachable: %w", err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		for _, c := range res.Cookies() {
			if c.Name == cookieName(conn) && c.Value != "" {
				return c.Value, nil
			}
		}
		return "", &APIError{Status: 401, Code: "auth", Message: "Grafana accepted the login but returned no session cookie — set the cookie name in the connection's advanced options if your Grafana renamed it."}
	}
	msg, _ := grafanaMessage(b)
	e := newAPIError(conn, http.StatusUnauthorized, nil)
	if msg != "" && res.StatusCode != http.StatusUnauthorized {
		e.Message += " (login: " + msg + ")"
	}
	return "", e
}

// SignIn is the explicit "log in with username/password and keep the
// session" call used to create a session connection without SSO. Returns the
// session cookie value.
func (r *Registry) SignIn(ctx context.Context, conn models.GrafanaConnection, user, pass string) (string, error) {
	st, err := r.state(models.GrafanaConnection{URL: conn.URL, OptionsJSON: conn.OptionsJSON, AuthKind: "basic"})
	if err != nil {
		return "", err
	}
	return r.postLogin(ctx, conn, st, user, pass)
}
