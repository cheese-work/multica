package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestUnpromptedDecisionRecordAPI(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	body := `{"decision_record":true,"message_ids":["` + fixture.messageID + `"],"escalation_class":"deploy"}`
	request := fixture.recordRequest()
	request.Body = ioBody(body)
	request = withIssueParam(request, fixture.issueID)
	response := httptest.NewRecorder()
	testHandler.CreateComment(response, request)
	if response.Code != http.StatusCreated {
		test.Fatalf("unprompted record: status=%d body=%s", response.Code, response.Body.String())
	}
	var comment db.Comment
	if err := json.Unmarshal(response.Body.Bytes(), &comment); err != nil {
		test.Fatal(err)
	}
	answer, err := testHandler.Queries.GetDecisionAnswer(context.Background(), db.GetDecisionAnswerParams{WorkspaceID: fixture.issue.WorkspaceID, ID: comment.DecisionAnswerID})
	if err != nil {
		test.Fatal(err)
	}
	decision, err := testHandler.Queries.GetDecisionRequest(context.Background(), db.GetDecisionRequestParams{WorkspaceID: fixture.issue.WorkspaceID, ID: answer.DecisionRequestID})
	if err != nil {
		test.Fatal(err)
	}
	if decision.Origin != "unprompted" || decision.Status != "answered" || decision.PingMessageID.Valid || decision.DecisionIntentID.Valid || decision.HumanUserID != parseUUID(fixture.humanID) || decision.IssueCommentID != comment.ID || answer.WakeAt.Valid || answer.ContentSnapshot != "  Yes.\nKeep the exact words.  " {
		test.Fatalf("incorrect pending unprompted record: %+v %+v", decision, answer)
	}
	if answer.EffectiveAt.Time.Sub(answer.CreatedAt.Time).Minutes() < 9.9 {
		test.Fatal("deploy record lacks the hold")
	}
	if dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, fixture.issueID) != 1 {
		test.Fatal("pending quote triggered another issue task")
	}
}

func TestDecisionRecordAPIRefusals(test *testing.T) {
	for _, mutation := range []string{"missing_class", "invalid_class", "missing_messages", "invalid_uuid", "pat", "terminal_issue", "human_removed", "mixed_intent", "attachments"} {
		test.Run(mutation, func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			body := map[string]any{"decision_record": true, "message_ids": []string{fixture.messageID}, "escalation_class": "reversible"}
			request := fixture.recordRequest()
			wanted := http.StatusBadRequest
			switch mutation {
			case "missing_class":
				delete(body, "escalation_class")
			case "invalid_class":
				body["escalation_class"] = "unknown"
			case "missing_messages":
				delete(body, "message_ids")
			case "invalid_uuid":
				body["message_ids"] = []string{"bad"}
			case "pat":
				request.Header.Set("X-Actor-Source", "pat")
				wanted = http.StatusForbidden
			case "terminal_issue":
				dbfx.Exec(test, `UPDATE issue SET status = 'done' WHERE id = $1`, fixture.issueID)
				wanted = http.StatusForbidden
			case "human_removed":
				dbfx.Exec(test, `DELETE FROM member WHERE user_id = $1 AND workspace_id = $2`, fixture.humanID, testWorkspaceID)
				wanted = http.StatusForbidden
			case "mixed_intent":
				body["decision_intent"] = true
			case "attachments":
				body["attachment_ids"] = []string{fixture.messageID}
			}
			encoded, _ := json.Marshal(body)
			request.Body = ioBody(string(encoded))
			response := httptest.NewRecorder()
			testHandler.CreateComment(response, withIssueParam(request, fixture.issueID))
			if response.Code != wanted {
				test.Fatalf("status=%d want=%d body=%s", response.Code, wanted, response.Body.String())
			}
			if dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1 AND origin = 'unprompted'`, fixture.issueID) != 0 {
				test.Fatal("refusal committed a request")
			}
			fixture.assertRecordCounts(test, 0)
		})
	}
}

func ioBody(body string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(body))
}

func withIssueParam(request *http.Request, issueID string) *http.Request {
	route := chi.NewRouteContext()
	route.URLParams.Add("id", issueID)
	return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, route))
}

func TestUnpromptedDecisionRecordClassesAndParents(test *testing.T) {
	for _, class := range []string{"reversible", "spend", "deploy", "publish", "irreversible", "product_direction"} {
		test.Run(class, func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			input := fixture.recordInput()
			input.ParentID, input.EscalationClass = pgtype.UUID{}, class
			if class != "reversible" {
				input.ParentID = parseUUID(dbfx.Comment(test, fixture.issueID, "An ordinary thread"))
			}
			result, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, input)
			if err != nil {
				test.Fatal(err)
			}
			if result.Request.Origin != "unprompted" || result.Comment.ParentID != input.ParentID || result.Request.IssueCommentID != result.Comment.ID || result.Answer.WakeAt.Valid || !strings.Contains(result.Message.Content, class) || !strings.Contains(result.Message.Content, "Disclaim:") {
				test.Fatalf("wrong unprompted record: %+v", result)
			}
			held := result.Answer.EffectiveAt.Time.Sub(result.Answer.CreatedAt.Time).Minutes() > 9.9
			if held != (class != "reversible") {
				test.Fatalf("class=%s held=%t", class, held)
			}
		})
	}
}

func TestUnpromptedDecisionSourceRefusals(test *testing.T) {
	for _, mutation := range []string{"archived", "intro", "channel", "bound_channel", "completed", "issue_task", "assistant", "cli_jwt", "task_token", "pat", "cloud_pat", "channel_ingested", "kickoff", "missing", "wrong_session", "late", "duplicate"} {
		test.Run(mutation, func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			input, request := fixture.recordInput(), fixture.recordRequest()
			input.ParentID, input.EscalationClass = pgtype.UUID{}, "spend"
			switch mutation {
			case "archived":
				dbfx.Exec(test, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, fixture.sessionID)
			case "intro":
				dbfx.Exec(test, `UPDATE chat_session SET is_agent_intro = true WHERE id = $1`, fixture.sessionID)
			case "channel":
				dbfx.Exec(test, `UPDATE chat_session SET channel_origin_at = now() WHERE id = $1`, fixture.sessionID)
			case "bound_channel":
				installationID := dbfx.Insert(test, "channel_installation", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": fixture.agentID, "channel_type": "slack", "installer_user_id": testUserID})
				dbfx.Insert(test, "channel_chat_session_binding", testutil.Cols{"chat_session_id": fixture.sessionID, "installation_id": installationID, "channel_type": "slack", "channel_chat_id": "bound-" + fixture.sessionID, "chat_type": "p2p"})
			case "completed":
				dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.chatTaskID)
			case "issue_task":
				request.Header.Set("X-Task-ID", fixture.taskID)
			case "assistant":
				dbfx.Exec(test, `UPDATE chat_message SET role = 'assistant' WHERE id = $1`, fixture.messageID)
			case "cli_jwt", "task_token", "pat", "cloud_pat":
				dbfx.Exec(test, `UPDATE chat_message SET sent_via = $1 WHERE id = $2`, mutation, fixture.messageID)
			case "channel_ingested":
				dbfx.Exec(test, `UPDATE chat_message SET channel_ingested = true WHERE id = $1`, fixture.messageID)
			case "kickoff":
				dbfx.Exec(test, `UPDATE chat_message SET message_kind = 'kickoff' WHERE id = $1`, fixture.messageID)
			case "missing":
				dbfx.Exec(test, `DELETE FROM chat_message WHERE id = $1`, fixture.messageID)
			case "wrong_session":
				otherSession := dbfx.ChatSession(test, fixture.agentID)
				dbfx.Exec(test, `UPDATE chat_message SET chat_session_id = $1 WHERE id = $2`, otherSession, fixture.messageID)
			case "late":
				input.MessageIDs = []pgtype.UUID{parseUUID(dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": fixture.sessionID, "role": "user", "content": "late", "sent_via": "interactive_jwt"}))}
			case "duplicate":
				input.MessageIDs = append(input.MessageIDs, input.MessageIDs[0])
			}
			_, err := testHandler.createAskedDecisionRecord(request, fixture.issue, input)
			var refusal decisionIntentRefusal
			if !errors.As(err, &refusal) || dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1 AND origin = 'unprompted'`, fixture.issueID) != 0 {
				test.Fatalf("source refusal failed: %v", err)
			}
		})
	}
}

