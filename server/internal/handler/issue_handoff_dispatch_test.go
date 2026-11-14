package handler

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-5486 (S1b): a workflow handoff to an agent writes an intent row in the
// status transaction; dispatchHandoffIntent turns it into exactly one run, in
// whatever state that run later reaches.

type handoffIntentRow struct {
	ID, State, TaskID, TargetType string
}

func handoffIntents(t *testing.T, issueID string) []handoffIntentRow {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `
		SELECT id::text, state, COALESCE(task_id::text, ''), target_type
		FROM issue_handoff_dispatch WHERE issue_id = $1 ORDER BY created_at`, issueID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []handoffIntentRow
	for rows.Next() {
		var r handoffIntentRow
		if err := rows.Scan(&r.ID, &r.State, &r.TaskID, &r.TargetType); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func wantOneIntent(t *testing.T, issueID string) handoffIntentRow {
	t.Helper()
	got := handoffIntents(t, issueID)
	if len(got) != 1 {
		t.Fatalf("handoff intents on the issue = %d, want 1", len(got))
	}
	return got[0]
}

// handoffDispatchFixture is a todo issue in a project whose in_progress step
// hands off to a ready agent.
func handoffDispatchFixture(t *testing.T) (issueID, agentID string) {
	t.Helper()
	seedTestCatalog(t)
	agentID = seededReadyAgentID(t)
	projectID := deliveryProject(t, "Dispatch intent project", agentID)
	issueID = dbfx.Issue(t, "dispatch intent", testutil.Cols{"project_id": projectID, "status": "todo"})
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
		testPool.Exec(context.Background(), `DELETE FROM issue_handoff_dispatch WHERE issue_id = $1`, issueID)
	})
	return issueID, agentID
}

func moveIntoHandoffStep(t *testing.T, issueID string, extra map[string]any) {
	t.Helper()
	body := map[string]any{"status": "in_progress"}
	for k, v := range extra {
		body[k] = v
	}
	issueWriters[0].write(t, issueID, body, nil).Want(http.StatusOK)
}

