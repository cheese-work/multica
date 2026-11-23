package handler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-9340: the stop-loss. An issue with 20 runs or 30 M tokens since its last
// release and no linked open, draft or merged PR starts no further run; the
// refusal parks it in need_decision with one card. The auto-approver never
// releases it.

// stopLossCase is a weekly-guard case whose workflow also lists need_decision,
// with the stop-loss in the given mode.
func stopLossCase(t *testing.T, installationID int64, mode string) budgetCase {
	t.Helper()
	addWorkflowStatuses(t)
	t.Setenv("MULTICA_WEEKLY_RUN_GUARD_MAX_RUNS", "100")
	steps := append(recoverySteps(seededReadyAgentID(t)),
		map[string]any{"status_key": "need_decision", "handler": map[string]any{"type": "agent", "id": seededReadyAgentID(t)}, "instructions": "Wait for the answer."})
	c := newBudgetCase(t, installationID, steps, 1000)
	if mode != "" {
		setAutoSettings(t, `{"run_guard":{"stop_loss":{"mode":"`+mode+`"}}}`)
	} else {
		// newBudgetCase turns the stop-loss off; "" means the shipped default.
		setAutoSettings(t, `{"run_guard":{}}`)
	}
	dbfx.Cleanup(t, `DELETE FROM decision_answer WHERE decision_request_id IN (SELECT id FROM decision_request WHERE issue_id = $1)`, c.issueID)
	dbfx.Cleanup(t, `DELETE FROM decision_request WHERE issue_id = $1`, c.issueID)
	dbfx.Cleanup(t, `DELETE FROM weekly_run_guard_refusal WHERE issue_id = $1`, c.issueID)
	dbfx.Cleanup(t, `DELETE FROM weekly_run_guard_allowance WHERE issue_id = $1`, c.issueID)
	dbfx.Cleanup(t, `DELETE FROM issue_decision_park WHERE issue_id = $1`, c.issueID)
	return c
}

// stopLossRefused expects the stop-loss to refuse one run and returns its event.
func (c budgetCase) stopLossRefused(t *testing.T) service.WeeklyRunGuardTripped {
	t.Helper()
	events := make(chan service.WeeklyRunGuardTripped, 1)
	service.SetOnWeeklyRunGuardTripped(func(_ context.Context, e service.WeeklyRunGuardTripped) { events <- e })
	t.Cleanup(func() { service.SetOnWeeklyRunGuardTripped(testHandler.onWeeklyRunGuardTripped) })
	if err := c.enqueue(t); !errors.Is(err, service.ErrStopLoss) || !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("run = %v, want ErrStopLoss wrapping ErrRunBudgetExhausted", err)
	}
	select {
	case e := <-events:
		if !e.StopLoss {
			t.Fatalf("event %+v is not a stop-loss event", e)
		}
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no stop-loss event")
		return service.WeeklyRunGuardTripped{}
	}
}

// seedTokens gives the issue's runs usage rows of n tokens in total.
func seedStopLossTokens(t *testing.T, c budgetCase, n int64) {
	t.Helper()
	dbfx.Exec(t, `INSERT INTO task_usage (task_id, provider, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens)
		SELECT id, 'anthropic', 'claude-test', $2, 0, 0, 0 FROM agent_task_queue WHERE issue_id = $1 ORDER BY created_at LIMIT 1`, c.issueID, n)
	dbfx.Cleanup(t, `DELETE FROM task_usage WHERE task_id IN (SELECT id FROM agent_task_queue WHERE issue_id = $1)`, c.issueID)
}

