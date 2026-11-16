package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// CHE-6189 (CHE-6165 fix 2b): a continuation-watchdog fire over an issue that is
// as it was when the last watchdog run started starts no run. The watchdog
// (multica-dotfiles) arms a fresh one-shot `at` wakeup per recovery, so the
// comparison cannot hang on the wakeup id.

var watchdogSeq int

func (e skipEnv) watchdogWith(t *testing.T, text string) pgtype.UUID {
	t.Helper()
	w := wakeCreate(t, e.f, e.s, e.issue, WakeupInput{AgentID: e.agent, Kind: "at", AfterSeconds: 60, Instruction: text})
	return w.ID
}

func (e skipEnv) watchdog(t *testing.T) pgtype.UUID {
	t.Helper()
	watchdogSeq++
	return e.watchdogWith(t, fmt.Sprintf("[continuation-watchdog %012x] No continuation for CHE-1: idle 5h. Finish the work or establish a continuation.", watchdogSeq))
}

func (e skipEnv) watchdogRuns(t *testing.T) int {
	t.Helper()
	return e.f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND context->>'wakeup_watchdog'='continuation'`, e.issue)
}

func TestWatchdogFireWithUnchangedSnapshotStartsNoRun(t *testing.T) {
	e := newSkipEnv(t, true)
	wakeTick(t, e.f, e.s, e.watchdog(t))
	e.finishRuns(t, "completed")
	wakeTick(t, e.f, e.s, e.watchdog(t))
	if got := e.watchdogRuns(t); got != 1 {
		t.Fatalf("watchdog runs after an unchanged snapshot = %d, want 1", got)
	}
	if got := e.skipped(t); got != 1 {
		t.Fatalf("skip activities = %d, want 1", got)
	}
}

// Most idle issues have no PR yet; the status and assignee still decide.
func TestWatchdogFireWithoutLinkedPRSkipsOnUnchangedSnapshot(t *testing.T) {
	e := newSkipEnv(t, false)
	wakeTick(t, e.f, e.s, e.watchdog(t))
	e.finishRuns(t, "completed")
	wakeTick(t, e.f, e.s, e.watchdog(t))
	if got := e.watchdogRuns(t); got != 1 {
		t.Fatalf("watchdog runs after an unchanged snapshot = %d, want 1", got)
	}
	if got := e.skipped(t); got != 1 {
		t.Fatalf("skip activities = %d, want 1", got)
	}
}

func TestWatchdogFirstFireStartsARun(t *testing.T) {
	e := newSkipEnv(t, true)
	wakeTick(t, e.f, e.s, e.watchdog(t))
	if got := e.watchdogRuns(t); got != 1 {
		t.Fatalf("first watchdog fire runs = %d, want 1", got)
	}
}

func TestWatchdogFireRunsWhenIssueStatusChanged(t *testing.T) {
	e := newSkipEnv(t, true)
	wakeTick(t, e.f, e.s, e.watchdog(t))
	e.finishRuns(t, "completed")
	e.f.Exec(t, `UPDATE issue SET status='blocked' WHERE id=$1`, e.issue)
	wakeTick(t, e.f, e.s, e.watchdog(t))
	if got := e.watchdogRuns(t); got != 2 {
		t.Fatalf("watchdog runs after a status change = %d, want 2", got)
	}
}

func TestWatchdogFireRunsWhenPRHeadChanged(t *testing.T) {
	e := newSkipEnv(t, true)
	wakeTick(t, e.f, e.s, e.watchdog(t))
	e.finishRuns(t, "completed")
	e.f.Exec(t, `UPDATE github_pull_request SET head_sha='h2' WHERE id=$1`, e.prID)
	wakeTick(t, e.f, e.s, e.watchdog(t))
	if got := e.watchdogRuns(t); got != 2 {
		t.Fatalf("watchdog runs after a new head = %d, want 2", got)
	}
}

func TestWatchdogFireRunsWhenCommentArrived(t *testing.T) {
	e := newSkipEnv(t, false)
	wakeTick(t, e.f, e.s, e.watchdog(t))
	e.finishRuns(t, "completed")
	e.f.Exec(t, `INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type) VALUES ($1, $2, 'member', $3, 'new input', 'comment')`, e.issue, e.f.WorkspaceID, e.f.UserID)
	wakeTick(t, e.f, e.s, e.watchdog(t))
	if got := e.watchdogRuns(t); got != 2 {
		t.Fatalf("watchdog runs after a new comment = %d, want 2", got)
	}
}

// Fail open: a last run whose snapshot was not stored cannot vouch for the state.
func TestWatchdogFireRunsWhenLastSnapshotIsUnreadable(t *testing.T) {
	e := newSkipEnv(t, true)
	wakeTick(t, e.f, e.s, e.watchdog(t))
	e.finishRuns(t, "completed")
	e.f.Exec(t, `UPDATE agent_task_queue SET context = context - 'wakeup_input_fingerprint' WHERE issue_id=$1`, e.issue)
	wakeTick(t, e.f, e.s, e.watchdog(t))
	if got := e.watchdogRuns(t); got != 2 {
		t.Fatalf("watchdog runs after an unreadable snapshot = %d, want 2", got)
	}
}

// A run that did not complete says nothing about the state.
func TestWatchdogFireRunsWhenLastRunFailed(t *testing.T) {
	e := newSkipEnv(t, true)
	wakeTick(t, e.f, e.s, e.watchdog(t))
	e.finishRuns(t, "failed")
	wakeTick(t, e.f, e.s, e.watchdog(t))
	if got := e.watchdogRuns(t); got != 2 {
		t.Fatalf("watchdog runs after a failed run = %d, want 2", got)
	}
}

// Deadline and outbox markers of the same script carry facts the snapshot does
// not hold, so only the recovery wakeup is compared.
func TestOtherWatchdogMarkersAlwaysRun(t *testing.T) {
	e := newSkipEnv(t, true)
	text := "[continuation-watchdog deadline:abc:2026-11-16] CHE-1 is past due."
	wakeTick(t, e.f, e.s, e.watchdogWith(t, text))
	e.finishRuns(t, "completed")
	wakeTick(t, e.f, e.s, e.watchdogWith(t, text))
	if got := e.f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND context ? 'wakeup_id'`, e.issue); got != 2 {
		t.Fatalf("deadline-marker runs = %d, want 2", got)
	}
	if got := e.skipped(t); got != 0 {
		t.Fatalf("skip activities = %d, want 0", got)
	}
}

