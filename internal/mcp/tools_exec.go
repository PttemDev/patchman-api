package mcp

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file exposes Patchman's read-first CONNECTION/EXECUTION surface to MCP
// clients. Every tool is a thin closure over the in-process HTTP caller, so
// the existing JWTAuth + grant checks (loadAccessible) + write-gates + audit +
// blocklist/intent guards apply unchanged — nothing here re-implements access.
//
// Connections are addressed by id (discover them with the list_*_connections
// tools) and resolved under the caller's grants. Execution tools use callCtx so
// client cancellation reaches the upstream op. Only es_request and
// redis_command can write, and only when allowWrite=true AND the caller holds a
// write grant; everything else is read-only.

// connNote is appended to connection-scoped tool descriptions: it states the
// id/grant resolution model and the hosted-backend reachability caveat.
// connNote / gitNote used to be appended to every connection / git tool's
// description (~13 KB of the tool list). Their text now goes once into the
// server instructions (connectionRules / gitRules, see instructions.go).
const connNote = ""

// s3PreviewCap bounds the object body s3_get_object returns. We fetch one byte
// past the cap so an overflow is detectable, then truncate + annotate.
const s3PreviewCap = 64 * 1024

// withQuery appends an encoded query string when any params are present.
func withQuery(path string, v url.Values) string {
	if len(v) == 0 {
		return path
	}
	return path + "?" + v.Encode()
}

// ---- Discovery: connection lists (light reads → plain reg) ------------------

func registerConnectionDiscoveryTools(s *mcpsdk.Server, c *caller) {
	list := func(name, path, kind string) {
		reg(s, name, "List the "+kind+" connections you can access (owner + granted). Read-only; returns ids to use with the other tools."+connNote,
			func(_ struct{}) (int, string) { return c.call("GET", path, nil) })
	}
	// Git connections from this computer and (signed in) the PatchMan server,
	// merged, each tagged runsOn local|cloud — see gitroute.go.
	reg(s, "list_git_connections", "List the Git connections you can access (owner + granted). Read-only; returns ids to use with the other tools."+connNote,
		func(_ struct{}) (int, string) { return c.listGitConnections(context.Background()) })
	list("list_db_connections", "/api/db-connections", "database")
	list("list_es_connections", "/api/es-connections", "Elasticsearch")
	list("list_redis_connections", "/api/redis-connections", "Redis")
	list("list_kafka_connections", "/api/kafka-connections", "Kafka")
	list("list_s3_connections", "/api/s3-connections", "S3")
	list("list_k8s_connections", "/api/k8s-connections", "Kubernetes")
	list("list_grafana_connections", "/api/grafana-connections", "Grafana")
	list("list_rabbitmq_connections", "/api/rabbit-connections", "RabbitMQ")
	list("list_graylog_connections", "/api/graylog-connections", "Graylog")
	list("list_observability_connections", "/api/observability-connections", "observability (APM: Coroot, Prometheus, Jaeger, Kiali, SigNoz, Datadog)")
}

// ---- HTTP proxy send --------------------------------------------------------

type httpBodyIn struct {
	Type    string `json:"type,omitempty" jsonschema:"body kind: json/form/xml/raw/binary/graphql/soap/none"`
	Content string `json:"content,omitempty" jsonschema:"raw body content as a string"`
}
type httpAuthIn struct {
	Type     string `json:"type,omitempty" jsonschema:"none/bearer/basic/apikey"`
	Token    string `json:"token,omitempty" jsonschema:"bearer token (concrete value)"`
	Username string `json:"username,omitempty" jsonschema:"basic auth username"`
	Password string `json:"password,omitempty" jsonschema:"basic auth password"`
	Key      string `json:"key,omitempty" jsonschema:"apikey name"`
	Value    string `json:"value,omitempty" jsonschema:"apikey value"`
	AddTo    string `json:"addTo,omitempty" jsonschema:"apikey placement: header or query"`
}
type sendHTTPRequestIn struct {
	Method          string            `json:"method,omitempty" jsonschema:"HTTP method (default GET)"`
	URL             string            `json:"url" jsonschema:"fully-qualified request URL"`
	Headers         map[string]string `json:"headers,omitempty" jsonschema:"request headers"`
	Params          map[string]string `json:"params,omitempty" jsonschema:"query params merged into the URL"`
	Body            *httpBodyIn       `json:"body,omitempty" jsonschema:"request body {type, content}"`
	Auth            *httpAuthIn       `json:"auth,omitempty" jsonschema:"auth {type,...}; pass concrete values"`
	Timeout         int               `json:"timeout,omitempty" jsonschema:"timeout in seconds"`
	FollowRedirects bool              `json:"followRedirects,omitempty" jsonschema:"follow HTTP redirects"`
	VerifySSL       bool              `json:"verifySsl,omitempty" jsonschema:"verify TLS certificates"`
}

func registerHTTPExecTools(s *mcpsdk.Server, c *caller) {
	regCtx(s, "send_http_request",
		"Send an ad-hoc HTTP request through Patchman's proxy and return the response. Read-or-write depends entirely on the URL/method you supply. Values are used verbatim: environment variables, {{placeholders}} and stored secrets are NOT resolved server-side, so pass concrete headers/auth. There is no run-saved-request endpoint; build the request inline.",
		func(ctx context.Context, in sendHTTPRequestIn) (int, string) {
			return c.callCtx(ctx, "POST", "/api/proxy/send", in)
		})
}

// ---- DB query execute -------------------------------------------------------

