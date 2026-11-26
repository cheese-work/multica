package decisionaction

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func projectSetPayload(workflow string, projects ...string) json.RawMessage {
	entries := make([]string, len(projects))
	for i, project := range projects {
		entries[i] = fmt.Sprintf(`{"project_id":%q,"workflow_id":%q,"status_mapping":{},"required":[],"total_issues":0,"overrides":[],"before":{"source":"project"},"after":{"source":"project"}}`, project, workflow)
	}
	return json.RawMessage(fmt.Sprintf(`{"workflow_id":%q,"projects":[%s]}`, workflow, strings.Join(entries, ",")))
}

func TestProjectWorkflowSetNormalize(t *testing.T) {
	const workflow = "2deb5a45-4154-41f1-8bcb-d5b6db3561d3"
	const a, b = "11111111-1111-4111-8111-111111111111", "00000000-0000-4000-8000-000000000001"

	out, _, err := normalizeProjectWorkflowSet(projectSetPayload(workflow, a, b), nil)
	if err != nil {
		t.Fatalf("valid payload refused: %v", err)
	}
	var params ProjectWorkflowSetParams
	if err := json.Unmarshal(out, &params); err != nil {
		t.Fatal(err)
	}
	if params.Projects[0].ProjectID != b {
		t.Fatalf("projects are not sorted by id: %s first", params.Projects[0].ProjectID)
	}

	many := make([]string, MaxProjectWorkflowSetProjects+1)
	for i := range many {
		many[i] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
	}
	cases := map[string]json.RawMessage{
		"no projects":       projectSetPayload(workflow),
		"too many projects": projectSetPayload(workflow, many...),
		"a project twice":   projectSetPayload(workflow, a, a),
		"another workflow":  json.RawMessage(strings.Replace(string(projectSetPayload(workflow, a)), `"workflow_id":"`+workflow+`","status_mapping"`, `"workflow_id":"`+b+`","status_mapping"`, 1)),
		"an unknown field":  json.RawMessage(strings.Replace(string(projectSetPayload(workflow, a)), `{"workflow_id"`, `{"extra":1,"workflow_id"`, 1)),
		"a malformed UUID":  projectSetPayload(workflow, "not-a-uuid"),
	}
	for name, raw := range cases {
		if _, _, err := normalizeProjectWorkflowSet(raw, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, _, err := normalizeProjectWorkflowSet(projectSetPayload(workflow, a), []SecretSlot{{}}); err == nil {
		t.Error("secret slots accepted")
	}
}
