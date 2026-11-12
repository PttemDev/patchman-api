// Package kafkaquery is the BE-side adapter for the user-facing "Kafka"
// tool. It mirrors internal/dbquery: a Registry pools live clients per
// connection, decrypting the SASL secret just-in-time, and exposes admin
// (topics/configs/groups), a strictly non-committing message peek, and a
// guarded produce path.
//
// The peek path NEVER joins a consumer group and NEVER commits offsets —
// it assigns partitions directly at explicit offsets on a throwaway
// client, so browsing a production topic leaves no trace in
// __consumer_offsets. See KAFKA_S3_AND_DBQUERY_FIXES_RFC.md §A.6.
package kafkaquery

// ConnectionConfig is the resolved (decrypted) config handed to the
// client builder. Built from models.KafkaConnection + a TokenCipher
// decrypt so the builder never sees ciphertext.
type ConnectionConfig struct {
	BootstrapServers string
	SecurityProtocol string // PLAINTEXT | SSL | SASL_PLAINTEXT | SASL_SSL
	SASLMechanism    string // PLAIN | SCRAM-SHA-256 | SCRAM-SHA-512
	SASLUsername     string
	SASLPassword     string
	// Parsed options_json.
	//   caCert              string (PEM) — custom CA for TLS
	//   insecureSkipVerify  bool         — accept self-signed (on-prem)
	//   clientId            string
	Options map[string]any
}

// TopicSummary is one row in the topic list.
type TopicSummary struct {
	Name              string `json:"name"`
	Partitions        int    `json:"partitions"`
	ReplicationFactor int    `json:"replicationFactor"`
	Internal          bool   `json:"internal"`
}

// TopicOverview is one row in the Topics tab list — a superset of
// TopicSummary. It adds an APPROXIMATE message count (the end−start offset
// span summed over partitions; over-counts on compacted topics and with
// transaction markers, hence "≈") plus health flags. MessageCount is -1
// when watermarks could not be listed (e.g. missing DESCRIBE on the topic).
// It costs two extra bulk offset RPCs over ListTopics, so the plain topic
// dropdown keeps using ListTopics; only the Topics tab asks for this.
type TopicOverview struct {
	Name              string `json:"name"`
	Partitions        int    `json:"partitions"`
	ReplicationFactor int    `json:"replicationFactor"`
	Internal          bool   `json:"internal"`
	MessageCount      int64  `json:"messageCount"`    // -1 when unknown
	UnderReplicated   bool   `json:"underReplicated"` // any partition with |ISR| < |Replicas|
	Offline           bool   `json:"offline"`         // any partition without a leader / load error
	// SizeBytes is the summed size of each partition's LEADER replica (≈ the
	// topic's data size); ReplicaSizeBytes adds every replica (disk footprint).
	// Both -1 when log dirs couldn't be described (needs DESCRIBE on CLUSTER)
	// or a partition's leader didn't report.
	SizeBytes        int64 `json:"sizeBytes"`
	ReplicaSizeBytes int64 `json:"replicaSizeBytes"`
}

// TopicPartitionDetail is one partition's placement + watermarks for the
// Topics-tab detail view. Low/High/Count are -1 when the watermarks could
// not be listed for that partition.
type TopicPartitionDetail struct {
	Partition int32   `json:"partition"`
	Leader    int32   `json:"leader"`
	Replicas  []int32 `json:"replicas"`
	ISR       []int32 `json:"isr"`
	Offline   []int32 `json:"offlineReplicas,omitempty"`
	Low       int64   `json:"low"`   // start (oldest) offset, -1 unknown
	High      int64   `json:"high"`  // end (high watermark) offset, -1 unknown
	Count     int64   `json:"count"` // high-low, -1 unknown
	// SizeBytes: the leader replica's log size; ReplicaSizeBytes: summed over
	// all replicas. -1 when unknown.
	SizeBytes        int64  `json:"sizeBytes"`
	ReplicaSizeBytes int64  `json:"replicaSizeBytes"`
	Err              string `json:"err,omitempty"`
}

