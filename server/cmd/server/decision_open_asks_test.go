package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestOpenAsksListsOnlyOpenRequestsWithoutAnswersForAnyWorkspaceCaller(test *testing.T) {
	fixture := workspaceFixture(test)
	runtimeID := fixture.Runtime(test, "Open asks runtime")
	agentID := fixture.Agent(test, "Open asks agent", runtimeID)
	issueID := fixture.Issue(test, "Open asks issue")
	otherHuman := fixture.User(test, "Other human", uuid.NewString()+"@example.test")
	fixture.Member(test, testWorkspaceID, otherHuman, "member")
	sessionID := fixture.Insert(test, "chat_session", testutil.Cols{
		"workspace_id": testWorkspaceID, "agent_id": agentID, "creator_id": testUserID, "title": "Asks",
	})
	base := time.Now().Add(-48 * time.Hour).UTC()
	insert := func(humanID, status, ask string, age int) string {
		return fixture.Insert(test, "decision_request", testutil.Cols{
			"workspace_id": testWorkspaceID, "issue_id": issueID, "asking_agent_id": agentID,
			"human_user_id": humanID, "chat_session_id": sessionID,
			"ask": ask, "escalation_class": "spend", "status": status, "origin": "asked",
			"ask_json":   `{"default_if_no_reply":"Keep the current plan","context_eli10":"SECRET-CONTEXT"}`,
			"created_at": base.Add(time.Duration(age) * time.Hour),
		})
	}
	newerID := insert(testUserID, "open", "New ask\nSECOND-LINE-QUOTE", 2)
	olderID := insert(testUserID, "open", "Old ask", 1)
	insert(otherHuman, "open", "Another human's ask", 0)
	for _, status := range []string{"answered", "cancelled", "disclaimed"} {
		insert(testUserID, status, "Closed ask "+status, 0)
	}
	taskID := fixture.Task(test, agentID, testutil.Cols{"status": "running", "runtime_id": runtimeID})
	taskToken := mintAgentTaskToken(test, agentID, taskID, testUserID)
	outsiderID := fixture.User(test, "Outsider", uuid.NewString()+"@example.test")
	outsiderToken, err := generateTestJWT(outsiderID, "outsider@example.test", "Outsider")
	if err != nil {
		test.Fatal(err)
	}
	path := "/api/decisions/open-asks?for=" + testUserID
	for name, token := range map[string]string{"member": testToken, "task token": taskToken} {
		test.Run(name, func(test *testing.T) {
			status, body := chatRequestWithToken(test, token, path)
			if status != http.StatusOK {
				test.Fatalf("status=%d body=%s", status, body)
			}
			var rows []map[string]any
			if err := json.Unmarshal(body, &rows); err != nil {
				test.Fatal(err)
			}
			if len(rows) != 2 || rows[0]["id"] != olderID || rows[1]["id"] != newerID {
				test.Fatalf("rows=%v, want open asks [%s %s] oldest first", rows, olderID, newerID)
			}
			for _, key := range []string{"issue_identifier", "ask", "escalation_class", "created_at", "card_link", "asking_agent_id", "default_if_no_reply"} {
				if rows[0][key] == nil || rows[0][key] == "" {
					test.Fatalf("row lacks %s: %v", key, rows[0])
				}
			}
			if rows[0]["default_if_no_reply"] != "Keep the current plan" {
				test.Fatalf("default_if_no_reply=%v", rows[0]["default_if_no_reply"])
			}
			if rows[1]["ask"] != "New ask" {
				test.Fatalf("ask must be the first line only: %q", rows[1]["ask"])
			}
			for _, leak := range []string{"SECRET-CONTEXT", "SECOND-LINE-QUOTE", "ask_json", "options", "answer"} {
				if strings.Contains(string(body), leak) {
					test.Fatalf("open-asks read leaks %q: %s", leak, body)
				}
			}
		})
	}
	status, body := chatRequestWithToken(test, outsiderToken, path)
	if status == http.StatusOK {
		test.Fatalf("non-member read open asks: %s", body)
	}
	status, body = chatRequestWithToken(test, testToken, "/api/decisions/open-asks?for="+otherHuman)
	if status != http.StatusForbidden {
		test.Fatalf("a member read another member's open asks: status=%d body=%s", status, body)
	}
	status, body = chatRequestWithToken(test, taskToken, "/api/decisions/open-asks?for="+otherHuman)
	if status != http.StatusOK || !strings.Contains(string(body), "Another human's ask") {
		test.Fatalf("task token must read any member's open asks: status=%d body=%s", status, body)
	}
	if status, _ = chatRequestWithToken(test, testToken, "/api/decisions/open-asks"); status != http.StatusBadRequest {
		test.Fatalf("missing for: status=%d, want 400", status)
	}
}
