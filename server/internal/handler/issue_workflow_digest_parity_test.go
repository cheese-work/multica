package handler

import (
	"encoding/json"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

// The CLI (multica config propose) hashes the steps array exactly as the
// workflow GET returns it. That digest must equal the server's, or every
// proposal would be refused as precondition_drift.
func TestWorkflowStepsHashEqualsTheDigestOfTheGETShape(t *testing.T) {
	id := parseUUID("11111111-1111-4111-8111-111111111111")
	steps := []issueworkflow.Step{
		{StatusKey: "todo", Handler: issueworkflow.Handler{Type: "agent", ID: "a"}, Instructions: "x <y> & z", NextStatusKey: "done"},
		{StatusKey: "done", Handler: issueworkflow.Handler{Type: "member", ID: "m"}, AdvanceOnCIGreen: true},
	}
	response, err := json.Marshal(IssueWorkflowResponse{ID: uuidToString(id), Steps: stepsPayload(steps)})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Steps json.RawMessage `json:"steps"`
	}
	if err := json.Unmarshal(response, &got); err != nil {
		t.Fatal(err)
	}
	want, err := decisionaction.WorkflowDigest(uuidToString(id), got.Steps)
	if err != nil {
		t.Fatal(err)
	}
	if server := workflowStepsHash(id, steps); server != want {
		t.Fatalf("server digest %s != digest of the GET shape %s", server, want)
	}
}
