package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func vetoDecisionHTTP(handler *Handler, fixture askedRecordFixture, parentID, reason string, issueTask bool) *httptest.ResponseRecorder {
	request := fixture.recordRequest()
	if issueTask {
		request.Header.Set("X-Task-ID", fixture.taskID)
	}
	body, _ := json.Marshal(map[string]any{"decision_veto": true, "parent_id": parentID, "content": reason})
	request.Body = ioBody(string(body))
	response := httptest.NewRecorder()
	handler.CreateComment(response, withIssueParam(request, fixture.issueID))
	return response
}

func TestDecisionVetoRoundTrip(test *testing.T) {
	for _, role := range []string{"issue", "dm", "dm_later_turn", "squad_leader", "squad_member"} {
		for _, origin := range []string{"asked", "unprompted"} {
			test.Run(role+"/"+origin, func(test *testing.T) {
				fixture := newAskedRecordFixture(test)
				input := fixture.recordInput()
				if origin == "unprompted" {
					input.ParentID, input.EscalationClass = pgtype.UUID{}, "reversible"
				}
				record, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, input)
				if err != nil {
					test.Fatal(err)
				}
				if role == "dm_later_turn" {
					dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.chatTaskID)
					fixture.chatTaskID = dbfx.Task(test, fixture.agentID, testutil.Cols{"status": "running", "chat_session_id": fixture.sessionID, "runtime_id": handlerTestRuntimeID(test)})
				}
				if strings.HasPrefix(role, "squad_") {
					leaderID := fixture.agentID
					if role == "squad_member" {
						leaderID = createHandlerTestAgent(test, "Veto leader", []byte("[]"))
					}
					squadID := dbfx.Insert(test, "squad", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Veto squad", "creator_id": testUserID, "leader_id": leaderID})
					dbfx.Exec(test, `INSERT INTO squad_member (squad_id, member_type, member_id, role) VALUES ($1, 'agent', $2, 'member')`, squadID, fixture.agentID)
					dbfx.Exec(test, `UPDATE issue SET assignee_type = 'squad', assignee_id = $1 WHERE id = $2`, squadID, fixture.issueID)
				}
				dbfx.Exec(test, `UPDATE decision_answer SET wake_at = now(), woken_at = now(), wake_outcome = 'run_enqueued' WHERE id = $1`, record.Answer.ID)
				reason := strings.Repeat("界", 610)
				response := vetoDecisionHTTP(testHandler, fixture, uuidToString(record.Comment.ID), reason, !strings.HasPrefix(role, "dm"))
				if response.Code != http.StatusCreated {
					test.Fatalf("veto status=%d body=%s", response.Code, response.Body.String())
				}
				var comment db.Comment
				if err := json.Unmarshal(response.Body.Bytes(), &comment); err != nil {
					test.Fatal(err)
				}
				answer, err := testHandler.Queries.GetDecisionAnswer(context.Background(), db.GetDecisionAnswerParams{WorkspaceID: fixture.issue.WorkspaceID, ID: record.Answer.ID})
				if err != nil {
					test.Fatal(err)
				}
				wantedReason, taskID := "agent_veto", fixture.taskID
				if strings.HasPrefix(role, "dm") {
					wantedReason, taskID = "record_retracted", fixture.chatTaskID
				}
				if !answer.DisclaimedAt.Valid || answer.DisclaimReason.String != wantedReason || answer.DisclaimedByAgentID != parseUUID(fixture.agentID) || answer.DisclaimedByTaskID != parseUUID(taskID) || answer.DisclaimedByUserID.Valid || answer.VetoReason.String != strings.Repeat("界", 600) || answer.WithdrawalCommentID != comment.ID {
					test.Fatalf("veto did not persist exact attribution/reason: %+v", answer)
				}
				if comment.DecisionAnswerID != answer.ID || comment.DecisionCommentKind.String != "veto" || comment.ParentID != record.Comment.ID || comment.AuthorID != parseUUID(fixture.agentID) || comment.SourceTaskID != parseUUID(taskID) || comment.Content != reason {
					test.Fatalf("veto is not a stamped reply: %+v", comment)
				}
				suppressed := role == "issue" || role == "squad_leader"
				if answer.RetractionDeliveredAt.Valid != suppressed || (answer.RetractionOutcome.String == "suppressed_agent_veto") != suppressed {
					test.Fatalf("wrong persisted self-recipient suppression: %+v", answer)
				}
				wantedStatus, cancelReason := "open", ""
				if origin == "unprompted" {
					wantedStatus, cancelReason = "cancelled", "record_vetoed"
				}
				if dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = $2 AND COALESCE(cancel_reason, '') = $3`, record.Request.ID, wantedStatus, cancelReason) != 1 || dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, record.Request.ID) != 1 {
					test.Fatal("veto changed the decision or failed its reopen/cancel guard")
				}
				if dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND content_snapshot = $2 AND source_chat_session_id = $3`, answer.ID, record.Answer.ContentSnapshot, record.Answer.SourceChatSessionID) != 1 || answer.VetoNotePostedAt.Valid {
					test.Fatal("veto changed source evidence or synchronously delivered the deferred DM note")
				}
			})
		}
	}
}

