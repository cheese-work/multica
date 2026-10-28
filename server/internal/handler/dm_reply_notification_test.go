package handler

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-1793 (N2): one coalesced inbox record per (recipient, DM). These tests
// drive the real completion/failure paths and read the result back through the
// inbox endpoints, so they only name symbols that already exist.

type dmInboxRow struct {
	ID            string         `json:"id"`
	Type          string         `json:"type"`
	Read          bool           `json:"read"`
	IssueID       *string        `json:"issue_id"`
	ChatSessionID *string        `json:"chat_session_id"`
	UpdatedAt     string         `json:"updated_at"`
	Body          *string        `json:"body"`
	Title         string         `json:"title"`
	Details       map[string]any `json:"details"`
}

// resetDMInbox starts from an opted-in user: DM switches are off by default
// (CHE-1848), and these tests cover the opted-in behavior of CHE-1793.
func resetDMInbox(t *testing.T) {
	t.Helper()
	clearDMInbox(t)
	setDMChannel(t, "in_app", true)
}

// clearDMInbox leaves the user with no DM switch rows, i.e. the defaults.
func clearDMInbox(t *testing.T) {
	t.Helper()
	clean := func() {
		testPool.Exec(context.Background(), `DELETE FROM inbox_item WHERE workspace_id = $1 AND recipient_id = $2 AND type IN ('dm_reply', 'mentioned')`, testWorkspaceID, testUserID)
		testPool.Exec(context.Background(), `DELETE FROM notification_channel_preference WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, testUserID)
	}
	clean()
	t.Cleanup(clean)
}

func listDMInbox(t *testing.T) []dmInboxRow {
	t.Helper()
	var rows []dmInboxRow
	testutil.Call(t, testHandler.ListInbox, withChatTestWorkspaceCtx(t, newRequest(http.MethodGet, "/api/inbox", nil))).Want(http.StatusOK).JSON(&rows)
	return rows
}

func unreadDMInbox(t *testing.T) int64 {
	t.Helper()
	var out map[string]int64
	testutil.Call(t, testHandler.CountUnreadInbox, withChatTestWorkspaceCtx(t, newRequest(http.MethodGet, "/api/inbox/unread-count", nil))).Want(http.StatusOK).JSON(&out)
	return out["count"]
}

func dmRecords(t *testing.T) []dmInboxRow {
	t.Helper()
	var out []dmInboxRow
	for _, row := range listDMInbox(t) {
		if row.Type == "dm_reply" {
			out = append(out, row)
		}
	}
	return out
}

func dmRecordCountInDB(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM inbox_item WHERE workspace_id = $1 AND recipient_id = $2 AND type = 'dm_reply'`, testWorkspaceID, testUserID)
}

// dmReply drives one full turn: the member sends, the agent's run completes.
func dmReply(t *testing.T, agentID, sessionID, output string) {
	t.Helper()
	ctx := context.Background()
	taskID := sendDirectChat(t, ctx, agentID, sessionID, "ping")
	markTaskRunning(t, ctx, taskID)
	if _, err := testHandler.TaskService.CompleteTask(ctx, parseUUID(taskID), completeResult(t, output), "", "", "", false, "", ""); err != nil {
		t.Fatalf("complete task: %v", err)
	}
}

// controlDM adds a second ordinary DM on the same agent and proves a reply in
// it notifies, so a "no record" assertion on another DM cannot pass merely
// because nothing notifies at all.
func controlDM(t *testing.T, agentID string) {
	t.Helper()
	var sessionID string
	dbfx.QueryRow(t, `INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, explicitly_created_at) VALUES ($1, $2, $3, 'control', now()) RETURNING id`,
		testWorkspaceID, agentID, testUserID).Scan(&sessionID)
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, sessionID) })
	dmReply(t, agentID, sessionID, "control reply")
}

func setDMChannel(t *testing.T, channel string, enabled bool) {
	t.Helper()
	dbfx.Exec(t, `INSERT INTO notification_channel_preference (workspace_id, user_id, source, channel, enabled) VALUES ($1, $2, 'dm', $3, $4)
		ON CONFLICT (workspace_id, user_id, source, channel) DO UPDATE SET enabled = EXCLUDED.enabled`,
		testWorkspaceID, testUserID, channel, enabled)
}

