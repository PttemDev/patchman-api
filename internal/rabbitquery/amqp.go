package rabbitquery

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"patchman-be/internal/models"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Message mirrors the management API's get answer so the FE renders peeked,
// stream-read and tailed messages with one viewer.
type Message struct {
	PayloadBytes    int            `json:"payload_bytes"`
	Redelivered     bool           `json:"redelivered"`
	Exchange        string         `json:"exchange"`
	RoutingKey      string         `json:"routing_key"`
	Properties      map[string]any `json:"properties"`
	Payload         string         `json:"payload"`
	PayloadEncoding string         `json:"payload_encoding"`
	// Stream offset (stream reads only).
	Offset *int64 `json:"offset,omitempty"`
	// Receive time (tail only).
	ReceivedAt int64 `json:"received_at,omitempty"`
}

const maxPayload = 256 << 10

func toMessage(d amqp.Delivery) Message {
	m := Message{
		PayloadBytes: len(d.Body), Redelivered: d.Redelivered, Exchange: d.Exchange, RoutingKey: d.RoutingKey,
		Properties: props(d),
	}
	body := d.Body
	if len(body) > maxPayload {
		body = body[:maxPayload]
	}
	if utf8.Valid(body) {
		m.Payload, m.PayloadEncoding = string(body), "string"
	} else {
		m.Payload, m.PayloadEncoding = base64.StdEncoding.EncodeToString(body), "base64"
	}
	if v, ok := d.Headers["x-stream-offset"]; ok {
		if n, ok := v.(int64); ok {
			m.Offset = &n
		}
	}
	return m
}

func props(d amqp.Delivery) map[string]any {
	p := map[string]any{}
	set := func(k string, v string) {
		if v != "" {
			p[k] = v
		}
	}
	set("content_type", d.ContentType)
	set("content_encoding", d.ContentEncoding)
	set("correlation_id", d.CorrelationId)
	set("reply_to", d.ReplyTo)
	set("expiration", d.Expiration)
	set("message_id", d.MessageId)
	set("type", d.Type)
	set("user_id", d.UserId)
	set("app_id", d.AppId)
	if d.DeliveryMode != 0 {
		p["delivery_mode"] = d.DeliveryMode
	}
	if d.Priority != 0 {
		p["priority"] = d.Priority
	}
	if !d.Timestamp.IsZero() {
		p["timestamp"] = d.Timestamp.Unix()
	}
	if len(d.Headers) > 0 {
		p["headers"] = plainTable(d.Headers)
	}
	return p
}

// plainTable makes an AMQP table JSON-friendly (bytes → string, times → unix).
func plainTable(t amqp.Table) map[string]any {
	out := make(map[string]any, len(t))
	for k, v := range t {
		out[k] = plainValue(v)
	}
	return out
}

func plainValue(v any) any {
	switch x := v.(type) {
	case amqp.Table:
		return plainTable(x)
	case []any:
		o := make([]any, len(x))
		for i, e := range x {
			o[i] = plainValue(e)
		}
		return o
	case []byte:
		if utf8.Valid(x) {
			return string(x)
		}
		return base64.StdEncoding.EncodeToString(x)
	case time.Time:
		return x.Unix()
	}
	return v
}

// ── stream reads ────────────────────────────────────────────────────

// StreamParams: Offset is first | last | next | <offset number> |
// <RFC3339 time or epoch ms>. Streams don't lose messages when read, so this
// path is side-effect free.
type StreamParams struct {
	Vhost  string
	Queue  string
	Offset string
	Count  int
}

func streamOffset(s string) any {
	s = strings.TrimSpace(s)
	switch s {
	case "", "first":
		return "first"
	case "last", "next":
		return s
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		// Epoch milliseconds look like timestamps; small numbers are offsets.
		if n > 1_000_000_000_000 {
			return time.UnixMilli(n)
		}
		return n
	}
	return "first"
}

