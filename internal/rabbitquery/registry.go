// Package rabbitquery talks to RabbitMQ on behalf of a saved
// RabbitConnection: the management HTTP API for reads, stats, peek
// (get-with-requeue), publish and admin actions, and AMQP 0-9-1 only where
// HTTP can't help — reading stream queues by offset, tailing an exchange
// live and moving messages between queues.
package rabbitquery

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/services"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Registry pools one *http.Client per connection and, lazily, one AMQP
// connection per (connection, vhost). State is rebuilt when the row changes
// (UpdatedAt) and reaped after a while unused.
type Registry struct {
	cipher *services.TokenCipher

	mu    sync.Mutex
	conns map[string]*connState
}

type connState struct {
	client   *http.Client
	stamp    time.Time
	lastUsed time.Time

	amqpMu sync.Mutex
	amqp   map[string]*amqp.Connection // by vhost

	infoMu  sync.Mutex
	version string
	infoAt  time.Time
}

const (
	idleTimeout = 10 * time.Minute
	versionTTL  = 10 * time.Minute
	maxBody     = 64 << 20
	httpTimeout = 30 * time.Second
	// testIDPrefix marks the throwaway state of an unsaved connection's Test.
	testIDPrefix = "test:"
)

func NewRegistry(cipher *services.TokenCipher) *Registry {
	r := &Registry{cipher: cipher, conns: map[string]*connState{}}
	go r.reapLoop()
	return r
}

// Close drops a connection's cached state (edit, delete).
func (r *Registry) Close(connID string) {
	r.mu.Lock()
	st := r.conns[connID]
	delete(r.conns, connID)
	r.mu.Unlock()
	if st != nil {
		st.closeAMQP()
	}
}

func (st *connState) closeAMQP() {
	st.amqpMu.Lock()
	defer st.amqpMu.Unlock()
	for vh, c := range st.amqp {
		_ = c.Close()
		delete(st.amqp, vh)
	}
}

func (r *Registry) reapLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-idleTimeout)
		var stale []*connState
		r.mu.Lock()
		for id, st := range r.conns {
			if st.lastUsed.Before(cutoff) {
				stale = append(stale, st)
				delete(r.conns, id)
			}
		}
		r.mu.Unlock()
		for _, st := range stale {
			st.closeAMQP()
		}
	}
}

func (r *Registry) state(conn models.RabbitConnection) (*connState, error) {
	if conn.ID != "" {
		r.mu.Lock()
		if st, ok := r.conns[conn.ID]; ok {
			if st.stamp.Equal(conn.UpdatedAt) {
				st.lastUsed = time.Now()
				r.mu.Unlock()
				return st, nil
			}
			delete(r.conns, conn.ID)
			go st.closeAMQP()
		}
		r.mu.Unlock()
	}
	cl, err := buildClient(conn)
	if err != nil {
		return nil, err
	}
	st := &connState{client: cl, stamp: conn.UpdatedAt, lastUsed: time.Now(), amqp: map[string]*amqp.Connection{}}
	if conn.ID != "" {
		r.mu.Lock()
		if existing, ok := r.conns[conn.ID]; ok && existing.stamp.Equal(conn.UpdatedAt) {
			st = existing
		} else {
			r.conns[conn.ID] = st
		}
		r.mu.Unlock()
	}
	return st, nil
}

