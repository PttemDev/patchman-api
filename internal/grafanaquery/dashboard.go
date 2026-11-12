package grafanaquery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"patchman-be/internal/models"
)

// DashboardMeta is the slice of /api/dashboards/uid meta the FE shows.
type DashboardMeta struct {
	URL         string `json:"url,omitempty"`
	Slug        string `json:"slug,omitempty"`
	FolderUID   string `json:"folderUid,omitempty"`
	FolderTitle string `json:"folderTitle,omitempty"`
	Version     int    `json:"version,omitempty"`
	// Schema is "v1" (classic JSON) or "v2" (Grafana 12 dynamic dashboard,
	// converted to the classic shape by this package).
	Schema string `json:"schema"`
}

// Dashboard is a dashboard in the classic (v1) JSON model, whatever the
// server stored, with library panels resolved in place.
type Dashboard struct {
	Dashboard map[string]any `json:"dashboard"`
	Meta      DashboardMeta  `json:"meta"`
}

// GetDashboard loads a dashboard by uid. Classic /api/dashboards/uid first —
// Grafana 13 converts v2 dashboards there itself. When the legacy endpoint
// refuses a v2 dashboard (Grafana 12.0/12.1 answer 406 / "use /apis…"), it
// reads the k8s-style API and converts the v2 spec here.
func (r *Registry) GetDashboard(ctx context.Context, conn models.GrafanaConnection, uid string) (*Dashboard, error) {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return nil, fmt.Errorf("dashboard uid is required")
	}
	var legacy struct {
		Dashboard map[string]any `json:"dashboard"`
		Meta      struct {
			URL         string `json:"url"`
			Slug        string `json:"slug"`
			FolderUID   string `json:"folderUid"`
			FolderTitle string `json:"folderTitle"`
			Version     int    `json:"version"`
		} `json:"meta"`
	}
	err := r.DoJSON(ctx, conn, http.MethodGet, "/api/dashboards/uid/"+url.PathEscape(uid), nil, nil, &legacy)
	if err == nil && legacy.Dashboard != nil && !isV2Spec(legacy.Dashboard) {
		d := &Dashboard{Dashboard: legacy.Dashboard, Meta: DashboardMeta{
			URL: legacy.Meta.URL, Slug: legacy.Meta.Slug, FolderUID: legacy.Meta.FolderUID,
			FolderTitle: legacy.Meta.FolderTitle, Version: legacy.Meta.Version, Schema: "v1",
		}}
		r.resolveLibraryPanels(ctx, conn, d.Dashboard)
		return d, nil
	}
	if err != nil && !looksLikeV2Refusal(err) {
		return nil, err
	}
	// v2 path.
	info, ierr := r.Info(ctx, conn)
	ns := "default"
	if ierr == nil && info.Namespace != "" {
		ns = info.Namespace
	}
	var lastErr error = err
	for _, ver := range []string{"v2", "v2beta1", "v2alpha1"} {
		var obj struct {
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
				Generation  int               `json:"generation"`
			} `json:"metadata"`
			Spec map[string]any `json:"spec"`
		}
		p := fmt.Sprintf("/apis/dashboard.grafana.app/%s/namespaces/%s/dashboards/%s", ver, url.PathEscape(ns), url.PathEscape(uid))
		if e := r.DoJSON(ctx, conn, http.MethodGet, p, nil, nil, &obj); e != nil {
			lastErr = e
			continue
		}
		dash := ConvertV2(obj.Spec)
		dash["uid"] = uid
		d := &Dashboard{Dashboard: dash, Meta: DashboardMeta{
			FolderUID: obj.Metadata.Annotations["grafana.app/folder"], Version: obj.Metadata.Generation,
			URL: "/d/" + uid, Schema: "v2",
		}}
		r.resolveLibraryPanels(ctx, conn, d.Dashboard)
		return d, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("dashboard %s not found", uid)
	}
	return nil, lastErr
}

func isV2Spec(d map[string]any) bool {
	_, hasElements := d["elements"]
	_, hasLayout := d["layout"]
	_, hasPanels := d["panels"]
	return hasElements && hasLayout && !hasPanels
}

