package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// RabbitMQ tools. They go through the same REST routes as the UI, so grants
// (publish needs write access), audit and the peek safety rules apply
// unchanged. Answers are compacted to one line per queue / exchange /
// binding instead of the management API's large objects.

type rabbitIDIn struct {
	ID string `json:"id" jsonschema:"RabbitMQ connection id (see list_rabbitmq_connections)"`
}

type rabbitListIn struct {
	ID    string `json:"id" jsonschema:"RabbitMQ connection id"`
	Vhost string `json:"vhost,omitempty" jsonschema:"virtual host (default: all vhosts)"`
	Name  string `json:"name,omitempty" jsonschema:"name filter (regular expression)"`
	Page  int    `json:"page,omitempty" jsonschema:"page number (100 per page)"`
}

type rabbitQueueIn struct {
	ID    string `json:"id" jsonschema:"RabbitMQ connection id"`
	Vhost string `json:"vhost" jsonschema:"virtual host, e.g. /"`
	Name  string `json:"name" jsonschema:"queue name"`
}

type rabbitVhostIn struct {
	ID    string `json:"id" jsonschema:"RabbitMQ connection id"`
	Vhost string `json:"vhost" jsonschema:"virtual host, e.g. /"`
}

type rabbitPeekIn struct {
	ID         string `json:"id" jsonschema:"RabbitMQ connection id"`
	Vhost      string `json:"vhost" jsonschema:"virtual host, e.g. /"`
	Queue      string `json:"queue" jsonschema:"queue name"`
	Count      int    `json:"count,omitempty" jsonschema:"messages to read (default 5, max 50)"`
	Offset     string `json:"offset,omitempty" jsonschema:"stream queues only: first | last | next | offset number | RFC3339 time (default first)"`
	AcceptRisk bool   `json:"acceptRisk,omitempty" jsonschema:"required on RabbitMQ 3.x quorum queues with a delivery limit, where each read counts as a delivery"`
}

type rabbitPublishIn struct {
	ID         string         `json:"id" jsonschema:"RabbitMQ connection id"`
	Vhost      string         `json:"vhost" jsonschema:"virtual host, e.g. /"`
	Exchange   string         `json:"exchange,omitempty" jsonschema:"exchange name; empty = default exchange (routingKey = queue name)"`
	RoutingKey string         `json:"routingKey,omitempty" jsonschema:"routing key"`
	Payload    string         `json:"payload" jsonschema:"message body (text)"`
	Properties map[string]any `json:"properties,omitempty" jsonschema:"AMQP properties, e.g. {\"content_type\":\"application/json\",\"headers\":{\"k\":\"v\"},\"delivery_mode\":2}"`
}

func rabbitPath(id, rest string) string { return "/api/rabbit/" + url.PathEscape(id) + rest }

