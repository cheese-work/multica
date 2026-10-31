package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// CHE-2119: the agent calls `continue` before its run's final reply is saved.
// That reply is the last thing the person reads, so it belongs in the new DM:
// first visible message there, with the notification, and not only in the
// archived one. The summary stays agent-only context.

func rolloverThenFinalReply(t *testing.T, label, output string) (sessionID, successorID, taskID string) {
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
	if _, err := testHandler.TaskService.CompleteTask(ctx, parseUUID(taskID), completeResult(t, output), "", "", "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	return sessionID, continuation.Session.ID, taskID
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
