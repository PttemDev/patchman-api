package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"patchman-be/internal/apm"
	"patchman-be/internal/k8sclient"
	"patchman-be/internal/models"
	"patchman-be/internal/servicemap"
	"patchman-be/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ServiceMapHandler builds the merged service map: the K8s config layer
// (+ the cluster's own Prometheus), any number of observability
// connections, and the user's saved DB / queue connections matched onto
// the nodes they point at. Read-only everywhere.
type ServiceMapHandler struct {
	db       *gorm.DB
	cipher   *services.TokenCipher
	k8sReg   *k8sclient.Registry
	k8sConns *K8sConnectionHandler
	apmReg   *apm.Registry
	obs      *ObservabilityConnectionHandler

	mu    sync.Mutex
	cache map[string]serviceMapCacheEntry
}

type serviceMapCacheEntry struct {
	at   time.Time
	body gin.H
}

const serviceMapCacheTTL = 30 * time.Second

func NewServiceMapHandler(db *gorm.DB, cipher *services.TokenCipher, k8sReg *k8sclient.Registry, k8sConns *K8sConnectionHandler, apmReg *apm.Registry, obs *ObservabilityConnectionHandler) *ServiceMapHandler {
	return &ServiceMapHandler{db: db, cipher: cipher, k8sReg: k8sReg, k8sConns: k8sConns, apmReg: apmReg, obs: obs, cache: map[string]serviceMapCacheEntry{}}
}

// ServiceMapRequest is the parsed query of GET /service-map.
type ServiceMapRequest struct {
	K8s         string
	Context     string
	Namespaces  []string
	ClusterProm bool
	APM         []struct{ ID, Scope string } // id[@scope]
	Range       time.Duration
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseRange(s string) time.Duration {
	switch s {
	case "5m":
		return 5 * time.Minute
	case "1h":
		return time.Hour
	case "6h":
		return 6 * time.Hour
	case "24h":
		return 24 * time.Hour
	}
	return 15 * time.Minute
}

func parseServiceMapRequest(q url.Values) ServiceMapRequest {
	r := ServiceMapRequest{
		K8s: strings.TrimSpace(q.Get("k8s")), Context: strings.TrimSpace(q.Get("context")),
		Namespaces: splitList(q.Get("namespaces")), ClusterProm: q.Get("clusterProm") == "1" || q.Get("clusterProm") == "true",
		Range: parseRange(q.Get("range")),
	}
	for _, item := range splitList(q.Get("apm")) {
		id, scope, _ := strings.Cut(item, "@")
		r.APM = append(r.APM, struct{ ID, Scope string }{strings.TrimSpace(id), strings.TrimSpace(scope)})
	}
	if len(r.APM) > 6 {
		r.APM = r.APM[:6]
	}
	return r
}

// Get: GET /service-map?k8s=&context=&namespaces=&clusterProm=1&apm=id[@scope],…&range=15m
func (h *ServiceMapHandler) Get(c *gin.Context) {
	userID := c.GetString("userID")
	q := c.Request.URL.Query()
	fresh := q.Get("fresh") == "1"
	q.Del("fresh")
	key := userID + "|" + q.Encode()
	if !fresh {
		h.mu.Lock()
		e, ok := h.cache[key]
		h.mu.Unlock()
		if ok && time.Since(e.at) < serviceMapCacheTTL {
			c.JSON(http.StatusOK, e.body)
			return
		}
	}
	req := parseServiceMapRequest(q)
	if req.K8s == "" && len(req.APM) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "pick a Kubernetes connection or an observability connection"})
		return
	}
	g, status, err := h.Build(c.Request.Context(), userID, req)
	if err != nil {
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	body := gin.H{"data": g}
	h.mu.Lock()
	h.cache[key] = serviceMapCacheEntry{at: time.Now(), body: body}
	for k, e := range h.cache {
		if time.Since(e.at) > 5*serviceMapCacheTTL {
			delete(h.cache, k)
		}
	}
	h.mu.Unlock()
	c.JSON(http.StatusOK, body)
}

