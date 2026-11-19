package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/decisiontx"
)

// A budget write needs a spend-class decision, settled with a passkey proof
// when the workspace gate blocks, by an owner who is not in recovery (CHE-6197).

type budgetBoundScenario struct {
	class   string
	answer  string // gate at answer time: "off" or "block"
	signed  bool   // answer carries a passkey assertion (needs answer == "block")
	write   string // gate at write time
	recover bool   // owner is in recovery_required at write time
}

// newBudgetBoundCard answers a card for field as the workspace owner, the way
// the scenario says, and returns it with the workspace in the write-time gate.
func newBudgetBoundCard(test *testing.T, field string, scenario budgetBoundScenario) *gatedCard {
	test.Helper()
	card := newGatedCard(test, scenario.class)
	dbfx.Exec(test, `UPDATE workspace SET decision_passkey_gate = $2, policy_revision = policy_revision + 1 WHERE id = $1`, testWorkspaceID, scenario.answer)
	options := `[{"label":"` + decisiontx.ApproveWriteLabel + `","what_happens":"apply"},{"label":"Reject","what_happens":"drop"}]`
	dbfx.Exec(test, `UPDATE decision_request SET options = $2::jsonb WHERE id = $1`, card.decision.ID, options)
	storeBoundWriteCard(test, card.decision.ID, decisiontx.BoundWriteDecision(field, boundBefore, boundAfter))
	dbfx.Exec(test, `UPDATE member SET role = 'owner' WHERE workspace_id = $1 AND user_id = $2`, card.issue.WorkspaceID, parseUUID(card.humanID))
	input := decisionCardAnswerInput{Option: decisiontx.ApproveWriteLabel}
	sid := ""
	if scenario.signed {
		sid = gatedSession
		input = card.signed(card.challenge(decisiontx.ApproveWriteLabel, ""), decisiontx.ApproveWriteLabel, "")
	}
	if _, err := card.answer(sid, input); err != nil {
		test.Fatal(err)
	}
	// A non-reversible answer takes effect after a delay; this one is past it.
	dbfx.Exec(test, `UPDATE decision_answer SET effective_at = created_at WHERE decision_request_id = $1`, card.decision.ID)
	dbfx.Exec(test, `UPDATE workspace SET decision_passkey_gate = $2, policy_revision = policy_revision + 1 WHERE id = $1`, testWorkspaceID, scenario.write)
	if scenario.recover {
		dbfx.Exec(test, `UPDATE "user" SET passkey_state = 'recovery_required' WHERE id = $1`, parseUUID(card.humanID))
		dbfx.Cleanup(test, `UPDATE "user" SET passkey_state = 'ready' WHERE id = $1`, parseUUID(card.humanID))
	}
	return card
}

func TestBudgetBoundWriteNeedsSpendClassPasskeyDecision(test *testing.T) {
	resetDecisionLease(test)
	for _, scenario := range []struct {
		name    string
		budget  budgetBoundScenario
		refusal string // empty: the write is applied
	}{
		{"reversible answer under gate block", budgetBoundScenario{class: "reversible", answer: "block", write: "block"}, "spend-class"},
		{"reversible answer under gate off", budgetBoundScenario{class: "reversible", answer: "off", write: "off"}, "spend-class"},
		{"product_direction answer is not spend", budgetBoundScenario{class: "product_direction", answer: "off", write: "off"}, "spend-class"},
		{"spend answer without passkey proof under gate block", budgetBoundScenario{class: "spend", answer: "off", write: "block"}, "passkey"},
		{"owner in recovery", budgetBoundScenario{class: "spend", answer: "block", signed: true, write: "block", recover: true}, "recovery"},
		{"spend answer with passkey proof", budgetBoundScenario{class: "spend", answer: "block", signed: true, write: "block"}, ""},
		{"spend answer under gate off needs no proof", budgetBoundScenario{class: "spend", answer: "off", write: "off"}, ""},
	} {
		for _, field := range []string{workflowBudgetField, decisionaction.IssueWorkflowField} {
			test.Run(scenario.name+"/"+field, func(test *testing.T) {
				card := newBudgetBoundCard(test, field, scenario.budget)
				err := verifyDecisionBinding(context.Background(), testHandler.Queries, allowAnyAgent, card.issue.WorkspaceID, card.decision.ID, parseUUID(card.agentID), field, boundBefore, boundAfter)
				if scenario.refusal == "" {
					if err != nil {
						test.Fatalf("refused: %v", err)
					}
					if rows := boundWriteRows(test, card.askedRecordFixture); rows != 1 {
						test.Fatalf("bound write rows = %d, want 1", rows)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), scenario.refusal) {
					test.Fatalf("err = %v, want a refusal naming %q", err, scenario.refusal)
				}
				if rows := boundWriteRows(test, card.askedRecordFixture); rows != 0 {
					test.Fatalf("refused write left %d decision_bound_write rows", rows)
				}
			})
		}
	}
}
