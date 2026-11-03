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

// priorChangesRounds records n earlier changes-requested verdicts of one PR
// lineage ("" is the run without a PR).
func (l reviewLoop) priorChangesRounds(t *testing.T, n int, prID string) {
	t.Helper()
	for i := 0; i < n; i++ {
		dbfx.Exec(t, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details)
			SELECT workspace_id, id, 'agent', $2, 'review_changes_requested', jsonb_build_object('pull_request_id', $3::text) FROM issue WHERE id = $1`, l.issueID, l.agentID, prID)
	}
}

// linkPR links a new open PR on head to the issue and stamps the review run
// with it, as a dispatched review run is.
func (l reviewLoop) linkPR(t *testing.T, head string) string {
	t.Helper()
	prID := l.newPR(t, head)
	dbfx.Exec(t, `UPDATE agent_task_queue SET context = jsonb_build_object('head_sha', $2::text), pull_request_id = $3 WHERE id = $1`, l.run, head, prID)
	return prID
}

// newPR links a new open PR on head to the issue.
func (l reviewLoop) newPR(t *testing.T, head string) string {
	t.Helper()
	prID := dbfx.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": testWorkspaceID, "installation_id": 1, "repo_owner": "multica-ai", "repo_name": "multica",
		"pr_number": testutil.Raw("(floor(random() * 1000000000))::int"), "title": "loop PR", "state": "open", "html_url": "https://example.test/pr",
		"pr_created_at": testutil.Raw("now()"), "pr_updated_at": testutil.Raw("now()"), "head_sha": head,
	})
	dbfx.Exec(t, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, l.issueID, prID)
	t.Cleanup(func() { testPool.Exec(t.Context(), `DELETE FROM issue_pull_request WHERE pull_request_id = $1`, prID) })
	return prID
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

func TestReviewLoop_SecondRoundStillMovesBack(t *testing.T) {
	l := newReviewLoop(t)
	l.priorChangesRounds(t, 1, "")
	l.verdict(t, "in_progress", finding("P0")).Want(http.StatusOK)
	if status, _, _ := issueStatusAndAssignee(t, l.issueID); status != "in_progress" {
		t.Fatalf("issue status = %q, want in_progress", status)
	}
}

// The third changes-requested verdict is the one that stops the loop.
func TestReviewLoop_ThirdChangesVerdictGoesToNeedDecision(t *testing.T) {
	l := newReviewLoop(t)
	l.priorChangesRounds(t, 2, "")
	l.verdict(t, "in_progress", finding("P0")).Want(http.StatusOK)
	if status, _, _ := issueStatusAndAssignee(t, l.issueID); status != "need_decision" {
		t.Fatalf("issue status = %q, want need_decision", status)
	}
	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'review_changes_requested'`, l.issueID).Scan(&n)
	if n != 3 {
		t.Fatalf("recorded rounds = %d, want 3", n)
	}
}

// A replacement PR starts its own count: rounds of another PR do not carry over.
func TestReviewLoop_RoundsOfAnotherPRDoNotCount(t *testing.T) {
	l := newReviewLoop(t)
	prID := l.linkPR(t, reviewHeadA)
	l.priorChangesRounds(t, 5, "00000000-0000-0000-0000-000000000001")
	l.verdict(t, "in_progress", finding("P1")).Want(http.StatusOK)
	if status, _, _ := issueStatusAndAssignee(t, l.issueID); status != "in_progress" {
		t.Fatalf("issue status = %q, want in_progress", status)
	}
	l.priorChangesRounds(t, 1, prID)
	dbfx.Exec(t, `UPDATE issue SET status = 'agent_review' WHERE id = $1`, l.issueID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET workflow_step = 'agent_review' WHERE id = $1`, l.run)
	l.verdict(t, "in_progress", finding("P1")).Want(http.StatusOK)
	if status, _, _ := issueStatusAndAssignee(t, l.issueID); status != "need_decision" {
		t.Fatalf("third round of this PR: status = %q, want need_decision", status)
	}
}

// The verdict that changes the status and the round that counts it commit
// together: a refused write leaves no round behind.
func TestReviewLoop_RefusedVerdictRecordsNoRound(t *testing.T) {
	l := newReviewLoop(t)
	l.verdict(t, "in_progress", finding("P3")).Want(http.StatusBadRequest)
	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'review_changes_requested'`, l.issueID).Scan(&n)
	if n != 0 {
		t.Fatalf("recorded rounds = %d, want 0", n)
	}
}

