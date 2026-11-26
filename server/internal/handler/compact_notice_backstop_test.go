package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// storeCompactedRow stores a `context_compacted` run message without running the
// notice writer: the state left by an ingestion-time write that failed.
func (f compactNoticeFixture) storeCompactedRow(t *testing.T, seq int, at time.Time) {
	t.Helper()
	dbfx.Exec(t, `INSERT INTO task_message (task_id, seq, type, content, input, created_at)
		VALUES ($1, $2, 'context_compacted', 'Context compacted', '{"tokens_before": 397000, "tokens_after": 31000}'::jsonb, $3)`,
		f.taskID, seq, at)
}

// cancelDeferred settles the task as the cancel path does: cancelled, with the
// finalize marker armed for the ack.
func (f compactNoticeFixture) cancelDeferred(t *testing.T) {
	t.Helper()
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'cancelled', completed_at = now(),
		chat_finalize_deferred_at = now() WHERE id = $1`, f.taskID)
}

func (f compactNoticeFixture) ack(t *testing.T) {
	t.Helper()
	req := newOwnerDaemonRequest(http.MethodPost, "/api/daemon/tasks/"+f.taskID+"/cancel-ack", nil)
	testutil.Call(t, testHandler.AckTaskCancelled, withURLParam(req, "taskId", f.taskID)).Want(http.StatusOK)
}

func (f compactNoticeFixture) complete(t *testing.T) {
	t.Helper()
	req := newOwnerDaemonRequest(http.MethodPost, "/api/daemon/tasks/"+f.taskID+"/complete", map[string]any{"output": "done"})
	testutil.Call(t, testHandler.CompleteTask, withURLParam(req, "taskId", f.taskID)).Want(http.StatusOK)
}

// A cancel settles the task first, the daemon then drains its transcript, and
// the ack finalizes: the late compaction gets exactly one notice, ahead of the
// "Stopped." row that closes the cancelled turn.
func TestCancelLateBatchThenAckLeavesOneNoticeBeforeStopped(t *testing.T) {
	f := newCompactNoticeChat(t, "")
	f.cancelDeferred(t)
	at := time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond)
	f.report(t, compactedEvent(1, "late", 397_000, 31_000, at))
	f.ack(t)

	notice := f.wantNotices(t, 1)[0]
	f.wantPublishes(t, 1)

	var stoppedAt time.Time
	var stoppedID string
	dbfx.QueryRow(t, `SELECT created_at, id::text FROM chat_message
		WHERE task_id = $1 AND role = 'assistant' AND content = 'Stopped.'`, f.taskID).Scan(&stoppedAt, &stoppedID)
	if stoppedID == "" {
		t.Fatal("no Stopped. row: the ack did not finalize the cancelled turn")
	}
	// Order the DM the way the client does: (created_at, id).
	var orderedFirst string
	dbfx.QueryRow(t, `SELECT id::text FROM chat_message
		WHERE chat_session_id = $1 AND (message_kind = 'compact_notice' OR id = $2::uuid)
		ORDER BY created_at, id LIMIT 1`, f.sessionID, stoppedID).Scan(&orderedFirst)
	if orderedFirst != notice.id {
		t.Fatalf("first row = %s, want the notice %s before Stopped. %s (notice at %v, stopped at %v)",
			orderedFirst, notice.id, stoppedID, notice.createdAt, stoppedAt)
	}
}

func TestCancelAckReplayedWritesNoSecondNoticeOrPublish(t *testing.T) {
	f := newCompactNoticeChat(t, "")
	f.cancelDeferred(t)
	f.report(t, compactedEvent(1, "late", 397_000, 31_000, time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond)))
	f.ack(t)
	f.ack(t)

	f.wantNotices(t, 1)
	f.wantPublishes(t, 1)
}

// The ingestion-time write failed (the run message is stored, the notice is
// not): the ack backstop writes and publishes it.
func TestCancelAckRecoversNoticeTheIngestionWriteMissed(t *testing.T) {
	f := newCompactNoticeChat(t, "")
	f.cancelDeferred(t)
	f.storeCompactedRow(t, 1, time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond))
	f.wantNotices(t, 0)

	f.ack(t)

	f.wantNotices(t, 1)
	f.wantPublishes(t, 1)
}

// The runtime and workspace-revoke paths call the same finalizer, so the
// backstop sits inside it rather than in the ack handler.
func TestFinalizeDeferredCancelledChatRecoversMissedNotice(t *testing.T) {
	f := newCompactNoticeChat(t, "")
	f.cancelDeferred(t)
	f.storeCompactedRow(t, 1, time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond))

	testHandler.TaskService.FinalizeDeferredCancelledChat(context.Background(), parseUUID(f.taskID))

	f.wantNotices(t, 1)
	f.wantPublishes(t, 1)
}

// The event was stored (and noticed) before the run completed: completion adds
// nothing.
func TestCompleteAfterIngestedEventLeavesOneNotice(t *testing.T) {
	f := newCompactNoticeChat(t, "")
	f.report(t, compactedEvent(1, "c1", 397_000, 31_000, time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond)))
	f.complete(t)

	f.wantNotices(t, 1)
	f.wantPublishes(t, 1)
}

// Completion backstop: an ingestion-time write that failed is recovered when the
// turn completes.
func TestCompleteRecoversNoticeTheIngestionWriteMissed(t *testing.T) {
	f := newCompactNoticeChat(t, "")
	f.storeCompactedRow(t, 1, time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond))

	f.complete(t)

	f.wantNotices(t, 1)
	f.wantPublishes(t, 1)
}

// Ingestion and finalization race: the session lock and the derived id leave
// one row, and only the inserting call publishes.
func TestIngestionAndFinalizeConcurrentlyWriteOneNotice(t *testing.T) {
	f := newCompactNoticeChat(t, "")
	f.cancelDeferred(t)
	at := time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond)
	f.storeCompactedRow(t, 1, at)

	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		// A second event: ingestion reconciles both rows' notices.
		w := httptest.NewRecorder()
		testHandler.ReportTaskMessages(w, batchMessagesRequest(t, f.taskID, []any{compactedEvent(2, "c2", 0, 0, at)}))
		if w.Code != http.StatusOK {
			t.Errorf("ReportTaskMessages status = %d, want 200: %s", w.Code, w.Body.String())
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		testHandler.TaskService.FinalizeDeferredCancelledChat(context.Background(), parseUUID(f.taskID))
	}()
	close(start)
	wg.Wait()

	f.wantNotices(t, 2)
	f.wantPublishes(t, 2)
}
