package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The run prompt tells the agent its budget (CHE-2297 L1b): runs left, tokens
// used and runs that reported no usage, read from GetIssueUsageSummary.
func TestClaimBriefShowsRunBudget(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 2311001, mergeEdgeSteps)
	agentID := seededReadyAgentID(t)
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `UPDATE issue_workflow SET max_runs_per_issue=5, run_budget_since=now()-interval '1 hour' WHERE id=$1`, c.workflowID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE issue SET status='in_progress', assignee_type='agent', assignee_id=$2 WHERE id=$1`, c.issueID, agentID); err != nil {
		t.Fatal(err)
	}
	runtimeID := handlerTestRuntimeID(t)
	for _, reported := range []bool{true, false} {
		var taskID string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, context, started_at, completed_at)
			VALUES ($1, $2, $3, 'completed', '{}'::jsonb, now(), now()) RETURNING id`, agentID, runtimeID, c.issueID).Scan(&taskID); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id=$1`, taskID) })
		if reported {
			if _, err := testPool.Exec(ctx, `INSERT INTO task_usage (task_id, provider, model, input_tokens, output_tokens) VALUES ($1, 'anthropic', 'm', 1200, 300)`, taskID); err != nil {
				t.Fatal(err)
			}
		}
	}

	issue, err := testHandler.Queries.GetIssue(ctx, util.MustParseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	got := testHandler.claimProjectWorkflow(ctx, issue, &db.AgentTaskQueue{AgentID: util.MustParseUUID(agentID)})
	if got == nil {
		t.Fatal("claim carries no workflow")
	}
	const want = "Run budget: 3 runs left of 5; 1500 tokens used (1 run unreported)."
	if !strings.Contains(got.Brief, want) {
		t.Fatalf("brief is missing %q:\n%s", want, got.Brief)
	}
}

// A budgeted Default workflow with no handoffs still enforces its budget at
// admission, so its claim carries the budget line alone, with no workflow text.
func TestClaimBriefShowsRunBudgetForDefaultWithoutHandoffs(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	seedTestCatalog(t)
	resetDefaultWorkflow(t)
	var list struct {
		Workflows []IssueWorkflowResponse `json:"workflows"`
	}
	testutil.Call(t, testHandler.ListIssueWorkflows, workflowRequest("GET", "/api/issue-workflows", nil)).Want(http.StatusOK).JSON(&list)
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `UPDATE issue_workflow SET max_runs_per_issue=5, run_budget_since=now()-interval '1 hour' WHERE id=$1`, list.Workflows[0].ID); err != nil {
		t.Fatal(err)
	}
	project := createWorkflowTestProject(t, "Budgeted default project")
	issueID := dbfx.Issue(t, "budgeted default", testutil.Cols{"project_id": project, "status": "todo"})
	issue, err := testHandler.Queries.GetIssue(ctx, util.MustParseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	got := testHandler.claimProjectWorkflow(ctx, issue, nil)
	if got == nil {
		t.Fatal("a budgeted Default workflow must carry its budget line")
	}
	if want := "Run budget: 5 runs left of 5; 0 tokens used (0 runs unreported).\n"; got.Brief != want {
		t.Fatalf("brief = %q, want only %q", got.Brief, want)
	}
}
