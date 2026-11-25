package handler

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-6192: the round cap counts entries into agent_review, so it also stops a
// loop whose reviewer leaves through Blocked or a wakeup, which records no
// CHANGES verdict. The entry that would start round 4 goes to need_decision.

var roundCapSteps = []map[string]any{
	{"status_key": "in_progress", "edges": []map[string]any{{"to": "agent_review", "guard": "agent_done"}}},
	{"status_key": "agent_review", "edges": []map[string]any{{"to": "in_progress", "guard": "agent_verdict(CHANGES)"}}},
	{"status_key": "need_decision"},
	{"status_key": "blocked"},
}

type roundCap struct {
	agentID, issueID, run string
}

func newRoundCap(t *testing.T) roundCap {
	t.Helper()
	for _, key := range []string{"agent_review", "need_decision"} {
		if dbfx.Count(t, `SELECT count(*) FROM issue_status WHERE workspace_id = $1 AND key = $2`, testWorkspaceID, key) == 0 {
			createTestCustomStatus(t, key, "started")
		}
	}
	agentID := seededReadyAgentID(t)
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID)
	wf := createTestWorkflowSteps(t, "Round cap "+workflowTestSuffix(), roundCapSteps)
	projectID := createWorkflowTestProject(t, "Round cap project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	issueID := dbfx.Issue(t, "round cap", testutil.Cols{
		"project_id": projectID, "status": "in_progress", "assignee_type": "agent", "assignee_id": agentID,
	})
	run := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "workflow_step": "in_progress",
		"dispatched_at": testutil.Raw("now() - interval '5 minutes'"), "started_at": testutil.Raw("now() - interval '4 minutes'"),
	})
	return roundCap{agentID, issueID, run}
}

// activity inserts one earlier activity row, minutesAgo minutes old.
func (c roundCap) activity(t *testing.T, action, to string, minutesAgo int) {
	t.Helper()
	dbfx.Exec(t, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details, created_at)
		SELECT workspace_id, id, 'agent', $2, $3, jsonb_build_object('to', $4::text), now() - make_interval(mins => $5) FROM issue WHERE id = $1`,
		c.issueID, c.agentID, action, to, minutesAgo)
}

func (c roundCap) enterReview(t *testing.T) {
	t.Helper()
	agentUpdateIssue(t, c.issueID, c.agentID, c.run, map[string]any{"status": "agent_review"}).Want(http.StatusOK)
}

// wantCard checks the decision card the cap opened: one open request on the
// issue, from the server, whose ask names the round count (CHE-6192).
func (c roundCap) wantCard(t *testing.T, rounds int) {
	t.Helper()
	var n int
	var ask string
	dbfx.QueryRow(t, `SELECT count(*), coalesce(max(ask_json::text), '') FROM decision_request WHERE issue_id = $1 AND status = 'open' AND request_fingerprint LIKE 'server:review_rounds:%'`, c.issueID).Scan(&n, &ask)
	if n == 0 {
		t.Fatalf("no open review-rounds decision card on the issue")
	}
	if want := fmt.Sprintf("%d review rounds", rounds); !strings.Contains(ask, want) {
		t.Fatalf("card ask does not name %q: %s", want, ask)
	}
}

func (c roundCap) status(t *testing.T) string {
	t.Helper()
	status, _, _ := issueStatusAndAssignee(t, c.issueID)
	return status
}

func TestRoundCap_FourthEntryIntoReviewGoesToNeedDecision(t *testing.T) {
	c := newRoundCap(t)
	for _, ago := range []int{300, 200, 100} {
		c.activity(t, "status_changed", "agent_review", ago)
	}
	c.enterReview(t)
	if got := c.status(t); got != "need_decision" {
		t.Fatalf("status after the 4th entry = %q, want need_decision", got)
	}
	c.wantCard(t, 3)
	var capped int
	dbfx.QueryRow(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'review_rounds_capped'`, c.issueID).Scan(&capped)
	if capped != 1 {
		t.Fatalf("review_rounds_capped rows = %d, want 1", capped)
	}
}

