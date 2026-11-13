package handler

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-5542: replay of an unread member steer, with persisted exponential retry.

func TestReplayRetryAtSchedule(t *testing.T) {
	now := time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)
	for failures, want := range []time.Duration{0, 5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second} {
		if failures == 0 {
			if _, ok := replayRetryAt(now, 0, 0); ok {
				t.Fatal("failures=0 must not schedule a retry")
			}
			continue
		}
		at, ok := replayRetryAt(now, failures, 0)
		if !ok || at.Sub(now) != want {
			t.Fatalf("retry %d = %v (ok=%v), want +%v", failures, at.Sub(now), ok, want)
		}
		lo, _ := replayRetryAt(now, failures, -1)
		hi, _ := replayRetryAt(now, failures, 1)
		if lo.Sub(now) != want*8/10 || hi.Sub(now) != want*12/10 {
			t.Fatalf("retry %d jitter = [%v, %v], want +-20%% of %v", failures, lo.Sub(now), hi.Sub(now), want)
		}
	}
	if _, ok := replayRetryAt(now, replayMaxRetries+1, 0); ok {
		t.Fatal("a sixth retry must not be scheduled: the five attempts ran out")
	}
}

func TestTransientReplayReason(t *testing.T) {
	for _, reason := range []DispatchReasonCode{ReasonInternalError, ReasonStageQueueFull, ReasonAdmissionDeferred, ReasonAlreadyActive} {
		if !transientReplayReason(reason) {
			t.Errorf("%s must retry", reason)
		}
	}
	for _, reason := range []DispatchReasonCode{ReasonRunBudgetExhausted, ReasonAgentPaused, ReasonAttributionBlocked, ReasonInvocationNotAllowed, ReasonTargetUnavailable} {
		if transientReplayReason(reason) {
			t.Errorf("%s is a policy refusal and must not retry", reason)
		}
	}
}

// steerReceipt reads the receipt row of the fixture's steer.
type steerReceiptRow struct {
	status, reason string
	attempts       int
	next           *time.Time
}

func readSteerReceipt(t *testing.T, commentID, taskID string) steerReceiptRow {
	t.Helper()
	var r steerReceiptRow
	dbfx.QueryRow(t, `SELECT status, COALESCE(failure_reason, ''), replay_attempts, replay_next_at
		FROM task_supplement WHERE comment_id = $1 AND task_id = $2`, commentID, taskID).
		Scan(&r.status, &r.reason, &r.attempts, &r.next)
	return r
}

// unreadSteerFixture posts a member steer to a running turn and ends the turn
// without the daemon reading it, then drops the follow-up the completion queued
// so a test can drive the retry paths itself.
func unreadSteerFixture(t *testing.T) (f supplementFixture, comment CommentResponse) {
	t.Helper()
	f = newSupplementFixture(t, "codex", "running", true)
	dbfx.Exec(t, `UPDATE comment SET content = $2 WHERE id = $1`, f.triggerID,
		fmt.Sprintf("[@Supplement](mention://agent/%s) original objective", f.agentID))
	dbfx.Exec(t, `UPDATE agent_task_queue SET delivered_comment_ids = ARRAY[trigger_comment_id] WHERE id = $1`, f.taskID)
	dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, f.issueID)
	supplementRequest(t, f, "0199a4e8-22ce-7b01-bba5-777777777777", "Only fix web.").Want(http.StatusCreated).JSON(&comment)
	completeTaskViaDaemon(t, f.taskID)
	return f, comment
}

