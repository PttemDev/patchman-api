package rabbitquery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"patchman-be/internal/models"
)

// Management API wrappers. Most reads are passed through as raw JSON (the FE
// types mirror the fields it shows); combined views (overview, queue detail,
// topology) are assembled here. vhost / names are always path-escaped here —
// the HTTP routes carry them as query params because gin would decode the
// default vhost's "%2F".

func vhostPath(vhost string) string { return url.PathEscape(vhost) }

// exchangePath maps the default exchange ("") to its management name.
func exchangePath(name string) string {
	if name == "" {
		name = "amq.default"
	}
	return url.PathEscape(name)
}

// samples asks for the last 10 minutes of message rates / queue lengths
// (charts); harmless when the broker keeps no stats.
func samples(q url.Values) url.Values {
	if q == nil {
		q = url.Values{}
	}
	q.Set("msg_rates_age", "600")
	q.Set("msg_rates_incr", "10")
	q.Set("lengths_age", "600")
	q.Set("lengths_incr", "10")
	return q
}

// Overview bundles /overview (with samples), /nodes, /vhosts and /whoami.
// Nodes need the monitoring tag; their error is reported, not fatal.
type Overview struct {
	Overview   json.RawMessage `json:"overview"`
	Nodes      json.RawMessage `json:"nodes,omitempty"`
	NodesError string          `json:"nodesError,omitempty"`
	Vhosts     json.RawMessage `json:"vhosts"`
	WhoAmI     json.RawMessage `json:"whoami,omitempty"`
}

func (r *Registry) Overview(ctx context.Context, conn models.RabbitConnection) (*Overview, error) {
	var (
		out            Overview
		wg             sync.WaitGroup
		ovErr, vhErr   error
		nodes, who, ov []byte
		vh             []byte
		nodesErr       error
	)
	wg.Add(4)
	go func() {
		defer wg.Done()
		ov, ovErr = r.DoRaw(ctx, conn, http.MethodGet, "/overview", samples(nil), nil, nil)
	}()
	go func() { defer wg.Done(); vh, vhErr = r.DoRaw(ctx, conn, http.MethodGet, "/vhosts", nil, nil, nil) }()
	go func() { defer wg.Done(); nodes, nodesErr = r.DoRaw(ctx, conn, http.MethodGet, "/nodes", nil, nil, nil) }()
	go func() { defer wg.Done(); who, _ = r.DoRaw(ctx, conn, http.MethodGet, "/whoami", nil, nil, nil) }()
	wg.Wait()
	if ovErr != nil {
		return nil, ovErr
	}
	if vhErr != nil {
		return nil, vhErr
	}
	out.Overview, out.Vhosts = ov, vh
	if nodesErr != nil {
		out.NodesError = nodesErr.Error()
	} else {
		out.Nodes = nodes
	}
	if len(who) > 0 {
		out.WhoAmI = who
	}
	return &out, nil
}

// TestResult is what the connection dialog shows.
type TestResult struct {
	Version     string   `json:"version"`
	ClusterName string   `json:"clusterName"`
	User        string   `json:"user"`
	Tags        []string `json:"tags"`
	Vhosts      []string `json:"vhosts"`
	// AMQP: "" when the probe succeeded, else why it failed (management
	// works without AMQP; only tail / stream reads / move need it).
	AMQPError string `json:"amqpError,omitempty"`
	AMQPOK    bool   `json:"amqpOk"`
}

func (r *Registry) Test(ctx context.Context, conn models.RabbitConnection) (*TestResult, error) {
	temp := conn.ID == ""
	if temp {
		conn.ID = fmt.Sprintf("%s%p", testIDPrefix, &conn)
		defer r.Close(conn.ID)
	} else {
		r.Close(conn.ID)
	}
	var ov struct {
		Version     string `json:"rabbitmq_version"`
		ClusterName string `json:"cluster_name"`
	}
	if err := r.Do(ctx, conn, http.MethodGet, "/overview", url.Values{"columns": {"rabbitmq_version,cluster_name"}}, nil, &ov); err != nil {
		return nil, err
	}
	var who struct {
		Name string `json:"name"`
		Tags any    `json:"tags"`
	}
	_ = r.Do(ctx, conn, http.MethodGet, "/whoami", nil, nil, &who)
	var vhosts []struct {
		Name string `json:"name"`
	}
	_ = r.Do(ctx, conn, http.MethodGet, "/vhosts", url.Values{"columns": {"name"}}, nil, &vhosts)
	res := &TestResult{Version: ov.Version, ClusterName: ov.ClusterName, User: who.Name, Tags: tagList(who.Tags), Vhosts: []string{}}
	for _, v := range vhosts {
		res.Vhosts = append(res.Vhosts, v.Name)
	}
	vhost := "/"
	if dv, _ := Options(conn)["defaultVhost"].(string); dv != "" {
		vhost = dv
	} else if len(res.Vhosts) > 0 && !contains(res.Vhosts, "/") {
		vhost = res.Vhosts[0]
	}
	if c, err := r.amqpConn(ctx, conn, vhost); err != nil {
		res.AMQPError = err.Error()
	} else {
		res.AMQPOK = !c.IsClosed()
	}
	return res, nil
}