// A person's "wake now" is a chosen run.
func TestWatchdogWakeNowByAPersonIsNeverSkipped(t *testing.T) {
	e := newSkipEnv(t, true)
	wakeTick(t, e.f, e.s, e.watchdog(t))
	e.finishRuns(t, "completed")
	id := e.watchdog(t)
	if err := e.s.Trigger(context.Background(), e.issue, id, parseTestUUID(t, e.f.UserID)); err != nil {
		t.Fatal(err)
	}
	// The manual run carries no snapshot key; count every wakeup run instead.
	if got := e.f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND context ? 'wakeup_id'`, e.issue); got != 2 {
		t.Fatalf("watchdog runs after wake now = %d, want 2", got)
	}
}

// CHE-6189 review round 1: the comparison is to the agent's last completed run,
// not only to its last watchdog run. A later ordinary run may have seen another
// state and left it, so an older watchdog snapshot cannot vouch for now.
func TestWatchdogFireRunsWhenAnOrdinaryRunCameAfterTheLastWatchdogRun(t *testing.T) {
	e := newSkipEnv(t, true)
	wakeTick(t, e.f, e.s, e.watchdog(t))
	e.finishRuns(t, "completed")
	e.f.Exec(t, `INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, context, completed_at)
		SELECT agent_id, runtime_id, issue_id, 'completed', priority, '{}'::jsonb, clock_timestamp() FROM agent_task_queue WHERE issue_id=$1 LIMIT 1`, e.issue)
	wakeTick(t, e.f, e.s, e.watchdog(t))
	if got := e.watchdogRuns(t); got != 2 {
		t.Fatalf("watchdog runs after a later ordinary run = %d, want 2", got)
	}
	if got := e.skipped(t); got != 0 {
		t.Fatalf("skip activities = %d, want 0", got)
	}
}
