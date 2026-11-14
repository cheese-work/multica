package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/bgagents"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Claim and run deadlines of output-only background tasks (CHE-4103, CHE-4013
// step 8). Tests backdate created_at / started_at instead of sleeping.

const deadlineSweepBatch = 50

func (b *bgFailoverFixture) sweep(t *testing.T) (cancelled, failed int) {
	t.Helper()
	cancelled, failed, err := b.svc.SweepBackgroundDeadlines(context.Background(), deadlineSweepBatch)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return cancelled, failed
}

// age backdates the task's creation, and its start when it has one.
func (b *bgFailoverFixture) age(t *testing.T, task db.AgentTaskQueue, seconds int) {
	t.Helper()
	b.rows.Exec(t, `UPDATE agent_task_queue SET created_at = created_at - make_interval(secs => $2),
		started_at = started_at - make_interval(secs => $2) WHERE id = $1`, util.UUIDToString(task.ID), float64(seconds))
}

func (b *bgFailoverFixture) start(t *testing.T, task db.AgentTaskQueue) {
	t.Helper()
	b.rows.Exec(t, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, util.UUIDToString(task.ID))
}

func (b *bgFailoverFixture) status(t *testing.T, task db.AgentTaskQueue) (status, reason string) {
	t.Helper()
	if err := b.pool.QueryRow(context.Background(), `SELECT status, COALESCE(failure_reason, '') FROM agent_task_queue WHERE id = $1`,
		task.ID).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	return status, reason
}

func (b *bgFailoverFixture) quickActionsInput() BackgroundTaskInput {
	in := b.input()
	in.Job = bgagents.QuickActions
	return in
}

func TestBackgroundSweepMovesAnUnclaimedTaskToTheNextSlot(t *testing.T) {
	b := newBGFailoverFixture(t)
	fresh := b.enqueue(t)
	b.age(t, fresh, 5) // inside the 10 s claim deadline
	if cancelled, failed := b.sweep(t); cancelled != 0 || failed != 0 {
		t.Fatalf("a task inside its claim deadline was swept: cancelled=%d failed=%d", cancelled, failed)
	}

	b.age(t, fresh, 6) // 11 s: past it
	if cancelled, failed := b.sweep(t); cancelled != 1 || failed != 0 {
		t.Fatalf("sweep cancelled=%d failed=%d, want 1/0", cancelled, failed)
	}
	if status, _ := b.status(t, fresh); status != "cancelled" {
		t.Fatalf("unclaimed primary status = %q, want cancelled", status)
	}
	if got := b.queued(t, b.fallback); got != 1 {
		t.Fatalf("fallback runs queued = %d, want 1", got)
	}
	if len(b.noAgentCalls) != 0 {
		t.Fatalf("no-agent path ran with a fallback available: %v", b.noAgentCalls)
	}
}

func TestBackgroundSweepFailsARunPastItsRunDeadlineThenRunsTheNoAgentPath(t *testing.T) {
	b := newBGFailoverFixture(t)
	primary := b.enqueue(t)
	b.start(t, primary)
	b.age(t, primary, 59)
	if cancelled, failed := b.sweep(t); cancelled != 0 || failed != 0 {
		t.Fatalf("a run inside its 60 s deadline was swept: cancelled=%d failed=%d", cancelled, failed)
	}

	b.age(t, primary, 2)
	if cancelled, failed := b.sweep(t); cancelled != 0 || failed != 1 {
		t.Fatalf("sweep cancelled=%d failed=%d, want 0/1", cancelled, failed)
	}
	if status, reason := b.status(t, primary); status != "failed" || reason != "bg_run_timeout" {
		t.Fatalf("primary status/reason = %q/%q, want failed/bg_run_timeout", status, reason)
	}
	if got := b.queued(t, b.fallback); got != 1 {
		t.Fatalf("fallback runs queued = %d, want 1", got)
	}

	var fb db.AgentTaskQueue
	if err := b.pool.QueryRow(context.Background(), `SELECT id FROM agent_task_queue WHERE agent_id = $1 AND purpose = 'bg:dm_title'`, b.fallback).Scan(&fb.ID); err != nil {
		t.Fatal(err)
	}
	b.start(t, fb)
	b.age(t, fb, 61)
	b.sweep(t)
	if len(b.noAgentCalls) != 1 || b.noAgentCalls[0] != "fallback" {
		t.Fatalf("no-agent path calls = %v, want one for the fallback slot (deterministic title)", b.noAgentCalls)
	}
}