func (r *Registry) StreamRead(ctx context.Context, conn models.RabbitConnection, p StreamParams) ([]Message, error) {
	if p.Count <= 0 {
		p.Count = 20
	}
	if p.Count > 1000 {
		p.Count = 1000
	}
	ch, err := r.channel(ctx, conn, p.Vhost)
	if err != nil {
		return nil, err
	}
	defer ch.Close()
	prefetch := p.Count
	if prefetch > 500 {
		prefetch = 500
	}
	if err := ch.Qos(prefetch, 0, false); err != nil {
		return nil, amqpError(err, "")
	}
	tag := fmt.Sprintf("patchman-read-%d", time.Now().UnixNano())
	dels, err := ch.Consume(p.Queue, tag, false, false, false, false, amqp.Table{"x-stream-offset": streamOffset(p.Offset)})
	if err != nil {
		return nil, amqpError(err, "")
	}
	out := []Message{}
	overall := time.NewTimer(8 * time.Second)
	defer overall.Stop()
	idle := time.NewTimer(3 * time.Second)
	defer idle.Stop()
	for len(out) < p.Count {
		select {
		case <-ctx.Done():
			return out, nil
		case <-overall.C:
			_ = ch.Cancel(tag, false)
			return out, nil
		case <-idle.C:
			_ = ch.Cancel(tag, false)
			return out, nil
		case d, ok := <-dels:
			if !ok {
				return out, nil
			}
			out = append(out, toMessage(d))
			// Streams need acks for credit; nothing is removed.
			_ = d.Ack(false)
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(time.Second)
		}
	}
	_ = ch.Cancel(tag, false)
	return out, nil
}

// ── live tail ───────────────────────────────────────────────────────

// TailParams: a temporary exclusive queue is bound to Exchange with
// RoutingKey (and headers-exchange Arguments) and consumed until the client
// goes away. Existing queues are never touched.
type TailParams struct {
	Vhost      string
	Exchange   string
	RoutingKey string
	Arguments  map[string]any
}

const (
	tailMaxDuration = 30 * time.Minute
	tailQueueMax    = 10000
)

// Tail calls emit for every message and ping every 15s (keeps proxies from
// timing the stream out). It returns when ctx ends, emit fails or the broker
// closes the channel.
func (r *Registry) Tail(ctx context.Context, conn models.RabbitConnection, p TailParams, emit func(Message) error, ping func() error) error {
	if p.Exchange == "" {
		return &APIError{Code: "bad_request", Message: "Pick an exchange to tail — the default exchange can't be bound to."}
	}
	ch, err := r.channel(ctx, conn, p.Vhost)
	if err != nil {
		return err
	}
	defer ch.Close()
	q, err := ch.QueueDeclare("", false, true, true, false, amqp.Table{
		"x-queue-type": "classic",
		"x-max-length": int32(tailQueueMax),
	})
	if err != nil {
		return amqpError(err, "")
	}
	args := amqp.Table{}
	for k, v := range p.Arguments {
		args[k] = v
	}
	if err := ch.QueueBind(q.Name, p.RoutingKey, p.Exchange, false, args); err != nil {
		return amqpError(err, "")
	}
	dels, err := ch.Consume(q.Name, "patchman-tail", true, true, false, false, nil)
	if err != nil {
		return amqpError(err, "")
	}
	closed := ch.NotifyClose(make(chan *amqp.Error, 1))
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	deadline := time.NewTimer(tailMaxDuration)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-deadline.C:
			return nil
		case e := <-closed:
			if e != nil {
				return amqpError(e, "")
			}
			return nil
		case <-tick.C:
			// Keep the pooled AMQP connection out of the idle reaper while
			// the tail runs (it only counts calls that go through state()).
			_, _ = r.state(conn)
			if err := ping(); err != nil {
				return nil
			}
		case d, ok := <-dels:
			if !ok {
				return nil
			}
			m := toMessage(d)
			m.ReceivedAt = time.Now().UnixMilli()
			if err := emit(m); err != nil {
				return nil
			}
		}
	}
}

// ── move messages ──────────────────────────────────────────────────

// MoveParams moves up to Count messages out of Source:
//   - Target "queue":    to queue TargetQueue (via the default exchange);
//   - Target "exchange": to TargetExchange with TargetRoutingKey (empty =
//     keep each message's routing key);
//   - Target "origin":   back where it was dead-lettered from (x-death).
//
// Each message is published with confirms, then acked on the source; a
// failure requeues it and stops. An interruption between publish and ack
// can duplicate one message — never lose it.
type MoveParams struct {
	Vhost            string `json:"vhost"`
	Source           string `json:"source"`
	Target           string `json:"target"`
	TargetQueue      string `json:"targetQueue"`
	TargetExchange   string `json:"targetExchange"`
	TargetRoutingKey string `json:"targetRoutingKey"`
	Count            int    `json:"count"`
}

