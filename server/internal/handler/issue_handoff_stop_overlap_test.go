package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// A handoff that stops the previous assignee's runs commits first and cancels
// afterwards. Another request can land between the two. These tests hold the
// acting agent's own task row so the handoff blocks right after its commit,
// change the issue from outside while it waits, and then let it go on.
//
// The cancellation was decided for the runs that existed, and for an issue
// that no longer belonged to the previous assignee, when the person confirmed
// the stop. It must not reach work that came later or work whose owner got the
// issue back.
func TestHandoffStopLeavesWorkThatCameAfterTheCommit(t *testing.T) {
	seedTestCatalog(t)
	handler := seededReadyAgentID(t)
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, handler).Scan(&runtimeID)

	for _, writer := range issueWriters {
		setup := func(t *testing.T) (issueID, previous, previousRun, callerRun string) {
			previous = dbfx.Agent(t, "Stop overlap previous "+workflowTestSuffix(), runtimeID)
			caller := dbfx.Agent(t, "Stop overlap caller "+workflowTestSuffix(), runtimeID)
			projectID := deliveryProject(t, "Stop overlap project "+workflowTestSuffix(), handler)
			issueID = dbfx.Issue(t, "stop overlap", testutil.Cols{
				"project_id": projectID, "status": "todo", "assignee_type": "agent", "assignee_id": previous,
			})
			previousRun = dbfx.Task(t, previous, testutil.Cols{"issue_id": issueID, "runtime_id": runtimeID, "status": "queued"})
			// The agent that makes the handoff is working on the issue from its
			// current step; its own task row is the one the handoff updates after
			// the commit.
			callerRun = dbfx.Task(t, caller, testutil.Cols{
				"issue_id": issueID, "runtime_id": runtimeID, "status": "running", "workflow_step": "todo",
			})
			dbfx.Exec(t, `UPDATE agent_task_queue SET started_at = now() WHERE id = $1`, callerRun)
			return issueID, previous, previousRun, callerRun
		}
		handoff := func(t *testing.T, issueID, callerRun string, overlap func(tx pgx.Tx)) {
			var callerAgent string
			dbfx.QueryRow(t, `SELECT agent_id::text FROM agent_task_queue WHERE id = $1`, callerRun).Scan(&callerAgent)
			headers := map[string]string{"X-Actor-Source": "task_token", "X-Agent-ID": callerAgent, "X-Task-ID": callerRun}
			raceUntilBlocked(t, func(tx pgx.Tx) {
				if _, err := tx.Exec(context.Background(), `SELECT 1 FROM agent_task_queue WHERE id = $1 FOR UPDATE`, util.MustParseUUID(callerRun)); err != nil {
					t.Fatal(err)
				}
			}, func() {
				writer.write(t, issueID, map[string]any{"status": "in_progress", "stop_previous_assignee_runs": true}, headers)
			}, func(tx pgx.Tx) {
				// Blocked after the commit, not before it.
				if status, _, _ := issueStatusAndAssignee(t, issueID); status != "in_progress" {
					t.Fatalf("the handoff was blocked before it committed (status %q): the overlap would not follow the commit", status)
				}
				overlap(tx)
			})
		}

		t.Run("a run the previous assignee gets after the commit stays/"+writer.name, func(t *testing.T) {
			issueID, previous, previousRun, callerRun := setup(t)
			var newerRun string
			handoff(t, issueID, callerRun, func(pgx.Tx) {
				// A mention wakes the previous assignee, who is no longer the owner.
				newerRun = dbfx.Task(t, previous, testutil.Cols{"issue_id": issueID, "runtime_id": runtimeID, "status": "running"})
			})

			if got := taskStatus(t, previousRun); got != "cancelled" {
				t.Fatalf("the run the person chose to stop = %q, want cancelled", got)
			}
			if got := taskStatus(t, newerRun); got != "running" {
				t.Fatalf("the run that started after the commit = %q, want running: the person never saw it", got)
			}
		})

		t.Run("a run stays when its agent gets the issue back/"+writer.name, func(t *testing.T) {
			issueID, previous, previousRun, callerRun := setup(t)
			handoff(t, issueID, callerRun, func(pgx.Tx) {
				// Another request hands the issue back to the previous assignee.
				dbfx.Exec(t, `UPDATE issue SET assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, issueID, previous)
			})

			if got := taskStatus(t, previousRun); got != "queued" {
				t.Fatalf("the run of the agent that owns the issue again = %q, want queued", got)
			}
		})
	}
}

// A handoff the person confirmed after a preview that found none is bound to
// that result. A handler that became resolvable since is refused, instead of
// handing the issue to someone the person never saw.
func TestNegativeHandoffPreviewIsBoundAtTheWrite(t *testing.T) {
	seedTestCatalog(t)
	handler := seededReadyAgentID(t)
	negative := map[string]any{
		"previous_assignee_type": nil, "previous_assignee_id": nil,
		"handler_type": nil, "handler_id": nil,
	}

	t.Run("a handler that became resolvable refuses the write", func(t *testing.T) {
		issueID := dbfx.Issue(t, "negative binding", testutil.Cols{
			"project_id": deliveryProject(t, "Negative binding project "+workflowTestSuffix(), handler), "status": "todo",
		})
		resp := updateIssueForTest(t, issueID, map[string]any{"status": "in_progress", "handoff_preview": negative})
		resp.Want(http.StatusConflict)
		wantIssueStatus(t, issueID, "todo")
		if _, _, assignee := issueStatusAndAssignee(t, issueID); assignee != "" {
			t.Fatalf("assignee = %q, want none: nobody confirmed the handoff", assignee)
		}
	})

	t.Run("a step that still hands off to no one applies", func(t *testing.T) {
		projectID := createWorkflowTestProject(t, "Negative binding plain project "+workflowTestSuffix())
		issueID := dbfx.Issue(t, "negative binding plain", testutil.Cols{"project_id": projectID, "status": "todo"})
		updateIssueForTest(t, issueID, map[string]any{"status": "in_review", "handoff_preview": negative}).Want(http.StatusOK)
		wantIssueStatus(t, issueID, "in_review")
	})
}
