package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestDecisionRecordRouter(test *testing.T) {
	fixture := workspaceFixture(test)
	runtimeID := fixture.Runtime(test, "Recorder runtime")
	agentID := fixture.Agent(test, "Recorder", runtimeID, testutil.Cols{"visibility": "workspace", "permission_mode": "public_to"})
	fixture.InsertNoID(test, "agent_invocation_target", testutil.Cols{"agent_id": agentID, "target_type": "workspace", "target_id": testWorkspaceID}, "agent_id = $1", agentID)
	issueID := fixture.Issue(test, "Unprompted router")
	sessionID := fixture.ChatSession(test, agentID, testutil.Cols{"creator_id": testUserID})
	messageID := fixture.Insert(test, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "user", "content": "Exact human words", "sent_via": "interactive_jwt"})
	taskID := fixture.Task(test, agentID, testutil.Cols{"chat_session_id": sessionID, "runtime_id": runtimeID, "status": "running", "started_at": testutil.Raw("clock_timestamp()")})
	fixture.Cleanup(test, `DELETE FROM decision_request WHERE issue_id = $1`, issueID)
	fixture.Cleanup(test, `DELETE FROM inbox_item WHERE issue_id = $1`, issueID)
	fixture.Cleanup(test, `DELETE FROM issue_subscriber WHERE issue_id = $1`, issueID)
	encoded, _ := json.Marshal(map[string]any{"decision_record": true, "message_ids": []string{messageID}, "escalation_class": "spend"})
	call := func(token string, forged bool) *http.Response {
		test.Helper()
		request, _ := http.NewRequest(http.MethodPost, testServer.URL+"/api/issues/"+issueID+"/comments", bytes.NewReader(encoded))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("X-Workspace-ID", testWorkspaceID)
		request.Header.Set("Content-Type", "application/json")
		if forged {
			request.Header.Set("X-Actor-Source", "task_token")
			request.Header.Set("X-Agent-ID", agentID)
			request.Header.Set("X-Task-ID", taskID)
		}
		response, err := testServer.Client().Do(request)
		if err != nil {
			test.Fatal(err)
		}
		return response
	}
	response := call(testToken, true)
	if response.StatusCode != http.StatusForbidden {
		test.Fatalf("forged record provenance status=%d", response.StatusCode)
	}
	response.Body.Close()
	response = call(mintAgentTaskToken(test, agentID, taskID, testUserID), false)
	if response.StatusCode != http.StatusCreated {
		var body map[string]any
		readJSON(test, response, &body)
		test.Fatalf("record status=%d body=%+v", response.StatusCode, body)
	}
	var quote struct {
		DecisionAnswerID    *string `json:"decision_answer_id"`
		DecisionCommentKind *string `json:"decision_comment_kind"`
		Content             string  `json:"content"`
		AuthorID            string  `json:"author_id"`
		SourceTaskID        *string `json:"source_task_id"`
	}
	readJSON(test, response, &quote)
	if quote.DecisionAnswerID == nil || quote.DecisionCommentKind == nil || *quote.DecisionCommentKind != "quote" || quote.Content != "> Exact human words" || quote.AuthorID != agentID || quote.SourceTaskID == nil || *quote.SourceTaskID != taskID {
		test.Fatalf("router lost attestation: %+v", quote)
	}
	if fixture.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1 AND origin = 'unprompted' AND status = 'answered'`, issueID) != 1 || fixture.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID) != 0 {
		test.Fatal("record did not stay pending without an issue run")
	}
}