type MoveResult struct {
	Moved   int    `json:"moved"`
	Stopped string `json:"stopped,omitempty"`
}

const moveMax = 10000

func deathOrigin(h amqp.Table) (string, string, bool) {
	deaths, ok := h["x-death"].([]any)
	if !ok || len(deaths) == 0 {
		return "", "", false
	}
	d, ok := deaths[0].(amqp.Table)
	if !ok {
		return "", "", false
	}
	ex, _ := d["exchange"].(string)
	rk := ""
	if keys, ok := d["routing-keys"].([]any); ok && len(keys) > 0 {
		rk, _ = keys[0].(string)
	}
	return ex, rk, true
}

func (r *Registry) Move(ctx context.Context, conn models.RabbitConnection, p MoveParams) (*MoveResult, error) {
	if p.Count <= 0 || p.Count > moveMax {
		p.Count = moveMax
	}
	switch p.Target {
	case "queue":
		if p.TargetQueue == "" {
			return nil, &APIError{Code: "bad_request", Message: "Target queue is required."}
		}
		if p.TargetQueue == p.Source {
			return nil, &APIError{Code: "bad_request", Message: "Source and target are the same queue."}
		}
	case "exchange":
		if p.TargetExchange == "" {
			return nil, &APIError{Code: "bad_request", Message: "Target exchange is required."}
		}
	case "origin":
	default:
		return nil, &APIError{Code: "bad_request", Message: "target must be queue, exchange or origin"}
	}
	ch, err := r.channel(ctx, conn, p.Vhost)
	if err != nil {
		return nil, err
	}
	defer ch.Close()
	if err := ch.Confirm(false); err != nil {
		return nil, amqpError(err, "")
	}
	confirms := ch.NotifyPublish(make(chan amqp.Confirmation, 1))
	returns := ch.NotifyReturn(make(chan amqp.Return, 1))
	res := &MoveResult{}
	for res.Moved < p.Count {
		if ctx.Err() != nil {
			res.Stopped = "cancelled"
			break
		}
		d, ok, err := ch.Get(p.Source, false)
		if err != nil {
			return res, amqpError(err, "")
		}
		if !ok {
			break
		}
		ex, rk := "", ""
		switch p.Target {
		case "queue":
			ex, rk = "", p.TargetQueue
		case "exchange":
			ex, rk = p.TargetExchange, p.TargetRoutingKey
			if rk == "" {
				rk = d.RoutingKey
			}
		case "origin":
			oe, ork, ok := deathOrigin(d.Headers)
			if !ok {
				_ = d.Nack(false, true)
				res.Stopped = "A message has no x-death header, so its origin is unknown — move it to a queue or exchange instead."
				return res, nil
			}
			ex, rk = oe, ork
		}
		pub := amqp.Publishing{
			Headers: d.Headers, ContentType: d.ContentType, ContentEncoding: d.ContentEncoding,
			DeliveryMode: d.DeliveryMode, Priority: d.Priority, CorrelationId: d.CorrelationId, ReplyTo: d.ReplyTo,
			Expiration: d.Expiration, MessageId: d.MessageId, Timestamp: d.Timestamp, Type: d.Type,
			UserId: d.UserId, AppId: d.AppId, Body: d.Body,
		}
		if err := ch.PublishWithContext(ctx, ex, rk, true, false, pub); err != nil {
			_ = d.Nack(false, true)
			return res, amqpError(err, "")
		}
		var returned bool
		select {
		case <-returns:
			returned = true
			c := <-confirms
			_ = c
		case c := <-confirms:
			if !c.Ack {
				_ = d.Nack(false, true)
				res.Stopped = "RabbitMQ did not confirm the publish; the message stayed in the source queue."
				return res, nil
			}
		case <-time.After(15 * time.Second):
			_ = d.Nack(false, true)
			res.Stopped = "No publish confirm within 15s; the message stayed in the source queue."
			return res, nil
		}
		if returned {
			_ = d.Nack(false, true)
			res.Stopped = fmt.Sprintf("The target doesn't route the message (exchange %q, routing key %q); it stayed in the source queue.", ex, rk)
			return res, nil
		}
		if err := d.Ack(false); err != nil {
			return res, amqpError(err, "")
		}
		res.Moved++
	}
	return res, nil
}
