package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// A Deployment as the API server returns it: managedFields and the
// last-applied annotation are a large share of the object.
func k8sDeploymentFixture() string {
	spec := `{"replicas":3,"selector":{"matchLabels":{"app":"api"}},"template":{"metadata":{"labels":{"app":"api"}},"spec":{"containers":[{"name":"api","image":"registry.example.com/api:1.2.3","ports":[{"containerPort":8080,"protocol":"TCP"}],"resources":{"limits":{"cpu":"500m","memory":"512Mi"},"requests":{"cpu":"100m","memory":"128Mi"}},"env":[{"name":"MODE","value":"prod"},{"name":"EMPTY","value":""}]}],"volumes":[]}}}`
	mf := []string{}
	for _, m := range []string{"kubectl-client-side-apply", "kube-controller-manager", "helm"} {
		mf = append(mf, fmt.Sprintf(`{"manager":%q,"operation":"Update","apiVersion":"apps/v1","time":"2026-11-18T10:00:00Z","fieldsType":"FieldsV1","fieldsV1":{"f:metadata":{"f:annotations":{".":{},"f:deployment.kubernetes.io/revision":{}},"f:labels":{".":{},"f:app":{}}},"f:spec":{"f:progressDeadlineSeconds":{},"f:replicas":{},"f:revisionHistoryLimit":{},"f:selector":{},"f:strategy":{"f:rollingUpdate":{".":{},"f:maxSurge":{},"f:maxUnavailable":{}},"f:type":{}},"f:template":{"f:metadata":{"f:labels":{".":{},"f:app":{}}},"f:spec":{"f:containers":{"k:{\"name\":\"api\"}":{".":{},"f:image":{},"f:imagePullPolicy":{},"f:name":{},"f:ports":{},"f:resources":{}}}}}},"f:status":{"f:availableReplicas":{},"f:conditions":{},"f:observedGeneration":{},"f:readyReplicas":{},"f:replicas":{},"f:updatedReplicas":{}}}}`, m))
	}
	lastAppliedJSON, _ := json.Marshal(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"api","namespace":"prod"},"spec":` + spec + `}`)
	return `{"data":{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"api","namespace":"prod","uid":"5f0c","resourceVersion":"123","generation":7,"labels":{"app":"api"},"annotations":{"deployment.kubernetes.io/revision":"7","` + lastApplied + `":` + string(lastAppliedJSON) + `},"managedFields":[` + strings.Join(mf, ",") + `],"finalizers":[],"ownerReferences":null},"spec":` + spec + `,"status":{"replicas":3,"readyReplicas":3,"conditions":[{"type":"Available","status":"True","reason":"","message":"Deployment has minimum availability."}]}}}`
}

func TestCompactReply(t *testing.T) {
	in := k8sDeploymentFixture()
	out := compactReply(in)
	t.Logf("k8s deployment: %d → %d bytes (-%d%%)", len(in), len(out), 100-100*len(out)/len(in))
	for _, gone := range []string{"managedFields", lastApplied, `"finalizers"`, `"EMPTY","value":""`, `"reason":""`} {
		if strings.Contains(out, gone) {
			t.Errorf("still contains %s", gone)
		}
	}
	for _, kept := range []string{`"replicas":3`, `"deployment.kubernetes.io/revision":"7"`, `"name":"EMPTY"`, "registry.example.com/api:1.2.3"} {
		if !strings.Contains(out, kept) {
			t.Errorf("lost %s", kept)
		}
	}

	// Exact numbers survive (no float64 rounding of big ids).
	if got := compactReply(`{"data":{"id":9007199254740993,"ok":false,"n":0,"x":null}}`); got != `{"data":{"id":9007199254740993,"n":0,"ok":false}}` {
		t.Errorf("numbers/false/0 must be kept: %s", got)
	}
	// Non-JSON passes through.
	if got := compactReply("plain log line\n"); got != "plain log line\n" {
		t.Errorf("text changed: %q", got)
	}

	// An oversized list is cut to fit, with a note.
	var items []string
	for i := 0; i < 2000; i++ {
		items = append(items, fmt.Sprintf(`{"id":%d,"name":"project-%d","fullPath":"group/project-%d","description":"%s"}`, i, i, i, strings.Repeat("x", 40)))
	}
	big := `{"data":[` + strings.Join(items, ",") + `]}`
	cut := compactReply(big)
	t.Logf("2000-item list: %d → %d bytes", len(big), len(cut))
	if len(cut) > maxReplyBytes || !strings.Contains(cut, "showing ") || !strings.Contains(cut, "of 2000 items") {
		t.Errorf("oversized list not capped: %d bytes", len(cut))
	}
	var m map[string]any
	if json.Unmarshal([]byte(cut), &m) != nil {
		t.Fatal("capped reply is not valid JSON")
	}
}

func TestNextStep(t *testing.T) {
	cases := []struct {
		code int
		body string
		want string
	}{
		{423, `{"error":"PatchMan is locked","locked":true}`, "unlock"},
		{428, `{"error":"confirm it's you to continue","stepUp":true}`, "Touch ID"},
		{412, `{"error":"Link your GitLab account in Settings to browse repositories"}`, "Settings → Integrations"},
		{404, `{"error":"Connection not found"}`, "list_*_connections"},
		{502, `{"error":"dial tcp 10.0.0.5:5432: i/o timeout"}`, "patchman-local"},
		{500, "upstream exploded", ""},
	}
	for _, c := range cases {
		got := withNextStep(c.code, c.body)
		if c.want == "" {
			if strings.Contains(got, "nextStep") {
				t.Errorf("%d: unexpected nextStep: %s", c.code, got)
			}
			continue
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("%d: want %q in %s", c.code, c.want, got)
		}
	}
	// An existing nextStep (e.g. the git desktop hint) is left alone.
	if got := withNextStep(404, `{"error":"x","nextStep":"keep"}`); got != `{"error":"x","nextStep":"keep"}` {
		t.Errorf("overwrote nextStep: %s", got)
	}
}
