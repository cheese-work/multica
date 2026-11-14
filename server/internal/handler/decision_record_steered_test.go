package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// steerMessage turns the fixture's source message into a DM follow-up the
// session creator steered into taskID: created after that task started, with a
// receipt in the given status.
func (fixture askedRecordFixture) steerMessage(test *testing.T, messageID, taskID, status string) {
	test.Helper()
	dbfx.Exec(test, `UPDATE chat_message SET created_at = clock_timestamp() WHERE id = $1`, messageID)
	dbfx.InsertNoID(test, "chat_task_supplement", testutil.Cols{
		"message_id": messageID, "task_id": taskID, "workspace_id": testWorkspaceID,
		"chat_session_id": fixture.sessionID, "author_id": testUserID, "status": status,
	}, "message_id = $1", messageID)
}

func TestSteeredMessageDecisionRecord(test *testing.T) {
	test.Skip("CHE-4873 quarantine (CHE-4909): seeds two active tasks in one chat session; idx_agent_task_queue_one_active_per_chat rejects it")
	for _, status := range []string{"delivered", "pending", "delivering", "uncertain", "failed"} {
		test.Run(status, func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			fixture.steerMessage(test, fixture.messageID, fixture.chatTaskID, status)
			result, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
			if status == "delivered" {
				if err != nil || result.Answer.ContentSnapshot != "  Yes.\nKeep the exact words.  " {
					test.Fatalf("delivered steered message refused or rewritten: %+v / %v", result.Answer, err)
				}
				fixture.assertRecordCounts(test, 1)
				return
			}
			var refusal decisionIntentRefusal
			if !errors.As(err, &refusal) || refusal.status != http.StatusBadRequest || result.Answer.ID.Valid {
				test.Fatalf("want 400 refusal for %s receipt, got %+v / %v", status, result, err)
			}
			fixture.assertRecordCounts(test, 0)
		})
	}
	test.Run("other_task", func(test *testing.T) {
		fixture := newAskedRecordFixture(test)
		otherTask := dbfx.Task(test, fixture.agentID, testutil.Cols{"status": "running", "chat_session_id": fixture.sessionID, "runtime_id": handlerTestRuntimeID(test)})
		fixture.steerMessage(test, fixture.messageID, otherTask, "delivered")
		var refusal decisionIntentRefusal
		if _, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput()); !errors.As(err, &refusal) || refusal.status != http.StatusBadRequest {
			test.Fatalf("want 400 refusal for a receipt bound to another task, got %v", err)
		}
		fixture.assertRecordCounts(test, 0)
	})
	test.Run("other_session", func(test *testing.T) {
		fixture := newAskedRecordFixture(test)
		fixture.steerMessage(test, fixture.messageID, fixture.chatTaskID, "delivered")
		dbfx.Exec(test, `UPDATE chat_message SET chat_session_id = $1 WHERE id = $2`, dbfx.ChatSession(test, fixture.agentID), fixture.messageID)
		var refusal decisionIntentRefusal
		if _, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput()); !errors.As(err, &refusal) || refusal.status != http.StatusBadRequest {
			test.Fatalf("want 400 refusal for another session's message, got %v", err)
		}
		fixture.assertRecordCounts(test, 0)
	})
}

func TestDecisionContextBlockListsDeliveredSteeredMessages(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	deliveredID := fixture.messageID
	fixture.steerMessage(test, deliveredID, fixture.chatTaskID, "delivered")
	uncertainID := dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": fixture.sessionID, "role": "user", "content": "maybe unseen", "sent_via": "interactive_jwt"})
	fixture.steerMessage(test, uncertainID, fixture.chatTaskID, "uncertain")
	inputID := dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": fixture.sessionID, "role": "user", "content": "next turn", "sent_via": "interactive_jwt", "created_at": testutil.Raw("clock_timestamp() + interval '1 minute'")})

	ctx := context.Background()
	session, err := testHandler.Queries.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{WorkspaceID: fixture.issue.WorkspaceID, ID: parseUUID(fixture.sessionID)})
	if err != nil {
		test.Fatal(err)
	}
	input, err := testHandler.Queries.GetChatMessage(ctx, parseUUID(inputID))
	if err != nil {
		test.Fatal(err)
	}
	block, err := testHandler.decisionContextBlock(ctx, session, parseUUID(fixture.chatTaskID), []db.ChatMessage{input})
	if err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(block, deliveredID+" (steered)") {
		test.Fatalf("block does not list the delivered steered message:\n%s", block)
	}
	if strings.Contains(block, uncertainID) {
		test.Fatalf("block lists a steered message the agent may not have seen:\n%s", block)
	}
}
