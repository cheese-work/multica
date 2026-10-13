package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
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
