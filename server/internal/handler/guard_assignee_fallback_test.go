package handler

import (
	"context"
	"testing"
)

// CHE-6101: an agent-assigned issue with no owning squad is alerted through its
// assignee instead of failing silently (CHE-5716 incident).

func (fixture idleActFixture) assigneeTasks(test *testing.T) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'
		AND squad_id IS NULL AND NOT is_leader_task AND handoff_note LIKE 'Idle started-issue guard:%'`, fixture.issue, fixture.agent)
}

func TestIdleGuardAlertsAssigneeWithoutSquad(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	dbfx.Exec(test, `DELETE FROM squad_member WHERE squad_id = $1`, fixture.squad)
	fixture.sweep(test)
	if fixture.receipt(test, "state") != "notified" || fixture.assigneeTasks(test) != 1 {
		test.Fatalf("state = %q, assignee tasks = %d, want notified and 1", fixture.receipt(test, "state"), fixture.assigneeTasks(test))
	}
	if got := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue t JOIN issue_idle_guard g ON g.issue_id = t.issue_id WHERE t.issue_id = $1 AND g.alert_id = t.id`, fixture.issue); got != 1 {
		test.Fatal("receipt does not name the alert task")
	}
}

func TestIdleGuardArchivedAssigneeWithoutSquadFails(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	dbfx.Exec(test, `DELETE FROM squad_member WHERE squad_id = $1`, fixture.squad)
	dbfx.Exec(test, `UPDATE agent SET archived_at = clock_timestamp() WHERE id = $1`, fixture.agent)
	for range 5 {
		_ = testHandler.SweepIdleStartedIssues(context.Background())
	}
	if fixture.receipt(test, "state") != "gave_up" || fixture.assigneeTasks(test) != 0 {
		test.Fatalf("state = %q, tasks = %d, want gave_up and 0", fixture.receipt(test, "state"), fixture.assigneeTasks(test))
	}
}

func TestDecisionWaitGuardAlertsAssigneeWithoutSquad(test *testing.T) {
	fixture, _, _ := decisionWaitFixture(test)
	dbfx.Exec(test, `UPDATE issue SET assignee_type = 'agent', assignee_id = $1 WHERE id = $2`, fixture.agentID, fixture.issueID)
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, fixture.taskID)
	dbfx.Exec(test, `UPDATE issue SET metadata = metadata || '{"decision_wait_route_error":"owning squad is missing or ambiguous"}' WHERE id = $1`, fixture.issueID)
	if err := testHandler.checkDecisionWaits(context.Background()); err != nil {
		test.Fatal(err)
	}
	if got := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue t JOIN issue i ON i.id = t.issue_id
		WHERE i.id = $1 AND t.agent_id = $2 AND t.squad_id IS NULL AND NOT t.is_leader_task AND t.status = 'queued'
		AND t.handoff_note LIKE 'Decision wait guard:%' AND i.metadata->>'decision_wait_alert_id' = t.id::text
		AND NOT i.metadata ? 'decision_wait_route_error'`, fixture.issueID, fixture.agentID); got != 1 {
		test.Fatalf("assignee alerts with receipt = %d, want 1", got)
	}
}

func TestDecisionWaitGuardArchivedAssigneeWithoutSquadFails(test *testing.T) {
	fixture, _, _ := decisionWaitFixture(test)
	dbfx.Exec(test, `UPDATE issue SET assignee_type = 'agent', assignee_id = $1 WHERE id = $2`, fixture.agentID, fixture.issueID)
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, fixture.taskID)
	dbfx.Exec(test, `UPDATE agent SET archived_at = clock_timestamp() WHERE id = $1`, fixture.agentID)
	if err := testHandler.checkDecisionWaits(context.Background()); err != nil {
		test.Fatal(err)
	}
	if dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'`, fixture.issueID, fixture.agentID) != 0 ||
		dbfx.Count(test, `SELECT count(*) FROM issue WHERE id = $1 AND metadata ? 'decision_wait_route_error' AND NOT metadata ? 'decision_wait_alert_id'`, fixture.issueID) != 1 {
		test.Fatal("archived assignee got a task or the route error was lost")
	}
}
