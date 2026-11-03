package handler

import (
	"net/http"
	"strings"
	"testing"
)

func setPasskeyState(test *testing.T, userID, state string) {
	test.Helper()
	dbfx.Exec(test, `UPDATE "user" SET passkey_state = $2 WHERE id = $1`, userID, state)
	dbfx.Cleanup(test, `UPDATE "user" SET passkey_state = 'ready' WHERE id = $1`, userID)
}

// Recovery revokes the keys but approval of any action class stays refused
// until the person holds two new ones.
func TestApproveDecisionActionIsRefusedWhileThePersonIsInRecovery(test *testing.T) {
	handler := withVerifier(acceptingVerifier)
	action := newPendingSecretAction(test)
	setPasskeyState(test, action.humanID, "recovery_required")

	body := action.approve(test, handler, action.claim(test, map[string]string{"KEY": secretSentinel}), http.StatusForbidden)
	if !strings.Contains(body, "recovery_required") {
		test.Fatalf("body %q lacks recovery_required", body)
	}
	action.assertUntouched(test)
	if n := dbfx.Count(test, `SELECT count(*) FROM agent WHERE id = $1 AND custom_env::text LIKE '%`+secretSentinel+`%'`, action.agentID); n != 0 {
		test.Fatal("the secret was written despite the refusal")
	}

	setPasskeyState(test, action.humanID, "ready")
	action.approve(test, handler, action.claim(test, map[string]string{"KEY": secretSentinel}), http.StatusOK)
}

// A gated answer is a non-reversible card answer under a blocking workspace.
func TestCardAnswerOfAGatedClassIsRefusedWhileThePersonIsInRecovery(test *testing.T) {
	fixture := newCardFixture(test)
	workspaceID := uuidToString(fixture.issue.WorkspaceID)
	dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'spend' WHERE id = $1`, fixture.decision.ID)
	dbfx.Exec(test, `UPDATE workspace SET decision_passkey_gate = 'block' WHERE id = $1`, workspaceID)
	dbfx.Cleanup(test, `UPDATE workspace SET decision_passkey_gate = 'off' WHERE id = $1`, workspaceID)
	setPasskeyState(test, fixture.humanID, "recovery_required")

	_, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option A"})
	if err == nil || !strings.Contains(err.Error(), "recovery_required") {
		test.Fatalf("err = %v, want recovery_required", err)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, fixture.decision.ID); n != 0 {
		test.Fatalf("the refusal left %d answers", n)
	}

	// Outside the gate nothing changes: a reversible class, or a workspace that is off.
	dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'reversible' WHERE id = $1`, fixture.decision.ID)
	if _, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option A"}); err != nil {
		test.Fatalf("reversible answer during recovery: %v", err)
	}
}