// linkPR links a PR in the state to the issue.
func linkStopLossPR(t *testing.T, c budgetCase, state string, number int) {
	t.Helper()
	var prID string
	if err := testPool.QueryRow(context.Background(), `INSERT INTO github_pull_request
		(workspace_id, installation_id, repo_owner, repo_name, pr_number, title, state, html_url, pr_created_at, pr_updated_at, head_sha)
		VALUES ($1, 1, 'multica-ai', 'multica', $2, 'stop-loss PR', $3, 'https://example.test/pr', now(), now(), 'abc')
		RETURNING id::text`, testWorkspaceID, number, state).Scan(&prID); err != nil {
		t.Fatalf("seed PR: %v", err)
	}
	dbfx.Exec(t, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, c.issueID, prID)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_pull_request WHERE pull_request_id = $1`, prID)
		testPool.Exec(context.Background(), `DELETE FROM github_pull_request WHERE id = $1`, prID)
	})
}

func (c budgetCase) status(t *testing.T) string {
	t.Helper()
	var s string
	dbfx.QueryRow(t, `SELECT status FROM issue WHERE id = $1`, c.issueID).Scan(&s)
	return s
}

func TestStopLossRefusesTheTwentyFirstRunWithoutAPR(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := stopLossCase(t, 9340001, "block")
	seedRuns(t, c, c.agentID, 20, 2*24*time.Hour)
	if e := c.stopLossRefused(t); e.Runs != 20 {
		t.Fatalf("event runs = %d, want 20", e.Runs)
	}
}

func TestStopLossAllowsTheTwentiethRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := stopLossCase(t, 9340002, "block")
	seedRuns(t, c, c.agentID, 19, 2*24*time.Hour)
	if err := c.enqueue(t); err != nil {
		t.Fatalf("20th run refused: %v", err)
	}
}

func TestStopLossRefusesAtThirtyMillionTokens(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := stopLossCase(t, 9340003, "block")
	seedRuns(t, c, c.agentID, 3, 2*24*time.Hour)
	seedStopLossTokens(t, c, 30_000_000)
	c.stopLossRefused(t)
}

func TestStopLossIgnoresAnIssueWithAnOpenDraftOrMergedPR(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for i, state := range []string{"open", "draft", "merged"} {
		c := stopLossCase(t, int64(9340010+i), "block")
		seedRuns(t, c, c.agentID, 25, 2*24*time.Hour)
		linkStopLossPR(t, c, state, 934010+i)
		if err := c.enqueue(t); err != nil {
			t.Fatalf("run refused with a %s PR linked: %v", state, err)
		}
	}
}

func TestStopLossCountsAClosedPRAsNoPR(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := stopLossCase(t, 9340014, "block")
	seedRuns(t, c, c.agentID, 20, 2*24*time.Hour)
	linkStopLossPR(t, c, "closed", 934014)
	c.stopLossRefused(t)
}

func TestStopLossIgnoresAPlanCoordinatorWaitingOnChildren(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := stopLossCase(t, 9340015, "block")
	dbfx.Exec(t, `UPDATE issue SET title = 'plan(cost): split the work' WHERE id = $1`, c.issueID)
	seedRuns(t, c, c.agentID, 25, 2*24*time.Hour)
	child := dbfx.Insert(t, "issue", testutil.Cols{"workspace_id": testWorkspaceID, "title": "child", "status": "in_progress", "parent_issue_id": c.issueID, "creator_type": "member", "creator_id": testUserID})
	_ = child
	if err := c.enqueue(t); err != nil {
		t.Fatalf("plan coordinator with an open child refused: %v", err)
	}
	// With every child finished the plan is judged like any issue.
	dbfx.Exec(t, `UPDATE issue SET status = 'done' WHERE parent_issue_id = $1`, c.issueID)
	c.stopLossRefused(t)
}

func TestStopLossWarnModeRefusesNothingAndListsTheIssueOncePerDay(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := stopLossCase(t, 9340016, "") // the default mode is warn
	seedRuns(t, c, c.agentID, 20, 2*24*time.Hour)
	events := make(chan service.WeeklyRunGuardTripped, 4)
	service.SetOnWeeklyRunGuardTripped(func(_ context.Context, e service.WeeklyRunGuardTripped) { events <- e })
	t.Cleanup(func() { service.SetOnWeeklyRunGuardTripped(testHandler.onWeeklyRunGuardTripped) })
	if err := c.enqueue(t); err != nil {
		t.Fatalf("run refused in warn mode: %v", err)
	}
	e := <-events
	if !e.StopLoss || !e.Warn {
		t.Fatalf("event %+v, want a stop-loss warning", e)
	}
	testHandler.onWeeklyRunGuardTripped(context.Background(), e)
	testHandler.onWeeklyRunGuardTripped(context.Background(), e)
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'stop_loss_warned'`, c.issueID); n != 1 {
		t.Fatalf("warn rows = %d, want 1", n)
	}
	line, err := testHandler.weeklyRunGuardBrief(context.Background(), parseUUID(testWorkspaceID), time.Now().Add(-time.Hour))
	if err != nil || !strings.Contains(line, "Stop-loss (warn mode) would have stopped runs on") || !strings.Contains(line, "20 runs") {
		t.Fatalf("brief line %q, %v: want the warn list", line, err)
	}
	if n := c.allowCards(t); n != 0 || c.status(t) != "in_progress" {
		t.Fatalf("warn mode opened %d cards, status %s", n, c.status(t))
	}
}

