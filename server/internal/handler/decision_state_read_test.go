package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5553: GET /api/decisions/{id}/state is the read a CI job makes to learn
// what became of one action decision. A decisions:read PAT of the workspace
// owner is the only credential that reaches it.

var stateReader = feedCaller{source: "pat", scopes: []string{DecisionReadScope}, owner: true}

type decisionStateRead struct {
	DecisionID    string          `json:"decision_id"`
	ActionID      string          `json:"action_id"`
	ActionType    string          `json:"action_type"`
	ActionVersion int             `json:"action_version"`
	State         string          `json:"state"`
	ApproverID    *string         `json:"approver_id"`
	AnsweredAt    *string         `json:"answered_at"`
	WithdrawnAt   *string         `json:"withdrawn_at"`
	RevokedAt     *string         `json:"revoked_at"`
	ExpiresAt     *string         `json:"expires_at"`
	Params        json.RawMessage `json:"params"`
}

func readDecisionState(test *testing.T, caller feedCaller, requestID string, status int) (read decisionStateRead) {
	test.Helper()
	request := withURLParam(caller.request(test, http.MethodGet, "/api/decisions/"+requestID+"/state", ""), "id", requestID)
	response := testutil.Call(test, testHandler.GetDecisionState, request).Want(status)
	if status == http.StatusOK {
		response.JSON(&read)
	}
	return read
}

func wantState(test *testing.T, action pendingAction, want string) decisionStateRead {
	test.Helper()
	read := readDecisionState(test, stateReader, action.requestID, http.StatusOK)
	if read.State != want {
		test.Fatalf("state = %q, want %q (read %+v)", read.State, want, read)
	}
	if read.DecisionID != action.requestID || read.ActionID != action.actionID || read.ActionType != "doop.deploy_lift" || read.ActionVersion != 1 {
		test.Fatalf("identity = %+v", read)
	}
	var params map[string]string
	if err := json.Unmarshal(read.Params, &params); err != nil || params["marker_value"] != deployLiftParams["marker_value"] || params["repo"] != deployLiftParams["repo"] {
		test.Fatalf("params = %s (%v)", read.Params, err)
	}
	if read.ExpiresAt == nil {
		test.Fatal("expires_at is missing")
	}
	return read
}

func TestDecisionStateReadsPending(test *testing.T) {
	read := wantState(test, newPendingDeployLift(test, deployLiftParams), "pending")
	if read.ApproverID != nil || read.AnsweredAt != nil || read.WithdrawnAt != nil || read.RevokedAt != nil {
		test.Fatalf("a pending decision carries an approver, answer, withdrawal or revoke: %+v", read)
	}
}

func TestDecisionStateReadsApproved(test *testing.T) {
	action := newPendingDeployLift(test, deployLiftParams)
	action.approve(test, withVerifier(acceptingVerifier), action.claim(test, nil), http.StatusOK)
	read := wantState(test, action, "approved")
	if read.ApproverID == nil || *read.ApproverID != action.humanID || read.AnsweredAt == nil || read.RevokedAt != nil {
		test.Fatalf("approved read = %+v, want the owner as approver and an answer time", read)
	}
}

func TestDecisionStateReadsRejected(test *testing.T) {
	action := newPendingDeployLift(test, deployLiftParams)
	action.reject(test, testHandler, http.StatusOK)
	read := wantState(test, action, "rejected")
	if read.ApproverID != nil || read.AnsweredAt == nil {
		test.Fatalf("rejected read = %+v, want no approver and an answer time", read)
	}
}

func TestDecisionStateReadsWithdrawn(test *testing.T) {
	action := newPendingDeployLift(test, deployLiftParams)
	request := withURLParam(action.call(map[string]any{"reason": "no longer needed"}), "id", action.requestID)
	testutil.Call(test, testHandler.WithdrawDecision, request).Want(http.StatusOK)
	read := wantState(test, action, "withdrawn")
	if read.ApproverID != nil || read.WithdrawnAt == nil {
		test.Fatalf("withdrawn read = %+v, want no approver and a withdrawal time", read)
	}
}

func TestDecisionStateReadsExpired(test *testing.T) {
	test.Run("window closed before the lifecycle scan", func(test *testing.T) {
		action := newPendingDeployLift(test, deployLiftParams)
		dbfx.Exec(test, `UPDATE decision_action SET expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, action.actionID)
		if read := wantState(test, action, "expired"); read.ApproverID != nil {
			test.Fatalf("expired read = %+v", read)
		}
	})
	test.Run("after the lifecycle scan", func(test *testing.T) {
		action := newPendingDeployLift(test, deployLiftParams)
		dbfx.Exec(test, `UPDATE decision_action SET expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, action.actionID)
		action.runLifecycle(test)
		wantState(test, action, "expired")
	})
}

// An owner who approved a lift can take the approval back through the public
// revoke route, and the state read then says so.
func TestDecisionStateReadsRevoked(test *testing.T) {
	resetDecisionLease(test)
	action := newPendingDeployLift(test, deployLiftParams)
	action.approve(test, withVerifier(acceptingVerifier), action.claim(test, nil), http.StatusOK)
	action.revoke(test, withVerifier(acceptingVerifier), map[string]any{"reason": "marker was wrong"}, http.StatusOK)
	read := wantState(test, action, "revoked")
	if read.RevokedAt == nil || read.ApproverID == nil {
		test.Fatalf("revoked read = %+v, want the revoke time and the original approver", read)
	}
	action.revoke(test, withVerifier(acceptingVerifier), map[string]any{"reason": "again"}, http.StatusConflict)
}

// An approval whose window has closed no longer lifts anything: the read says
// expired, not approved, and still names the approver.
func TestDecisionStateReadsAnApprovalPastItsDeadlineAsExpired(test *testing.T) {
	action := newPendingDeployLift(test, deployLiftParams)
	action.approve(test, withVerifier(acceptingVerifier), action.claim(test, nil), http.StatusOK)
	wantState(test, action, "approved")
	dbfx.Exec(test, `UPDATE decision_action SET expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, action.actionID)
	read := wantState(test, action, "expired")
	if read.ApproverID == nil {
		test.Fatalf("expired read = %+v, want the original approver kept", read)
	}
}

func TestDecisionStateRefusesEveryOtherCredential(test *testing.T) {
	action := newPendingDeployLift(test, deployLiftParams)
	for name, caller := range map[string]feedCaller{
		"no scope":        {source: "pat", owner: true},
		"feed scope only": {source: "pat", scopes: []string{DecisionFeedScope}, owner: true},
		"ci scope only":   {source: "pat", scopes: []string{DecisionCIRequestScope}, owner: true},
		"not the owner":   {source: "pat", scopes: []string{DecisionReadScope}},
		"not a PAT":       {source: "interactive_jwt", scopes: []string{DecisionReadScope}, owner: true},
	} {
		test.Run(name, func(test *testing.T) {
			readDecisionState(test, caller, action.requestID, http.StatusForbidden)
		})
	}
}

func TestDecisionStateUnknownDecisionIsNotFound(test *testing.T) {
	readDecisionState(test, stateReader, "00000000-0000-4000-8000-000000000000", http.StatusNotFound)
}
