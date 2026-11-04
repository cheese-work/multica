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

const (
	workflowNoopAction    = "test.workflow.noop"
	workflowPreviewAction = "test.workflow.preview"
)

// registerWorkflowActions adds two spend-class actions that target a workflow
// and change nothing: one plain, one that requires a preview.
func registerWorkflowActions(test *testing.T) {
	test.Helper()
	normalize := func(raw json.RawMessage, slots []decisionaction.SecretSlot) (json.RawMessage, []decisionaction.SecretSlot, error) {
		return raw, slots, nil
	}
	decisionaction.Register(&decisionaction.Definition{Type: workflowNoopAction, Version: 1, MinClass: "spend", Target: decisionaction.TargetWorkflow, Normalize: normalize})
	decisionaction.Register(&decisionaction.Definition{Type: workflowPreviewAction, Version: 1, MinClass: "spend", Target: decisionaction.TargetWorkflow, RequiresPreview: true, Normalize: normalize})
	noop := noopAssertingTarget("workflow_id", func(execution decisionActionExecution) pgtype.UUID { return execution.Workflow.ID })
	decisionActionExecutors[workflowNoopAction], decisionActionExecutors[workflowPreviewAction] = noop, noop
	decisionActionPreviews[workflowPreviewAction] = func(context.Context, *db.Queries, db.IssueWorkflow, json.RawMessage) (any, error) {
		return map[string]string{"before": "old", "after": "new"}, nil
	}
	test.Cleanup(func() {
		delete(decisionActionExecutors, workflowNoopAction)
		delete(decisionActionExecutors, workflowPreviewAction)
		delete(decisionActionPreviews, workflowPreviewAction)
	})
}

func workflowInThisWorkspace(test *testing.T) string {
	test.Helper()
	var id string
	dbfx.QueryRow(test, `INSERT INTO issue_workflow (workspace_id, name, initial_status_key, steps) VALUES ($1, 'target wf', 'todo', '[]'::jsonb) RETURNING id`, testWorkspaceID).Scan(&id)
	dbfx.Cleanup(test, `DELETE FROM issue_workflow WHERE id = $1`, id)
	return id
}

func (fixture singleCallFixture) workflowAction(actionType, workflowID string) map[string]any {
	return map[string]any{"type": actionType, "version": 1, "params": map[string]any{"workflow_id": workflowID, "status_key": "todo", "base_sha256": "abc"}}
}

func newPendingWorkflowAction(test *testing.T, actionType string) (pendingAction, string) {
	test.Helper()
	registerWorkflowActions(test)
	workflowID := workflowInThisWorkspace(test)
	fixture := newActionFixture(test)
	var created actionResponse
	fixture.respond(test, fixture.withAction(fixture.workflowAction(actionType, workflowID)), http.StatusCreated, &created)
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	return pendingAction{fixture, created.ID, created.Action.ID}, workflowID
}

