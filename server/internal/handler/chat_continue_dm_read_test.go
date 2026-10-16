package handler

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-1793 (N2) review round 3: continuing a DM archives it, and an archived DM
// gets no more notifications, so its unread dm_reply records close with it —
// whoever asked for the continuation.

func unreadRecordsOf(t *testing.T, sessionID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM inbox_item WHERE chat_session_id = $1 AND recipient_id = $2 AND type = 'dm_reply' AND read = false`, sessionID, testUserID)
}

func TestContinueClosesTheSourceDMRecords(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for _, byAgent := range []bool{false, true} {
		name := "creator continue"
		if byAgent {
			name = "agent rollover"
		}
		t.Run(name, func(t *testing.T) {
			resetDMInbox(t)
			ctx := context.Background()
			agentID, sessionID, _, _ := setupDirectChatSession(t, ctx, "close on continue")
			cleanupSuccessor(t, sessionID)
			dmReply(t, agentID, sessionID, "an unread reply")
			if got := unreadRecordsOf(t, sessionID); got != 1 {
				t.Fatalf("setup: unread source records = %d, want 1", got)
			}

			var mu sync.Mutex
			var readEvents int
			testHandler.Bus.Subscribe(protocol.EventInboxRead, func(events.Event) {
				mu.Lock()
				defer mu.Unlock()
				readEvents++
			})

			if byAgent {
				taskID := runningChatTask(t, agentID, sessionID)
				if response := continueAsAgentTask(t, testHandler, sessionID, taskID, agentID); response.Code != http.StatusCreated {
					t.Fatalf("continue: %d %s", response.Code, response.Body.String())
				}
			} else if response := continueChatRequest(t, sessionID, "Goal: keep the context.", ""); response.Code != http.StatusCreated {
				t.Fatalf("continue: %d %s", response.Code, response.Body.String())
			}

			if got := unreadRecordsOf(t, sessionID); got != 0 {
				t.Fatalf("the archived source DM still has %d unread dm_reply records", got)
			}
			mu.Lock()
			defer mu.Unlock()
			if readEvents == 0 {
				t.Fatal("closing the source records published no inbox:read event")
			}
		})
	}
}

func TestMarkingAnOlderDMRecordUnreadKeepsOneUnreadRecord(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	resetDMInbox(t)
	ctx := context.Background()
	agentID, sessionID, _, _ := setupDirectChatSession(t, ctx, "mark unread")
	dmReply(t, agentID, sessionID, "first")
	first := dmRecords(t)
	if len(first) != 1 {
		t.Fatalf("setup: records = %d, want 1", len(first))
	}
	read := withChatTestWorkspaceCtx(t, withURLParam(newRequest(http.MethodPost, "/api/inbox/"+first[0].ID+"/read", nil), "id", first[0].ID))
	testutil.Call(t, testHandler.MarkInboxRead, read).Want(http.StatusOK)
	dmReply(t, agentID, sessionID, "second")
	if got := unreadRecordsOf(t, sessionID); got != 1 {
		t.Fatalf("setup: unread records = %d, want 1", got)
	}

	unread := withChatTestWorkspaceCtx(t, withURLParam(newRequest(http.MethodPost, "/api/inbox/"+first[0].ID+"/unread", nil), "id", first[0].ID))
	response := testutil.Call(t, testHandler.MarkInboxUnread, unread)
	if response.Code == http.StatusInternalServerError {
		t.Fatalf("marking the older record unread must not be a server error: %s", response.Body.String())
	}
	if response.Code != http.StatusConflict {
		t.Fatalf("marking the older record unread: %d, want 409 (a newer unread record already holds the DM)", response.Code)
	}
	if got := unreadRecordsOf(t, sessionID); got != 1 {
		t.Fatalf("the DM has %d unread records, want exactly 1", got)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM inbox_item WHERE id = $1 AND read = true`, first[0].ID); got != 1 {
		t.Fatal("the older record must stay read")
	}
}
