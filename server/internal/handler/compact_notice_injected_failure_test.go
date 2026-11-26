package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

// failNoticeWrites makes the DB reject every `compact_notice` insert for the
// fixture's DM, the way a failed ingestion-time write looks to the handler. The
// returned func lifts the failure (the DB "recovers"). The trigger is scoped to
// this session id, so no other test is affected.
func (f compactNoticeFixture) failNoticeWrites(t *testing.T) (lift func()) {
	t.Helper()
	name := "cn_fail_" + strings.ReplaceAll(f.sessionID[:8], "-", "")
	dbfx.Exec(t, fmt.Sprintf(`CREATE OR REPLACE FUNCTION %[1]s() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'injected compact notice write failure'; END $$ LANGUAGE plpgsql`, name))
	dbfx.Exec(t, fmt.Sprintf(`CREATE TRIGGER %[1]s BEFORE INSERT ON chat_message
		FOR EACH ROW WHEN (NEW.message_kind = 'compact_notice' AND NEW.chat_session_id = '%[2]s'::uuid)
		EXECUTE FUNCTION %[1]s()`, name, f.sessionID))
	lifted := false
	lift = func() {
		if lifted {
			return
		}
		lifted = true
		testPool.Exec(t.Context(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %[1]s ON chat_message`, name))
		testPool.Exec(t.Context(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %[1]s()`, name))
	}
	t.Cleanup(lift)
	return lift
}

func (f compactNoticeFixture) wantRunMessages(t *testing.T, n int) {
	t.Helper()
	var got int
	dbfx.QueryRow(t, `SELECT count(*) FROM task_message WHERE task_id = $1 AND type = 'context_compacted'`, f.taskID).Scan(&got)
	if got != n {
		t.Fatalf("context_compacted run messages = %d, want %d", got, n)
	}
}

// newCancellableCompactNoticeChat seeds a started chat task, empty transcript,
// in a DM that testUserID created, so the real cancel handler can run on it.
func newCancellableCompactNoticeChat(t *testing.T) compactNoticeFixture {
	t.Helper()
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "CompactNoticeCancelAgent", []byte("[]"))
	sessionID := createHandlerTestChatSession(t, agentID)
	taskID, _ := createStartedEmptyChatTask(t, sessionID, agentID, "compact me")
	dbfx.Exec(t, `UPDATE agent_task_queue SET chat_input_task_id = id WHERE id = $1`, taskID)
	countCompactNoticePublishes(t, sessionID) // registers the subscriber
	return compactNoticeFixture{agentID: agentID, sessionID: sessionID, taskID: taskID}
}

// cancelByUser is the real cancel endpoint as a draft-restore-capable client
// calls it: the empty transcript defers finalization to the daemon's ack.
func (f compactNoticeFixture) cancelByUser(t *testing.T) {
	t.Helper()
	req := cancelTaskByUserRequest(t, testUserID, f.taskID)
	req.Header.Set("X-Client-Capabilities", protocol.AppCapabilityChatDraftRestoreV1)
	w := httptest.NewRecorder()
	testHandler.CancelTaskByUser(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("CancelTaskByUser = %d, want 200: %s", w.Code, w.Body.String())
	}
	if chatFinalizeDeferredAt(t, f.taskID) == nil {
		t.Fatal("cancel did not defer chat finalization; the ack path is not under test")
	}
}

// Whole flow through the real handlers: user cancel (deferred), the daemon's
// late batch with a compaction, the cancel ack. One notice, published once.
func TestRealCancelLateBatchThenAckLeavesOneNotice(t *testing.T) {
	f := newCancellableCompactNoticeChat(t)
	f.cancelByUser(t)
	f.report(t, compactedEvent(1, "late", 397_000, 31_000, time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond)))
	f.wantNotices(t, 1) // ingestion already wrote it; the ack must add nothing

	f.ack(t)
	f.ack(t)

	f.wantNotices(t, 1)
	f.wantPublishes(t, 1)
}

// A write that keeps failing through the ack is the plan's stated residual: the
// ack still answers 200, the run message is kept, and no notice or publish
// appears. This pins that the failure never fails the request.
func TestRealCancelAckWithWriteStillFailingAnswersOKAndWritesNothing(t *testing.T) {
	f := newCancellableCompactNoticeChat(t)
	f.cancelByUser(t)
	f.failNoticeWrites(t)

	f.report(t, compactedEvent(1, "late", 397_000, 31_000, time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond)))
	f.ack(t)

	f.wantRunMessages(t, 1)
	f.wantNotices(t, 0)
	f.wantPublishes(t, 0)
}

// Acceptance case: the ingestion-time notice write fails (injected in the DB;
// the handler logs and still answers 200), the run message is stored, and the
// real cancel ack recovers the notice once the DB takes the write again.
func TestRealCancelAckRecoversNoticeWhenWriteHealsBeforeAck(t *testing.T) {
	f := newCancellableCompactNoticeChat(t)
	f.cancelByUser(t)
	lift := f.failNoticeWrites(t)

	f.report(t, compactedEvent(1, "late", 397_000, 31_000, time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond)))
	f.wantRunMessages(t, 1)
	f.wantNotices(t, 0)
	f.wantPublishes(t, 0)

	lift()
	f.ack(t)

	f.wantNotices(t, 1)
	f.wantPublishes(t, 1)
}

// Completion backstop with a real injected failure on a running turn.
func TestCompleteRecoversNoticeAfterInjectedIngestionWriteFailure(t *testing.T) {
	f := newCompactNoticeChat(t, "")
	lift := f.failNoticeWrites(t)

	f.report(t, compactedEvent(1, "c1", 397_000, 31_000, time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond)))
	f.wantRunMessages(t, 1)
	f.wantNotices(t, 0)

	lift()
	f.complete(t)

	f.wantNotices(t, 1)
	f.wantPublishes(t, 1)
}
