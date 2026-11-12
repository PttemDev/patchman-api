package grafanaquery

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// VarValue is the current selection of one dashboard variable.
type VarValue struct {
	// Values are the selected values ("$__all" when All is selected).
	Values []string `json:"values"`
	// Texts parallel Values (display text; ${var:text}).
	Texts []string `json:"texts,omitempty"`
	// Multi / IncludeAll mirror the variable definition: they decide the
	// default formatting (Prometheus regex-escapes only multi/all variables).
	Multi      bool `json:"multi,omitempty"`
	IncludeAll bool `json:"includeAll,omitempty"`
	// AllValue is the custom "All" value (e.g. ".*"); used raw when set.
	AllValue string `json:"allValue,omitempty"`
	// AllOptions are every option value — what $__all expands to when no
	// custom AllValue is set.
	AllOptions []string `json:"allOptions,omitempty"`
}

// IsAll reports whether All is selected.
func (v VarValue) IsAll() bool {
	for _, s := range v.Values {
		if s == "$__all" {
			return true
		}
	}
	return false
}

// TimeRange is a resolved absolute range.
type TimeRange struct {
	From time.Time
	To   time.Time
}

func (t TimeRange) FromMs() int64 { return t.From.UnixMilli() }
func (t TimeRange) ToMs() int64   { return t.To.UnixMilli() }

// variableRE is Grafana's variable syntax: $var, [[var]], [[var:fmt]],
// ${var}, ${var.field}, ${var:fmt}.
var variableRE = regexp.MustCompile(`\$(\w+)|\[\[(\w+?)(?::(\w+))?\]\]|\$\{(\w+)(?:\.([^:^\}]+))?(?::([^\}]+))?\}`)

// Interpolator substitutes dashboard variables. Only dashboard variables and
// $__from/$__to/$__all are handled here; every other $__ macro ($__interval,
// $__rate_interval, $__range, $__timeFilter, …) passes through untouched for
// the datasource backend to expand from intervalMs/maxDataPoints/range.
type Interpolator struct {
	Vars  map[string]VarValue
	Range TimeRange
	// DefaultFormat is the datasource's format for multi-value/All variables:
	// "regex" (prometheus, loki), "sqlstring" (SQL), "lucene" (elasticsearch),
	// "glob" (everything else).
	DefaultFormat string
	// ScopedVars override Vars (e.g. $__interval resolved for the fallback
	// proxy path, which has no Grafana backend to expand it).
	ScopedVars map[string]string
}

// DefaultFormatFor maps a datasource plugin id to its multi-value format.
func DefaultFormatFor(dsType string) string {
	t := strings.ToLower(dsType)
	switch {
	case IsPromLike(t) || t == "loki" || strings.Contains(t, "influx"):
		return "regex"
	case strings.Contains(t, "postgres") || strings.Contains(t, "mysql") || strings.Contains(t, "mssql") ||
		strings.Contains(t, "clickhouse") || strings.Contains(t, "sql"):
		return "sqlstring"
	case t == "elasticsearch" || strings.Contains(t, "opensearch"):
		return "lucene"
	}
	return "glob"
}

// Replace interpolates one string.
func (ip *Interpolator) Replace(s string) string {
	if !strings.ContainsAny(s, "$[") {
		return s
	}
	return variableRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := variableRE.FindStringSubmatch(m)
		name, fieldPath, format := sub[1], "", ""
		if sub[2] != "" {
			name, format = sub[2], sub[3]
		} else if sub[4] != "" {
			name, fieldPath, format = sub[4], sub[5], sub[6]
		}
		if v, ok := ip.ScopedVars[name]; ok {
			return v
		}
		switch name {
		case "__from":
			return formatTime(ip.Range.From, format)
		case "__to":
			return formatTime(ip.Range.To, format)
		}
		if strings.HasPrefix(name, "__") {
			return m // backend macro — leave it
		}
		v, ok := ip.Vars[name]
		if !ok {
			return m
		}
		if fieldPath != "" {
			// ${var.field} — only meaningful for object values; fall back to raw.
			format = "raw"
		}
		return ip.format(v, format)
	})
}