type runDBQueryIn struct {
	ConnectionID string `json:"connectionId" jsonschema:"target DB connection id (see list_db_connections)"`
	Query        string `json:"query" jsonschema:"the query to run: SQL, or Mongo shell syntax like db.coll.find({filter}, {projection})"`
	Limit        *int   `json:"limit,omitempty" jsonschema:"max rows (default 500; 0 = uncapped, owner only)"`
	Offset       int    `json:"offset,omitempty" jsonschema:"row offset for pagination"`
	AllowWrite   bool   `json:"allowWrite,omitempty" jsonschema:"set true to run a write (SQL INSERT/UPDATE/DELETE; Mongo insertOne/insertMany/updateOne/updateMany/replaceOne/deleteOne/deleteMany). Also needs read-write enabled on the connection and owner/co-owner access (default false)"`
	ConfirmCost  bool   `json:"confirmCost,omitempty" jsonschema:"BigQuery only: set true ONLY after the user agreed to the scan size a previous call reported (costConfirm). Never set it on your own"`
}

func registerDBExecTools(s *mcpsdk.Server, c *caller) {
	regCtx(s, "run_db_query",
		"Run a query against a saved DB connection (Postgres/MySQL/SQL Server/Mongo/BigQuery). Read-only by default; a write runs only with allowWrite=true on a connection whose owner enabled read-write (list_db_connections shows allowWrite), and only for its owner or a co-owner. Mongo: updateMany/deleteMany refuse an empty filter. BigQuery: every query is dry-run first; one over the user's scan limit is refused, one over their ask threshold returns costConfirm with the size."+connNote,
		func(ctx context.Context, in runDBQueryIn) (int, string) {
			return c.callCtx(ctx, "POST", "/api/db-query/execute", in)
		})
}

// ---- Elasticsearch ----------------------------------------------------------

type esIDIn struct {
	ID string `json:"id" jsonschema:"Elasticsearch connection id"`
}
type esMappingIn struct {
	ID    string `json:"id" jsonschema:"Elasticsearch connection id"`
	Index string `json:"index,omitempty" jsonschema:"index name or pattern; empty = all indices"`
}
type esDataViewsIn struct {
	ID         string `json:"id" jsonschema:"Elasticsearch connection id"`
	DataViewID string `json:"dataViewId,omitempty" jsonschema:"a data view id for its details; empty = list all"`
	Space      string `json:"space,omitempty" jsonschema:"Kibana space id (default = the connection's space)"`
}
type esDataViewSearchIn struct {
	ID          string `json:"id" jsonschema:"Elasticsearch connection id"`
	DataViewID  string `json:"dataViewId" jsonschema:"data view id (see es_data_views)"`
	Space       string `json:"space,omitempty" jsonschema:"Kibana space id (default = the connection's space)"`
	From        string `json:"from,omitempty" jsonschema:"range start: ISO-8601 or date math like now-1h (default now-15m)"`
	To          string `json:"to,omitempty" jsonschema:"range end (default now)"`
	Query       string `json:"query,omitempty" jsonschema:"Lucene query_string, e.g. level:error AND service:api"`
	Size        int    `json:"size,omitempty" jsonschema:"hits per page (default 50, max 500)"`
	Histogram   bool   `json:"histogram,omitempty" jsonschema:"also return a time histogram of matching docs"`
	PitID       string `json:"pitId,omitempty" jsonschema:"pitId from the previous page (paging)"`
	SearchAfter []any  `json:"searchAfter,omitempty" jsonschema:"the last hit's sort array from the previous page (paging)"`
}
type esRequestIn struct {
	ID         string `json:"id" jsonschema:"Elasticsearch connection id"`
	Method     string `json:"method,omitempty" jsonschema:"HTTP method (default GET)"`
	Path       string `json:"path" jsonschema:"ES API path, e.g. /myindex/_search"`
	Body       any    `json:"body,omitempty" jsonschema:"request body: a JSON value (query DSL) or a string (NDJSON for _bulk)"`
	AllowWrite bool   `json:"allowWrite,omitempty" jsonschema:"allow a mutating request; still requires a write grant (default false)"`
}

