package handler

import (
	"strings"
	"testing"
)

// The card of a workflow.step.instructions.set request carries the stored
// preview's text diff and its digest, the digest approval compares (CHE-2949 D5c).
func TestCardDiffShowsTheExactBeforeAndAfterOfTheOneStep(test *testing.T) {
	request := newStepInstructionsRequest(test)
	card := request.action.cardAction(test)
	if card == nil || card.Diff == nil {
		test.Fatalf("card action has no diff: %+v", card)
	}
	if card.Diff.Kind != "text" || card.Diff.SHA256 != request.preconditions(test)["preview_sha256"] {
		test.Fatalf("diff kind %q sha %q; want text and the stored preview digest", card.Diff.Kind, card.Diff.SHA256)
	}
	var lines []string
	for _, hunk := range card.Diff.Hunks {
		lines = append(lines, hunk.Lines...)
	}
	if got := strings.Join(lines, "|"); got != "-do A|+do A better" {
		test.Fatalf("diff lines %q; want the step's exact before and after text", got)
	}
}

func TestCardDiffIsGoneOnceTheActionIsDecided(test *testing.T) {
	request := newStepInstructionsRequest(test)
	dbfx.Exec(test, `UPDATE decision_action SET state = 'rejected' WHERE id = $1`, request.action.actionID)
	if card := request.action.cardAction(test); card == nil || card.Diff != nil {
		test.Fatalf("a decided action still carries a diff: %+v", card)
	}
}

// Two workflows can hold identical step text, so the diff alone cannot say
// which one an approval changes. The card names the target from the live
// workflow row, never from the agent's ask text (CHE-2962 security finding).
func TestCardDiffNamesTheServerResolvedWorkflowAndStatusKey(test *testing.T) {
	first := newStepInstructionsRequest(test)
	second := newStepInstructionsRequest(test)
	dbfx.Exec(test, `UPDATE issue_workflow SET name = 'Sandbox flow' WHERE id = $1`, first.workflowID)
	dbfx.Exec(test, `UPDATE issue_workflow SET name = 'Production flow' WHERE id = $1`, second.workflowID)
	firstCard, secondCard := first.action.cardAction(test), second.action.cardAction(test)
	if firstCard == nil || firstCard.Diff == nil || firstCard.Diff.Target == nil || secondCard == nil || secondCard.Diff == nil || secondCard.Diff.Target == nil {
		test.Fatalf("cards lack a diff target: %+v / %+v", firstCard, secondCard)
	}
	if got := *firstCard.Diff.Target; got.WorkflowID != first.workflowID || got.WorkflowName != "Sandbox flow" || got.StatusKey != "todo" {
		test.Fatalf("first target %+v; want the live workflow %s, name and status key", got, first.workflowID)
	}
	if got := *secondCard.Diff.Target; got.WorkflowID != second.workflowID || got.WorkflowName != "Production flow" || got.StatusKey != "todo" {
		test.Fatalf("second target %+v; want the live workflow %s, name and status key", got, second.workflowID)
	}
	if firstCard.Diff.Hunks[0].Lines[0] != secondCard.Diff.Hunks[0].Lines[0] {
		test.Fatal("fixture drift: the two diffs must be identical for this regression")
	}
}

func TestCardDiffIsWithheldWhenItsTargetCannotBeResolved(test *testing.T) {
	request := newStepInstructionsRequest(test)
	dbfx.Exec(test, `UPDATE issue_workflow SET steps = '[]'::jsonb WHERE id = $1`, request.workflowID)
	if card := request.action.cardAction(test); card == nil || card.Diff != nil {
		test.Fatalf("an unresolvable target still carries a diff: %+v", card)
	}
}