func formatTime(t time.Time, format string) string {
	switch {
	case format == "" || format == "date:ms":
		return strconv.FormatInt(t.UnixMilli(), 10)
	case format == "date" || format == "date:iso":
		return t.UTC().Format(time.RFC3339Nano)
	case format == "date:seconds":
		return strconv.FormatInt(t.Unix(), 10)
	}
	return strconv.FormatInt(t.UnixMilli(), 10)
}

func (ip *Interpolator) format(v VarValue, format string) string {
	values := v.Values
	texts := v.Texts
	if v.IsAll() {
		if v.AllValue != "" && format == "" {
			return v.AllValue // custom all value is used verbatim
		}
		if v.AllValue != "" {
			values = []string{v.AllValue}
		} else {
			values = v.AllOptions
		}
		texts = values
		// All with no known options (values couldn't be listed): match
		// everything rather than nothing.
		if len(values) == 0 && (format == "" || format == "regex" || format == "glob" || format == "lucene") {
			f := format
			if f == "" {
				f = ip.DefaultFormat
			}
			switch f {
			case "regex":
				return ".*"
			case "glob", "lucene":
				return "*"
			}
		}
	}
	if format == "text" {
		if len(texts) == 0 {
			texts = values
		}
		return strings.Join(texts, " + ")
	}
	if format == "" {
		if !v.Multi && !v.IncludeAll {
			// Single-value variables are substituted (almost) raw.
			if len(values) == 0 {
				return ""
			}
			switch ip.DefaultFormat {
			case "regex":
				return promRegularEscape(values[0])
			case "sqlstring":
				return values[0]
			}
			return values[0]
		}
		format = ip.DefaultFormat
		if format == "" {
			format = "glob"
		}
	}
	return applyFormat(values, format)
}

func applyFormat(values []string, format string) string {
	if len(values) == 0 {
		return ""
	}
	join := func(f func(string) string, sep string) string {
		out := make([]string, len(values))
		for i, v := range values {
			out[i] = f(v)
		}
		return strings.Join(out, sep)
	}
	id := func(s string) string { return s }
	switch format {
	case "raw":
		return join(id, ",")
	case "regex":
		if len(values) == 1 {
			return promSpecialRegexEscape(values[0])
		}
		return "(" + join(promSpecialRegexEscape, "|") + ")"
	case "pipe":
		return join(id, "|")
	case "csv":
		return join(id, ",")
	case "distributed":
		return join(id, ",")
	case "json":
		b, _ := json.Marshal(values)
		return string(b)
	case "glob":
		if len(values) == 1 {
			return values[0]
		}
		return "{" + join(id, ",") + "}"
	case "lucene":
		if len(values) == 1 {
			return luceneEscape(values[0])
		}
		return "(" + join(func(s string) string { return `"` + luceneEscape(s) + `"` }, " OR ") + ")"
	case "sqlstring":
		return join(func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }, ",")
	case "singlequote":
		return join(func(s string) string { return "'" + strings.ReplaceAll(s, "'", `\'`) + "'" }, ",")
	case "doublequote":
		return join(func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }, ",")
	case "queryparam":
		return join(func(s string) string { return "var-" + url.QueryEscape(s) }, "&")
	case "percentencode":
		return join(url.QueryEscape, ",")
	case "text":
		return join(id, " + ")
	}
	return join(id, ",")
}

// promRegularEscape mirrors Grafana's prometheusRegularEscape.
func promRegularEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `'`, `\\'`)
}

// promSpecialRegexEscape mirrors Grafana's prometheusSpecialRegexEscape.
func promSpecialRegexEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\\\`)
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`$^*{}[]'+?.()|`, r) {
			b.WriteString(`\\`)
		}
		b.WriteRune(r)
	}
	return b.String()
}

func luceneEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`+-=&|><!(){}[]^"~*?:\/ `, r) {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// InterpolateTarget walks a query target and interpolates every string,
// leaving refId and the datasource reference alone.
func (ip *Interpolator) InterpolateTarget(target map[string]any) map[string]any {
	out := make(map[string]any, len(target))
	for k, v := range target {
		switch k {
		case "refId", "datasource", "datasourceId", "hide":
			out[k] = v
		default:
			out[k] = ip.walk(v)
		}
	}
	return out
}

func (ip *Interpolator) walk(v any) any {
	switch t := v.(type) {
	case string:
		return ip.Replace(t)
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, vv := range t {
			m[k] = ip.walk(vv)
		}
		return m
	case []any:
		a := make([]any, len(t))
		for i, vv := range t {
			a[i] = ip.walk(vv)
		}
		return a
	}
	return v
}

// ── time ────────────────────────────────────────────────────────────

var relRE = regexp.MustCompile(`^now(?:([+-])(\d+)([smhdwMy]))?(?:/([smhdwMy]))?$`)

// ParseTime parses a Grafana time expression: now, now-6h, now/d,
// now-1d/d, epoch milliseconds or RFC3339. roundUp rounds "/unit" to the end
// of the unit (used for "to").
func ParseTime(s string, now time.Time, roundUp bool) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return now, nil
	}
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.UnixMilli(ms), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t, nil
	}
	m := relRE.FindStringSubmatch(strings.ReplaceAll(s, " ", ""))
	if m == nil {
		return time.Time{}, fmt.Errorf("invalid time %q", s)
	}
	t := now
	if m[2] != "" {
		n, _ := strconv.Atoi(m[2])
		if m[1] == "-" {
			n = -n
		}
		t = addUnit(t, n, m[3])
	}
	if m[4] != "" {
		t = roundTo(t, m[4], roundUp)
	}
	return t, nil
}

func addUnit(t time.Time, n int, unit string) time.Time {
	switch unit {
	case "s":
		return t.Add(time.Duration(n) * time.Second)
	case "m":
		return t.Add(time.Duration(n) * time.Minute)
	case "h":
		return t.Add(time.Duration(n) * time.Hour)
	case "d":
		return t.AddDate(0, 0, n)
	case "w":
		return t.AddDate(0, 0, 7*n)
	case "M":
		return t.AddDate(0, n, 0)
	case "y":
		return t.AddDate(n, 0, 0)
	}
	return t
}

func roundTo(t time.Time, unit string, up bool) time.Time {
	var start time.Time
	switch unit {
	case "s":
		start = t.Truncate(time.Second)
	case "m":
		start = t.Truncate(time.Minute)
	case "h":
		start = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, t.Location())
	case "d":
		start = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	case "w":
		d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
		wd := (int(d.Weekday()) + 6) % 7 // Monday-based
		start = d.AddDate(0, 0, -wd)
	case "M":
		start = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
	case "y":
		start = time.Date(t.Year(), 1, 1, 0, 0, 0, 0, t.Location())
	default:
		return t
	}
	if !up {
		return start
	}
	return addUnit(start, 1, unit).Add(-time.Millisecond)
}

// ParseRange resolves a from/to pair.
func ParseRange(from, to string, now time.Time) (TimeRange, error) {
	if from == "" {
		from = "now-6h"
	}
	if to == "" {
		to = "now"
	}
	f, err := ParseTime(from, now, false)
	if err != nil {
		return TimeRange{}, err
	}
	t, err := ParseTime(to, now, true)
	if err != nil {
		return TimeRange{}, err
	}
	if !f.Before(t) {
		return TimeRange{}, fmt.Errorf("time range: from must be before to")
	}
	return TimeRange{From: f, To: t}, nil
}

// ParseDuration parses Grafana interval strings: 15s, 1m, 2h, 1d, 1w, 1y,
// 500ms. A leading ">" (min interval marker) is ignored.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), ">")
	if s == "" {
		return 0, fmt.Errorf("empty interval")
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	m := regexp.MustCompile(`^(\d+(?:\.\d+)?)(ms|s|m|h|d|w|M|y)$`).FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("invalid interval %q", s)
	}
	n, _ := strconv.ParseFloat(m[1], 64)
	unit := map[string]time.Duration{
		"ms": time.Millisecond, "s": time.Second, "m": time.Minute, "h": time.Hour,
		"d": 24 * time.Hour, "w": 7 * 24 * time.Hour, "M": 30 * 24 * time.Hour, "y": 365 * 24 * time.Hour,
	}[m[2]]
	return time.Duration(n * float64(unit)), nil
}

// niceIntervals is Grafana's kbn.round_interval ladder.
var niceIntervals = []time.Duration{
	10 * time.Millisecond, 20 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond,
	200 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 5 * time.Second,
	10 * time.Second, 15 * time.Second, 20 * time.Second, 30 * time.Second, time.Minute,
	2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 20 * time.Minute,
	30 * time.Minute, time.Hour, 2 * time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour,
	24 * time.Hour, 7 * 24 * time.Hour, 30 * 24 * time.Hour, 365 * 24 * time.Hour,
}

