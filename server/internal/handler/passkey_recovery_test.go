package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/passkey"
	"github.com/multica-ai/multica/server/internal/testutil"
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

// Matrix row 3, end to end: after the C00 recovery an action approval stays
// refused with no key and with one new key, and goes through with the second.
func TestApproveDecisionActionStaysRefusedAfterRecoveryUntilTwoNewKeys(test *testing.T) {
	mailer := &codeMailbox{}
	handler := enrollHandler(test, true, mailer)
	handler.ApprovalVerifier = acceptingVerifier
	action := newPendingSecretAction(test)
	for _, table := range []string{"passkey_enroll_ceremony", "passkey_enroll_limit", "passkey_event", "user_passkey"} {
		dbfx.Cleanup(test, `DELETE FROM `+table+` WHERE user_id = $1`, action.humanID)
	}
	dbfx.Cleanup(test, `UPDATE "user" SET passkey_state = 'ready' WHERE id = $1`, action.humanID)
	var email string
	dbfx.QueryRow(test, `SELECT email FROM "user" WHERE id = $1`, action.humanID).Scan(&email)

	if _, err := passkey.RecoverUser(context.Background(), handler.TxStarter, email); err != nil {
		test.Fatal(err)
	}
	refused := func(stage string) {
		test.Helper()
		body := action.approve(test, handler, action.claim(test, map[string]string{"KEY": secretSentinel}), http.StatusForbidden)
		if !strings.Contains(body, "recovery_required") {
			test.Fatalf("%s: body %q lacks recovery_required", stage, body)
		}
		action.assertUntouched(test)
	}
	refused("after recovery")
	for enrolled := 1; enrolled <= 2; enrolled++ {
		mailer.body = nil
		started := startEnrollment(test, handler, action.humanID)
		testutil.Call(test, handler.FinishPasskeyEnrollment, enrollRequest("/api/passkeys/enroll/finish", action.humanID, "interactive_jwt", finishBody(test, started, mailer.code(test)))).Want(http.StatusOK)
		if enrolled == 1 {
			refused("after the first new key")
		}
	}
	action.approve(test, handler, action.claim(test, map[string]string{"KEY": secretSentinel}), http.StatusOK)
}

// A card answer that began before recovery and finishes after re-enrolment
// sees state `ready` again, so only its own generation tells it apart.
func TestCardAnswerFromAnOlderSessionGenerationIsRefusedAfterRecoveryAndReenrolment(test *testing.T) {
	fixture := newCardFixture(test)
	dbfx.Exec(test, `UPDATE "user" SET session_generation = session_generation + 1 WHERE id = $1`, fixture.humanID)
	dbfx.Cleanup(test, `UPDATE "user" SET session_generation = 0 WHERE id = $1`, fixture.humanID)
	request := fixture.request()
	request.Header.Set(auth.SessionGenerationHeader, "0")

	_, err := testHandler.answerDecisionRequestTx(request, fixture.issue.WorkspaceID, fixture.decision.ID, parseUUID(fixture.humanID), decisionCardAnswerInput{Option: "Option A"})
	if err == nil || !strings.Contains(err.Error(), "session revoked") {
		test.Fatalf("err = %v, want session revoked", err)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, fixture.decision.ID); n != 0 {
		test.Fatalf("the stale session left %d answers", n)
	}

	request.Header.Set(auth.SessionGenerationHeader, "1")
	if _, err := testHandler.answerDecisionRequestTx(request, fixture.issue.WorkspaceID, fixture.decision.ID, parseUUID(fixture.humanID), decisionCardAnswerInput{Option: "Option A"}); err != nil {
		test.Fatalf("current generation: %v", err)
	}
}
