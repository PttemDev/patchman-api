package kafkaquery

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"patchman-be/internal/models"
	"patchman-be/internal/services"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"
)

// Registry pools one long-lived *kgo.Client per connection (used for
// admin + produce), decrypting the SASL secret just-in-time. Peek uses a
// throwaway client instead so it can assign partitions without a group.
type Registry struct {
	cipher *services.TokenCipher

	mu      sync.Mutex
	clients map[string]*pooledClient // key = connection.ID
}

type pooledClient struct {
	client   *kgo.Client
	lastUsed time.Time
}

const clientIdleTimeout = 5 * time.Minute

func NewRegistry(cipher *services.TokenCipher) *Registry {
	r := &Registry{cipher: cipher, clients: map[string]*pooledClient{}}
	go r.reapLoop()
	return r
}

// baseOpts builds the shared kgo options (brokers, TLS, SASL) from a
// decrypted config. Callers append role-specific opts (consume/produce).
func baseOpts(cfg ConnectionConfig) ([]kgo.Opt, error) {
	seeds := splitServers(cfg.BootstrapServers)
	if len(seeds) == 0 {
		return nil, fmt.Errorf("no bootstrap servers configured")
	}
	clientID := "patchman"
	if v, ok := cfg.Options["clientId"].(string); ok && v != "" {
		clientID = v
	}
	opts := []kgo.Opt{
		kgo.SeedBrokers(seeds...),
		kgo.ClientID(clientID),
	}

	proto := strings.ToUpper(strings.TrimSpace(cfg.SecurityProtocol))
	if proto == "SSL" || proto == "SASL_SSL" {
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
		opts = append(opts, kgo.DialTLSConfig(tlsCfg))
	}

	if strings.HasPrefix(proto, "SASL") {
		mech := strings.ToUpper(strings.TrimSpace(cfg.SASLMechanism))
		switch mech {
		case "PLAIN":
			opts = append(opts, kgo.SASL(plain.Auth{User: cfg.SASLUsername, Pass: cfg.SASLPassword}.AsMechanism()))
		case "SCRAM-SHA-256":
			opts = append(opts, kgo.SASL(scram.Auth{User: cfg.SASLUsername, Pass: cfg.SASLPassword}.AsSha256Mechanism()))
		case "SCRAM-SHA-512":
			opts = append(opts, kgo.SASL(scram.Auth{User: cfg.SASLUsername, Pass: cfg.SASLPassword}.AsSha512Mechanism()))
		case "":
			return nil, fmt.Errorf("SASL mechanism is required for %s", proto)
		default:
			return nil, fmt.Errorf("unsupported SASL mechanism: %s", cfg.SASLMechanism)
		}
	}
	return opts, nil
}

// client returns the pooled admin/produce client for a connection.
func (r *Registry) client(conn models.KafkaConnection) (*kgo.Client, error) {
	r.mu.Lock()
	if pc, ok := r.clients[conn.ID]; ok {
		pc.lastUsed = time.Now()
		r.mu.Unlock()
		return pc.client, nil
	}
	r.mu.Unlock()

	cfg, err := r.resolve(conn)
	if err != nil {
		return nil, err
	}
	opts, err := baseOpts(cfg)
	if err != nil {
		return nil, err
	}
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.clients[conn.ID] = &pooledClient{client: cl, lastUsed: time.Now()}
	r.mu.Unlock()
	return cl, nil
}

// Admin wraps the pooled client for kadm operations.
func (r *Registry) Admin(conn models.KafkaConnection) (*kadm.Client, error) {
	cl, err := r.client(conn)
	if err != nil {
		return nil, err
	}
	return kadm.NewClient(cl), nil
}

// Test opens a throwaway client and lists brokers to confirm the config
// actually connects + authenticates.
func (r *Registry) Test(ctx context.Context, conn models.KafkaConnection) error {
	cfg, err := r.resolve(conn)
	if err != nil {
		return err
	}
	opts, err := baseOpts(cfg)
	if err != nil {
		return err
	}
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		return err
	}
	defer cl.Close()
	adm := kadm.NewClient(cl)
	brokers, err := adm.ListBrokers(ctx)
	if err != nil {
		return err
	}
	if len(brokers) == 0 {
		return fmt.Errorf("connected but no brokers reported")
	}
	return nil
}

// Close discards the pooled client (config edit / delete).
func (r *Registry) Close(connectionID string) {
	r.mu.Lock()
	pc, ok := r.clients[connectionID]
	delete(r.clients, connectionID)
	r.mu.Unlock()
	if ok {
		pc.client.Close()
	}
}

func (r *Registry) resolve(conn models.KafkaConnection) (ConnectionConfig, error) {
	pass := ""
	if conn.SASLPasswordEnc != "" {
		p, err := r.cipher.Decrypt(conn.SASLPasswordEnc)
		if err != nil {
			return ConnectionConfig{}, fmt.Errorf("decrypt sasl password: %w", err)
		}
		pass = p
	}
	return ConnectionConfig{
		BootstrapServers: conn.BootstrapServers,
		SecurityProtocol: conn.SecurityProtocol,
		SASLMechanism:    conn.SASLMechanism,
		SASLUsername:     conn.SASLUsername,
		SASLPassword:     pass,
		Options:          parseOptions(conn.OptionsJSON),
	}, nil
}

func (r *Registry) reapLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-clientIdleTimeout)
		r.mu.Lock()
		for id, pc := range r.clients {
			if pc.lastUsed.Before(cutoff) {
				pc.client.Close()
				delete(r.clients, id)
			}
		}
		r.mu.Unlock()
	}
}

func splitServers(s string) []string {
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

// encodeBytes renders a Kafka key/value for JSON transport: UTF-8 text
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

// sortMessages orders by (partition, offset) for a stable, readable list.
func sortMessages(msgs []Message) {
	sort.Slice(msgs, func(i, j int) bool {
		if msgs[i].Partition != msgs[j].Partition {
			return msgs[i].Partition < msgs[j].Partition
		}
		return msgs[i].Offset < msgs[j].Offset
	})
}
