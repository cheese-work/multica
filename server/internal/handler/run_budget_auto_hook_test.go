package handler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
)

// exhaust asks for one more run in the spent window, which fires the exhaustion hook.
func (a autoCase) exhaust(t *testing.T) {
	t.Helper()
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(a.c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue); !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("admission = %v, want ErrRunBudgetExhausted", err)
	}
}

// waitFor polls a count until it reaches want, then lets stray duplicates land.
func waitForCount(t *testing.T, want int, query string, args ...any) int {
	t.Helper()
	n := 0
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if n = dbfx.Count(t, query, args...); n >= want {
			break
		}
	}
	time.Sleep(300 * time.Millisecond)
	return dbfx.Count(t, query, args...)
}

func (a autoCase) perIssueCards(t *testing.T) int {
	return dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE issue_id=$1 AND request_fingerprint LIKE 'server:run_budget_action:%'`, a.c.issueID)
}

func TestRunBudgetAutoApprovesAnExhaustedWindowWithProgress(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	a := newAutoCase(t, 2226901)
	a.seed(t, a.c.issueID, goodRuns(5)...)
	a.exhaust(t)
	const topUps = `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='run_budget_top_up' AND actor_type='system' AND details->>'auto'='auto-approver v1' AND (details->>'runs')::int=5`
	if n := waitForCount(t, 1, topUps, a.c.issueID); n != 1 {
		t.Fatalf("auto top-ups = %d, want 1", n)
	}
	if n := a.perIssueCards(t); n != 0 {
		t.Fatalf("per-issue cards = %d, want 0: a person is not asked about an approved window", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id=$1 AND author_type='system' AND content LIKE '%auto-approver v1%' AND content LIKE '%run_budget_auto%'`, a.c.issueID); n != 1 {
		t.Fatalf("audit comments = %d, want 1 naming the rule and the kill switch", n)
	}
	// The refused run itself resumes: no later enqueue is needed, and the issue
	// is not parked in blocked first.
	if n := waitForCount(t, 1, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND status IN ('queued','dispatched')`, a.c.issueID); n != 1 {
		t.Fatalf("queued runs after the auto top-up = %d, want 1: the refused run must resume", n)
	}
	if got := a.c.status(t); got != "in_progress" {
		t.Fatalf("issue is %s after an approved window, want in_progress", got)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='run_budget_exhausted'`, a.c.issueID); n != 0 {
		t.Fatalf("block records = %d, want 0: an approved window never blocks", n)
	}
}

func TestRunBudgetAutoEscalatedIssueKeepsItsRecoveryRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	a := newAutoCaseSteps(t, 2226906, recoverySteps(seededReadyAgentID(t)))
	loop := []autoRun{{status: "failed", err: "go test: exit status 1"}, {status: "failed", err: "go test: exit status 2"}, {status: "failed", err: "go test: exit status 3"}}
	a.seed(t, a.c.issueID, append(loop, goodRuns(2)...)...)
	a.exhaust(t)
	waitForCount(t, 1, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='run_budget_auto_escalated'`, a.c.issueID)
	if n := waitForCount(t, 1, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND task_kind='budget_recovery'`, a.c.issueID); n != 1 {
		t.Fatalf("recovery runs on an escalated blocked issue = %d, want 1", n)
	}
	if got := a.c.status(t); got != "blocked" {
		t.Fatalf("issue is %s, want blocked: an anomaly is never approved", got)
	}
}

