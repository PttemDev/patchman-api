package mcp

// Compact tool replies: every token a tool returns lands in the agent's
// context, so results are trimmed before they leave the server —
//   - empty values (null, "", [], {}) are dropped: they carry no information;
//   - Kubernetes bookkeeping no reader needs is dropped: metadata.managedFields
//     and the kubectl last-applied-configuration annotation (kubectl hides the
//     first by default; the second repeats the whole object as a string);
//   - a reply still over maxReplyBytes keeps the first items of its biggest
//     list that fit and says how many were left out, so the agent can narrow
//     the query instead of the client rejecting an oversized result.
// Non-JSON bodies pass through untouched.

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// maxReplyBytes keeps a reply under typical client limits (Claude Code's
// default MCP output cap is 25k tokens ≈ 100 KB) with room to spare.
const maxReplyBytes = 80 * 1024

const lastApplied = "kubectl.kubernetes.io/last-applied-configuration"

func compactReply(body string) string {
	trimmed := bytes.TrimSpace([]byte(body))
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return body
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber() // keep big ids / exact numbers as sent
	var v any
	if err := dec.Decode(&v); err != nil {
		return body
	}
	v = prune(v)
	out, err := json.Marshal(v)
	if err != nil {
		return body
	}
	if len(out) > maxReplyBytes {
		if cut, ok := capLargestList(v, len(out)); ok {
			if b, err := json.Marshal(cut); err == nil {
				out = b
			}
		}
	}
	if len(out) >= len(trimmed) {
		return body // never make a reply bigger (e.g. pretty input was already tight)
	}
	return string(out)
}

// prune drops empty values and Kubernetes bookkeeping, recursively.
func prune(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == "managedFields" || k == lastApplied {
				delete(t, k)
				continue
			}
			p := prune(val)
			if isEmpty(p) {
				delete(t, k)
				continue
			}
			t[k] = p
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = prune(val)
		}
		return t
	default:
		return v
	}
}

func isEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

// capLargestList shortens the biggest array in the reply (the list the agent
// asked for) to what fits, and records what was left out next to it.
func capLargestList(root any, size int) (any, bool) {
	var best *[]any
	var bestParent map[string]any
	var bestKey string
	bestLen := 0
	var walk func(v any, parent map[string]any, key string)
	walk = func(v any, parent map[string]any, key string) {
		switch t := v.(type) {
		case map[string]any:
			for k, val := range t {
				walk(val, t, k)
			}
		case []any:
			if b, _ := json.Marshal(t); len(b) > bestLen && len(t) > 1 {
				bestLen, bestParent, bestKey = len(b), parent, key
				arr := t
				best = &arr
			}
			for _, val := range t {
				walk(val, nil, "")
			}
		}
	}
	walk(root, nil, "")
	if best == nil {
		return root, false
	}
	items := *best
	budget := maxReplyBytes - (size - bestLen) - 512
	kept, used := 0, 2
	for _, it := range items {
		b, _ := json.Marshal(it)
		if used+len(b)+1 > budget {
			break
		}
		used += len(b) + 1
		kept++
	}
	if kept == 0 {
		kept = 1
	}
	note := fmt.Sprintf("showing %d of %d items to keep the reply small — narrow the query (filters, limit, a more specific name) to see the rest", kept, len(items))
	if bestParent == nil {
		// The reply itself is the list.
		return map[string]any{"items": items[:kept], "truncated": note}, true
	}
	bestParent[bestKey] = items[:kept]
	bestParent["truncated"] = note
	return root, true
}
