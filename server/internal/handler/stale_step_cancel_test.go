package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func staleStepIssue(t *testing.T, agentID string) (issueID, runtimeID string) {
	t.Helper()
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID)
	projectID := createWorkflowTestProject(t, "Stale step cancel project")
	setColumnHandoff(t, projectID, "in_review", handToAgent(agentID, "Review it.")).Want(http.StatusOK)
	issueID = dbfx.Issue(t, "stale step cancel", testutil.Cols{"project_id": projectID, "status": "in_review"})
	return issueID, runtimeID
}

func cancelStaleStepRuns(t *testing.T, issueID, agentID string) {
	t.Helper()
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	testHandler.IssueService.CancelStaleStepRuns(context.Background(), issue, parseUUID(agentID))
}

// The stale runs are chosen with one statement and cancelled with another. A
// dispatched run the daemon starts in between is real work in flight, and a
// handoff must not cancel it: the cancel has to hold the stale-and-pending
// condition itself, not rely on the earlier selection.
func TestStaleStepCancelLeavesARunThatStartedAfterSelection(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	issueID, runtimeID := staleStepIssue(t, agentID)
	stale := dbfx.Task(t, agentID, testutil.Cols{
		"issue_id": issueID, "runtime_id": runtimeID, "status": "dispatched", "workflow_step": "in_progress",
		"dispatched_at": testutil.Raw("now()"),
	})

	raceUntilBlocked(t, func(tx pgx.Tx) {
		if _, err := tx.Exec(context.Background(),
			`UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, parseUUID(stale)); err != nil {
			t.Fatal(err)
		}
	}, func() { cancelStaleStepRuns(t, issueID, agentID) })

	if got := taskStatus(t, stale); got != "running" {
		t.Fatalf("the stale run that started after selection = %q, want running", got)
	}
}

// What the handoff still cancels: a stale run that has not started.
func TestStaleStepCancelStillCancelsPendingRuns(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	issueID, runtimeID := staleStepIssue(t, agentID)
	stale := dbfx.Task(t, agentID, testutil.Cols{
		"issue_id": issueID, "runtime_id": runtimeID, "status": "queued", "workflow_step": "in_progress",
	})

	cancelStaleStepRuns(t, issueID, agentID)

	if got := taskStatus(t, stale); got != "cancelled" {
		t.Fatalf("the stale queued run = %q, want cancelled", got)
	}
}

// Explicit cancellation of running work is a different path and stays allowed.
func TestUserCancellationStillCancelsARunningTask(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	issueID, runtimeID := staleStepIssue(t, agentID)
	running := dbfx.Task(t, agentID, testutil.Cols{
		"issue_id": issueID, "runtime_id": runtimeID, "status": "running", "workflow_step": "in_progress",
		"started_at": testutil.Raw("now()"),
	})

	if _, err := testHandler.TaskService.CancelTaskByUser(context.Background(), parseUUID(running), service.TaskCancellationActor{
		Type: "member", ID: parseUUID(testUserID), Name: "tester",
	}); err != nil {
		t.Fatal(err)
	}

	if got := taskStatus(t, running); got != "cancelled" {
		t.Fatalf("the running task after a user cancel = %q, want cancelled", got)
	}
}