func TestRoundCap_ThirdEntryStillGoesToReview(t *testing.T) {
	c := newRoundCap(t)
	c.activity(t, "status_changed", "agent_review", 200)
	c.activity(t, "status_changed", "agent_review", 100)
	c.enterReview(t)
	if got := c.status(t); got != "agent_review" {
		t.Fatalf("status after the 3rd entry = %q, want agent_review", got)
	}
}

// A PASS or an earlier cap ends the count: rounds restart from zero.
func TestRoundCap_PassOrEarlierCapRestartsTheCount(t *testing.T) {
	for _, marker := range []string{"review_passed", "review_rounds_capped"} {
		t.Run(marker, func(t *testing.T) {
			c := newRoundCap(t)
			c.activity(t, "status_changed", "agent_review", 300)
			c.activity(t, "status_changed", "agent_review", 250)
			c.activity(t, "status_changed", "agent_review", 200)
			c.activity(t, marker, "", 100)
			c.enterReview(t)
			if got := c.status(t); got != "agent_review" {
				t.Fatalf("status = %q, want agent_review", got)
			}
		})
	}
}

// A reviewer on another lab hands the work back by wakeup, not by a status move
// (agent_review step 4b): the issue never re-enters agent_review, so only the
// wakeup is counted. The 3rd CHANGES refuses the wakeup, moves the issue to
// need_decision and opens the card (CHE-6192).

// reviewerWakeup is the reviewer's run on agent_review handing the issue to the
// author by wakeup.
func (c roundCap) reviewerWakeup(t *testing.T, reviewRun, author string) *testutil.Response {
	t.Helper()
	req := withURLParams(newRequest(http.MethodPost, "/api/issues/"+c.issueID+"/wakeups",
		map[string]any{"agent_id": author, "kind": "at", "after_seconds": 60, "instruction": "fix the findings"}), "id", c.issueID, "wakeupID", "")
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", c.agentID)
	req.Header.Set("X-Task-ID", reviewRun)
	return testutil.Call(t, testHandler.CreateIssueWakeup, req)
}

// inReview puts the issue in agent_review and returns a reviewer run on it.
func (c roundCap) inReview(t *testing.T) (reviewRun string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE issue SET status = 'agent_review' WHERE id = $1`, c.issueID)
	var runtimeID, human string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, c.agentID).Scan(&runtimeID)
	human = dbfx.User(t, "Reviewer originator", "round-cap-"+workflowTestSuffix()+"@test.invalid")
	dbfx.Insert(t, "member", testutil.Cols{"workspace_id": testWorkspaceID, "user_id": human, "role": "owner"})
	run := dbfx.Task(t, c.agentID, testutil.Cols{
		"runtime_id": runtimeID, "issue_id": c.issueID, "status": "running", "workflow_step": "agent_review",
		"dispatched_at": testutil.Raw("now() - interval '5 minutes'"), "started_at": testutil.Raw("now() - interval '4 minutes'"),
		"originator_user_id": human, "accountable_user_id": human, "originator_source": "trigger_owner",
	})
	dbfx.Cleanup(t, `DELETE FROM issue_wakeup_receipt WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE issue_id = $1)`, c.issueID)
	dbfx.Cleanup(t, `DELETE FROM issue_wakeup WHERE issue_id = $1`, c.issueID)
	dbfx.Cleanup(t, `DELETE FROM decision_request WHERE issue_id = $1`, c.issueID)
	return run
}

func (c roundCap) wakeups(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM issue_wakeup WHERE issue_id = $1`, c.issueID)
}