func TestDMReplyTenRepliesCoalesceIntoOneUnreadRecord(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	resetDMInbox(t)
	agentID, sessionID, _, _ := setupDirectChatSession(t, context.Background(), "coalesce")

	for i := 0; i < 10; i++ {
		dmReply(t, agentID, sessionID, "reply")
	}

	records := dmRecords(t)
	if len(records) != 1 {
		t.Fatalf("dm_reply records = %d, want 1: %+v", len(records), records)
	}
	record := records[0]
	if record.Read || record.IssueID != nil || record.ChatSessionID == nil || *record.ChatSessionID != sessionID {
		t.Fatalf("record must be unread, issue-less and linked to the DM: %+v", record)
	}
	if record.Details["reply_count"] != float64(10) {
		t.Fatalf("reply_count = %v, want 10", record.Details["reply_count"])
	}
	if record.UpdatedAt == "" {
		t.Fatal("record carries no updated_at")
	}
	if record.Body != nil {
		t.Fatalf("record body must not carry message text: %q", *record.Body)
	}
	if got := unreadDMInbox(t); got != 1 {
		t.Fatalf("unread count = %d, want 1", got)
	}
}

func TestDMReplyReadingTheDMMarksTheRecordRead(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	resetDMInbox(t)
	agentID, sessionID, _, _ := setupDirectChatSession(t, context.Background(), "read sync")
	dmReply(t, agentID, sessionID, "first")

	readRequest := withChatTestWorkspaceCtx(t, withURLParam(newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/read", nil), "sessionId", sessionID))
	testutil.Call(t, testHandler.MarkChatSessionRead, readRequest).Want(http.StatusNoContent)

	records := dmRecords(t)
	if len(records) != 1 || !records[0].Read {
		t.Fatalf("reading the DM must mark its record read: %+v", records)
	}
	if got := unreadDMInbox(t); got != 0 {
		t.Fatalf("unread count = %d, want 0", got)
	}

	dmReply(t, agentID, sessionID, "second")
	records = dmRecords(t)
	if len(records) != 2 {
		t.Fatalf("a reply after reading must start a new record: %+v", records)
	}
	unread := 0
	for _, record := range records {
		if !record.Read {
			unread++
			if record.Details["reply_count"] != float64(1) {
				t.Fatalf("new record reply_count = %v, want 1", record.Details["reply_count"])
			}
		}
	}
	if unread != 1 {
		t.Fatalf("unread records = %d, want 1: %+v", unread, records)
	}
}

func TestDMReplyMarkingTheRecordReadAdvancesTheDMCursor(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	resetDMInbox(t)
	agentID, sessionID, _, _ := setupDirectChatSession(t, context.Background(), "cursor sync")
	dmReply(t, agentID, sessionID, "hello")
	records := dmRecords(t)
	if len(records) != 1 {
		t.Fatalf("dm_reply records = %d, want 1", len(records))
	}

	markRequest := withChatTestWorkspaceCtx(t, withURLParam(newRequest(http.MethodPost, "/api/inbox/"+records[0].ID+"/read", nil), "id", records[0].ID))
	testutil.Call(t, testHandler.MarkInboxRead, markRequest).Want(http.StatusOK)

	if unread := dbfx.Count(t, `
		SELECT count(*) FROM chat_message m JOIN chat_session cs ON cs.id = m.chat_session_id
		WHERE cs.id = $1 AND m.role = 'assistant' AND m.created_at > cs.last_read_at`, sessionID); unread != 0 {
		t.Fatalf("DM still has %d unread assistant messages after its record was read", unread)
	}
}

func TestDMReplyFailedRunNotifies(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	resetDMInbox(t)
	ctx := context.Background()
	agentID, sessionID, _, _ := setupDirectChatSession(t, ctx, "failure")
	taskID := sendDirectChat(t, ctx, agentID, sessionID, "do it")
	markTaskRunning(t, ctx, taskID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET max_attempts = 1 WHERE id = $1`, taskID)
	if _, err := testHandler.TaskService.FailTask(ctx, parseUUID(taskID), "boom", "", "", "", "agent_error", false, "", ""); err != nil {
		t.Fatalf("fail task: %v", err)
	}

	records := dmRecords(t)
	if len(records) != 1 || records[0].ChatSessionID == nil || *records[0].ChatSessionID != sessionID {
		t.Fatalf("a failed run's error reply must notify: %+v", records)
	}
}

func TestDMReplyNoRecordForArchivedOrChannelBoundDM(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	t.Run("archived", func(t *testing.T) {
		resetDMInbox(t)
		agentID, sessionID, _, _ := setupDirectChatSession(t, ctx, "archived")
		taskID := sendDirectChat(t, ctx, agentID, sessionID, "ping")
		markTaskRunning(t, ctx, taskID)
		dbfx.Exec(t, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, sessionID)
		if _, err := testHandler.TaskService.CompleteTask(ctx, parseUUID(taskID), completeResult(t, "late"), "", "", "", false, "", ""); err != nil {
			t.Fatalf("complete task: %v", err)
		}
		controlDM(t, agentID)
		if n := dmRecordCountInDB(t); n != 1 {
			t.Fatalf("records = %d, want only the control DM's record (archived DM must add none)", n)
		}
	})

	t.Run("channel-bound", func(t *testing.T) {
		resetDMInbox(t)
		agentID, sessionID, _, _ := setupDirectChatSession(t, ctx, "bound")
		dbfx.Exec(t, `INSERT INTO channel_chat_session_binding (chat_session_id, installation_id, channel_type, channel_chat_id, chat_type) VALUES ($1, $2, 'slack', $3, 'p2p')`,
			sessionID, uuid.NewString(), uuid.NewString())
		t.Cleanup(func() {
			testPool.Exec(ctx, `DELETE FROM channel_chat_session_binding WHERE chat_session_id = $1`, sessionID)
		})
		dmReply(t, agentID, sessionID, "bound reply")
		controlDM(t, agentID)
		if n := dmRecordCountInDB(t); n != 1 {
			t.Fatalf("records = %d, want only the control DM's record (channel-bound DM must add none)", n)
		}
	})
}

func TestDMReplyHonoursChannelSwitches(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	t.Run("both off creates nothing", func(t *testing.T) {
		resetDMInbox(t)
		setDMChannel(t, "in_app", false)
		setDMChannel(t, "push", false)
		agentID, sessionID, _, _ := setupDirectChatSession(t, ctx, "switches off")
		dmReply(t, agentID, sessionID, "quiet")
		if n := dmRecordCountInDB(t); n != 0 {
			t.Fatalf("records = %d, want 0", n)
		}
		setDMChannel(t, "in_app", true)
		controlDM(t, agentID)
		if n := dmRecordCountInDB(t); n != 1 {
			t.Fatalf("with the switches restored a reply must notify, records = %d", n)
		}
	})

	t.Run("in-app off, push on keeps a hidden record", func(t *testing.T) {
		resetDMInbox(t)
		setDMChannel(t, "in_app", false)
		setDMChannel(t, "push", true)
		agentID, sessionID, _, _ := setupDirectChatSession(t, ctx, "push only")
		dmReply(t, agentID, sessionID, "push me")
		if n := dmRecordCountInDB(t); n != 1 {
			t.Fatalf("records = %d, want 1 (kept for push read-sync)", n)
		}
		if records := dmRecords(t); len(records) != 0 {
			t.Fatalf("inbox list must hide the push-only record: %+v", records)
		}
		if got := unreadDMInbox(t); got != 0 {
			t.Fatalf("unread count = %d, want 0", got)
		}
	})
}

// CHE-1848: with no DM switch row a reply creates no record, and a record made
// while opted in leaves the inbox once the user is back on the defaults.
func TestDMReplyOffByDefault(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	t.Run("no switch row creates nothing", func(t *testing.T) {
		clearDMInbox(t)
		agentID, sessionID, _, _ := setupDirectChatSession(t, ctx, "default off")
		dmReply(t, agentID, sessionID, "unasked")
		if n := dmRecordCountInDB(t); n != 0 {
			t.Fatalf("records = %d, want 0 without an opt-in", n)
		}
		if records := dmRecords(t); len(records) != 0 {
			t.Fatalf("inbox list must show no dm_reply rows: %+v", records)
		}
		if got := unreadDMInbox(t); got != 0 {
			t.Fatalf("unread count = %d, want 0", got)
		}
		setDMChannel(t, "in_app", true)
		controlDM(t, agentID)
		if n := dmRecordCountInDB(t); n != 1 {
			t.Fatalf("after opting in a reply must notify, records = %d", n)
		}
	})

	t.Run("existing record hides on the defaults", func(t *testing.T) {
		resetDMInbox(t)
		agentID, sessionID, _, _ := setupDirectChatSession(t, ctx, "legacy row")
		dmReply(t, agentID, sessionID, "before the change")
		if records := dmRecords(t); len(records) != 1 {
			t.Fatalf("opted-in reply must show one record: %+v", records)
		}
		testPool.Exec(ctx, `DELETE FROM notification_channel_preference WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, testUserID)
		if records := dmRecords(t); len(records) != 0 {
			t.Fatalf("inbox list must hide the record on the defaults: %+v", records)
		}
		if got := unreadDMInbox(t); got != 0 {
			t.Fatalf("unread count = %d, want 0", got)
		}
		if n := dmRecordCountInDB(t); n != 1 {
			t.Fatalf("records = %d, want the row kept (no data change)", n)
		}
	})
}

