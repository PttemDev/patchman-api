package mcp

import (
	"encoding/json"
	"strings"
)

// leanCollections turns GET /api/collections into what an agent needs to
// find its way: the tree of collections and folders, each request as a
// one-line summary. Full requests (headers with tokens and cookies, bodies)
// made one big collection fill the reply alone, so the rest was cut off —
// and they put secrets in front of the agent for no reason; get_request
// gives one request in full. Filters: name (with ancestors and subtree),
// collectionId (a subtree), depth (folder levels), requests (summary |
// count | none). A body it can't read comes back unchanged.
func leanCollections(body string, in listCollectionsIn) string {
	var env struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil || env.Data == nil {
		return body
	}
	mode := strings.ToLower(strings.TrimSpace(in.Requests))
	if mode != "count" && mode != "none" {
		mode = "summary"
	}
	nodes := env.Data
	if id := strings.TrimSpace(in.CollectionID); id != "" {
		if n := findNode(nodes, id); n != nil {
			nodes = []map[string]any{n}
		} else {
			b, _ := json.Marshal(map[string]any{"data": []any{}, "note": "no collection or folder with id " + id + " — list_collections without collectionId shows the ids"})
			return string(b)
		}
	}
	q := strings.ToLower(strings.TrimSpace(in.Name))
	out := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		if l := leanNode(n, q, mode, in.Depth, 0); l != nil {
			out = append(out, l)
		}
	}
	res := map[string]any{"data": out}
	if q != "" && len(out) == 0 {
		res["note"] = "no collection or folder name contains \"" + in.Name + "\""
	}
	b, err := json.Marshal(res)
	if err != nil {
		return body
	}
	return string(b)
}

func children(n map[string]any) []map[string]any {
	raw, _ := n["children"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, c := range raw {
		if m, ok := c.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func findNode(list []map[string]any, id string) map[string]any {
	for _, n := range list {
		if s, _ := n["id"].(string); s == id {
			return n
		}
		if f := findNode(children(n), id); f != nil {
			return f
		}
	}
	return nil
}

// leanNode keeps id / name / type / description / baseUrl / sharing of a
// collection; with a name filter it returns nil for a branch with no match
// (a matching node keeps its whole subtree).
func leanNode(n map[string]any, q, mode string, maxDepth, depth int) map[string]any {
	name, _ := n["name"].(string)
	matched := q == "" || strings.Contains(strings.ToLower(name), q)
	sub := q
	if matched {
		sub = "" // everything below a match is kept
	}
	l := map[string]any{"id": n["id"], "name": name}
	for _, k := range []string{"type", "workspaceId", "baseUrl", "shared", "ownerName", "myRole"} {
		if v, ok := n[k]; ok && v != nil && v != "" && v != false {
			l[k] = v
		}
	}
	if d, _ := n["description"].(string); d != "" {
		if len([]rune(d)) > 160 {
			d = string([]rune(d)[:157]) + "…"
		}
		l["description"] = d
	}
	kids := children(n)
	if maxDepth > 0 && depth >= maxDepth {
		if len(kids) > 0 {
			l["folders"] = len(kids)
		}
	} else {
		var lk []map[string]any
		for _, c := range kids {
			if x := leanNode(c, sub, mode, maxDepth, depth+1); x != nil {
				lk = append(lk, x)
			}
		}
		if len(lk) > 0 {
			l["children"] = lk
		}
		if !matched && len(lk) == 0 {
			return nil
		}
	}
	if !matched && maxDepth > 0 && depth >= maxDepth {
		return nil
	}
	reqs, _ := n["requests"].([]any)
	switch {
	case len(reqs) == 0 || mode == "none" || !matched && q != "":
		// A path node above a match: its own requests aren't what was asked.
	case mode == "count":
		l["requests"] = len(reqs)
	default:
		sum := make([]map[string]any, 0, len(reqs))
		for _, r := range reqs {
			m, ok := r.(map[string]any)
			if !ok {
				continue
			}
			s := map[string]any{"id": m["id"], "name": m["name"]}
			if v, _ := m["method"].(string); v != "" {
				s["method"] = v
			}
			if v, _ := m["url"].(string); v != "" {
				s["url"] = v
			}
			sum = append(sum, s)
		}
		l["requests"] = sum
	}
	return l
}
