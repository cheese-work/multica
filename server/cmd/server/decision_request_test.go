package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestDecisionTaskTokenRoundTripThroughRouter(test *testing.T) {
	fixture := workspaceFixture(test)
	runtimeID := fixture.Runtime(test, "Decision runtime")
	agentID := fixture.Agent(test, "Decision requester", runtimeID, testutil.Cols{"visibility": "workspace", "permission_mode": "public_to"})
	fixture.InsertNoID(test, "agent_invocation_target", testutil.Cols{"agent_id": agentID, "target_type": "workspace", "target_id": testWorkspaceID}, "agent_id = $1", agentID)
	issueID := fixture.Issue(test, "Decision HTTP roundtrip", testutil.Cols{"assignee_type": "agent", "assignee_id": agentID})
	taskID := fixture.Task(test, agentID, testutil.Cols{"issue_id": issueID, "runtime_id": runtimeID, "status": "running"})
	sessionID := fixture.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": agentID, "creator_id": testUserID, "title": "Decisions"})
	fixture.Insert(test, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "user", "content": "see [issue](mention://issue/" + issueID + ")"})
	fixture.Cleanup(test, `DELETE FROM decision_intent WHERE issue_id = $1`, issueID)
	fixture.Cleanup(test, `DELETE FROM decision_request WHERE issue_id = $1`, issueID)
	fixture.Cleanup(test, `DELETE FROM inbox_item WHERE issue_id = $1`, issueID)
	fixture.Cleanup(test, `DELETE FROM issue_subscriber WHERE issue_id = $1`, issueID)
	token := mintAgentTaskToken(test, agentID, taskID, testUserID)
	call := func(method, path string, body any, authToken string, forged bool) *http.Response {
		test.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			test.Fatal(err)
		}
		request, err := http.NewRequest(method, testServer.URL+path, bytes.NewReader(encoded))
		if err != nil {
			test.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+authToken)
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
	response := call(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": "[@Human](mention://member/" + testUserID + ")", "decision_intent": true}, token, false)
	if response.StatusCode != http.StatusCreated {
		var body map[string]any
		readJSON(test, response, &body)
		test.Fatalf("task could not open intent: status=%d body=%+v", response.StatusCode, body)
	}
	response.Body.Close()
	sourceID := fixture.Insert(test, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": issueID, "author_type": "agent", "author_id": agentID, "source_task_id": taskID, "content": "Checks completed."})
	ask := decisiontx.Ask{ContextELI10: "A purchase is waiting. You decide whether to pay.", AgentDid: "Completed all the requested checks.", Decision: "Approve spend?", StakesIfWrong: "Money is spent for nothing.", Options: []decisiontx.AskOption{{Label: "Approve", WhatHappens: "Agents pay.", Pros: []string{"Unblocked"}, Cons: []string{"Costs money"}, CostOrTime: "10 USD"}, {Label: "Decline", WhatHappens: "No payment.", Pros: []string{"Free"}, Cons: []string{"Stays blocked"}, CostOrTime: "No extra time"}}, Recommendation: "Decline", DefaultIfNoReply: "Do not spend.", SourceIssueID: issueID, SourceCommentID: sourceID}
	input := map[string]any{"issue_id": issueID, "session_id": sessionID, "ask": ask, "escalation_class": "spend"}
	response = call(http.MethodPost, "/api/decisions", input, testToken, true)
	if response.StatusCode != http.StatusForbidden {
		test.Fatalf("forged task provenance admitted: status=%d", response.StatusCode)
	}
	response.Body.Close()
	response = call(http.MethodPost, "/api/decisions", input, token, false)
	var created db.DecisionRequest
	if response.StatusCode != http.StatusCreated {
		var body map[string]any
		readJSON(test, response, &body)
		test.Fatalf("task request failed: status=%d body=%+v", response.StatusCode, body)
	}
	readJSON(test, response, &created)
	if !created.PingNotifiedAt.Valid || created.Ask != ask.Content(integrationTestWorkspaceSlug) || created.SourceCommentID != toPgUUID(test, sourceID) {
		test.Fatalf("missing receipt or wrong ask: %+v", created)
	}
	var requestID string
	fixture.QueryRow(test, `SELECT id FROM decision_request WHERE issue_id = $1`, issueID).Scan(&requestID)
	response = call(http.MethodGet, "/api/decisions/"+requestID, nil, token, false)
	if response.StatusCode != http.StatusOK {
		test.Fatalf("task get failed: status=%d", response.StatusCode)
	}
	var fetched db.DecisionRequest
	readJSON(test, response, &fetched)
	if !reflect.DeepEqual(fetched, created) {
		test.Fatal("get did not roundtrip the committed request")
	}
	response = call(http.MethodGet, "/api/decisions?issue="+issueID, nil, token, false)
	if response.StatusCode != http.StatusOK {
		test.Fatalf("task list failed: status=%d", response.StatusCode)
	}
	var requests []db.DecisionRequest
	readJSON(test, response, &requests)
	if len(requests) != 1 || !reflect.DeepEqual(requests[0], created) {
		test.Fatalf("wrong list: %+v", requests)
	}
	if count := fixture.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1 AND recipient_id = $2`, issueID, testUserID); count != 1 {
		test.Fatalf("HTTP ask and generic mention path created %d notifications", count)
	}
	if count := fixture.Count(test, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, sessionID); count != 0 {
		test.Fatal("HTTP ping started a DM turn")
	}
	if _, err := db.New(testPool).ListDecisionPingRetries(context.Background(), 100); err != nil {
		test.Fatal(err)
	}
	response = call(http.MethodPost, "/api/decisions/"+requestID+"/withdraw", map[string]any{"reason": "Superseded."}, token, false)
	if response.StatusCode != http.StatusOK {
		var body map[string]any
		readJSON(test, response, &body)
		test.Fatalf("task withdrawal route: status=%d body=%+v", response.StatusCode, body)
	}
	var withdrawn db.DecisionRequest
	readJSON(test, response, &withdrawn)
	if withdrawn.Status != "cancelled" || withdrawn.Ask != created.Ask || withdrawn.WithdrawnByTaskID != toPgUUID(test, taskID) {
		test.Fatalf("wrong authenticated withdrawal: %+v", withdrawn)
	}
}

func TestDecisionRoutesThroughRouter(test *testing.T) {
	fixture := workspaceFixture(test)
	issueID := fixture.Issue(test, "Decision routing")
	response := authRequest(test, http.MethodGet, "/api/decisions?issue="+issueID, nil)
	if response.StatusCode != http.StatusOK {
		test.Fatalf("decision list route: status=%d", response.StatusCode)
	}
	var requests []map[string]any
	readJSON(test, response, &requests)
	if requests == nil || len(requests) != 0 {
		test.Fatalf("empty list contract: %+v", requests)
	}
	response = authRequest(test, http.MethodGet, "/api/decisions/"+uuid.NewString(), nil)
	if response.StatusCode != http.StatusNotFound {
		test.Fatalf("decision get route: status=%d", response.StatusCode)
	}
	response.Body.Close()
	response = authRequest(test, http.MethodPost, "/api/decisions", map[string]any{
		"issue_id": issueID, "session_id": uuid.NewString(), "ask": "Choose", "escalation_class": "spend",
	})
	if response.StatusCode != http.StatusForbidden {
		test.Fatalf("interactive caller admitted to task-only request: status=%d", response.StatusCode)
	}
	response.Body.Close()
}

func TestDecisionRequestCommentSkipsDuplicateTargetNotification(test *testing.T) {
	fixture := workspaceFixture(test)
	issueID := fixture.Issue(test, "Decision notifications")
	targetID := fixture.User(test, "Decision target", "decision-target@example.test")
	otherID := fixture.User(test, "Other subscriber", "decision-subscriber@example.test")
	fixture.Member(test, testWorkspaceID, targetID, "member")
	fixture.Member(test, testWorkspaceID, otherID, "member")
	fixture.Cleanup(test, `DELETE FROM inbox_item WHERE issue_id = $1`, issueID)
	fixture.Cleanup(test, `DELETE FROM issue_subscriber WHERE issue_id = $1`, issueID)
	addTestSubscriber(test, issueID, "member", targetID, "mentioned")
	addTestSubscriber(test, issueID, "member", otherID, "manual")
	queries := db.New(testPool)
	bus := newNotificationBus(test, queries)
	bus.Publish(events.Event{
		Type: protocol.EventCommentCreated, WorkspaceID: testWorkspaceID, ActorType: "agent", ActorID: uuid.NewString(),
		Payload: map[string]any{
			"comment":     handler.CommentResponse{ID: uuid.NewString(), IssueID: issueID, AuthorType: "agent", Content: "[@Target](mention://member/" + targetID + ") Choose"},
			"issue_title": "Decision notifications", "issue_status": "todo", "decision_intent_targets": []string{targetID},
		},
	})
	if items := inboxItemsForRecipient(test, queries, targetID); len(items) != 0 {
		test.Fatalf("target got %d generic inbox rows in addition to its receipt-owned ping", len(items))
	}
	if items := inboxItemsForRecipient(test, queries, otherID); len(items) != 1 || items[0].Type != "new_comment" {
		test.Fatalf("unrelated subscriber notification changed: %+v", items)
	}
}