func followUps(t *testing.T, f supplementFixture, commentID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND id <> $2
		AND (trigger_comment_id = $3 OR $3 = ANY(coalesced_comment_ids))`, f.issueID, f.taskID, commentID)
}

func TestCompletionQueuesOneFollowUpAndShowsQueued(t *testing.T) {
	f, c := unreadSteerFixture(t)
	if n := followUps(t, f, c.ID); n != 1 {
		t.Fatalf("follow-ups = %d, want 1", n)
	}
	r := readSteerReceipt(t, c.ID, f.taskID)
	if r.status != "failed" || r.reason != ReplayQueuedReason || r.next != nil {
		t.Fatalf("receipt = %+v, want queued with no retry due", r)
	}
}

func TestSettleReplayedSteerBacksOffThenExhausts(t *testing.T) {
	f, c := unreadSteerFixture(t)
	comment, err := testHandler.Queries.GetComment(t.Context(), parseUUID(c.ID))
	if err != nil {
		t.Fatal(err)
	}
	// Fake clock: every attempt "runs" at a fixed instant.
	now := time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)
	transient := replayOutcome{routed: true, reason: ReasonInternalError}
	for failures := 0; failures < replayMaxRetries; failures++ {
		testHandler.settleReplayedSteer(t.Context(), parseUUID(testWorkspaceID), parseUUID(f.taskID), comment, transient, failures, now)
		r := readSteerReceipt(t, c.ID, f.taskID)
		base := replayBaseDelay << failures
		if r.reason != ReplayQueuedReason || r.attempts != failures+1 || r.next == nil {
			t.Fatalf("after failure %d receipt = %+v, want queued attempt %d", failures+1, r, failures+1)
		}
		if got := r.next.Sub(now); got < base*8/10 || got > base*12/10 {
			t.Fatalf("retry %d due in %v, want %v +-20%%", failures+1, got, base)
		}
	}
	testHandler.settleReplayedSteer(t.Context(), parseUUID(testWorkspaceID), parseUUID(f.taskID), comment, transient, replayMaxRetries, now)
	r := readSteerReceipt(t, c.ID, f.taskID)
	if r.reason != ReplayExhaustedReason || r.next != nil {
		t.Fatalf("after the last attempt receipt = %+v, want exhausted with no retry due", r)
	}
}

func TestSettleReplayedSteerDoesNotRetryAPolicyRefusal(t *testing.T) {
	f, c := unreadSteerFixture(t)
	comment, err := testHandler.Queries.GetComment(t.Context(), parseUUID(c.ID))
	if err != nil {
		t.Fatal(err)
	}
	refusal := replayOutcome{routed: true, reason: ReasonRunBudgetExhausted}
	testHandler.settleReplayedSteer(t.Context(), parseUUID(testWorkspaceID), parseUUID(f.taskID), comment, refusal, 0, time.Now())
	r := readSteerReceipt(t, c.ID, f.taskID)
	if r.status != "failed" || r.reason != string(ReasonRunBudgetExhausted) || r.next != nil {
		t.Fatalf("receipt = %+v, want the refusal reason and no retry", r)
	}
}

func TestSteerReplaySweeperRetriesDueReceiptsOnce(t *testing.T) {
	f, c := unreadSteerFixture(t)
	now := time.Now()
	dbfx.Exec(t, `DELETE FROM agent_task_queue WHERE issue_id = $1 AND id <> $2`, f.issueID, f.taskID)
	setDue := func(at time.Time) {
		dbfx.Exec(t, `UPDATE task_supplement SET failure_reason = $3, replay_attempts = 2, replay_next_at = $4
			WHERE comment_id = $1 AND task_id = $2`, c.ID, f.taskID, ReplayQueuedReason, at)
	}

	// Not due yet: the sweep leaves it alone.
	setDue(now.Add(30 * time.Second))
	testHandler.SweepSteerReplays(t.Context(), now)
	if n := followUps(t, f, c.ID); n != 0 {
		t.Fatalf("a retry that is not due queued %d runs", n)
	}

	// Due: one follow-up, and the receipt reads queued with no retry pending.
	setDue(now.Add(-time.Second))
	testHandler.SweepSteerReplays(t.Context(), now)
	if n := followUps(t, f, c.ID); n != 1 {
		t.Fatalf("due retry queued %d follow-ups, want 1", n)
	}
	if r := readSteerReceipt(t, c.ID, f.taskID); r.reason != ReplayQueuedReason || r.next != nil || r.attempts != 2 {
		t.Fatalf("receipt = %+v, want queued, no retry due, attempts kept", r)
	}

	// A second path replaying the same comment (receipt due again) adds no run.
	setDue(now.Add(-time.Second))
	testHandler.SweepSteerReplays(t.Context(), now)
	if n := followUps(t, f, c.ID); n != 1 {
		t.Fatalf("second settlement path queued %d follow-ups, want still 1", n)
	}
}

func TestSteerReplaySweeperLeasesClaimedReceipts(t *testing.T) {
	f, c := unreadSteerFixture(t)
	now := time.Now()
	dbfx.Exec(t, `UPDATE task_supplement SET failure_reason = $3, replay_next_at = $4 WHERE comment_id = $1 AND task_id = $2`,
		c.ID, f.taskID, ReplayQueuedReason, now.Add(-time.Second))
	claim := func() int {
		rows, err := testHandler.Queries.ClaimDueSupplementReplays(t.Context(), db.ClaimDueSupplementReplaysParams{
			LeaseUntil: pgtype.Timestamptz{Time: now.Add(replayLease), Valid: true},
			Now:        pgtype.Timestamptz{Time: now, Valid: true},
			BatchSize:  replayBatchSize,
		})
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	if n := claim(); n != 1 {
		t.Fatalf("first claim = %d, want 1", n)
	}
	if n := claim(); n != 0 {
		t.Fatalf("second claim = %d, want 0: the lease must hide a claimed receipt", n)
	}
}
