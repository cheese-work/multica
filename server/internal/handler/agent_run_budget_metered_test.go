package handler

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
)

// The metered-spend checks of the agent run-budget cap (CHE-3251): a grant is
// refused when the month's metered spend plus the credit agents hold would pass
// 80% of the metered limit, and a run on agent credit is refused at the same
// line. Every test sets the ceiling relative to what the shared test database
// already holds, so earlier tests' tasks and credit do not move the boundary.

func roundUp4(n int64) int64   { return (n + 3) / 4 * 4 }
func roundDown4(n int64) int64 { return n / 4 * 4 }

// setCeiling sets the metered limit so that 80% of it is ceiling (a multiple of
// 4): on the handler's config (grant time) and on the admission gate.
func setCeiling(t *testing.T, ceiling int64) {
	t.Helper()
	limit := ceiling / 4 * 5
	testHandler.AgentRunBudget.MeteredLimitMicroUSD = limit
	service.SetMeteredGate(&service.MeteredGate{PerRunMicroUSD: micro, LimitMicroUSD: limit})
}

func meteredBudget(t *testing.T) {
	t.Helper()
	enableAgentBudget(t)
	t.Cleanup(func() { service.SetMeteredGate(nil) })
	setCeiling(t, roundUp4(1_000_000*micro))
}

