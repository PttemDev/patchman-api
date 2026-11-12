package esquery

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"patchman-be/internal/models"
)

// Mapping returns the flattened field list for an index (or index pattern /
// alias). It reads _field_caps for the type/searchable/aggregatable facts and
// _mapping to detect nested types and keyword multi-fields.
func (r *Registry) Mapping(ctx context.Context, conn models.ESConnection, index string) ([]Field, error) {
	idx := strings.TrimSpace(index)
	if idx == "" {
		idx = "_all"
	}
	esc := url.PathEscape(idx)

	fcRes, err := r.rawDo(ctx, conn, http.MethodGet, "/"+esc+"/_field_caps?fields=*", nil)
	if err != nil {
		return nil, err
	}
	if fcRes.Status < 200 || fcRes.Status >= 300 {
		return nil, upstreamError(fcRes)
	}

	// _field_caps → { fields: { "<path>": { "<type>": {searchable, aggregatable} } } }
	var fc struct {
		Fields map[string]map[string]struct {
			Type         string `json:"type"`
			Searchable   bool   `json:"searchable"`
			Aggregatable bool   `json:"aggregatable"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(fcRes.Body, &fc); err != nil {
		return nil, err
	}

	// Nested-field set from _mapping (best effort — never fatal).
	nested := r.nestedPaths(ctx, conn, esc)

	out := make([]Field, 0, len(fc.Fields))
	for path, byType := range fc.Fields {
		if strings.HasPrefix(path, "_") {
			// Skip metadata fields (_id, _index, _source, …) from mapping view.
			continue
		}
		for t, caps := range byType {
			out = append(out, Field{
				Path:         path,
				Type:         t,
				Searchable:   caps.Searchable,
				Aggregatable: caps.Aggregatable,
				IsKeyword:    t == "keyword",
				IsNested:     nested[path] || t == "nested",
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path == out[j].Path {
			return out[i].Type < out[j].Type
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

// nestedPaths walks _mapping to collect dotted paths whose type is "nested".
// Failure returns an empty set rather than an error — _field_caps already
// carries the essential facts.
func (r *Registry) nestedPaths(ctx context.Context, conn models.ESConnection, esc string) map[string]bool {
	nested := map[string]bool{}
	res, err := r.rawDo(ctx, conn, http.MethodGet, "/"+esc+"/_mapping", nil)
	if err != nil || res.Status < 200 || res.Status >= 300 {
		return nested
	}
	// { "<index>": { "mappings": { "properties": {...} } } }
	var byIndex map[string]struct {
		Mappings struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"mappings"`
	}
	if err := json.Unmarshal(res.Body, &byIndex); err != nil {
		return nested
	}
	for _, idx := range byIndex {
		collectNested(idx.Mappings.Properties, "", nested)
	}
	return nested
}

// collectNested recurses a _mapping properties tree, recording dotted paths
// whose declared type is "nested".
func collectNested(props map[string]json.RawMessage, prefix string, nested map[string]bool) {
	for name, raw := range props {
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		var node struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(raw, &node); err != nil {
			continue
		}
		if node.Type == "nested" {
			nested[path] = true
		}
		if len(node.Properties) > 0 {
			collectNested(node.Properties, path, nested)
		}
	}
}
