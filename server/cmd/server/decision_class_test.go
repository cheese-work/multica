package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestDecisionRaiseClassRouter(test *testing.T) {
	for _, state := range []string{"pending", "scheduled", "woken", "withdrawn", "open", "cancelled", "lowering", "same", "other_human", "task_token", "invalid", "wrong_workspace"} {
		test.Run(state, func(test *testing.T) {
			fixture := workspaceFixture(test)
			runtimeID := fixture.Runtime(test, "Class runtime")
			agentID := fixture.Agent(test, "Class agent", runtimeID, testutil.Cols{"visibility": "workspace"})
			issueID := fixture.Issue(test, "Raise class")
			sessionID := fixture.ChatSession(test, agentID)
			taskID := fixture.Task(test, agentID, testutil.Cols{"chat_session_id": sessionID, "status": "running", "runtime_id": runtimeID})
			requestStatus, fromClass := "answered", "reversible"
			if state == "cancelled" || state == "open" {
				requestStatus = state
			}
			if state == "lowering" || state == "same" {
				fromClass = "deploy"
			}
			humanID := testUserID
			if state == "other_human" {
				humanID = fixture.User(test, "Other human", "class-other-"+issueID+"@test.local")
			}
			requestID := fixture.Insert(test, "decision_request", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": issueID, "asking_agent_id": agentID, "human_user_id": humanID, "chat_session_id": sessionID, "status": requestStatus, "origin": "asked", "escalation_class": fromClass})
			fixture.Cleanup(test, `DELETE FROM decision_request WHERE id = $1`, requestID)
			answerID := fixture.Insert(test, "decision_answer", testutil.Cols{"decision_request_id": requestID, "answered_by_task_id": taskID, "source_chat_session_id": sessionID, "chat_message_ids": testutil.Raw("ARRAY[gen_random_uuid()]"), "content_snapshot": "immutable", "effective_at": testutil.Raw("clock_timestamp() - interval '1 minute'")})
			if state == "scheduled" || state == "woken" {
				fixture.Exec(test, `UPDATE decision_answer SET wake_at = effective_at WHERE id = $1`, answerID)
			}
			if state == "woken" {
				fixture.Exec(test, `UPDATE decision_answer SET woken_at = now(), wake_outcome = 'run_enqueued' WHERE id = $1`, answerID)
			}
			if state == "withdrawn" {
				fixture.Exec(test, `UPDATE decision_answer SET disclaimed_at = now(), disclaim_reason = 'human' WHERE id = $1`, answerID)
			}
			class, wanted := "deploy", http.StatusOK
			if state == "lowering" {
				class = "reversible"
			}
			if state == "invalid" {
				class, wanted = "unknown", http.StatusBadRequest
			}
			if state == "cancelled" || state == "lowering" || state == "same" {
				wanted = http.StatusConflict
			}
			token, workspaceID := testToken, testWorkspaceID
			if state == "other_human" || state == "task_token" {
				wanted = http.StatusForbidden
			}
			if state == "task_token" {
				token = mintAgentTaskToken(test, agentID, taskID, testUserID)
			}
			if state == "wrong_workspace" {
				workspaceID, wanted = "11111111-1111-4111-8111-111111111111", http.StatusNotFound
			}
			request, _ := http.NewRequest(http.MethodPost, testServer.URL+"/api/decision-requests/"+requestID+"/class", strings.NewReader(`{"escalation_class":"`+class+`"}`))
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("X-Workspace-ID", workspaceID)
			response, err := testServer.Client().Do(request)
			if err != nil {
				test.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != wanted {
				test.Fatalf("raise status=%d want=%d", response.StatusCode, wanted)
			}
			wantedAudit := 0
			if wanted == http.StatusOK {
				wantedAudit = 1
			}
			if fixture.Count(test, `SELECT count(*) FROM decision_class_change WHERE decision_request_id = $1 AND from_class = $2 AND to_class = $3 AND changed_by_user_id = $4`, requestID, fromClass, class, testUserID) != wantedAudit {
				test.Fatal("missing or unexpected class audit")
			}
			var held, scheduled bool
			if err := testPool.QueryRow(context.Background(), `SELECT effective_at > clock_timestamp() + interval '9 minutes', wake_at IS NOT NULL FROM decision_answer WHERE id = $1`, answerID).Scan(&held, &scheduled); err != nil {
				test.Fatal(err)
			}
			shouldHold := wanted == http.StatusOK && state != "woken" && state != "withdrawn" && state != "open"
			if held != shouldHold || scheduled != (state == "scheduled" || state == "woken") {
				test.Fatalf("hold=%t want=%t scheduled=%t", held, shouldHold, scheduled)
			}
		})
	}
}