func position(t *testing.T) service.MeteredPosition {
	t.Helper()
	p, err := service.ReadMeteredPosition(context.Background(), testHandler.Queries, time.Now(), micro, pgtype.UUID{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// addSpend records spend usd (in micro-USD) as a priced usage row on a task.
func addSpend(t *testing.T, amount int64) string {
	t.Helper()
	agentID, taskID, _ := raisingAgent(t)
	_ = agentID
	dbfx.Exec(t, `INSERT INTO task_usage (task_id, provider, model, input_tokens, output_tokens, cost_usd_ticks) VALUES ($1, 'p', 'm', 1, 1, $2)`, taskID, amount*10_000)
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM task_usage WHERE task_id=$1`, taskID) })
	return taskID
}

// creditIssue returns a budgeted issue that holds one run of agent credit and
// no base allowance: spent to its cap, then raised by an agent by one run.
func creditIssue(t *testing.T, id int64, base int) edgeCase {
	t.Helper()
	c := budgetIssue(t, id, base)
	_, _, asAgent := raisingAgent(t)
	if w := raiseBudget(t, c.issueID, map[string]any{"runs": base + 1}, asAgent); w.Code != http.StatusOK {
		t.Fatalf("raise = %d: %s", w.Code, w.Body.String())
	}
	return c
}

func enqueueOn(t *testing.T, c edgeCase) error {
	t.Helper()
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	_, err = testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue)
	return err
}

func taskCount(t *testing.T, c edgeCase) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, c.issueID)
}

// A run on agent credit is admitted exactly up to the ceiling: spend plus the
// runs in flight plus this run, at USD 1 a run.
func TestRunOnAgentCreditIsAdmittedAtTheCeilingAndRefusedPastIt(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	meteredBudget(t)
	c := creditIssue(t, 3251040, 2)
	p := position(t)
	committed := p.Spent + p.InFlight
	tasks := taskCount(t, c)

	setCeiling(t, roundDown4(committed+micro-1))
	if err := enqueueOn(t, c); !errors.Is(err, service.ErrRunMeteredLimit) || !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("run one micro-dollar past the ceiling = %v, want ErrRunMeteredLimit (a run-budget refusal)", err)
	}
	if got := taskCount(t, c); got != tasks {
		t.Fatalf("a refused run left %d new task rows", got-tasks)
	}
	if got := ledgerBalance(t, c.issueID); got != 1 {
		t.Fatalf("balance after the refusal = %d, want 1 (the credit is kept)", got)
	}

	setCeiling(t, roundUp4(committed+micro))
	if err := enqueueOn(t, c); err != nil {
		t.Fatalf("run exactly at the ceiling = %v, want admitted", err)
	}
	if got := ledgerBalance(t, c.issueID); got != 0 {
		t.Fatalf("balance after the admitted run = %d, want 0", got)
	}
}

// Two runs on agent credit that race for the last room under the ceiling: one is admitted.
func TestConcurrentAgentCreditRunsAdmitOnlyWhatFits(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	meteredBudget(t)
	a, b := creditIssue(t, 3251041, 2), creditIssue(t, 3251042, 2)
	p := position(t)
	setCeiling(t, roundUp4(p.Spent+p.InFlight+micro))

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, c := range []edgeCase{a, b} {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = enqueueOn(t, c) }()
	}
	wg.Wait()
	admitted, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			admitted++
		case errors.Is(err, service.ErrRunMeteredLimit):
			refused++
		default:
			t.Fatalf("unexpected error %v", err)
		}
	}
	if admitted != 1 || refused != 1 {
		t.Fatalf("admitted %d, refused %d; want one of each", admitted, refused)
	}
}

// Runs inside the base allowance are never gated, only the runs the agent's credit pays for.
func TestRunsInsideTheBaseAllowanceAreNotGated(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	meteredBudget(t)
	c := newEdgeCase(t, 3251044, mergeEdgeSteps)
	dbfx.Exec(t, `UPDATE issue_workflow SET max_runs_per_issue=2, run_budget_since=now()-interval '1 hour' WHERE id=$1`, c.workflowID)
	dbfx.Exec(t, `UPDATE issue SET status='in_progress', assignee_type='agent', assignee_id=$2 WHERE id=$1`, c.issueID, seededReadyAgentID(t))
	_, _, asAgent := raisingAgent(t)
	if w := raiseBudget(t, c.issueID, map[string]any{"runs": 3}, asAgent); w.Code != http.StatusOK {
		t.Fatalf("raise = %d: %s", w.Code, w.Body.String())
	}
	setCeiling(t, 0) // nothing is under the ceiling
	for i := 1; i <= 2; i++ {
		if err := enqueueOn(t, c); err != nil {
			t.Fatalf("base-allowance run %d = %v, want admitted past the ceiling", i, err)
		}
		finishRuns(t, c)
	}
	if err := enqueueOn(t, c); !errors.Is(err, service.ErrRunMeteredLimit) {
		t.Fatalf("the run the agent's credit pays for = %v, want ErrRunMeteredLimit", err)
	}
}

// Credit granted earlier is kept when the line is reached, and the next ICT
// month reopens it.
func TestRefusedCreditRunReopensWhenTheMonthTurns(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	meteredBudget(t)
	c := creditIssue(t, 3251045, 2)
	base := position(t)
	taskID := addSpend(t, 5*micro)
	setCeiling(t, roundDown4(base.Spent+base.InFlight+5*micro+micro-1))
	if err := enqueueOn(t, c); !errors.Is(err, service.ErrRunMeteredLimit) {
		t.Fatalf("run at the line = %v, want ErrRunMeteredLimit", err)
	}
	dbfx.Exec(t, `UPDATE task_usage SET created_at = created_at - interval '40 days' WHERE task_id=$1`, taskID)
	if err := enqueueOn(t, c); err != nil {
		t.Fatalf("run in the next month = %v, want admitted", err)
	}
}

// An unreadable spend refuses the run: the gate fails closed.
func TestAgentCreditRunIsRefusedWhenSpendCannotBeRead(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	meteredBudget(t)
	c := creditIssue(t, 3251046, 2)
	tasks := taskCount(t, c)
	dbfx.Exec(t, `ALTER TABLE task_usage RENAME COLUMN cost_usd_ticks TO cost_usd_ticks_broken`)
	restored := false
	restore := func() {
		if !restored {
			restored = true
			dbfx.Exec(t, `ALTER TABLE task_usage RENAME COLUMN cost_usd_ticks_broken TO cost_usd_ticks`)
		}
	}
	t.Cleanup(restore)
	err := enqueueOn(t, c)
	restore()
	if err == nil {
		t.Fatal("a run was admitted while spend could not be read")
	}
	if got := taskCount(t, c); got != tasks {
		t.Fatalf("a refused run left %d new task rows", got-tasks)
	}
	if got := ledgerBalance(t, c.issueID); got != 1 {
		t.Fatalf("balance after the failed read = %d, want 1 (the credit is kept)", got)
	}
}

// A run in flight counts at the price of a run until its usage is reported,
// then at what it cost.
func TestInFlightRunCountsAtThePriceOfARunUntilUsageIsReported(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	meteredBudget(t)
	before := position(t)
	_, taskID, _ := raisingAgent(t)
	during := position(t)
	if got := during.InFlight - before.InFlight; got != micro {
		t.Fatalf("a run with no usage counts %d, want %d", got, micro)
	}
	dbfx.Exec(t, `INSERT INTO task_usage (task_id, provider, model, input_tokens, output_tokens, cost_usd_ticks) VALUES ($1, 'p', 'm', 1, 1, $2)`, taskID, 250_000*10_000)
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM task_usage WHERE task_id=$1`, taskID) })
	after := position(t)
	if after.InFlight != before.InFlight || after.Spent-before.Spent != 250_000 {
		t.Fatalf("after usage: in-flight +%d, spent +%d; want +0 and +250000", after.InFlight-before.InFlight, after.Spent-before.Spent)
	}
}

// Grant time: the month's spend plus the credit agents hold plus the raise must
// fit under the ceiling, or the raise is refused with the monthly limit.
func TestAgentRaiseIsRefusedPastTheMeteredCeiling(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	meteredBudget(t)
	c := budgetIssue(t, 3251047, 2)
	_, _, asAgent := raisingAgent(t)
	p := position(t)
	committed := p.Spent + p.InFlight + p.Credit

	setCeiling(t, roundDown4(committed+micro-1))
	w := raiseBudget(t, c.issueID, map[string]any{"runs": 3}, asAgent)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("raise past the ceiling = %d, want 429: %s", w.Code, w.Body.String())
	}
	got := decodeRefusal(t, w)
	if got.Reason != "monthly_limit" || got.RequestedRuns != 1 || got.ResetsAt == "" || got.RetryAfterSeconds == "" {
		t.Fatalf("refusal = %+v, want monthly_limit with a reset", got)
	}
	if overrideOf(t, c.issueID) != 0 {
		t.Fatal("a refused raise still stored an override")
	}

	setCeiling(t, roundUp4(committed+micro))
	if w := raiseBudget(t, c.issueID, map[string]any{"runs": 3}, asAgent); w.Code != http.StatusOK {
		t.Fatalf("raise exactly at the ceiling = %d, want 200: %s", w.Code, w.Body.String())
	}
	// The credit just granted now counts against the next grant.
	if w := raiseBudget(t, c.issueID, map[string]any{"runs": 4}, asAgent); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second raise = %d, want 429 (the first one's credit counts): %s", w.Code, w.Body.String())
	}
}

