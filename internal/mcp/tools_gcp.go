package mcp

// Google Cloud tools (GOOGLE_CLOUD_RFC.md): read-first over the user's shared
// Google credentials. Writes (upload / publish) stay in the app, where the
// credential's "Allow writes" switch and the confirmation are.

import (
	"context"
	"net/url"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type gcpCredIn struct {
	CredentialID string `json:"credentialId" jsonschema:"Google Cloud credential id (see list_gcp_credentials)"`
	Project      string `json:"project,omitempty" jsonschema:"GCP project id; default: the credential's project"`
}

type gcsObjectsIn struct {
	gcpCredIn
	Bucket    string `json:"bucket" jsonschema:"bucket name"`
	Prefix    string `json:"prefix,omitempty" jsonschema:"list under this prefix (a 'folder' ends with /)"`
	PageToken string `json:"pageToken,omitempty"`
}

type gcsObjectIn struct {
	gcpCredIn
	Bucket string `json:"bucket"`
	Name   string `json:"name" jsonschema:"object name (full path)"`
}

type pubsubSubsIn struct {
	gcpCredIn
	Topic string `json:"topic,omitempty" jsonschema:"only this topic's subscriptions"`
}

type pubsubPeekIn struct {
	gcpCredIn
	Subscription string `json:"subscription"`
	Max          int    `json:"max,omitempty" jsonschema:"1-100, default 10"`
}

type gcpLogsIn struct {
	gcpCredIn
	Filter    string `json:"filter,omitempty" jsonschema:"Cloud Logging query language, e.g. severity>=ERROR AND resource.type=\"k8s_container\""`
	From      string `json:"from,omitempty" jsonschema:"RFC 3339 start (default: an hour ago)"`
	To        string `json:"to,omitempty" jsonschema:"RFC 3339 end"`
	PageSize  int    `json:"pageSize,omitempty" jsonschema:"max entries, default 200"`
	PageToken string `json:"pageToken,omitempty"`
}

func gcpPath(id, suffix string, v url.Values) string {
	return withQuery("/api/gcp/credentials/"+url.PathEscape(id)+suffix, v)
}

func projQuery(p string) url.Values {
	v := url.Values{}
	if p != "" {
		v.Set("project", p)
	}
	return v
}

func registerGCPTools(s *mcpsdk.Server, c *caller) {
	reg(s, "list_gcp_credentials", "List the user's Google Cloud credentials (service account, gcloud on this computer, linked Google account): id, kind, account, default project. Read-only.",
		func(struct{}) (int, string) { return c.call("GET", "/api/gcp/credentials", nil) })
	regCtx(s, "gcp_projects", "List the GCP projects a Google Cloud credential can see. Read-only.",
		func(ctx context.Context, in gcpCredIn) (int, string) {
			return c.callCtx(ctx, "GET", gcpPath(in.CredentialID, "/projects", nil), nil)
		})
	regCtx(s, "gcs_list_buckets", "List a project's Cloud Storage buckets. Read-only.",
		func(ctx context.Context, in gcpCredIn) (int, string) {
			return c.callCtx(ctx, "GET", gcpPath(in.CredentialID, "/gcs/buckets", projQuery(in.Project)), nil)
		})
	regCtx(s, "gcs_list_objects", "List one level of a Cloud Storage bucket: sub-prefixes ('folders') and objects with size, type, updated. Read-only.",
		func(ctx context.Context, in gcsObjectsIn) (int, string) {
			v := url.Values{"bucket": {in.Bucket}}
			if in.Prefix != "" {
				v.Set("prefix", in.Prefix)
			}
			if in.PageToken != "" {
				v.Set("pageToken", in.PageToken)
			}
			return c.callCtx(ctx, "GET", gcpPath(in.CredentialID, "/gcs/objects", v), nil)
		})
	regCtx(s, "gcs_get_object", "Read a Cloud Storage object: metadata plus its content as base64 (up to 5 MB). Read-only.",
		func(ctx context.Context, in gcsObjectIn) (int, string) {
			return c.callCtx(ctx, "GET", gcpPath(in.CredentialID, "/gcs/object", url.Values{"bucket": {in.Bucket}, "name": {in.Name}}), nil)
		})
	regCtx(s, "pubsub_topics", "List a project's Pub/Sub topics. Read-only.",
		func(ctx context.Context, in gcpCredIn) (int, string) {
			return c.callCtx(ctx, "GET", gcpPath(in.CredentialID, "/pubsub/topics", projQuery(in.Project)), nil)
		})
	regCtx(s, "pubsub_subscriptions", "List Pub/Sub subscriptions (optionally one topic's): ack deadline, retention, filter, push, dead-letter topic. Read-only.",
		func(ctx context.Context, in pubsubSubsIn) (int, string) {
			v := projQuery(in.Project)
			if in.Topic != "" {
				v.Set("topic", in.Topic)
			}
			return c.callCtx(ctx, "GET", gcpPath(in.CredentialID, "/pubsub/subscriptions", v), nil)
		})
	regCtx(s, "pubsub_peek", "Look at up to max messages waiting on a subscription without consuming them: they are pulled and handed straight back, so they will be delivered again (as redelivered). Each peek counts as a delivery attempt — with a dead-letter policy, repeated peeks can move a message to the dead-letter topic.",
		func(ctx context.Context, in pubsubPeekIn) (int, string) {
			return c.callCtx(ctx, "POST", gcpPath(in.CredentialID, "/pubsub/peek", projQuery(in.Project)), map[string]any{"subscription": in.Subscription, "max": in.Max})
		})
	regCtx(s, "gcp_logs_search", "Search Cloud Logging, newest first: timestamp, severity, log, resource, message (+ structured payload). Read-only.",
		func(ctx context.Context, in gcpLogsIn) (int, string) {
			return c.callCtx(ctx, "POST", gcpPath(in.CredentialID, "/logging/entries", nil), map[string]any{
				"project": in.Project, "filter": in.Filter, "from": in.From, "to": in.To, "pageSize": in.PageSize, "pageToken": in.PageToken})
		})
	regCtx(s, "gke_clusters", "List a project's GKE clusters: location, status, version, node count. Add one to the Kubernetes tool in the app to use the k8s_* tools on it. Read-only.",
		func(ctx context.Context, in gcpCredIn) (int, string) {
			return c.callCtx(ctx, "GET", gcpPath(in.CredentialID, "/gke/clusters", projQuery(in.Project)), nil)
		})
}
