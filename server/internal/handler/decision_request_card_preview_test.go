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

func TestCardPreviewForAgentCopyDisclosesEverythingTheCopyRuns(test *testing.T) {
	action := newPendingCopy(test, "che2959 loud copy", map[string]any{
		"instructions":          "new prompt text",
		"custom_args":           []string{"--flag-one", "--flag-two"},
		"model":                 "model-x",
		"max_concurrent_tasks":  7,
		"description":           "a loud copy",
		"conversation_starters": []map[string]string{{"label": "go", "prompt": "do it"}},
	})
	fields := previewFields(test, action.cardAction(test))
	want := map[string]string{
		"Instructions":          "new prompt text",
		"Custom args":           "--flag-one --flag-two",
		"Model":                 "model-x",
		"Max concurrent tasks":  "7",
		"Description":           "a loud copy",
		"Conversation starters": "go: do it",
	}
	for label, proposed := range want {
		if got := fields[label]; len(got) != 2 || got[1] != proposed {
			test.Errorf("%s = %v, want proposed %q", label, got, proposed)
		}
	}
	for _, label := range []string{"Runtime", "Thinking level", "Service tier", "Avatar"} {
		if _, found := fields[label]; !found {
			test.Errorf("copy preview hides %q: %v", label, fields)
		}
	}
}

func TestCardPreviewForAgentCopyDisclosesInheritedAccessAndSkills(test *testing.T) {
	action := newPendingCopy(test, "che2959 inherit copy", nil)
	var skillID string
	dbfx.QueryRow(test, `INSERT INTO skill (workspace_id, name, description, content, config, created_by)
		VALUES ($1, 'che2959 inherited skill', '', '# x', '{}'::jsonb, $2) RETURNING id`, testWorkspaceID, testUserID).Scan(&skillID)
	dbfx.Cleanup(test, `DELETE FROM agent_skill WHERE skill_id = '`+skillID+`'`)
	dbfx.Cleanup(test, `DELETE FROM skill WHERE id = '`+skillID+`'`)
	dbfx.Exec(test, `INSERT INTO agent_skill (agent_id, skill_id) VALUES ($1, $2)`, action.agentID, skillID)
	dbfx.Exec(test, `UPDATE agent SET permission_mode = 'public_to' WHERE id = $1`, action.agentID)
	dbfx.Exec(test, `INSERT INTO agent_invocation_target (agent_id, target_type, target_id) VALUES ($1, 'workspace', $2) ON CONFLICT DO NOTHING`, action.agentID, testWorkspaceID)
	fields := previewFields(test, action.cardAction(test))
	want := map[string]string{
		"Who can invoke": "public_to: workspace:" + testWorkspaceID,
		"Skills":         "che2959 inherited skill",
	}
	for label, inherited := range want {
		if got := fields[label]; len(got) != 2 || got[0] != inherited || got[1] != inherited {
			test.Errorf("%s = %v, want %q kept from the source", label, got, inherited)
		}
	}
}

// The card is rendered for the request's human; a source they cannot view
// must not reach their chat, the executor refuses it at approval anyway.
func TestCardPreviewForAgentCopyIsMissingForASourceTheRecipientCannotSee(test *testing.T) {
	action := newPendingCopy(test, "che2959 hidden source copy", nil)
	dbfx.Exec(test, `UPDATE agent SET owner_id = $2, permission_mode = 'private' WHERE id = $1`, action.agentID, testUserID)
	dbfx.Exec(test, `UPDATE member SET role = 'member' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, action.humanID)
	card := action.cardAction(test)
	if card == nil || card.Preview != nil {
		raw, _ := json.Marshal(card)
		test.Fatalf("preview of a source the recipient cannot view: %s", raw)
	}
}

func TestCardPreviewForEnvPatchIsMissingForAnAgentTheRecipientCannotSee(test *testing.T) {
	action := newPendingAction(test)
	dbfx.Exec(test, `UPDATE agent SET owner_id = $2, permission_mode = 'private' WHERE id = $1`, action.agentID, testUserID)
	dbfx.Exec(test, `UPDATE member SET role = 'member' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, action.humanID)
	if card := action.cardAction(test); card == nil || card.Preview != nil {
		test.Fatalf("env preview of an agent the recipient cannot view: %+v", card)
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

// After approval the request is answered and the action leaves pending: the card
// keeps the action, with its state and no preview, so it can show the outcome.
func TestCardActionStaysAfterTheActionLeavesPending(test *testing.T) {
	action := newPendingAction(test)
	dbfx.Exec(test, `UPDATE decision_action SET state = 'executed' WHERE id = $1`, action.actionID)
	card := action.cardAction(test)
	if card == nil || card.State != "executed" || card.Preview != nil || card.ID != action.actionID {
		test.Fatalf("executed action on the card: %+v", card)
	}
}
