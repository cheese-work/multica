package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// Two handoffs overlap. Request A captured step B (in_review). Request B moves
// the issue on to step C (in_progress) and queues the same agent's run for C.
// A's cancellation then runs. Judged against the step A captured, C's run is
// "stale"; judged against the issue as it stands, it is the current run and
// must stay.
func TestStaleStepCancelFromAnOlderRequestLeavesTheCurrentStepsRun(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	issueID, runtimeID := staleStepIssue(t, agentID) // status in_review (step B)
	setColumnHandoff(t, issueProjectID(t, issueID), "in_progress", handToAgent(agentID, "Implement it.")).Want(http.StatusOK)

	issueB, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	// Request B commits: the issue is on C and C's run exists.
	dbfx.Exec(t, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, issueID)
	current := dbfx.Task(t, agentID, testutil.Cols{
		"issue_id": issueID, "runtime_id": runtimeID, "status": "queued", "workflow_step": "in_progress",
	})

	// Request A resumes with the issue as it captured it.
	testHandler.IssueService.CancelStaleStepRuns(context.Background(), issueB, parseUUID(agentID))

	if got := taskStatus(t, current); got != "queued" {
		t.Fatalf("the current step's run = %q, want queued: an older request cancelled it", got)
	}
}

// The same overlap with the older cancellation paused. The cancellation takes
// the issue's row into account, so it waits for the newer step to commit, then
// finds the issue moved and cancels nothing.
func TestStaleStepCancelWaitsForTheNewerStepAndThenSkips(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	issueID, runtimeID := staleStepIssue(t, agentID)
	setColumnHandoff(t, issueProjectID(t, issueID), "in_progress", handToAgent(agentID, "Implement it.")).Want(http.StatusOK)
	staleB := dbfx.Task(t, agentID, testutil.Cols{
		"issue_id": issueID, "runtime_id": runtimeID, "status": "queued", "workflow_step": "implement_old",
	})
	issueB, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	var currentID string

	raceUntilBlocked(t, func(tx pgx.Tx) {
		// Request B, uncommitted: the issue moves to C.
		if _, err := tx.Exec(context.Background(), `UPDATE issue SET status = 'in_progress' WHERE id = $1`, util.MustParseUUID(issueID)); err != nil {
			t.Fatal(err)
		}
	}, func() {
		testHandler.IssueService.CancelStaleStepRuns(context.Background(), issueB, parseUUID(agentID))
	}, func(tx pgx.Tx) {
		// Still before B commits: B's old run is gone and C's run is queued. The
		// pending-run index admits one run per issue and agent, so the old one
		// goes first.
		if _, err := tx.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, util.MustParseUUID(staleB)); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(context.Background(), `INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, workflow_step)
			VALUES ($1, $2, $3, 'queued', 0, 'in_progress') RETURNING id::text`,
			util.MustParseUUID(agentID), util.MustParseUUID(runtimeID), util.MustParseUUID(issueID)).Scan(&currentID); err != nil {
			t.Fatal(err)
		}
	})

	if got := taskStatus(t, currentID); got != "queued" {
		t.Fatalf("the current step's run = %q, want queued", got)
	}
}

func issueProjectID(t *testing.T, issueID string) string {
	t.Helper()
	var projectID string
	dbfx.QueryRow(t, `SELECT project_id::text FROM issue WHERE id = $1`, issueID).Scan(&projectID)
	return projectID
}