func looksLikeV2Refusal(err error) bool {
	ae, ok := AsAPIError(err)
	if !ok {
		return false
	}
	if ae.Status == http.StatusNotAcceptable {
		return true
	}
	m := strings.ToLower(ae.Message)
	return strings.Contains(m, "/apis/") || strings.Contains(m, "v2") || strings.Contains(m, "dashboard api version")
}

// resolveLibraryPanels replaces library-panel stubs (8.0+) with their model,
// keeping the placement (gridPos/id) of the stub. Best effort: an unreadable
// library element stays a stub and the FE shows it as unsupported.
func (r *Registry) resolveLibraryPanels(ctx context.Context, conn models.GrafanaConnection, dash map[string]any) {
	cache := map[string]map[string]any{}
	var walk func(panels []any)
	walk = func(panels []any) {
		for i, p := range panels {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if nested, ok := pm["panels"].([]any); ok {
				walk(nested)
			}
			lib, ok := pm["libraryPanel"].(map[string]any)
			if !ok {
				continue
			}
			uid, _ := lib["uid"].(string)
			if uid == "" {
				continue
			}
			model, ok := cache[uid]
			if !ok {
				var res struct {
					Result struct {
						Model map[string]any `json:"model"`
					} `json:"result"`
				}
				if r.DoJSON(ctx, conn, http.MethodGet, "/api/library-elements/"+url.PathEscape(uid), nil, nil, &res) == nil {
					model = res.Result.Model
				}
				cache[uid] = model
			}
			if model == nil {
				continue
			}
			merged := map[string]any{}
			for k, v := range model {
				merged[k] = v
			}
			for _, k := range []string{"gridPos", "id", "libraryPanel"} {
				if v, ok := pm[k]; ok {
					merged[k] = v
				}
			}
			if t, ok := pm["title"].(string); ok && t != "" {
				merged["title"] = t
			}
			panels[i] = merged
		}
	}
	if panels, ok := dash["panels"].([]any); ok {
		walk(panels)
	}
}

// ── v2 → classic conversion ─────────────────────────────────────────

