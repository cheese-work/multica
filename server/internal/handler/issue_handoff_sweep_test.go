package handler

import (
	"context"
	"errors"
	"testing"
	"time"
)

// CHE-5488 (S1c): the recovery sweep re-dispatches a handoff intent whose run
// was not started, and only that.

// failDispatchAt makes dispatchHandoffIntent fail at point until the returned
// func is called. The intent stays pending, as after a crash at that point.
func failDispatchAt(t *testing.T, point string) (release func()) {
	t.Helper()
	testHandler.handoffDispatchHook = func(p string) error {
		if p == point {
			return errors.New("test hook: stopped at " + p)
		}
		return nil
	}
	release = func() { testHandler.handoffDispatchHook = nil }
	t.Cleanup(release)
	return release
}

// dueIntent makes the intent old enough, and due, for the sweep.
func dueIntent(t *testing.T, intentID string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE issue_handoff_dispatch SET created_at = now() - interval '2 minutes', next_attempt_at = now() - interval '2 minutes' WHERE id = $1`, intentID)
}

func sweepHandoffs() { testHandler.SweepHandoffIntents(context.Background(), time.Now()) }

type runAttribution struct {
	AgentID, TriggerComment, Originator, Accountable, Source string
}

func attributionOf(t *testing.T, issueID string) runAttribution {
	t.Helper()
	var a runAttribution
	dbfx.QueryRow(t, `SELECT agent_id::text, COALESCE(trigger_comment_id::text, ''), COALESCE(originator_user_id::text, ''),
		COALESCE(accountable_user_id::text, ''), COALESCE(originator_source, '')
		FROM agent_task_queue WHERE issue_id = $1`, issueID).Scan(&a.AgentID, &a.TriggerComment, &a.Originator, &a.Accountable, &a.Source)
	return a
}

// Acceptance 1: dispatch fails after the status commit. The issue has moved and
// no run exists; the sweep then makes exactly one run, built as the first try.
func TestSweepHandoffIntents_RedispatchesALostRunWithTheFirstTriesAttribution(t *testing.T) {
	// The run a healthy first try makes, for comparison.
	wantIssue, _ := handoffDispatchFixture(t)
	commentOn(t, wantIssue, "handoff context", "", "")
	var wantComment string
	dbfx.QueryRow(t, `SELECT id::text FROM comment WHERE issue_id = $1 ORDER BY created_at DESC LIMIT 1`, wantIssue).Scan(&wantComment)
	moveIntoHandoffStep(t, wantIssue, map[string]any{"handoff_comment_id": wantComment})
	want := attributionOf(t, wantIssue)
	want.TriggerComment = "" // ids differ between the two issues; compared by presence below

	issueID, agentID := handoffDispatchFixture(t)
	commentOn(t, issueID, "handoff context", "", "")
	var commentID string
	dbfx.QueryRow(t, `SELECT id::text FROM comment WHERE issue_id = $1 ORDER BY created_at DESC LIMIT 1`, issueID).Scan(&commentID)
	failDispatchAt(t, "before_dispatch")
	moveIntoHandoffStep(t, issueID, map[string]any{"handoff_comment_id": commentID})
	intent := wantOneIntent(t, issueID)
	if n := taskCount(t, issueID); n != 0 || intent.State != "pending" {
		t.Fatalf("after the failed dispatch: runs = %d, intent = %+v, want no run and a pending intent", n, intent)
	}
	testHandler.handoffDispatchHook = nil
	dueIntent(t, intent.ID)

	sweepHandoffs()
	sweepHandoffs()

	if n := taskCount(t, issueID); n != 1 {
		t.Fatalf("runs after the sweep = %d, want exactly 1 (agent %s)", n, agentID)
	}
	got := attributionOf(t, issueID)
	if got.TriggerComment != commentID {
		t.Errorf("run context comment = %q, want %q", got.TriggerComment, commentID)
	}
	got.TriggerComment = ""
	if got != want {
		t.Errorf("sweep run attribution = %+v, first-try run = %+v", got, want)
	}
	if after := wantOneIntent(t, issueID); after.State != "dispatched" || after.TaskID == "" {
		t.Errorf("intent after the sweep = %+v, want dispatched with a task", after)
	}
}

// Acceptance 2: the process stopped after the task insert committed. The task
// ran to completed; the sweep must not make a second one.
func TestSweepHandoffIntents_CrashAfterTheRunCommittedMakesNoSecondRun(t *testing.T) {
	issueID, _ := handoffDispatchFixture(t)
	failDispatchAt(t, "after_commit")
	moveIntoHandoffStep(t, issueID, nil)
	intent := wantOneIntent(t, issueID)
	if intent.TaskID == "" || taskCount(t, issueID) != 1 {
		t.Fatalf("setup: intent = %+v, runs = %d, want the run committed", intent, taskCount(t, issueID))
	}
	testHandler.handoffDispatchHook = nil
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, intent.TaskID)
	// Even an intent that never recorded its run finds the run it made.
	pendingAgain(t, intent.ID)
	dueIntent(t, intent.ID)

	sweepHandoffs()

	if n := taskCount(t, issueID); n != 1 {
		t.Fatalf("runs after the sweep = %d, want 1: the completed run is the delivery", n)
	}
	if after := wantOneIntent(t, issueID); after.State != "dispatched" || after.TaskID != intent.TaskID {
		t.Fatalf("intent = %+v, want dispatched with the first task %s", after, intent.TaskID)
	}
}

// Acceptance 3: the issue moved again before the sweep.
func TestSweepHandoffIntents_SupersededIntentMakesNoRun(t *testing.T) {
	issueID, _ := handoffDispatchFixture(t)
	failDispatchAt(t, "before_dispatch")
	moveIntoHandoffStep(t, issueID, nil)
	intent := wantOneIntent(t, issueID)
	testHandler.handoffDispatchHook = nil
	time.Sleep(10 * time.Millisecond) // the later move gets a later timestamp
	issueWriters[0].write(t, issueID, map[string]any{"status": "todo"}, nil).Want(200)
	dueIntent(t, intent.ID)

	sweepHandoffs()

	if n := taskCount(t, issueID); n != 0 {
		t.Fatalf("runs = %d, want 0: the issue moved on", n)
	}
	var state, reason string
	dbfx.QueryRow(t, `SELECT state, COALESCE(skip_reason, '') FROM issue_handoff_dispatch WHERE id = $1`, intent.ID).Scan(&state, &reason)
	if state != "skipped" || reason != "superseded" {
		t.Fatalf("intent = (%s, %q), want (skipped, superseded)", state, reason)
	}
}

// Acceptance 4: five sweep attempts fail; the intent fails and the timeline says so.
func TestSweepHandoffIntents_FiveFailedAttemptsFailTheIntentWithATimelineEntry(t *testing.T) {
	issueID, _ := handoffDispatchFixture(t)
	failDispatchAt(t, "before_dispatch")
	moveIntoHandoffStep(t, issueID, nil)
	intent := wantOneIntent(t, issueID)
	dueIntent(t, intent.ID)

	var delays []time.Duration
	for attempt := 1; attempt <= 5; attempt++ {
		sweepHandoffs()
		var state string
		var attempts int
		var waitSeconds float64
		dbfx.QueryRow(t, `SELECT state, attempts, EXTRACT(EPOCH FROM next_attempt_at - now()) FROM issue_handoff_dispatch WHERE id = $1`, intent.ID).Scan(&state, &attempts, &waitSeconds)
		if attempts != attempt {
			t.Fatalf("attempts after sweep %d = %d", attempt, attempts)
		}
		if attempt < 5 {
			if state != "pending" {
				t.Fatalf("state after attempt %d = %s, want pending", attempt, state)
			}
			delays = append(delays, time.Duration(waitSeconds+0.5)*time.Second)
			// Not due yet: another sweep leaves it alone.
			sweepHandoffs()
			dbfx.QueryRow(t, `SELECT attempts FROM issue_handoff_dispatch WHERE id = $1`, intent.ID).Scan(&attempts)
			if attempts != attempt {
				t.Fatalf("a sweep before the backoff ended retried: attempts = %d", attempts)
			}
			dueIntent(t, intent.ID)
		}
	}
	if want := []time.Duration{30 * time.Second, 30 * time.Second, 60 * time.Second, 60 * time.Second}; len(delays) != 4 ||
		delays[0] != want[0] || delays[1] != want[1] || delays[2] != want[2] || delays[3] != want[3] {
		t.Errorf("backoff = %v, want %v", delays, want)
	}
	if after := wantOneIntent(t, issueID); after.State != "failed" {
		t.Fatalf("intent after five failed attempts = %+v, want failed", after)
	}
	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'handoff_run_not_started'
		AND details->>'message' LIKE 'handoff run not started: %'`, issueID).Scan(&n)
	if n != 1 {
		t.Fatalf("timeline entries = %d, want 1", n)
	}
}