func tlsConfig(conn models.RabbitConnection) (*tls.Config, error) {
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

func buildClient(conn models.RabbitConnection) (*http.Client, error) {
	cfg, err := tlsConfig(conn)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			TLSClientConfig:     cfg,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		// A management URL that redirects (UI path behind an SSO proxy) must
		// not turn into a misleading 200 login page.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// Options parses the connection's OptionsJSON.
func Options(conn models.RabbitConnection) map[string]any {
	out := map[string]any{}
	if strings.TrimSpace(conn.OptionsJSON) != "" {
		_ = json.Unmarshal([]byte(conn.OptionsJSON), &out)
	}
	return out
}

// BaseURL normalizes the management URL: scheme defaults to http (the
// management plugin's default listener), a trailing /api or slash is dropped.
func BaseURL(conn models.RabbitConnection) (string, error) {
	u := strings.TrimSpace(conn.URL)
	if u == "" {
		return "", fmt.Errorf("management URL is required")
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "http://" + u
	}
	u = strings.TrimRight(u, "/")
	u = strings.TrimSuffix(u, "/api")
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return "", fmt.Errorf("invalid management URL")
	}
	return u, nil
}

func (r *Registry) password(conn models.RabbitConnection) (string, error) {
	if conn.PasswordEnc == "" {
		return "", nil
	}
	p, err := r.cipher.Decrypt(conn.PasswordEnc)
	if err != nil {
		return "", fmt.Errorf("decrypt password: %w", err)
	}
	return p, nil
}

// ── errors ──────────────────────────────────────────────────────────

// APIError is a management API (or AMQP) failure with a readable message
// and a machine code: auth | forbidden | not_found | bad_request |
// unreachable | upstream.
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
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(body, &b)
	reason := strings.TrimSpace(b.Reason)
	if reason == "" {
		reason = strings.TrimSpace(b.Error)
	}
	e := &APIError{Status: status, Code: "upstream"}
	switch status {
	case http.StatusUnauthorized:
		e.Code = "auth"
		e.Message = "RabbitMQ rejected the username or password (401)."
		if strings.Contains(strings.ToLower(reason), "management") {
			e.Code = "forbidden"
			e.Message = "This RabbitMQ user may not use the management API — give it the management, monitoring or administrator tag."
		}
	case http.StatusForbidden:
		e.Code = "forbidden"
		e.Message = "RabbitMQ refused the action for this user (403)" + suffix(reason)
	case http.StatusNotFound:
		e.Code = "not_found"
		e.Message = "Not found" + suffix(reason)
	case http.StatusBadRequest, http.StatusMethodNotAllowed:
		e.Code = "bad_request"
		e.Message = "RabbitMQ refused the request" + suffix(reason)
	default:
		e.Message = fmt.Sprintf("RabbitMQ returned %d", status) + suffix(reason)
	}
	return e
}

func suffix(reason string) string {
	if reason == "" {
		return "."
	}
	return ": " + reason
}

// ── management HTTP ─────────────────────────────────────────────────

// Do calls the management API: path is relative to /api ("/queues/%2F").
// body (any) is JSON-encoded; out (any) receives the decoded answer.
func (r *Registry) Do(ctx context.Context, conn models.RabbitConnection, method, path string, query url.Values, body any, out any) error {
	raw, err := r.DoRaw(ctx, conn, method, path, query, body, nil)
	if err != nil {
		return err
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode RabbitMQ response: %w", err)
	}
	return nil
}

// DoRaw is Do returning the raw body (pass-through endpoints).
func (r *Registry) DoRaw(ctx context.Context, conn models.RabbitConnection, method, path string, query url.Values, body any, headers map[string]string) ([]byte, error) {
	st, err := r.state(conn)
	if err != nil {
		return nil, err
	}
	base, err := BaseURL(conn)
	if err != nil {
		return nil, err
	}
	pass, err := r.password(conn)
	if err != nil {
		return nil, err
	}
	u := base + "/api" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	cctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(conn.Username, pass)
	// "*/*" too: the purge endpoint (DELETE …/contents) answers 406 to a
	// strict application/json Accept.
	req.Header.Set("Accept", "application/json, */*")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := st.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return nil, &APIError{Code: "unreachable", Message: "RabbitMQ management API did not answer in time."}
		}
		return nil, &APIError{Code: "unreachable", Message: "Cannot reach the RabbitMQ management API at " + base + ": " + shortNetErr(err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, &APIError{Status: resp.StatusCode, Code: "bad_request", Message: "The URL redirects (" + resp.Header.Get("Location") + ") — use the management API address, e.g. http://host:15672."}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newAPIError(resp.StatusCode, raw)
	}
	return raw, nil
}

func shortNetErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ne *net.OpError
	if errors.As(err, &ne) && ne.Err != nil {
		return ne.Err.Error()
	}
	return err.Error()
}

// Version returns the broker version (cached).
func (r *Registry) Version(ctx context.Context, conn models.RabbitConnection) string {
	st, err := r.state(conn)
	if err != nil {
		return ""
	}
	st.infoMu.Lock()
	if st.version != "" && time.Since(st.infoAt) < versionTTL {
		v := st.version
		st.infoMu.Unlock()
		return v
	}
	st.infoMu.Unlock()
	var ov struct {
		Version string `json:"rabbitmq_version"`
	}
	if r.Do(ctx, conn, http.MethodGet, "/overview", url.Values{"columns": {"rabbitmq_version"}}, nil, &ov) != nil {
		return ""
	}
	st.infoMu.Lock()
	st.version, st.infoAt = ov.Version, time.Now()
	st.infoMu.Unlock()
	return ov.Version
}

// Major parses "4.0.5" → 4 (0 when unknown).
func Major(version string) int {
	n, _ := strconv.Atoi(strings.SplitN(version, ".", 2)[0])
	return n
}

// ── AMQP ────────────────────────────────────────────────────────────

