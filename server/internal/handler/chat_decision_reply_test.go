package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestSendChatDecisionReplyBindsOnlySelectedRequest(test *testing.T) {
	agentID, sessionID, _, _ := setupDirectChatSession(test, context.Background(), "Annotated decision")
	firstID, firstParentID, firstIssue := seedDMDecision(test, agentID, sessionID, "A. First ask", "open", "2 minutes")
	secondID, parentID, secondIssue := seedDMDecision(test, agentID, sessionID, "C. CHE-2162", "open", "1 minute")
	content := "> C. CHE-2162\n\n1\n\n> D. Start one at a time\n\ndone"
	request := newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/messages", map[string]any{"content": content, "decision_request_id": secondID})
	request.Header.Set("X-Actor-Source", "interactive_jwt")
	request = withChatTestWorkspaceCtx(test, withURLParam(request, "sessionId", sessionID))
	var sent SendChatMessageResponse
	testutil.Call(test, testHandler.SendChatMessage, request).Want(http.StatusCreated).JSON(&sent)
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'open'`, firstID); count != 1 {
		test.Fatal("reply changed the first request")
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, firstID); count != 0 {
		test.Fatal("reply recorded on the first request")
	}
	var snapshot, quote string
	err := testPool.QueryRow(context.Background(), `SELECT a.content_snapshot, c.content FROM decision_answer a JOIN comment c ON c.id = a.issue_comment_id JOIN decision_request r ON r.id = a.decision_request_id WHERE a.decision_request_id = $1 AND r.status = 'answered' AND c.issue_id = $2 AND c.parent_id = $3 AND a.answered_by_task_id = $4 AND a.chat_message_ids = ARRAY[$5::uuid]`, secondID, secondIssue, parentID, sent.TaskID, sent.MessageID).Scan(&snapshot, &quote)
	if err != nil {
		test.Fatalf("reply was not recorded on card 2: %v", err)
	}
	if snapshot != content || !strings.Contains(quote, "C. CHE-2162") || strings.Contains(quote, "&#46;") || strings.Contains(quote, "&#45;") || strings.Index(quote, "C. CHE-2162") > strings.Index(quote, "D. Start") {
		test.Fatalf("quotes were escaped or reordered: %q / %q", snapshot, quote)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id = $1 AND decision_comment_kind = 'quote'`, firstIssue); count != 0 {
		test.Fatal("answer leaked onto card 1's issue")
	}
	message, err := testHandler.Queries.GetChatMessage(context.Background(), parseUUID(sent.MessageID))
	if err != nil {
		test.Fatal(err)
	}
	session, err := testHandler.Queries.GetChatSession(context.Background(), parseUUID(sessionID))
	if err != nil {
		test.Fatal(err)
	}
	block, err := testHandler.decisionContextBlock(context.Background(), session, parseUUID(sent.TaskID), []db.ChatMessage{message})
	if err != nil || strings.Contains(block, "One open decision request") || !strings.Contains(block, "already recorded") {
		test.Fatalf("explicit answer could be guessed onto card 1: %q / %v", block, err)
	}
	markTaskRunning(test, context.Background(), sent.TaskID)
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(firstIssue))
	if err != nil {
		test.Fatal(err)
	}
	recordRequest := newRequest(http.MethodPost, "/", nil)
	recordRequest.Header.Set("X-Actor-Source", "task_token")
	recordRequest.Header.Set("X-Task-ID", sent.TaskID)
	recordRequest.Header.Set("X-Agent-ID", agentID)
	_, err = testHandler.createAskedDecisionRecord(recordRequest, issue, decisionRecordInput{ParentID: parseUUID(firstParentID), MessageIDs: []pgtype.UUID{message.ID}})
	requireRequestRefusal(test, err)
	if dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, firstID) != 0 {
		test.Fatal("answer was reused for the other request")
	}
}

