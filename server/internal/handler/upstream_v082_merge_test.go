package handler

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
)

// CHE-5320 merge preservation (upstream v0.8.2 into the fork). Where both
// sides changed a feature the fork wins; upstream's addition rides along.

// Upstream added a project filter to search; the fork rewrote the same query
// for diacritic folding. The merged builder keeps both.
func TestSearchKeepsDiacriticRankWithProjectFilter(t *testing.T) {
	project := pgtype.UUID{Bytes: uuid.MustParse("11111111-2222-3333-4444-555555555555"), Valid: true}
	sql, args := buildSearchQuery("đăng nhập", splitSearchTerms("đăng nhập"), 0, false, false, nil, project)
	if !strings.Contains(sql, "content_accents") {
		t.Errorf("project-filtered accented query lost the fork diacritic rank:\n%s", sql)
	}
	if !strings.Contains(sql, "i.project_id = $") {
		t.Errorf("accented query lost upstream's project filter:\n%s", sql)
	}
	var found bool
	for _, a := range args {
		found = found || a == project
	}
	if !found {
		t.Errorf("project id not bound as an argument: %#v", args)
	}
}

// Upstream's deferred workflow-squad handoff row must still pass the fork's run
// budget. Inserting it straight (the naive take-upstream shape) skips the gate
// and the debit, so a spent budget would keep producing leader runs.
func TestWorkflowSquadHandoffStaysBehindRunBudget(t *testing.T) {
	seedTestCatalog(t)
	fx := newSquadBriefingClaimFixture(t, t.Context(), "Budgeted squad handoff")
	wf := createTestWorkflow(t, "Budgeted flow "+workflowTestSuffix(), "in_progress")
	projectID := createWorkflowTestProject(t, "Budgeted squad project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	setWorkflowHandoff(t, wf.ID, "in_progress", map[string]any{"handler": map[string]any{"type": "squad", "id": fx.SquadID}}).Want(http.StatusOK)
	dbfx.Exec(t, `UPDATE issue_workflow SET max_runs_per_issue=1, run_budget_since=now()-interval '1 hour' WHERE id=$1`, wf.ID)
	dbfx.Exec(t, `UPDATE issue SET project_id=$2,status='todo' WHERE id=$1`, fx.IssueID, projectID)
	restore := withoutRunBudgetWriters()
	defer restore()

	updateIssueForTest(t, fx.IssueID, map[string]any{"status": "in_progress"}).Want(http.StatusOK)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=clock_timestamp() WHERE issue_id=$1`, fx.IssueID)

	issue, err := testHandler.Queries.GetIssue(t.Context(), parseUUID(fx.IssueID))
	if err != nil {
		t.Fatal(err)
	}
	_, err = testHandler.TaskService.EnqueueTaskForWorkflowSquadLeader(t.Context(), issue, parseUUID(fx.AgentID), parseUUID(fx.SquadID), "", parseUUID(testUserID))
	if !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("handoff past the run budget = %v, want ErrRunBudgetExhausted", err)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, fx.IssueID); n != 1 {
		t.Fatalf("handoff past the run budget wrote a task: %d rows, want 1", n)
	}
}
