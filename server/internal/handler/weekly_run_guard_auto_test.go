package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-6248: run-guard allowances follow the run-budget auto-approver's rules,
// and the guard has an off | warn | block mode.

// guardAutoCase is a weekly-guard issue with 30 runs in the window: delivered
// of them committed, looping of them failed with the same error. The test
// member owns the workspace. The guard has tripped once; the hook has not run.
func guardAutoCase(t *testing.T, installationID int64, delivered, looping int) (budgetCase, service.WeeklyRunGuardTripped) {
	t.Helper()
	c := weeklyCase(t, installationID)
	var role string
	dbfx.QueryRow(t, `SELECT role FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, testUserID).Scan(&role)
	dbfx.Exec(t, `UPDATE member SET role = 'owner' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, testUserID)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE member SET role = $3 WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, testUserID, role)
	})
	dbfx.Exec(t, `UPDATE issue SET requested_by_id = $2 WHERE id = $1`, c.issueID, testUserID)
	dbfx.Cleanup(t, `DELETE FROM decision_answer WHERE decision_request_id IN (SELECT id FROM decision_request WHERE issue_id = $1)`, c.issueID)
	dbfx.Cleanup(t, `DELETE FROM decision_request WHERE issue_id = $1`, c.issueID)
	dbfx.Cleanup(t, `DELETE FROM weekly_run_guard_refusal WHERE issue_id = $1`, c.issueID)
	dbfx.Cleanup(t, `DELETE FROM weekly_run_guard_allowance WHERE issue_id = $1`, c.issueID)
	for i := 0; i < 30; i++ {
		at := time.Now().Add(-2 * 24 * time.Hour).Add(time.Duration(i) * time.Minute)
		cols := testutil.Cols{
			"issue_id": c.issueID, "status": "completed", "created_at": at, "started_at": at, "completed_at": at,
			"trigger_evidence_kind": "comment",
		}
		if i >= 30-looping {
			cols["status"], cols["error"] = "failed", "boom: exit status 1"
		}
		taskID := dbfx.Task(t, c.agentID, cols)
		if i < delivered {
			dbfx.Insert(t, "task_message", testutil.Cols{"task_id": taskID, "seq": 1, "type": "tool_result", "output": "[agent/x 1a2b3c4] feat: change\n 1 file changed, 2 insertions(+)"})
		}
	}
	return c, c.weeklyTripped(t)
}

func (c budgetCase) allowanceRuns(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT coalesce(sum(runs),0) FROM weekly_run_guard_allowance WHERE issue_id = $1`, c.issueID)
}

func (c budgetCase) autoApprovals(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'run_guard_auto_approved'`, c.issueID)
}

// AC1: with run_budget_auto on, a guard refusal on an issue with progress is
// approved without a card; with it off, the card opens and nothing is granted.
func TestRunGuardAutoApprovesAllowanceWithProgress(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	setAutoSettings(t, `{"run_budget_auto":{"enabled":true}}`)
	c, e := guardAutoCase(t, 6248001, 10, 0)
	testHandler.onWeeklyRunGuardTripped(context.Background(), e)

	if n := c.allowCards(t); n != 0 {
		t.Fatalf("open allow cards = %d, want 0 after an auto approval", n)
	}
	if n := c.allowanceRuns(t); n != 10 {
		t.Fatalf("allowance = %d runs, want 10", n)
	}
	if n := c.autoApprovals(t); n != 1 {
		t.Fatalf("auto approval records = %d, want 1", n)
	}
	if n := c.queuedRuns(t); n != 1 {
		t.Fatalf("queued runs = %d, want the refused trigger once", n)
	}
}

func TestRunGuardAutoStaysPendingWhenAutoApproverIsOff(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	setAutoSettings(t, `{"run_budget_auto":{"enabled":false}}`)
	c, e := guardAutoCase(t, 6248002, 10, 0)
	testHandler.onWeeklyRunGuardTripped(context.Background(), e)

	if n := c.allowCards(t); n != 1 {
		t.Fatalf("open allow cards = %d, want 1 with the kill switch off", n)
	}
	if n := c.allowanceRuns(t); n != 0 {
		t.Fatalf("allowance = %d runs, want 0", n)
	}
}

// AC2: a loop (the same failure in 3 runs) keeps its card for a person, even
// with progress in the window.
func TestRunGuardAutoKeepsTheCardOfALoop(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	setAutoSettings(t, `{"run_budget_auto":{"enabled":true}}`)
	c, e := guardAutoCase(t, 6248003, 10, 3)
	testHandler.onWeeklyRunGuardTripped(context.Background(), e)

	if n := c.allowCards(t); n != 1 {
		t.Fatalf("open allow cards = %d, want 1 for a loop", n)
	}
	if n := c.allowanceRuns(t); n != 0 {
		t.Fatalf("allowance = %d runs, want 0 for a loop", n)
	}
}

