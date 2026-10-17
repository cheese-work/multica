package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// A direct run that loses the pending-task unique index to another run counts
// as delivery only when that other run has not started: a queued run reads the
// issue when it is claimed, and a dispatched one read it earlier. The caller
// settles the handed agent's receipts only on delivery, so a dispatched run
// that holds the slot leaves the event wakeup as the change's delivery.
func TestDispatchIssueRunCountsOnlyAnUnstartedDuplicateAsDelivery(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID)
	cases := []struct {
		name     string
		existing string
		want     bool
	}{
		{"no run holds the slot", "", true},
		{"a queued run holds the slot", "queued", true},
		{"a dispatched run holds the slot", "dispatched", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issueID := dbfx.Issue(t, "duplicate dispatch", testutil.Cols{
				"status": "todo", "assignee_type": "agent", "assignee_id": agentID,
			})
			t.Cleanup(func() {
				testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
			})
			if tc.existing != "" {
				cols := testutil.Cols{"issue_id": issueID, "runtime_id": runtimeID, "status": tc.existing}
				if tc.existing == "dispatched" {
					cols["dispatched_at"] = testutil.Raw("now()")
				}
				dbfx.Task(t, agentID, cols)
			}
			issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
			if err != nil {
				t.Fatal(err)
			}

			got := testHandler.dispatchIssueRun(context.Background(), issue,
				service.IssueRunTrigger{IssueID: issue.ID, AgentID: parseUUID(agentID), AssigneeType: "agent"},
				"member", testUserID, "")

			if got != tc.want {
				t.Fatalf("dispatchIssueRun = %v, want %v", got, tc.want)
			}
		})
	}
}

// A media-gated run that the issue's earlier step left behind still holds the
// unique pending slot. The write that enters the next step must cancel it and
// queue the run for the new step, or the replacement deduplicates against the
// stale run and the change reaches the agent through neither.
func TestStaleMediaDeferredStepRunIsReplacedWithReceiptsKept(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID)
	for _, writer := range issueWriters {
		t.Run(writer.name, func(t *testing.T) {
			projectID := createWorkflowTestProject(t, "Stale deferred project")
			setColumnHandoff(t, projectID, "in_review", handToAgent(agentID, "Review it.")).Want(http.StatusOK)
			issueID := dbfx.Issue(t, "stale deferred", testutil.Cols{"project_id": projectID, "status": "in_progress"})
			stale := dbfx.Task(t, agentID, testutil.Cols{
				"issue_id": issueID, "runtime_id": runtimeID, "status": "deferred", "workflow_step": "in_progress",
				"context": testutil.Raw(`'{"channel_issue_media_pending":"true"}'::jsonb`),
			})
			wakeupID := subscribeToIssueEvent(t, issueID, agentID, "issue.status_changed")

			writer.write(t, issueID, map[string]any{"status": "in_review"}, nil).Want(http.StatusOK)

			var staleStatus string
			dbfx.QueryRow(t, `SELECT status FROM agent_task_queue WHERE id = $1`, stale).Scan(&staleStatus)
			if staleStatus != "cancelled" {
				t.Fatalf("the stale media-deferred run = %q, want cancelled", staleStatus)
			}
			if steps := pendingStepRuns(t, issueID, agentID); len(steps) != 1 || steps[0] != "in_review" {
				t.Fatalf("pending runs = %v, want one stamped in_review", steps)
			}
			// The executable run carries the change, so the receipt is handled.
			if got := pendingEventReceipts(t, wakeupID); got != 0 {
				t.Fatalf("pending receipts = %d, want 0 once the new-step run exists", got)
			}
		})
	}
}
