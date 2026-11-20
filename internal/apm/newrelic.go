package apm

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/servicemap"
)

// ── New Relic ──────────────────────────────────────────────────────────
// Everything goes through NerdGraph (POST <base>/graphql, header API-Key: a
// user key, NRAK-…): https://api.newrelic.com (US) or
// https://api.eu.newrelic.com (EU). The connection scope is the account id.
//
// The map: APM applications (entitySearch) are the nodes, with health from
// their alert severity and RED stats from NRQL over Transaction; their CALLS
// relationships (relatedEntities) are the edges. relatedEntities' filter
// arguments aren't documented, so all relationships are read and CALLS kept.
type newRelicProvider struct{ base }

func init() { register(newRelicProvider{}) }

func (newRelicProvider) Name() string         { return "newrelic" }
func (newRelicProvider) Title() string        { return "New Relic" }
func (newRelicProvider) AuthKinds() []string  { return []string{"apikey"} }
func (newRelicProvider) APIKeyHeader() string { return "API-Key" }

// nerdGraph runs one GraphQL request and decodes its data into out.
func nerdGraph(ctx context.Context, c *Client, query string, vars map[string]any, out any) error {
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	// The URL may be the API host or the full …/graphql endpoint.
	path := "/graphql"
	if strings.HasSuffix(strings.ToLower(c.Base), "/graphql") {
		path = ""
	}
	if err := c.Do(ctx, http.MethodPost, path, nil, map[string]any{"query": query, "variables": vars}, &resp); err != nil {
		return err
	}
	if len(resp.Errors) > 0 {
		msgs := make([]string, 0, len(resp.Errors))
		for _, e := range resp.Errors {
			msgs = append(msgs, e.Message)
		}
		return &APIError{http.StatusBadGateway, "upstream", "New Relic: " + strings.Join(msgs, "; ")}
	}
	if out == nil || len(resp.Data) == 0 {
		return nil
	}
	return json.Unmarshal(resp.Data, out)
}

func (newRelicProvider) Test(ctx context.Context, c *Client) (*TestResult, error) {
	var d struct {
		Actor struct {
			User struct {
				Email string `json:"email"`
				Name  string `json:"name"`
			} `json:"user"`
			Accounts []struct {
				ID   int    `json:"id"`
				Name string `json:"name"`
			} `json:"accounts"`
		} `json:"actor"`
	}
	if err := nerdGraph(ctx, c, `{ actor { user { email name } accounts { id name } } }`, nil, &d); err != nil {
		return nil, err
	}
	res := &TestResult{OK: true, User: firstNonEmpty(d.Actor.User.Name, d.Actor.User.Email)}
	for _, a := range d.Actor.Accounts {
		res.Scopes = append(res.Scopes, Scope{ID: strconv.Itoa(a.ID), Name: a.Name})
	}
	res.Notes = append(res.Notes, "accounts:"+strconv.Itoa(len(d.Actor.Accounts)))
	return res, nil
}

func (p newRelicProvider) Scopes(ctx context.Context, c *Client) ([]Scope, error) {
	r, err := p.Test(ctx, c)
	if err != nil {
		return nil, err
	}
	return r.Scopes, nil
}

// NewRelicAccount is the account id a call runs against: the request's, else
// the connection scope.
func NewRelicAccount(c *Client, requested string) (int, error) {
	s := strings.TrimSpace(firstNonEmpty(requested, c.Conn.Scope))
	if s == "" {
		return 0, &APIError{http.StatusBadRequest, "unsupported", "Pick the New Relic account (its id) on the connection or pass accountId."}
	}
	id, err := strconv.Atoi(s)
	if err != nil || id <= 0 {
		return 0, &APIError{http.StatusBadRequest, "unsupported", "The New Relic account id must be a number."}
	}
	return id, nil
}

// NewRelicNRQL runs one NRQL query (read-only by nature) on an account and
// returns its result rows.
func NewRelicNRQL(ctx context.Context, c *Client, account int, nrql string) ([]map[string]any, error) {
	nrql = strings.TrimSpace(nrql)
	if nrql == "" {
		return nil, &APIError{http.StatusBadRequest, "unsupported", "query is required"}
	}
	var d struct {
		Actor struct {
			Account struct {
				NRQL struct {
					Results []map[string]any `json:"results"`
				} `json:"nrql"`
			} `json:"account"`
		} `json:"actor"`
	}
	q := `query($id: Int!, $q: Nrql!) { actor { account(id: $id) { nrql(query: $q, timeout: 60) { results } } } }`
	if err := nerdGraph(ctx, c, q, map[string]any{"id": account, "q": nrql}, &d); err != nil {
		return nil, err
	}
	if d.Actor.Account.NRQL.Results == nil {
		return []map[string]any{}, nil
	}
	return d.Actor.Account.NRQL.Results, nil
}

