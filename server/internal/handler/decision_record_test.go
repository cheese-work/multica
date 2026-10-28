package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type askedRecordFixture struct {
	requestCoreFixture
	decision                        db.DecisionRequest
	parentID, messageID, chatTaskID string
}

func newAskedRecordFixture(test *testing.T) askedRecordFixture {
	test.Helper()
	fixture := newRequestCoreFixture(test, false)
	result, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input())
	if err != nil {
		test.Fatal(err)
	}
	messageID := dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": fixture.sessionID, "role": "user", "content": "  Yes.\nKeep the exact words.  ", "sent_via": "interactive_jwt"})
	chatTaskID := dbfx.Task(test, fixture.agentID, testutil.Cols{"status": "running", "chat_session_id": fixture.sessionID, "runtime_id": handlerTestRuntimeID(test)})
	dbfx.Exec(test, `UPDATE agent_task_queue SET started_at = clock_timestamp() WHERE id = $1`, chatTaskID)
	return askedRecordFixture{fixture, result.Request, uuidToString(result.Comment.ID), messageID, chatTaskID}
}

func (fixture askedRecordFixture) recordInput() decisionRecordInput {
	return decisionRecordInput{ParentID: parseUUID(fixture.parentID), MessageIDs: []pgtype.UUID{parseUUID(fixture.messageID)}, Content: "Agent explanation, not the human quote."}
}

func (fixture askedRecordFixture) recordRequest() *http.Request {
	request := fixture.request()
	request.Header.Set("X-Task-ID", fixture.chatTaskID)
	return request
}

func (fixture askedRecordFixture) assertRecordCounts(test *testing.T, wanted int) {
	test.Helper()
	for label, query := range map[string]string{
		"answers":    `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`,
		"quotes":     `SELECT count(*) FROM comment WHERE decision_answer_id IN (SELECT id FROM decision_answer WHERE decision_request_id = $1) AND decision_comment_kind = 'quote'`,
		"references": `SELECT count(*) FROM chat_message WHERE chat_session_id = (SELECT chat_session_id FROM decision_request WHERE id = $1) AND message_kind = 'decision_reference'`,
	} {
		if count := dbfx.Count(test, query, fixture.decision.ID); count != wanted {
			test.Fatalf("%s = %d, want %d", label, count, wanted)
		}
	}
}

func TestAskedDecisionRecordRoundTrip(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	result, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	fixture.assertRecordCounts(test, 1)
	if result.Request.Status != "answered" || result.Request.ChatSessionID != fixture.decision.ChatSessionID || result.Answer.SourceChatSessionID != fixture.decision.ChatSessionID || result.Answer.AnsweredByTaskID != parseUUID(fixture.chatTaskID) || result.Answer.WakeAt.Valid || result.Answer.DisclaimedAt.Valid {
		test.Fatalf("wrong answered/pending state: %+v %+v", result.Request, result.Answer)
	}
	if result.Answer.ContentSnapshot != "  Yes.\nKeep the exact words.  " || len(result.Answer.ChatMessageIds) != 1 || result.Answer.ChatMessageIds[0] != parseUUID(fixture.messageID) {
		test.Fatalf("source snapshot changed: %+v", result.Answer)
	}
	if result.Comment.DecisionAnswerID != result.Answer.ID || result.Comment.DecisionCommentKind.String != "quote" || result.Comment.ParentID != parseUUID(fixture.parentID) || result.Comment.SourceTaskID != parseUUID(fixture.chatTaskID) || result.Answer.IssueCommentID != result.Comment.ID || !strings.Contains(result.Comment.Content, ">   Yes.\n> Keep the exact words.  ") {
		test.Fatalf("unstamped or rewritten quote: %+v", result.Comment)
	}
	if result.Message.MessageKind != "decision_reference" || result.Message.ChatSessionID != fixture.decision.ChatSessionID {
		test.Fatalf("missing DM reference: %+v", result.Message)
	}
	workspace, err := testHandler.Queries.GetWorkspace(context.Background(), fixture.issue.WorkspaceID)
	if err != nil {
		test.Fatal(err)
	}
	wantIssueLink := "/" + workspace.Slug + "/issues/" + uuidToString(fixture.issue.ID)
	if !strings.Contains(result.Message.Content, "Open issue: "+wantIssueLink) {
		test.Fatalf("decision note has wrong issue link: %s", result.Message.Content)
	}
	replay, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil || !replay.Replayed || replay.Answer.ID != result.Answer.ID || replay.Comment.ID != result.Comment.ID {
		test.Fatalf("identical retry did not return the original answer: %+v %v", replay, err)
	}
	fixture.assertRecordCounts(test, 1)
}

