package esquery

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/services"
)

// Registry pools one *http.Client per connection, decrypting the auth secret
// just-in-time. Unlike the Redis registry there is no per-db keying — an ES
// connection is a single HTTP endpoint set. Mirrors the pool/reap/Close/Test
// shape of redisquery.Registry.
type Registry struct {
	cipher *services.TokenCipher

	mu      sync.Mutex
	clients map[string]*pooledClient // key = connID
}

type pooledClient struct {
	client   *http.Client
	lastUsed time.Time
}

const clientIdleTimeout = 5 * time.Minute

func NewRegistry(cipher *services.TokenCipher) *Registry {
	r := &Registry{cipher: cipher, clients: map[string]*pooledClient{}}
	go r.reapLoop()
	return r
}

// resolved is the decrypted, ready-to-use view of a connection.
type resolved struct {
	baseURL    string
	authHeader string
	client     *http.Client

	// kibanaProxy fields: when isKibanaProxy is set, rawDo rewrites the request
	// through Kibana's Console proxy. baseURL then holds the Kibana URL.
	isKibanaProxy   bool
	kibanaSpace     string // sanitized space slug ("" = default space)
	kibanaAuthMode  string // "basic" | "cookie"
	kibanaAuthValue string // "Basic base64(user:pass)" for basic mode, else ""
	kibanaCookie    string // sanitized Cookie header value for cookie mode, else ""
}

// client returns the pooled *http.Client for a connection, building it (and
// its TLS transport) on first use.
func (r *Registry) client(conn models.ESConnection) (*http.Client, error) {
	r.mu.Lock()
	if pc, ok := r.clients[conn.ID]; ok && conn.ID != "" {
		pc.lastUsed = time.Now()
		r.mu.Unlock()
		return pc.client, nil
	}
	r.mu.Unlock()

	cl, err := buildClient(conn)
	if err != nil {
		return nil, err
	}
	if conn.ID != "" {
		r.mu.Lock()
		r.clients[conn.ID] = &pooledClient{client: cl, lastUsed: time.Now()}
		r.mu.Unlock()
	}
	return cl, nil
}

// buildClient assembles an *http.Client with a TLS transport derived from the
// connection's OptionsJSON: caCert PEM pool, insecureSkipVerify, caFingerprint
// (ES8 self-signed cert SHA-256 pin), client cert/key (mTLS).
func buildClient(conn models.ESConnection) (*http.Client, error) {
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
	// caFingerprint: pin the leaf/CA cert by SHA-256 (ES8 self-signed http_ca).
	// We skip the library's chain verification and validate the fingerprint
	// against every presented cert ourselves.
	if fp, ok := opts["caFingerprint"].(string); ok && strings.TrimSpace(fp) != "" {
		want := normalizeFingerprint(fp)
		tlsCfg.InsecureSkipVerify = true
		tlsCfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			for _, raw := range rawCerts {
				sum := sha256.Sum256(raw)
				if strings.EqualFold(hex.EncodeToString(sum[:]), want) {
					return nil
				}
			}
			return fmt.Errorf("server certificate fingerprint does not match caFingerprint")
		}
	}
	// mTLS: client cert + key PEM.
	if cert, ok := opts["clientCert"].(string); ok && strings.TrimSpace(cert) != "" {
		key, _ := opts["clientKey"].(string)
		pair, err := tls.X509KeyPair([]byte(cert), []byte(key))
		if err != nil {
			return nil, fmt.Errorf("client cert/key: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{pair}
	}

	transport := &http.Transport{
		TLSClientConfig:       tlsCfg,
		MaxIdleConns:          20,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableKeepAlives:     false,
	}
	return &http.Client{Transport: transport}, nil
}

// resolve decrypts secrets, computes the base URL + auth header, and returns
// the pooled client — the single entry point every op path uses.
func (r *Registry) resolve(conn models.ESConnection) (*resolved, error) {
	cl, err := r.client(conn)
	if err != nil {
		return nil, err
	}
	base, err := baseURL(conn)
	if err != nil {
		return nil, err
	}
	auth, err := r.authHeader(conn)
	if err != nil {
		return nil, err
	}
	res := &resolved{baseURL: base, authHeader: auth, client: cl}
	if strings.EqualFold(strings.TrimSpace(conn.AuthKind), "kibanaProxy") {
		if err := r.fillKibana(conn, res); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// fillKibana populates the kibanaProxy fields on res: the space slug, the auth
// mode, and the decrypted auth material (a "Basic …" header for basic mode, or
// the sanitized session cookie for cookie mode). rawDo consumes these.
func (r *Registry) fillKibana(conn models.ESConnection, res *resolved) error {
	opts := parseOptions(conn.OptionsJSON)
	res.isKibanaProxy = true
	if s, ok := opts["kibanaSpace"].(string); ok {
		res.kibanaSpace = strings.Trim(strings.TrimSpace(s), "/")
	}
	mode := "basic"
	if m, ok := opts["kibanaAuthMode"].(string); ok && strings.TrimSpace(m) != "" {
		mode = strings.ToLower(strings.TrimSpace(m))
	}
	res.kibanaAuthMode = mode
	if mode == "cookie" {
		if conn.CookieEnc != "" {
			ck, err := r.cipher.Decrypt(conn.CookieEnc)
			if err != nil {
				return fmt.Errorf("decrypt cookie: %w", err)
			}
			// Strip whitespace/newlines so a pasted cookie never produces an
			// invalid header field value; prefix "sid=" when it's a bare value.
			ck = strings.TrimSpace(strings.NewReplacer("\r", "", "\n", "").Replace(ck))
			if ck != "" && !strings.Contains(ck, "=") {
				ck = "sid=" + ck
			}
			res.kibanaCookie = ck
		}
		return nil
	}
	pass := ""
	if conn.PasswordEnc != "" {
		p, err := r.cipher.Decrypt(conn.PasswordEnc)
		if err != nil {
			return fmt.Errorf("decrypt password: %w", err)
		}
		pass = p
	}
	raw := conn.Username + ":" + pass
	res.kibanaAuthValue = "Basic " + base64.StdEncoding.EncodeToString([]byte(raw))
	return nil
}

// authHeader builds the Authorization header value for the connection's
// AuthKind, decrypting the relevant secret. Empty string = no auth header.
func (r *Registry) authHeader(conn models.ESConnection) (string, error) {
	switch strings.ToLower(strings.TrimSpace(conn.AuthKind)) {
	case "basic":
		pass := ""
		if conn.PasswordEnc != "" {
			p, err := r.cipher.Decrypt(conn.PasswordEnc)
			if err != nil {
				return "", fmt.Errorf("decrypt password: %w", err)
			}
			pass = p
		}
		raw := conn.Username + ":" + pass
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(raw)), nil
	case "apikey":
		if conn.ApiKeyEnc == "" {
			return "", nil
		}
		key, err := r.cipher.Decrypt(conn.ApiKeyEnc)
		if err != nil {
			return "", fmt.Errorf("decrypt apiKey: %w", err)
		}
		return "ApiKey " + strings.TrimSpace(key), nil
	case "bearer":
		if conn.BearerEnc == "" {
			return "", nil
		}
		tok, err := r.cipher.Decrypt(conn.BearerEnc)
		if err != nil {
			return "", fmt.Errorf("decrypt bearer: %w", err)
		}
		return "Bearer " + strings.TrimSpace(tok), nil
	default: // none
		return "", nil
	}
}

// Test opens the pooled client and GETs / to confirm reachability + auth.
func (r *Registry) Test(ctx context.Context, conn models.ESConnection) (*ExecResult, error) {
	return r.rawDo(ctx, conn, http.MethodGet, "/", nil)
}

// Close discards the pooled client for a connection (config edit / delete).
func (r *Registry) Close(connectionID string) {
	r.mu.Lock()
	pc, ok := r.clients[connectionID]
	delete(r.clients, connectionID)
	r.mu.Unlock()
	if ok {
		pc.client.CloseIdleConnections()
	}
}

func (r *Registry) reapLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-clientIdleTimeout)
		r.mu.Lock()
		for key, pc := range r.clients {
			if pc.lastUsed.Before(cutoff) {
				pc.client.CloseIdleConnections()
				delete(r.clients, key)
			}
		}
		r.mu.Unlock()
	}
}

