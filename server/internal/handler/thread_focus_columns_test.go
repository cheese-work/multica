package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-6426 (S1 of CHE-6385): CreateComment stores whether a comment is an FYI.
func TestCreateCommentStoresNotifyOnly(t *testing.T) {
	f := newAskGuardCommentFixture(t)

	f.post(t, map[string]any{"content": "FYI only", "notify_only": true}).Want(http.StatusCreated)
	f.post(t, map[string]any{"content": "plain note"}).Want(http.StatusCreated)

	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content = 'FYI only' AND notify_only IS TRUE`, f.issueID); n != 1 {
		t.Errorf("notify_only comments stored true = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content = 'plain note' AND notify_only IS FALSE`, f.issueID); n != 1 {
		t.Errorf("plain comments stored false = %d, want 1", n)
	}
}

// A row written before the migration has no answer: the column stays NULL, not false.
func TestCommentNotifyOnlyIsNullWithoutAWriter(t *testing.T) {
	f := newAskGuardCommentFixture(t)
	dbfx.Exec(t, `INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type) VALUES ($1, $2, 'agent', $3, 'old row', 'comment')`, f.issueID, testWorkspaceID, f.agentID)

	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content = 'old row' AND notify_only IS NULL AND ask_source_root_id IS NULL`, f.issueID); n != 1 {
		t.Fatalf("old-style row with NULL notify_only = %d, want 1", n)
	}
}

// The recovery and repair workers read every open `block` row of an ended task.
// A `move` or `refused` row must never reach them (plan r3 F7).
func TestAskGuardWorkersIgnoreMoveAndRefusedRows(t *testing.T) {
	f := newIntentFixture(t)
	dbfx.Cleanup(t, `DELETE FROM ask_guard_event WHERE issue_id = $1`, f.issueID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, f.taskID)
	for _, mode := range []string{"move", "refused"} {
		dbfx.Exec(t, `INSERT INTO ask_guard_event (id, workspace_id, issue_id, task_id, operation_kind, payload_sha256, mode, root_id)
			VALUES ($1, $2, $3, $4, 'thread_focus_test', $5, $6, $1)`, uuid.NewString(), testWorkspaceID, f.issueID, f.taskID, uuid.NewString(), mode)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM ask_guard_event WHERE issue_id = $1 AND mode IN ('move', 'refused')`, f.issueID); n != 2 {
		t.Fatalf("stored move/refused rows = %d, want 2", n)
	}

	ctx := context.Background()
	recoverable, err := testHandler.Queries.ListAskGuardRecoverableEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := testHandler.Queries.ListAskRecoveryEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	repair, err := testHandler.Queries.ListAskRepairEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for name, events := range map[string][]string{
		"ListAskGuardRecoverableEvents": eventIssueIDs(recoverable),
		"ListAskRecoveryEvents":         eventIssueIDs(recovery),
		"ListAskRepairEvents":           eventIssueIDs(repair),
	} {
		for _, issue := range events {
			if issue == f.issueID {
				t.Errorf("%s returned a move or refused row", name)
			}
		}
	}

	deliverRecoveries(t)
	if n := dbfx.Count(t, `SELECT count(*) FROM ask_recovery_notice n JOIN ask_guard_event e ON e.id = n.event_id WHERE e.issue_id = $1`, f.issueID); n != 0 {
		t.Errorf("recovery notices for move/refused rows = %d, want 0", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM ask_guard_event WHERE issue_id = $1 AND (cleared_at IS NOT NULL OR repair_reason IS NOT NULL)`, f.issueID); n != 0 {
		t.Errorf("move/refused rows touched by workers = %d, want 0", n)
	}
}

func eventIssueIDs(events []db.AskGuardEvent) []string {
	ids := make([]string, 0, len(events))
	for _, event := range events {
		ids = append(ids, uuidToString(event.IssueID))
	}
	return ids
}

// ListAskGuardIntentTargets filters neither mode nor operation_kind: it skips a
// target that already has an event for the same intent and member. A move or
// refused row (no intent) for the same member must not hide the target.
func TestIntentTargetReconcileIgnoresMoveAndRefusedRows(t *testing.T) {
	setAskGuardMode(t, "block")
	fixture := newIntentFixture(t)
	a, _, intentID := twoTargetIntent(t, fixture)
	for _, mode := range []string{"move", "refused"} {
		dbfx.Exec(t, `INSERT INTO ask_guard_event (id, workspace_id, issue_id, task_id, operation_kind, payload_sha256, mode, member_id, root_id)
			VALUES ($1, $2, $3, $4, 'thread_focus_test', $5, $6, $7, $1)`, uuid.NewString(), testWorkspaceID, fixture.issueID, fixture.taskID, uuid.NewString(), mode, a)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM ask_guard_event WHERE issue_id = $1`, fixture.issueID)
	})
	endTaskAndSeal(t, fixture, intentID)
	reconcileAskGuard(t)

	if n := dbfx.Count(t, `SELECT count(*) FROM ask_guard_event WHERE intent_id = $1`, intentID); n != 2 {
		t.Fatalf("intent target events = %d, want 2 (both members)", n)
	}
}