func TestRoundCap_ThirdReviewerWakeupGoesToNeedDecision(t *testing.T) {
	c := newRoundCap(t)
	run := c.inReview(t)
	author := otherReadyAgentID(t)
	for round := 1; round <= 2; round++ {
		c.reviewerWakeup(t, run, author).Want(http.StatusCreated)
	}
	if got := c.status(t); got != "agent_review" {
		t.Fatalf("status after 2 wakeup rounds = %q, want agent_review", got)
	}
	c.reviewerWakeup(t, run, author).Want(http.StatusConflict)
	if got := c.status(t); got != "need_decision" {
		t.Fatalf("status after the 3rd wakeup round = %q, want need_decision", got)
	}
	if n := c.wakeups(t); n != 2 {
		t.Fatalf("wakeups = %d, want 2: the 3rd must not wake the author", n)
	}
	c.wantCard(t, 3)
}

// A wakeup to the reviewer's own agent, or one made outside agent_review, is
// not a CHANGES round.
func TestRoundCap_OtherWakeupsAreNotRounds(t *testing.T) {
	c := newRoundCap(t)
	run := c.inReview(t)
	for i := 0; i < 4; i++ {
		c.reviewerWakeup(t, run, c.agentID).Want(http.StatusCreated)
	}
	if got := c.status(t); got != "agent_review" {
		t.Fatalf("status = %q, want agent_review: a self-wakeup is not a round", got)
	}
	// An author's return wakeup is made from its in_progress run.
	author := otherReadyAgentID(t)
	dbfx.Exec(t, `UPDATE agent_task_queue SET workflow_step = 'in_progress' WHERE id = $1`, run)
	for i := 0; i < 4; i++ {
		c.reviewerWakeup(t, run, author).Want(http.StatusCreated)
	}
	if got := c.status(t); got != "agent_review" {
		t.Fatalf("status = %q, want agent_review: a return wakeup is not a round", got)
	}
}

// Rounds of another issue do not count, and a PASS restarts the count.
func TestRoundCap_WakeupRoundsAreCountedPerIssueAndEndAtPass(t *testing.T) {
	other := newRoundCap(t)
	otherRun := other.inReview(t)
	author := otherReadyAgentID(t)
	other.reviewerWakeup(t, otherRun, author).Want(http.StatusCreated)
	other.reviewerWakeup(t, otherRun, author).Want(http.StatusCreated)

	c := newRoundCap(t)
	run := c.inReview(t)
	c.reviewerWakeup(t, run, author).Want(http.StatusCreated)
	c.reviewerWakeup(t, run, author).Want(http.StatusCreated)
	c.activity(t, "review_passed", "", 0)
	c.reviewerWakeup(t, run, author).Want(http.StatusCreated)
	if got := c.status(t); got != "agent_review" {
		t.Fatalf("status = %q, want agent_review: a PASS restarts the count", got)
	}
}

// The third CHANGES verdict of a PR is rewritten to need_decision by the
// CHE-3243 guard. That path records the cap and opens the card too.
func TestRoundCap_ThirdStatusVerdictOpensCardAndRestartsTheCount(t *testing.T) {
	l := newReviewLoop(t)
	l.priorChangesRounds(t, 2, "")
	l.verdict(t, "in_progress", finding("P1")).Want(http.StatusOK)
	if status, _, _ := issueStatusAndAssignee(t, l.issueID); status != "need_decision" {
		t.Fatalf("status after the 3rd verdict = %q, want need_decision", status)
	}
	c := roundCap{l.agentID, l.issueID, l.run}
	c.wantCard(t, 3)
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'review_rounds_capped'`, l.issueID); n != 1 {
		t.Fatalf("review_rounds_capped rows = %d, want 1", n)
	}
}

// newReviewRun is the review-loop fixture with a reviewer run that may hand the
// work to another agent by wakeup, and a PR lineage on the run.
func newReviewRun(t *testing.T) (roundCap, string) {
	t.Helper()
	l := newReviewLoop(t)
	human := dbfx.User(t, "Reviewer originator", "round-cap-"+workflowTestSuffix()+"@test.invalid")
	dbfx.Insert(t, "member", testutil.Cols{"workspace_id": testWorkspaceID, "user_id": human, "role": "owner"})
	dbfx.Exec(t, `UPDATE agent_task_queue SET originator_user_id = $2, accountable_user_id = $2, originator_source = 'trigger_owner' WHERE id = $1`, l.run, human)
	c := roundCap{l.agentID, l.issueID, l.run}
	c.reviewerPR(t, l.run)
	return c, l.run
}

// reviewerPR stamps the reviewer's run with a PR lineage, as a dispatched
// review run is.
func (c roundCap) reviewerPR(t *testing.T, run string) string {
	t.Helper()
	var prID string
	dbfx.QueryRow(t, `SELECT gen_random_uuid()::text`).Scan(&prID)
	c.runOnPR(t, run, prID)
	return prID
}

func (c roundCap) runOnPR(t *testing.T, run, prID string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE agent_task_queue SET pull_request_id = $2 WHERE id = $1`, run, prID)
}

