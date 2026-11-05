package handler

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
)

func (action pendingAction) cardAction(test *testing.T) *DecisionRequestCardAction {
	test.Helper()
	return testHandler.decisionCardAction(context.Background(), parseUUID(testWorkspaceID), parseUUID(action.requestID))
}

func previewFields(test *testing.T, card *DecisionRequestCardAction) map[string][2]string {
	test.Helper()
	if card == nil || card.Preview == nil {
		test.Fatalf("card action has no preview: %+v", card)
	}
	fields := map[string][2]string{}
	for _, field := range card.Preview.Fields {
		fields[field.Label] = [2]string{field.Current, field.Proposed}
	}
	return fields
}

// A preview carries the digest of its own fields, so the client can show what
// the approver saw.
func assertPreviewDigest(test *testing.T, preview *ActionPreview) {
	test.Helper()
	raw, err := json.Marshal(preview.Fields)
	if err != nil {
		test.Fatal(err)
	}
	if want := previewDigest(raw); preview.SHA256 != want {
		test.Fatalf("preview sha256 = %s; want %s", preview.SHA256, want)
	}
}

func TestCardPreviewForRaiseEqualsWhatTheExecutorChecks(test *testing.T) {
	action := newPendingRaise(test, 30)
	card := action.cardAction(test)
	fields := previewFields(test, card)
	assertPreviewDigest(test, card.Preview)
	// The executor reads this cap and window under the approval lock.
	current, windowID, err := service.OpenRunBudgetWindowID(context.Background(), testHandler.Queries, parseUUID(action.issueID))
	if err != nil || windowID != action.windowID {
		test.Fatalf("executor window = %q (%v); want %q", windowID, err, action.windowID)
	}
	if got, want := fields["Run cap"], [2]string{strconv.Itoa(current), "30"}; got != want {
		test.Fatalf("Run cap = %v; want %v", got, want)
	}
	if _, found := fields["Runs left"]; !found {
		test.Fatalf("raise preview lacks runs left: %v", fields)
	}
	if card.ID != action.actionID || card.ActionType != raiseAction || card.PayloadSHA256 == "" {
		test.Fatalf("card action = %+v", card)
	}
	out := action.approveAccepted(test)
	if out.State != "executed" {
		test.Fatalf("outcome = %+v", out)
	}
	if cap, _ := action.maxRunsOverride(test); strconv.Itoa(cap) != fields["Run cap"][1] {
		test.Fatalf("executed cap = %d; preview promised %s", cap, fields["Run cap"][1])
	}
}

func TestCardPreviewForRaiseIsMissingOnceTheWindowMoved(test *testing.T) {
	action := newPendingRaise(test, 30)
	dbfx.Exec(test, `UPDATE issue SET max_runs_override = 40 WHERE id = $1`, action.issueID) // cap now above the raise
	if card := action.cardAction(test); card == nil || card.Preview != nil {
		test.Fatalf("a raise the executor would refuse still has a preview: %+v", card)
	}
}

func TestCardPreviewForTopUpShowsTheWindowReset(test *testing.T) {
	action, _ := newPendingTopUp(test, true)
	card := action.cardAction(test)
	fields := previewFields(test, card)
	assertPreviewDigest(test, card.Preview)
	if got := fields["Runs used"]; got[1] != "0" {
		test.Fatalf("Runs used = %v; a top-up resets the window to 0", got)
	}
}

func TestCardPreviewForEnvPatchListsKeysNeverValues(test *testing.T) {
	action := newPendingAction(test) // sets A=1
	card := action.cardAction(test)
	fields := previewFields(test, card)
	assertPreviewDigest(test, card.Preview)
	if fields["Agent"][0] == "" || fields["Set keys"][1] != "A" {
		test.Fatalf("env preview = %v", fields)
	}
	if raw, _ := json.Marshal(card); strings.Contains(string(raw), `"1"`) {
		test.Fatalf("env preview leaks a value: %s", raw)
	}
}

func TestCardPreviewForAgentCopyNamesSourceAndCopy(test *testing.T) {
	action := newPendingCopy(test, "che2959 copy", nil)
	fields := previewFields(test, action.cardAction(test))
	if fields["Agent"][1] != "che2959 copy" || fields["Agent"][0] == "" {
		test.Fatalf("copy preview = %v", fields)
	}
}

func TestCardActionIsAbsentForARequestWithoutAnAction(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	if card := testHandler.decisionCardAction(context.Background(), parseUUID(testWorkspaceID), fixture.decision.ID); card != nil {
		test.Fatalf("plain ask has an action: %+v", card)
	}
}

func TestCardPreviewIsMissingForAnUnknownActionType(test *testing.T) {
	action := newPendingAction(test)
	dbfx.Exec(test, `UPDATE decision_action SET action_type = 'future.thing' WHERE id = $1`, action.actionID)
	card := action.cardAction(test)
	if card == nil || card.Preview != nil || card.ActionType != "future.thing" {
		test.Fatalf("unknown type should be listed without a preview: %+v", card)
	}
}