// NewRelicEntity is one entity from entitySearch.
type NewRelicEntity struct {
	GUID          string `json:"guid"`
	Name          string `json:"name"`
	EntityType    string `json:"entityType"`
	Domain        string `json:"domain"`
	AccountID     int    `json:"accountId"`
	Reporting     bool   `json:"reporting"`
	AlertSeverity string `json:"alertSeverity,omitempty"`
}

// NewRelicEntities searches entities with New Relic's entity search syntax
// (e.g. "domainType IN ('APM-APPLICATION')", "name LIKE 'checkout'"); a
// plain word is a name search. At most max entities (≤ 600).
func NewRelicEntities(ctx context.Context, c *Client, query string, max int) ([]NewRelicEntity, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		query = "domainType IN ('APM-APPLICATION')"
	} else if !strings.ContainsAny(query, "='()") && !strings.Contains(strings.ToUpper(query), " LIKE ") {
		query = "name LIKE '" + strings.ReplaceAll(query, "'", "") + "'"
	}
	if max <= 0 || max > 600 {
		max = 200
	}
	gql := `query($q: String!, $cursor: String) { actor { entitySearch(query: $q) { results(cursor: $cursor) { nextCursor entities { guid name entityType domain accountId reporting ... on AlertableEntityOutline { alertSeverity } } } } } }`
	var out []NewRelicEntity
	var cursor any
	for page := 0; page < 3 && len(out) < max; page++ {
		var d struct {
			Actor struct {
				EntitySearch struct {
					Results struct {
						NextCursor *string          `json:"nextCursor"`
						Entities   []NewRelicEntity `json:"entities"`
					} `json:"results"`
				} `json:"entitySearch"`
			} `json:"actor"`
		}
		if err := nerdGraph(ctx, c, gql, map[string]any{"q": query, "cursor": cursor}, &d); err != nil {
			return nil, err
		}
		out = append(out, d.Actor.EntitySearch.Results.Entities...)
		if d.Actor.EntitySearch.Results.NextCursor == nil || *d.Actor.EntitySearch.Results.NextCursor == "" {
			break
		}
		cursor = *d.Actor.EntitySearch.Results.NextCursor
	}
	if len(out) > max {
		out = out[:max]
	}
	if out == nil {
		out = []NewRelicEntity{}
	}
	return out, nil
}

// NewRelicAlerts lists alert incidents of the last `since` (NRQL over
// NrAiIncident): open ones first. openOnly drops the closed ones.
func NewRelicAlerts(ctx context.Context, c *Client, account int, since time.Duration, openOnly bool) ([]map[string]any, error) {
	return newRelicAlertsWhere(ctx, c, account, since, openOnly, "")
}

// newRelicAlertsWhere is NewRelicAlerts narrowed by an NRQL WHERE clause
// (e.g. one entity's incidents).
func newRelicAlertsWhere(ctx context.Context, c *Client, account int, since time.Duration, openOnly bool, where string) ([]map[string]any, error) {
	if since < time.Hour {
		since = 24 * time.Hour
	}
	nrql := fmt.Sprintf("SELECT latest(title) AS title, latest(priority) AS priority, latest(event) AS state, latest(entity.name) AS entity, latest(conditionName) AS condition, latest(policyName) AS policy, min(timestamp) AS openedAt FROM NrAiIncident %s FACET incidentId SINCE %d hours ago LIMIT 200", where, int(math.Ceil(since.Hours())))
	rows, err := NewRelicNRQL(ctx, c, account, nrql)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		if openOnly && fmt.Sprint(r["state"]) != "open" {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return fmt.Sprint(out[i]["state"]) == "open" && fmt.Sprint(out[j]["state"]) != "open"
	})
	return out, nil
}

