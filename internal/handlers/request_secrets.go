package handlers

// What an AI agent (MCP) sees of a saved request and an environment: secret
// values — auth tokens and passwords, Authorization / Cookie / API-key
// headers, secret-looking params and body fields, secret environment
// variables — come back as "***" unless the collection (its top-level one)
// or the environment has "Show secret values to AI agents" on (off by
// default). An agent writing "***" back leaves the stored value as it was.

import (
	"encoding/json"
	"regexp"
	"strings"

	"patchman-be/internal/models"

	"gorm.io/gorm"
)

const secretMask = "***"

// secretKeyRE: names whose values are secrets (header, param, body field or
// auth property).
var secretKeyRE = regexp.MustCompile(`(?i)^(authorization|proxy-authorization|cookie|set-cookie)$|api[-_]?key|apikey|token|secret|passw(or)?d|pwd|session|credential|private[-_]?key|signature|x-auth`)

// varRefRE: a value that is only a {{variable}} reference leaks nothing and
// tells the agent which variable to set, so it is kept.
var varRefRE = regexp.MustCompile(`^\s*\{\{[^{}]+\}\}\s*$`)

func isSecretKey(k string) bool { return secretKeyRE.MatchString(strings.TrimSpace(k)) }

// masker collects the secrets it hid, in order, so a write-back of "***"
// can put the originals back.
type masker struct{ hidden []string }

func (m *masker) hide(v string) string {
	if v == "" || v == secretMask || varRefRE.MatchString(v) {
		return v
	}
	m.hidden = append(m.hidden, v)
	return secretMask
}

// walk masks secret-named leaves of a decoded JSON value. A {key, value}
// object (header / param / form row) is masked by its key.
func (m *masker) walk(v any, secretParent bool) any {
	switch x := v.(type) {
	case map[string]any:
		if k, ok := x["key"].(string); ok {
			if val, ok := x["value"].(string); ok && isSecretKey(k) {
				x["value"] = m.hide(val)
			}
			for kk, vv := range x {
				if kk != "key" && kk != "value" {
					x[kk] = m.walk(vv, false)
				}
			}
			return x
		}
		for _, kk := range sortedKeys(x) {
			x[kk] = m.walk(x[kk], isSecretKey(kk))
		}
		return x
	case []any:
		for i := range x {
			x[i] = m.walk(x[i], secretParent)
		}
		return x
	case string:
		if secretParent {
			return m.hide(x)
		}
		return x
	}
	return v
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Deterministic order: write-back matches hidden values by position.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

var (
	xmlSecretRE = regexp.MustCompile(`(?i)(<(?:[\w-]+:)?(?:password|passwd|pwd|token|secret|apikey|api_key|accesstoken)[^>]*>)([^<]+)(</)`)
	kvSecretRE  = regexp.MustCompile(`(?i)((?:^|[?&\s;])(?:password|passwd|pwd|token|access_token|api_key|apikey|secret|client_secret)=)([^&\s;]+)`)
)

// text masks a JSON document (by key) or, failing that, plain text: XML
// elements named like secrets (SOAP bodies) and key=value pairs.
func (m *masker) text(s string) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err == nil {
		if _, isStr := v.(string); !isStr {
			out := m.walk(v, false)
			b, err := marshalRaw(out)
			if err == nil {
				// The body field nests its content as a JSON string; mask it too.
				if mm, ok := out.(map[string]any); ok {
					if content, ok := mm["content"].(string); ok && content != "" {
						mm["content"] = m.text(content)
						if b2, err := marshalRaw(mm); err == nil {
							return string(b2)
						}
					}
				}
				return string(b)
			}
		}
	}
	s = xmlSecretRE.ReplaceAllStringFunc(s, func(x string) string {
		p := xmlSecretRE.FindStringSubmatch(x)
		return p[1] + m.hide(p[2]) + p[3]
	})
	return kvSecretRE.ReplaceAllStringFunc(s, func(x string) string {
		p := kvSecretRE.FindStringSubmatch(x)
		return p[1] + m.hide(p[2])
	})
}

// maskRequestSecrets returns a copy of req with its secret values masked.
func maskRequestSecrets(req models.SavedRequest) models.SavedRequest {
	req.URL = (&masker{}).text(req.URL)
	req.Headers = (&masker{}).text(req.Headers)
	req.Params = (&masker{}).text(req.Params)
	req.Body = (&masker{}).text(req.Body)
	req.Auth = maskAuth(req.Auth)
	return req
}

