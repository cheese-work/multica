package handler

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// L3a: in-place compaction ends a compact task with a summary row owned by that
// task and a bumped session epoch, not with continued_into_session_id. The
// settle guard and the 1.1x re-arm rule must read that outcome.

// startedCompaction runs one run over the threshold, then marks the compact
// task it queued running and stamped with the epoch it started in (the claim
// does this in production).
func (dm autoCompactDM) startedCompaction(t *testing.T) compactionTask {
	t.Helper()
	dm.complete(t, dm.sendRunning(t, "hello"), 170_000, 200_000)
	tasks := dm.compactionTasks(t)
	if len(tasks) != 1 {
		t.Fatalf("%d compaction tasks, want 1", len(tasks))
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running', started_at = now(), chat_compaction_epoch = 0 WHERE id = $1`, tasks[0].ID)
	return tasks[0]
}

// summarise does what POST /api/chat/compact does to the DM, by SQL: a summary
// row owned by the task and one epoch bump.
func (dm autoCompactDM) summarise(t *testing.T, taskID string) {
	t.Helper()
	dbfx.Insert(t, "chat_message", testutil.Cols{
		"chat_session_id": dm.sessionID, "role": "assistant", "content": "SUMMARY",
		"message_kind": "compaction_summary", "task_id": taskID,
	})
	dbfx.Exec(t, `UPDATE chat_session SET compaction_epoch = compaction_epoch + 1, session_id = NULL WHERE id = $1`, dm.sessionID)
}

func (dm autoCompactDM) failedNotices(t *testing.T) int {
	t.Helper()
	n := 0
	for _, c := range dm.messages(t, "compact_notice") {
		if len(c) >= 17 && c[:17] == "Auto-compact fail" {
			n++
		}
	}
	return n
}

// S1: a compact task that wrote its summary did compact the DM; ending it any
// way afterwards must not claim the compaction failed.
func TestCompactSettle_SummaryOwnedByTheTaskIsNotAFailure(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	for _, end := range []string{"completed", "failed"} {
		t.Run(end, func(t *testing.T) {
			dm := newAutoCompactDM(t, `{}`, `{}`)
			task := dm.startedCompaction(t)
			dm.summarise(t, task.ID)
			if end == "failed" {
				dm.fail(t, task.ID)
			} else {
				dm.complete(t, task.ID, 0, 0)
			}
			if n := dm.failedNotices(t); n != 0 {
				t.Fatalf("%d failure notices after a task that stored its summary", n)
			}
		})
	}
}

// S2 (pin): without a summary row of its own the failure notice stays. A
// summary written by some other task does not count.
func TestCompactSettle_NoSummaryOfItsOwnStillPostsTheFailureNotice(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dm := newAutoCompactDM(t, `{}`, `{}`)
	task := dm.startedCompaction(t)
	other := dbfx.Task(t, dm.agentID, testutil.Cols{
		"runtime_id": dm.runtimeID, "chat_session_id": dm.sessionID, "status": "completed",
		"started_at": testutil.Raw("now()"), "completed_at": testutil.Raw("now()"),
	})
	dbfx.Insert(t, "chat_message", testutil.Cols{
		"chat_session_id": dm.sessionID, "role": "assistant", "content": "SOMEONE ELSE'S",
		"message_kind": "compaction_summary", "task_id": other,
	})
	dm.fail(t, task.ID)
	if n := dm.failedNotices(t); n != 1 {
		t.Fatalf("%d failure notices, want 1", n)
	}
}

// R1: an automatic compaction that succeeded needs no 1.1x wait. The next run
// that reaches the threshold in the new epoch queues a fresh one.
func TestCompactRearm_SucceededCompactionDoesNotHoldTheGuard(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dm := newAutoCompactDM(t, `{}`, `{}`)
	task := dm.startedCompaction(t)
	dm.summarise(t, task.ID)
	dm.complete(t, task.ID, 0, 0)

	next := dm.sendRunning(t, "after compaction")
	dbfx.Exec(t, `UPDATE agent_task_queue SET chat_compaction_epoch = 1 WHERE id = $1`, next)
	// 171000 < 1.1 x 170000: the old rule would still be waiting.
	dm.complete(t, next, 171_000, 200_000)
	if n := len(dm.compactionTasks(t)); n != 2 {
		t.Fatalf("%d compaction tasks, want 2 (the finished one and a new one)", n)
	}
}

// R1 pin: a failed attempt in the SAME epoch keeps the 1.1x guard.
func TestCompactRearm_FailedAttemptInTheSameEpochKeepsTheGuard(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dm := newAutoCompactDM(t, `{}`, `{}`)
	task := dm.startedCompaction(t)
	dm.fail(t, task.ID)
	dm.complete(t, dm.sendRunning(t, "again"), 171_000, 200_000)
	if n := len(dm.compactionTasks(t)); n != 1 {
		t.Fatalf("%d compaction tasks, want 1: a failed attempt must still wait for 1.1x", n)
	}
}

// R2 (CHE-1701): an ordinary run that wrote a summary through the endpoint and
// then completes over the threshold has just compacted the DM. Its own
// completion must not queue another compaction.
func TestCompactRearm_OrdinaryTaskThatWroteASummaryDoesNotTrigger(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dm := newAutoCompactDM(t, `{}`, `{}`)
	run := dm.sendRunning(t, "please compact")
	dbfx.Exec(t, `UPDATE agent_task_queue SET chat_compaction_epoch = 0 WHERE id = $1`, run)
	dm.summarise(t, run)
	dm.complete(t, run, 170_000, 200_000)
	if n := len(dm.compactionTasks(t)); n != 0 {
		t.Fatalf("%d compaction tasks queued by the run that had just compacted", n)
	}
}
