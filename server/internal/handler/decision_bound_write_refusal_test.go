package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5843: a card that asks the owner to "Apply this change to <field>?" for a
// field only a passkey action can write cannot authorize anything. The server
// refuses it when it is created, so no card reaches the owner.
func TestRequestDecisionRefusesBoundWriteCardForPasskeyOnlyField(test *testing.T) {
	cases := map[string]string{
		"workflow.instructions": "multica config propose workflow.step.instructions <workflow-id> --status-key <key> --file <path> --out <out>",
		"workflow.steps":        "multica config propose workflow.steps <workflow-id> --file <path> --out <out>",
		"workspace.context":     "multica config propose workspace.context --file <path> --out <out>",
		"squad.instructions":    "multica config propose squad.instructions <squad-id> --file <path> --out <out>",
		"agent.instructions":    "multica config propose agent.instructions <agent-id> --file <path> --out <out>",
	}
	for field, command := range cases {
		test.Run(field, func(test *testing.T) {
			fixture := newSingleCallFixture(test)
			fixture.details.Decision = decisiontx.BoundWriteDecision(field, strings.Repeat("a", 64), strings.Repeat("b", 64))
			fixture.details.Options = []decisiontx.AskOption{testAskOption(decisiontx.ApproveWriteLabel), testAskOption("Reject")}
			fixture.details.RecommendedOption = decisiontx.ApproveWriteLabel
			fixture.details.Recommendation = decisiontx.ApproveWriteLabel

			var refusal map[string]any
			testutil.Call(test, testHandler.RequestDecision, fixture.call(fixture.body())).Want(http.StatusBadRequest).JSON(&refusal)
			message, _ := refusal["error"].(string)
			if !strings.Contains(message, command) || strings.Contains(message, "\n") {
				test.Fatalf("error = %q, want one line naming %q", message, command)
			}
			if cards := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, fixture.issueID); cards != 0 {
				test.Fatalf("refused ask left %d cards", cards)
			}
		})
	}
}

// The CHE-5157 ask exactly: the workflow.instructions heading with no hash lines.
func TestRequestDecisionRefusesTheChe5157Ask(test *testing.T) {
	fixture := newSingleCallFixture(test)
	fixture.details.Decision = "Apply this change to workflow.instructions?"
	fixture.details.Options = []decisiontx.AskOption{testAskOption("Approve write"), testAskOption("Reject")}
	fixture.details.RecommendedOption, fixture.details.Recommendation = "Approve write", "Approve write"
	testutil.Call(test, testHandler.RequestDecision, fixture.call(fixture.body())).Want(http.StatusBadRequest)
	if cards := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, fixture.issueID); cards != 0 {
		test.Fatalf("refused ask left %d cards", cards)
	}
}

// A bound-write ask for a field a card can still authorize is not refused.
func TestRequestDecisionKeepsBoundWriteCardForOtherFields(test *testing.T) {
	fixture := newSingleCallFixture(test)
	fixture.details.Decision = decisiontx.BoundWriteDecision("project.description", strings.Repeat("a", 64), strings.Repeat("b", 64))
	testutil.Call(test, testHandler.RequestDecision, fixture.call(fixture.body())).Want(http.StatusCreated)
}