// Acceptance 5: the destination agent's comment.created receipt settles on a
// sweep dispatch, so the comment does not start a second run.
func TestSweepHandoffIntents_SettlesTheDestinationsCommentReceipt(t *testing.T) {
	issueID, agentID := handoffDispatchFixture(t)
	wakeupID := subscribeToIssueEvent(t, issueID, agentID, "comment.created")
	failDispatchAt(t, "before_dispatch")
	moveIntoHandoffStep(t, issueID, nil)
	intent := wantOneIntent(t, issueID)
	var write string
	dbfx.QueryRow(t, `SELECT handoff_write FROM issue_handoff_dispatch WHERE id = $1`, intent.ID).Scan(&write)
	tx, err := testPool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(context.Background(), `SELECT set_config('multica.handoff_write', $1, true)`, write); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `INSERT INTO comment (workspace_id, issue_id, author_type, author_id, content)
		SELECT workspace_id, id, 'member', $2, 'handoff context' FROM issue WHERE id = $1`, issueID, testUserID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := pendingEventReceipts(t, wakeupID); n != 1 {
		t.Fatalf("setup: pending receipts = %d, want 1", n)
	}
	testHandler.handoffDispatchHook = nil
	dueIntent(t, intent.ID)

	sweepHandoffs()

	if runs := pendingStepRuns(t, issueID, agentID); len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	if n := pendingEventReceipts(t, wakeupID); n != 0 {
		t.Fatalf("pending receipts after the sweep dispatch = %d, want 0", n)
	}
}
