package handler

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// Retrospective coverage for the lock order of the stop. The cancellation reads
// the issue row FOR SHARE in its own statement, like the stale-step cancel, so a
// hand-back that is still uncommitted is waited for instead of missed.
func TestStopPreviousRunWaitsForAnUncommittedHandBack(t *testing.T) {
	seedTestCatalog(t)
	handler := seededReadyAgentID(t)
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, handler).Scan(&runtimeID)
	previous := dbfx.Agent(t, "Stop wait previous "+workflowTestSuffix(), runtimeID)
	issueID := dbfx.Issue(t, "stop wait", testutil.Cols{
		"project_id": deliveryProject(t, "Stop wait project "+workflowTestSuffix(), handler), "status": "in_progress",
		"assignee_type": "agent", "assignee_id": handler,
	})
	run := dbfx.Task(t, previous, testutil.Cols{"issue_id": issueID, "runtime_id": runtimeID, "status": "queued"})

	raceUntilBlocked(t, func(tx pgx.Tx) {
		if _, err := tx.Exec(context.Background(), `UPDATE issue SET assignee_id = $2 WHERE id = $1`, util.MustParseUUID(issueID), util.MustParseUUID(previous)); err != nil {
			t.Fatal(err)
		}
	}, func() {
		_, err := testHandler.TaskService.CancelTaskWithResult(context.Background(), parseUUID(run), service.CancelTaskOptions{
			UserInitiated:  true,
			CancelledBy:    service.TaskCancellationActor{Type: "member", ID: parseUUID(testUserID)},
			UnlessAssignee: service.TaskAssignee{Type: "agent", ID: parseUUID(previous)},
		})
		if err != nil {
			t.Error(err)
		}
	})

	if got := taskStatus(t, run); got != "queued" {
		t.Fatalf("the run = %q, want queued: the hand-back committed before the cancellation read the issue", got)
	}
}