// CalcInterval picks the query interval for a range drawn over maxDataPoints
// points, rounded to a nice step and floored at minInterval.
func CalcInterval(r TimeRange, maxDataPoints int, minInterval time.Duration) time.Duration {
	if maxDataPoints <= 0 {
		maxDataPoints = 1000
	}
	raw := r.To.Sub(r.From) / time.Duration(maxDataPoints)
	iv := niceIntervals[len(niceIntervals)-1]
	for _, n := range niceIntervals {
		if n >= raw {
			iv = n
			break
		}
	}
	if minInterval > iv {
		iv = minInterval
	}
	if iv < time.Millisecond {
		iv = time.Millisecond
	}
	return iv
}

// FormatDuration renders a duration the way Grafana writes intervals (1m,
// 30s, 2h, 1d).
func FormatDuration(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	case d%time.Second == 0:
		return fmt.Sprintf("%ds", d/time.Second)
	}
	return fmt.Sprintf("%dms", d/time.Millisecond)
}

// ── variable option helpers ─────────────────────────────────────────

// Option is one selectable value.
type Option struct {
	Text  string `json:"text"`
	Value string `json:"value"`
}

// ApplyRegex filters/extracts options with a Grafana variable regex
// (/pattern/flags). A capture group (or named groups text/value) picks the
// value. Patterns Go's RE2 cannot compile (look-arounds) leave the options
// unfiltered.
func ApplyRegex(opts []Option, pattern string) []Option {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return opts
	}
	flags := ""
	if strings.HasPrefix(pattern, "/") {
		if i := strings.LastIndex(pattern, "/"); i > 0 {
			flags = pattern[i+1:]
			pattern = pattern[1:i]
		}
	}
	if strings.Contains(flags, "i") {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return opts
	}
	textIdx, valueIdx := re.SubexpIndex("text"), re.SubexpIndex("value")
	var out []Option
	seen := map[string]bool{}
	for _, o := range opts {
		m := re.FindStringSubmatch(o.Value)
		if m == nil {
			continue
		}
		n := o
		switch {
		case textIdx > 0 || valueIdx > 0:
			if valueIdx > 0 && m[valueIdx] != "" {
				n.Value = m[valueIdx]
			}
			if textIdx > 0 && m[textIdx] != "" {
				n.Text = m[textIdx]
			} else {
				n.Text = n.Value
			}
		case len(m) > 1:
			n = Option{Text: m[1], Value: m[1]}
		}
		if seen[n.Value] {
			continue
		}
		seen[n.Value] = true
		out = append(out, n)
	}
	if out == nil {
		out = []Option{}
	}
	return out
}

// SortOptions applies Grafana's variable sort modes.
func SortOptions(opts []Option, mode int) {
	if mode == 0 {
		return
	}
	numeric := func(s string) float64 {
		re := regexp.MustCompile(`-?\d+(\.\d+)?`)
		if m := re.FindString(s); m != "" {
			f, _ := strconv.ParseFloat(m, 64)
			return f
		}
		return math.Inf(-1)
	}
	less := func(i, j int) bool {
		a, b := opts[i].Text, opts[j].Text
		switch mode {
		case 1:
			return a < b
		case 2:
			return a > b
		case 3:
			return numeric(a) < numeric(b)
		case 4:
			return numeric(a) > numeric(b)
		case 5:
			return strings.ToLower(a) < strings.ToLower(b)
		case 6:
			return strings.ToLower(a) > strings.ToLower(b)
		case 7:
			return naturalLess(a, b)
		case 8:
			return naturalLess(b, a)
		}
		return false
	}
	sort.SliceStable(opts, less)
}

var chunkRE = regexp.MustCompile(`\d+|\D+`)

func naturalLess(a, b string) bool {
	ca, cb := chunkRE.FindAllString(a, -1), chunkRE.FindAllString(b, -1)
	for i := 0; i < len(ca) && i < len(cb); i++ {
		na, ea := strconv.Atoi(ca[i])
		nb, eb := strconv.Atoi(cb[i])
		if ea == nil && eb == nil {
			if na != nb {
				return na < nb
			}
			continue
		}
		if ca[i] != cb[i] {
			return ca[i] < cb[i]
		}
	}
	return len(ca) < len(cb)
}