func registerESTools(s *mcpsdk.Server, c *caller) {
	regCtx(s, "es_cluster", "Get Elasticsearch cluster health. Read-only."+connNote,
		func(ctx context.Context, in esIDIn) (int, string) {
			return c.callCtx(ctx, "GET", "/api/es/"+url.PathEscape(in.ID)+"/cluster", nil)
		})
	regCtx(s, "es_indices", "List Elasticsearch indices. Read-only."+connNote,
		func(ctx context.Context, in esIDIn) (int, string) {
			return c.callCtx(ctx, "GET", "/api/es/"+url.PathEscape(in.ID)+"/indices", nil)
		})
	regCtx(s, "es_mapping", "Get field mappings for an index (or all). Read-only."+connNote,
		func(ctx context.Context, in esMappingIn) (int, string) {
			v := url.Values{}
			if in.Index != "" {
				v.Set("index", in.Index)
			}
			return c.callCtx(ctx, "GET", withQuery("/api/es/"+url.PathEscape(in.ID)+"/mapping", v), nil)
		})
	regCtx(s, "es_request",
		"Send a raw Elasticsearch request. Read-only by default; set allowWrite=true for a mutating request, which still requires a write grant."+connNote,
		func(ctx context.Context, in esRequestIn) (int, string) {
			return c.callCtx(ctx, "POST", "/api/es/"+url.PathEscape(in.ID)+"/request", in)
		})
	regCtx(s, "es_data_views",
		"List Kibana data views (index patterns on Kibana 7.x / OpenSearch Dashboards) of an Elasticsearch connection — each has an index pattern and a time field. Pass dataViewId for one data view's details (time field + runtime fields). Needs a Kibana URL on the connection (or the 'via Kibana' mode). Read-only."+connNote,
		func(ctx context.Context, in esDataViewsIn) (int, string) {
			v := url.Values{}
			if in.Space != "" {
				v.Set("space", in.Space)
			}
			p := "/api/es/" + url.PathEscape(in.ID) + "/data-views"
			if in.DataViewID != "" {
				p += "/" + url.PathEscape(in.DataViewID)
			}
			return c.callCtx(ctx, "GET", withQuery(p, v), nil)
		})
	regCtx(s, "es_data_view_search",
		"Search logs/documents through a Kibana data view, like Kibana Discover: the data view's time field filters by from/to (ISO or ES date math, default now-15m..now) and sorts newest first; query is Lucene query_string syntax (empty = all). Returns hits, total and (histogram=true) a time histogram. For the next page pass the previous pitId and the last hit's sort array as searchAfter. Read-only."+connNote,
		func(ctx context.Context, in esDataViewSearchIn) (int, string) {
			body := map[string]any{
				"space": in.Space, "from": in.From, "to": in.To, "query": in.Query,
				"size": in.Size, "histogram": in.Histogram,
			}
			if in.PitID != "" {
				body["pitId"] = in.PitID
			}
			if len(in.SearchAfter) > 0 {
				body["searchAfter"] = in.SearchAfter
			}
			if in.Size == 0 {
				body["size"] = 50
			}
			return c.callCtx(ctx, "POST", "/api/es/"+url.PathEscape(in.ID)+"/data-views/"+url.PathEscape(in.DataViewID)+"/search", body)
		})
}

// ---- Redis ------------------------------------------------------------------

type redisScanIn struct {
	ID      string `json:"id" jsonschema:"Redis connection id"`
	Pattern string `json:"pattern,omitempty" jsonschema:"key match pattern, e.g. user:* (default *)"`
	Cursor  uint64 `json:"cursor,omitempty" jsonschema:"scan cursor from a previous page (0 to start)"`
	Count   int    `json:"count,omitempty" jsonschema:"max matching keys to return (default 100, max 2000); the server keeps SCANning until that many match or a ~1.5s budget runs out"`
	DB      *int   `json:"db,omitempty" jsonschema:"redis db index (default the connection's db)"`
}
type redisKeyIn struct {
	ID  string `json:"id" jsonschema:"Redis connection id"`
	Key string `json:"key" jsonschema:"the key to read"`
	DB  *int   `json:"db,omitempty" jsonschema:"redis db index (default the connection's db)"`
}
type redisCommandIn struct {
	ID         string   `json:"id" jsonschema:"Redis connection id"`
	Args       []string `json:"args" jsonschema:"command + arguments, e.g. [\"GET\",\"mykey\"]"`
	DB         int      `json:"db,omitempty" jsonschema:"redis db index"`
	AllowWrite bool     `json:"allowWrite,omitempty" jsonschema:"allow a mutating command; still requires a write grant (default false)"`
}

func registerRedisTools(s *mcpsdk.Server, c *caller) {
	regCtx(s, "redis_scan_keys", "Scan Redis keys by glob pattern (cursor-paginated: when complete=false, call again with the returned cursor; stopReason says why it paused). Read-only."+connNote,
		func(ctx context.Context, in redisScanIn) (int, string) {
			v := url.Values{}
			if in.Pattern != "" {
				v.Set("match", in.Pattern)
			}
			if in.Cursor != 0 {
				v.Set("cursor", strconv.FormatUint(in.Cursor, 10))
			}
			if in.Count != 0 {
				v.Set("count", strconv.Itoa(in.Count))
			}
			if in.DB != nil {
				v.Set("db", strconv.Itoa(*in.DB))
			}
			return c.callCtx(ctx, "GET", withQuery("/api/redis/"+url.PathEscape(in.ID)+"/keys", v), nil)
		})
	regCtx(s, "redis_get", "Get the value of a Redis key. Read-only."+connNote,
		func(ctx context.Context, in redisKeyIn) (int, string) {
			return c.callCtx(ctx, "GET", withQuery("/api/redis/"+url.PathEscape(in.ID)+"/key", redisKeyQuery(in)), nil)
		})
	regCtx(s, "redis_key_meta", "Get metadata (type, TTL, size) for a Redis key. Read-only."+connNote,
		func(ctx context.Context, in redisKeyIn) (int, string) {
			return c.callCtx(ctx, "GET", withQuery("/api/redis/"+url.PathEscape(in.ID)+"/key/meta", redisKeyQuery(in)), nil)
		})
	regCtx(s, "redis_command",
		"Run a single Redis command. Read-only by default; set allowWrite=true for a mutating command, which also needs read-write enabled on the connection (list_redis_connections shows allowWrite) and a write grant. Dangerous commands (e.g. FLUSHALL, KEYS, CONFIG) are hard-blocked server-side regardless of grant."+connNote,
		func(ctx context.Context, in redisCommandIn) (int, string) {
			return c.callCtx(ctx, "POST", "/api/redis/"+url.PathEscape(in.ID)+"/command", in)
		})
}

func redisKeyQuery(in redisKeyIn) url.Values {
	v := url.Values{}
	if in.Key != "" {
		v.Set("key", in.Key)
	}
	if in.DB != nil {
		v.Set("db", strconv.Itoa(*in.DB))
	}
	return v
}

// ---- Kafka (read-only) ------------------------------------------------------