func TestRunBudgetAutoAddsLaterAnomaliesToTheDaysCard(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	loop := []autoRun{{status: "failed", err: "go test: exit status 1"}, {status: "failed", err: "go test: exit status 2"}, {status: "failed", err: "go test: exit status 3"}}
	first, second := newAutoCase(t, 2226907), newAutoCase(t, 2226908)
	first.seed(t, first.c.issueID, append(loop, goodRuns(2)...)...)
	first.exhaust(t)
	waitForCount(t, 1, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='run_budget_auto_escalated'`, first.c.issueID)
	// The owner decides the card; a later anomaly the same day must still reach them.
	day := service.RunBudgetAutoDayStart(time.Now()).Format("2006-01-02")
	fingerprint := "server:run_budget_anomaly_batch:" + day
	dbfx.Exec(t, `UPDATE decision_request SET status='answered' WHERE workspace_id=$1 AND request_fingerprint=$2`, testWorkspaceID, fingerprint)
	second.seed(t, second.c.issueID, append(loop, goodRuns(2)...)...)
	second.exhaust(t)
	waitForCount(t, 1, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='run_budget_auto_escalated'`, second.c.issueID)
	const want = `SELECT count(*) FROM chat_message m JOIN decision_request d ON d.chat_session_id=m.chat_session_id WHERE d.workspace_id=$1 AND d.request_fingerprint=$2 AND d.human_user_id=$3 AND m.content LIKE '%' || $4 || '%'`
	if n := waitForCount(t, 1, want, testWorkspaceID, fingerprint, testUserID, "mention://issue/"+second.c.issueID); n != 1 {
		t.Fatalf("follow-up messages naming the second issue on the day's card = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE workspace_id=$1 AND human_user_id=$2 AND request_fingerprint=$3`, testWorkspaceID, testUserID, fingerprint); n != 1 {
		t.Fatalf("batch cards for the owner today = %d, want still 1", n)
	}
	line, err := testHandler.runBudgetAutoBrief(context.Background(), parseUUID(testWorkspaceID), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var number int
	if err := testPool.QueryRow(context.Background(), `SELECT number FROM issue WHERE id=$1`, second.c.issueID).Scan(&number); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, fmt.Sprintf("-%d", number)) {
		t.Fatalf("brief line = %q, want it to name the second issue (number %d)", line, number)
	}
}

func TestRunBudgetAutoEscalatesALoopInOneBatchCardPerDay(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	loop := []autoRun{{status: "failed", err: "go test: exit status 1"}, {status: "failed", err: "go test: exit status 2"}, {status: "failed", err: "go test: exit status 3"}}
	first, second := newAutoCase(t, 2226902), newAutoCase(t, 2226903)
	for _, a := range []autoCase{first, second} {
		a.seed(t, a.c.issueID, append(loop, goodRuns(2)...)...)
		a.exhaust(t)
	}
	for _, a := range []autoCase{first, second} {
		if n := waitForCount(t, 1, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='run_budget_auto_escalated' AND details->>'kind'='loop'`, a.c.issueID); n != 1 {
			t.Fatalf("escalations on %s = %d, want 1 of kind loop", a.c.issueID, n)
		}
		if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='run_budget_top_up'`, a.c.issueID); n != 0 {
			t.Fatalf("top-ups on a looping issue = %d, want 0", n)
		}
		if n := a.perIssueCards(t); n != 0 {
			t.Fatalf("per-issue cards = %d, want 0: anomalies go to the batch card", n)
		}
		if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id=$1 AND author_type='system' AND content LIKE '%loop%'`, a.c.issueID); n != 1 {
			t.Fatalf("audit comments = %d, want 1", n)
		}
	}
	day := service.RunBudgetAutoDayStart(time.Now()).Format("2006-01-02")
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE workspace_id=$1 AND human_user_id=$2 AND request_fingerprint=$3`, testWorkspaceID, testUserID, "server:run_budget_anomaly_batch:"+day); n != 1 {
		t.Fatalf("batch cards for the owner today = %d, want exactly 1 for two anomalies", n)
	}
}

func TestRunBudgetAutoEnvelopeFallsBackToThePerIssueCard(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	a := newAutoCase(t, 2226904)
	setAutoSettings(t, `{"run_budget_auto":{"enabled":true,"daily_envelope":3}}`)
	a.seed(t, a.c.issueID, goodRuns(5)...)
	a.exhaust(t)
	if n := waitForCount(t, 1, `SELECT count(*) FROM decision_request WHERE issue_id=$1 AND request_fingerprint LIKE 'server:run_budget_action:%'`, a.c.issueID); n != 1 {
		t.Fatalf("per-issue cards = %d, want 1: a spent envelope hands the window to a person", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='run_budget_top_up'`, a.c.issueID); n != 0 {
		t.Fatalf("top-ups = %d, want 0", n)
	}
}

func TestRunBudgetAutoBriefCountsDecisions(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	a := newAutoCase(t, 2226905)
	a.seed(t, a.c.issueID, goodRuns(5)...)
	a.exhaust(t)
	waitForCount(t, 1, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='run_budget_top_up'`, a.c.issueID)
	line, err := testHandler.runBudgetAutoBrief(context.Background(), parseUUID(testWorkspaceID), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "auto-approver v1") || !strings.Contains(line, "approved 1") || !strings.Contains(line, "escalated 0") {
		t.Fatalf("brief line = %q, want the rule, approved 1 and escalated 0", line)
	}
}