// pendingAgain puts an intent back to the state a crash between the task's
// commit and the intent's mark would have left it in, if the two were not one
// transaction: pending, with no task id recorded.
func pendingAgain(t *testing.T, intentID string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE issue_handoff_dispatch SET state = 'pending', task_id = NULL WHERE id = $1`, intentID)
}

// Acceptance 1: the first run reached a terminal state; a second dispatch of the
// same intent must not make another.
func TestDispatchHandoffIntent_NoSecondRunAfterTheFirstEnded(t *testing.T) {
	for _, ended := range []string{"completed", "failed", "cancelled"} {
		t.Run(ended, func(t *testing.T) {
			issueID, agentID := handoffDispatchFixture(t)
			moveIntoHandoffStep(t, issueID, nil)
			intent := wantOneIntent(t, issueID)
			if intent.State != "dispatched" || intent.TaskID == "" || intent.TargetType != "agent" {
				t.Fatalf("intent after the move = %+v, want dispatched with a task", intent)
			}
			dbfx.Exec(t, `UPDATE agent_task_queue SET status = $2 WHERE id = $1`, intent.TaskID, ended)
			pendingAgain(t, intent.ID)

			got, err := testHandler.dispatchHandoffIntent(context.Background(), parseUUID(intent.ID))
			if err != nil {
				t.Fatal(err)
			}

			if n := taskCount(t, issueID); n != 1 {
				t.Fatalf("runs on the issue = %d, want 1 (agent %s): the first run ended %s", n, agentID, ended)
			}
			if got.State != "dispatched" || uuidToString(got.TaskID) != intent.TaskID {
				t.Fatalf("intent = (%s, %s), want dispatched with the first task %s", got.State, uuidToString(got.TaskID), intent.TaskID)
			}
		})
	}
}

// Acceptance 2: two dispatchers race on one intent. One run; the loser reads it
// as delivered.
func TestDispatchHandoffIntent_RaceMakesOneRun(t *testing.T) {
	issueID, _ := handoffDispatchFixture(t)
	moveIntoHandoffStep(t, issueID, nil)
	intent := wantOneIntent(t, issueID)
	dbfx.Exec(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	pendingAgain(t, intent.ID)

	var wg sync.WaitGroup
	results := make([]db.IssueHandoffDispatch, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = testHandler.dispatchHandoffIntent(context.Background(), parseUUID(intent.ID))
		}()
	}
	wg.Wait()

	if n := taskCount(t, issueID); n != 1 {
		t.Fatalf("runs on the issue = %d, want 1", n)
	}
	for i, r := range results {
		if errs[i] != nil {
			t.Fatalf("dispatcher %d: %v", i, errs[i])
		}
		if r.State != "dispatched" || uuidToString(r.TaskID) == "" {
			t.Fatalf("dispatcher %d read the intent as (%s, %q), want dispatched with a task", i, r.State, uuidToString(r.TaskID))
		}
	}
	if results[0].TaskID != results[1].TaskID {
		t.Fatalf("dispatchers disagree on the run: %s vs %s", uuidToString(results[0].TaskID), uuidToString(results[1].TaskID))
	}
}

// Acceptance 3: the destination agent subscribed to the move's event. It hears
// the change once, as the run, and that run is the one the intent records.
func TestDispatchHandoffIntent_EventWakeupRuleGivesOneRunAndSettlesReceipt(t *testing.T) {
	issueID, agentID := handoffDispatchFixture(t)
	wakeupID := subscribeToIssueEvent(t, issueID, agentID, "issue.status_changed")

	moveIntoHandoffStep(t, issueID, nil)

	intent := wantOneIntent(t, issueID)
	runs := pendingStepRuns(t, issueID, agentID)
	if len(runs) != 1 || intent.State != "dispatched" || intent.TaskID == "" {
		t.Fatalf("runs = %d, intent = %+v, want one run recorded on a dispatched intent", len(runs), intent)
	}
	if n := pendingEventReceipts(t, wakeupID); n != 0 {
		t.Fatalf("pending receipts = %d, want 0: the change would reach the agent twice", n)
	}
}

// Acceptance 4: another agent @-mentioned in the handoff comment still gets the
// run its mention starts, next to the destination's run.
func TestDispatchHandoffIntent_MentionedThirdAgentKeepsItsRun(t *testing.T) {
	issueID, destID := handoffDispatchFixture(t)
	thirdID := createHandlerTestAgent(t, "che5486-third", nil)
	commentOn(t, issueID, "[@Third](mention://agent/"+thirdID+") please look, then hand off", "", "")
	var commentID string
	dbfx.QueryRow(t, `SELECT id::text FROM comment WHERE issue_id = $1 ORDER BY created_at DESC LIMIT 1`, issueID).Scan(&commentID)

	moveIntoHandoffStep(t, issueID, map[string]any{"handoff_comment_id": commentID})

	intent := wantOneIntent(t, issueID)
	if intent.State != "dispatched" {
		t.Fatalf("intent = %+v, want dispatched", intent)
	}
	for name, agent := range map[string]string{"destination": destID, "mentioned": thirdID} {
		if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, issueID, agent); n != 1 {
			t.Errorf("%s agent runs = %d, want 1", name, n)
		}
	}
}

// Acceptance 5: a handoff to a member, or to nobody, has no run to protect.
func TestHandoffToMemberOrNoneWritesNoIntent(t *testing.T) {
	seedTestCatalog(t)
	for name, handler := range map[string]map[string]any{
		"member": {"type": "member", "id": testUserID},
		"none":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			projectID := createWorkflowTestProject(t, "Intent-free handoff "+name)
			column := map[string]any{"instructions": "Look at it."}
			if handler != nil {
				column["handler"] = handler
			}
			setColumnHandoff(t, projectID, "in_progress", column).Want(http.StatusOK)
			issueID := dbfx.Issue(t, "intent-free "+name, testutil.Cols{"project_id": projectID, "status": "todo"})
			t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue_handoff_dispatch WHERE issue_id = $1`, issueID) })

			moveIntoHandoffStep(t, issueID, nil)

			if got := handoffIntents(t, issueID); len(got) != 0 {
				t.Fatalf("handoff intents = %+v, want none for a %s handoff", got, name)
			}
		})
	}
}
