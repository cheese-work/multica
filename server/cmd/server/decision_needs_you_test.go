package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestDecisionLifecycleNotesDoNotChangeChatActivity(test *testing.T) {
	fixture := workspaceFixture(test)
	agentID := fixture.Agent(test, "Lifecycle agent", "")
	base := time.Now().Add(-8 * time.Hour).UTC()
	createSession := func(title string) string {
		return fixture.Insert(test, "chat_session", testutil.Cols{
			"workspace_id": testWorkspaceID, "agent_id": agentID, "creator_id": testUserID,
			"title": title, "created_at": base, "updated_at": base, "last_read_at": base,
			"explicitly_created_at": base,
		})
	}
	asks := createSession("Asks")
	recent := createSession("Recent conversation")
	notesOnly := createSession("Notes only")
	message := func(sessionID, kind, content string, hours int) {
		fixture.Insert(test, "chat_message", testutil.Cols{
			"chat_session_id": sessionID, "role": "assistant", "message_kind": kind,
			"content": content, "created_at": base.Add(time.Duration(hours) * time.Hour),
		})
	}
	message(asks, "decision_request", "Original ask", 1)
	message(recent, "message", "Recent reply", 2)
	for _, kind := range []string{"decision_cancel_notice", "decision_reference"} {
		message(asks, kind, "Lifecycle note", 3)
		message(notesOnly, kind, "Lifecycle note", 4)
	}
	fixture.Exec(test, `UPDATE chat_session SET updated_at = $1 WHERE id IN ($2, $3)`, base.Add(5*time.Hour), asks, notesOnly)
	for _, path := range []string{"/api/chat/sessions", "/api/chat/sessions?status=all"} {
		test.Run(path, func(test *testing.T) {
			status, body := interactiveChatRequest(test, path)
			if status != http.StatusOK {
				test.Fatalf("session list: status=%d body=%s", status, body)
			}
			var rows []struct {
				ID          string `json:"id"`
				UnreadCount int    `json:"unread_count"`
				LastMessage *struct {
					Content string `json:"content"`
				} `json:"last_message"`
			}
			if err := json.Unmarshal(body, &rows); err != nil {
				test.Fatal(err)
			}
			positions := map[string]int{}
			for index, row := range rows {
				positions[row.ID] = index
				if row.ID == asks && (row.UnreadCount != 1 || row.LastMessage == nil || row.LastMessage.Content != "Original ask") {
					test.Fatalf("lifecycle note changed ask activity: %+v", row)
				}
				if row.ID == notesOnly && (row.UnreadCount != 0 || row.LastMessage != nil) {
					test.Fatalf("lifecycle-only DM has unread or preview: %+v", row)
				}
			}
			for _, sessionID := range []string{asks, recent, notesOnly} {
				if _, found := positions[sessionID]; !found {
					test.Fatalf("session %s missing from list", sessionID)
				}
			}
			if positions[asks] < positions[recent] || positions[notesOnly] < positions[recent] {
				test.Fatalf("lifecycle note bumped a DM: positions=%v", positions)
			}
		})
	}
}

