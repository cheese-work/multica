package handler

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// compactNoticePublishes counts `chat:message` events of kind compact_notice per
// chat session. The bus has no unsubscribe, so one subscriber serves the file.
var (
	compactNoticePublishMu   sync.Mutex
	compactNoticePublishes   = map[string]int{}
	compactNoticeSubscribeMu sync.Once
)

func countCompactNoticePublishes(t *testing.T, sessionID string) int {
	t.Helper()
	compactNoticeSubscribeMu.Do(func() {
		testHandler.Bus.Subscribe(protocol.EventChatMessage, func(e events.Event) {
			p, ok := e.Payload.(protocol.ChatMessagePayload)
			if !ok || p.MessageKind != protocol.ChatMessageKindCompactNotice {
				return
			}
			compactNoticePublishMu.Lock()
			compactNoticePublishes[p.ChatSessionID]++
			compactNoticePublishMu.Unlock()
		})
	})
	compactNoticePublishMu.Lock()
	defer compactNoticePublishMu.Unlock()
	return compactNoticePublishes[sessionID]
}

type compactNoticeFixture struct {
	agentID, sessionID, taskID string
}

// newCompactNoticeChat seeds a DM and a running chat task of kind taskKind ("" for a
// normal turn). The subscriber is registered before any write.
func newCompactNoticeChat(t *testing.T, taskKind string) compactNoticeFixture {
	t.Helper()
	if testHandler == nil {
		t.Skip("database not available")
	}
	runtimeID := handlerTestRuntimeID(t)
	agentID := dbfx.Agent(t, "compact notice agent", runtimeID, testutil.Cols{
		"instructions": "",
		"custom_env":   testutil.Raw("'{}'::jsonb"),
		"custom_args":  testutil.Raw("'[]'::jsonb"),
	})
	sessionID := dbfx.ChatSession(t, agentID)
	cols := testutil.Cols{
		"runtime_id":      runtimeID,
		"chat_session_id": sessionID,
		"status":          "running",
		"started_at":      testutil.Raw("now()"),
	}
	if taskKind != "" {
		cols["task_kind"] = taskKind
	}
	taskID := dbfx.Task(t, agentID, cols)
	dbfx.Exec(t, `UPDATE agent_task_queue SET chat_input_task_id = id WHERE id = $1`, taskID)
	countCompactNoticePublishes(t, sessionID) // registers the subscriber
	return compactNoticeFixture{agentID: agentID, sessionID: sessionID, taskID: taskID}
}

func compactedEvent(seq int, key string, before, after int64, at time.Time) map[string]any {
	input := map[string]any{"trigger": "auto"}
	if before > 0 {
		input["tokens_before"] = before
	}
	if after > 0 {
		input["tokens_after"] = after
	}
	return map[string]any{
		"seq": seq, "type": "context_compacted", "content": "Context compacted",
		"input": input, "idempotency_key": key, "created_at": at.UTC().Format(time.RFC3339Nano),
	}
}

func (f compactNoticeFixture) report(t *testing.T, messages ...any) {
	t.Helper()
	testutil.Call(t, testHandler.ReportTaskMessages, batchMessagesRequest(t, f.taskID, messages)).Want(http.StatusOK)
}

type noticeRow struct {
	id, content string
	createdAt   time.Time
	taskID      *string
}