func TestDMReplyArchiveAndDeleteMarkRecordsRead(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	unreadRecords := func() int {
		return dbfx.Count(t, `SELECT count(*) FROM inbox_item WHERE workspace_id = $1 AND recipient_id = $2 AND type = 'dm_reply' AND read = false`, testWorkspaceID, testUserID)
	}

	t.Run("archive", func(t *testing.T) {
		resetDMInbox(t)
		agentID, sessionID, _, _ := setupDirectChatSession(t, ctx, "archive me")
		dmReply(t, agentID, sessionID, "hi")
		if unreadRecords() != 1 {
			t.Fatalf("setup: want one unread record")
		}
		archive := withChatTestWorkspaceCtx(t, withURLParam(newRequest(http.MethodPatch, "/api/chat/sessions/"+sessionID+"/archive", map[string]any{"archived": true}), "sessionId", sessionID))
		testutil.Call(t, testHandler.SetChatSessionArchived, archive).Want(http.StatusOK)
		if n := unreadRecords(); n != 0 {
			t.Fatalf("archiving left %d unread records", n)
		}
	})

	t.Run("delete", func(t *testing.T) {
		resetDMInbox(t)
		agentID, sessionID, _, _ := setupDirectChatSession(t, ctx, "delete me")
		dmReply(t, agentID, sessionID, "hi")
		if unreadRecords() != 1 {
			t.Fatalf("setup: want one unread record")
		}
		del := withChatTestWorkspaceCtx(t, withURLParam(newRequest(http.MethodDelete, "/api/chat/sessions/"+sessionID, nil), "sessionId", sessionID))
		testutil.Call(t, testHandler.DeleteChatSession, del).Want(http.StatusNoContent)
		if n := unreadRecords(); n != 0 {
			t.Fatalf("deleting left %d unread records", n)
		}
	})
}

