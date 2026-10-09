package handler

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// waitForLockWait blocks until a backend of this test database waits on a
// lock of one of the given kinds, so the race below is staged, not timed.
func waitForLockWait(t *testing.T, waitEvents ...string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := testPool.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND wait_event = ANY($1)
		`, waitEvents).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no backend waited on %v", waitEvents)
}

// OCR-185-1: a claim that commits while the fold waits on the row must win.
// The claimed run keeps its trigger and attribution; the fold reports a miss.
func TestCommentFoldLosesToConcurrentClaim_CHE1418(t *testing.T) {
	ctx := context.Background()
	fx := newIssueFoldFixture(t, "fold claim race")
	rootA := dbfx.Comment(t, fx.issueID, "thread A")
	rootB := dbfx.Comment(t, fx.issueID, "thread B")
	taskID := dbfx.Task(t, fx.agentID, testutil.Cols{"runtime_id": fx.runtime, "issue_id": fx.issueID, "trigger_comment_id": rootA})

	claim, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer claim.Rollback(ctx)
	if _, err := claim.Exec(ctx, `UPDATE agent_task_queue SET status = 'dispatched', dispatched_at = now() WHERE id = $1`, taskID); err != nil {
		t.Fatal(err)
	}

	folded := make(chan bool, 1)
	go func() {
		folded <- testHandler.foldCommentIntoQueuedIssueTask(ctx, fx.issue,
			commentAgentTrigger{Agent: fx.agent, Source: commentTriggerSourceMentionAgent}, parseUUID(rootB), pgtype.Text{})
	}()
	waitForLockWait(t, "transactionid", "tuple")
	if err := claim.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if <-folded {
		t.Fatal("fold reported coalesced into a task the claim already won")
	}
	task, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(taskID))
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "dispatched" || uuidToString(task.TriggerCommentID) != rootA || len(task.CoalescedCommentIds) != 0 {
		t.Fatalf("claimed run was rewritten: status=%s trigger=%s coalesced=%v",
			task.Status, uuidToString(task.TriggerCommentID), coalescedIDs(task))
	}
}

// SOL-185-2: two new-thread arrivals in flight at once must still leave one
// queued run. Arrival A is held mid-insert (its thread slot is occupied by an
// uncommitted row); arrival B starts while A is undecided.
func TestConcurrentNewThreadArrivalsQueueOneRun_CHE1418(t *testing.T) {
	ctx := context.Background()
	fx := newIssueFoldFixture(t, "fold concurrent arrivals")
	rootA := dbfx.Comment(t, fx.issueID, "thread A")
	rootB := dbfx.Comment(t, fx.issueID, "thread B")
	mention := commentAgentTrigger{Source: commentTriggerSourceMentionAgent}

	hold, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(ctx)
	if _, err := hold.Exec(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, trigger_comment_id)
		VALUES ($1, $2, $3, 'queued', 0, $4)
	`, fx.agentID, fx.runtime, fx.issueID, rootA); err != nil {
		t.Fatal(err)
	}

	results := make(chan DispatchStatus, 2)
	go func() { results <- fx.enqueue(t, rootA, mention) }()
	waitForLockWait(t, "transactionid")
	go func() { results <- fx.enqueue(t, rootB, mention) }()

	// B either finishes unprotected or waits behind A's decision.
	deadline := time.Now().Add(10 * time.Second)
	for len(results) == 0 && time.Now().Before(deadline) {
		var waiting int
		if err := testPool.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND wait_event = 'advisory'
		`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	got := map[DispatchStatus]int{}
	for range 2 {
		got[<-results]++
	}
	if queued := fx.tasks(t, "queued"); len(queued) != 1 {
		t.Fatalf("got %d queued runs (outcomes %v), want 1", len(queued), got)
	}
	if got[DispatchQueued] != 1 || got[DispatchCoalesced] != 1 {
		t.Fatalf("outcomes %v, want one queued and one coalesced", got)
	}
}
