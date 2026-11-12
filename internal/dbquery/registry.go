package dbquery

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"patchman-be/internal/models"
	"patchman-be/internal/services"
)

// Registry owns the per-connection driver sessions + schema cache. A
// single instance is wired at boot from main.go and shared across all
// HTTP handlers (handlers/dbquery_*.go). It's safe for concurrent use.
type Registry struct {
	cipher *services.TokenCipher

	mu       sync.Mutex
	sessions map[string]*pooledSession // key = connection.ID
	schema   map[string]cachedSchema   // key = connection.ID
	drivers  map[Kind]Driver
}

type pooledSession struct {
	session  Session
	openedAt time.Time
	lastUsed time.Time
}

type cachedSchema struct {
	schema Schema
	at     time.Time
}

const (
	sessionIdleTimeout = 5 * time.Minute
	schemaCacheTTL     = 15 * time.Minute
)

// NewRegistry wires the three drivers and starts the idle reaper.
// Wire-compatible aliases (cockroachdb / yugabytedb / redshift / mariadb /
// tidb) re-use the same driver as their base family — the FE just gets
// a clearer label and a sensible default port.
func NewRegistry(cipher *services.TokenCipher) *Registry {
	pg := NewPostgresDriver()
	my := NewMySQLDriver()
	mongo := NewMongoDriver()
	mssql := NewSQLServerDriver()
	r := &Registry{
		cipher:   cipher,
		sessions: map[string]*pooledSession{},
		schema:   map[string]cachedSchema{},
		drivers: map[Kind]Driver{
			KindPostgres:  pg,
			KindCockroach: pg,
			KindYugabyte:  pg,
			KindRedshift:  pg,
			KindMySQL:     my,
			KindMariaDB:   my,
			KindTiDB:      my,
			KindMongo:     mongo,
			KindSQLServer: mssql,
		},
	}
	go r.reapLoop()
	return r
}

// Session returns an open Session for the given connection, opening
// one on the first call and reusing it on subsequent ones until the
// idle reaper closes it. Decrypts the password just-in-time so the
// plaintext lives only inside the driver-call frame.
func (r *Registry) Session(ctx context.Context, conn models.DBConnection) (Session, error) {
	r.mu.Lock()
	if ps, ok := r.sessions[conn.ID]; ok {
		ps.lastUsed = time.Now()
		r.mu.Unlock()
		return ps.session, nil
	}
	r.mu.Unlock()

	driver, ok := r.drivers[Kind(conn.Kind)]
	if !ok {
		return nil, fmt.Errorf("unknown driver kind: %s", conn.Kind)
	}

	password, err := r.cipher.Decrypt(conn.PasswordEnc)
	if err != nil {
		return nil, fmt.Errorf("decrypt password: %w", err)
	}
	dsn := ""
	if conn.ConnectionStringEnc != "" {
		dsn, err = r.cipher.Decrypt(conn.ConnectionStringEnc)
		if err != nil {
			return nil, fmt.Errorf("decrypt connection string: %w", err)
		}
	}

	cfg := ConnectionConfig{
		Kind:             Kind(conn.Kind),
		Host:             conn.Host,
		Port:             conn.Port,
		Database:         conn.Database,
		Username:         conn.Username,
		Password:         password,
		Options:          parseOptions(conn.OptionsJSON),
		ConnectionString: dsn,
	}
	sess, err := driver.Connect(ctx, cfg)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	r.mu.Lock()
	r.sessions[conn.ID] = &pooledSession{session: sess, openedAt: now, lastUsed: now}
	r.mu.Unlock()
	return sess, nil
}

// Schema returns a cached metadata snapshot, refreshing if the entry
// is missing or older than schemaCacheTTL.
func (r *Registry) Schema(ctx context.Context, conn models.DBConnection) (Schema, error) {
	r.mu.Lock()
	cs, ok := r.schema[conn.ID]
	r.mu.Unlock()
	if ok && time.Since(cs.at) < schemaCacheTTL {
		return cs.schema, nil
	}
	sess, err := r.Session(ctx, conn)
	if err != nil {
		return Schema{}, err
	}
	driver := r.drivers[Kind(conn.Kind)]
	out, err := driver.Introspect(ctx, sess)
	if err != nil {
		return Schema{}, err
	}
	r.mu.Lock()
	r.schema[conn.ID] = cachedSchema{schema: out, at: time.Now()}
	r.mu.Unlock()
	return out, nil
}