func TestAskedDecisionRecordRefusals(test *testing.T) {
	for _, mutation := range []string{"pat", "jwt", "other_issue_task", "task_completed", "task_failed", "task_cancelled", "wrong_agent", "archived_lane", "channel_lane", "intro_lane", "answered", "cancelled", "unprompted", "ambiguous_parent", "deleted_parent", "wrong_parent_issue", "wrong_human", "wrong_request_lane", "assistant_source", "task_token_source", "pat_source", "cli_source", "channel_source", "kickoff_source", "missing_source", "other_session_source", "before_ping", "after_start", "no_messages", "duplicate_messages"} {
		test.Run(mutation, func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			request, input := fixture.recordRequest(), fixture.recordInput()
			wanted := http.StatusForbidden
			switch mutation {
			case "pat", "jwt":
				request.Header.Set("X-Actor-Source", mutation)
			case "other_issue_task":
				request.Header.Set("X-Task-ID", fixture.taskID)
				dbfx.Exec(test, `UPDATE agent_task_queue SET issue_id = $1 WHERE id = $2`, dbfx.Issue(test, "Other task issue"), fixture.taskID)
			case "task_completed", "task_failed", "task_cancelled":
				dbfx.Exec(test, `UPDATE agent_task_queue SET status = $1 WHERE id = $2`, strings.TrimPrefix(mutation, "task_"), fixture.chatTaskID)
			case "wrong_agent":
				request.Header.Set("X-Agent-ID", createHandlerTestAgent(test, "wrong recorder", []byte("[]")))
			case "archived_lane":
				dbfx.Exec(test, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, fixture.sessionID)
			case "channel_lane":
				dbfx.Exec(test, `UPDATE chat_session SET channel_origin_at = now() WHERE id = $1`, fixture.sessionID)
			case "intro_lane":
				dbfx.Exec(test, `UPDATE chat_session SET is_agent_intro = true WHERE id = $1`, fixture.sessionID)
			case "answered", "cancelled":
				wanted = http.StatusConflict
				dbfx.Exec(test, `UPDATE decision_request SET status = $1 WHERE id = $2`, mutation, fixture.decision.ID)
			case "unprompted":
				wanted = http.StatusConflict
				dbfx.Exec(test, `UPDATE decision_request SET origin = 'unprompted' WHERE id = $1`, fixture.decision.ID)
			case "ambiguous_parent":
				wanted, input.ParentID = http.StatusBadRequest, parseUUID(fixture.tagID)
			case "deleted_parent":
				wanted = http.StatusNotFound
				dbfx.Exec(test, `UPDATE comment SET deleted_at = now() WHERE id = $1`, fixture.parentID)
			case "wrong_parent_issue":
				wanted = http.StatusNotFound
				dbfx.Exec(test, `UPDATE comment SET issue_id = $1 WHERE id = $2`, dbfx.Issue(test, "wrong issue"), fixture.parentID)
			case "wrong_human":
				dbfx.Exec(test, `UPDATE decision_request SET human_user_id = $1 WHERE id = $2`, testUserID, fixture.decision.ID)
			case "wrong_request_lane":
				dbfx.Exec(test, `UPDATE decision_request SET chat_session_id = $1 WHERE id = $2`, dbfx.ChatSession(test, fixture.agentID), fixture.decision.ID)
			default:
				wanted = http.StatusBadRequest
				switch mutation {
				case "assistant_source":
					dbfx.Exec(test, `UPDATE chat_message SET role = 'assistant' WHERE id = $1`, fixture.messageID)
				case "task_token_source", "pat_source", "cli_source":
					source := strings.TrimSuffix(mutation, "_source")
					if source == "cli" {
						source = "cli_jwt"
					}
					dbfx.Exec(test, `UPDATE chat_message SET sent_via = $1 WHERE id = $2`, source, fixture.messageID)
				case "channel_source":
					dbfx.Exec(test, `UPDATE chat_message SET channel_ingested = true WHERE id = $1`, fixture.messageID)
				case "kickoff_source":
					dbfx.Exec(test, `UPDATE chat_message SET message_kind = 'kickoff' WHERE id = $1`, fixture.messageID)
				case "missing_source":
					dbfx.Exec(test, `DELETE FROM chat_message WHERE id = $1`, fixture.messageID)
				case "other_session_source":
					dbfx.Exec(test, `UPDATE chat_message SET chat_session_id = $1 WHERE id = $2`, dbfx.ChatSession(test, fixture.agentID), fixture.messageID)
				case "before_ping":
					dbfx.Exec(test, `UPDATE chat_message SET created_at = 'epoch' WHERE id = $1`, fixture.messageID)
				case "after_start":
					dbfx.Exec(test, `UPDATE chat_message SET created_at = clock_timestamp() + interval '1 hour' WHERE id = $1`, fixture.messageID)
				case "no_messages":
					input.MessageIDs = nil
				case "duplicate_messages":
					input.MessageIDs = append(input.MessageIDs, input.MessageIDs[0])
				}
			}
			result, err := testHandler.createAskedDecisionRecord(request, fixture.issue, input)
			var refusal decisionIntentRefusal
			if !errors.As(err, &refusal) || refusal.status != wanted || result.Answer.ID.Valid {
				test.Fatalf("want refusal %d with no result, got %+v / %v", wanted, result, err)
			}
			fixture.assertRecordCounts(test, 0)
		})
	}
}