func TestNeedsYouListsOpenAsksForCurrentHuman(test *testing.T) {
	fixture := workspaceFixture(test)
	agentID := fixture.Agent(test, "Asking agent", "")
	issueID := fixture.Issue(test, "Needs you issue")
	otherHuman := fixture.User(test, "Other human", uuid.NewString()+"@example.test")
	fixture.Member(test, testWorkspaceID, otherHuman, "member")
	sessionID := fixture.Insert(test, "chat_session", testutil.Cols{
		"workspace_id": testWorkspaceID, "agent_id": agentID, "creator_id": testUserID, "title": "Asks",
	})
	messageID := fixture.Insert(test, "chat_message", testutil.Cols{
		"chat_session_id": sessionID, "role": "assistant", "message_kind": "decision_request", "content": "Old ask",
	})
	base := time.Now().Add(-48 * time.Hour).UTC()
	insert := func(humanID, workspaceID, status, ask string, age int, ping any) string {
		return fixture.Insert(test, "decision_request", testutil.Cols{
			"workspace_id": workspaceID, "issue_id": issueID, "asking_agent_id": agentID,
			"human_user_id": humanID, "chat_session_id": sessionID, "ping_message_id": ping,
			"ask": ask, "escalation_class": "spend", "status": status, "origin": "asked",
			"created_at": base.Add(time.Duration(age) * time.Hour),
		})
	}
	newerID := insert(testUserID, testWorkspaceID, "open", "New ask", 2, nil)
	olderID := insert(testUserID, testWorkspaceID, "open", "Old ask", 1, messageID)
	otherID := insert(otherHuman, testWorkspaceID, "open", "Another human's ask", 0, nil)
	foreignWorkspace := fixture.Workspace(test, "Another workspace", "needs-you-"+uuid.NewString())
	foreignFixture := testutil.New(testPool, foreignWorkspace, testUserID)
	foreignAgent := foreignFixture.Agent(test, "Foreign agent", "")
	foreignIssue := foreignFixture.Issue(test, "Foreign issue")
	foreignFixture.Insert(test, "decision_request", testutil.Cols{
		"workspace_id": foreignWorkspace, "issue_id": foreignIssue, "asking_agent_id": foreignAgent,
		"human_user_id": testUserID, "chat_session_id": sessionID, "ask": "Another workspace's ask",
		"escalation_class": "spend", "status": "open", "origin": "asked",
	})
	for _, status := range []string{"answered", "cancelled", "disclaimed"} {
		insert(testUserID, testWorkspaceID, status, "Closed ask", 0, nil)
	}
	for index := 0; index < 60; index++ {
		fixture.Insert(test, "chat_message", testutil.Cols{
			"chat_session_id": sessionID, "role": "user", "content": "Later conversation",
		})
	}
	read := func(token string, wantIDs []string) {
		test.Helper()
		request, err := http.NewRequest(http.MethodGet, testServer.URL+"/api/decisions/open?human_user_id="+otherHuman, nil)
		if err != nil {
			test.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("X-Workspace-ID", testWorkspaceID)
		response, err := testServer.Client().Do(request)
		if err != nil {
			test.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			test.Fatalf("Needs you list: status=%d, want 200", response.StatusCode)
		}
		var rows []struct {
			ID              string  `json:"id"`
			IssueID         string  `json:"issue_id"`
			IssueIdentifier string  `json:"issue_identifier"`
			AskingAgentID   string  `json:"asking_agent_id"`
			AskingAgentName string  `json:"asking_agent_name"`
			Ask             string  `json:"ask"`
			Class           string  `json:"escalation_class"`
			SessionID       string  `json:"chat_session_id"`
			MessageID       *string `json:"message_id"`
		}
		readJSON(test, response, &rows)
		if len(rows) != len(wantIDs) {
			test.Fatalf("Needs you rows=%+v, want IDs %v", rows, wantIDs)
		}
		for index, wanted := range wantIDs {
			row := rows[index]
			if row.ID != wanted || row.IssueID != issueID || row.IssueIdentifier == "" || row.AskingAgentID != agentID || row.AskingAgentName != "Asking agent" || row.Class != "spend" || row.SessionID != sessionID {
				test.Fatalf("Needs you row=%+v, want ID %s with issue, agent, class and DM", row, wanted)
			}
			if wanted == olderID && (row.MessageID == nil || *row.MessageID != messageID || row.Ask != "Old ask") {
				test.Fatalf("old ask lost its headline or jump target: %+v", row)
			}
		}
	}
	read(testToken, []string{olderID, newerID})
	request, err := http.NewRequest(http.MethodPost, testServer.URL+"/api/chat/sessions/"+sessionID+"/read", nil)
	if err != nil {
		test.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+interactiveToken(test))
	request.Header.Set("X-Workspace-ID", testWorkspaceID)
	response, err := testServer.Client().Do(request)
	if err != nil {
		test.Fatal(err)
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNoContent {
		response.Body.Close()
		test.Fatalf("mark read: status=%d", response.StatusCode)
	}
	response.Body.Close()
	read(testToken, []string{olderID, newerID})
	otherToken, err := generateTestJWT(otherHuman, "other@example.test", "Other human")
	if err != nil {
		test.Fatal(err)
	}
	read(otherToken, []string{otherID})
	for _, status := range []string{"answered", "cancelled", "disclaimed"} {
		fixture.Exec(test, `UPDATE decision_request SET status = $1 WHERE id = $2`, status, olderID)
		read(testToken, []string{newerID})
	}
}

func TestNeedsYouRejectsMachineActorsAndOutsiders(test *testing.T) {
	fixture := workspaceFixture(test)
	runtimeID := fixture.Runtime(test, "Needs you auth runtime")
	agentID := fixture.Agent(test, "Needs you auth agent", runtimeID)
	taskID := fixture.Task(test, agentID, testutil.Cols{"status": "running", "runtime_id": runtimeID})
	agentToken := mintAgentTaskToken(test, agentID, taskID, testUserID)
	outsiderID := fixture.User(test, "Outsider", uuid.NewString()+"@example.test")
	outsiderToken, err := generateTestJWT(outsiderID, "outsider@example.test", "Outsider")
	if err != nil {
		test.Fatal(err)
	}
	for name, token := range map[string]string{"agent": agentToken, "outsider": outsiderToken, "anonymous": ""} {
		test.Run(name, func(test *testing.T) {
			status, body := chatRequestWithToken(test, token, "/api/decisions/open")
			if status != http.StatusForbidden && status != http.StatusNotFound && status != http.StatusUnauthorized {
				test.Fatalf("%s read private open asks: status=%d body=%s", name, status, body)
			}
		})
	}
}
