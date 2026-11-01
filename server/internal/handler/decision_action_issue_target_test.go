package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const issueNoopAction = "test.issue.noop"

// registerIssueNoopAction adds a spend-class action that targets an issue and
// changes nothing, so the target and authority rules run without an executor.
func registerIssueNoopAction(test *testing.T) {
	test.Helper()
	decisionaction.Register(&decisionaction.Definition{
		Type: issueNoopAction, Version: 1, MinClass: "spend", Target: decisionaction.TargetIssue,
		Normalize: func(raw json.RawMessage, slots []decisionaction.SecretSlot) (json.RawMessage, []decisionaction.SecretSlot, error) {
			var params struct {
				IssueID string `json:"issue_id"`
			}
			if err := json.Unmarshal(raw, &params); err != nil {
				return nil, nil, err
			}
			return raw, slots, nil
		},
	})
	decisionActionExecutors[issueNoopAction] = func(context.Context, decisionActionExecution) (json.RawMessage, error) {
		return json.RawMessage(`{"ok":true}`), nil
	}
	test.Cleanup(func() { delete(decisionActionExecutors, issueNoopAction) })
}

func (fixture singleCallFixture) issueNoop(issueID string) map[string]any {
	return map[string]any{"type": issueNoopAction, "version": 1, "params": map[string]any{"issue_id": issueID}}
}

func newPendingIssueAction(test *testing.T) pendingAction {
	test.Helper()
	registerIssueNoopAction(test)
	fixture := newActionFixture(test)
	var created actionResponse
	fixture.respond(test, fixture.withAction(fixture.issueNoop(fixture.issueID)), http.StatusCreated, &created)
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	return pendingAction{fixture, created.ID, created.Action.ID}
}

func TestDecisionActionRequestRefusesAnotherIssueTarget(test *testing.T) {
	registerIssueNoopAction(test)
	fixture := newActionFixture(test)
	otherIssueID := dbfx.Issue(test, "other issue")
	body := fixture.respond(test, fixture.withAction(fixture.issueNoop(otherIssueID)), http.StatusBadRequest, nil)
	if !strings.Contains(body, "target_issue_mismatch") {
		test.Fatalf("body %q; want target_issue_mismatch", body)
	}
	if requests, actions := fixture.requestCounts(test); requests != 0 || actions != 0 {
		test.Fatalf("refused request stored %d requests and %d actions; want none", requests, actions)
	}
}

func TestDecisionActionRequestStoresAnIssueTarget(test *testing.T) {
	action := newPendingIssueAction(test)
	var class, preconditions string
	dbfx.QueryRow(test, `SELECT request.escalation_class, action.preconditions::text FROM decision_action action JOIN decision_request request ON request.id = action.decision_request_id WHERE action.id = $1`, action.actionID).Scan(&class, &preconditions)
	if class != "spend" || !strings.Contains(preconditions, action.issueID) {
		test.Fatalf("class %q preconditions %q; want spend and the issue id", class, preconditions)
	}
}

func TestApproveDecisionActionRefusesAnotherIssueTarget(test *testing.T) {
	action := newPendingIssueAction(test)
	otherIssueID := dbfx.Issue(test, "other issue")
	dbfx.Exec(test, `UPDATE decision_action SET params = jsonb_build_object('issue_id', $1::text) WHERE id = $2`, otherIssueID, action.actionID)
	body := action.approve(test, withVerifier(acceptingVerifier), action.claim(test, map[string]string{}), http.StatusConflict)
	if !strings.Contains(body, "target_issue_mismatch") {
		test.Fatalf("body %q; want target_issue_mismatch", body)
	}
	action.assertUntouched(test)
}

// A server-opened request (CHE-2318) is addressed to any member. When it
// carries an action, the member shortcut must not apply.
func TestApproveDecisionActionOnServerRequestNeedsAnOwner(test *testing.T) {
	action := newPendingIssueAction(test)
	dbfx.Exec(test, `UPDATE decision_request SET request_fingerprint = 'server:test:' || id::text WHERE id = $1`, action.requestID)
	dbfx.Exec(test, `UPDATE member SET role = 'member' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, action.humanID)
	body := action.approve(test, withVerifier(acceptingVerifier), action.claim(test, map[string]string{}), http.StatusForbidden)
	if !strings.Contains(body, "class_needs_owner") {
		test.Fatalf("body %q; want class_needs_owner", body)
	}
	action.assertUntouched(test)

	dbfx.Exec(test, `UPDATE member SET role = 'owner' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, action.humanID)
	action.approve(test, withVerifier(acceptingVerifier), action.claim(test, map[string]string{}), http.StatusOK)
	if state := action.state(test); state != "executed" {
		test.Fatalf("action state = %s, want executed", state)
	}
}

// A role change can commit while the passkey is being verified, after the
// authority reads and before the member lock. The approver's role is read again
// under that lock, for either origin.
func TestApproveDecisionActionRefusesDemotionDuringVerification(test *testing.T) {
	for _, origin := range []string{"server", "agent"} {
		test.Run(origin, func(test *testing.T) {
			action := newPendingIssueAction(test)
			if origin == "server" {
				dbfx.Exec(test, `UPDATE decision_request SET request_fingerprint = 'server:test:' || id::text WHERE id = $1`, action.requestID)
			}
			demoteDuringVerification := func(context.Context, *db.Queries, ApprovalClaim) (pgtype.UUID, error) {
				dbfx.Exec(test, `UPDATE member SET role = 'member' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, action.humanID)
				return pgtype.UUID{}, nil
			}
			body := action.approve(test, withVerifier(demoteDuringVerification), action.claim(test, map[string]string{}), http.StatusForbidden)
			if !strings.Contains(body, "class_needs_owner") {
				test.Fatalf("body %q; want class_needs_owner", body)
			}
			action.assertUntouched(test)
		})
	}
}

// The owner rule covers every action on a server-opened request, not only an
// issue target: an agent's own owner is not a workspace owner.
func TestApproveServerAgentActionRefusesDemotionDuringVerification(test *testing.T) {
	action := newPendingAction(test)
	dbfx.Exec(test, `UPDATE decision_request SET request_fingerprint = 'server:test:' || id::text WHERE id = $1`, action.requestID)
	dbfx.Exec(test, `UPDATE member SET role = 'owner' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, action.humanID)
	demoteDuringVerification := func(context.Context, *db.Queries, ApprovalClaim) (pgtype.UUID, error) {
		dbfx.Exec(test, `UPDATE member SET role = 'member' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, action.humanID)
		return pgtype.UUID{}, nil
	}
	body := action.approve(test, withVerifier(demoteDuringVerification), action.claim(test, nil), http.StatusForbidden)
	if !strings.Contains(body, "class_needs_owner") {
		test.Fatalf("body %q; want class_needs_owner", body)
	}
	action.assertUntouched(test)
}