// After a changes-requested verdict on a head, entering agent_review on the same
// head dispatches nothing; a new head dispatches one.
func TestReviewLoop_SameHeadIsNotReviewedAgain(t *testing.T) {
	l := newReviewLoop(t)
	prID := l.linkPR(t, reviewHeadA)
	l.verdict(t, "in_progress", finding("P1")).Want(http.StatusOK)

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

// A review run that completed without a verdict reviewed nothing: the head is
// still owed a review.
func TestReviewLoop_CompletedRunWithoutVerdictDoesNotSkipReview(t *testing.T) {
	l := newReviewLoop(t)
	l.linkPR(t, reviewHeadA)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, l.run)
	dbfx.Exec(t, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, l.issueID)

	updateIssueForTest(t, l.issueID, map[string]any{"status": "agent_review"}).Want(http.StatusOK)
	if runs := queuedRuns(t, l.issueID); len(runs) != 1 || runs[0].head != reviewHeadA {
		t.Fatalf("queued %v, want one run on %s", runs, reviewHeadA)
	}
}

// An ordinarily dispatched review run is bound to the PR it reviews, so the
// rounds of a replaced PR do not carry over to its replacement.
func TestReviewLoop_OrdinaryDispatchCountsRoundsPerReplacementPR(t *testing.T) {
	l := newReviewLoop(t)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, l.run)
	dbfx.Exec(t, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, l.issueID)
	prA := l.newPR(t, reviewHeadA)
	l.priorChangesRounds(t, 2, prA)

	dbfx.Exec(t, `DELETE FROM issue_pull_request WHERE issue_id = $1 AND pull_request_id = $2`, l.issueID, prA)
	prB := l.newPR(t, reviewHeadB)
	updateIssueForTest(t, l.issueID, map[string]any{"status": "agent_review"}).Want(http.StatusOK)
	runs := queuedRuns(t, l.issueID)
	if len(runs) != 1 {
		t.Fatalf("queued %v, want one review run", runs)
	}
	var bound string
	dbfx.QueryRow(t, `SELECT COALESCE(pull_request_id::text, '') FROM agent_task_queue WHERE id = $1`, runs[0].id).Scan(&bound)
	if bound != prB {
		t.Fatalf("review run bound to PR %q, want %q", bound, prB)
	}

	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running', dispatched_at = now(), started_at = now() WHERE id = $1`, runs[0].id)
	agentUpdateIssue(t, l.issueID, l.agentID, runs[0].id, map[string]any{"status": "in_progress", "review_findings": []map[string]any{finding("P1")}}).Want(http.StatusOK)
	if status, _, _ := issueStatusAndAssignee(t, l.issueID); status != "in_progress" {
		t.Fatalf("first round of the replacement PR: status = %q, want in_progress", status)
	}
}

// A PASS reviews the head too: coming back to agent_review on it dispatches nothing.
func TestReviewLoop_PassOnHeadSkipsReturnToReview(t *testing.T) {
	l := newReviewLoop(t)
	l.linkPR(t, reviewHeadA)
	l.verdict(t, "in_review").Want(http.StatusOK)

	updateIssueForTest(t, l.issueID, map[string]any{"status": "agent_review"}).Want(http.StatusOK)
	if runs := queuedRuns(t, l.issueID); len(runs) != 0 {
		t.Fatalf("same head after PASS queued %d review run(s), want 0", len(runs))
	}
}

