package mcp

import (
	"strings"
	"testing"
)

func TestCompactQARadarSaysWhatIsCut(t *testing.T) {
	body := `{"data":{"generatedAt":"t","links":[
{"gitProject":"grp/api","status":"ok","shown":300,"total":412,"truncated":true},
{"gitProject":"o/r","status":"noAccess","error":"You can't use this link's Git connection","shown":0,"total":null}],
"mrs":[{"ref":"mr:l1:1","project":"grp/api","provider":"gitlab","iid":1,"title":"MER-1 x","state":"opened","author":{"username":"ada"},
"reviewers":[{"username":"bo"}],"approvedBy":["bo"],"pipeline":"failed","jiraKey":"MER-1","jira":{"status":"Done"},
"signals":[{"kind":"stuck","reason":"pipeline"},{"kind":"aiAssisted","reason":"trailer"}]}],"merged":[]}}`
	out := compactQARadar(body, "", true)
	for _, want := range []string{"300 of 412 open MRs", "TRUNCATED", "noAccess", "grp/api!1", "bo✓", "MER-1 Done", "stuck(pipeline)", "possibly AI-assisted"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if f := compactQARadar(body, "noJira", true); !strings.Contains(f, "0 of 1 open MRs have signal noJira") {
		t.Errorf("filter: %s", f)
	}
	if hidden := compactQARadar(body, "", false); strings.Contains(hidden, "MER-1 Done") || !strings.Contains(hidden, "Jira details hidden") {
		t.Errorf("jira leaked without the jira group:\n%s", hidden)
	}
}
