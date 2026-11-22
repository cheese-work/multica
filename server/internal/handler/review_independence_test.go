package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/designguard"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// Verdict-time reviewer independence (CHE-7658 step 14): on a step whose
// handler is `reviewer`, a PASS verdict is refused unless the reviewer's run is
// independent of every author of the issue (designguard A1 to A5), judged from
// the step-11 role evidence. A refusal writes no verdict row.

// independence is a review loop on a `reviewer` step with agents of two labs.
type independence struct {
	reviewLoop
	claudeRuntime, codexRuntime string
	authorID                    string // anthropic, authored the issue
	sameLabID                   string // anthropic
	crossLabID                  string // openai
}

func (i independence) agentOn(t *testing.T, name, runtimeID, model string) string {
	t.Helper()
	id := createHandlerTestAgent(t, name+"-"+safeTestName(t), nil)
	dbfx.Exec(t, `UPDATE agent SET runtime_id = $1, model = $2 WHERE id = $3`, runtimeID, model, id)
	return id
}

// task is a task of agentID on the issue with its role row, as enqueue leaves it.
func (i independence) task(t *testing.T, agentID, runtimeID, model, status, step, role string) string {
	t.Helper()
	id := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": runtimeID, "issue_id": i.issueID, "status": status, "workflow_step": step, "applied_model": model,
		"dispatched_at": testutil.Raw("now() - interval '5 minutes'"), "started_at": testutil.Raw("now() - interval '4 minutes'"),
	})
	dbfx.Exec(t, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details)
		VALUES ($1, $2, 'agent', $3::text::uuid, 'workflow_role', jsonb_build_object('task_id', $4::text, 'agent_id', $3::text, 'role', $5::text, 'status', $6::text))`,
		testWorkspaceID, i.issueID, agentID, id, role, step)
	return id
}

// reviewerRun is a running review task of agentID, stamped with the PR and head
// as a dispatched review run is.
func (i independence) reviewerRun(t *testing.T, agentID, runtimeID, model, prID, head string) string {
	t.Helper()
	id := i.task(t, agentID, runtimeID, model, "running", "agent_review", "reviewer")
	dbfx.Exec(t, `UPDATE agent_task_queue SET context = jsonb_build_object('head_sha', $2::text), pull_request_id = $3 WHERE id = $1`, id, head, prID)
	return id
}

func newIndependence(t *testing.T) independence {
	t.Helper()
	t.Setenv("MULTICA_VOTE_GATE_LABS", voteLabs)
	createTestCustomStatus(t, "agent_review", "started")
	createTestCustomStatus(t, "need_decision", "started")
	i := independence{}
	i.claudeRuntime = dbfx.Runtime(t, "che7679-claude-"+safeTestName(t), testutil.Cols{"provider": "claude"})
	i.codexRuntime = dbfx.Runtime(t, "che7679-codex-"+safeTestName(t), testutil.Cols{"provider": "codex"})
	i.authorID = i.agentOn(t, "che7679-author", i.claudeRuntime, "claude-test")
	i.sameLabID = i.agentOn(t, "che7679-same", i.claudeRuntime, "claude-test")
	i.crossLabID = i.agentOn(t, "che7679-cross", i.codexRuntime, "gpt-test")

	wf := createTestWorkflowSteps(t, "Independence "+workflowTestSuffix(), reviewLoopSteps)
	// The reviewer handler type has no handoff form yet, so the step is stored directly.
	dbfx.Exec(t, `UPDATE issue_workflow SET steps = (
		SELECT jsonb_agg(CASE WHEN s->>'status_key' = 'agent_review' THEN s || '{"handler":{"type":"reviewer"}}'::jsonb ELSE s END ORDER BY o)
		FROM jsonb_array_elements(steps) WITH ORDINALITY AS e(s, o)) WHERE id = $1`, wf.ID)
	projectID := createWorkflowTestProject(t, "Independence project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	i.projectID = projectID
	i.issueID = dbfx.Issue(t, "independence", testutil.Cols{"project_id": projectID, "status": "agent_review"})
	i.task(t, i.authorID, i.claudeRuntime, "claude-test", "completed", "in_progress", "agent")
	return i
}

// review sets the issue's reviewer to agentID on its own run of that lab and
// returns the PR head the run reviews.
func (i *independence) review(t *testing.T, agentID, runtimeID, model, head string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE issue SET status = 'agent_review', assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, i.issueID, agentID)
	i.agentID = agentID
	i.runtimeID = runtimeID
	prID := i.newPR(t, head)
	i.run = i.reviewerRun(t, agentID, runtimeID, model, prID, head)
}

func (i independence) passRows(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'review_passed'`, i.issueID)
}

