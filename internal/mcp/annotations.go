package mcp

// Tool annotations (MCP ToolAnnotations): read-only, destructive, idempotent,
// open-world. Clients use them to auto-approve safe reads and to ask before a
// write — so every tool must be classified here (TestEveryToolAnnotated fails
// on a new tool without an entry). They are hints for the client, never the
// security boundary: grants, allowWrite, step-up and the lock still decide.
//
// Classification rules:
//   - readOnly: never changes anything (incl. commit-less Kafka reads);
//   - destructive: may overwrite or delete, or runs something with effects we
//     can't see (CI jobs that deploy, arbitrary HTTP / SQL / Redis / ES with
//     allowWrite, test runs that call mutating endpoints);
//   - idempotent: repeating the same call has no further effect;
//   - openWorld: talks to a system outside PatchMan (a connection's target,
//     GitLab, the internet). PatchMan's own data (collections, notes, boards,
//     mocks…) is closed-world.

import mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

type toolKind uint8

const (
	kRead toolKind = 1 << iota
	kDestructive
	kIdempotent
	kOpenWorld
)

const (
	readLocal    = kRead | kIdempotent
	readRemote   = kRead | kIdempotent | kOpenWorld
	createLocal  = toolKind(0)                // additive write
	updateLocal  = kDestructive | kIdempotent // overwrites fields
	deleteLocal  = kDestructive | kIdempotent // removes
	reorderLocal = kIdempotent                // non-lossy rearrange
	writeRemote  = kDestructive | kOpenWorld  // may change / run things out there
	addRemote    = kOpenWorld                 // additive write out there
	idemRemote   = kIdempotent | kOpenWorld   // e.g. approve
	stopRemote   = kDestructive | kIdempotent | kOpenWorld
)

