package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/dbquery"
	"patchman-be/internal/grafanaquery"
	"patchman-be/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// GrafanaOpsHandler serves the dashboard surface: server info, the folder /
// dashboard tree, a dashboard (v2 converted, library panels resolved),
// variable resolution, panel queries and the allowlisted Prometheus/Loki
// resource calls used for autocomplete. Any access (owner or grant) can
// read; a read grant may only run read-only raw SQL.
type GrafanaOpsHandler struct {
	db        *gorm.DB
	registry  *grafanaquery.Registry
	connsHand *GrafanaConnectionHandler
}

func NewGrafanaOpsHandler(db *gorm.DB, reg *grafanaquery.Registry, connsHand *GrafanaConnectionHandler) *GrafanaOpsHandler {
	return &GrafanaOpsHandler{db: db, registry: reg, connsHand: connsHand}
}

func (h *GrafanaOpsHandler) access(c *gin.Context) (*models.GrafanaConnection, string, bool) {
	conn, role, err := h.connsHand.loadAccessible(c.Param("id"), c.GetString("userID"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return nil, "", false
	}
	return conn, role, true
}

// fail maps a grafanaquery error to an HTTP answer. Grafana auth problems
// are 502 + code so the FE can show "update credentials / sign in again"
// instead of logging the PatchMan user out.
func (h *GrafanaOpsHandler) fail(c *gin.Context, err error) {
	if ae, ok := grafanaquery.AsAPIError(err); ok {
		status := http.StatusBadGateway
		if ae.Code == "not_found" {
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"error": ae.Message, "code": ae.Code})
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "Grafana did not answer in time"})
		return
	}
	c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
}

func sqlGuard(role string) grafanaquery.SQLGuard {
	if role == "owner" || role == "write" {
		return nil
	}
	return func(sql string) error {
		if !dbquery.IsReadOnlySQL(sql) {
			return fmt.Errorf("read-only access: only read queries (SELECT / WITH / SHOW / EXPLAIN) are allowed on this Grafana connection")
		}
		return nil
	}
}