// AC2: no progress keeps the card too.
func TestRunGuardAutoKeepsTheCardWithoutProgress(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	setAutoSettings(t, `{"run_budget_auto":{"enabled":true}}`)
	c, e := guardAutoCase(t, 6248004, 0, 0)
	testHandler.onWeeklyRunGuardTripped(context.Background(), e)

	if n := c.allowCards(t); n != 1 {
		t.Fatalf("open allow cards = %d, want 1 without progress", n)
	}
	if n := c.allowanceRuns(t); n != 0 {
		t.Fatalf("allowance = %d runs, want 0 without progress", n)
	}
}

// AC3: an open card with progress is approved and closed on the next pass
// after the auto-approver is switched on.
func TestRunGuardAutoSweepApprovesOpenCardsWithProgress(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	setAutoSettings(t, `{"run_budget_auto":{"enabled":false}}`)
	c, e := guardAutoCase(t, 6248005, 10, 0)
	testHandler.onWeeklyRunGuardTripped(context.Background(), e)
	if n := c.allowCards(t); n != 1 {
		t.Fatalf("open allow cards = %d, want 1 before the switch", n)
	}

	setAutoSettings(t, `{"run_budget_auto":{"enabled":true}}`)
	testHandler.sweepRunGuardAuto(context.Background())

	if n := c.allowCards(t); n != 0 {
		t.Fatalf("open allow cards = %d, want 0 after the pass", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_request r JOIN decision_action a ON a.decision_request_id = r.id
		WHERE r.issue_id = $1 AND r.status = 'cancelled' AND a.action_type = $2 AND a.state = 'cancelled'`, c.issueID, weeklyAllowType); n != 1 {
		t.Fatalf("closed cards = %d, want 1", n)
	}
	if n := c.allowanceRuns(t); n != 10 {
		t.Fatalf("allowance = %d runs, want 10", n)
	}
	if n := c.queuedRuns(t); n != 1 {
		t.Fatalf("queued runs = %d, want the refused trigger once", n)
	}
	// The outcome is the approval, not a bare cancel: the refusal is approved or
	// resumed, one auto-approval is logged, and one audit comment says so.
	if n := dbfx.Count(t, `SELECT count(*) FROM weekly_run_guard_refusal WHERE issue_id = $1 AND state IN ('approved', 'resumed')`, c.issueID); n != 1 {
		t.Fatalf("approved refusals = %d, want 1", n)
	}
	if n := c.autoApprovals(t); n != 1 {
		t.Fatalf("auto-approvals logged = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content LIKE 'Weekly run guard: % approved 10 more runs%'`, c.issueID); n != 1 {
		t.Fatalf("approval comments = %d, want 1", n)
	}
	// A second pass grants nothing more.
	testHandler.sweepRunGuardAuto(context.Background())
	if n := c.allowanceRuns(t); n != 10 {
		t.Fatalf("allowance after a second pass = %d runs, want 10", n)
	}
}

// A pass walks every open refusal: 100 older cards the auto-approver keeps
// (here: no such issue) must not hide a newer card that qualifies.
func TestRunGuardAutoSweepReachesCardsBehindARetainedPage(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	setAutoSettings(t, `{"run_budget_auto":{"enabled":false}}`)
	c, e := guardAutoCase(t, 6248009, 10, 0)
	testHandler.onWeeklyRunGuardTripped(context.Background(), e)
	dbfx.Cleanup(t, `DELETE FROM weekly_run_guard_refusal WHERE trigger = 'che6248-retained'`)
	dbfx.Exec(t, `INSERT INTO weekly_run_guard_refusal (id, workspace_id, issue_id, trigger, runs, state, created_at)
		SELECT gen_random_uuid(), $1, gen_random_uuid(), 'che6248-retained', 30, 'open', now() - interval '30 days' + n * interval '1 second'
		FROM generate_series(1, 101) AS n`, testWorkspaceID)
	setAutoSettings(t, `{"run_budget_auto":{"enabled":true}}`)
	testHandler.sweepRunGuardAuto(context.Background())

	if n := c.allowanceRuns(t); n != 10 {
		t.Fatalf("allowance = %d runs, want 10 for the card behind 101 retained ones", n)
	}
}

func TestRunGuardAutoSweepLeavesALoopCardOpen(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	setAutoSettings(t, `{"run_budget_auto":{"enabled":false}}`)
	c, e := guardAutoCase(t, 6248006, 10, 3)
	testHandler.onWeeklyRunGuardTripped(context.Background(), e)
	setAutoSettings(t, `{"run_budget_auto":{"enabled":true}}`)
	testHandler.sweepRunGuardAuto(context.Background())

	if n := c.allowCards(t); n != 1 {
		t.Fatalf("open allow cards = %d, want 1 for a loop", n)
	}
	if n := c.allowanceRuns(t); n != 0 {
		t.Fatalf("allowance = %d runs, want 0", n)
	}
}

// AC4: warn refuses nothing and records the would-be refusal.
func TestWeeklyRunGuardWarnModeRefusesNothingAndRecordsIt(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	setAutoSettings(t, `{"run_guard":{"mode":"warn"}}`)
	c := weeklyCase(t, 6248007)
	seedRuns(t, c, c.agentID, 30, 2*24*time.Hour)
	events := make(chan service.WeeklyRunGuardTripped, 1)
	service.SetOnWeeklyRunGuardTripped(func(_ context.Context, e service.WeeklyRunGuardTripped) { events <- e })
	t.Cleanup(func() { service.SetOnWeeklyRunGuardTripped(testHandler.onWeeklyRunGuardTripped) })

	if err := c.enqueue(t); err != nil {
		t.Fatalf("run refused in warn mode: %v", err)
	}
	var e service.WeeklyRunGuardTripped
	select {
	case e = <-events:
	case <-time.After(5 * time.Second):
		t.Fatal("no would-be refusal was reported")
	}
	if !e.Warn || e.Runs != 30 {
		t.Fatalf("event = %+v, want a warn event with 30 runs", e)
	}
	testHandler.onWeeklyRunGuardTripped(context.Background(), e)
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='weekly_run_guard_warned'`, c.issueID); n != 1 {
		t.Fatalf("would-be refusal records = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM weekly_run_guard_refusal WHERE issue_id=$1`, c.issueID); n != 0 {
		t.Fatalf("refusals kept = %d, want 0 in warn mode", n)
	}
	if n := c.allowCards(t); n != 0 {
		t.Fatalf("open allow cards = %d, want 0 in warn mode", n)
	}
	var wsID string
	dbfx.QueryRow(t, `SELECT workspace_id::text FROM issue WHERE id=$1`, c.issueID).Scan(&wsID)
	line, err := testHandler.weeklyRunGuardBrief(context.Background(), parseUUID(wsID), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "would have stopped") || !strings.Contains(line, "30 runs") {
		t.Fatalf("brief line %q does not show the would-be refusal", line)
	}
}