// tagList accepts both the 3.x comma string and the 4.x array form.
func tagList(v any) []string {
	out := []string{}
	switch t := v.(type) {
	case string:
		for _, s := range strings.Split(t, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	case []any:
		for _, s := range t {
			if str, ok := s.(string); ok && str != "" {
				out = append(out, str)
			}
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ListParams are the paging/filter knobs of the queue / exchange /
// connection lists (management pagination: RabbitMQ 3.6+).
type ListParams struct {
	Vhost       string
	Page        int
	PageSize    int
	Name        string
	Regex       bool
	Sort        string
	SortReverse bool
}

func (p ListParams) values(columns string) url.Values {
	q := url.Values{}
	page := p.Page
	if page < 1 {
		page = 1
	}
	size := p.PageSize
	if size <= 0 || size > 500 {
		size = 100
	}
	q.Set("page", strconv.Itoa(page))
	q.Set("page_size", strconv.Itoa(size))
	if p.Name != "" {
		q.Set("name", p.Name)
		q.Set("use_regex", strconv.FormatBool(p.Regex))
	}
	if p.Sort != "" {
		q.Set("sort", p.Sort)
		q.Set("sort_reverse", strconv.FormatBool(p.SortReverse))
	}
	if columns != "" {
		q.Set("columns", columns)
	}
	return q
}

func scoped(kind, vhost string) string {
	if vhost == "" {
		return "/" + kind
	}
	return "/" + kind + "/" + vhostPath(vhost)
}

const queueColumns = "name,vhost,type,state,durable,auto_delete,exclusive,arguments,policy,consumers,consumer_utilisation," +
	"messages,messages_ready,messages_unacknowledged,message_bytes,memory,node,leader,members,online,delivery_limit," +
	"message_stats.publish_details.rate,message_stats.deliver_get_details.rate,message_stats.ack_details.rate," +
	"message_stats.redeliver_details.rate,effective_policy_definition,idle_since,single_active_consumer_tag"

func (r *Registry) Queues(ctx context.Context, conn models.RabbitConnection, p ListParams) (json.RawMessage, error) {
	return r.DoRaw(ctx, conn, http.MethodGet, scoped("queues", p.Vhost), p.values(queueColumns), nil, nil)
}

const exchangeColumns = "name,vhost,type,durable,auto_delete,internal,arguments,policy," +
	"message_stats.publish_in_details.rate,message_stats.publish_out_details.rate"

func (r *Registry) Exchanges(ctx context.Context, conn models.RabbitConnection, p ListParams) (json.RawMessage, error) {
	return r.DoRaw(ctx, conn, http.MethodGet, scoped("exchanges", p.Vhost), p.values(exchangeColumns), nil, nil)
}

func (r *Registry) Connections(ctx context.Context, conn models.RabbitConnection, p ListParams) (json.RawMessage, error) {
	// /connections has no per-vhost listing; filter client-side by vhost.
	q := p.values("")
	return r.DoRaw(ctx, conn, http.MethodGet, "/connections", q, nil, nil)
}

func (r *Registry) Channels(ctx context.Context, conn models.RabbitConnection, p ListParams) (json.RawMessage, error) {
	return r.DoRaw(ctx, conn, http.MethodGet, scoped("channels", p.Vhost), p.values(""), nil, nil)
}

func (r *Registry) Consumers(ctx context.Context, conn models.RabbitConnection, vhost string) (json.RawMessage, error) {
	return r.DoRaw(ctx, conn, http.MethodGet, scoped("consumers", vhost), nil, nil, nil)
}

func (r *Registry) Policies(ctx context.Context, conn models.RabbitConnection, vhost string) (json.RawMessage, error) {
	return r.DoRaw(ctx, conn, http.MethodGet, scoped("policies", vhost), nil, nil, nil)
}

// PeekInfo tells the FE what reading a queue's messages will do.
type PeekInfo struct {
	// requeue (classic/quorum: get + put back) | stream (AMQP offset read).
	Mode string `json:"mode"`
	// none | redelivered | delivery_limit — the last when a RabbitMQ 3.x
	// quorum queue has a delivery limit: each peek counts as a delivery and
	// messages read that many times are dead-lettered or dropped.
	Risk          string `json:"risk"`
	DeliveryLimit int    `json:"deliveryLimit,omitempty"`
	HasDLX        bool   `json:"hasDlx"`
	Version       string `json:"version"`
}

// QueueDetail bundles the queue (with samples) and its bindings.
type QueueDetail struct {
	Queue    json.RawMessage `json:"queue"`
	Bindings json.RawMessage `json:"bindings"`
	Peek     PeekInfo        `json:"peek"`
}

type queueMeta struct {
	Type          string         `json:"type"`
	Arguments     map[string]any `json:"arguments"`
	Effective     map[string]any `json:"effective_policy_definition"`
	DeliveryLimit any            `json:"delivery_limit"`
}

func (r *Registry) Queue(ctx context.Context, conn models.RabbitConnection, vhost, name string) (*QueueDetail, error) {
	base := "/queues/" + vhostPath(vhost) + "/" + url.PathEscape(name)
	q, err := r.DoRaw(ctx, conn, http.MethodGet, base, samples(nil), nil, nil)
	if err != nil {
		return nil, err
	}
	b, err := r.DoRaw(ctx, conn, http.MethodGet, base+"/bindings", nil, nil, nil)
	if err != nil {
		b = []byte("[]")
	}
	var m queueMeta
	_ = json.Unmarshal(q, &m)
	return &QueueDetail{Queue: q, Bindings: b, Peek: peekInfo(m, r.Version(ctx, conn))}, nil
}

func numArg(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case string:
		n, _ := strconv.Atoi(t)
		return n
	}
	return 0
}

func peekInfo(m queueMeta, version string) PeekInfo {
	p := PeekInfo{Mode: "requeue", Risk: "redelivered", Version: version}
	if m.Arguments == nil {
		m.Arguments = map[string]any{}
	}
	if m.Effective == nil {
		m.Effective = map[string]any{}
	}
	_, dlx := m.Arguments["x-dead-letter-exchange"]
	_, pdlx := m.Effective["dead-letter-exchange"]
	p.HasDLX = dlx || pdlx
	switch m.Type {
	case "stream":
		p.Mode, p.Risk = "stream", "none"
		return p
	case "quorum":
		limit := numArg(m.Arguments["x-delivery-limit"])
		if pl := numArg(m.Effective["delivery-limit"]); pl > 0 && (limit == 0 || pl < limit) {
			limit = pl
		}
		if limit == 0 {
			limit = numArg(m.DeliveryLimit)
		}
		p.DeliveryLimit = limit
		// RabbitMQ 4 counts requeued gets in x-acquired-count, not toward
		// the delivery limit (verified on 4.4); 3.x counts every get.
		if limit > 0 && Major(version) < 4 {
			p.Risk = "delivery_limit"
		}
	}
	return p
}

// ExchangeDetail bundles an exchange with its bindings in and out.
type ExchangeDetail struct {
	Exchange json.RawMessage `json:"exchange"`
	Source   json.RawMessage `json:"source"`
	Dest     json.RawMessage `json:"destination"`
}

func (r *Registry) Exchange(ctx context.Context, conn models.RabbitConnection, vhost, name string) (*ExchangeDetail, error) {
	base := "/exchanges/" + vhostPath(vhost) + "/" + exchangePath(name)
	e, err := r.DoRaw(ctx, conn, http.MethodGet, base, samples(nil), nil, nil)
	if err != nil {
		return nil, err
	}
	src, _ := r.DoRaw(ctx, conn, http.MethodGet, base+"/bindings/source", nil, nil, nil)
	dst, _ := r.DoRaw(ctx, conn, http.MethodGet, base+"/bindings/destination", nil, nil, nil)
	if src == nil {
		src = []byte("[]")
	}
	if dst == nil {
		dst = []byte("[]")
	}
	return &ExchangeDetail{Exchange: e, Source: src, Dest: dst}, nil
}

// Topology is a vhost's exchanges, queues and bindings (for the graph).
type Topology struct {
	Exchanges json.RawMessage `json:"exchanges"`
	Queues    json.RawMessage `json:"queues"`
	Bindings  json.RawMessage `json:"bindings"`
}

func (r *Registry) Topology(ctx context.Context, conn models.RabbitConnection, vhost string) (*Topology, error) {
	var (
		wg         sync.WaitGroup
		ex, qs, bs []byte
		e1, e2, e3 error
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		ex, e1 = r.DoRaw(ctx, conn, http.MethodGet, scoped("exchanges", vhost), url.Values{"columns": {"name,vhost,type,internal,durable,message_stats.publish_in_details.rate"}}, nil, nil)
	}()
	go func() {
		defer wg.Done()
		qs, e2 = r.DoRaw(ctx, conn, http.MethodGet, scoped("queues", vhost), url.Values{"columns": {"name,vhost,type,messages,consumers,arguments,state"}}, nil, nil)
	}()
	go func() {
		defer wg.Done()
		bs, e3 = r.DoRaw(ctx, conn, http.MethodGet, scoped("bindings", vhost), nil, nil, nil)
	}()
	wg.Wait()
	for _, err := range []error{e1, e2, e3} {
		if err != nil {
			return nil, err
		}
	}
	return &Topology{Exchanges: ex, Queues: qs, Bindings: bs}, nil
}

// ── messages over the management API ──────────────────────────────

// GetParams: count up to 500; Remove=true acks (destructive, write role).
type GetParams struct {
	Vhost    string
	Queue    string
	Count    int
	Remove   bool
	Truncate int
}

func (r *Registry) GetMessages(ctx context.Context, conn models.RabbitConnection, p GetParams) (json.RawMessage, error) {
	if p.Count <= 0 {
		p.Count = 10
	}
	if p.Count > 500 {
		p.Count = 500
	}
	ack := "ack_requeue_true"
	if p.Remove {
		ack = "ack_requeue_false"
	}
	body := map[string]any{"count": p.Count, "ackmode": ack, "encoding": "auto"}
	if p.Truncate > 0 {
		body["truncate"] = p.Truncate
	}
	return r.DoRaw(ctx, conn, http.MethodPost, "/queues/"+vhostPath(p.Vhost)+"/"+url.PathEscape(p.Queue)+"/get", nil, body, nil)
}

// PublishParams: Exchange "" = default exchange (routing key = queue name).
type PublishParams struct {
	Vhost           string         `json:"vhost"`
	Exchange        string         `json:"exchange"`
	RoutingKey      string         `json:"routingKey"`
	Payload         string         `json:"payload"`
	PayloadEncoding string         `json:"payloadEncoding"` // string | base64
	Properties      map[string]any `json:"properties"`
}

func (r *Registry) Publish(ctx context.Context, conn models.RabbitConnection, p PublishParams) (bool, error) {
	enc := p.PayloadEncoding
	if enc != "base64" {
		enc = "string"
	}
	props := p.Properties
	if props == nil {
		props = map[string]any{}
	}
	var res struct {
		Routed bool `json:"routed"`
	}
	err := r.Do(ctx, conn, http.MethodPost, "/exchanges/"+vhostPath(p.Vhost)+"/"+exchangePath(p.Exchange)+"/publish", nil,
		map[string]any{"properties": props, "routing_key": p.RoutingKey, "payload": p.Payload, "payload_encoding": enc}, &res)
	return res.Routed, err
}

func (r *Registry) Purge(ctx context.Context, conn models.RabbitConnection, vhost, queue string) error {
	_, err := r.DoRaw(ctx, conn, http.MethodDelete, "/queues/"+vhostPath(vhost)+"/"+url.PathEscape(queue)+"/contents", nil, nil, nil)
	return err
}

// QueueSpec / ExchangeSpec create queues and exchanges.
type QueueSpec struct {
	Vhost      string         `json:"vhost"`
	Name       string         `json:"name"`
	Durable    bool           `json:"durable"`
	AutoDelete bool           `json:"autoDelete"`
	Arguments  map[string]any `json:"arguments"`
}

func (r *Registry) CreateQueue(ctx context.Context, conn models.RabbitConnection, s QueueSpec) error {
	if strings.TrimSpace(s.Name) == "" {
		return &APIError{Code: "bad_request", Message: "Queue name is required."}
	}
	args := s.Arguments
	if args == nil {
		args = map[string]any{}
	}
	_, err := r.DoRaw(ctx, conn, http.MethodPut, "/queues/"+vhostPath(s.Vhost)+"/"+url.PathEscape(s.Name), nil,
		map[string]any{"durable": s.Durable, "auto_delete": s.AutoDelete, "arguments": args}, nil)
	return err
}

func (r *Registry) DeleteQueue(ctx context.Context, conn models.RabbitConnection, vhost, name string, ifEmpty, ifUnused bool) error {
	q := url.Values{}
	if ifEmpty {
		q.Set("if-empty", "true")
	}
	if ifUnused {
		q.Set("if-unused", "true")
	}
	_, err := r.DoRaw(ctx, conn, http.MethodDelete, "/queues/"+vhostPath(vhost)+"/"+url.PathEscape(name), q, nil, nil)
	return err
}

type ExchangeSpec struct {
	Vhost      string         `json:"vhost"`
	Name       string         `json:"name"`
	Type       string         `json:"type"`
	Durable    bool           `json:"durable"`
	AutoDelete bool           `json:"autoDelete"`
	Internal   bool           `json:"internal"`
	Arguments  map[string]any `json:"arguments"`
}

func (r *Registry) CreateExchange(ctx context.Context, conn models.RabbitConnection, s ExchangeSpec) error {
	if strings.TrimSpace(s.Name) == "" || strings.HasPrefix(s.Name, "amq.") {
		return &APIError{Code: "bad_request", Message: "Exchange name is required and may not start with \"amq.\"."}
	}
	if s.Type == "" {
		s.Type = "direct"
	}
	args := s.Arguments
	if args == nil {
		args = map[string]any{}
	}
	_, err := r.DoRaw(ctx, conn, http.MethodPut, "/exchanges/"+vhostPath(s.Vhost)+"/"+url.PathEscape(s.Name), nil,
		map[string]any{"type": s.Type, "durable": s.Durable, "auto_delete": s.AutoDelete, "internal": s.Internal, "arguments": args}, nil)
	return err
}

func (r *Registry) DeleteExchange(ctx context.Context, conn models.RabbitConnection, vhost, name string) error {
	if name == "" || strings.HasPrefix(name, "amq.") {
		return &APIError{Code: "bad_request", Message: "Built-in exchanges can't be deleted."}
	}
	_, err := r.DoRaw(ctx, conn, http.MethodDelete, "/exchanges/"+vhostPath(vhost)+"/"+url.PathEscape(name), nil, nil, nil)
	return err
}

// BindingSpec binds an exchange to a queue (DestType "q") or exchange ("e").
type BindingSpec struct {
	Vhost         string         `json:"vhost"`
	Source        string         `json:"source"`
	Destination   string         `json:"destination"`
	DestType      string         `json:"destinationType"` // queue | exchange
	RoutingKey    string         `json:"routingKey"`
	Arguments     map[string]any `json:"arguments"`
	PropertiesKey string         `json:"propertiesKey"` // for unbind
}

func (b BindingSpec) path() (string, error) {
	if b.Source == "" {
		return "", &APIError{Code: "bad_request", Message: "Bindings to the default exchange are implicit and can't be changed."}
	}
	d := "q"
	if b.DestType == "exchange" || b.DestType == "e" {
		d = "e"
	}
	if b.Destination == "" {
		return "", &APIError{Code: "bad_request", Message: "Destination is required."}
	}
	return "/bindings/" + vhostPath(b.Vhost) + "/e/" + url.PathEscape(b.Source) + "/" + d + "/" + url.PathEscape(b.Destination), nil
}

func (r *Registry) Bind(ctx context.Context, conn models.RabbitConnection, b BindingSpec) error {
	p, err := b.path()
	if err != nil {
		return err
	}
	args := b.Arguments
	if args == nil {
		args = map[string]any{}
	}
	_, err = r.DoRaw(ctx, conn, http.MethodPost, p, nil, map[string]any{"routing_key": b.RoutingKey, "arguments": args}, nil)
	return err
}

func (r *Registry) Unbind(ctx context.Context, conn models.RabbitConnection, b BindingSpec) error {
	p, err := b.path()
	if err != nil {
		return err
	}
	key := b.PropertiesKey
	if key == "" {
		key = "~"
		if b.RoutingKey != "" {
			key = b.RoutingKey
		}
	}
	_, err = r.DoRaw(ctx, conn, http.MethodDelete, p+"/"+url.PathEscape(key), nil, nil, nil)
	return err
}

// CloseConnection force-closes a client connection (X-Reason shown to it).
func (r *Registry) CloseConnection(ctx context.Context, conn models.RabbitConnection, name, reason string) error {
	if reason == "" {
		reason = "Closed from PatchMan"
	}
	_, err := r.DoRaw(ctx, conn, http.MethodDelete, "/connections/"+url.PathEscape(name), nil, nil, map[string]string{"X-Reason": reason})
	return err
}
