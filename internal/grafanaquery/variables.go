package grafanaquery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"patchman-be/internal/models"
)

// ResolvedVar is one dashboard variable with its options and the effective
// selection, ready for the FE dropdowns and for query interpolation.
type ResolvedVar struct {
	Name       string   `json:"name"`
	Label      string   `json:"label,omitempty"`
	Type       string   `json:"type"`
	Hide       int      `json:"hide"`
	Multi      bool     `json:"multi"`
	IncludeAll bool     `json:"includeAll"`
	AllValue   string   `json:"allValue,omitempty"`
	Options    []Option `json:"options"`
	Current    VarValue `json:"current"`
	// Free is true when options can't be listed (unsupported query
	// datasource, textbox): the FE shows a text input.
	Free  bool   `json:"free,omitempty"`
	Error string `json:"error,omitempty"`
}

// Selection is the user's choice for a variable (from the FE / URL / MCP).
type Selection struct {
	Values []string `json:"values"`
	Texts  []string `json:"texts,omitempty"`
}

// ResolveVariables walks the templating list in order — later variables may
// reference earlier ones — and returns each variable's options + selection.
// Failures are per variable (Error set), never fatal.
func (r *Registry) ResolveVariables(ctx context.Context, conn models.GrafanaConnection, templating []any, selected map[string]Selection, rng TimeRange) ([]ResolvedVar, error) {
	info, err := r.Info(ctx, conn)
	if err != nil {
		return nil, err
	}
	vars := map[string]VarValue{}
	defs := make([]map[string]any, len(templating))
	for i, raw := range templating {
		defs[i], _ = raw.(map[string]any)
	}
	// Resolve in dependency order (a query may reference a variable defined
	// further down, as Grafana allows) but report in dashboard order.
	resolved := make([]*ResolvedVar, len(defs))
	for _, i := range resolveOrder(defs) {
		def := defs[i]
		if def == nil {
			continue
		}
		rv := ResolvedVar{
			Name:       str(def["name"]),
			Label:      str(def["label"]),
			Type:       str(def["type"]),
			Hide:       num(def["hide"]),
			Multi:      truthy(def["multi"]),
			IncludeAll: truthy(def["includeAll"]),
			AllValue:   str(def["allValue"]),
			Options:    []Option{},
		}
		if rv.Name == "" || rv.Type == "adhoc" || rv.Type == "groupby" || rv.Type == "snapshot" {
			continue
		}
		if rv.Type == "constant" {
			rv.Hide = 2
		}
		ip := &Interpolator{Vars: vars, Range: rng, DefaultFormat: "glob"}
		opts, free, oerr := r.variableOptions(ctx, conn, info, def, ip, rng)
		if oerr != nil {
			rv.Error = oerr.Error()
			free = true
			opts = savedOptions(def)
		}
		rv.Free = free
		opts = ApplyRegex(opts, ip.Replace(str(def["regex"])))
		SortOptions(opts, num(def["sort"]))
		if rv.IncludeAll && len(opts) > 0 {
			opts = append([]Option{{Text: "All", Value: "$__all"}}, opts...)
		}
		if opts == nil {
			opts = []Option{} // JSON [] — a regex matching nothing must not send null
		}
		rv.Options = opts
		rv.Current = pickCurrent(rv, def, selected[rv.Name], opts)
		if rv.Current.Values == nil {
			rv.Current.Values = []string{}
		}
		if rv.Current.Texts == nil {
			rv.Current.Texts = []string{}
		}
		rv.Current.Multi, rv.Current.IncludeAll, rv.Current.AllValue = rv.Multi, rv.IncludeAll, rv.AllValue
		for _, o := range opts {
			if o.Value != "$__all" {
				rv.Current.AllOptions = append(rv.Current.AllOptions, o.Value)
			}
		}
		// Interval "auto" resolves against the range now so dependants see it.
		if rv.Type == "interval" && len(rv.Current.Values) == 1 && rv.Current.Values[0] == "$__auto" {
			count := num(def["auto_count"])
			if count <= 0 {
				count = 30
			}
			minIv, _ := ParseDuration(str(def["auto_min"]))
			if minIv == 0 {
				minIv = 10 * time.Second
			}
			v := FormatDuration(CalcInterval(rng, count, minIv))
			rv.Current.Values, rv.Current.Texts = []string{v}, []string{"auto"}
		}
		vars[rv.Name] = rv.Current
		resolved[i] = &rv
	}
	out := make([]ResolvedVar, 0, len(defs))
	for _, rv := range resolved {
		if rv != nil {
			out = append(out, *rv)
		}
	}
	return out, nil
}