func TestDecisionVetoRefusals(test *testing.T) {
	for _, mutation := range []string{"pat", "cloud_pat", "interactive_jwt", "cli_jwt", "completed", "failed", "cancelled", "dm_completed", "dm_failed", "dm_cancelled", "other_issue", "non_assignee", "other_dm", "wrong_agent", "unstamped", "deleted", "missing", "withdrawn", "empty", "whitespace", "intent", "record", "messages", "class", "attachments", "suppression", "steering"} {
		test.Run(mutation, func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			record, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
			if err != nil {
				test.Fatal(err)
			}
			request := fixture.request()
			body := map[string]any{"decision_veto": true, "parent_id": uuidToString(record.Comment.ID), "content": "Wrong decision"}
			wanted := http.StatusForbidden
			switch mutation {
			case "pat", "cloud_pat", "interactive_jwt", "cli_jwt":
				request.Header.Set("X-Actor-Source", mutation)
			case "completed", "failed", "cancelled":
				dbfx.Exec(test, `UPDATE agent_task_queue SET status = $1 WHERE id = $2`, mutation, fixture.taskID)
			case "dm_completed", "dm_failed", "dm_cancelled":
				request.Header.Set("X-Task-ID", fixture.chatTaskID)
				dbfx.Exec(test, `UPDATE agent_task_queue SET status = $1 WHERE id = $2`, strings.TrimPrefix(mutation, "dm_"), fixture.chatTaskID)
			case "other_issue":
				dbfx.Exec(test, `UPDATE agent_task_queue SET issue_id = $1 WHERE id = $2`, dbfx.Issue(test, "Another issue"), fixture.taskID)
			case "non_assignee":
				dbfx.Exec(test, `UPDATE issue SET assignee_id = $1 WHERE id = $2`, createHandlerTestAgent(test, "Other assignee", []byte("[]")), fixture.issueID)
			case "other_dm":
				request.Header.Set("X-Task-ID", fixture.chatTaskID)
				dbfx.Exec(test, `UPDATE agent_task_queue SET chat_session_id = $1 WHERE id = $2`, dbfx.ChatSession(test, fixture.agentID), fixture.chatTaskID)
			case "wrong_agent":
				request.Header.Set("X-Agent-ID", createHandlerTestAgent(test, "Wrong agent", []byte("[]")))
			case "unstamped":
				body["parent_id"], wanted = fixture.tagID, http.StatusBadRequest
			case "deleted":
				dbfx.Exec(test, `UPDATE comment SET deleted_at = now() WHERE id = $1`, record.Comment.ID)
				wanted = http.StatusNotFound
			case "missing":
				body["parent_id"], wanted = testUserID, http.StatusNotFound
			case "withdrawn":
				dbfx.Exec(test, `UPDATE decision_answer SET disclaimed_at = now(), disclaim_reason = 'human' WHERE id = $1`, record.Answer.ID)
				wanted = http.StatusConflict
			case "empty", "whitespace":
				body["content"], wanted = "", http.StatusBadRequest
				if mutation == "whitespace" {
					body["content"] = " \n\t"
				}
			case "intent", "record":
				body["decision_"+mutation], wanted = true, http.StatusBadRequest
			case "messages", "class", "attachments", "suppression", "steering":
				key := map[string]string{"messages": "message_ids", "class": "escalation_class", "attachments": "attachment_ids", "suppression": "suppress_agent_ids", "steering": "steer_task_ids"}[mutation]
				body[key], wanted = []string{fixture.agentID}, http.StatusBadRequest
				if mutation == "class" {
					body[key] = "spend"
				}
			}
			before := dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id = $1`, fixture.issueID)
			encoded, _ := json.Marshal(body)
			request.Body = ioBody(string(encoded))
			response := httptest.NewRecorder()
			testHandler.CreateComment(response, withIssueParam(request, fixture.issueID))
			if response.Code != wanted || dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id = $1`, fixture.issueID) != before {
				test.Fatalf("veto refusal status=%d want=%d body=%s", response.Code, wanted, response.Body.String())
			}
		})
	}
}