func TestStopLossOffModeNeverRefuses(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := stopLossCase(t, 9340017, "off")
	seedRuns(t, c, c.agentID, 25, 2*24*time.Hour)
	if err := c.enqueue(t); err != nil {
		t.Fatalf("run refused with the stop-loss off: %v", err)
	}
}

func TestStopLossParksTheIssueWithOneCardAndNoAutoApproval(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	setAutoSettings(t, `{"run_budget_auto":{"enabled":true}}`)
	c := stopLossCase(t, 9340018, "block")
	seedRuns(t, c, c.agentID, 20, 2*24*time.Hour)
	// Every run delivered: the auto-approver would approve a plain guard refusal.
	dbfx.Exec(t, `INSERT INTO task_message (task_id, seq, type, output) SELECT id, 1, 'tool_result', '[agent/x 1a2b3c4] feat: change' FROM agent_task_queue WHERE issue_id = $1`, c.issueID)
	e := c.stopLossRefused(t)
	testHandler.onWeeklyRunGuardTripped(context.Background(), e)
	testHandler.onWeeklyRunGuardTripped(context.Background(), e)

	if n := c.allowCards(t); n != 1 {
		t.Fatalf("open cards = %d, want 1", n)
	}
	if n := c.allowanceRuns(t); n != 0 || c.autoApprovals(t) != 0 {
		t.Fatalf("the auto-approver released a stopped issue: allowance %d", n)
	}
	if got := c.status(t); got != "need_decision" {
		t.Fatalf("status = %s, want need_decision", got)
	}
}

func TestStopLossAutoApproverRefusesAStoppedIssue(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	setAutoSettings(t, `{"run_budget_auto":{"enabled":true}}`)
	c := stopLossCase(t, 9340019, "block")
	seedRuns(t, c, c.agentID, 20, 2*24*time.Hour)
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	v, err := service.EvaluateRunBudgetAuto(context.Background(), testHandler.Queries, issue, time.Now().Add(-time.Hour), time.Now())
	if err != nil || v.Approve || v.Reason != "stop_loss" {
		t.Fatalf("run budget verdict = %+v, %v; want stop_loss, not approved", v, err)
	}
	v, err = service.EvaluateRunGuardAuto(context.Background(), testHandler.Queries, issue, time.Now(), 10)
	if err != nil || v.Approve || v.Reason != "stop_loss" {
		t.Fatalf("run guard verdict = %+v, %v; want stop_loss, not approved", v, err)
	}
}

func TestStopLossAnApprovedCardOpensAFreshWindow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := stopLossCase(t, 9340020, "block")
	seedRuns(t, c, c.agentID, 20, 2*24*time.Hour)
	c.stopLossRefused(t)
	dbfx.Exec(t, `INSERT INTO weekly_run_guard_allowance (id, workspace_id, issue_id, runs, action_id) VALUES (gen_random_uuid(), $1, $2, 10, gen_random_uuid())`, testWorkspaceID, c.issueID)
	if err := c.enqueue(t); err != nil {
		t.Fatalf("run refused after Cheese released the issue: %v", err)
	}
}