// A bare reply ("1", "yes") is unreadable on the issue unless the record also
// attests the options it chose between (CHE-1958).
func TestDecisionRecordAPIQuotesOfferedOptions(test *testing.T) {
	for _, mutation := range []string{"ok", "other_session", "user_message", "after_reply", "missing"} {
		test.Run(mutation, func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			offered := map[string]string{"chat_session_id": fixture.sessionID, "role": "assistant", "content": "1) Ship now\n2) Hold", "created_at": "2020-01-01T00:00:00Z"}
			switch mutation {
			case "other_session":
				offered["chat_session_id"] = dbfx.ChatSession(test, fixture.agentID)
			case "user_message":
				offered["role"] = "user"
			case "after_reply":
				delete(offered, "created_at")
			}
			cols := testutil.Cols{}
			for column, value := range offered {
				cols[column] = value
			}
			contextID := dbfx.Insert(test, "chat_message", cols)
			if mutation == "missing" {
				contextID = "11111111-1111-4111-8111-111111111111"
			}
			body := `{"decision_record":true,"message_ids":["` + fixture.messageID + `"],"escalation_class":"reversible","context_message_id":"` + contextID + `"}`
			request := fixture.recordRequest()
			request.Body = ioBody(body)
			response := httptest.NewRecorder()
			testHandler.CreateComment(response, withIssueParam(request, fixture.issueID))
			if mutation != "ok" {
				if response.Code != http.StatusBadRequest {
					test.Fatalf("context refusal: status=%d body=%s", response.Code, response.Body.String())
				}
				return
			}
			var comment db.Comment
			if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &comment) != nil {
				test.Fatalf("record with context: status=%d body=%s", response.Code, response.Body.String())
			}
			const wanted = "Options offered:\n> 1) Ship now\n> 2) Hold\n\nReply:\n>   Yes.\n> Keep the exact words.  "
			if comment.Content != wanted {
				test.Fatalf("quote lacks the offered options:\n%q", comment.Content)
			}
		})
	}
}