// baseURL returns the first node URL, or decodes CloudID. Trailing slash
// trimmed so path joins are clean. For kibanaProxy connections it returns the
// Kibana URL (from OptionsJSON) so the pooled client + TLS opts still resolve;
// nodes/cloudId may be empty in that mode.
func baseURL(conn models.ESConnection) (string, error) {
	if strings.EqualFold(strings.TrimSpace(conn.AuthKind), "kibanaProxy") {
		opts := parseOptions(conn.OptionsJSON)
		ku, _ := opts["kibanaUrl"].(string)
		ku = strings.TrimSpace(ku)
		if ku == "" {
			return "", fmt.Errorf("kibanaUrl is required for the Kibana proxy auth mode")
		}
		return withScheme(strings.TrimRight(ku, "/"), conn.TLS), nil
	}
	for _, part := range strings.Split(conn.Nodes, ",") {
		if p := strings.TrimSpace(part); p != "" {
			return withScheme(strings.TrimRight(p, "/"), conn.TLS), nil
		}
	}
	if strings.TrimSpace(conn.CloudID) != "" {
		u, err := decodeCloudID(conn.CloudID)
		if err != nil {
			return "", err
		}
		return u, nil
	}
	return "", fmt.Errorf("no nodes or cloudId configured")
}

// withScheme prepends https:// (or http:// when TLS is off) to a bare host.
func withScheme(host string, tlsOn bool) string {
	if strings.Contains(host, "://") {
		return host
	}
	if tlsOn {
		return "https://" + host
	}
	return "http://" + host
}

// decodeCloudID decodes an Elastic Cloud id: base64 → "cluster$esId$kibanaId"
// → https://<esId>.<host>:443. Port is optional in the encoded host segment.
func decodeCloudID(cloudID string) (string, error) {
	id := cloudID
	if i := strings.Index(id, ":"); i >= 0 {
		// Elastic Cloud ids are usually "<label>:<base64>".
		id = id[i+1:]
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(id))
	if err != nil {
		return "", fmt.Errorf("cloudId is not valid base64")
	}
	parts := strings.Split(string(decoded), "$")
	if len(parts) < 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", fmt.Errorf("cloudId decodes to an unexpected shape")
	}
	hostPart := parts[0]
	port := "443"
	if i := strings.Index(hostPart, ":"); i >= 0 {
		host := hostPart[:i]
		port = hostPart[i+1:]
		hostPart = host
	}
	return fmt.Sprintf("https://%s.%s:%s", parts[1], hostPart, port), nil
}

func normalizeFingerprint(fp string) string {
	fp = strings.TrimSpace(fp)
	fp = strings.ReplaceAll(fp, ":", "")
	fp = strings.ReplaceAll(fp, " ", "")
	return strings.ToLower(fp)
}

func parseOptions(raw string) map[string]any {
	if raw == "" {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}