func TestSendChatDecisionReplyRefusesWithoutWriting(test *testing.T) {
	for _, mutation := range []string{"malformed", "missing", "other_workspace", "answered", "cancelled", "unprompted", "other_session", "other_human", "other_agent", "closed_issue", "pat", "channel", "intro"} {
		test.Run(mutation, func(test *testing.T) {
			agentID, sessionID, _, _ := setupDirectChatSession(test, context.Background(), "Invalid annotation")
			requestID, _, issueID := seedDMDecision(test, agentID, sessionID, "Ask", "open", "1 minute")
			status := http.StatusConflict
			source := "interactive_jwt"
			switch mutation {
			case "malformed":
				requestID, status = "bad", http.StatusBadRequest
			case "missing":
				requestID, status = "00000000-0000-4000-8000-000000000000", http.StatusNotFound
			case "other_workspace":
				dbfx.Exec(test, `UPDATE decision_request SET workspace_id = '00000000-0000-4000-8000-000000000001' WHERE id = $1`, requestID)
				status = http.StatusNotFound
			case "answered", "cancelled":
				dbfx.Exec(test, `UPDATE decision_request SET status = $2 WHERE id = $1`, requestID, mutation)
			case "unprompted":
				dbfx.Exec(test, `UPDATE decision_request SET origin = 'unprompted' WHERE id = $1`, requestID)
			case "other_session":
				dbfx.Exec(test, `UPDATE decision_request SET chat_session_id = $2 WHERE id = $1`, requestID, dbfx.ChatSession(test, agentID))
				status = http.StatusForbidden
			case "other_human":
				dbfx.Exec(test, `UPDATE decision_request SET human_user_id = $2 WHERE id = $1`, requestID, dbfx.User(test, "Other human", "che2175-other@example.test"))
				status = http.StatusForbidden
			case "other_agent":
				dbfx.Exec(test, `UPDATE decision_request SET asking_agent_id = $2 WHERE id = $1`, requestID, createHandlerTestAgent(test, "Other asker", []byte("[]")))
				status = http.StatusForbidden
			case "closed_issue":
				dbfx.Exec(test, `UPDATE issue SET status = 'done' WHERE id = $1`, issueID)
			case "pat":
				source, status = "pat", http.StatusForbidden
			case "channel":
				dbfx.Exec(test, `UPDATE chat_session SET channel_origin_at = now() WHERE id = $1`, sessionID)
				status = http.StatusForbidden
			case "intro":
				dbfx.Exec(test, `UPDATE chat_session SET is_agent_intro = true WHERE id = $1`, sessionID)
				status = http.StatusForbidden
			}
			before := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sessionID)
			request := newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/messages", map[string]any{"content": "> Ask\n\n1", "decision_request_id": requestID})
			request.Header.Set("X-Actor-Source", source)
			request = withChatTestWorkspaceCtx(test, withURLParam(request, "sessionId", sessionID))
			testutil.Call(test, testHandler.SendChatMessage, request).Want(status)
			if dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sessionID) != before || dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, sessionID) != 0 {
				test.Fatal("refused annotation left a message or task")
			}
		})
	}
}

func TestSendChatDecisionReplyKeepsHoldAndSingleAnswer(test *testing.T) {
	agentID, sessionID, _, _ := setupDirectChatSession(test, context.Background(), "Held decision")
	requestID, _, _ := seedDMDecision(test, agentID, sessionID, "Deploy?", "open", "1 minute")
	dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'deploy' WHERE id = $1`, requestID)
	before := time.Now()
	for attempt, status := range []int{http.StatusCreated, http.StatusConflict} {
		request := newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/messages", map[string]any{"content": "> Deploy?\n\nYes", "decision_request_id": requestID})
		request.Header.Set("X-Actor-Source", "interactive_jwt")
		request = withChatTestWorkspaceCtx(test, withURLParam(request, "sessionId", sessionID))
		testutil.Call(test, testHandler.SendChatMessage, request).Want(status)
		if attempt == 0 {
			var effectiveAt time.Time
			if err := testPool.QueryRow(context.Background(), `SELECT effective_at FROM decision_answer WHERE decision_request_id = $1 AND wake_at IS NULL`, requestID).Scan(&effectiveAt); err != nil || effectiveAt.Before(before.Add(10*time.Minute)) {
				test.Fatalf("held decision became effective early: %v / %v", effectiveAt, err)
			}
		}
	}
	if dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, requestID) != 1 || dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, sessionID) != 1 {
		test.Fatal("second send duplicated the decision or task")
	}
}

func TestSendChatDecisionReplyRollback(test *testing.T) {
	agentID, sessionID, _, _ := setupDirectChatSession(test, context.Background(), "Rollback decision")
	requestID, _, issueID := seedDMDecision(test, agentID, sessionID, "Ask", "open", "1 minute")
	handler := *testHandler
	handler.TxStarter = rollbackOnCommitTxStarter{pool: testPool}
	before := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sessionID)
	request := newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/messages", map[string]any{"content": "> Ask\n\n1", "decision_request_id": requestID})
	request.Header.Set("X-Actor-Source", "interactive_jwt")
	request = withChatTestWorkspaceCtx(test, withURLParam(request, "sessionId", sessionID))
	testutil.Call(test, handler.SendChatMessage, request).Want(http.StatusInternalServerError)
	if dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sessionID) != before || dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, sessionID) != 0 || dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, requestID) != 0 || dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id = $1 AND decision_comment_kind = 'quote'`, issueID) != 0 {
		test.Fatal("rollback left a task, message, answer, or issue quote")
	}
	if dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'open'`, requestID) != 1 {
		test.Fatal("rollback changed the request state")
	}
}