func (h *GrafanaOpsHandler) Info(c *gin.Context) {
	conn, _, ok := h.access(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	info, err := h.registry.Info(ctx, *conn)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": info})
}

func (h *GrafanaOpsHandler) Search(c *gin.Context) {
	conn, role, ok := h.access(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	res, err := h.registry.Search(ctx, *conn, c.Query("query"))
	h.audit(c, conn, role, "search", c.Query("query"), err)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

func (h *GrafanaOpsHandler) Dashboard(c *gin.Context) {
	conn, role, ok := h.access(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	d, err := h.registry.GetDashboard(ctx, *conn, c.Param("uid"))
	h.audit(c, conn, role, "dashboard", c.Param("uid"), err)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": d})
}

type grafanaVariablesInput struct {
	UID        string                            `json:"uid"`
	Templating []any                             `json:"templating"`
	Selected   map[string]grafanaquery.Selection `json:"selected"`
	From       string                            `json:"from"`
	To         string                            `json:"to"`
}

func (h *GrafanaOpsHandler) Variables(c *gin.Context) {
	conn, _, ok := h.access(c)
	if !ok {
		return
	}
	var in grafanaVariablesInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 45*time.Second)
	defer cancel()
	templating := in.Templating
	from, to := in.From, in.To
	if len(templating) == 0 && in.UID != "" {
		d, err := h.registry.GetDashboard(ctx, *conn, in.UID)
		if err != nil {
			h.fail(c, err)
			return
		}
		templating = templatingList(d.Dashboard)
		if from == "" {
			from, to = dashboardTime(d.Dashboard)
		}
	}
	rng, err := grafanaquery.ParseRange(from, to, time.Now())
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	vars, err := h.registry.ResolveVariables(ctx, *conn, templating, in.Selected, rng)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": vars})
}

func templatingList(dash map[string]any) []any {
	t, _ := dash["templating"].(map[string]any)
	l, _ := t["list"].([]any)
	return l
}

func dashboardTime(dash map[string]any) (string, string) {
	tm, _ := dash["time"].(map[string]any)
	from, _ := tm["from"].(string)
	to, _ := tm["to"].(string)
	if from == "" {
		from = "now-6h"
	}
	if to == "" {
		to = "now"
	}
	return from, to
}

func (h *GrafanaOpsHandler) Query(c *gin.Context) {
	conn, role, ok := h.access(c)
	if !ok {
		return
	}
	var in grafanaquery.QueryRequest
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(in.Targets) == 0 {
		c.JSON(http.StatusOK, gin.H{"data": grafanaquery.QueryResult{Results: map[string]*grafanaquery.RefResult{}}})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	res, err := h.registry.Query(ctx, *conn, in, sqlGuard(role))
	// Ad-hoc queries carry intent and are audited; dashboard panel queries
	// (auto-refresh: one per panel per tick) are not.
	if in.Source != "" || c.Query("summary") != "" {
		h.audit(c, conn, role, "query", targetSummary(in.Targets), err)
	}
	if err != nil {
		h.fail(c, err)
		return
	}
	if c.Query("summary") != "" {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"range": gin.H{"from": res.From, "to": res.To}, "series": summarizeFrames(res, "")}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

func targetSummary(ts []map[string]any) string {
	var parts []string
	for _, t := range ts {
		for _, k := range []string{"expr", "rawSql", "query"} {
			if s, ok := t[k].(string); ok && s != "" {
				parts = append(parts, s)
				break
			}
		}
	}
	s := strings.Join(parts, " | ")
	if len(s) > 1000 {
		s = s[:1000]
	}
	return s
}

// PanelData is the server-side composition used by MCP: load the dashboard,
// resolve its variables (optionally overridden), run one panel's queries and
// return either raw frames or a compact per-series summary.
func (h *GrafanaOpsHandler) PanelData(c *gin.Context) {
	conn, role, ok := h.access(c)
	if !ok {
		return
	}
	var in struct {
		UID      string                            `json:"uid"`
		PanelID  int                               `json:"panelId"`
		Title    string                            `json:"title"`
		From     string                            `json:"from"`
		To       string                            `json:"to"`
		Selected map[string]grafanaquery.Selection `json:"selected"`
		Summary  bool                              `json:"summary"`
		Points   int                               `json:"points"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	d, err := h.registry.GetDashboard(ctx, *conn, in.UID)
	if err != nil {
		h.fail(c, err)
		return
	}
	panel := findPanel(d.Dashboard, in.PanelID, in.Title)
	if panel == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "panel not found in dashboard"})
		return
	}
	from, to := in.From, in.To
	if from == "" {
		from, to = dashboardTime(d.Dashboard)
	}
	rng, err := grafanaquery.ParseRange(from, to, time.Now())
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	vars, err := h.registry.ResolveVariables(ctx, *conn, templatingList(d.Dashboard), in.Selected, rng)
	if err != nil {
		h.fail(c, err)
		return
	}
	var targets []map[string]any
	if ts, ok := panel["targets"].([]any); ok {
		for _, t := range ts {
			if tm, ok := t.(map[string]any); ok {
				targets = append(targets, tm)
			}
		}
	}
	points := in.Points
	if points <= 0 {
		points = 300
	}
	req := grafanaquery.QueryRequest{
		From: from, To: to, Targets: targets, PanelDatasource: panel["datasource"],
		Vars: grafanaquery.VarsFrom(vars), MaxDataPoints: points,
	}
	if iv, ok := panel["interval"].(string); ok {
		req.MinInterval = iv
	}
	res, err := h.registry.Query(ctx, *conn, req, sqlGuard(role))
	h.audit(c, conn, role, "query", fmt.Sprintf("%s#%v", in.UID, panel["id"]), err)
	if err != nil {
		h.fail(c, err)
		return
	}
	out := gin.H{
		"panel":     gin.H{"id": panel["id"], "title": panel["title"], "type": panel["type"]},
		"range":     gin.H{"from": res.From, "to": res.To},
		"variables": compactVars(vars),
	}
	if in.Summary {
		out["series"] = summarizeFrames(res, unitOf(panel))
	} else {
		out["results"] = res.Results
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func findPanel(dash map[string]any, id int, title string) map[string]any {
	var found map[string]any
	var walk func([]any)
	walk = func(ps []any) {
		for _, p := range ps {
			pm, _ := p.(map[string]any)
			if pm == nil || found != nil {
				continue
			}
			if nested, ok := pm["panels"].([]any); ok {
				walk(nested)
			}
			pid, _ := pm["id"].(float64)
			pt, _ := pm["title"].(string)
			if (id > 0 && int(pid) == id) || (id <= 0 && title != "" && strings.EqualFold(pt, title)) {
				if pm["type"] != "row" {
					found = pm
				}
			}
		}
	}
	ps, _ := dash["panels"].([]any)
	walk(ps)
	return found
}

func unitOf(panel map[string]any) string {
	fc, _ := panel["fieldConfig"].(map[string]any)
	def, _ := fc["defaults"].(map[string]any)
	u, _ := def["unit"].(string)
	return u
}

func compactVars(vars []grafanaquery.ResolvedVar) map[string]any {
	out := map[string]any{}
	for _, v := range vars {
		if len(v.Current.Texts) == 1 {
			out[v.Name] = v.Current.Texts[0]
		} else {
			out[v.Name] = v.Current.Texts
		}
	}
	return out
}

// summarizeFrames reduces frames to per-series stats + a few points — what
// an LLM needs, without megabytes of raw values.
func summarizeFrames(res *grafanaquery.QueryResult, unit string) []map[string]any {
	var out []map[string]any
	for ref, rr := range res.Results {
		if rr.Error != "" {
			out = append(out, map[string]any{"refId": ref, "error": rr.Error})
			continue
		}
		for _, raw := range rr.Frames {
			var f grafanaquery.Frame
			if json.Unmarshal(raw, &f) != nil {
				continue
			}
			timeIdx := -1
			var strIdx []int
			for i, fl := range f.Schema.Fields {
				if fl.Type == "time" && timeIdx < 0 {
					timeIdx = i
				}
				if fl.Type == "string" {
					strIdx = append(strIdx, i)
				}
			}
			// Table result (no time column): one entry per row, named by
			// its string columns — what an LLM needs from a GROUP BY.
			if timeIdx < 0 && len(strIdx) > 0 {
				for i, fl := range f.Schema.Fields {
					if fl.Type != "number" || i >= len(f.Data.Values) {
						continue
					}
					for r, v := range f.Data.Values[i] {
						var parts []string
						for _, si := range strIdx {
							if si < len(f.Data.Values) && r < len(f.Data.Values[si]) {
								parts = append(parts, fmt.Sprint(f.Data.Values[si][r]))
							}
						}
						out = append(out, map[string]any{"refId": ref, "name": strings.Join(parts, " / "), "field": fl.Name, "value": v})
						if len(out) >= 200 {
							return out
						}
					}
				}
				continue
			}
			for i, fl := range f.Schema.Fields {
				if fl.Type != "number" || i >= len(f.Data.Values) {
					continue
				}
				name := fl.Name
				if dn, ok := fl.Config["displayNameFromDS"].(string); ok && dn != "" {
					name = dn
				} else if len(fl.Labels) > 0 {
					name = labelString(fl.Labels)
				}
				vals := f.Data.Values[i]
				var nums []float64
				for _, v := range vals {
					if x, ok := v.(float64); ok && !math.IsNaN(x) {
						nums = append(nums, x)
					}
				}
				s := map[string]any{"refId": ref, "name": name, "count": len(nums)}
				if unit != "" {
					s["unit"] = unit
				}
				if len(fl.Labels) > 0 {
					s["labels"] = fl.Labels
				}
				if len(nums) > 0 {
					mn, mx, sum := nums[0], nums[0], 0.0
					for _, x := range nums {
						mn, mx, sum = math.Min(mn, x), math.Max(mx, x), sum+x
					}
					s["last"], s["min"], s["max"], s["avg"] = nums[len(nums)-1], mn, mx, sum/float64(len(nums))
				}
				if timeIdx >= 0 && timeIdx < len(f.Data.Values) {
					s["points"] = samplePoints(f.Data.Values[timeIdx], vals, 12)
				}
				out = append(out, s)
				if len(out) >= 100 {
					return out
				}
			}
		}
	}
	return out
}

func labelString(l map[string]string) string {
	parts := make([]string, 0, len(l))
	for k, v := range l {
		parts = append(parts, k+"="+v)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func samplePoints(times, vals []any, n int) [][2]any {
	if len(times) == 0 || len(vals) == 0 {
		return nil
	}
	step := len(times) / n
	if step < 1 {
		step = 1
	}
	var out [][2]any
	for i := 0; i < len(times) && i < len(vals); i += step {
		out = append(out, [2]any{msToRFC3339(times[i]), vals[i]})
	}
	last := len(times) - 1
	if last < len(vals) && (len(out) == 0 || out[len(out)-1][0] != msToRFC3339(times[last])) {
		out = append(out, [2]any{msToRFC3339(times[last]), vals[last]})
	}
	return out
}

func msToRFC3339(v any) string {
	f, ok := v.(float64)
	if !ok {
		return fmt.Sprint(v)
	}
	return time.UnixMilli(int64(f)).UTC().Format(time.RFC3339)
}

// PublicDashboard loads a public (externally shared) dashboard by its token.
func (h *GrafanaOpsHandler) PublicDashboard(c *gin.Context) {
	conn, role, ok := h.access(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	d, err := h.registry.GetPublicDashboard(ctx, *conn, c.Param("token"))
	h.audit(c, conn, role, "dashboard", "public:"+c.Param("token"), err)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": d})
}

// PublicPanelQuery runs one panel of a public dashboard (no variables).
func (h *GrafanaOpsHandler) PublicPanelQuery(c *gin.Context) {
	conn, _, ok := h.access(c)
	if !ok {
		return
	}
	var in struct {
		From          string `json:"from"`
		To            string `json:"to"`
		MaxDataPoints int    `json:"maxDataPoints"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	panelID, err := strconv.Atoi(c.Param("panelId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid panel id"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	res, err := h.registry.QueryPublicPanel(ctx, *conn, c.Param("token"), panelID, in.From, in.To, in.MaxDataPoints)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

// Resource proxies one allowlisted Prometheus/Loki API path (autocomplete,
// label browser). ?path=api/v1/labels plus the API's own query params.
func (h *GrafanaOpsHandler) Resource(c *gin.Context) {
	conn, _, ok := h.access(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	info, err := h.registry.Info(ctx, *conn)
	if err != nil {
		h.fail(c, err)
		return
	}
	ds := info.FindDatasource(c.Param("uid"))
	if ds == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "datasource not found"})
		return
	}
	q := url.Values{}
	for k, v := range c.Request.URL.Query() {
		if k != "path" {
			q[k] = v
		}
	}
	body, err := h.registry.DSResource(ctx, *conn, ds, c.Query("path"), q)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.Data(http.StatusOK, "application/json", body)
}

func (h *GrafanaOpsHandler) audit(c *gin.Context, conn *models.GrafanaConnection, role, action, target string, actErr error) {
	var delegated *string
	if role != "owner" {
		owner := conn.UserID
		delegated = &owner
	}
	errText := ""
	if actErr != nil {
		errText = actErr.Error()
	}
	if len(target) > 1024 {
		target = target[:1024]
	}
	row := models.GrafanaActivityHistory{
		ID: uuid.New().String(), UserID: c.GetString("userID"), DelegatedOwner: delegated,
		ConnectionID: conn.ID, Action: action, Target: target, ErrorText: errText, ExecutedAt: time.Now(),
	}
	go func() { _ = h.db.Create(&row).Error }()
}