func TestDMReplyInboxHidesRecordsOfUnreadableDMs(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	resetDMInbox(t)
	agentID, sessionID, _, _ := setupDirectChatSession(t, context.Background(), "unreadable")
	dmReply(t, agentID, sessionID, "secret")
	if len(dmRecords(t)) != 1 || unreadDMInbox(t) != 1 {
		t.Fatalf("setup: record must be visible first")
	}

	other := dbfx.User(t, "Other creator", "dm-other-"+uuid.NewString()+"@test.invalid")
	dbfx.Insert(t, "member", testutil.Cols{"workspace_id": testWorkspaceID, "user_id": other, "role": "member"})
	dbfx.Exec(t, `UPDATE chat_session SET creator_id = $1 WHERE id = $2`, other, sessionID)

	if records := dmRecords(t); len(records) != 0 {
		t.Fatalf("a DM the user no longer owns must disappear from the inbox: %+v", records)
	}
	if got := unreadDMInbox(t); got != 0 {
		t.Fatalf("unread count = %d, want 0", got)
	}
}

func TestDMReplyDecisionRequestMakesOneRecordLinkedToTheDM(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fixture := newRequestCoreFixture(t, false)
	dbfx.Cleanup(t, `DELETE FROM inbox_item WHERE issue_id = $1`, fixture.issueID)
	dbfx.Cleanup(t, `DELETE FROM inbox_item WHERE recipient_id = $1`, fixture.humanID)
	testutil.Call(t, testHandler.RequestDecision, fixture.apiRequest()).Want(http.StatusCreated)

	request := chatPendingCtxAs(t, newRequest(http.MethodGet, "/api/inbox", nil), fixture.humanID)
	request.Header.Set("X-User-ID", fixture.humanID)
	var rows []dmInboxRow
	testutil.Call(t, testHandler.ListInbox, request).Want(http.StatusOK).JSON(&rows)
	if len(rows) != 1 {
		t.Fatalf("decision request must produce exactly one record, got %d: %+v", len(rows), rows)
	}
	if rows[0].ChatSessionID == nil || *rows[0].ChatSessionID != fixture.sessionID {
		t.Fatalf("decision record must link the DM %s: %+v", fixture.sessionID, rows[0])
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM inbox_item WHERE recipient_id = $1 AND type = 'dm_reply'`, fixture.humanID); n != 0 {
		t.Fatalf("decision messages must not also create a dm_reply record, got %d", n)
	}
}

// Retrospective tests: added after the implementation, for behavior the
// tests-only commit above did not pin.

func TestDMReplyMarkAllInboxReadAdvancesTheDMCursor(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	resetDMInbox(t)
	agentID, sessionID, _, _ := setupDirectChatSession(t, context.Background(), "mark all")
	dmReply(t, agentID, sessionID, "hello")

	testutil.Call(t, testHandler.MarkAllInboxRead, withChatTestWorkspaceCtx(t, newRequest(http.MethodPost, "/api/inbox/mark-all-read", nil))).Want(http.StatusOK)

	if unread := dbfx.Count(t, `
		SELECT count(*) FROM chat_message m JOIN chat_session cs ON cs.id = m.chat_session_id
		WHERE cs.id = $1 AND m.role = 'assistant' AND m.created_at > cs.last_read_at`, sessionID); unread != 0 {
		t.Fatalf("DM still has %d unread assistant messages after mark-all-read", unread)
	}
	if got := unreadDMInbox(t); got != 0 {
		t.Fatalf("unread count = %d, want 0", got)
	}
}

func TestDMReplyAnsweringTheDecisionReadsItsRecord(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fixture := newAskedRecordFixture(t)
	dbfx.Cleanup(t, `DELETE FROM inbox_item WHERE recipient_id = $1`, fixture.humanID)
	if item, err := decisiontx.NotifyPing(context.Background(), testPool, testHandler.Queries, fixture.decision.WorkspaceID, fixture.decision.ID); err != nil || item == nil {
		t.Fatalf("ping: item=%v err=%v", item, err)
	}
	unread := func() int {
		return dbfx.Count(t, `SELECT count(*) FROM inbox_item WHERE recipient_id = $1 AND type = 'mentioned' AND read = false`, fixture.humanID)
	}
	if unread() != 1 {
		t.Fatalf("setup: the ping must be unread")
	}
	if _, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput()); err != nil {
		t.Fatal(err)
	}
	if unread() != 0 {
		t.Fatal("answering the decision must read its ping record")
	}
}

func TestDMReplyUpdatePublishesInboxUpdated(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	resetDMInbox(t)
	// The bus has no unsubscribe, so the handlers outlive this test and later
	// notice goroutines from other tests publish into them: guard the slice and
	// stop recording once the test is over.
	var mu sync.Mutex
	var types []string
	recording := true
	t.Cleanup(func() {
		mu.Lock()
		recording = false
		mu.Unlock()
	})
	record := func(e events.Event) {
		mu.Lock()
		defer mu.Unlock()
		if recording {
			types = append(types, e.Type)
		}
	}
	testHandler.Bus.Subscribe(protocol.EventInboxNew, record)
	testHandler.Bus.Subscribe(protocol.EventInboxUpdated, record)
	agentID, sessionID, _, _ := setupDirectChatSession(t, context.Background(), "events")
	dmReply(t, agentID, sessionID, "one")
	dmReply(t, agentID, sessionID, "two")
	mu.Lock()
	got := append([]string(nil), types...)
	mu.Unlock()
	if len(got) != 2 || got[0] != protocol.EventInboxNew || got[1] != protocol.EventInboxUpdated {
		t.Fatalf("events = %v, want [inbox:new inbox:updated]", got)
	}
}