// backToReview puts the issue back in agent_review for the reviewer's next
// verdict, as the run's own step was advanced by the earlier one.
func (c roundCap) backToReview(t *testing.T, run string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE issue SET status = 'agent_review' WHERE id = $1`, c.issueID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET workflow_step = 'agent_review' WHERE id = $1`, run)
}

func (c roundCap) statusVerdict(t *testing.T, run string) *testutil.Response {
	t.Helper()
	c.backToReview(t, run)
	return agentUpdateIssue(t, c.issueID, c.agentID, run, map[string]any{
		"status": "in_progress", "review_findings": []map[string]any{{"severity": "P1", "summary": "finding P1", "category": "unverified_claim"}},
	})
}

// One wakeup round and two status verdicts on one PR are three CHANGES rounds,
// whichever way each was handed back.
func TestRoundCap_MixedWakeupAndStatusRoundsCountOnePR(t *testing.T) {
	c, run := newReviewRun(t)
	author := otherReadyAgentID(t)
	c.reviewerWakeup(t, run, author).Want(http.StatusCreated)
	c.statusVerdict(t, run).Want(http.StatusOK)
	c.statusVerdict(t, run).Want(http.StatusOK)
	if got := c.status(t); got != "need_decision" {
		t.Fatalf("status after wakeup + 2 status verdicts = %q, want need_decision", got)
	}
	c.wantCard(t, 3)
}

// Two status verdicts then a wakeup: the wakeup is the third round.
func TestRoundCap_StatusRoundsThenWakeupIsTheThirdRound(t *testing.T) {
	c, run := newReviewRun(t)
	author := otherReadyAgentID(t)
	c.statusVerdict(t, run).Want(http.StatusOK)
	c.statusVerdict(t, run).Want(http.StatusOK)
	c.backToReview(t, run)
	c.reviewerWakeup(t, run, author).Want(http.StatusConflict)
	if got := c.status(t); got != "need_decision" {
		t.Fatalf("status = %q, want need_decision", got)
	}
	c.wantCard(t, 3)
}

// Rounds of another PR on the same issue do not count: a replacement PR starts
// at zero.
func TestRoundCap_RoundsAreCountedPerPR(t *testing.T) {
	c, run := newReviewRun(t)
	first := c.reviewerPR(t, run)
	author := otherReadyAgentID(t)
	c.reviewerWakeup(t, run, author).Want(http.StatusCreated)
	c.statusVerdict(t, run).Want(http.StatusOK)
	c.reviewerPR(t, run)
	c.backToReview(t, run)
	c.reviewerWakeup(t, run, author).Want(http.StatusCreated)
	c.statusVerdict(t, run).Want(http.StatusOK)
	if got := c.status(t); got != "in_progress" {
		t.Fatalf("status = %q, want in_progress: 2 rounds on each of 2 PRs", got)
	}
	c.runOnPR(t, run, first)
	c.statusVerdict(t, run).Want(http.StatusOK)
	if got := c.status(t); got != "need_decision" {
		t.Fatalf("status = %q, want need_decision: the first PR's 3rd round", got)
	}
}