var toolKinds = map[string]toolKind{
	// Collections / requests / environments
	"list_collections": readLocal, "list_collection_requests": readLocal, "get_request": readLocal,
	"create_collection": createLocal, "update_collection": updateLocal, "delete_collection": deleteLocal,
	"create_request": createLocal, "update_request": updateLocal, "delete_request": deleteLocal,
	"reorder_collections": reorderLocal, "reorder_requests": reorderLocal,
	"list_environments": readLocal, "create_environment": createLocal, "update_environment": updateLocal, "delete_environment": deleteLocal,
	// Test cases
	"list_test_cases": readLocal, "get_test_case": readLocal, "list_test_folders": readLocal,
	"create_test_case": createLocal, "update_test_case": updateLocal, "delete_test_case": deleteLocal,
	"create_test_folder": createLocal, "rename_test_folder": reorderLocal, "delete_test_folder": deleteLocal,
	"move_test_case":  reorderLocal,
	"run_test_folder": kDestructive | kOpenWorld, // sends the folder's requests
	// Mocks (served by PatchMan itself)
	"list_mock_servers": readLocal, "list_mock_routes": readLocal,
	"create_mock_server": createLocal, "update_mock_server": updateLocal, "delete_mock_server": deleteLocal,
	"create_mock_route": createLocal, "update_mock_route": updateLocal, "delete_mock_route": deleteLocal,
	// Connection discovery
	"list_git_connections": readLocal, "list_db_connections": readLocal, "list_es_connections": readLocal,
	"list_redis_connections": readLocal, "list_kafka_connections": readLocal, "list_s3_connections": readLocal,
	"list_k8s_connections": readLocal, "list_grafana_connections": readLocal, "list_rabbitmq_connections": readLocal,
	"list_graylog_connections": readLocal, "list_observability_connections": readLocal,
	// Execution against connections
	"send_http_request": writeRemote, "dns_lookup": readRemote,
	"run_db_query": writeRemote, // read-only unless allowWrite on a read-write connection
	"es_cluster":   readRemote, "es_indices": readRemote, "es_mapping": readRemote,
	"es_data_views": readRemote, "es_data_view_search": readRemote,
	"es_request": writeRemote, // allowWrite
	"redis_get":  readRemote, "redis_key_meta": readRemote, "redis_scan_keys": readRemote,
	"redis_command": writeRemote, // allowWrite
	"kafka_brokers": readRemote, "kafka_topics": readRemote, "kafka_groups": readRemote, "kafka_lag": readRemote,
	"kafka_consume":   readRemote, // commit-less, no consumer group
	"s3_list_buckets": readRemote, "s3_list_objects": readRemote, "s3_get_object": readRemote,
	"k8s_contexts": readRemote, "k8s_overview": readRemote, "k8s_list_resources": readRemote, "k8s_get_resource": readRemote,
	"rabbitmq_overview": readRemote, "rabbitmq_queues": readRemote, "rabbitmq_queue": readRemote,
	"rabbitmq_exchanges": readRemote, "rabbitmq_topology": readRemote,
	"rabbitmq_peek":      kOpenWorld, // messages are put back but marked redelivered
	"rabbitmq_publish":   addRemote,
	"grafana_dashboards": readRemote, "grafana_dashboard": readRemote, "grafana_datasources": readRemote,
	"grafana_panel_data": readRemote, "grafana_query": readRemote,
	"graylog_streams": readRemote, "graylog_search": readRemote, "graylog_dashboards": readRemote,
	"service_map": readRemote, "newrelic_nrql": readRemote, "newrelic_entities": readRemote, "newrelic_alerts": readRemote,
	// Git (GitLab)
	"git_repos": readRemote, "git_file": readRemote,
	"git_merge_requests": readRemote, "git_merge_request": readRemote, "git_merge_request_changes": readRemote,
	"git_merge_request_notes": readRemote, "git_pipelines": readRemote, "git_pipeline": readRemote,
	"git_job_log": readRemote, "git_workflows": readRemote,
	// Google Cloud
	"list_gcp_credentials": readLocal, "gcp_projects": readRemote, "gcs_list_buckets": readRemote, "gcs_list_objects": readRemote,
	"gcs_get_object": readRemote, "pubsub_topics": readRemote, "pubsub_subscriptions": readRemote, "gcp_logs_search": readRemote,
	"gke_clusters":           readRemote,
	"git_pipeline_schedules": readRemote, "git_ci_variables": readRemote,
	// pubsub_peek hands the messages back, but counts a delivery attempt.
	"pubsub_peek":                   idemRemote,
	"git_create_merge_request_note": addRemote,
	"git_approve_merge_request":     idemRemote,
	"git_cancel_pipeline":           stopRemote,
	// CI jobs can deploy: treated as destructive so clients ask first.
	"git_run_pipeline": writeRemote, "git_retry_pipeline": writeRemote, "git_play_job": writeRemote,
	"git_retry_job": writeRemote, "git_play_schedule": writeRemote,
	// Whiteboards
	"list_whiteboards": readLocal, "get_whiteboard": readLocal,
	"create_whiteboard": createLocal, "update_whiteboard": updateLocal, "delete_whiteboard": deleteLocal,
	"draw_on_whiteboard": createLocal,
	// Notes / agent memory
	"list_notes": readLocal, "get_note": readLocal, "search_notes": readLocal, "recall": readLocal, "forget": deleteLocal, "memory_folders": readLocal,
	"create_note": createLocal, "update_note": updateLocal, "delete_note": deleteLocal,
	"remember": createLocal, "link_notes": kIdempotent,
}

// annotationsFor returns the MCP annotations for a tool (nil when unknown,
// which TestEveryToolAnnotated reports).
func annotationsFor(name string) *mcpsdk.ToolAnnotations {
	k, ok := toolKinds[name]
	if !ok {
		return nil
	}
	a := &mcpsdk.ToolAnnotations{
		ReadOnlyHint:   k&kRead != 0,
		IdempotentHint: k&kIdempotent != 0,
	}
	ow := k&kOpenWorld != 0
	a.OpenWorldHint = &ow
	if !a.ReadOnlyHint {
		d := k&kDestructive != 0
		a.DestructiveHint = &d
	}
	return a
}