// makeApproverOwner sets the asked human's workspace role and restores the
// original role when the test ends, so later tests see the shared fixture.
func makeApproverOwner(test *testing.T, action pendingAction, role string) {
	test.Helper()
	var original string
	dbfx.QueryRow(test, `SELECT role FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, action.humanID).Scan(&original)
	test.Cleanup(func() {
		dbfx.Exec(test, `UPDATE member SET role = $3 WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, action.humanID, original)
	})
	dbfx.Exec(test, `UPDATE member SET role = $3 WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, action.humanID, role)
}

func TestDecisionActionRequestRefusesAForeignWorkspaceWorkflow(test *testing.T) {
	registerWorkflowActions(test)
	fixture := newActionFixture(test)
	body := fixture.respond(test, fixture.withAction(fixture.workflowAction(workflowNoopAction, foreignWorkspaceWorkflow(test))), http.StatusBadRequest, nil)
	if !strings.Contains(body, "target_workflow_mismatch") {
		test.Fatalf("body %q; want target_workflow_mismatch", body)
	}
	if requests, actions := fixture.requestCounts(test); requests != 0 || actions != 0 {
		test.Fatalf("refused request stored %d requests and %d actions; want none", requests, actions)
	}
}

func TestDecisionActionRequestStoresAWorkflowTarget(test *testing.T) {
	action, workflowID := newPendingWorkflowAction(test, workflowNoopAction)
	var preconditions struct {
		WorkflowID string `json:"workflow_id"`
		StatusKey  string `json:"status_key"`
		BaseSHA256 string `json:"base_sha256"`
	}
	var raw string
	dbfx.QueryRow(test, `SELECT preconditions::text FROM decision_action WHERE id = $1`, action.actionID).Scan(&raw)
	if err := json.Unmarshal([]byte(raw), &preconditions); err != nil {
		test.Fatal(err)
	}
	if preconditions.WorkflowID != workflowID || preconditions.StatusKey != "todo" || preconditions.BaseSHA256 != "abc" {
		test.Fatalf("preconditions %s; want workflow_id %s, status_key todo, base_sha256 abc", raw, workflowID)
	}
}

func TestApproveWorkflowTargetExecutesForAnOwner(test *testing.T) {
	action, _ := newPendingWorkflowAction(test, workflowNoopAction)
	makeApproverOwner(test, action, "owner")
	action.approve(test, withVerifier(acceptingVerifier), action.claim(test, map[string]string{}), http.StatusOK)
	if state := action.state(test); state != "executed" {
		test.Fatalf("action state = %s, want executed", state)
	}
}

func TestApproveWorkflowTargetRefusesAWorkflowOutsideTheWorkspace(test *testing.T) {
	test.Run("workflow moved away after the request", func(test *testing.T) {
		action, workflowID := newPendingWorkflowAction(test, workflowNoopAction)
		makeApproverOwner(test, action, "owner")
		foreign := foreignWorkspaceWorkflow(test)
		dbfx.Exec(test, `UPDATE issue_workflow SET workspace_id = (SELECT workspace_id FROM issue_workflow WHERE id = $1) WHERE id = $2`, foreign, workflowID)
		body := action.approve(test, withVerifier(acceptingVerifier), action.claim(test, map[string]string{}), http.StatusConflict)
		if !strings.Contains(body, "target_workflow_mismatch") {
			test.Fatalf("body %q; want target_workflow_mismatch", body)
		}
		action.assertUntouched(test)
	})
	test.Run("params point at a foreign workflow", func(test *testing.T) {
		action, _ := newPendingWorkflowAction(test, workflowNoopAction)
		makeApproverOwner(test, action, "owner")
		dbfx.Exec(test, `UPDATE decision_action SET params = jsonb_set(params, '{workflow_id}', to_jsonb($1::text)) WHERE id = $2`, foreignWorkspaceWorkflow(test), action.actionID)
		body := action.approve(test, withVerifier(acceptingVerifier), action.claim(test, map[string]string{}), http.StatusConflict)
		if !strings.Contains(body, "target_workflow_mismatch") {
			test.Fatalf("body %q; want target_workflow_mismatch", body)
		}
		action.assertUntouched(test)
	})
}

// serverOpened turns the request into a server-opened one, which the lifecycle
// sweep keeps open for a non-owner; the approval transaction must then refuse.
func serverOpened(test *testing.T, action pendingAction) {
	test.Helper()
	dbfx.Exec(test, `UPDATE decision_request SET request_fingerprint = 'server:test:' || id::text WHERE id = $1`, action.requestID)
}

func TestApproveWorkflowTargetNeedsAnOwner(test *testing.T) {
	refused := func(test *testing.T, action pendingAction, handler *Handler, status int, code string) {
		test.Helper()
		body := action.approve(test, handler, action.claim(test, map[string]string{}), status)
		if !strings.Contains(body, code) {
			test.Fatalf("body %q; want %s", body, code)
		}
		action.assertUntouched(test)
	}
	test.Run("non-owner approver", func(test *testing.T) {
		action, _ := newPendingWorkflowAction(test, workflowNoopAction)
		serverOpened(test, action)
		makeApproverOwner(test, action, "member")
		refused(test, action, withVerifier(acceptingVerifier), http.StatusForbidden, "class_needs_owner")
	})
	test.Run("owner demoted between propose and approve", func(test *testing.T) {
		action, _ := newPendingWorkflowAction(test, workflowNoopAction)
		serverOpened(test, action)
		makeApproverOwner(test, action, "owner")
		makeApproverOwner(test, action, "admin")
		refused(test, action, withVerifier(acceptingVerifier), http.StatusForbidden, "class_needs_owner")
	})
	test.Run("owner demoted during verification", func(test *testing.T) {
		action, _ := newPendingWorkflowAction(test, workflowNoopAction)
		makeApproverOwner(test, action, "owner")
		demote := func(context.Context, *db.Queries, ApprovalClaim) (pgtype.UUID, error) {
			makeApproverOwner(test, action, "admin")
			return pgtype.UUID{}, nil
		}
		refused(test, action, withVerifier(demote), http.StatusForbidden, "class_needs_owner")
	})
	test.Run("agent-opened, owner demoted between propose and approve", func(test *testing.T) {
		action, _ := newPendingWorkflowAction(test, workflowNoopAction)
		makeApproverOwner(test, action, "owner")
		makeApproverOwner(test, action, "admin")
		refused(test, action, withVerifier(acceptingVerifier), http.StatusForbidden, "class_needs_owner")
	})
	test.Run("agent-opened, non-owner approver", func(test *testing.T) {
		action, _ := newPendingWorkflowAction(test, workflowNoopAction)
		makeApproverOwner(test, action, "member")
		refused(test, action, withVerifier(acceptingVerifier), http.StatusForbidden, "class_needs_owner")
	})
	// The lifecycle worker can record the cancellation before the approval
	// arrives; the answer stays the owner refusal and the request stays cancelled.
	test.Run("agent-opened, demoted and already cancelled by the worker", func(test *testing.T) {
		resetDecisionLease(test)
		action, _ := newPendingWorkflowAction(test, workflowNoopAction)
		makeApproverOwner(test, action, "owner")
		makeApproverOwner(test, action, "admin")
		_, runner := newDecisionRunnerFixture(test)
		decisionPass(test, runner)
		if count := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'cancelled' AND cancel_reason = 'class_needs_owner'`, action.requestID); count != 1 {
			test.Fatal("the lifecycle worker did not cancel the request with class_needs_owner")
		}
		body := action.approve(test, withVerifier(acceptingVerifier), action.claim(test, map[string]string{}), http.StatusForbidden)
		if !strings.Contains(body, "class_needs_owner") {
			test.Fatalf("body %q; want class_needs_owner", body)
		}
		if state := action.state(test); state == "executed" {
			test.Fatal("the cancelled action was executed")
		}
	})
}

