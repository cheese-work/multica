package handler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
)

// weeklyCase is an in_progress agent-assigned issue whose run budget and loop
// breaker are out of reach, so only the weekly guard can refuse a run.
func weeklyCase(t *testing.T, installationID int64) budgetCase {
	t.Helper()
	t.Setenv("MULTICA_WEEKLY_RUN_GUARD_MAX_RUNS", "30")
	return newBudgetCase(t, installationID, recoverySteps(seededReadyAgentID(t)), 1000)
}

func (c budgetCase) weeklyTripped(t *testing.T) service.WeeklyRunGuardTripped {
	t.Helper()
	events := make(chan service.WeeklyRunGuardTripped, 1)
	service.SetOnWeeklyRunGuardTripped(func(_ context.Context, e service.WeeklyRunGuardTripped) { events <- e })
	t.Cleanup(func() { service.SetOnWeeklyRunGuardTripped(testHandler.onWeeklyRunGuardTripped) })
	if err := c.enqueue(t); !errors.Is(err, service.ErrWeeklyRunGuard) {
		t.Fatalf("run = %v, want ErrWeeklyRunGuard", err)
	}
	select {
	case e := <-events:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no weekly-guard event")
		return service.WeeklyRunGuardTripped{}
	}
}

func TestWeeklyRunGuardRefusesRunAfterThirtyInSevenDays(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := weeklyCase(t, 5720001)
	seedRuns(t, c, c.agentID, 30, 2*24*time.Hour)
	e := c.weeklyTripped(t)
	if e.Runs != 30 {
		t.Fatalf("event runs = %d, want 30", e.Runs)
	}
	if len(e.Outcomes) != 3 || e.Outcomes[0] != "completed" {
		t.Fatalf("outcomes = %v, want the last 3 run statuses", e.Outcomes)
	}
}

func TestWeeklyRunGuardAllowsThirtiethRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := weeklyCase(t, 5720002)
	seedRuns(t, c, c.agentID, 29, 2*24*time.Hour)
	if err := c.enqueue(t); err != nil {
		t.Fatalf("30th run refused: %v", err)
	}
}

func TestWeeklyRunGuardIgnoresRunsOlderThanSevenDays(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := weeklyCase(t, 5720003)
	seedRuns(t, c, c.agentID, 40, 8*24*time.Hour)
	if err := c.enqueue(t); err != nil {
		t.Fatalf("run refused for runs older than 7 days: %v", err)
	}
}

func TestWeeklyRunGuardCanBeTurnedOff(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := weeklyCase(t, 5720004)
	t.Setenv("MULTICA_WEEKLY_RUN_GUARD_MAX_RUNS", "0")
	seedRuns(t, c, c.agentID, 40, 2*24*time.Hour)
	if err := c.enqueue(t); err != nil {
		t.Fatalf("run refused with the guard off: %v", err)
	}
}

func TestWeeklyRunGuardIncidentIsRecordedOnceAndReachesTheBrief(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := weeklyCase(t, 5720005)
	seedRuns(t, c, c.agentID, 30, 2*24*time.Hour)
	e := c.weeklyTripped(t)
	testHandler.onWeeklyRunGuardTripped(context.Background(), e)
	testHandler.onWeeklyRunGuardTripped(context.Background(), e)

	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='weekly_run_guard_tripped'`, c.issueID); n != 1 {
		t.Fatalf("incident rows = %d, want 1", n)
	}
	var wsID string
	if err := testPool.QueryRow(context.Background(), `SELECT workspace_id::text FROM issue WHERE id=$1`, c.issueID).Scan(&wsID); err != nil {
		t.Fatal(err)
	}
	line, err := testHandler.weeklyRunGuardBrief(context.Background(), parseUUID(wsID), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"30 runs", "completed, completed, completed"} {
		if !strings.Contains(line, want) {
			t.Fatalf("brief line %q lacks %q", line, want)
		}
	}
}