// Build assembles the map (also used by the MCP tool through the HTTP API).
func (h *ServiceMapHandler) Build(parent context.Context, userID string, req ServiceMapRequest) (*servicemap.Graph, int, error) {
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	b := servicemap.NewBuilder()
	sources := map[string]*servicemap.SourceStatus{}
	mreq := apm.MapRequest{Namespaces: req.Namespaces, Range: req.Range, End: time.Now().UTC()}

	if req.K8s != "" {
		conn, _, err := h.k8sConns.loadAccessible(req.K8s, userID)
		if err != nil {
			return nil, http.StatusNotFound, errors.New("Kubernetes connection not found")
		}
		st := &servicemap.SourceStatus{OK: true}
		sources["config"] = st
		kctx, kcancel := context.WithTimeout(ctx, 20*time.Second)
		in, notes, err := h.k8sReg.ServiceMapInput(kctx, *conn, req.Context, req.Namespaces)
		kcancel()
		st.Notes = append(st.Notes, notes...)
		if err != nil {
			st.OK, st.Error = false, err.Error()
		} else {
			servicemap.BuildConfig(b, in, st)
		}
		if req.ClusterProm {
			ps := &servicemap.SourceStatus{OK: true}
			sources["cluster-prometheus"] = ps
			pctx, pcancel := context.WithTimeout(ctx, 20*time.Second)
			q, target, found, err := h.k8sReg.ClusterPrometheus(pctx, *conn, req.Context)
			switch {
			case err != nil:
				ps.OK, ps.Error = false, err.Error()
			case !found:
				ps.OK, ps.Notes = false, []string{"prometheus_not_found"}
			default:
				ps.Notes = append(ps.Notes, "target:"+target)
				if err := apm.MapFromPrometheus(pctx, q, mreq, b, ps, "prometheus"); err != nil {
					ps.OK, ps.Error = false, err.Error()
				}
			}
			pcancel()
		}
	}

	for _, a := range req.APM {
		conn, role, err := h.obs.loadAccessible(a.ID, userID)
		if err != nil {
			sources["apm:"+a.ID] = &servicemap.SourceStatus{Error: "connection not found"}
			continue
		}
		st := &servicemap.SourceStatus{OK: true}
		sources["apm:"+a.ID] = st
		cl, err := h.apmReg.Client(*conn)
		if err == nil {
			actx, acancel := context.WithTimeout(ctx, 25*time.Second)
			r := mreq
			r.Scope = a.Scope
			err = cl.Provider.Map(actx, cl, r, b, st)
			acancel()
		}
		if err != nil {
			st.OK, st.Error = false, err.Error()
			if ae, ok := apm.AsAPIError(err); ok {
				st.Error = ae.Message
			}
		}
		h.obs.auditCtx(userID, conn, role, "map", a.Scope, err)
	}

	b.AttachConnections(h.savedConns(userID))
	b.FinishHealth()
	g := b.Graph(sources)
	if len(req.Namespaces) > 0 {
		g = filterNamespaces(g, req.Namespaces)
	}
	return g, http.StatusOK, nil
}

// filterNamespaces keeps nodes in the namespaces (or without one) plus the
// direct neighbours of in-namespace nodes, so cross-namespace calls stay.
func filterNamespaces(g *servicemap.Graph, namespaces []string) *servicemap.Graph {
	in := map[string]bool{}
	for _, ns := range namespaces {
		in[ns] = true
	}
	keep := map[string]bool{}
	core := map[string]bool{}
	for _, n := range g.Nodes {
		if in[n.Namespace] {
			core[n.ID], keep[n.ID] = true, true
		}
	}
	for _, e := range g.Edges {
		if core[e.From] || core[e.To] {
			keep[e.From], keep[e.To] = true, true
		}
	}
	out := &servicemap.Graph{Sources: g.Sources}
	for _, n := range g.Nodes {
		if keep[n.ID] {
			out.Nodes = append(out.Nodes, n)
		}
	}
	for _, e := range g.Edges {
		if keep[e.From] && keep[e.To] {
			out.Edges = append(out.Edges, e)
		}
	}
	if out.Nodes == nil {
		out.Nodes = []*servicemap.Node{}
	}
	if out.Edges == nil {
		out.Edges = []*servicemap.Edge{}
	}
	return out
}

func (h *ObservabilityConnectionHandler) auditCtx(userID string, conn *models.ObservabilityConnection, role, action, target string, actErr error) {
	var delegated *string
	if role != "owner" {
		owner := conn.UserID
		delegated = &owner
	}
	errText := ""
	if actErr != nil {
		errText = actErr.Error()
	}
	row := models.ObservabilityActivityHistory{
		ID: uuid.New().String(), UserID: userID, DelegatedOwner: delegated, ConnectionID: conn.ID,
		Action: action, Target: target, ErrorText: errText, ExecutedAt: time.Now(),
	}
	go func() { _ = h.db.Create(&row).Error }()
}

// accessibleWhere is the owner-or-grant filter for a tool's connections.
func accessibleWhere(grantTable string) string {
	return fmt.Sprintf(`user_id = ? OR id IN (
		SELECT g.connection_id FROM %s g
		WHERE (g.subject_kind = 'user' AND g.subject_id = ?)
		   OR (g.subject_kind = 'team' AND g.subject_id IN (SELECT tm.team_id FROM team_members tm WHERE tm.user_id = ?)))`, grantTable)
}