type kafkaIDIn struct {
	ID string `json:"id" jsonschema:"Kafka connection id"`
}
type kafkaLagIn struct {
	ID    string `json:"id" jsonschema:"Kafka connection id"`
	Topic string `json:"topic,omitempty" jsonschema:"optional: only groups with lag on this topic"`
}
type kafkaConsumeIn struct {
	ID          string `json:"id" jsonschema:"Kafka connection id"`
	Topic       string `json:"topic" jsonschema:"topic to peek"`
	Mode        string `json:"mode,omitempty" jsonschema:"first|last|offset|timestamp (default last)"`
	N           int    `json:"n,omitempty" jsonschema:"max messages across partitions (default 100, max 5000)"`
	Partition   *int32 `json:"partition,omitempty" jsonschema:"specific partition; omit or -1 = all partitions"`
	Offset      int64  `json:"offset,omitempty" jsonschema:"start offset for mode=offset"`
	TimestampMs int64  `json:"timestampMs,omitempty" jsonschema:"start time (ms since epoch) for mode=timestamp"`
}

func registerKafkaTools(s *mcpsdk.Server, c *caller) {
	regCtx(s, "kafka_topics", "List Kafka topics. Read-only."+connNote,
		func(ctx context.Context, in kafkaIDIn) (int, string) {
			return c.callCtx(ctx, "GET", "/api/kafka/"+url.PathEscape(in.ID)+"/topics", nil)
		})
	regCtx(s, "kafka_brokers", "List Kafka brokers. Read-only."+connNote,
		func(ctx context.Context, in kafkaIDIn) (int, string) {
			return c.callCtx(ctx, "GET", "/api/kafka/"+url.PathEscape(in.ID)+"/brokers", nil)
		})
	regCtx(s, "kafka_groups", "List Kafka consumer groups. Read-only."+connNote,
		func(ctx context.Context, in kafkaIDIn) (int, string) {
			return c.callCtx(ctx, "GET", "/api/kafka/"+url.PathEscape(in.ID)+"/groups", nil)
		})
	regCtx(s, "kafka_lag", "Get consumer-group lag summary (optionally filtered by topic). Read-only."+connNote,
		func(ctx context.Context, in kafkaLagIn) (int, string) {
			v := url.Values{}
			if in.Topic != "" {
				v.Set("topic", in.Topic)
			}
			return c.callCtx(ctx, "GET", withQuery("/api/kafka/"+url.PathEscape(in.ID)+"/lag", v), nil)
		})
	regCtx(s, "kafka_consume",
		"Peek messages from a Kafka topic. Read-only: it joins no consumer group and commits no offset."+connNote,
		func(ctx context.Context, in kafkaConsumeIn) (int, string) {
			// Partition defaults to -1 (all) to match the UI: an omitted 0
			// would otherwise read only partition 0.
			part := int32(-1)
			if in.Partition != nil {
				part = *in.Partition
			}
			body := map[string]any{"topic": in.Topic, "partition": part}
			if in.Mode != "" {
				body["mode"] = in.Mode
			}
			if in.N != 0 {
				body["n"] = in.N
			}
			if in.Offset != 0 {
				body["offset"] = in.Offset
			}
			if in.TimestampMs != 0 {
				body["timestampMs"] = in.TimestampMs
			}
			return c.callCtx(ctx, "POST", "/api/kafka/"+url.PathEscape(in.ID)+"/consume", body)
		})
}

// ---- S3 (read-only) ---------------------------------------------------------

type s3IDIn struct {
	ID string `json:"id" jsonschema:"S3 connection id"`
}
type s3ListObjectsIn struct {
	ID     string `json:"id" jsonschema:"S3 connection id"`
	Bucket string `json:"bucket" jsonschema:"bucket name"`
	Prefix string `json:"prefix,omitempty" jsonschema:"key prefix filter"`
	Token  string `json:"token,omitempty" jsonschema:"continuation token from a previous page"`
	Max    int    `json:"max,omitempty" jsonschema:"max keys per page (default 500)"`
}
type s3GetObjectIn struct {
	ID     string `json:"id" jsonschema:"S3 connection id"`
	Bucket string `json:"bucket" jsonschema:"bucket name"`
	Key    string `json:"key" jsonschema:"object key"`
}

func registerS3Tools(s *mcpsdk.Server, c *caller) {
	regCtx(s, "s3_list_buckets", "List S3 buckets. Read-only."+connNote,
		func(ctx context.Context, in s3IDIn) (int, string) {
			return c.callCtx(ctx, "GET", "/api/s3/"+url.PathEscape(in.ID)+"/buckets", nil)
		})
	regCtx(s, "s3_list_objects", "List objects under a bucket/prefix (paginated). Read-only."+connNote,
		func(ctx context.Context, in s3ListObjectsIn) (int, string) {
			v := url.Values{}
			if in.Bucket != "" {
				v.Set("bucket", in.Bucket)
			}
			if in.Prefix != "" {
				v.Set("prefix", in.Prefix)
			}
			if in.Token != "" {
				v.Set("token", in.Token)
			}
			if in.Max != 0 {
				v.Set("max", strconv.Itoa(in.Max))
			}
			return c.callCtx(ctx, "GET", withQuery("/api/s3/"+url.PathEscape(in.ID)+"/objects", v), nil)
		})
	regCtx(s, "s3_get_object",
		"Fetch the start of an S3 object's body (capped at ~64KB; larger/binary objects are truncated with a marker). Read-only."+connNote,
		func(ctx context.Context, in s3GetObjectIn) (int, string) {
			base := "/api/s3/" + url.PathEscape(in.ID) + "/object"
			v := url.Values{}
			if in.Bucket != "" {
				v.Set("bucket", in.Bucket)
			}
			if in.Key != "" {
				v.Set("key", in.Key)
			}
			// Fetch one byte past the cap so truncation is detectable, and
			// bound memory instead of buffering a multi-GB object.
			ranged := url.Values{}
			for k, val := range v {
				ranged[k] = val
			}
			ranged.Set("range", fmt.Sprintf("bytes=0-%d", s3PreviewCap))
			code, body := c.callCtx(ctx, "GET", withQuery(base, ranged), nil)
			if code >= 400 {
				// Zero-byte objects (and stores that reject a Range past EOF)
				// fail the ranged GET; retry once without the Range.
				code, body = c.callCtx(ctx, "GET", withQuery(base, v), nil)
			}
			if code < 400 && len(body) > s3PreviewCap {
				body = body[:s3PreviewCap] + "\n…(truncated)"
			}
			return code, body
		})
}