// ConvertV2 turns a Grafana 12+ v2 dashboard spec (elements + layout) into the
// classic JSON model the FE renders: rows become row panels, tabs become
// rows, auto grids get computed gridPos. Handles both v2alpha1 query shape
// ({kind: <ds type>, spec}) and v2beta1+/v2 ({kind: DataQuery, group,
// datasource.name, spec}).
func ConvertV2(spec map[string]any) map[string]any {
	out := map[string]any{
		"title":   spec["title"],
		"tags":    spec["tags"],
		"panels":  []any{},
		"version": 0,
	}
	if ts, ok := spec["timeSettings"].(map[string]any); ok {
		out["time"] = map[string]any{"from": ts["from"], "to": ts["to"]}
		if ar, ok := ts["autoRefresh"].(string); ok && ar != "" {
			out["refresh"] = ar
		}
		if tz, ok := ts["timezone"].(string); ok {
			out["timezone"] = tz
		}
	}
	elements, _ := spec["elements"].(map[string]any)
	var panels []any
	y := 0
	nextID := 1000
	panelFor := func(name string, x, py, w, h int) map[string]any {
		el, _ := elements[name].(map[string]any)
		p := convertV2Element(el)
		if p == nil {
			return nil
		}
		if _, ok := p["id"]; !ok {
			nextID++
			p["id"] = nextID
		}
		p["gridPos"] = map[string]any{"x": x, "y": py, "w": w, "h": h}
		return p
	}

	var emitLayout func(layout map[string]any, baseY int, into *[]any) int
	emitLayout = func(layout map[string]any, baseY int, into *[]any) int {
		kind, _ := layout["kind"].(string)
		lspec, _ := layout["spec"].(map[string]any)
		maxY := baseY
		switch kind {
		case "GridLayout":
			items, _ := lspec["items"].([]any)
			for _, it := range items {
				im, _ := it.(map[string]any)
				is, _ := im["spec"].(map[string]any)
				name := refName(is["element"])
				x, py, w, h := num(is["x"]), num(is["y"]), num(is["width"]), num(is["height"])
				if w == 0 {
					w = 12
				}
				if h == 0 {
					h = 8
				}
				if p := panelFor(name, x, baseY+py, w, h); p != nil {
					*into = append(*into, p)
				}
				if baseY+py+h > maxY {
					maxY = baseY + py + h
				}
			}
		case "AutoGridLayout":
			cols := num(lspec["maxColumnCount"])
			if cols <= 0 {
				cols = 3
			}
			if cols > 24 {
				cols = 24
			}
			w := 24 / cols
			h := 9
			if m, _ := lspec["rowHeightMode"].(string); m == "short" {
				h = 6
			} else if m == "tall" {
				h = 12
			}
			items, _ := lspec["items"].([]any)
			for i, it := range items {
				im, _ := it.(map[string]any)
				is, _ := im["spec"].(map[string]any)
				name := refName(is["element"])
				py := baseY + (i/cols)*h
				if p := panelFor(name, (i%cols)*w, py, w, h); p != nil {
					*into = append(*into, p)
				}
				if py+h > maxY {
					maxY = py + h
				}
			}
		case "RowsLayout", "TabsLayout":
			key, childKind := "rows", "collapse"
			if kind == "TabsLayout" {
				key, childKind = "tabs", ""
			}
			children, _ := lspec[key].([]any)
			cur := baseY
			for _, ch := range children {
				cm, _ := ch.(map[string]any)
				cs, _ := cm["spec"].(map[string]any)
				title, _ := cs["title"].(string)
				collapsed := false
				if childKind != "" {
					collapsed, _ = cs[childKind].(bool)
				}
				nextID++
				row := map[string]any{
					"type": "row", "id": nextID, "title": title, "collapsed": collapsed,
					"gridPos": map[string]any{"x": 0, "y": cur, "w": 24, "h": 1},
				}
				inner, _ := cs["layout"].(map[string]any)
				var rowPanels []any
				end := emitLayout(inner, cur+1, &rowPanels)
				if collapsed {
					row["panels"] = rowPanels
					*into = append(*into, row)
					cur++
				} else {
					row["panels"] = []any{}
					*into = append(*into, row)
					*into = append(*into, rowPanels...)
					cur = end
				}
			}
			maxY = cur
		}
		return maxY
	}
	if layout, ok := spec["layout"].(map[string]any); ok {
		y = emitLayout(layout, 0, &panels)
	}
	_ = y
	out["panels"] = panels

	var vars []any
	if list, ok := spec["variables"].([]any); ok {
		for _, v := range list {
			if cv := convertV2Variable(v); cv != nil {
				vars = append(vars, cv)
			}
		}
	}
	out["templating"] = map[string]any{"list": vars}
	return out
}

func refName(v any) string {
	m, _ := v.(map[string]any)
	n, _ := m["name"].(string)
	return n
}