func (i independence) wantRefused(t *testing.T, resp *testutil.Response, code string) {
	t.Helper()
	resp.Want(http.StatusConflict)
	if !strings.Contains(resp.Text(), code) {
		t.Fatalf("refusal body %q lacks the code %q", resp.Text(), code)
	}
	if status, _, _ := issueStatusAndAssignee(t, i.issueID); status != "agent_review" {
		t.Fatalf("issue status = %q after a refused verdict, want agent_review", status)
	}
	if n := i.passRows(t); n != 0 {
		t.Fatalf("%d review_passed rows after a refused verdict, want none", n)
	}
}

func TestReviewIndependence_SameLabPassIsRefusedAndWritesNoRow(t *testing.T) {
	i := newIndependence(t)
	i.review(t, i.sameLabID, i.claudeRuntime, "claude-test", "head-1")
	i.wantRefused(t, i.verdict(t, "in_review"), designguard.CodeSameLab)
}

func TestReviewIndependence_AuthorAsReviewerIsRefused(t *testing.T) {
	i := newIndependence(t)
	i.review(t, i.authorID, i.claudeRuntime, "claude-test", "head-1")
	i.wantRefused(t, i.verdict(t, "in_review"), designguard.CodeSelfReview)
}

func TestReviewIndependence_CrossLabPassRowCarriesTheReviewedHead(t *testing.T) {
	i := newIndependence(t)
	i.review(t, i.crossLabID, i.codexRuntime, "gpt-test", "head-cross")
	i.verdict(t, "in_review").Want(http.StatusOK)
	var head string
	dbfx.QueryRow(t, `SELECT details->>'head_sha' FROM activity_log WHERE issue_id = $1 AND action = 'review_passed'`, i.issueID).Scan(&head)
	if head != "head-cross" {
		t.Fatalf("review_passed head_sha = %q, want head-cross", head)
	}
}

// The first author still counts after a CHANGES round and a reassignment: the
// head is unchanged, so a same-lab reviewer must not sign it just because the
// issue now sits with someone else.
func TestReviewIndependence_FirstAuthorStillCountsAfterChangesAndReassignment(t *testing.T) {
	i := newIndependence(t)
	i.review(t, i.crossLabID, i.codexRuntime, "gpt-test", "head-same")
	i.verdict(t, "in_progress", finding("P1")).Want(http.StatusOK)

	// The work goes to another agent, then back to review on the same head.
	dbfx.Exec(t, `UPDATE issue SET assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, i.issueID, i.crossLabID)
	i.review(t, i.sameLabID, i.claudeRuntime, "claude-test", "head-same")
	i.wantRefused(t, i.verdict(t, "in_review"), designguard.CodeSameLab)
}

// Only steps with a reviewer handler are judged: the same verdict on a step
// without one passes, as on base.
func TestReviewIndependence_StepWithoutReviewerHandlerIsUnchanged(t *testing.T) {
	i := newIndependence(t)
	dbfx.Exec(t, `UPDATE issue_workflow SET steps = (
		SELECT jsonb_agg(s - 'handler' ORDER BY o) FROM jsonb_array_elements(steps) WITH ORDINALITY AS e(s, o)) WHERE id = (SELECT workflow_id FROM project WHERE id = $1)`, i.projectID)
	i.review(t, i.sameLabID, i.claudeRuntime, "claude-test", "head-1")
	i.verdict(t, "in_review").Want(http.StatusOK)
}
