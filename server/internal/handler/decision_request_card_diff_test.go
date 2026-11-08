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
