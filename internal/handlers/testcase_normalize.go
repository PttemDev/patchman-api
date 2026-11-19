package handlers

import (
	"encoding/json"
	"strings"
)

// The QA UI stores test-case tags as a comma-separated string, the type as one
// of its four categories, and steps as a JSON array whose items carry a step
// type. API callers (MCP agents above all) sent tags as a JSON array string,
// type manual/automated and steps without a type, which the editor showed as
// a raw string, an empty Type and "stepKind.undefined". These helpers bring
// every write to the shape the UI reads.

// testCaseTypes are the categories the editor's Type select offers.
var testCaseTypes = map[string]bool{"functional": true, "integration": true, "performance": true, "security": true}

// normalizeTestCaseType maps empty and the legacy manual/automated values
// (the old default) to "functional"; any other value is kept lower-cased.
func normalizeTestCaseType(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	if t == "" || t == "manual" || t == "automated" {
		return "functional"
	}
	return t
}

// normalizeTestCaseTags turns a JSON array string (["a","b"]) into "a,b" and
// trims a comma list; anything else is returned trimmed.
func normalizeTestCaseTags(s string) string {
	s = strings.TrimSpace(s)
	var list []string
	if strings.HasPrefix(s, "[") {
		var arr []any
		if json.Unmarshal([]byte(s), &arr) != nil {
			return s
		}
		for _, v := range arr {
			if str, ok := v.(string); ok {
				list = append(list, str)
			}
		}
	} else {
		list = strings.Split(s, ",")
	}
	out := make([]string, 0, len(list))
	for _, t := range list {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, ",")
}

// normalizeTestCaseSteps fills a missing stepNo (its position) and infers a
// missing type from the fields the step carries: request → "request",
// assertions → "assertion", extractions → "extract", waitMs → "wait". A step
// with only action/expected stays untyped: a manual step, shown as such in the
// UI and skipped by the runner. Input that isn't a JSON array of objects is
// returned unchanged.
func normalizeTestCaseSteps(s string) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	var steps []map[string]any
	if json.Unmarshal([]byte(s), &steps) != nil {
		return s
	}
	changed := false
	for i, st := range steps {
		if st == nil {
			continue
		}
		if n, ok := st["stepNo"].(float64); !ok || n <= 0 {
			st["stepNo"] = i + 1
			changed = true
		}
		if t, _ := st["type"].(string); t != "" {
			continue
		}
		switch {
		case st["request"] != nil:
			st["type"] = "request"
		case nonEmptyList(st["assertions"]):
			st["type"] = "assertion"
		case nonEmptyList(st["extractions"]):
			st["type"] = "extract"
		case st["waitMs"] != nil:
			st["type"] = "wait"
		default:
			continue
		}
		changed = true
	}
	if !changed {
		return s
	}
	b, err := json.Marshal(steps)
	if err != nil {
		return s
	}
	return string(b)
}

func nonEmptyList(v any) bool {
	l, ok := v.([]any)
	return ok && len(l) > 0
}
