package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

const sampleCollections = `{"data":[
 {"id":"c1","name":"EXTERNAL","type":"collection","description":"outside services","children":[
   {"id":"f1","name":"logo-api","type":"folder","requests":[
     {"id":"r1","name":"Upload","method":"POST","url":"https://logo/upload","headers":"[{\"key\":\"Authorization\",\"value\":\"Bearer s3cret\"}]","body":"{\"password\":\"hunter2\"}"}],
    "children":[{"id":"f1a","name":"deep","type":"folder","requests":[{"id":"r9","name":"Deep","method":"GET","url":"https://deep"}]}]}
 ],"requests":[{"id":"r0","name":"Root req","method":"GET","url":"https://root"}]},
 {"id":"c2","name":"MERCHANT","type":"collection","children":[
   {"id":"f2","name":"merchant-jobs","type":"folder","requests":[{"id":"r2","name":"Run","method":"POST","url":"https://m/run"}]}
 ]}
]}`

func lean(t *testing.T, in listCollectionsIn) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(leanCollections(sampleCollections, in)), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLeanCollections(t *testing.T) {
	all := leanCollections(sampleCollections, listCollectionsIn{})
	if strings.Contains(all, "s3cret") || strings.Contains(all, "hunter2") || strings.Contains(all, "headers") {
		t.Fatalf("request headers / body leaked: %s", all)
	}
	if !strings.Contains(all, `"MERCHANT"`) || !strings.Contains(all, `"method":"POST"`) || !strings.Contains(all, `"url":"https://logo/upload"`) {
		t.Fatalf("tree / summaries missing: %s", all)
	}

	// name: the matching collection with its subtree, nothing else.
	m := lean(t, listCollectionsIn{Name: "merchant"})
	data := m["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["name"] != "MERCHANT" {
		t.Fatalf("name filter: %v", m)
	}
	// name matching a nested folder keeps the path above it, but not the
	// path nodes' own requests.
	m = lean(t, listCollectionsIn{Name: "deep"})
	s, _ := json.Marshal(m)
	if !strings.Contains(string(s), `"EXTERNAL"`) || !strings.Contains(string(s), `"Deep"`) || strings.Contains(string(s), "Root req") || strings.Contains(string(s), "Upload") {
		t.Fatalf("nested name filter: %s", s)
	}
	if m := lean(t, listCollectionsIn{Name: "nope"}); m["note"] == nil {
		t.Errorf("no-match note missing: %v", m)
	}

	// collectionId: one subtree.
	m = lean(t, listCollectionsIn{CollectionID: "f2"})
	data = m["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["name"] != "merchant-jobs" {
		t.Fatalf("collectionId: %v", m)
	}
	if m := lean(t, listCollectionsIn{CollectionID: "zzz"}); m["note"] == nil {
		t.Errorf("unknown id note missing: %v", m)
	}

	// depth 1: folders under the top level as a count; requests as counts.
	m = lean(t, listCollectionsIn{Depth: 1, Requests: "count"})
	s, _ = json.Marshal(m)
	if !strings.Contains(string(s), `"folders":1`) || strings.Contains(string(s), `"deep"`) || !strings.Contains(string(s), `"requests":1`) {
		t.Fatalf("depth/count: %s", s)
	}
	if s := leanCollections(sampleCollections, listCollectionsIn{Requests: "none"}); strings.Contains(s, `"requests"`) {
		t.Errorf("requests none: %s", s)
	}
	// Unreadable bodies pass through.
	if got := leanCollections("not json", listCollectionsIn{}); got != "not json" {
		t.Errorf("passthrough: %q", got)
	}
}
