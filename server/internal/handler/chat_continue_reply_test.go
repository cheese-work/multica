package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-2119: the agent calls `continue` before its run's final reply is saved.
// That reply is the last thing the person reads, so it belongs in the new DM:
// first visible message there, with the notification, and not only in the
// archived one. The summary stays agent-only context.

func rolloverRunning(t *testing.T, label string) (sessionID, successorID, taskID string) {
	t.Helper()
	ctx := context.Background()
	resetDMInbox(t)
	agentID, sessionID, _, _ := setupDirectChatSession(t, ctx, label)
	cleanupSuccessor(t, sessionID)
	taskID = runningChatTask(t, agentID, sessionID)

	response := continueAsAgentTask(t, testHandler, sessionID, taskID, agentID)
	if response.Code != http.StatusCreated {
		t.Fatalf("continue: %d %s", response.Code, response.Body.String())
	}
	var continuation ChatContinueResponse
	if err := json.Unmarshal(response.Body.Bytes(), &continuation); err != nil {
		t.Fatal(err)
	}
	return sessionID, continuation.Session.ID, taskID
}

func rolloverThenFinalReply(t *testing.T, label, output string) (sessionID, successorID, taskID string) {
	t.Helper()
	sessionID, successorID, taskID = rolloverRunning(t, label)
	if _, err := testHandler.TaskService.CompleteTask(context.Background(), parseUUID(taskID), completeResult(t, output), "", "", "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	return sessionID, successorID, taskID
}

// chatMessageSessions records the session of every chat:message event for the
// task, so a test can tell which DM a client would be told to refresh.
func chatMessageSessions(taskID string) func() []string {
	var mu sync.Mutex
	var sessions []string
	testHandler.Bus.Subscribe(protocol.EventChatMessage, func(e events.Event) {
		payload, ok := e.Payload.(protocol.ChatMessagePayload)
		if !ok || payload.TaskID != taskID {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		sessions = append(sessions, payload.ChatSessionID)
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), sessions...)
	}
}

// assertOutcomeLandedInSuccessor checks the run's only assistant message sits in
// the new DM, not the archived one, and clients of the new DM were told.
func assertOutcomeLandedInSuccessor(t *testing.T, sessionID, successorID, taskID string, announced []string) {
	t.Helper()
	if count := dbfx.Count(t, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND role = 'assistant' AND task_id = $2`, sessionID, taskID); count != 0 {
		t.Fatalf("%d assistant messages of the run were written only to the archived DM", count)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND role = 'assistant' AND task_id = $2`, successorID, taskID); count != 1 {
		t.Fatalf("the new DM has %d assistant messages of the run, want 1", count)
	}
	if len(announced) != 1 || announced[0] != successorID {
		t.Fatalf("chat:message events went to sessions %v, want only the new DM %s", announced, successorID)
	}
}

func TestRolloverFailureLandsInTheNewDMAndIsAnnounced(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	sessionID, successorID, taskID := rolloverRunning(t, "rollover failure")
	announced := chatMessageSessions(taskID)
	if _, err := testHandler.TaskService.FailTask(context.Background(), parseUUID(taskID), "boom", "", "", "", "agent_error", false, "", ""); err != nil {
		t.Fatal(err)
	}
	assertOutcomeLandedInSuccessor(t, sessionID, successorID, taskID, announced())
}

func TestRolloverCancelLandsInTheNewDMAndIsAnnounced(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	sessionID, successorID, taskID := rolloverRunning(t, "rollover cancel")
	insertTaskTranscriptRow(t, ctx, taskID)
	announced := chatMessageSessions(taskID)
	if _, err := testHandler.TaskService.CancelTask(ctx, parseUUID(taskID)); err != nil {
		t.Fatal(err)
	}
	assertOutcomeLandedInSuccessor(t, sessionID, successorID, taskID, announced())
}

// The deferred path settles after the daemon's transcript flush. Its
// chat:cancel_finalized payload must name the DM the "Stopped." row is in:
// web inserts the row into the cache of the payload's session.
func TestRolloverDeferredCancelFinalizedTargetsTheNewDM(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	sessionID, successorID, taskID := rolloverRunning(t, "rollover deferred cancel")
	announced := chatMessageSessions(taskID)
	var mu sync.Mutex
	var finalized []string
	testHandler.Bus.Subscribe(protocol.EventChatCancelFinalized, func(e events.Event) {
		payload, ok := e.Payload.(protocol.ChatCancelFinalizedPayload)
		if !ok || payload.TaskID != taskID {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		finalized = append(finalized, payload.ChatSessionID+"/"+e.ChatSessionID)
	})
	if _, err := testHandler.TaskService.CancelTask(ctx, parseUUID(taskID)); err != nil {
		t.Fatal(err)
	}
	insertTaskTranscriptRow(t, ctx, taskID)
	if !testHandler.TaskService.FinalizeDeferredCancelledChat(ctx, parseUUID(taskID)) {
		t.Fatal("deferred cancel was not settled")
	}
	assertOutcomeLandedInSuccessor(t, sessionID, successorID, taskID, announced())
	mu.Lock()
	defer mu.Unlock()
	if want := successorID + "/" + successorID; len(finalized) != 1 || finalized[0] != want {
		t.Fatalf("chat:cancel_finalized payload/envelope sessions = %v, want [%s]", finalized, want)
	}
}

func TestRolloverFinalReplyIsTheNewDMsFirstVisibleMessage(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const reply = "Câu trả lời cuối của lượt này."
	sessionID, successorID, taskID := rolloverThenFinalReply(t, "rollover reply", reply)

	var firstVisible string
	err := testPool.QueryRow(context.Background(),
		`SELECT content FROM chat_message
		 WHERE chat_session_id = $1 AND message_kind IS DISTINCT FROM 'continuation_summary'
		 ORDER BY created_at, id LIMIT 1`, successorID).Scan(&firstVisible)
	if err != nil || firstVisible != reply {
		t.Fatalf("first visible message of the new DM = %q (err %v), want the final reply %q", firstVisible, err, reply)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND role = 'assistant' AND task_id = $2`, sessionID, taskID); count != 0 {
		t.Fatalf("%d assistant messages of the run were written only to the archived DM", count)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND message_kind = 'continuation_summary'`, successorID); count != 1 {
		t.Fatalf("the agent-only summary must stay in the new DM for the next run, found %d", count)
	}
	records := dmRecords(t)
	if len(records) != 1 || records[0].Read || records[0].ChatSessionID == nil || *records[0].ChatSessionID != successorID {
		t.Fatalf("the final reply must raise one unread notification on the new DM %s: %+v", successorID, records)
	}
}
