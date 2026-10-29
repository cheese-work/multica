package handler

import (
	"context"
	"errors"
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

func TestSendChatDecisionReplyWithdrawnTurnCanBeRecordedAgain(test *testing.T) {
	for _, status := range []string{"failed", "cancelled"} {
		test.Run(status, func(test *testing.T) {
			ctx := context.Background()
			agentID, sessionID, runtimeID, _ := setupDirectChatSession(test, ctx, "Withdrawn bound answer")
			requestID, parentID, issueID := seedDMDecision(test, agentID, sessionID, "Deploy?", "open", "1 minute")
			dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'deploy' WHERE id = $1`, requestID)
			request := newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/messages", map[string]any{"content": "> Deploy?\n\nYes", "decision_request_id": requestID})
			request.Header.Set("X-Actor-Source", "interactive_jwt")
			request = withChatTestWorkspaceCtx(test, withURLParam(request, "sessionId", sessionID))
			var sent SendChatMessageResponse
			testutil.Call(test, testHandler.SendChatMessage, request).Want(http.StatusCreated).JSON(&sent)
			message, err := testHandler.Queries.GetChatMessage(ctx, parseUUID(sent.MessageID))
			if err != nil || !message.DecisionAnswerID.Valid {
				test.Fatalf("missing initial message binding: %v", err)
			}
			dbfx.Exec(test, `UPDATE agent_task_queue SET status = $2, completed_at = clock_timestamp() WHERE id = $1`, sent.TaskID, status)
			withdrawn, err := testHandler.FinalizeDecisionAnswer(ctx, parseUUID(testWorkspaceID), parseUUID(requestID))
			if err != nil {
				test.Fatal(err)
			}
			if withdrawn.Request.Status != "open" || !withdrawn.Answer.DisclaimedAt.Valid || withdrawn.Answer.DisclaimReason.String != "answering_turn_"+status || withdrawn.Answer.WakeAt.Valid {
				test.Fatalf("withdrawal changed worker semantics: %+v / %+v", withdrawn.Request, withdrawn.Answer)
			}
			persisted, err := testHandler.Queries.GetChatMessage(ctx, message.ID)
			if err != nil {
				test.Fatal(err)
			}
			if persisted.DecisionAnswerID != withdrawn.Answer.ID {
				test.Error("withdrawal lost the source message binding")
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE decision_answer_id = $1 AND message_kind = 'decision_reference'`, withdrawn.Answer.ID); count != 1 {
				test.Error("withdrawal removed the historical decision reference")
			}
			session, err := testHandler.Queries.GetChatSession(ctx, parseUUID(sessionID))
			if err != nil {
				test.Fatal(err)
			}
			block, err := testHandler.decisionContextBlock(ctx, session, parseUUID(sent.TaskID), []db.ChatMessage{message})
			if err != nil || strings.Contains(block, "already recorded") || !strings.Contains(block, requestID) {
				test.Errorf("withdrawn answer still suppresses retry context: %q / %v", block, err)
			}
			retryID := dbfx.Task(test, agentID, testutil.Cols{"chat_session_id": sessionID, "runtime_id": runtimeID})
			markTaskRunning(test, ctx, retryID)
			issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(issueID))
			if err != nil {
				test.Fatal(err)
			}
			recordRequest := newRequest(http.MethodPost, "/", nil)
			recordRequest.Header.Set("X-Actor-Source", "task_token")
			recordRequest.Header.Set("X-Task-ID", retryID)
			recordRequest.Header.Set("X-Agent-ID", agentID)
			before := time.Now()
			recorded, err := testHandler.createAskedDecisionRecord(recordRequest, issue, decisionRecordInput{ParentID: parseUUID(parentID), MessageIDs: []pgtype.UUID{message.ID}})
			if err != nil {
				test.Fatalf("withdrawn source cannot be recorded again: %v", err)
			}
			if recorded.Request.Status != "answered" || recorded.Answer.DisclaimedAt.Valid || recorded.Answer.ID == withdrawn.Answer.ID || recorded.Answer.WakeAt.Valid || recorded.Answer.EffectiveAt.Time.Before(before.Add(10*time.Minute)) || recorded.Answer.ContentSnapshot != message.Content {
				test.Fatalf("retry lost the answer or its hold: %+v", recorded.Answer)
			}
		})
	}
}