func TestApproveRequiresPreviewDigest(test *testing.T) {
	action, _ := newPendingWorkflowAction(test, workflowPreviewAction)
	makeApproverOwner(test, action, "owner")
	var stored string
	dbfx.QueryRow(test, `SELECT preconditions->>'preview_sha256' FROM decision_action WHERE id = $1`, action.actionID).Scan(&stored)
	if stored == "" {
		test.Fatal("preview digest missing from the stored preconditions")
	}
	for name, digest := range map[string]string{"absent": "", "wrong": strings.Repeat("0", 64)} {
		claim := action.claim(test, map[string]string{})
		if digest != "" {
			claim["preview_sha256"] = digest
		}
		body := action.approve(test, withVerifier(acceptingVerifier), claim, http.StatusConflict)
		if !strings.Contains(body, "preview_required") {
			test.Fatalf("%s digest: body %q; want preview_required", name, body)
		}
		action.assertUntouched(test)
	}
	claim := action.claim(test, map[string]string{})
	claim["preview_sha256"] = stored
	action.approve(test, withVerifier(acceptingVerifier), claim, http.StatusOK)
	if state := action.state(test); state != "executed" {
		test.Fatalf("action state = %s, want executed", state)
	}
}

func TestApproveIgnoresPreviewDigestWhenNotRequired(test *testing.T) {
	action, _ := newPendingWorkflowAction(test, workflowNoopAction)
	makeApproverOwner(test, action, "owner")
	action.approve(test, withVerifier(acceptingVerifier), action.claim(test, map[string]string{}), http.StatusOK)
}