// ---- Kubernetes (read-only, non-streaming) ----------------------------------

type k8sIDIn struct {
	ID string `json:"id" jsonschema:"Kubernetes connection id"`
}
type k8sOverviewIn struct {
	ID      string `json:"id" jsonschema:"Kubernetes connection id"`
	Context string `json:"context,omitempty" jsonschema:"kubeconfig context (default current)"`
}
type k8sListResourcesIn struct {
	ID        string `json:"id" jsonschema:"Kubernetes connection id"`
	Kind      string `json:"kind" jsonschema:"resource kind, e.g. Pod, Deployment, Service"`
	Context   string `json:"context,omitempty" jsonschema:"kubeconfig context (default current)"`
	Namespace string `json:"namespace,omitempty" jsonschema:"namespace filter; empty = all/default"`
}
type k8sGetResourceIn struct {
	ID        string `json:"id" jsonschema:"Kubernetes connection id"`
	Kind      string `json:"kind" jsonschema:"resource kind"`
	Name      string `json:"name" jsonschema:"resource name"`
	Context   string `json:"context,omitempty" jsonschema:"kubeconfig context"`
	Namespace string `json:"namespace,omitempty" jsonschema:"namespace"`
}

func registerK8sTools(s *mcpsdk.Server, c *caller) {
	regCtx(s, "k8s_contexts", "List the kubeconfig contexts for a K8s connection (no cluster call). Read-only."+connNote,
		func(ctx context.Context, in k8sIDIn) (int, string) {
			return c.callCtx(ctx, "GET", "/api/k8s/"+url.PathEscape(in.ID)+"/contexts", nil)
		})
	regCtx(s, "k8s_overview", "Get the cluster overview (nodes/pods/workloads/namespaces/events). Read-only."+connNote,
		func(ctx context.Context, in k8sOverviewIn) (int, string) {
			v := url.Values{}
			if in.Context != "" {
				v.Set("context", in.Context)
			}
			return c.callCtx(ctx, "GET", withQuery("/api/k8s/"+url.PathEscape(in.ID)+"/overview", v), nil)
		})
	regCtx(s, "k8s_list_resources", "List K8s resources of a kind (optionally scoped to a namespace). Read-only."+connNote,
		func(ctx context.Context, in k8sListResourcesIn) (int, string) {
			v := url.Values{}
			if in.Kind != "" {
				v.Set("kind", in.Kind)
			}
			if in.Context != "" {
				v.Set("context", in.Context)
			}
			if in.Namespace != "" {
				v.Set("namespace", in.Namespace)
			}
			return c.callCtx(ctx, "GET", withQuery("/api/k8s/"+url.PathEscape(in.ID)+"/resources", v), nil)
		})
	regCtx(s, "k8s_get_resource", "Get a single K8s resource as JSON + YAML. Read-only."+connNote,
		func(ctx context.Context, in k8sGetResourceIn) (int, string) {
			v := url.Values{}
			if in.Kind != "" {
				v.Set("kind", in.Kind)
			}
			if in.Name != "" {
				v.Set("name", in.Name)
			}
			if in.Context != "" {
				v.Set("context", in.Context)
			}
			if in.Namespace != "" {
				v.Set("namespace", in.Namespace)
			}
			return c.callCtx(ctx, "GET", withQuery("/api/k8s/"+url.PathEscape(in.ID)+"/resource", v), nil)
		})
}

// ---- Dev tools --------------------------------------------------------------

type dnsLookupIn struct {
	Name   string   `json:"name" jsonschema:"hostname to resolve (or an IP for PTR)"`
	Types  []string `json:"types,omitempty" jsonschema:"record types: A/AAAA/MX/TXT/NS/CNAME/SRV/PTR (default A+AAAA)"`
	Server string   `json:"server,omitempty" jsonschema:"custom resolver ip or ip:port (default system resolver)"`
}

func registerDevToolsTools(s *mcpsdk.Server, c *caller) {
	regCtx(s, "dns_lookup", "Resolve DNS records for a name. Read-only; runs from the MCP backend's network vantage point.",
		func(ctx context.Context, in dnsLookupIn) (int, string) {
			return c.callCtx(ctx, "POST", "/api/devtools/dns", in)
		})
}

// ---- Git (code + merge-request review) --------------------------------------

// gitNote states the git-specific auth model appended to every git tool: the
// call authenticates as the CALLER's own linked GitLab account (not the
// connection owner's), and the write tools additionally need a write grant.
const gitNote = ""

type gitReposIn struct {
	ConnectionID string `json:"connectionId" jsonschema:"target git connection id (see list_git_connections)"`
	Search       string `json:"search,omitempty" jsonschema:"filter projects by name (optional)"`
}