// TopicDetail is the expanded per-topic view: partition placement +
// watermarks, health flags, and the full config list (the UI highlights the
// interesting keys and hides the rest behind a toggle).
type TopicDetail struct {
	Name              string                 `json:"name"`
	Partitions        int                    `json:"partitions"`
	ReplicationFactor int                    `json:"replicationFactor"`
	Internal          bool                   `json:"internal"`
	MessageCount      int64                  `json:"messageCount"` // -1 when unknown
	UnderReplicated   bool                   `json:"underReplicated"`
	Offline           bool                   `json:"offline"`
	SizeBytes         int64                  `json:"sizeBytes"`        // Σ leader sizes, -1 unknown
	ReplicaSizeBytes  int64                  `json:"replicaSizeBytes"` // Σ all replicas, -1 unknown
	PartitionDetail   []TopicPartitionDetail `json:"partitionDetail"`
	Config            []ConfigEntry          `json:"config"`
}

// ConfigEntry is a single topic config key, with an `editable` flag the
// UI uses to gate the field (we only expose the alterable subset).
type ConfigEntry struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Source    string `json:"source"`
	Sensitive bool   `json:"sensitive"`
	Editable  bool   `json:"editable"`
}

// BrokerInfo is used by the create-topic wizard to validate RF/min-ISR
// against the actual cluster size.
type BrokerInfo struct {
	NodeID int32  `json:"nodeId"`
	Host   string `json:"host"`
	Port   int32  `json:"port"`
	Rack   string `json:"rack,omitempty"`
}

// MessageHeader is one Kafka record header (value shown as UTF-8/best-effort).
type MessageHeader struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Message is one peeked record. Value/Key are UTF-8 text when valid,
// otherwise base64 (encoding flagged) so the payload survives JSON
// transport without corruption.
type Message struct {
	Partition     int32           `json:"partition"`
	Offset        int64           `json:"offset"`
	TimestampMs   int64           `json:"timestampMs"`
	Key           *string         `json:"key"`
	Value         string          `json:"value"`
	ValueEncoding string          `json:"valueEncoding"` // "utf8" | "base64"
	Headers       []MessageHeader `json:"headers,omitempty"`
}

// ConsumeRequest describes a bounded, non-committing peek.
type ConsumeRequest struct {
	Topic string `json:"topic"`
	// Mode ∈ first | last | offset | timestamp. Default "last".
	Mode string `json:"mode"`
	// N is the total message cap across the selected partitions.
	N int `json:"n"`
	// Partition: -1 (or nil) = all partitions; otherwise a single one.
	Partition   int32 `json:"partition"`
	Offset      int64 `json:"offset"`      // for mode=offset
	TimestampMs int64 `json:"timestampMs"` // for mode=timestamp
}

// ConsumeResult is the peek response.
type ConsumeResult struct {
	Messages   []Message `json:"messages"`
	Partitions int       `json:"partitions"`
	Truncated  bool      `json:"truncated"`
	// TimedOut: the peek budget ran out before the target count arrived;
	// Messages holds what did (possibly none).
	TimedOut bool `json:"timedOut"`
	FetchMs  int  `json:"fetchMs"`
}

// ProduceRequest publishes a single record. Partition -1 = let the
// client partition (by key / round-robin).
type ProduceRequest struct {
	Topic       string          `json:"topic"`
	Partition   int32           `json:"partition"`
	Key         string          `json:"key"`
	Value       string          `json:"value"`
	Headers     []MessageHeader `json:"headers,omitempty"`
	KeyBase64   bool            `json:"keyBase64,omitempty"`
	ValueBase64 bool            `json:"valueBase64,omitempty"`
}