// InvalidateSchema bumps the cache so the next Schema() call re-runs
// Introspect — used by the explicit refresh endpoint + connection
// option edits.
func (r *Registry) InvalidateSchema(connectionID string) {
	r.mu.Lock()
	delete(r.schema, connectionID)
	r.mu.Unlock()
}

// CloseSession discards the pooled session, e.g. after a config edit
// or grant revocation.
func (r *Registry) CloseSession(connectionID string) {
	r.mu.Lock()
	ps, ok := r.sessions[connectionID]
	delete(r.sessions, connectionID)
	r.mu.Unlock()
	if ok {
		_ = ps.session.Close()
	}
}

// Test opens a one-shot session, pings via a trivial query, then
// discards it. Used by the connection-test endpoint so the user gets
// an immediate green/red signal before saving.
func (r *Registry) Test(ctx context.Context, conn models.DBConnection) error {
	_, err := r.TestInfo(ctx, conn)
	return err
}

// TestInfo is what a successful test learned about the connection.
type TestInfo struct {
	// AuthSource is set when sign-in only worked through this auth source
	// (Mongo falling back to admin), so the caller can save it.
	AuthSource string `json:"authSource,omitempty"`
	// DirectHost is the seed a Mongo replica set was reached through when
	// its announced member names (Members) do not resolve here.
	DirectHost    string   `json:"directHost,omitempty"`
	DirectPrimary bool     `json:"directPrimary,omitempty"`
	Members       []string `json:"members,omitempty"`
}

// testInfoReporter is implemented by sessions whose connect needed a
// fallback worth reporting.
type testInfoReporter interface{ testInfo() TestInfo }

// TestInfo dials the connection like Test and reports what it learned.
func (r *Registry) TestInfo(ctx context.Context, conn models.DBConnection) (TestInfo, error) {
	driver, ok := r.drivers[Kind(conn.Kind)]
	if !ok {
		return TestInfo{}, fmt.Errorf("unknown driver kind: %s", conn.Kind)
	}
	password, err := r.cipher.Decrypt(conn.PasswordEnc)
	if err != nil {
		return TestInfo{}, fmt.Errorf("decrypt password: %w", err)
	}
	dsn := ""
	if conn.ConnectionStringEnc != "" {
		dsn, err = r.cipher.Decrypt(conn.ConnectionStringEnc)
		if err != nil {
			return TestInfo{}, fmt.Errorf("decrypt connection string: %w", err)
		}
	}
	cfg := ConnectionConfig{
		Kind:             Kind(conn.Kind),
		Host:             conn.Host,
		Port:             conn.Port,
		Database:         conn.Database,
		Username:         conn.Username,
		Password:         password,
		Options:          parseOptions(conn.OptionsJSON),
		ConnectionString: dsn,
	}
	sess, err := driver.Connect(ctx, cfg)
	if err != nil {
		return TestInfo{}, err
	}
	defer sess.Close()
	var info TestInfo
	if rep, ok := sess.(testInfoReporter); ok {
		info = rep.testInfo()
	}
	return info, nil
}

func parseOptions(raw string) map[string]any {
	if raw == "" {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return map[string]any{}
	}
	if out == nil {
		out = map[string]any{}
	}
	return out
}

// reapLoop closes idle sessions on a steady cadence so a forgotten
// connection doesn't hog driver memory forever. Runs until the process
// exits — no shutdown hook in v1 since the process restart is what we
// rely on for cleanup.
func (r *Registry) reapLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-sessionIdleTimeout)
		r.mu.Lock()
		for id, ps := range r.sessions {
			if ps.lastUsed.Before(cutoff) {
				_ = ps.session.Close()
				delete(r.sessions, id)
			}
		}
		r.mu.Unlock()
	}
}