// AC4: block keeps today's behavior, and so does a mode the setting cannot read.
func TestWeeklyRunGuardBlockModeStillRefuses(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for i, settings := range []string{`{"run_guard":{"mode":"block"}}`, `{"run_guard":{"mode":"nonsense"}}`} {
		setAutoSettings(t, settings)
		c := weeklyCase(t, int64(6248010+i))
		seedRuns(t, c, c.agentID, 30, 2*24*time.Hour)
		if err := c.enqueue(t); !errors.Is(err, service.ErrWeeklyRunGuard) {
			t.Fatalf("settings %s: run = %v, want ErrWeeklyRunGuard", settings, err)
		}
	}
}

func TestWeeklyRunGuardOffModeAdmitsTheRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	setAutoSettings(t, `{"run_guard":{"mode":"off"}}`)
	c := weeklyCase(t, 6248012)
	seedRuns(t, c, c.agentID, 30, 2*24*time.Hour)
	if err := c.enqueue(t); err != nil {
		t.Fatalf("run refused with the guard off: %v", err)
	}
}

func TestWeeklyRunGuardModeFromSettings(t *testing.T) {
	for settings, want := range map[string]string{
		``:                              service.WeeklyRunGuardBlock,
		`{}`:                            service.WeeklyRunGuardBlock,
		`{"run_guard":{"mode":"warn"}}`: service.WeeklyRunGuardWarn,
		`{"run_guard":{"mode":"off"}}`:  service.WeeklyRunGuardOff,
		`{"run_guard":{"mode":"x"}}`:    service.WeeklyRunGuardBlock,
		`not json`:                      service.WeeklyRunGuardBlock,
	} {
		if got := service.WeeklyRunGuardModeFromSettings([]byte(settings)); got != want {
			t.Errorf("settings %q: mode = %q, want %q", settings, got, want)
		}
	}
}

// The budget read names the guard as blocking only while it refuses.
func TestIssueBudgetGetDoesNotReportAWarnOnlyGuardAsBlocking(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	setAutoSettings(t, `{"run_guard":{"mode":"warn"}}`)
	c := weeklyCase(t, 6248008)
	seedRuns(t, c, c.agentID, 30, 2*24*time.Hour)
	var got struct {
		Blocking   []string `json:"blocking"`
		WeeklyRuns int64    `json:"weekly_runs"`
	}
	request := withURLParam(newRequest(http.MethodGet, "/api/issues/"+c.issueID+"/run-budget", nil), "id", c.issueID)
	testutil.Call(t, testHandler.GetIssueRunBudget, request).Want(http.StatusOK).JSON(&got)
	if got.WeeklyRuns != 30 {
		t.Fatalf("weekly runs = %d, want 30", got.WeeklyRuns)
	}
	if len(got.Blocking) != 0 {
		t.Fatalf("blocking = %v, want none in warn mode", got.Blocking)
	}
}