// maskAuth hides every string in the auth object except its type and the
// names that aren't secret (username, key / header name, where to add it).
func maskAuth(s string) string {
	var v map[string]any
	if strings.TrimSpace(s) == "" || json.Unmarshal([]byte(s), &v) != nil {
		return (&masker{}).text(s)
	}
	m := &masker{}
	var walk func(map[string]any)
	walk = func(o map[string]any) {
		for _, k := range sortedKeys(o) {
			switch x := o[k].(type) {
			case string:
				switch strings.ToLower(k) {
				case "type", "username", "user", "key", "addto", "in", "headername", "grant_type", "granttype", "tokenurl", "authurl", "scope", "algorithm", "region", "service", "prefix":
				default:
					o[k] = m.hide(x)
				}
			case map[string]any:
				walk(x)
			}
		}
	}
	walk(v)
	b, err := marshalRaw(v)
	if err != nil {
		return s
	}
	return string(b)
}

// errStillMasked: a write from an agent kept "***" in a field it changed,
// where PatchMan can't tell which hidden value it stands for.
type errStillMasked struct{ field string }

func (e errStillMasked) Error() string {
	return "the " + e.field + " still contains *** placeholders for hidden secret values. Leave the field out to keep it, keep the *** rows as they are, or send the real value."
}

// restoreRequestField resolves "***" in an agent's write of one request
// field against the stored value: header / param rows by key, auth fields by
// name; any other field kept only when it is exactly what the agent was shown.
func restoreRequestField(field, incoming, stored string) (string, error) {
	if !strings.Contains(incoming, secretMask) {
		return incoming, nil
	}
	var shown string
	if field == "auth" {
		shown = maskAuth(stored)
	} else {
		shown = (&masker{}).text(stored)
	}
	if incoming == shown || jsonEqual(incoming, shown) {
		return stored, nil
	}
	switch field {
	case "headers", "params":
		if out, ok := restoreRows(incoming, stored); ok {
			return out, nil
		}
	case "auth":
		if out, ok := restoreObject(incoming, stored); ok {
			return out, nil
		}
	}
	return "", errStillMasked{field}
}

// restoreRows: [{key, value, …}] rows whose value is "***" take the stored
// row's value with the same key.
func restoreRows(incoming, stored string) (string, bool) {
	var in, st []map[string]any
	if json.Unmarshal([]byte(incoming), &in) != nil || json.Unmarshal([]byte(stored), &st) != nil {
		return "", false
	}
	byKey := map[string]any{}
	for _, r := range st {
		if k, ok := r["key"].(string); ok {
			if _, seen := byKey[strings.ToLower(k)]; !seen {
				byKey[strings.ToLower(k)] = r["value"]
			}
		}
	}
	for _, r := range in {
		if v, _ := r["value"].(string); v == secretMask {
			k, _ := r["key"].(string)
			orig, ok := byKey[strings.ToLower(k)]
			if !ok {
				return "", false
			}
			r["value"] = orig
		}
	}
	b, err := marshalRaw(in)
	return string(b), err == nil
}

// restoreObject: object fields that are "***" take the stored field's value.
func restoreObject(incoming, stored string) (string, bool) {
	var in, st map[string]any
	if json.Unmarshal([]byte(incoming), &in) != nil || json.Unmarshal([]byte(stored), &st) != nil {
		return "", false
	}
	var fill func(a, b map[string]any) bool
	fill = func(a, b map[string]any) bool {
		for k, v := range a {
			switch x := v.(type) {
			case string:
				if x == secretMask {
					orig, ok := b[k]
					if !ok {
						return false
					}
					a[k] = orig
				}
			case map[string]any:
				bb, _ := b[k].(map[string]any)
				if !fill(x, bb) {
					return false
				}
			}
		}
		return true
	}
	if !fill(in, st) {
		return "", false
	}
	out, err := marshalRaw(in)
	return string(out), err == nil
}

// requestShowsSecrets reports whether the request's top-level collection lets
// AI agents see secret values.
func requestShowsSecrets(db *gorm.DB, collectionID string) bool {
	id := collectionID
	for i := 0; i < 32 && id != ""; i++ {
		var col models.Collection
		if db.Select("id", "parent_id", "mcp_show_secrets").First(&col, "id = ?", id).Error != nil {
			return false
		}
		if col.ParentID == nil || *col.ParentID == "" {
			return col.MCPShowSecrets
		}
		id = *col.ParentID
	}
	return false
}

// maskCollectionTree masks the requests of a collection and its folders.
func maskCollectionTree(col *models.Collection) {
	for i := range col.Requests {
		col.Requests[i] = maskRequestSecrets(col.Requests[i])
	}
	for i := range col.Children {
		maskCollectionTree(&col.Children[i])
	}
}

// marshalRaw is json.Marshal without HTML escaping: a SOAP / XML body must
// come back as <tags>, not \u003ctags\u003e.
func marshalRaw(v any) ([]byte, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(strings.TrimRight(b.String(), "\n")), nil
}

// jsonEqual: both are JSON with the same value (key order / spacing aside).
func jsonEqual(a, b string) bool {
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return string(xb) == string(yb)
}
