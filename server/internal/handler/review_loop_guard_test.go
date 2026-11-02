package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// Review-loop guards (CHE-3243): a CHANGES verdict needs a P0/P1 finding, the
// fourth one is turned into a decision request, and a review is not dispatched
// for a head that already has a completed review.

var reviewLoopSteps = []map[string]any{
	{"status_key": "in_progress"},
	{"status_key": "agent_review", "edges": []map[string]any{
		{"to": "in_progress", "guard": "agent_verdict(CHANGES)"},
		{"to": "in_review", "guard": "agent_verdict(PASS)"},
	}},
	{"status_key": "need_decision"},
	{"status_key": "in_review"},
	{"status_key": "blocked"},
}

type reviewLoop struct {
	agentID, runtimeID, issueID, projectID, run string
}

func newReviewLoop(t *testing.T) reviewLoop {
	t.Helper()
	createTestCustomStatus(t, "agent_review", "started")
	createTestCustomStatus(t, "need_decision", "started")
	agentID := seededReadyAgentID(t)
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID)
	wf := createTestWorkflowSteps(t, "Review loop "+workflowTestSuffix(), reviewLoopSteps)
	projectID := createWorkflowTestProject(t, "Review loop project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	setColumnHandoff(t, projectID, "agent_review", handToAgent(agentID, "Review it.")).Want(http.StatusOK)
	issueID := dbfx.Issue(t, "review loop", testutil.Cols{
		"project_id": projectID, "status": "agent_review", "assignee_type": "agent", "assignee_id": agentID,
	})
	run := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "workflow_step": "agent_review",
		"dispatched_at": testutil.Raw("now() - interval '5 minutes'"), "started_at": testutil.Raw("now() - interval '4 minutes'"),
	})
	return reviewLoop{agentID, runtimeID, issueID, projectID, run}
}

func (l reviewLoop) verdict(t *testing.T, status string, findings ...map[string]any) *testutil.Response {
	t.Helper()
	body := map[string]any{"status": status}
	if findings != nil {
		body["review_findings"] = findings
	}
	return agentUpdateIssue(t, l.issueID, l.agentID, l.run, body)
}

func (l reviewLoop) priorChangesRounds(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		dbfx.Exec(t, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details)
			SELECT workspace_id, id, 'agent', $2, 'review_changes_requested', '{}'::jsonb FROM issue WHERE id = $1`, l.issueID, l.agentID)
	}
}

func finding(severity string) map[string]any {
	return map[string]any{"severity": severity, "summary": "finding " + severity}
}

func TestReviewLoop_ChangesVerdictWithoutP0P1IsRejected(t *testing.T) {
	l := newReviewLoop(t)
	for name, findings := range map[string][]map[string]any{
		"no findings":  nil,
		"only P2 / P3": {finding("P2"), finding("P3")},
		"bad severity": {finding("P9")},
	} {
		body := l.verdict(t, "in_progress", findings...).Want(http.StatusBadRequest).Map()
		if body["code"] != "review_verdict_findings_required" {
			t.Fatalf("%s: refusal = %v", name, body)
		}
		if status, _, _ := issueStatusAndAssignee(t, l.issueID); status != "agent_review" {
			t.Fatalf("%s: issue status after refused verdict = %q, want agent_review", name, status)
		}
	}
}

func TestReviewLoop_ChangesVerdictWithP1MovesBackAndIsRecorded(t *testing.T) {
	l := newReviewLoop(t)
	l.verdict(t, "in_progress", finding("P2"), finding("P1")).Want(http.StatusOK)
	if status, _, _ := issueStatusAndAssignee(t, l.issueID); status != "in_progress" {
		t.Fatalf("issue status = %q, want in_progress", status)
	}
	var details string
	dbfx.QueryRow(t, `SELECT details::text FROM activity_log WHERE issue_id = $1 AND action = 'review_changes_requested'`, l.issueID).Scan(&details)
	if details == "" || !strings.Contains(details, "P1") {
		t.Fatalf("activity details %q lack the findings", details)
	}
}

func TestReviewLoop_PassVerdictNeedsNoFindings(t *testing.T) {
	l := newReviewLoop(t)
	l.verdict(t, "in_review").Want(http.StatusOK)
	if status, _, _ := issueStatusAndAssignee(t, l.issueID); status != "in_review" {
		t.Fatalf("issue status = %q, want in_review", status)
	}
}

func TestReviewLoop_ThirdRoundStillMovesBack(t *testing.T) {
	l := newReviewLoop(t)
	l.priorChangesRounds(t, 2)
	l.verdict(t, "in_progress", finding("P0")).Want(http.StatusOK)
	if status, _, _ := issueStatusAndAssignee(t, l.issueID); status != "in_progress" {
		t.Fatalf("issue status = %q, want in_progress", status)
	}
}

func TestReviewLoop_RoundAfterThreeChangesVerdictsGoesToNeedDecision(t *testing.T) {
	l := newReviewLoop(t)
	l.priorChangesRounds(t, 3)
	l.verdict(t, "in_progress", finding("P0")).Want(http.StatusOK)
	if status, _, _ := issueStatusAndAssignee(t, l.issueID); status != "need_decision" {
		t.Fatalf("issue status = %q, want need_decision", status)
	}
}

// A move back into agent_review with the head unchanged since a completed
// review dispatches nothing; a new head dispatches one.
func TestReviewLoop_SameHeadIsNotReviewedAgain(t *testing.T) {
	l := newReviewLoop(t)
	prID := dbfx.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": testWorkspaceID, "installation_id": 1, "repo_owner": "multica-ai", "repo_name": "multica",
		"pr_number": testutil.Raw("(floor(random() * 1000000000))::int"), "title": "loop PR", "state": "open", "html_url": "https://example.test/pr",
		"pr_created_at": testutil.Raw("now()"), "pr_updated_at": testutil.Raw("now()"), "head_sha": reviewHeadA,
	})
	dbfx.Exec(t, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, l.issueID, prID)
	t.Cleanup(func() { testPool.Exec(t.Context(), `DELETE FROM issue_pull_request WHERE pull_request_id = $1`, prID) })
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now(), context = jsonb_build_object('head_sha', $2::text), pull_request_id = $3 WHERE id = $1`, l.run, reviewHeadA, prID)
	dbfx.Exec(t, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, l.issueID)

	updateIssueForTest(t, l.issueID, map[string]any{"status": "agent_review"}).Want(http.StatusOK)
	if runs := queuedRuns(t, l.issueID); len(runs) != 0 {
		t.Fatalf("same head queued %d review run(s), want 0", len(runs))
	}

	dbfx.Exec(t, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, l.issueID)
	pushHead(t, prID, reviewHeadB)
	updateIssueForTest(t, l.issueID, map[string]any{"status": "agent_review"}).Want(http.StatusOK)
	if runs := queuedRuns(t, l.issueID); len(runs) != 1 || runs[0].head != reviewHeadB {
		t.Fatalf("new head queued %v, want one run on %s", runs, reviewHeadB)
	}
}