// Only the last verdict's head is "already reviewed": A, then B, then A again
// is a head nobody has looked at since B.
func TestReviewLoop_OnlyTheLastVerdictHeadIsSkipped(t *testing.T) {
	l := newReviewLoop(t)
	prID := l.newPR(t, reviewHeadA)
	for head, ago := range map[string]string{reviewHeadA: "2 minutes", reviewHeadB: "1 minute"} {
		dbfx.Exec(t, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details, created_at)
			SELECT workspace_id, id, 'agent', $2, 'review_changes_requested', jsonb_build_object('head_sha', $3::text), now() - $4::interval FROM issue WHERE id = $1`,
			l.issueID, l.agentID, head, ago)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, l.run)
	dbfx.Exec(t, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, l.issueID)

	updateIssueForTest(t, l.issueID, map[string]any{"status": "agent_review"}).Want(http.StatusOK)
	if runs := queuedRuns(t, l.issueID); len(runs) != 1 {
		t.Fatalf("head A after a verdict on B queued %v, want one run", runs)
	}

	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'cancelled' WHERE issue_id = $1 AND status = 'queued'`, l.issueID)
	dbfx.Exec(t, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, l.issueID)
	pushHead(t, prID, reviewHeadB)
	updateIssueForTest(t, l.issueID, map[string]any{"status": "agent_review"}).Want(http.StatusOK)
	if runs := queuedRuns(t, l.issueID); len(runs) != 0 {
		t.Fatalf("head B after the verdict on B queued %v, want none", runs)
	}
}

// otherIssueRun is a running run of the loop's agent on a different issue.
func (l reviewLoop) otherIssueRun(t *testing.T) string {
	t.Helper()
	other := dbfx.Issue(t, "other issue", testutil.Cols{"project_id": l.projectID, "status": "in_progress"})
	return dbfx.Task(t, l.agentID, testutil.Cols{
		"runtime_id": l.runtimeID, "issue_id": other, "status": "running",
		"dispatched_at": testutil.Raw("now() - interval '5 minutes'"), "started_at": testutil.Raw("now() - interval '4 minutes'"),
	})
}

func (l reviewLoop) verdictsRecorded(t *testing.T) (n int) {
	t.Helper()
	dbfx.QueryRow(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action IN ('review_changes_requested', 'review_passed')`, l.issueID).Scan(&n)
	return n
}

// A verdict from a run of another issue names no reviewed PR or head, so it
// could neither count as a round nor set the head to skip on: it is refused,
// CHANGES and PASS alike, and the issue stays at its review step.
func TestReviewLoop_VerdictFromRunOfAnotherIssueIsRefused(t *testing.T) {
	l := newReviewLoop(t)
	l.linkPR(t, reviewHeadA)
	otherRun := l.otherIssueRun(t)
	for name, body := range map[string]map[string]any{
		"CHANGES": {"status": "in_progress", "review_findings": []map[string]any{finding("P1")}},
		"PASS":    {"status": "in_review"},
	} {
		resp := agentUpdateIssue(t, l.issueID, l.agentID, otherRun, body).Want(http.StatusForbidden).Map()
		if resp["code"] != "review_verdict_run_required" {
			t.Fatalf("%s: refusal = %v", name, resp)
		}
		if status, _, _ := issueStatusAndAssignee(t, l.issueID); status != "agent_review" {
			t.Fatalf("%s: status after refused verdict = %q, want agent_review", name, status)
		}
	}
	if n := l.verdictsRecorded(t); n != 0 {
		t.Fatalf("recorded verdicts = %d, want 0", n)
	}
}

// Mixed origins: two rounds on the PR, then a third from another issue's run
// is refused instead of counting zero rounds; the reviewer's own run then
// hits the limit.
func TestReviewLoop_CrossIssueVerdictCannotResetTheRoundCount(t *testing.T) {
	l := newReviewLoop(t)
	prID := l.linkPR(t, reviewHeadA)
	l.priorChangesRounds(t, 2, prID)
	body := map[string]any{"status": "in_progress", "review_findings": []map[string]any{finding("P1")}}

	agentUpdateIssue(t, l.issueID, l.agentID, l.otherIssueRun(t), body).Want(http.StatusForbidden)
	if status, _, _ := issueStatusAndAssignee(t, l.issueID); status != "agent_review" {
		t.Fatalf("status after cross-issue verdict = %q, want agent_review", status)
	}
	agentUpdateIssue(t, l.issueID, l.agentID, l.run, body).Want(http.StatusOK)
	if status, _, _ := issueStatusAndAssignee(t, l.issueID); status != "need_decision" {
		t.Fatalf("third round from the reviewer's run: status = %q, want need_decision", status)
	}
}
