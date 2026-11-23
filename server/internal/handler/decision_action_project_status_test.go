package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-9125: project.status.set is the card an agent proposes instead of
// writing a project status. The status changes only when the owner approves it
// with a passkey, and the result reads the new status back.

type projectStatusFixture struct {
	pendingAction
	projectID string
}

func newPendingProjectStatus(test *testing.T, from, to string) projectStatusFixture {
	test.Helper()
	fixture := newActionFixture(test)
	projectID := inactiveProject(test, from)
	body := fixture.withAction(map[string]any{
		"type": decisionaction.ProjectStatusSet, "version": 1,
		"params": map[string]any{"project_id": projectID, "status": to},
	})
	var created actionResponse
	fixture.respond(test, body, http.StatusCreated, &created)
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	return projectStatusFixture{pendingAction{fixture, created.ID, created.Action.ID}, projectID}
}

func TestProjectStatusSetIsRegisteredWithAPasskeyAndAnExecutor(test *testing.T) {
	definition, ok := decisionaction.Lookup(decisionaction.ProjectStatusSet, 1)
	if !ok {
		test.Fatal("project.status.set is not on the allow table")
	}
	if definition.Passkey != decisionaction.PasskeyRequired {
		test.Fatalf("passkey = %v, want required", definition.Passkey)
	}
	if _, ok := decisionActionExecutors[decisionaction.ProjectStatusSet]; !ok {
		test.Fatal("project.status.set has no executor")
	}
}

func TestProjectStatusSetProposalLeavesTheStatusUntilApproval(test *testing.T) {
	pending := newPendingProjectStatus(test, "in_progress", "paused")
	if got := projectStatusOf(test, pending.projectID); got != "in_progress" {
		test.Fatalf("status = %s before approval, want in_progress", got)
	}
	if got := pending.state(test); got != "pending" {
		test.Fatalf("action state = %s, want pending", got)
	}
}

func TestProjectStatusSetAppliesOnlyAfterApprovalAndReadsItBack(test *testing.T) {
	for _, status := range projectStatusValues {
		test.Run(status, func(test *testing.T) {
			from := "in_progress"
			if status == from {
				from = "planned"
			}
			pending := newPendingProjectStatus(test, from, status)
			out := pending.approveAccepted(test)
			if out.State != "executed" {
				test.Fatalf("outcome = %+v, want executed", out)
			}
			if got := projectStatusOf(test, pending.projectID); got != status {
				test.Fatalf("status = %s after approval, want %s", got, status)
			}
			var result struct {
				ProjectID string `json:"project_id"`
				From      string `json:"from_status"`
				Status    string `json:"status"`
			}
			if err := json.Unmarshal(out.Result, &result); err != nil || result.ProjectID != pending.projectID || result.From != from || result.Status != status {
				test.Fatalf("result %s (%v), want %s %s -> %s", out.Result, err, pending.projectID, from, status)
			}
		})
	}
}

func TestProjectStatusSetRejectLeavesTheStatus(test *testing.T) {
	pending := newPendingProjectStatus(test, "in_progress", "cancelled")
	pending.reject(test, withVerifier(acceptingVerifier), http.StatusOK)
	if got := projectStatusOf(test, pending.projectID); got != "in_progress" {
		test.Fatalf("status = %s after reject, want in_progress", got)
	}
}

func TestProjectStatusSetPauseEndsQueuedRuns(test *testing.T) {
	pending := newPendingProjectStatus(test, "in_progress", "paused")
	dbfx.Exec(test, `UPDATE issue SET project_id = $1 WHERE id = $2`, pending.projectID, pending.issueID)
	dbfx.Cleanup(test, `UPDATE issue SET project_id = NULL WHERE id = $1`, pending.issueID)
	var runtimeID string
	dbfx.QueryRow(test, `SELECT runtime_id::text FROM agent WHERE id = $1`, pending.agentID).Scan(&runtimeID)
	taskID := dbfx.Task(test, pending.agentID, testutil.Cols{"issue_id": pending.issueID, "runtime_id": runtimeID, "status": "queued"})
	out := pending.approveAccepted(test)
	if out.State != "executed" {
		test.Fatalf("outcome = %+v, want executed", out)
	}
	var taskStatus string
	dbfx.QueryRow(test, `SELECT status FROM agent_task_queue WHERE id = $1`, taskID).Scan(&taskStatus)
	if taskStatus != "cancelled" {
		test.Fatalf("queued run status = %s after pause, want cancelled", taskStatus)
	}
}

func TestProjectStatusSetEndsAsAFailureWhenTheProjectIsGone(test *testing.T) {
	pending := newPendingProjectStatus(test, "in_progress", "paused")
	dbfx.Exec(test, `DELETE FROM project WHERE id = $1`, pending.projectID)
	out := pending.approveAccepted(test)
	if out.State != "failed" || out.ErrorCode != "project_missing" {
		test.Fatalf("outcome = %+v, want failed/project_missing", out)
	}
}

func TestProjectStatusSetCardShowsTheStatusBeforeAndAfter(test *testing.T) {
	pending := newPendingProjectStatus(test, "in_progress", "paused")
	card := pending.cardAction(test)
	fields := previewFields(test, card)
	assertPreviewDigest(test, card.Preview)
	if got := fields["Status"]; got[0] != "in_progress" || got[1] != "paused" {
		test.Fatalf("Status = %v, want in_progress -> paused", got)
	}
}

func TestProjectStatusSetProposalRefusesABadStatusOrExtraField(test *testing.T) {
	fixture := newActionFixture(test)
	projectID := inactiveProject(test, "in_progress")
	for name, params := range map[string]map[string]any{
		"unknown status": {"project_id": projectID, "status": "archived"},
		"missing status": {"project_id": projectID},
		"bad project id": {"project_id": "nope", "status": "paused"},
		"extra field":    {"project_id": projectID, "status": "paused", "title": "x"},
	} {
		test.Run(name, func(test *testing.T) {
			body := fixture.withAction(map[string]any{"type": decisionaction.ProjectStatusSet, "version": 1, "params": params})
			fixture.respond(test, body, http.StatusBadRequest, nil)
		})
	}
}