func (f compactNoticeFixture) notices(t *testing.T) []noticeRow {
	t.Helper()
	rows, err := dbfx.Pool.Query(context.Background(),
		`SELECT id::text, content, created_at, task_id::text FROM chat_message
		 WHERE chat_session_id = $1 AND message_kind = 'compact_notice' ORDER BY created_at, id`, f.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []noticeRow
	for rows.Next() {
		var r noticeRow
		if err := rows.Scan(&r.id, &r.content, &r.createdAt, &r.taskID); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (f compactNoticeFixture) wantNotices(t *testing.T, n int) []noticeRow {
	t.Helper()
	got := f.notices(t)
	if len(got) != n {
		t.Fatalf("compact notices = %d (%+v), want %d", len(got), got, n)
	}
	return got
}

func (f compactNoticeFixture) wantPublishes(t *testing.T, n int) {
	t.Helper()
	if got := countCompactNoticePublishes(t, f.sessionID); got != n {
		t.Fatalf("chat:message compact_notice publishes = %d, want %d", got, n)
	}
}

func TestReportTaskMessagesWritesCompactNoticeForRunningChatTask(t *testing.T) {
	f := newCompactNoticeChat(t, "")
	at := time.Now().UTC().Add(time.Second).Truncate(time.Microsecond)
	f.report(t, compactedEvent(1, "c1", 397_000, 31_000, at))

	got := f.wantNotices(t, 1)
	if want := "Compacted mid-turn at 397k tokens (kept 31k). Earlier details may be lost."; got[0].content != want {
		t.Fatalf("content = %q, want %q", got[0].content, want)
	}
	if !got[0].createdAt.Equal(at) {
		t.Fatalf("created_at = %v, want the run message time %v", got[0].createdAt, at)
	}
	f.wantPublishes(t, 1)
}

func TestReportTaskMessagesWritesOneNoticePerCompactionInARun(t *testing.T) {
	f := newCompactNoticeChat(t, "")
	base := time.Now().UTC().Add(time.Second).Truncate(time.Microsecond)
	f.report(t, compactedEvent(1, "c1", 397_000, 31_000, base))
	f.report(t, compactedEvent(2, "c2", 390_000, 30_000, base.Add(time.Minute)))

	got := f.wantNotices(t, 2)
	if got[0].id == got[1].id {
		t.Fatalf("both notices share id %s", got[0].id)
	}
	f.wantPublishes(t, 2)
}

func TestReportTaskMessagesReplayedBatchWritesOneNotice(t *testing.T) {
	f := newCompactNoticeChat(t, "")
	at := time.Now().UTC().Add(time.Second).Truncate(time.Microsecond)
	batch := compactedEvent(1, "c1", 397_000, 31_000, at)
	f.report(t, batch)
	f.report(t, batch)

	f.wantNotices(t, 1)
	f.wantPublishes(t, 1)
}

func TestReportTaskMessagesNoticeForBatchAfterCancelSettled(t *testing.T) {
	f := newCompactNoticeChat(t, "")
	// The cancel settled the task before the daemon drained its transcript.
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'cancelled', completed_at = now() WHERE id = $1`, f.taskID)
	at := time.Now().UTC().Add(time.Second).Truncate(time.Microsecond)
	f.report(t, compactedEvent(1, "late", 397_000, 31_000, at))

	f.wantNotices(t, 1)
	f.wantPublishes(t, 1)
}

func TestReconcileCompactNoticesConcurrentCallsWriteOneRowAndPublishOnce(t *testing.T) {
	f := newCompactNoticeChat(t, "")
	at := time.Now().UTC().Add(time.Second).Truncate(time.Microsecond)
	// Store the run message without the notice writer: insert it directly.
	dbfx.Exec(t, `INSERT INTO task_message (task_id, seq, type, content, input, created_at)
		VALUES ($1, 1, 'context_compacted', 'Context compacted', '{"tokens_before": 397000, "tokens_after": 31000}'::jsonb, $2)`, f.taskID, at)

	const callers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := testHandler.TaskService.ReconcileCompactNotices(context.Background(), parseUUID(f.taskID))
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	f.wantNotices(t, 1)
	f.wantPublishes(t, 1)
}

func TestReportTaskMessagesNoNoticeOutsideWebChatTurns(t *testing.T) {
	at := time.Now().UTC().Add(time.Second).Truncate(time.Microsecond)

	t.Run("compact task", func(t *testing.T) {
		f := newCompactNoticeChat(t, "compact")
		f.report(t, compactedEvent(1, "c1", 397_000, 31_000, at))
		f.wantNotices(t, 0)
		f.wantPublishes(t, 0)
	})

	t.Run("channel-bound DM", func(t *testing.T) {
		f := newCompactNoticeChat(t, "")
		seedChatSourceBinding(t, f.agentID, f.sessionID)
		f.report(t, compactedEvent(1, "c1", 397_000, 31_000, at))
		f.wantNotices(t, 0)
		f.wantPublishes(t, 0)
	})

	t.Run("issue task", func(t *testing.T) {
		if testHandler == nil {
			t.Skip("database not available")
		}
		runtimeID := handlerTestRuntimeID(t)
		agentID := dbfx.Agent(t, "compact notice issue agent", runtimeID, testutil.Cols{
			"instructions": "",
			"custom_env":   testutil.Raw("'{}'::jsonb"),
			"custom_args":  testutil.Raw("'[]'::jsonb"),
		})
		issueID := dbfx.Issue(t, "compact notice issue")
		taskID := dbfx.Task(t, agentID, testutil.Cols{
			"runtime_id": runtimeID, "issue_id": issueID,
			"status": "running", "started_at": testutil.Raw("now()"),
		})
		testutil.Call(t, testHandler.ReportTaskMessages, batchMessagesRequest(t, taskID, []any{
			compactedEvent(1, "c1", 397_000, 31_000, at),
		})).Want(http.StatusOK)
		if n := dbfx.Count(t, `SELECT count(*) FROM chat_message WHERE message_kind = 'compact_notice' AND created_at = $1`, at); n != 0 {
			t.Fatalf("issue task wrote %d compact notices", n)
		}
	})
}

func TestReportTaskMessagesNoticeWithoutTokenCountsUsesShortText(t *testing.T) {
	f := newCompactNoticeChat(t, "")
	at := time.Now().UTC().Add(time.Second).Truncate(time.Microsecond)
	f.report(t, compactedEvent(1, "c1", 0, 0, at))

	got := f.wantNotices(t, 1)
	if want := "Compacted mid-turn. Earlier details may be lost."; got[0].content != want {
		t.Fatalf("content = %q, want %q", got[0].content, want)
	}
}

// Rule W: a notice whose run message predates the DM's segment floor lands
// just above the floor, in the new segment, never below it.
func TestReportTaskMessagesNoticeStaysAboveSegmentFloor(t *testing.T) {
	f := newCompactNoticeChat(t, "")
	floor := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	dbfx.Exec(t, `UPDATE chat_session SET segment_floor_at = $2 WHERE id = $1`, f.sessionID, floor)
	f.report(t, compactedEvent(1, "c1", 397_000, 31_000, time.Now().UTC()))

	got := f.wantNotices(t, 1)
	if !got[0].createdAt.After(floor) {
		t.Fatalf("created_at = %v, want above the segment floor %v", got[0].createdAt, floor)
	}
}