func TestAskedDecisionRecordEscalationHold(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'deploy' WHERE id = $1`, fixture.decision.ID)
	var before pgtype.Timestamptz
	dbfx.QueryRow(test, `SELECT clock_timestamp()`).Scan(&before)
	result, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	if result.Answer.EffectiveAt.Time.Sub(before.Time).Minutes() < 10 || result.Answer.WakeAt.Valid {
		test.Fatalf("hold was bypassed: %+v", result.Answer)
	}
}

func TestAskedDecisionRecordNearestParent(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	child := dbfx.Insert(test, "comment", testutil.Cols{"issue_id": fixture.issueID, "workspace_id": testWorkspaceID, "author_type": "member", "author_id": testUserID, "parent_id": fixture.parentID, "content": "Thread reply"})
	input := fixture.recordInput()
	input.ParentID = parseUUID(child)
	result, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, input)
	if err != nil || result.Comment.ParentID != input.ParentID || result.Request.ID != fixture.decision.ID {
		test.Fatalf("parent-chain context did not resolve: %+v %v", result, err)
	}
	var status string
	if err := testPool.QueryRow(context.Background(), `SELECT status FROM decision_request WHERE id = $1`, fixture.decision.ID).Scan(&status); err != nil || status != "answered" {
		test.Fatalf("request was not answered: %s %v", status, err)
	}
}

func seedAskedRecordContext(test *testing.T, fixture askedRecordFixture) (string, string) {
	test.Helper()
	requestID := dbfx.Insert(test, "decision_request", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": fixture.issueID, "asking_agent_id": fixture.agentID, "human_user_id": fixture.humanID, "chat_session_id": fixture.sessionID, "ping_message_id": fixture.decision.PingMessageID, "escalation_class": "reversible", "origin": "asked", "status": "open"})
	parentID := dbfx.Insert(test, "comment", testutil.Cols{"issue_id": fixture.issueID, "workspace_id": testWorkspaceID, "author_type": "agent", "author_id": fixture.agentID, "content": "Another ask", "decision_request_id": requestID, "decision_comment_kind": "ask"})
	return requestID, parentID
}

func TestAskedDecisionRecordRepeatedSource(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	requestID, parentID := seedAskedRecordContext(test, fixture)
	input := fixture.recordInput()
	secondMessage := dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": fixture.sessionID, "role": "user", "content": "Second paragraph.", "sent_via": "interactive_jwt"})
	dbfx.Exec(test, `UPDATE agent_task_queue SET started_at = clock_timestamp() WHERE id = $1`, fixture.chatTaskID)
	input.MessageIDs = append(input.MessageIDs, parseUUID(secondMessage))
	first, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, input)
	if err != nil {
		test.Fatal(err)
	}
	input.ParentID = parseUUID(parentID)
	second, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, input)
	if err != nil || second.Request.ID != parseUUID(requestID) || second.Answer.ID == first.Answer.ID || second.Answer.ContentSnapshot != first.Answer.ContentSnapshot || second.Answer.ContentSnapshot != "  Yes.\nKeep the exact words.  \n\nSecond paragraph." {
		test.Fatalf("same source did not answer both requests intact: %+v %v", second, err)
	}
	if dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE source_chat_session_id = $1 AND wake_at IS NULL`, fixture.sessionID) != 2 {
		test.Fatal("repeated-source records did not remain pending")
	}
}

func TestAskedDecisionRecordNearestQuoteDoesNotFallback(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	answer, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	requestID, parentID := seedAskedRecordContext(test, fixture)
	dbfx.Exec(test, `UPDATE comment SET parent_id = $1 WHERE id = $2`, parentID, answer.Comment.ID)
	child := dbfx.Insert(test, "comment", testutil.Cols{"issue_id": fixture.issueID, "workspace_id": testWorkspaceID, "author_type": "member", "author_id": testUserID, "parent_id": answer.Comment.ID, "content": "Reply below quote"})
	input := fixture.recordInput()
	input.ParentID = parseUUID(child)
	_, err = testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, input)
	var refusal decisionIntentRefusal
	if !errors.As(err, &refusal) || refusal.status != http.StatusConflict {
		test.Fatalf("nearest answered quote was not refused: %v", err)
	}
	if dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'open'`, requestID) != 1 || dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, requestID) != 0 {
		test.Fatal("record fell back to the eligible ancestor")
	}
}