func registerRabbitTools(s *mcpsdk.Server, c *caller) {
	regCtx(s, "rabbitmq_overview", "RabbitMQ cluster overview: version, totals (queues, connections, consumers, ready/unacked messages), message rates, nodes with memory/disk alarms, vhosts. Read-only."+connNote,
		func(ctx context.Context, in rabbitIDIn) (int, string) {
			code, body := c.callCtx(ctx, "GET", rabbitPath(in.ID, "/overview"), nil)
			if code != 200 {
				return code, body
			}
			return code, compactRabbitOverview(body)
		})
	regCtx(s, "rabbitmq_queues", "List RabbitMQ queues: type, state, ready/unacked, consumers, rates, dead-letter target. Read-only."+connNote,
		func(ctx context.Context, in rabbitListIn) (int, string) {
			code, body := c.callCtx(ctx, "GET", withQuery(rabbitPath(in.ID, "/queues"), rabbitListQuery(in)), nil)
			if code != 200 {
				return code, body
			}
			return code, compactRabbitQueues(body)
		})
	regCtx(s, "rabbitmq_queue", "Describe one RabbitMQ queue: counts, rates, arguments/policy (TTL, length limit, dead-lettering, delivery limit), consumers and bindings. Read-only."+connNote,
		func(ctx context.Context, in rabbitQueueIn) (int, string) {
			v := url.Values{"vhost": {in.Vhost}, "name": {in.Name}}
			code, body := c.callCtx(ctx, "GET", withQuery(rabbitPath(in.ID, "/queue"), v), nil)
			if code != 200 {
				return code, body
			}
			return code, compactRabbitQueue(body)
		})
	regCtx(s, "rabbitmq_exchanges", "List RabbitMQ exchanges (type, durable/internal, publish rates). Read-only."+connNote,
		func(ctx context.Context, in rabbitListIn) (int, string) {
			code, body := c.callCtx(ctx, "GET", withQuery(rabbitPath(in.ID, "/exchanges"), rabbitListQuery(in)), nil)
			if code != 200 {
				return code, body
			}
			return code, compactRabbitExchanges(body)
		})
	regCtx(s, "rabbitmq_topology", "A vhost's routing topology as \"exchange -[routing key]-> queue/exchange\" lines. Read-only."+connNote,
		func(ctx context.Context, in rabbitVhostIn) (int, string) {
			code, body := c.callCtx(ctx, "GET", withQuery(rabbitPath(in.ID, "/topology"), url.Values{"vhost": {in.Vhost}}), nil)
			if code != 200 {
				return code, body
			}
			return code, compactRabbitTopology(body)
		})
	regCtx(s, "rabbitmq_peek",
		"Read messages from a RabbitMQ queue without consuming them. Classic and quorum queues: messages are fetched and put back, so they become marked \"redelivered\" (on RabbitMQ 3.x quorum queues with a delivery limit each read also counts as a delivery — pass acceptRisk). Stream queues are read by offset with no side effects."+connNote,
		func(ctx context.Context, in rabbitPeekIn) (int, string) {
			v := url.Values{"vhost": {in.Vhost}, "name": {in.Queue}}
			code, body := c.callCtx(ctx, "GET", withQuery(rabbitPath(in.ID, "/queue"), v), nil)
			if code != 200 {
				return code, body
			}
			var q struct {
				Data struct {
					Peek struct {
						Mode          string `json:"mode"`
						Risk          string `json:"risk"`
						DeliveryLimit int    `json:"deliveryLimit"`
					} `json:"peek"`
				} `json:"data"`
			}
			_ = json.Unmarshal([]byte(body), &q)
			count := in.Count
			if count <= 0 {
				count = 5
			}
			if count > 50 {
				count = 50
			}
			if q.Data.Peek.Mode == "stream" {
				return c.callCtx(ctx, "POST", rabbitPath(in.ID, "/messages/stream"), map[string]any{
					"vhost": in.Vhost, "queue": in.Queue, "offset": in.Offset, "count": count,
				})
			}
			if q.Data.Peek.Risk == "delivery_limit" && !in.AcceptRisk {
				return 400, fmt.Sprintf(`{"error":"This RabbitMQ 3.x quorum queue has delivery-limit %d: every read counts as a delivery and messages read that many times are dead-lettered or dropped. Call again with acceptRisk=true to read anyway."}`, q.Data.Peek.DeliveryLimit)
			}
			return c.callCtx(ctx, "POST", rabbitPath(in.ID, "/messages/get"), map[string]any{
				"vhost": in.Vhost, "queue": in.Queue, "count": count, "truncate": 20000,
			})
		})
	regCtx(s, "rabbitmq_publish", "Publish one message to a RabbitMQ exchange (needs write access to the connection). Returns whether it was routed to any queue."+connNote,
		func(ctx context.Context, in rabbitPublishIn) (int, string) {
			return c.callCtx(ctx, "POST", rabbitPath(in.ID, "/publish"), map[string]any{
				"vhost": in.Vhost, "exchange": in.Exchange, "routingKey": in.RoutingKey,
				"payload": in.Payload, "payloadEncoding": "string", "properties": in.Properties,
			})
		})
}

func rabbitListQuery(in rabbitListIn) url.Values {
	v := url.Values{}
	if in.Vhost != "" {
		v.Set("vhost", in.Vhost)
	}
	if in.Name != "" {
		v.Set("name", in.Name)
		v.Set("regex", "1")
	}
	if in.Page > 0 {
		v.Set("page", fmt.Sprint(in.Page))
	}
	return v
}

// ── compaction ──────────────────────────────────────────────────────

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