// amqpURI builds the AMQP URI: host from options.amqpHost (default the
// management host), port options.amqpPort (default 5672, 5671 with TLS).
func (r *Registry) amqpURI(conn models.RabbitConnection, vhost string) (string, bool, error) {
	base, err := BaseURL(conn)
	if err != nil {
		return "", false, err
	}
	mu, _ := url.Parse(base)
	opts := Options(conn)
	host := strings.TrimSpace(fmt.Sprint(opts["amqpHost"]))
	if host == "" || host == "<nil>" {
		host = mu.Hostname()
	}
	useTLS, _ := opts["amqpTLS"].(bool)
	port := 5672
	if useTLS {
		port = 5671
	}
	switch p := opts["amqpPort"].(type) {
	case float64:
		if p > 0 {
			port = int(p)
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && n > 0 {
			port = n
		}
	}
	pass, err := r.password(conn)
	if err != nil {
		return "", false, err
	}
	scheme := "amqp"
	if useTLS {
		scheme = "amqps"
	}
	u := url.URL{
		Scheme: scheme,
		User:   url.UserPassword(conn.Username, pass),
		Host:   net.JoinHostPort(host, strconv.Itoa(port)),
		// "/" must travel as %2F; an empty path is the default vhost too.
		RawPath: "/" + url.PathEscape(vhost),
		Path:    "/" + vhost,
	}
	return u.String(), useTLS, nil
}

// amqpConn returns a live AMQP connection for a vhost, dialing on demand.
func (r *Registry) amqpConn(ctx context.Context, conn models.RabbitConnection, vhost string) (*amqp.Connection, error) {
	st, err := r.state(conn)
	if err != nil {
		return nil, err
	}
	st.amqpMu.Lock()
	defer st.amqpMu.Unlock()
	if c, ok := st.amqp[vhost]; ok && !c.IsClosed() {
		return c, nil
	}
	uri, useTLS, err := r.amqpURI(conn, vhost)
	if err != nil {
		return nil, err
	}
	cfg := amqp.Config{
		Heartbeat:  20 * time.Second,
		Locale:     "en_US",
		Properties: amqp.Table{"connection_name": "PatchMan"},
		Dial: func(network, addr string) (net.Conn, error) {
			d := net.Dialer{Timeout: 10 * time.Second}
			return d.DialContext(ctx, network, addr)
		},
	}
	if useTLS {
		tc, err := tlsConfig(conn)
		if err != nil {
			return nil, err
		}
		cfg.TLSClientConfig = tc
	}
	c, err := amqp.DialConfig(uri, cfg)
	if err != nil {
		return nil, amqpError(err, uri)
	}
	st.amqp[vhost] = c
	return c, nil
}

// amqpError turns AMQP failures into readable APIErrors.
func amqpError(err error, uri string) error {
	var ae *amqp.Error
	if errors.As(err, &ae) {
		switch ae.Code {
		case amqp.AccessRefused:
			return &APIError{Code: "forbidden", Message: "RabbitMQ refused the AMQP operation for this user (ACCESS_REFUSED): " + ae.Reason + ". Check the user's configure/write/read permissions on the vhost."}
		case amqp.NotFound:
			return &APIError{Code: "not_found", Message: "Not found: " + ae.Reason}
		case amqp.PreconditionFailed:
			return &APIError{Code: "bad_request", Message: "RabbitMQ refused it (PRECONDITION_FAILED): " + ae.Reason}
		}
		return &APIError{Code: "upstream", Message: "AMQP error " + strconv.Itoa(ae.Code) + ": " + ae.Reason}
	}
	host := uri
	if u, perr := url.Parse(uri); perr == nil {
		host = u.Host
	}
	if strings.Contains(err.Error(), "username or password not allowed") || strings.Contains(err.Error(), "ACCESS_REFUSED") {
		return &APIError{Code: "auth", Message: "RabbitMQ refused the AMQP login (user/password, or no access to this vhost)."}
	}
	return &APIError{Code: "unreachable", Message: "Cannot open an AMQP connection to " + host + " (set the AMQP host/port in the connection's advanced options if it differs from the management address): " + shortNetErr(err)}
}

// channel opens a fresh channel; a closed connection is redialed once.
func (r *Registry) channel(ctx context.Context, conn models.RabbitConnection, vhost string) (*amqp.Channel, error) {
	c, err := r.amqpConn(ctx, conn, vhost)
	if err != nil {
		return nil, err
	}
	ch, err := c.Channel()
	if err != nil {
		_ = c.Close()
		c, err = r.amqpConn(ctx, conn, vhost)
		if err != nil {
			return nil, err
		}
		if ch, err = c.Channel(); err != nil {
			return nil, amqpError(err, "")
		}
	}
	return ch, nil
}