// savedConns lists the user's DB / Kafka / RabbitMQ / Redis / ES / S3
// connections with the addresses they point at.
func (h *ServiceMapHandler) savedConns(userID string) []servicemap.SavedConn {
	var out []servicemap.SavedConn
	args := []any{userID, userID, userID}
	hostPort := func(s string, defPort int) []servicemap.Endpoint {
		var eps []servicemap.Endpoint
		for _, part := range splitList(s) {
			if strings.Contains(part, "://") {
				eps = append(eps, servicemap.ExtractEndpoints("", part, nil)...)
				continue
			}
			if e := servicemap.ExtractEndpoints("", part, nil); len(e) > 0 {
				eps = append(eps, e...)
				continue
			}
			eps = append(eps, servicemap.Endpoint{Host: strings.ToLower(part), Port: defPort})
		}
		return eps
	}

	var dbs []models.DBConnection
	h.db.Where(accessibleWhere("db_connection_grants"), args...).Find(&dbs)
	for _, d := range dbs {
		sub := dbKindSubtype(d.Kind)
		eps := hostPort(d.Host, d.Port) // Mongo may list a replica set: h1,h2:27018
		if d.ConnectionStringEnc != "" && h.cipher != nil {
			if dsn, err := h.cipher.Decrypt(d.ConnectionStringEnc); err == nil {
				if e := servicemap.ExtractEndpoints("", dsn, nil); len(e) > 0 {
					eps = e // the DSN wins over a stale Host
				}
			}
		}
		out = append(out, servicemap.SavedConn{Ref: servicemap.ConnRef{Tool: "db", ID: d.ID, Name: d.Name}, Subtype: sub, Endpoints: eps})
	}
	var kafkas []models.KafkaConnection
	h.db.Where(accessibleWhere("kafka_connection_grants"), args...).Find(&kafkas)
	for _, k := range kafkas {
		out = append(out, servicemap.SavedConn{Ref: servicemap.ConnRef{Tool: "kafka", ID: k.ID, Name: k.Name}, Subtype: "kafka", Endpoints: hostPort(k.BootstrapServers, 9092)})
	}
	var rabbits []models.RabbitConnection
	h.db.Where(accessibleWhere("rabbit_connection_grants"), args...).Find(&rabbits)
	for _, r := range rabbits {
		var opt map[string]any
		_ = json.Unmarshal([]byte(r.OptionsJSON), &opt)
		host, _ := opt["amqpHost"].(string)
		if host == "" {
			if u, err := url.Parse(r.URL); err == nil {
				host = u.Hostname()
			}
		}
		port := 5672
		switch v := opt["amqpPort"].(type) {
		case string:
			if p, err := strconv.Atoi(v); err == nil {
				port = p
			}
		case float64:
			port = int(v)
		}
		eps := []servicemap.Endpoint{{Host: strings.ToLower(host), Port: port}}
		if u, err := url.Parse(r.URL); err == nil && u.Hostname() != "" && !strings.EqualFold(u.Hostname(), host) {
			eps = append(eps, servicemap.Endpoint{Host: strings.ToLower(u.Hostname()), Port: 5672})
		}
		out = append(out, servicemap.SavedConn{Ref: servicemap.ConnRef{Tool: "rabbit", ID: r.ID, Name: r.Name}, Subtype: "rabbitmq", Endpoints: eps})
	}
	var redises []models.RedisConnection
	h.db.Where(accessibleWhere("redis_connection_grants"), args...).Find(&redises)
	for _, r := range redises {
		out = append(out, servicemap.SavedConn{Ref: servicemap.ConnRef{Tool: "redis", ID: r.ID, Name: r.Name}, Subtype: "redis", Endpoints: hostPort(r.Address, 6379)})
	}
	var ess []models.ESConnection
	h.db.Where(accessibleWhere("es_connection_grants"), args...).Find(&ess)
	for _, e := range ess {
		out = append(out, servicemap.SavedConn{Ref: servicemap.ConnRef{Tool: "es", ID: e.ID, Name: e.Name}, Subtype: "elasticsearch", Endpoints: hostPort(e.Nodes, 9200)})
	}
	var s3s []models.S3Connection
	h.db.Where(accessibleWhere("s3_connection_grants"), args...).Find(&s3s)
	for _, s := range s3s {
		if strings.TrimSpace(s.Endpoint) != "" {
			out = append(out, servicemap.SavedConn{Ref: servicemap.ConnRef{Tool: "s3", ID: s.ID, Name: s.Name}, Subtype: "s3", Endpoints: hostPort(s.Endpoint, 443)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ref.Name < out[j].Ref.Name })
	return out
}

func dbKindSubtype(kind string) string {
	switch strings.ToLower(kind) {
	case "postgres", "postgresql", "cockroachdb", "yugabytedb", "redshift":
		return "postgres"
	case "mysql", "mariadb", "tidb":
		return "mysql"
	case "sqlserver", "mssql":
		return "mssql"
	case "mongo", "mongodb":
		return "mongo"
	}
	return strings.ToLower(kind)
}