func TestSendChatDecisionReplyOrphanedBinding(test *testing.T) {
	for _, scenario := range []string{"context", "record"} {
		test.Run(scenario, func(test *testing.T) {
			ctx := context.Background()
			agentID, sessionID, _, _ := setupDirectChatSession(test, ctx, "Orphaned bound answer")
			requestID, _, issueID := seedDMDecision(test, agentID, sessionID, "Original ask", "open", "2 minutes")
			var survivingID, survivingParentID, survivingIssueID string
			if scenario == "record" {
				survivingID, survivingParentID, survivingIssueID = seedDMDecision(test, agentID, sessionID, "Surviving ask", "open", "1 minute")
			}
			content := "> Original ask\n\nYes"
			request := newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/messages", map[string]any{"content": content, "decision_request_id": requestID})
			request.Header.Set("X-Actor-Source", "interactive_jwt")
			request = withChatTestWorkspaceCtx(test, withURLParam(request, "sessionId", sessionID))
			var sent SendChatMessageResponse
			testutil.Call(test, testHandler.SendChatMessage, request).Want(http.StatusCreated).JSON(&sent)
			dbfx.Exec(test, `DELETE FROM issue WHERE id = $1`, issueID)
			if err := testHandler.cleanupDecisionOrphans(ctx, parseUUID(testWorkspaceID), parseUUID(issueID)); err != nil {
				test.Fatal(err)
			}
			if dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1`, requestID) != 0 || dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, requestID) != 0 {
				test.Fatal("orphan cleanup kept the deleted issue's decision rows")
			}
			message, err := testHandler.Queries.GetChatMessage(ctx, parseUUID(sent.MessageID))
			if err != nil || !message.DecisionAnswerID.Valid {
				test.Fatalf("orphan cleanup did not leave the expected stale message binding: %v", err)
			}
			if scenario == "context" {
				session, err := testHandler.Queries.GetChatSession(ctx, parseUUID(sessionID))
				if err != nil {
					test.Fatal(err)
				}
				block, err := testHandler.decisionContextBlock(ctx, session, parseUUID(sent.TaskID), []db.ChatMessage{message})
				if err != nil {
					test.Fatalf("orphaned binding wedges chat decision context: %v", err)
				}
				if !strings.Contains(block, sent.MessageID) || strings.Contains(block, "already recorded") || strings.Contains(block, "explicitly selected decision request") {
					test.Fatalf("orphaned source was not treated as unbound: %q", block)
				}
				return
			}
			markTaskRunning(test, ctx, sent.TaskID)
			issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(survivingIssueID))
			if err != nil {
				test.Fatal(err)
			}
			recordRequest := newRequest(http.MethodPost, "/", nil)
			recordRequest.Header.Set("X-Actor-Source", "task_token")
			recordRequest.Header.Set("X-Task-ID", sent.TaskID)
			recordRequest.Header.Set("X-Agent-ID", agentID)
			recorded, err := testHandler.createAskedDecisionRecordTx(recordRequest, issue, decisionRecordInput{ParentID: parseUUID(survivingParentID), MessageIDs: []pgtype.UUID{message.ID}})
			if err != nil {
				test.Fatalf("orphaned binding makes decision recording fail: %v", err)
			}
			if recorded.Request.ID != parseUUID(survivingID) || recorded.Request.Status != "answered" || recorded.Answer.ContentSnapshot != content || dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, survivingID) != 1 {
				test.Fatalf("orphaned source did not record as unbound: %+v", recorded)
			}
		})
	}
}

func TestSendChatDecisionReplyWithdrawnSelectionStaysOnCard(test *testing.T) {
	for _, status := range []string{"failed", "cancelled"} {
		test.Run(status, func(test *testing.T) {
			ctx := context.Background()
			agentID, sessionID, runtimeID, _ := setupDirectChatSession(test, ctx, "Keep the selected card")
			firstID, firstParentID, firstIssueID := seedDMDecision(test, agentID, sessionID, "A. First ask", "open", "2 minutes")
			secondID, secondParentID, secondIssueID := seedDMDecision(test, agentID, sessionID, "C. Selected ask", "open", "1 minute")
			request := newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/messages", map[string]any{"content": "> C. Selected ask\n\nYes", "decision_request_id": secondID})
			request.Header.Set("X-Actor-Source", "interactive_jwt")
			request = withChatTestWorkspaceCtx(test, withURLParam(request, "sessionId", sessionID))
			var sent SendChatMessageResponse
			testutil.Call(test, testHandler.SendChatMessage, request).Want(http.StatusCreated).JSON(&sent)
			dbfx.Exec(test, `UPDATE agent_task_queue SET status = $2 WHERE id = $1`, sent.TaskID, status)
			withdrawn, err := testHandler.FinalizeDecisionAnswer(ctx, parseUUID(testWorkspaceID), parseUUID(secondID))
			if err != nil || !withdrawn.Answer.DisclaimedAt.Valid || withdrawn.Request.Status != "open" {
				test.Fatalf("could not withdraw selected answer: %+v / %v", withdrawn, err)
			}
			message, err := testHandler.Queries.GetChatMessage(ctx, parseUUID(sent.MessageID))
			if err != nil {
				test.Fatal(err)
			}
			if message.DecisionAnswerID != withdrawn.Answer.ID {
				test.Error("withdrawal lost the selected request binding")
			}
			session, err := testHandler.Queries.GetChatSession(ctx, parseUUID(sessionID))
			if err != nil {
				test.Fatal(err)
			}
			block, err := testHandler.decisionContextBlock(ctx, session, parseUUID(sent.TaskID), []db.ChatMessage{message})
			if err != nil || !strings.Contains(block, secondID) || strings.Contains(block, firstID) || strings.Contains(block, "already recorded") {
				test.Errorf("withdrawn context lost the selected request: %q / %v", block, err)
			}
			retryID := dbfx.Task(test, agentID, testutil.Cols{"chat_session_id": sessionID, "runtime_id": runtimeID})
			markTaskRunning(test, ctx, retryID)
			recordRequest := newRequest(http.MethodPost, "/", nil)
			recordRequest.Header.Set("X-Actor-Source", "task_token")
			recordRequest.Header.Set("X-Task-ID", retryID)
			recordRequest.Header.Set("X-Agent-ID", agentID)
			firstIssue, err := testHandler.Queries.GetIssue(ctx, parseUUID(firstIssueID))
			if err != nil {
				test.Fatal(err)
			}
			_, err = testHandler.createAskedDecisionRecord(recordRequest, firstIssue, decisionRecordInput{ParentID: parseUUID(firstParentID), MessageIDs: []pgtype.UUID{message.ID}})
			var refusal decisionIntentRefusal
			if !errors.As(err, &refusal) || refusal.status != http.StatusBadRequest {
				test.Fatalf("card-2 reply was accepted for card 1: err=%v card1_answers=%d", err, dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, firstID))
			}
			if dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, firstID) != 0 || dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id = $1 AND decision_comment_kind = 'quote'`, firstIssueID) != 0 {
				test.Fatal("refused reuse left an answer or quote on card 1")
			}
			secondIssue, err := testHandler.Queries.GetIssue(ctx, parseUUID(secondIssueID))
			if err != nil {
				test.Fatal(err)
			}
			recorded, err := testHandler.createAskedDecisionRecord(recordRequest, secondIssue, decisionRecordInput{ParentID: parseUUID(secondParentID), MessageIDs: []pgtype.UUID{message.ID}})
			if err != nil || recorded.Request.Status != "answered" || recorded.Answer.DisclaimedAt.Valid || recorded.Answer.ID == withdrawn.Answer.ID || recorded.Answer.ContentSnapshot != message.Content {
				test.Fatalf("card-2 reply could not be recorded on card 2: %+v / %v", recorded, err)
			}
			message, err = testHandler.Queries.GetChatMessage(ctx, message.ID)
			if err != nil || message.DecisionAnswerID != recorded.Answer.ID {
				test.Errorf("re-recording did not bind the live answer: %+v / %v", message, err)
			}
			block, err = testHandler.decisionContextBlock(ctx, session, parseUUID(retryID), []db.ChatMessage{message})
			if err != nil || !strings.Contains(block, secondID) || !strings.Contains(block, "already recorded") {
				test.Errorf("re-recorded context lost the live selected answer: %q / %v", block, err)
			}
		})
	}
}