// resolveOrder returns the indexes of defs ordered so every variable comes
// after the variables its query / datasource / regex reference. Ties keep the
// dashboard order; cycles are broken at the back edge.
func resolveOrder(defs []map[string]any) []int {
	index := map[string]int{}
	for i, d := range defs {
		if n := str(d["name"]); n != "" {
			if _, dup := index[n]; !dup {
				index[n] = i
			}
		}
	}
	deps := make([][]int, len(defs))
	for i, d := range defs {
		if d == nil {
			continue
		}
		var b strings.Builder
		for _, k := range []string{"query", "datasource", "regex", "definition"} {
			switch v := d[k].(type) {
			case string:
				b.WriteString(v)
			case nil:
			default:
				if j, err := json.Marshal(v); err == nil {
					b.Write(j)
				}
			}
			b.WriteByte('\n')
		}
		for _, m := range variableRE.FindAllStringSubmatch(b.String(), -1) {
			for _, name := range []string{m[1], m[2], m[4]} {
				if j, ok := index[name]; ok && name != "" && j != i {
					deps[i] = append(deps[i], j)
				}
			}
		}
	}
	state := make([]uint8, len(defs)) // 0 new, 1 visiting, 2 done
	order := make([]int, 0, len(defs))
	var visit func(int)
	visit = func(i int) {
		if state[i] != 0 {
			return
		}
		state[i] = 1
		for _, j := range deps[i] {
			visit(j)
		}
		state[i] = 2
		order = append(order, i)
	}
	for i := range defs {
		visit(i)
	}
	return order
}

// VarsFrom turns resolved variables into the interpolation map.
func VarsFrom(resolved []ResolvedVar) map[string]VarValue {
	m := make(map[string]VarValue, len(resolved))
	for _, v := range resolved {
		m[v.Name] = v.Current
	}
	return m
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	return ""
}

func truthy(v any) bool {
	b, _ := v.(bool)
	return b
}

func savedOptions(def map[string]any) []Option {
	var out []Option
	if list, ok := def["options"].([]any); ok {
		for _, o := range list {
			om, _ := o.(map[string]any)
			val := firstString(om["value"])
			if val == "" || val == "$__all" {
				continue
			}
			out = append(out, Option{Text: firstString(om["text"]), Value: val})
		}
	}
	if len(out) == 0 {
		cur, _ := def["current"].(map[string]any)
		for i, v := range stringList(cur["value"]) {
			if v == "$__all" {
				continue
			}
			texts := stringList(cur["text"])
			t := v
			if i < len(texts) {
				t = texts[i]
			}
			out = append(out, Option{Text: t, Value: v})
		}
	}
	if out == nil {
		out = []Option{}
	}
	return out
}

func firstString(v any) string {
	l := stringList(v)
	if len(l) == 0 {
		return ""
	}
	return l[0]
}

func stringList(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			out = append(out, str(x))
		}
		return out
	case []string:
		return t
	}
	return nil
}

