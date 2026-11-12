// Package redisquery is the BE-side adapter for the user-facing "Redis"
// tool. It mirrors internal/kafkaquery: a Registry pools live clients per
// (connection, db-index), decrypting the auth secret just-in-time, and
// exposes SCAN-only key browsing, type-aware value reads, key metadata, a
// guarded command console (read-only by default, three-layer write gate),
// and a strictly non-committing pub/sub peek.
//
// The browse path NEVER issues KEYS (it would block the server on a large
// keyspace) — it uses SCAN with a bounded COUNT and paginates by cursor.
// The pub/sub peek uses a throwaway SUBSCRIBE/PSUBSCRIBE client and never
// registers a durable consumer, so browsing a channel leaves no trace.
package redisquery

// ConnectionConfig is the resolved (decrypted) config handed to the client
// builder. Built from models.RedisConnection + a TokenCipher decrypt so the
// builder never sees ciphertext.
type ConnectionConfig struct {
	Address          string // "host:6379" | comma-joined seeds
	Mode             string // standalone | sentinel | cluster
	SentinelMaster   string
	Username         string
	Password         string
	SentinelPassword string
	DBIndex          int
	TLS              bool
	// Parsed options_json.
	//   caCert               string (PEM) — custom CA for TLS
	//   insecureSkipVerify   bool
	//   clientName           string
	//   routeReadsToReplicas bool
	//   dialTimeoutMs        float64
	//   poolSize             float64
	Options map[string]any
}

// KeyEntry is one row in a key listing. TtlMs is -1 for no-expire, -2 when
// the key is missing (raced away between SCAN and PTTL). SizeBytes is the
// approximate MEMORY USAGE, -1 when unavailable.
type KeyEntry struct {
	Key       string `json:"key"`
	Type      string `json:"type"`
	TtlMs     int64  `json:"ttlMs"`
	SizeBytes int64  `json:"sizeBytes"`
}

// KeyListing is one browse page (one or more SCAN calls). Cursor=0 +
// Complete=true means the scan is exhausted; otherwise StopReason says why
// it paused ("limit" = enough matches, "budget" = time/iteration budget)
// and Cursor continues it. Pattern is the effective MATCH glob (after the
// smart search box expanded plain text), Limit the match target used, and
// Iterations the number of SCAN calls made.
type KeyListing struct {
	Keys       []KeyEntry `json:"keys"`
	Cursor     uint64     `json:"cursor"`
	Complete   bool       `json:"complete"`
	DB         int        `json:"db"`
	ScanMs     int        `json:"scanMs"`
	Pattern    string     `json:"pattern"`
	Limit      int        `json:"limit"`
	StopReason string     `json:"stopReason,omitempty"`
	Iterations int        `json:"iterations"`
}

// KeyMeta is the full metadata block for a single key.
type KeyMeta struct {
	Key         string `json:"key"`
	Type        string `json:"type"`
	Encoding    string `json:"encoding"`
	TtlMs       int64  `json:"ttlMs"`
	PttlMs      int64  `json:"pttlMs"`
	MemoryBytes int64  `json:"memoryBytes"`
	Length      int64  `json:"length"`
}

// ZMember is one sorted-set member with its score.
type ZMember struct {
	Member string  `json:"member"`
	Score  float64 `json:"score"`
}

// StreamEntry is one stream record (id + field/value map).
type StreamEntry struct {
	ID     string            `json:"id"`
	Fields map[string]string `json:"fields"`
}

// ValueResult is the type-aware value view. Only the field matching Type is
// populated; the rest stay nil/zero and are omitted.
type ValueResult struct {
	Key           string            `json:"key"`
	Type          string            `json:"type"`
	ValueEncoding string            `json:"valueEncoding"`
	TtlMs         int64             `json:"ttlMs"`
	MemoryBytes   int64             `json:"memoryBytes"`
	String        *string           `json:"string,omitempty"`
	Hash          map[string]string `json:"hash,omitempty"`
	List          []string          `json:"list,omitempty"`
	Set           []string          `json:"set,omitempty"`
	ZSet          []ZMember         `json:"zset,omitempty"`
	Stream        []StreamEntry     `json:"stream,omitempty"`
	Cursor        uint64            `json:"cursor"`
	Truncated     bool              `json:"truncated"`
}

// CommandResult is the console response. Reply is the rendered RESP reply
// (binary bulk strings become base64). Kind is the command class
// ("read" | "write"). Write echoes whether it was a write command.
type CommandResult struct {
	Reply      any    `json:"reply"`
	Kind       string `json:"kind"`
	DurationMs int    `json:"durationMs"`
	Write      bool   `json:"write"`
}

// PubSubMessage is one peeked pub/sub payload.
type PubSubMessage struct {
	Channel         string `json:"channel"`
	Pattern         string `json:"pattern,omitempty"`
	Payload         string `json:"payload"`
	PayloadEncoding string `json:"payloadEncoding"` // "utf8" | "base64"
}

// PubSubResult is the peek response. Truncated is true when the N cap was
// hit before the deadline.
type PubSubResult struct {
	Messages  []PubSubMessage `json:"messages"`
	Truncated bool            `json:"truncated"`
	FetchMs   int             `json:"fetchMs"`
}