// A run that finished without reporting usage stays counted at the price of a
// run until its usage arrives; a run that never started is not counted.
func TestFinishedRunWithoutUsageStaysCountedUntilUsageArrives(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	meteredBudget(t)
	before := position(t)
	_, taskID, _ := raisingAgent(t)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed', completed_at=now() WHERE id=$1`, taskID)
	if got := position(t).InFlight - before.InFlight; got != micro {
		t.Fatalf("a finished run with no usage counts %d, want %d", got, micro)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='cancelled', started_at=NULL WHERE id=$1`, taskID)
	if got := position(t).InFlight - before.InFlight; got != 0 {
		t.Fatalf("a run that never started counts %d, want 0", got)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='failed', started_at=now() WHERE id=$1`, taskID)
	if got := position(t).InFlight - before.InFlight; got != micro {
		t.Fatalf("a failed run with no usage counts %d, want %d", got, micro)
	}
	dbfx.Exec(t, `INSERT INTO task_usage (task_id, provider, model, input_tokens, output_tokens, cost_usd_ticks) VALUES ($1, 'p', 'm', 1, 1, $2)`, taskID, 250_000*10_000)
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM task_usage WHERE task_id=$1`, taskID) })
	after := position(t)
	if after.InFlight != before.InFlight || after.Spent-before.Spent != 250_000 {
		t.Fatalf("after late usage: in-flight +%d, spent +%d; want +0 and +250000", after.InFlight-before.InFlight, after.Spent-before.Spent)
	}
}

// Grant time prices the runs that have not reported usage, like run time does.
func TestAgentRaiseCountsRunsWithoutUsageAtTheCeiling(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	meteredBudget(t)
	c := budgetIssue(t, 3251048, 2)
	_, _, asAgent := raisingAgent(t) // its running task has reported no usage
	p := position(t)
	if p.InFlight < micro {
		t.Fatalf("setup: in-flight = %d, want at least one run", p.InFlight)
	}

	setCeiling(t, roundUp4(p.Spent+p.Credit+micro)) // room only if in-flight runs were free
	w := raiseBudget(t, c.issueID, map[string]any{"runs": 3}, asAgent)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("raise with in-flight runs over the ceiling = %d, want 429: %s", w.Code, w.Body.String())
	}
	if got := decodeRefusal(t, w); got.Reason != "monthly_limit" || got.UsedUSD != usd(p.Spent+p.InFlight+p.Credit) {
		t.Fatalf("refusal = %+v, want monthly_limit with used %v", got, usd(p.Spent+p.InFlight+p.Credit))
	}

	setCeiling(t, roundUp4(p.Spent+p.InFlight+p.Credit+micro))
	if w := raiseBudget(t, c.issueID, map[string]any{"runs": 3}, asAgent); w.Code != http.StatusOK {
		t.Fatalf("raise exactly at the ceiling = %d, want 200: %s", w.Code, w.Body.String())
	}
}

// Agent credit is used up by the run it pays for: a person's raise or top-up
// after that is the person's, and is not gated at the ceiling.
func TestHumanFundingAfterAgentCreditWasSpentIsNotGated(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fund := map[string]func(*testing.T, edgeCase){
		"raise": func(t *testing.T, c edgeCase) {
			if w := raiseBudget(t, c.issueID, map[string]any{"runs": 4}, nil); w.Code != http.StatusOK {
				t.Fatalf("member raise = %d: %s", w.Code, w.Body.String())
			}
		},
		"top-up": func(t *testing.T, c edgeCase) {
			if w := topUpBudget(t, c.issueID, nil); w.Code != http.StatusOK {
				t.Fatalf("member top-up = %d: %s", w.Code, w.Body.String())
			}
		},
	}
	for name, run := range fund {
		t.Run(name, func(t *testing.T) {
			meteredBudget(t)
			c := creditIssue(t, 3251049, 2)
			before := position(t)
			if err := enqueueOn(t, c); err != nil {
				t.Fatalf("run on agent credit = %v, want admitted", err)
			}
			finishRuns(t, c)
			if got := before.Credit - position(t).Credit; got != micro {
				t.Fatalf("agent credit left dropped by %d, want %d (one run used)", got, micro)
			}
			run(t, c)
			setCeiling(t, 0) // nothing is under the ceiling
			if err := enqueueOn(t, c); err != nil {
				t.Fatalf("run paid by a person = %v, want admitted past the ceiling", err)
			}
		})
	}
}