func TestBackgroundSweepQuickActionsGivesNoPills(t *testing.T) {
	b := newBGFailoverFixture(t)
	var noAgent, written int
	purpose := bgagents.Purpose(bgagents.QuickActions)
	// The enqueue re-reads the stored chain (CHE-4114): give the member a personal one.
	b.rows.Exec(t, `UPDATE member_setting SET value = value || jsonb_build_object($3::text, jsonb_build_object('agent_id', $4::text))
		WHERE workspace_id = $1 AND user_id = $2 AND key = 'background_agents'`,
		b.workspaceID, b.userID, string(bgagents.QuickActions), b.primary)
	b.svc.BackgroundNoAgent[purpose] = func(context.Context, db.AgentTaskQueue) { noAgent++ }
	b.svc.BackgroundWriters = map[string]BackgroundWriter{
		purpose: func(context.Context, *db.Queries, db.AgentTaskQueue, []byte) error { written++; return nil },
	}
	enqueue := func() db.AgentTaskQueue {
		task, err := b.svc.EnqueueBackgroundChain(context.Background(), b.quickActionsInput(),
			[]bgagents.Candidate{{Slot: bgagents.Primary, AgentID: b.primary}})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}

	unclaimed := enqueue()
	b.age(t, unclaimed, 6) // 5 s claim deadline
	b.sweep(t)
	if status, _ := b.status(t, unclaimed); status != "cancelled" || noAgent != 1 {
		t.Fatalf("unclaimed quick_actions status=%q no-agent calls=%d, want cancelled/1", status, noAgent)
	}

	running := enqueue()
	b.start(t, running)
	b.age(t, running, 21) // 20 s run deadline
	b.sweep(t)
	if status, reason := b.status(t, running); status != "failed" || reason != "bg_run_timeout" || noAgent != 2 {
		t.Fatalf("running quick_actions status/reason=%q/%q no-agent calls=%d, want failed/bg_run_timeout/2", status, reason, noAgent)
	}
	if written != 0 {
		t.Fatalf("pills written %d times, want 0", written)
	}
}

// A completion from an expired or cancelled attempt is refused and writes
// nothing. Three ways an attempt gets there: its run deadline passed but the
// sweep has not run yet, the sweep failed it, the sweep cancelled it unclaimed.
func TestBackgroundLateCompletionOfAnExpiredAttemptWritesNothing(t *testing.T) {
	setup := func(t *testing.T) (*bgFailoverFixture, db.AgentTaskQueue, *int) {
		b := newBGFailoverFixture(t)
		writes := new(int)
		b.svc.BackgroundWriters = map[string]BackgroundWriter{
			bgagents.Purpose(bgagents.DMTitle): func(context.Context, *db.Queries, db.AgentTaskQueue, []byte) error {
				*writes++
				return nil
			},
		}
		task := b.enqueue(t)
		return b, task, writes
	}
	complete := func(b *bgFailoverFixture, task db.AgentTaskQueue) {
		// The outcome (error or idempotent no-op) is not the contract; the
		// writer staying silent and the status staying put are.
		_, _ = b.svc.CompleteTask(context.Background(), task.ID, json.RawMessage(`"A real title"`), "", "", "", false, "", "")
	}
	cases := []struct {
		name       string
		prepare    func(*testing.T, *bgFailoverFixture, db.AgentTaskQueue)
		wantStatus string
	}{
		{"past the run deadline, not swept yet", func(t *testing.T, b *bgFailoverFixture, task db.AgentTaskQueue) {
			b.start(t, task)
			b.age(t, task, 61)
		}, "running"},
		{"failed by the sweep", func(t *testing.T, b *bgFailoverFixture, task db.AgentTaskQueue) {
			b.start(t, task)
			b.age(t, task, 61)
			b.sweep(t)
		}, "failed"},
		{"cancelled unclaimed by the sweep", func(t *testing.T, b *bgFailoverFixture, task db.AgentTaskQueue) {
			b.age(t, task, 11)
			b.sweep(t)
		}, "cancelled"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, task, writes := setup(t)
			c.prepare(t, b, task)
			complete(b, task)
			if *writes != 0 {
				t.Fatalf("writer ran %d times for an expired attempt", *writes)
			}
			if status, _ := b.status(t, task); status != c.wantStatus {
				t.Fatalf("status after the late completion = %q, want %q", status, c.wantStatus)
			}
		})
	}
}

