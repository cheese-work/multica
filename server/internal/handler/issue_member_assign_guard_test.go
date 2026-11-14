package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5516: an agent never parks an issue on a member. No agent run starts on a
// member-owned issue, so the work stops and nobody is told. The one exception is
// an open decision card on that issue: the human has an ask to answer.
func TestAgentCannotAssignIssueToMemberWithoutOpenCard_CHE5516(t *testing.T) {
	seedTestCatalog(t)
	memberBody := map[string]any{"assignee_type": "member", "assignee_id": testUserID}

	agentHeaders := func(t *testing.T, agentID, issueID string) map[string]string {
		taskID := createHandlerTestTaskForAgentOnIssue(t, agentID, issueID)
		return map[string]string{"X-Agent-ID": agentID, "X-Task-ID": taskID}
	}
	newIssue := func(t *testing.T) (agentID, issueID string) {
		agentID = createHandlerTestAgent(t, "member-assign guard", nil)
		issueID = dbfx.Issue(t, "member-assign guard", testutil.Cols{
			"status": "in_progress", "assignee_type": "agent", "assignee_id": agentID,
		})
		dbfx.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id = $1", issueID)
		return agentID, issueID
	}
	assigneeOf := func(t *testing.T, issueID string) string {
		_, aType, _ := issueStatusAndAssignee(t, issueID)
		return aType
	}

	for _, writer := range issueWriters {
		t.Run(writer.name+"/agent without card is refused", func(t *testing.T) {
			agentID, issueID := newIssue(t)
			resp := writer.write(t, issueID, memberBody, agentHeaders(t, agentID, issueID))
			if writer.name == "single" {
				resp.Want(http.StatusForbidden)
				var body struct {
					Error string `json:"error"`
				}
				resp.JSON(&body)
				for _, want := range []string{"decision request", "agent"} {
					if !strings.Contains(body.Error, want) {
						t.Fatalf("refusal %q must name %q", body.Error, want)
					}
				}
			}
			if got := assigneeOf(t, issueID); got != "agent" {
				t.Fatalf("assignee_type = %q, want the issue to stay with its agent", got)
			}
		})

		t.Run(writer.name+"/agent with an open card succeeds", func(t *testing.T) {
			agentID, issueID := newIssue(t)
			insertOpenHumanCard(t, issueID, agentID)
			writer.write(t, issueID, memberBody, agentHeaders(t, agentID, issueID)).Want(http.StatusOK)
			if got := assigneeOf(t, issueID); got != "member" {
				t.Fatalf("assignee_type = %q, want member", got)
			}
		})

		t.Run(writer.name+"/human assigns to a member freely", func(t *testing.T) {
			_, issueID := newIssue(t)
			writer.write(t, issueID, memberBody, nil).Want(http.StatusOK)
			if got := assigneeOf(t, issueID); got != "member" {
				t.Fatalf("assignee_type = %q, want member", got)
			}
		})
	}
}

func insertOpenHumanCard(t *testing.T, issueID, agentID string) {
	t.Helper()
	var id string
	dbfx.QueryRow(t, `INSERT INTO decision_request (workspace_id, issue_id, asking_agent_id, human_user_id, chat_session_id, ask, escalation_class, origin)
		VALUES ($1, $2, $3, $4, gen_random_uuid(), 'Pick', 'reversible', 'asked') RETURNING id::text`,
		testWorkspaceID, issueID, agentID, testUserID).Scan(&id)
	dbfx.Cleanup(t, `DELETE FROM decision_request WHERE id = $1`, id)
}