// GroupSummary is one row in the consumer-group list.
type GroupSummary struct {
	Group        string `json:"group"`
	State        string `json:"state"`
	ProtocolType string `json:"protocolType"`
	Members      int    `json:"members"`
}

// GroupMemberOffset is one topic/partition committed offset + lag for a
// group (read-only inspection; the tool never resets offsets in v1).
type GroupMemberOffset struct {
	Topic     string `json:"topic"`
	Partition int32  `json:"partition"`
	Committed int64  `json:"committed"`
	End       int64  `json:"end"`
	Lag       int64  `json:"lag"`
}

// GroupDetail is the describe response for a single group.
type GroupDetail struct {
	Group        string              `json:"group"`
	State        string              `json:"state"`
	ProtocolType string              `json:"protocolType"`
	Protocol     string              `json:"protocol"`
	Members      int                 `json:"members"`
	Offsets      []GroupMemberOffset `json:"offsets"`
}

// GroupLagSummary is one row in the cluster-wide consumer-group lag list
// (the lag dashboard). TotalLag is -1 when the group could not be
// described/fetched (Error is then set).
type GroupLagSummary struct {
	Group        string `json:"group"`
	State        string `json:"state"`
	ProtocolType string `json:"protocolType"`
	Members      int    `json:"members"`
	TotalLag     int64  `json:"totalLag"` // -1 when unknown/errored
	Coordinator  int32  `json:"coordinator"`
	Error        string `json:"error,omitempty"`
}

// GroupMemberInfo is one live member of a group with the partitions the
// leader assigned to it.
type GroupMemberInfo struct {
	MemberID   string       `json:"memberId"`
	ClientID   string       `json:"clientId"`
	ClientHost string       `json:"clientHost"`
	InstanceID string       `json:"instanceId,omitempty"`
	Assigned   []PartAssign `json:"assigned"`
}

// PartAssign is a member's assignment for one topic.
type PartAssign struct {
	Topic      string  `json:"topic"`
	Partitions []int32 `json:"partitions"`
}

// PartitionLag is per-topic/partition lag for a group. Lag is -1 when it
// could not be computed (commit or list-offset error; Err is then set).
type PartitionLag struct {
	Topic     string `json:"topic"`
	Partition int32  `json:"partition"`
	Committed int64  `json:"committed"`
	End       int64  `json:"end"`
	Lag       int64  `json:"lag"` // -1 when unknown
	MemberID  string `json:"memberId,omitempty"`
	ClientID  string `json:"clientId,omitempty"`
	Err       string `json:"err,omitempty"`
}

// GroupLagDetail is the full lag view for a single group (members +
// per-partition lag). TotalLag is -1 when the group itself errored.
type GroupLagDetail struct {
	Group        string            `json:"group"`
	State        string            `json:"state"`
	ProtocolType string            `json:"protocolType"`
	Coordinator  int32             `json:"coordinator"`
	TotalLag     int64             `json:"totalLag"`
	MemberCount  int               `json:"memberCount"`
	Error        string            `json:"error,omitempty"`
	Members      []GroupMemberInfo `json:"members"`
	Partitions   []PartitionLag    `json:"partitions"`
}

// AllowedTopicConfigs is the alterable subset the UI is permitted to edit.
// Everything else is shown read-only. These are the keys that Kafka
// accepts via AlterConfigs at the topic level and that are meaningful to
// tune from a browser tool.
var AllowedTopicConfigs = map[string]bool{
	"retention.ms":           true, // time retention
	"retention.bytes":        true, // size retention (per partition)
	"min.insync.replicas":    true,
	"cleanup.policy":         true, // delete | compact | delete,compact
	"max.message.bytes":      true,
	"segment.ms":             true,
	"segment.bytes":          true,
	"min.compaction.lag.ms":  true,
	"delete.retention.ms":    true,
	"compression.type":       true,
	"message.timestamp.type": true,
	"flush.messages":         true,
	"max.compaction.lag.ms":  true,
}