func dig(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

func rate(m map[string]any, key string) string {
	r := num(dig(m, "message_stats", key, "rate"))
	if r == 0 {
		return "0"
	}
	return fmt.Sprintf("%.1f", r)
}

func compactRabbitOverview(body string) string {
	var env struct {
		Data struct {
			Overview   map[string]any   `json:"overview"`
			Nodes      []map[string]any `json:"nodes"`
			NodesError string           `json:"nodesError"`
			Vhosts     []map[string]any `json:"vhosts"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(body), &env) != nil {
		return body
	}
	ov := env.Data.Overview
	var b strings.Builder
	fmt.Fprintf(&b, "RabbitMQ %v (Erlang %v), cluster %v\n", ov["rabbitmq_version"], ov["erlang_version"], ov["cluster_name"])
	if t, ok := ov["object_totals"].(map[string]any); ok {
		fmt.Fprintf(&b, "totals: %v queues, %v exchanges, %v connections, %v channels, %v consumers\n",
			t["queues"], t["exchanges"], t["connections"], t["channels"], t["consumers"])
	}
	if t, ok := ov["queue_totals"].(map[string]any); ok {
		fmt.Fprintf(&b, "messages: %v ready, %v unacked\n", t["messages_ready"], t["messages_unacknowledged"])
	}
	fmt.Fprintf(&b, "rates/s: publish %s, deliver %s, ack %s, redeliver %s\n",
		rate(ov, "publish_details"), rate(ov, "deliver_get_details"), rate(ov, "ack_details"), rate(ov, "redeliver_details"))
	for _, n := range env.Data.Nodes {
		alarms := []string{}
		if b, _ := n["mem_alarm"].(bool); b {
			alarms = append(alarms, "MEMORY ALARM")
		}
		if b, _ := n["disk_free_alarm"].(bool); b {
			alarms = append(alarms, "DISK ALARM")
		}
		fmt.Fprintf(&b, "node %v: running=%v mem %.0f/%.0f MiB, disk free %.0f MiB, fd %v/%v %s\n", n["name"], n["running"],
			num(n["mem_used"])/1048576, num(n["mem_limit"])/1048576, num(n["disk_free"])/1048576, n["fd_used"], n["fd_total"], strings.Join(alarms, " "))
	}
	if env.Data.NodesError != "" {
		fmt.Fprintf(&b, "nodes: %s\n", env.Data.NodesError)
	}
	names := []string{}
	for _, v := range env.Data.Vhosts {
		names = append(names, fmt.Sprint(v["name"]))
	}
	fmt.Fprintf(&b, "vhosts: %s\n", strings.Join(names, ", "))
	return b.String()
}

func compactRabbitQueues(body string) string {
	var env struct {
		Data struct {
			Items         []map[string]any `json:"items"`
			FilteredCount int              `json:"filtered_count"`
			Page          int              `json:"page"`
			PageCount     int              `json:"page_count"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(body), &env) != nil {
		return body
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d queues (page %d/%d). name [vhost] type state | ready unacked consumers | in/out per s | notes\n", env.Data.FilteredCount, env.Data.Page, env.Data.PageCount)
	for _, q := range env.Data.Items {
		notes := []string{}
		if args, ok := q["arguments"].(map[string]any); ok {
			if dlx, ok := args["x-dead-letter-exchange"]; ok {
				notes = append(notes, fmt.Sprintf("dlx=%v", dlx))
			}
			if ttl, ok := args["x-message-ttl"]; ok {
				notes = append(notes, fmt.Sprintf("ttl=%vms", ttl))
			}
			if ml, ok := args["x-max-length"]; ok {
				notes = append(notes, fmt.Sprintf("max-length=%v", ml))
			}
		}
		if p, _ := q["policy"].(string); p != "" {
			notes = append(notes, "policy="+p)
		}
		fmt.Fprintf(&b, "%v [%v] %v %v | %v %v %v | %s/%s | %s\n", q["name"], q["vhost"], q["type"], q["state"],
			q["messages_ready"], q["messages_unacknowledged"], q["consumers"],
			rate(q, "publish_details"), rate(q, "deliver_get_details"), strings.Join(notes, " "))
	}
	return b.String()
}

func compactRabbitQueue(body string) string {
	var env struct {
		Data struct {
			Queue    map[string]any   `json:"queue"`
			Bindings []map[string]any `json:"bindings"`
			Peek     map[string]any   `json:"peek"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(body), &env) != nil {
		return body
	}
	q := env.Data.Queue
	var b strings.Builder
	fmt.Fprintf(&b, "queue %v [vhost %v] type=%v state=%v durable=%v node=%v\n", q["name"], q["vhost"], q["type"], q["state"], q["durable"], q["node"])
	fmt.Fprintf(&b, "messages: %v total, %v ready, %v unacked, %.0f bytes\n", q["messages"], q["messages_ready"], q["messages_unacknowledged"], num(q["message_bytes"]))
	fmt.Fprintf(&b, "rates/s: publish %s, deliver %s, ack %s, redeliver %s\n", rate(q, "publish_details"), rate(q, "deliver_get_details"), rate(q, "ack_details"), rate(q, "redeliver_details"))
	if a, ok := q["arguments"].(map[string]any); ok && len(a) > 0 {
		j, _ := json.Marshal(a)
		fmt.Fprintf(&b, "arguments: %s\n", j)
	}
	if p, ok := q["effective_policy_definition"].(map[string]any); ok && len(p) > 0 {
		j, _ := json.Marshal(p)
		fmt.Fprintf(&b, "policy %v: %s\n", q["policy"], j)
	}
	if cs, ok := q["consumer_details"].([]any); ok {
		fmt.Fprintf(&b, "consumers: %d\n", len(cs))
		for i, cd := range cs {
			if i >= 20 {
				break
			}
			m, _ := cd.(map[string]any)
			fmt.Fprintf(&b, "  %v prefetch=%v ack=%v active=%v channel=%v\n", m["consumer_tag"], m["prefetch_count"], m["ack_required"], m["active"], dig(m, "channel_details", "name"))
		}
	}
	for _, bd := range env.Data.Bindings {
		src := fmt.Sprint(bd["source"])
		if src == "" {
			src = "(default)"
		}
		fmt.Fprintf(&b, "binding: %s -[%v]-> this queue\n", src, bd["routing_key"])
	}
	if env.Data.Peek != nil {
		fmt.Fprintf(&b, "peek: mode=%v risk=%v deliveryLimit=%v\n", env.Data.Peek["mode"], env.Data.Peek["risk"], env.Data.Peek["deliveryLimit"])
	}
	return b.String()
}

func compactRabbitExchanges(body string) string {
	var env struct {
		Data struct {
			Items         []map[string]any `json:"items"`
			FilteredCount int              `json:"filtered_count"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(body), &env) != nil {
		return body
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d exchanges. name [vhost] type flags | in/out per s\n", env.Data.FilteredCount)
	for _, e := range env.Data.Items {
		name := fmt.Sprint(e["name"])
		if name == "" {
			name = "(default)"
		}
		flags := []string{}
		if d, _ := e["durable"].(bool); d {
			flags = append(flags, "durable")
		}
		if d, _ := e["internal"].(bool); d {
			flags = append(flags, "internal")
		}
		fmt.Fprintf(&b, "%s [%v] %v %s | %s/%s\n", name, e["vhost"], e["type"], strings.Join(flags, ","), rate(e, "publish_in_details"), rate(e, "publish_out_details"))
	}
	return b.String()
}

func compactRabbitTopology(body string) string {
	var env struct {
		Data struct {
			Bindings []map[string]any `json:"bindings"`
			Queues   []map[string]any `json:"queues"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(body), &env) != nil {
		return body
	}
	lines := []string{}
	bound := map[string]bool{}
	for _, b := range env.Data.Bindings {
		src := fmt.Sprint(b["source"])
		if src == "" {
			continue // implicit default-exchange bindings add nothing
		}
		kind := "queue"
		if b["destination_type"] == "exchange" {
			kind = "exchange"
		}
		lines = append(lines, fmt.Sprintf("%s -[%v]-> %s %v", src, b["routing_key"], kind, b["destination"]))
		if kind == "queue" {
			bound[fmt.Sprint(b["destination"])] = true
		}
	}
	sort.Strings(lines)
	for _, q := range env.Data.Queues {
		if !bound[fmt.Sprint(q["name"])] {
			lines = append(lines, fmt.Sprintf("(default exchange only) -> queue %v", q["name"]))
		}
	}
	return strings.Join(lines, "\n")
}