func (newRelicProvider) Map(ctx context.Context, c *Client, req MapRequest, b *servicemap.Builder, st *servicemap.SourceStatus) error {
	account := strings.TrimSpace(firstNonEmpty(req.Scope, c.Conn.Scope))
	q := "domainType IN ('APM-APPLICATION')"
	if account != "" {
		if _, err := strconv.Atoi(account); err != nil {
			return &APIError{http.StatusBadRequest, "unsupported", "The New Relic account id must be a number."}
		}
		q += " AND accountId = " + account
	}
	apps, err := NewRelicEntities(ctx, c, q, 200)
	if err != nil {
		return err
	}
	ids := map[string]string{} // guid → node id
	for _, e := range apps {
		id := b.ServiceNode("", e.Name, "newrelic")
		b.AddRef(id, "newrelic", e.Name, "", e.GUID)
		if n := b.Node(id); n != nil {
			n.Health = worseHealth(n.Health, nrHealth(e.AlertSeverity, e.Reporting))
		}
		ids[e.GUID] = id
	}

	// RED per application from Transaction events (needs an account).
	if account != "" && len(apps) > 0 {
		acc, _ := strconv.Atoi(account)
		mins := int(math.Ceil(req.Range.Minutes()))
		if mins < 5 {
			mins = 30
		}
		rows, err := NewRelicNRQL(ctx, c, acc, fmt.Sprintf("SELECT rate(count(*), 1 second) AS rps, percentage(count(*), WHERE error IS true) AS errPct, percentile(duration, 95) AS p95 FROM Transaction FACET appName SINCE %d minutes ago LIMIT MAX", mins))
		if err != nil {
			st.Notes = append(st.Notes, "stats_failed")
		} else {
			for _, r := range rows {
				name := fmt.Sprint(firstVal(r, "appName", "facet"))
				n := b.Node(b.ServiceNode("", name, "newrelic"))
				if n == nil {
					continue
				}
				n.Stats = servicemap.Stats{RPS: num(r["rps"]), ErrorRate: num(r["errPct"]) / 100, P95Ms: num(r["p95"]) * 1000, HasRED: true}
			}
		}
	} else if account == "" {
		st.Notes = append(st.Notes, "account_required_for_stats")
	}

	// CALLS relationships, 25 entities per request.
	guids := make([]string, 0, len(apps))
	for _, e := range apps {
		guids = append(guids, e.GUID)
	}
	gql := `query($guids: [EntityGuid]!) { actor { entities(guids: $guids) { guid relatedEntities { results { type source { entity { guid name entityType } } target { entity { guid name entityType } } } } } } }`
	for i := 0; i < len(guids); i += 25 {
		end := i + 25
		if end > len(guids) {
			end = len(guids)
		}
		var d struct {
			Actor struct {
				Entities []struct {
					RelatedEntities struct {
						Results []struct {
							Type   string `json:"type"`
							Source struct {
								Entity nrRef `json:"entity"`
							} `json:"source"`
							Target struct {
								Entity nrRef `json:"entity"`
							} `json:"target"`
						} `json:"results"`
					} `json:"relatedEntities"`
				} `json:"entities"`
			} `json:"actor"`
		}
		if err := nerdGraph(ctx, c, gql, map[string]any{"guids": guids[i:end]}, &d); err != nil {
			st.Notes = append(st.Notes, "relationships_failed")
			break
		}
		for _, e := range d.Actor.Entities {
			for _, r := range e.RelatedEntities.Results {
				if !strings.EqualFold(r.Type, "CALLS") {
					continue
				}
				from := nodeFor(b, ids, r.Source.Entity)
				to := nodeFor(b, ids, r.Target.Entity)
				if from != "" && to != "" {
					b.Link(from, to, "request", "http", "newrelic", servicemap.Stats{}, nil)
				}
			}
		}
	}
	st.Notes = append(st.Notes, "no_edge_rates")
	return nil
}

type nrRef struct {
	GUID       string `json:"guid"`
	Name       string `json:"name"`
	EntityType string `json:"entityType"`
}

// nodeFor is the map node of a relationship end: a known application, or a
// new service node for an entity outside the search (another account, an
// external service).
func nodeFor(b *servicemap.Builder, ids map[string]string, e nrRef) string {
	if id, ok := ids[e.GUID]; ok {
		return id
	}
	if e.Name == "" {
		return ""
	}
	id := b.ServiceNode("", e.Name, "newrelic")
	ids[e.GUID] = id
	return id
}

func nrHealth(severity string, reporting bool) string {
	switch strings.ToUpper(severity) {
	case "CRITICAL":
		return "critical"
	case "WARNING":
		return "warning"
	case "NOT_ALERTING":
		if reporting {
			return "ok"
		}
	}
	return "unknown"
}

func worseHealth(a, b string) string {
	rank := map[string]int{"": 0, "unknown": 0, "ok": 1, "warning": 2, "critical": 3}
	if rank[b] > rank[a] {
		return b
	}
	if a == "" {
		return "unknown"
	}
	return a
}

func firstVal(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			if s, ok := v.([]any); ok && len(s) > 0 {
				return s[0]
			}
			return v
		}
	}
	return ""
}

// num reads an NRQL value: a number, or a percentile map ({"95": 0.12}).
func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case map[string]any:
		for _, inner := range x {
			if f, ok := inner.(float64); ok {
				return f
			}
		}
	}
	return 0
}