type gitFileIn struct {
	ConnectionID string `json:"connectionId" jsonschema:"target git connection id (see list_git_connections)"`
	Project      string `json:"project" jsonschema:"GitLab project (numeric id or 'group/repo') or GitHub repository (numeric id or 'owner/name')"`
	Path         string `json:"path" jsonschema:"file path within the repo"`
	Ref          string `json:"ref,omitempty" jsonschema:"branch/tag/commit sha (default: the repo's default branch)"`
}

type gitMRListIn struct {
	ConnectionID string `json:"connectionId" jsonschema:"target git connection id (see list_git_connections)"`
	Project      string `json:"project" jsonschema:"GitLab project (numeric id or 'group/repo') or GitHub repository (numeric id or 'owner/name')"`
	State        string `json:"state,omitempty" jsonschema:"filter: opened/closed/merged/all (default all)"`
}

type gitMRIn struct {
	ConnectionID string `json:"connectionId" jsonschema:"target git connection id (see list_git_connections)"`
	Project      string `json:"project" jsonschema:"GitLab project (numeric id or 'group/repo') or GitHub repository (numeric id or 'owner/name')"`
	IID          int64  `json:"iid" jsonschema:"the merge request IID (the per-project number shown as !N)"`
}

type gitMRNoteIn struct {
	ConnectionID string `json:"connectionId" jsonschema:"target git connection id (see list_git_connections)"`
	Project      string `json:"project" jsonschema:"GitLab project (numeric id or 'group/repo') or GitHub repository (numeric id or 'owner/name')"`
	IID          int64  `json:"iid" jsonschema:"the merge request IID (the per-project number shown as !N)"`
	Body         string `json:"body" jsonschema:"the review comment to post (GitLab-flavored markdown)"`
}

type gitProjectIn struct {
	ConnectionID string `json:"connectionId" jsonschema:"target git connection id (see list_git_connections)"`
	Project      string `json:"project" jsonschema:"GitLab project (numeric id or 'group/repo') or GitHub repository (numeric id or 'owner/name')"`
}

type gitPipelinesIn struct {
	ConnectionID string `json:"connectionId" jsonschema:"target git connection id (see list_git_connections)"`
	Project      string `json:"project" jsonschema:"GitLab project (numeric id or 'group/repo') or GitHub repository (numeric id or 'owner/name')"`
	Ref          string `json:"ref,omitempty" jsonschema:"only pipelines of this branch or tag"`
	Status       string `json:"status,omitempty" jsonschema:"only this status: running, pending, success, failed, canceled, skipped, manual, scheduled"`
	Source       string `json:"source,omitempty" jsonschema:"only this trigger: push, schedule, web, merge_request_event, api, trigger, pipeline"`
	Page         int    `json:"page,omitempty" jsonschema:"page (30 per page, newest first); the result's nextPage says if there is more"`
}

type gitPipelineIn struct {
	ConnectionID string `json:"connectionId" jsonschema:"target git connection id (see list_git_connections)"`
	Project      string `json:"project" jsonschema:"GitLab project (numeric id or 'group/repo') or GitHub repository (numeric id or 'owner/name')"`
	Pipeline     int64  `json:"pipeline" jsonschema:"pipeline id (from git_pipelines)"`
}

type gitJobIn struct {
	ConnectionID string `json:"connectionId" jsonschema:"target git connection id (see list_git_connections)"`
	Project      string `json:"project" jsonschema:"GitLab project (numeric id or 'group/repo') or GitHub repository (numeric id or 'owner/name')"`
	Job          int64  `json:"job" jsonschema:"job id (from git_pipeline)"`
}

type gitJobLogIn struct {
	ConnectionID string `json:"connectionId" jsonschema:"target git connection id (see list_git_connections)"`
	Project      string `json:"project" jsonschema:"GitLab project (numeric id or 'group/repo') or GitHub repository (numeric id or 'owner/name')"`
	Job          int64  `json:"job" jsonschema:"job id (from git_pipeline)"`
	Lines        int    `json:"lines,omitempty" jsonschema:"how many lines from the end, 20-3000 (default 300)"`
}

type gitCIVarsIn struct {
	ConnectionID  string `json:"connectionId" jsonschema:"target git connection id (see list_git_connections)"`
	Project       string `json:"project" jsonschema:"GitLab project (numeric id or 'group/repo') or GitHub repository (numeric id or 'owner/name')"`
	IncludeValues bool   `json:"includeValues,omitempty" jsonschema:"also return the values of variables that are neither masked nor hidden (masked / hidden values are never returned)"`
}

type gitRunPipelineIn struct {
	ConnectionID string `json:"connectionId" jsonschema:"target git connection id (see list_git_connections)"`
	Project      string `json:"project" jsonschema:"GitLab project (numeric id or 'group/repo') or GitHub repository (numeric id or 'owner/name')"`
	Ref          string `json:"ref" jsonschema:"branch or tag to run the pipeline on"`
	Variables    []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	} `json:"variables,omitempty" jsonschema:"extra CI/CD variables for this run (GitHub: the workflow's inputs)"`
	Workflow string `json:"workflow,omitempty" jsonschema:"GitHub only: the workflow to dispatch (id or file name, see git_workflows); it must have workflow_dispatch"`
}

type gitScheduleIn struct {
	ConnectionID string `json:"connectionId" jsonschema:"target git connection id (see list_git_connections)"`
	Project      string `json:"project" jsonschema:"GitLab project (numeric id or 'group/repo') or GitHub repository (numeric id or 'owner/name')"`
	Schedule     int64  `json:"schedule" jsonschema:"pipeline schedule id (from git_pipeline_schedules)"`
}