func TestSendChatDecisionReplyRefusesCompact(test *testing.T) {
	for _, content := range []string{"/compact", " /compact keep decisions ", "/compact\nkeep decisions"} {
		test.Run(content, func(test *testing.T) {
			agentID, sessionID, _, _ := setupDirectChatSession(test, context.Background(), "Compact is not an answer")
			requestID, _, issueID := seedDMDecision(test, agentID, sessionID, "Ask", "open", "1 minute")
			before := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sessionID)
			request := newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/messages", map[string]any{"content": content, "decision_request_id": requestID})
			request.Header.Set("X-Actor-Source", "interactive_jwt")
			request = withChatTestWorkspaceCtx(test, withURLParam(request, "sessionId", sessionID))
			testutil.Call(test, testHandler.SendChatMessage, request).Want(http.StatusBadRequest)
			if dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sessionID) != before || dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, sessionID) != 0 || dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, requestID) != 0 || dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id = $1 AND decision_comment_kind = 'quote'`, issueID) != 0 {
				test.Fatal("refused compact command left a task, message, answer, or quote")
			}
		})
	}
}

func TestSendChatDecisionReplyWithdrawalRollback(test *testing.T) {
	ctx := context.Background()
	agentID, sessionID, _, _ := setupDirectChatSession(test, ctx, "Atomic bound withdrawal")
	requestID, _, _ := seedDMDecision(test, agentID, sessionID, "Ask", "open", "1 minute")
	request := newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/messages", map[string]any{"content": "> Ask\n\nYes", "decision_request_id": requestID})
	request.Header.Set("X-Actor-Source", "interactive_jwt")
	request = withChatTestWorkspaceCtx(test, withURLParam(request, "sessionId", sessionID))
	var sent SendChatMessageResponse
	testutil.Call(test, testHandler.SendChatMessage, request).Want(http.StatusCreated).JSON(&sent)
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'failed' WHERE id = $1`, sent.TaskID)
	failure := errors.New("request reopen failed")
	broken := *testHandler
	broken.TxStarter = &requestCoreStarter{base: testPool, query: "-- name: ReopenDecisionRequest", fail: failure}
	result, err := broken.FinalizeDecisionAnswer(ctx, parseUUID(testWorkspaceID), parseUUID(requestID))
	if !errors.Is(err, failure) || len(result.Comments) != 0 {
		test.Fatalf("failed reopen escaped rollback: %+v / %v", result, err)
	}
	if dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1 AND disclaimed_at IS NULL`, requestID) != 1 || dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'answered'`, requestID) != 1 || dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE id = $1 AND decision_answer_id IS NOT NULL`, sent.MessageID) != 1 || dbfx.Count(test, `SELECT count(*) FROM comment WHERE decision_answer_id IN (SELECT id FROM decision_answer WHERE decision_request_id = $1) AND decision_comment_kind = 'disclaim'`, requestID) != 0 {
		test.Fatal("failed reopen partially withdrew the answer, request, message binding, or receipt")
	}
	if _, err := testHandler.FinalizeDecisionAnswer(ctx, parseUUID(testWorkspaceID), parseUUID(requestID)); err != nil {
		test.Fatalf("withdrawal could not recover after rollback: %v", err)
	}
}
