package redisquery

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/redis/go-redis/v9"
)

// Registry pools one *redis.UniversalClient per (connection, db-index),
// decrypting the auth secret just-in-time. The pool key embeds the db so
// switching db-index is race-free — each db gets its own client rather than
// mutating a shared one with SELECT. Cluster connections always use db 0.
type Registry struct {
	cipher *services.TokenCipher

	mu      sync.Mutex
	clients map[string]*pooledClient // key = connID + ":" + db
}

type pooledClient struct {
	client   redis.UniversalClient
	lastUsed time.Time
}

const clientIdleTimeout = 5 * time.Minute

func NewRegistry(cipher *services.TokenCipher) *Registry {
	r := &Registry{cipher: cipher, clients: map[string]*pooledClient{}}
	go r.reapLoop()
	return r
}

// buildOptions translates a decrypted config into redis.UniversalOptions.
// db overrides cfg.DBIndex so a pooled client can be minted per db-index.
func buildOptions(cfg ConnectionConfig, db int) (*redis.UniversalOptions, error) {
	seeds := splitAddrs(cfg.Address)
	if len(seeds) == 0 {
		return nil, fmt.Errorf("no address configured")
	}
	opts := &redis.UniversalOptions{
		Addrs:    seeds,
		Username: cfg.Username,
		Password: cfg.Password,
	}

	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	switch mode {
	case "sentinel":
		if strings.TrimSpace(cfg.SentinelMaster) == "" {
			return nil, fmt.Errorf("sentinelMaster is required for sentinel mode")
		}
		opts.MasterName = cfg.SentinelMaster
		opts.SentinelPassword = cfg.SentinelPassword
		opts.DB = db
	case "cluster":
		// Cluster ignores DB (always logical db 0) and has no MasterName.
		opts.MasterName = ""
	default: // standalone
		opts.MasterName = ""
		opts.DB = db
	}

	if cfg.TLS {
		tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if b, ok := cfg.Options["insecureSkipVerify"].(bool); ok && b {
			tlsCfg.InsecureSkipVerify = true
		}
		if ca, ok := cfg.Options["caCert"].(string); ok && strings.TrimSpace(ca) != "" {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM([]byte(ca)) {
				return nil, fmt.Errorf("caCert is not valid PEM")
			}
			tlsCfg.RootCAs = pool
		}
		opts.TLSConfig = tlsCfg
	}

	// Client name: only when the user set one, and best-effort. go-redis's own
	// ClientName pipelines CLIENT SETNAME into the connection handshake and
	// fails the connection on any error — so an ACL user without the CLIENT
	// command (common for app/dev users) got "NOPERM … 'client|setname'" on
	// every call. Naming the connection is cosmetic (CLIENT LIST), never worth
	// that; there is no default name for the same reason.
	if name, ok := cfg.Options["clientName"].(string); ok && strings.TrimSpace(name) != "" {
		name = strings.TrimSpace(name)
		opts.OnConnect = func(ctx context.Context, cn *redis.Conn) error {
			_ = cn.ClientSetName(ctx, name).Err()
			return nil
		}
	}
	if b, ok := cfg.Options["routeReadsToReplicas"].(bool); ok && b {
		opts.ReadOnly = true
		opts.RouteRandomly = true
	}
	if v, ok := cfg.Options["dialTimeoutMs"].(float64); ok && v > 0 {
		opts.DialTimeout = time.Duration(v) * time.Millisecond
	} else {
		opts.DialTimeout = 10 * time.Second
	}
	if v, ok := cfg.Options["poolSize"].(float64); ok && v > 0 {
		opts.PoolSize = int(v)
	}
	return opts, nil
}

// effectiveDB clamps the requested db to 0 for cluster mode.
func effectiveDB(conn models.RedisConnection, db int) int {
	if strings.ToLower(strings.TrimSpace(conn.Mode)) == "cluster" {
		return 0
	}
	if db < 0 {
		return conn.DBIndex
	}
	return db
}

// client returns the pooled client for a (connection, db) pair, building it
// on first use.
func (r *Registry) client(conn models.RedisConnection, db int) (redis.UniversalClient, error) {
	db = effectiveDB(conn, db)
	key := conn.ID + ":" + strconv.Itoa(db)

	r.mu.Lock()
	if pc, ok := r.clients[key]; ok {
		pc.lastUsed = time.Now()
		r.mu.Unlock()
		return pc.client, nil
	}
	r.mu.Unlock()

	cfg, err := r.resolve(conn)
	if err != nil {
		return nil, err
	}
	opts, err := buildOptions(cfg, db)
	if err != nil {
		return nil, err
	}
	cl := redis.NewUniversalClient(opts)

	r.mu.Lock()
	r.clients[key] = &pooledClient{client: cl, lastUsed: time.Now()}
	r.mu.Unlock()
	return cl, nil
}

// Test opens a throwaway client and PINGs to confirm the config connects +
// authenticates.
func (r *Registry) Test(ctx context.Context, conn models.RedisConnection) error {
	cfg, err := r.resolve(conn)
	if err != nil {
		return err
	}
	opts, err := buildOptions(cfg, effectiveDB(conn, conn.DBIndex))
	if err != nil {
		return err
	}
	cl := redis.NewUniversalClient(opts)
	defer cl.Close()
	if err := cl.Ping(ctx).Err(); err != nil {
		return err
	}
	return nil
}

// Close discards every pooled client for a connection (config edit / delete)
// — one per db-index that has been touched.
func (r *Registry) Close(connectionID string) {
	prefix := connectionID + ":"
	r.mu.Lock()
	var drop []redis.UniversalClient
	for key, pc := range r.clients {
		if strings.HasPrefix(key, prefix) {
			drop = append(drop, pc.client)
			delete(r.clients, key)
		}
	}
	r.mu.Unlock()
	for _, cl := range drop {
		_ = cl.Close()
	}
}

func (r *Registry) resolve(conn models.RedisConnection) (ConnectionConfig, error) {
	pass := ""
	if conn.PasswordEnc != "" {
		p, err := r.cipher.Decrypt(conn.PasswordEnc)
		if err != nil {
			return ConnectionConfig{}, fmt.Errorf("decrypt password: %w", err)
		}
		pass = p
	}
	sentPass := ""
	if conn.SentinelPasswordEnc != "" {
		p, err := r.cipher.Decrypt(conn.SentinelPasswordEnc)
		if err != nil {
			return ConnectionConfig{}, fmt.Errorf("decrypt sentinel password: %w", err)
		}
		sentPass = p
	}
	return ConnectionConfig{
		Address:          conn.Address,
		Mode:             conn.Mode,
		SentinelMaster:   conn.SentinelMaster,
		Username:         conn.Username,
		Password:         pass,
		SentinelPassword: sentPass,
		DBIndex:          conn.DBIndex,
		TLS:              conn.TLS,
		Options:          parseOptions(conn.OptionsJSON),
	}, nil
}

func (r *Registry) reapLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-clientIdleTimeout)
		r.mu.Lock()
		for key, pc := range r.clients {
			if pc.lastUsed.Before(cutoff) {
				_ = pc.client.Close()
				delete(r.clients, key)
			}
		}
		r.mu.Unlock()
	}
}

func splitAddrs(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
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

// encodeBytes renders a Redis binary value for JSON transport: UTF-8 text
// when valid, otherwise base64 with the encoding flagged.
func encodeBytes(b []byte) (string, string) {
	if b == nil {
		return "", "utf8"
	}
	if utf8.Valid(b) {
		return string(b), "utf8"
	}
	return base64.StdEncoding.EncodeToString(b), "base64"
}