func num(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

func convertV2Element(el map[string]any) map[string]any {
	if el == nil {
		return nil
	}
	kind, _ := el["kind"].(string)
	spec, _ := el["spec"].(map[string]any)
	p := map[string]any{}
	if id, ok := spec["id"]; ok {
		p["id"] = id
	}
	p["title"] = spec["title"]
	if d, ok := spec["description"]; ok {
		p["description"] = d
	}
	if kind == "LibraryPanel" {
		p["type"] = "library-panel-stub"
		p["libraryPanel"] = spec["libraryPanel"]
		return p
	}
	viz, _ := spec["vizConfig"].(map[string]any)
	vtype, _ := viz["group"].(string)
	if vtype == "" {
		vtype, _ = viz["kind"].(string) // v2alpha1: kind = plugin id
	}
	if vtype == "VizConfig" {
		vtype = ""
	}
	p["type"] = vtype
	if vs, ok := viz["spec"].(map[string]any); ok {
		p["options"] = vs["options"]
		p["fieldConfig"] = vs["fieldConfig"]
	}
	data, _ := spec["data"].(map[string]any)
	dspec, _ := data["spec"].(map[string]any)
	var targets []any
	if qs, ok := dspec["queries"].([]any); ok {
		for _, q := range qs {
			qm, _ := q.(map[string]any)
			qspec, _ := qm["spec"].(map[string]any)
			t := convertV2Query(qspec)
			if t != nil {
				targets = append(targets, t)
			}
		}
	}
	p["targets"] = targets
	if tr, ok := dspec["transformations"]; ok {
		p["transformations"] = tr
	}
	if qo, ok := dspec["queryOptions"].(map[string]any); ok {
		for _, k := range []string{"maxDataPoints", "interval", "timeFrom", "timeShift"} {
			if v, ok := qo[k]; ok {
				p[k] = v
			}
		}
	}
	return p
}

// convertV2Query maps a PanelQuery spec to a classic target.
func convertV2Query(qspec map[string]any) map[string]any {
	if qspec == nil {
		return nil
	}
	t := map[string]any{}
	query, _ := qspec["query"].(map[string]any)
	inner, _ := query["spec"].(map[string]any)
	for k, v := range inner {
		t[k] = v
	}
	if ref, ok := qspec["refId"]; ok {
		t["refId"] = ref
	}
	if h, ok := qspec["hidden"].(bool); ok && h {
		t["hide"] = true
	}
	dsType, _ := query["group"].(string)
	if qk, _ := query["kind"].(string); dsType == "" && qk != "" && qk != "DataQuery" {
		dsType = qk // v2alpha1
	}
	ds := map[string]any{}
	if dsType != "" {
		ds["type"] = dsType
	}
	if dn, ok := query["datasource"].(map[string]any); ok {
		if n, ok := dn["name"].(string); ok {
			ds["uid"] = n
		}
	}
	if old, ok := qspec["datasource"].(map[string]any); ok { // v2alpha1
		for k, v := range old {
			ds[k] = v
		}
	}
	if len(ds) > 0 {
		t["datasource"] = ds
	}
	return t
}

var v2VarKinds = map[string]string{
	"QueryVariable": "query", "CustomVariable": "custom", "ConstantVariable": "constant",
	"IntervalVariable": "interval", "TextVariable": "textbox", "DatasourceVariable": "datasource",
	"AdhocVariable": "adhoc", "GroupByVariable": "groupby",
}

var v2Hide = map[string]int{"dontHide": 0, "hideLabel": 1, "hideVariable": 2}

var v2Refresh = map[string]int{"never": 0, "onDashboardLoad": 1, "onTimeRangeChanged": 2}

var v2Sort = map[string]int{
	"disabled": 0, "alphabeticalAsc": 1, "alphabeticalDesc": 2, "numericalAsc": 3, "numericalDesc": 4,
	"alphabeticalCaseInsensitiveAsc": 5, "alphabeticalCaseInsensitiveDesc": 6, "naturalAsc": 7, "naturalDesc": 8,
}

func convertV2Variable(v any) map[string]any {
	vm, _ := v.(map[string]any)
	kind, _ := vm["kind"].(string)
	typ, ok := v2VarKinds[kind]
	if !ok {
		return nil
	}
	spec, _ := vm["spec"].(map[string]any)
	out := map[string]any{"type": typ}
	for k, val := range spec {
		switch k {
		case "hide":
			if s, ok := val.(string); ok {
				out["hide"] = v2Hide[s]
				continue
			}
		case "refresh":
			if s, ok := val.(string); ok {
				out["refresh"] = v2Refresh[s]
				continue
			}
		case "sort":
			if s, ok := val.(string); ok {
				out["sort"] = v2Sort[s]
				continue
			}
		case "pluginId":
			out["query"] = val // datasource variable: plugin id is the classic `query`
			continue
		case "query":
			if qm, ok := val.(map[string]any); ok && typ == "query" {
				inner, _ := qm["spec"].(map[string]any)
				if legacy, ok := inner["__legacyStringValue"].(string); ok {
					out["query"] = legacy
				} else {
					out["query"] = inner
				}
				dsType, _ := qm["group"].(string)
				ds := map[string]any{}
				if dsType != "" {
					ds["type"] = dsType
				}
				if dn, ok := qm["datasource"].(map[string]any); ok {
					if n, ok := dn["name"].(string); ok {
						ds["uid"] = n
					}
				}
				if len(ds) > 0 {
					out["datasource"] = ds
				}
				continue
			}
		}
		out[k] = val
	}
	if old, ok := spec["datasource"].(map[string]any); ok && typ == "query" {
		out["datasource"] = old // v2alpha1 kept the classic ref
	}
	return out
}