// gitCIWrite is the note on the CI/CD write tools.
const gitCIWrite = " WRITE: needs a write grant on the git connection and a token that may write (GitLab: api scope, not read_api; GitHub: Actions write), plus rights on the project."

func gitPath(connID, suffix string, v url.Values) string {
	return withQuery("/api/git/connections/"+url.PathEscape(connID)+suffix, v)
}

// Git tools run where their connection works — per connection, see gitroute.go.
func registerGitTools(s *mcpsdk.Server, c *caller) {
	regCtx(s, "git_repos", "List the GitLab projects / GitHub repositories you can access on a git connection. Read-only."+gitNote,
		func(ctx context.Context, in gitReposIn) (int, string) {
			v := url.Values{}
			if in.Search != "" {
				v.Set("search", in.Search)
			}
			return c.gitCallCtx(ctx, in.ConnectionID, "GET", gitPath(in.ConnectionID, "/repos", v), nil)
		})
	regCtx(s, "git_file", "Read a file's contents (base64) at a ref — for reviewing code in context. Read-only."+gitNote,
		func(ctx context.Context, in gitFileIn) (int, string) {
			v := url.Values{}
			v.Set("project", in.Project)
			v.Set("path", in.Path)
			if in.Ref != "" {
				v.Set("ref", in.Ref)
			}
			return c.gitCallCtx(ctx, in.ConnectionID, "GET", gitPath(in.ConnectionID, "/file", v), nil)
		})
	regCtx(s, "git_merge_requests", "List a project's merge requests (GitHub: pull requests; iid = the PR number). Read-only."+gitNote,
		func(ctx context.Context, in gitMRListIn) (int, string) {
			v := url.Values{}
			v.Set("project", in.Project)
			if in.State != "" {
				v.Set("state", in.State)
			}
			return c.gitCallCtx(ctx, in.ConnectionID, "GET", gitPath(in.ConnectionID, "/merge_requests", v), nil)
		})
	regCtx(s, "git_merge_request", "Get a single merge request: title, description, state, branches, sha + diff_refs. Read-only."+gitNote,
		func(ctx context.Context, in gitMRIn) (int, string) {
			v := url.Values{}
			v.Set("project", in.Project)
			v.Set("iid", strconv.FormatInt(in.IID, 10))
			return c.gitCallCtx(ctx, in.ConnectionID, "GET", gitPath(in.ConnectionID, "/merge_request", v), nil)
		})
	regCtx(s, "git_merge_request_changes", "Get a merge request's per-file diffs (the surface to review) plus diff_refs. Read-only."+gitNote,
		func(ctx context.Context, in gitMRIn) (int, string) {
			v := url.Values{}
			v.Set("project", in.Project)
			v.Set("iid", strconv.FormatInt(in.IID, 10))
			return c.gitCallCtx(ctx, in.ConnectionID, "GET", gitPath(in.ConnectionID, "/merge_request_changes", v), nil)
		})
	regCtx(s, "git_merge_request_notes", "List a merge request's existing comments/notes. Read-only."+gitNote,
		func(ctx context.Context, in gitMRIn) (int, string) {
			v := url.Values{}
			v.Set("project", in.Project)
			v.Set("iid", strconv.FormatInt(in.IID, 10))
			return c.gitCallCtx(ctx, in.ConnectionID, "GET", gitPath(in.ConnectionID, "/merge_request_notes", v), nil)
		})
	regCtx(s, "git_create_merge_request_note", "Post a comment on a merge request / pull request. WRITE: requires a write grant on the git connection (and rights on the project at GitLab / GitHub)."+gitNote,
		func(ctx context.Context, in gitMRNoteIn) (int, string) {
			v := url.Values{}
			v.Set("project", in.Project)
			v.Set("iid", strconv.FormatInt(in.IID, 10))
			return c.gitCallCtx(ctx, in.ConnectionID, "POST", gitPath(in.ConnectionID, "/merge_request_notes", v), map[string]string{"body": in.Body})
		})
	// CI/CD.
	projQ := func(project string) url.Values {
		v := url.Values{}
		v.Set("project", project)
		return v
	}
	regCtx(s, "git_pipelines", "List a project's CI/CD pipelines, newest first: status, ref, sha, trigger (source: push, schedule, …), times. Read-only."+gitNote,
		func(ctx context.Context, in gitPipelinesIn) (int, string) {
			v := projQ(in.Project)
			for k, val := range map[string]string{"ref": in.Ref, "status": in.Status, "source": in.Source} {
				if val != "" {
					v.Set(k, val)
				}
			}
			if in.Page > 1 {
				v.Set("page", strconv.Itoa(in.Page))
			}
			return c.gitCallCtx(ctx, in.ConnectionID, "GET", gitPath(in.ConnectionID, "/pipelines", v), nil)
		})
	regCtx(s, "git_pipeline", "Get one pipeline with its stages and jobs (status, failure reason, duration), including trigger jobs (kind \"bridge\") with their downstream child / multi-project pipeline (open a same-project child with git_pipeline). Use git_job_log on a failed job to see why; trigger jobs have no log. Read-only."+gitNote,
		func(ctx context.Context, in gitPipelineIn) (int, string) {
			v := projQ(in.Project)
			v.Set("pipeline", strconv.FormatInt(in.Pipeline, 10))
			return c.gitCallCtx(ctx, in.ConnectionID, "GET", gitPath(in.ConnectionID, "/pipeline", v), nil)
		})
	regCtx(s, "git_job_log", "Read the end of a CI job's log as plain text (colours and section markers removed). Read-only."+gitNote,
		func(ctx context.Context, in gitJobLogIn) (int, string) {
			v := projQ(in.Project)
			v.Set("job", strconv.FormatInt(in.Job, 10))
			if in.Lines > 0 {
				v.Set("lines", strconv.Itoa(in.Lines))
			}
			return c.gitCallCtx(ctx, in.ConnectionID, "GET", gitPath(in.ConnectionID, "/job_log", v), nil)
		})
	regCtx(s, "git_pipeline_schedules", "List a project's scheduled pipelines: cron expression, time zone, branch, next run, active, owner. Read-only."+gitNote,
		func(ctx context.Context, in gitProjectIn) (int, string) {
			return c.gitCallCtx(ctx, in.ConnectionID, "GET", gitPath(in.ConnectionID, "/pipeline_schedules", projQ(in.Project)), nil)
		})
	regCtx(s, "git_ci_variables", "List a project's CI/CD variables and those inherited from its groups: key, environment scope, protected / masked / hidden. Values are left out unless includeValues is set, and even then only for variables that are neither masked nor hidden. Needs a write grant on the connection, and GitLab requires the Maintainer role (project) or Maintainer/Owner (group); a level you can't read says so. Read-only."+gitNote,
		func(ctx context.Context, in gitCIVarsIn) (int, string) {
			v := projQ(in.Project)
			if in.IncludeValues {
				v.Set("values", "unmasked")
			}
			return c.gitCallCtx(ctx, in.ConnectionID, "GET", gitPath(in.ConnectionID, "/ci_variables", v), nil)
		})
	regCtx(s, "git_run_pipeline", "Start a new pipeline on a branch or tag, optionally with extra variables."+gitCIWrite+gitNote,
		func(ctx context.Context, in gitRunPipelineIn) (int, string) {
			return c.gitCallCtx(ctx, in.ConnectionID, "POST", gitPath(in.ConnectionID, "/pipeline_run", projQ(in.Project)), map[string]any{"ref": in.Ref, "variables": in.Variables, "workflow": in.Workflow})
		})
	regCtx(s, "git_retry_pipeline", "Retry a pipeline's failed or canceled jobs."+gitCIWrite+gitNote,
		func(ctx context.Context, in gitPipelineIn) (int, string) {
			v := projQ(in.Project)
			v.Set("pipeline", strconv.FormatInt(in.Pipeline, 10))
			return c.gitCallCtx(ctx, in.ConnectionID, "POST", gitPath(in.ConnectionID, "/pipeline_retry", v), nil)
		})
	regCtx(s, "git_cancel_pipeline", "Cancel a running pipeline."+gitCIWrite+gitNote,
		func(ctx context.Context, in gitPipelineIn) (int, string) {
			v := projQ(in.Project)
			v.Set("pipeline", strconv.FormatInt(in.Pipeline, 10))
			return c.gitCallCtx(ctx, in.ConnectionID, "POST", gitPath(in.ConnectionID, "/pipeline_cancel", v), nil)
		})
	regCtx(s, "git_play_job", "Start a manual job (status manual)."+gitCIWrite+gitNote,
		func(ctx context.Context, in gitJobIn) (int, string) {
			v := projQ(in.Project)
			v.Set("job", strconv.FormatInt(in.Job, 10))
			return c.gitCallCtx(ctx, in.ConnectionID, "POST", gitPath(in.ConnectionID, "/job_play", v), nil)
		})
	regCtx(s, "git_retry_job", "Retry one CI job."+gitCIWrite+gitNote,
		func(ctx context.Context, in gitJobIn) (int, string) {
			v := projQ(in.Project)
			v.Set("job", strconv.FormatInt(in.Job, 10))
			return c.gitCallCtx(ctx, in.ConnectionID, "POST", gitPath(in.ConnectionID, "/job_retry", v), nil)
		})
	regCtx(s, "git_play_schedule", "Run a scheduled pipeline now (it keeps its schedule). GitHub: schedule = the workflow id; it dispatches the workflow on the default branch and needs workflow_dispatch."+gitCIWrite+gitNote,
		func(ctx context.Context, in gitScheduleIn) (int, string) {
			v := projQ(in.Project)
			v.Set("schedule", strconv.FormatInt(in.Schedule, 10))
			return c.gitCallCtx(ctx, in.ConnectionID, "POST", gitPath(in.ConnectionID, "/schedule_play", v), nil)
		})
	regCtx(s, "git_workflows", "GitHub only: list a repository's Actions workflows — id, name, file, crons, whether it can be run by hand (workflow_dispatch) and its input names. Use before git_run_pipeline on GitHub. Read-only."+gitNote,
		func(ctx context.Context, in gitProjectIn) (int, string) {
			return c.gitCallCtx(ctx, in.ConnectionID, "GET", gitPath(in.ConnectionID, "/workflows", projQ(in.Project)), nil)
		})
	regCtx(s, "git_approve_merge_request", "Approve a merge request / pull request. WRITE: requires a write grant on the git connection. GitLab needs its MR Approvals feature (404 without it); GitHub doesn't let you approve your own pull request."+gitNote,
		func(ctx context.Context, in gitMRIn) (int, string) {
			v := url.Values{}
			v.Set("project", in.Project)
			v.Set("iid", strconv.FormatInt(in.IID, 10))
			return c.gitCallCtx(ctx, in.ConnectionID, "POST", gitPath(in.ConnectionID, "/merge_request_approve", v), nil)
		})
}
