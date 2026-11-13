package service

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// CHE-3113: an autopilot create_issue run starts the issue on its project
// workflow's start status, not a hard-coded todo.
func TestAutopilotCreateIssueUsesWorkflowStartStatus(t *testing.T) {
	for _, tc := range []struct {
		name        string
		startStatus any // NULL column when nil
		want        string
	}{
		{"workflow start status in_progress", "in_progress", "in_progress"},
		{"workflow without the field keeps todo", nil, "todo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx, owner := newPrincipalFixture(t)
			agentID := fx.privateAgentOwnedBy(t, owner, "start")
			workflowID := fx.Insert(t, "issue_workflow", testutil.Cols{
				"workspace_id":       fx.WorkspaceID,
				"name":               "start " + tc.name,
				"initial_status_key": "todo",
				"start_status_key":   tc.startStatus,
				"steps":              `[{"status_key":"todo"},{"status_key":"in_progress"},{"status_key":"done"},{"status_key":"cancelled"}]`,
			})
			projectID := fx.Project(t, "briefs", testutil.Cols{"workflow_id": workflowID})
			autopilotID := fx.Insert(t, "autopilot", testutil.Cols{
				"workspace_id":    fx.WorkspaceID,
				"project_id":      projectID,
				"title":           "brief",
				"assignee_type":   "agent",
				"assignee_id":     agentID,
				"status":          "active",
				"execution_mode":  "create_issue",
				"created_by_type": "member",
				"created_by_id":   owner,
			})
			triggerID := fx.trigger(t, autopilotID, "member", owner)

			run := fx.dispatch(t, autopilotID, triggerID)
			if run.Status == "skipped" || run.Status == "failed" {
				t.Fatalf("dispatch status = %q (%v)", run.Status, run.FailureReason)
			}

			var status, assigneeID string
			fx.QueryRow(t, `SELECT status, assignee_id::text FROM issue WHERE id = $1`, util.UUIDToString(run.IssueID)).Scan(&status, &assigneeID)
			if status != tc.want {
				t.Errorf("issue status = %q, want %q", status, tc.want)
			}
			if assigneeID != agentID {
				t.Errorf("assignee = %s, want the autopilot agent %s", assigneeID, agentID)
			}
			if n := fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, util.UUIDToString(run.IssueID)); n != 1 {
				t.Errorf("tasks for issue = %d, want exactly 1", n)
			}
		})
	}
}
