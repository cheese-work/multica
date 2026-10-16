package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestDecisionVetoRouter(test *testing.T) {
	for _, role := range []string{"issue", "dm"} {
		test.Run(role, func(test *testing.T) {
			fixture := workspaceFixture(test)
			runtimeID := fixture.Runtime(test, "Veto runtime")
			agentID := fixture.Agent(test, "Veto agent", runtimeID, testutil.Cols{"visibility": "workspace", "permission_mode": "public_to"})
			fixture.InsertNoID(test, "agent_invocation_target", testutil.Cols{"agent_id": agentID, "target_type": "workspace", "target_id": testWorkspaceID}, "agent_id = $1", agentID)
			issueID := fixture.Issue(test, "Veto attestation", testutil.Cols{"assignee_type": "agent", "assignee_id": agentID})
			sessionID := fixture.ChatSession(test, agentID, testutil.Cols{"creator_id": testUserID})
			messageID := fixture.Insert(test, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "user", "content": "Exact human decision", "sent_via": "interactive_jwt"})
			chatTaskID := fixture.Task(test, agentID, testutil.Cols{"chat_session_id": sessionID, "runtime_id": runtimeID, "status": "running", "started_at": testutil.Raw("clock_timestamp()")})
			fixture.Cleanup(test, `DELETE FROM decision_request WHERE issue_id = $1`, issueID)
			call := func(token string, body map[string]any) *http.Response {
				test.Helper()
				encoded, _ := json.Marshal(body)
				request, _ := http.NewRequest(http.MethodPost, testServer.URL+"/api/issues/"+issueID+"/comments", bytes.NewReader(encoded))
				request.Header.Set("Authorization", "Bearer "+token)
				request.Header.Set("X-Workspace-ID", testWorkspaceID)
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("X-Actor-Source", "task_token")
				request.Header.Set("X-Agent-ID", agentID)
				request.Header.Set("X-Task-ID", chatTaskID)
				response, err := testServer.Client().Do(request)
				if err != nil {
					test.Fatal(err)
				}
				return response
			}
			response := call(mintAgentTaskToken(test, agentID, chatTaskID, testUserID), map[string]any{"decision_record": true, "message_ids": []string{messageID}, "escalation_class": "reversible"})
			if response.StatusCode != http.StatusCreated {
				test.Fatalf("record status=%d", response.StatusCode)
			}
			var quote map[string]any
			readJSON(test, response, &quote)
			body := map[string]any{"decision_veto": true, "parent_id": quote["id"], "content": "Wrong issue"}
			response = call(testToken, body)
			if response.StatusCode != http.StatusForbidden {
				test.Fatalf("forged veto provenance status=%d", response.StatusCode)
			}
			response.Body.Close()
			taskID := chatTaskID
			if role == "issue" {
				taskID = fixture.Task(test, agentID, testutil.Cols{"issue_id": issueID, "runtime_id": runtimeID, "status": "running"})
			}
			response = call(mintAgentTaskToken(test, agentID, taskID, testUserID), body)
			if response.StatusCode != http.StatusCreated {
				test.Fatalf("veto status=%d", response.StatusCode)
			}
			var veto map[string]any
			readJSON(test, response, &veto)
			if veto["decision_answer_id"] != quote["decision_answer_id"] || veto["decision_comment_kind"] != "veto" || veto["source_task_id"] != taskID || veto["parent_id"] != quote["id"] || fixture.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND disclaimed_by_task_id = $2 AND disclaimed_by_agent_id = $3 AND wake_at IS NULL AND veto_note_posted_at IS NULL`, quote["decision_answer_id"], taskID, agentID) != 1 {
				test.Fatalf("veto lost stamped provenance or delivered synchronously: %+v", veto)
			}
		})
	}
}