func pickCurrent(rv ResolvedVar, def map[string]any, sel Selection, opts []Option) VarValue {
	valid := func(vals []string) bool {
		if len(vals) == 0 {
			return false
		}
		if rv.Free || rv.Type == "textbox" {
			return true
		}
		for _, v := range vals {
			found := false
			for _, o := range opts {
				if o.Value == v {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	}
	textsFor := func(vals []string) []string {
		out := make([]string, len(vals))
		for i, v := range vals {
			out[i] = v
			for _, o := range opts {
				if o.Value == v {
					out[i] = o.Text
					break
				}
			}
		}
		return out
	}
	if valid(sel.Values) {
		return VarValue{Values: sel.Values, Texts: textsFor(sel.Values)}
	}
	cur, _ := def["current"].(map[string]any)
	if vals := stringList(cur["value"]); valid(vals) {
		return VarValue{Values: vals, Texts: textsFor(vals)}
	}
	if rv.Type == "textbox" {
		v := str(def["query"])
		return VarValue{Values: []string{v}, Texts: []string{v}}
	}
	if len(opts) > 0 {
		return VarValue{Values: []string{opts[0].Value}, Texts: []string{opts[0].Text}}
	}
	return VarValue{Values: []string{}, Texts: []string{}}
}

// variableOptions lists a variable's options. free=true means "free text"
// (options are only suggestions).
func (r *Registry) variableOptions(ctx context.Context, conn models.GrafanaConnection, info *ServerInfo, def map[string]any, ip *Interpolator, rng TimeRange) ([]Option, bool, error) {
	switch str(def["type"]) {
	case "custom":
		return parseCustom(ip.Replace(str(def["query"]))), false, nil
	case "constant":
		v := ip.Replace(str(def["query"]))
		return []Option{{Text: v, Value: v}}, false, nil
	case "textbox":
		v := str(def["query"])
		if v == "" {
			return []Option{}, true, nil
		}
		return []Option{{Text: v, Value: v}}, true, nil
	case "interval":
		var out []Option
		if truthy(def["auto"]) {
			out = append(out, Option{Text: "auto", Value: "$__auto"})
		}
		for _, p := range strings.Split(str(def["query"]), ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, Option{Text: p, Value: p})
			}
		}
		return out, false, nil
	case "datasource":
		pluginID := strings.ToLower(str(def["query"]))
		var out []Option
		for _, ds := range info.Datasources {
			if pluginID == "" || strings.ToLower(ds.Type) == pluginID {
				out = append(out, Option{Text: ds.Name, Value: ds.UID})
			}
		}
		if out == nil {
			out = []Option{}
		}
		return out, false, nil
	case "query":
		return r.queryVariable(ctx, conn, info, def, ip, rng)
	}
	return savedOptions(def), true, nil
}

// parseCustom parses "a,b,c" and "text : value" pairs (\, escapes a comma).
func parseCustom(q string) []Option {
	var parts []string
	var cur strings.Builder
	for i := 0; i < len(q); i++ {
		if q[i] == '\\' && i+1 < len(q) && q[i+1] == ',' {
			cur.WriteByte(',')
			i++
			continue
		}
		if q[i] == ',' {
			parts = append(parts, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(q[i])
	}
	parts = append(parts, cur.String())
	out := []Option{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if kv := strings.SplitN(p, " : ", 2); len(kv) == 2 {
			out = append(out, Option{Text: strings.TrimSpace(kv[0]), Value: strings.TrimSpace(kv[1])})
			continue
		}
		out = append(out, Option{Text: p, Value: p})
	}
	return out
}

// dsRef extracts a datasource reference (uid / name / id) from a classic ref
// (string name, {uid,type} object, or nil).
func dsRef(v any, ip *Interpolator) string {
	switch t := v.(type) {
	case string:
		if t == "null" {
			return "" // migration leftover: the default datasource
		}
		return ip.Replace(t)
	case map[string]any:
		if u := str(t["uid"]); u != "" && u != "null" {
			return ip.Replace(u)
		}
		if n := str(t["name"]); n != "" {
			return ip.Replace(n)
		}
	}
	return ""
}

// dsType returns the "type" of a datasource ref object ("" for strings).
func dsType(v any) string {
	if m, ok := v.(map[string]any); ok {
		return str(m["type"])
	}
	return ""
}

func (r *Registry) queryVariable(ctx context.Context, conn models.GrafanaConnection, info *ServerInfo, def map[string]any, ip *Interpolator, rng TimeRange) ([]Option, bool, error) {
	ds := resolveTargetDS(info, dsRef(def["datasource"], ip), dsType(def["datasource"]), "", "")
	if ds == nil {
		return nil, true, fmt.Errorf("datasource not found for variable %s", str(def["name"]))
	}
	ip.DefaultFormat = DefaultFormatFor(ds.Type)
	query := def["query"]
	rawQs := ""
	switch t := query.(type) {
	case string:
		rawQs = t
	case map[string]any:
		rawQs = str(t["query"])
		if rawQs == "" {
			rawQs = str(t["rawSql"])
		}
	}
	qs := strings.TrimSpace(ip.Replace(rawQs))
	t := strings.ToLower(ds.Type)
	switch {
	case IsPromLike(t):
		if qm, ok := query.(map[string]any); ok && qs == "" {
			qs = promQueryFromEditor(qm, ip)
		}
		opts, err := r.promVariable(ctx, conn, ds, qs, rng)
		return opts, false, err
	case strings.Contains(t, "loki"):
		if qm, ok := query.(map[string]any); ok && qs == "" {
			qs = lokiQueryFromEditor(qm, ip)
		}
		opts, err := r.lokiVariable(ctx, conn, ds, qs, rng)
		return opts, false, err
	case strings.Contains(t, "elasticsearch") || strings.Contains(t, "opensearch"):
		// The ES query is JSON: parse it before interpolating, or a lucene
		// multi-value ("a" OR "b") breaks the JSON string.
		opts, err := r.esVariable(ctx, conn, ds, strings.TrimSpace(rawQs), ip, rng)
		return opts, false, err
	case strings.Contains(t, "influx"):
		// InfluxQL (SHOW TAG VALUES / SHOW MEASUREMENTS …) and Flux both run
		// as a raw query through the influx backend.
		opts, err := r.tableVariable(ctx, conn, ds, map[string]any{"refId": "A", "query": qs, "rawQuery": true, "resultFormat": "table"}, rng)
		return opts, false, err
	case DefaultFormatFor(t) == "sqlstring":
		var format any = "table"
		if t == "grafana-clickhouse-datasource" {
			format = 1 // the plugin's Format enum (older sqlds versions reject the string)
		}
		opts, err := r.tableVariable(ctx, conn, ds, map[string]any{"refId": "A", "rawSql": qs, "format": format, "rawQuery": true, "editorMode": "code"}, rng)
		return opts, false, err
	}
	// Any other backend datasource: send the variable query as a target and
	// read the first column. Datasources whose variable support lives only in
	// Grafana's frontend fail here — the error says so and the dashboard's
	// saved values are offered instead (plus a custom value in the FE).
	target := map[string]any{"refId": "A"}
	if qm, ok := query.(map[string]any); ok {
		for k, v := range ip.InterpolateTarget(qm) {
			target[k] = v
		}
	} else {
		target["query"] = qs
	}
	opts, err := r.tableVariable(ctx, conn, ds, target, rng)
	if err != nil {
		return nil, true, fmt.Errorf("values of %q variables can't be listed from %s datasources yet (%v) — showing the dashboard's saved values", str(def["name"]), ds.Type, err)
	}
	return opts, false, nil
}

// IsPromLike: datasources that speak the Prometheus HTTP API (Prometheus,
// Amazon Managed Prometheus, VictoriaMetrics, Mimir, Thanos, Cortex).
func IsPromLike(dsType string) bool {
	t := strings.ToLower(dsType)
	for _, k := range []string{"prometheus", "victoria", "mimir", "thanos", "cortex"} {
		if strings.Contains(t, k) {
			return true
		}
	}
	return false
}

// esVariable runs an Elasticsearch / OpenSearch variable query — the JSON the
// Grafana editor stores, {"find":"terms","field":"…","query":"…","size":…} —
// as a terms aggregation through /api/ds/query.
func (r *Registry) esVariable(ctx context.Context, conn models.GrafanaConnection, ds *Datasource, qs string, ip *Interpolator, rng TimeRange) ([]Option, error) {
	var q struct {
		Find    string `json:"find"`
		Field   string `json:"field"`
		Query   string `json:"query"`
		Size    any    `json:"size"`
		OrderBy string `json:"orderBy"`
		Order   string `json:"order"`
	}
	if err := json.Unmarshal([]byte(qs), &q); err != nil {
		return nil, fmt.Errorf("unsupported Elasticsearch variable query %q", qs)
	}
	q.Field = ip.Replace(q.Field)
	q.Query = ip.Replace(q.Query)
	if q.Find != "terms" || q.Field == "" {
		return nil, fmt.Errorf("Elasticsearch %q variable queries are not supported yet", q.Find)
	}
	size := "500"
	if s := str(q.Size); s != "" && s != "0" {
		size = s
	}
	orderBy := "_term"
	if q.OrderBy == "doc_count" {
		orderBy = "_count"
	}
	order := "asc"
	if q.Order == "desc" {
		order = "desc"
	}
	lucene := strings.TrimSpace(q.Query)
	if lucene == "" {
		lucene = "*"
	}
	target := map[string]any{
		"refId":   "A",
		"query":   lucene,
		"metrics": []any{map[string]any{"type": "count", "id": "1"}},
		"bucketAggs": []any{map[string]any{
			"type": "terms", "id": "2", "field": q.Field,
			"settings": map[string]any{"min_doc_count": "1", "size": size, "order": order, "orderBy": orderBy},
		}},
	}
	if ds.TimeField != "" {
		target["timeField"] = ds.TimeField
	}
	return r.tableVariable(ctx, conn, ds, target, rng)
}

// promQueryFromEditor rebuilds the classic string from the Grafana 10+
// Prometheus variable editor object (qryType: 0 label names, 1 label values,
// 2 metrics, 3 query result, 4 series query).
func promQueryFromEditor(q map[string]any, ip *Interpolator) string {
	label, metric := ip.Replace(str(q["label"])), ip.Replace(str(q["metric"]))
	switch num(q["qryType"]) {
	case 0:
		return "label_names()"
	case 1:
		if metric != "" {
			return fmt.Sprintf("label_values(%s, %s)", metric, label)
		}
		return fmt.Sprintf("label_values(%s)", label)
	case 2:
		return fmt.Sprintf("metrics(%s)", metric)
	case 3:
		return fmt.Sprintf("query_result(%s)", ip.Replace(str(q["varQuery"])))
	case 4:
		return ip.Replace(str(q["seriesQuery"]))
	}
	return ""
}

func lokiQueryFromEditor(q map[string]any, ip *Interpolator) string {
	label, stream := ip.Replace(str(q["label"])), ip.Replace(str(q["stream"]))
	if num(q["type"]) == 0 {
		return "label_names()"
	}
	if stream != "" {
		return fmt.Sprintf("label_values(%s, %s)", stream, label)
	}
	return fmt.Sprintf("label_values(%s)", label)
}

var (
	reLabelNames  = regexp.MustCompile(`^label_names\(\s*(.*?)\s*\)\s*$`)
	reLabelValues = regexp.MustCompile(`^label_values\((?:(.+),\s*)?([a-zA-Z_$][a-zA-Z0-9_]*)\)\s*$`)
	reMetrics     = regexp.MustCompile(`^metrics\((.+)\)\s*$`)
	reQueryResult = regexp.MustCompile(`^query_result\((.+)\)\s*$`)
)

func rangeParams(rng TimeRange) url.Values {
	return url.Values{
		"start": {strconv.FormatInt(rng.From.Unix(), 10)},
		"end":   {strconv.FormatInt(rng.To.Unix(), 10)},
	}
}

func toOptions(vals []string) []Option {
	out := make([]Option, 0, len(vals))
	seen := map[string]bool{}
	for _, v := range vals {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, Option{Text: v, Value: v})
	}
	return out
}

func (r *Registry) promVariable(ctx context.Context, conn models.GrafanaConnection, ds *Datasource, q string, rng TimeRange) ([]Option, error) {
	params := rangeParams(rng)
	switch {
	case q == "":
		return []Option{}, nil
	case reLabelNames.MatchString(q):
		if m := reLabelNames.FindStringSubmatch(q); m[1] != "" {
			params.Set("match[]", m[1])
		}
		body, err := r.DSResource(ctx, conn, ds, "api/v1/labels", params)
		if err != nil {
			return nil, err
		}
		vals, err := dsStrings(body)
		return toOptions(vals), err
	case reLabelValues.MatchString(q):
		m := reLabelValues.FindStringSubmatch(q)
		selector, label := strings.TrimSpace(m[1]), m[2]
		if selector != "" {
			params.Set("match[]", selector)
		}
		body, err := r.DSResource(ctx, conn, ds, "api/v1/label/"+url.PathEscape(label)+"/values", params)
		if err == nil {
			vals, derr := dsStrings(body)
			if derr == nil {
				return toOptions(vals), nil
			}
			err = derr
		}
		if selector == "" {
			return nil, err
		}
		// Old Prometheus ignores match[] on label values → series fallback.
		body, err = r.DSResource(ctx, conn, ds, "api/v1/series", params)
		if err != nil {
			return nil, err
		}
		series, err := dsSeries(body)
		if err != nil {
			return nil, err
		}
		var vals []string
		for _, s := range series {
			if v, ok := s[label]; ok {
				vals = append(vals, v)
			}
		}
		sort.Strings(vals)
		return toOptions(vals), nil
	case reMetrics.MatchString(q):
		re := strings.TrimSpace(reMetrics.FindStringSubmatch(q)[1])
		body, err := r.DSResource(ctx, conn, ds, "api/v1/label/__name__/values", params)
		if err != nil {
			return nil, err
		}
		vals, err := dsStrings(body)
		if err != nil {
			return nil, err
		}
		return ApplyRegex(toOptions(vals), re), nil
	case reQueryResult.MatchString(q):
		expr := reQueryResult.FindStringSubmatch(q)[1]
		p := url.Values{"query": {expr}, "time": {strconv.FormatInt(rng.To.Unix(), 10)}}
		body, err := r.DSResource(ctx, conn, ds, "api/v1/query", p)
		if err != nil {
			return nil, err
		}
		var res struct {
			Data struct {
				Result []struct {
					Metric map[string]string `json:"metric"`
					Value  []any             `json:"value"`
				} `json:"result"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			return nil, err
		}
		var vals []string
		for _, s := range res.Data.Result {
			ts, val := "", ""
			if len(s.Value) == 2 {
				if f, ok := s.Value[0].(float64); ok {
					ts = strconv.FormatInt(int64(f*1000), 10)
				}
				val = str(s.Value[1])
			}
			vals = append(vals, fmt.Sprintf("%s %s %s", formatSeries(s.Metric), val, ts))
		}
		return toOptions(vals), nil
	default:
		params.Set("match[]", q)
		body, err := r.DSResource(ctx, conn, ds, "api/v1/series", params)
		if err != nil {
			return nil, err
		}
		series, err := dsSeries(body)
		if err != nil {
			return nil, err
		}
		var vals []string
		for _, s := range series {
			vals = append(vals, formatSeries(s))
		}
		return toOptions(vals), nil
	}
}

// formatSeries renders name{a="b", c="d"} like Grafana.
func formatSeries(m map[string]string) string {
	name := m["__name__"]
	keys := make([]string, 0, len(m))
	for k := range m {
		if k != "__name__" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%q", k, m[k])
	}
	return name + "{" + strings.Join(parts, ", ") + "}"
}

func (r *Registry) lokiVariable(ctx context.Context, conn models.GrafanaConnection, ds *Datasource, q string, rng TimeRange) ([]Option, error) {
	params := url.Values{
		"start": {strconv.FormatInt(rng.From.UnixNano(), 10)},
		"end":   {strconv.FormatInt(rng.To.UnixNano(), 10)},
	}
	switch {
	case q == "" || reLabelNames.MatchString(q):
		body, err := r.DSResource(ctx, conn, ds, "loki/api/v1/labels", params)
		if err != nil {
			return nil, err
		}
		vals, err := dsStrings(body)
		return toOptions(vals), err
	case reLabelValues.MatchString(q):
		m := reLabelValues.FindStringSubmatch(q)
		if s := strings.TrimSpace(m[1]); s != "" {
			params.Set("query", s)
		}
		body, err := r.DSResource(ctx, conn, ds, "loki/api/v1/label/"+url.PathEscape(m[2])+"/values", params)
		if err != nil {
			return nil, err
		}
		vals, err := dsStrings(body)
		return toOptions(vals), err
	}
	return nil, fmt.Errorf("unsupported Loki variable query %q", q)
}

// tableVariable runs one target through /api/ds/query and turns the first
// frame into options: __text/__value columns when present, else the first
// column.
func (r *Registry) tableVariable(ctx context.Context, conn models.GrafanaConnection, ds *Datasource, target map[string]any, rng TimeRange) ([]Option, error) {
	res, err := r.runDSQuery(ctx, conn, ds, []map[string]any{target}, rng)
	if err != nil {
		return nil, err
	}
	var frames []Frame
	for _, rr := range res {
		if rr.Error != "" {
			return nil, fmt.Errorf("%s", rr.Error)
		}
		for _, raw := range rr.Frames {
			var f Frame
			if json.Unmarshal(raw, &f) == nil {
				frames = append(frames, f)
			}
		}
	}
	var out []Option
	for _, f := range frames {
		textIdx, valIdx := -1, -1
		for i, fl := range f.Schema.Fields {
			switch fl.Name {
			case "__text":
				textIdx = i
			case "__value":
				valIdx = i
			}
		}
		if valIdx < 0 && textIdx < 0 {
			pick := -1
			for i, fl := range f.Schema.Fields {
				if strings.EqualFold(fl.Name, "value") && fl.Type == "string" {
					pick = i
					break
				}
			}
			if pick < 0 {
				for i, fl := range f.Schema.Fields {
					if fl.Type == "string" {
						pick = i
						break
					}
				}
			}
			if pick < 0 {
				pick = 0
			}
			valIdx, textIdx = pick, pick
		} else if valIdx < 0 {
			valIdx = textIdx
		} else if textIdx < 0 {
			textIdx = valIdx
		}
		if valIdx >= len(f.Data.Values) || textIdx >= len(f.Data.Values) {
			continue
		}
		vals, texts := f.Data.Values[valIdx], f.Data.Values[textIdx]
		for i := range vals {
			v := str(vals[i])
			t := v
			if i < len(texts) {
				t = str(texts[i])
			}
			out = append(out, Option{Text: t, Value: v})
		}
	}
	if out == nil {
		out = []Option{}
	}
	return out, nil
}