func (b *bgFailoverFixture) claim(t *testing.T) (db.AgentTaskQueue, error) {
	t.Helper()
	return db.New(b.pool).ClaimAgentTask(context.Background(), db.ClaimAgentTaskParams{
		PrepareLeaseSecs: 60, AgentID: util.MustParseUUID(b.primary), RuntimeID: util.MustParseUUID(b.primaryRuntime), RuntimeStaleSecs: 3600,
	})
}

// A task past its claim deadline is never handed to a daemon, even before the
// sweep cancels it: the claim itself refuses it (CHE-4103 review round 1).
func TestBackgroundClaimRefusesATaskPastItsClaimDeadline(t *testing.T) {
	b := newBGFailoverFixture(t)
	task := b.enqueue(t)
	b.age(t, task, 11)
	if claimed, err := b.claim(t); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("claim of a task past its 10 s claim deadline = %v, %v; want no rows", claimed.ID, err)
	}

	fresh := newBGFailoverFixture(t)
	fresh.enqueue(t)
	if _, err := fresh.claim(t); err != nil {
		t.Fatalf("claim inside the claim deadline: %v", err)
	}
}

// A task claimed in time that never starts must not sit dispatched for minutes:
// the start bound is the claim deadline again, counted from the claim.
func TestBackgroundSweepFailsADispatchedTaskThatNeverStarts(t *testing.T) {
	b := newBGFailoverFixture(t)
	task := b.enqueue(t)
	if _, err := b.claim(t); err != nil {
		t.Fatal(err)
	}
	b.rows.Exec(t, `UPDATE agent_task_queue SET dispatched_at = now() - interval '9 seconds' WHERE id = $1`, util.UUIDToString(task.ID))
	if cancelled, failed := b.sweep(t); cancelled != 0 || failed != 0 {
		t.Fatalf("a claim inside the start bound was swept: %d/%d", cancelled, failed)
	}

	b.rows.Exec(t, `UPDATE agent_task_queue SET dispatched_at = now() - interval '11 seconds' WHERE id = $1`, util.UUIDToString(task.ID))
	if _, failed := b.sweep(t); failed != 1 {
		t.Fatalf("sweep failed=%d, want 1", failed)
	}
	if status, reason := b.status(t, task); status != "failed" || reason != "bg_claim_timeout" {
		t.Fatalf("status/reason = %q/%q, want failed/bg_claim_timeout", status, reason)
	}
	if got := b.queued(t, b.fallback); got != 1 {
		t.Fatalf("fallback runs queued = %d, want 1", got)
	}
}

// The start is fenced too: an expired dispatched attempt cannot become running
// between the deadline and the sweep.
func TestBackgroundStartRefusedPastTheStartBound(t *testing.T) {
	b := newBGFailoverFixture(t)
	task := b.enqueue(t)
	if _, err := b.claim(t); err != nil {
		t.Fatal(err)
	}
	b.rows.Exec(t, `UPDATE agent_task_queue SET dispatched_at = now() - interval '11 seconds' WHERE id = $1`, util.UUIDToString(task.ID))
	var dispatched pgtype.Timestamptz
	if err := b.pool.QueryRow(context.Background(), `SELECT dispatched_at FROM agent_task_queue WHERE id = $1`, task.ID).Scan(&dispatched); err != nil {
		t.Fatal(err)
	}
	_, err := b.svc.StartTaskForClaim(context.Background(), db.LockAgentTaskStartClaimParams{
		ID: task.ID, RuntimeID: util.MustParseUUID(b.primaryRuntime), DispatchedAt: dispatched,
	})
	if !errors.Is(err, ErrBackgroundAttemptExpired) {
		t.Fatalf("start past the start bound = %v, want ErrBackgroundAttemptExpired", err)
	}
	if status, _ := b.status(t, task); status != "dispatched" {
		t.Fatalf("status after the refused start = %q, want dispatched", status)
	}
}
