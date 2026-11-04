package service

import (
	"context"
	"testing"

	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// Routing agent failover (CHE-3627): when the routing agent's run dies on a
// provider failure, the configured fallback agent takes over the same issue.

const providerDown = "agent_error.provider_server_error"

// routingFailoverFixture: the issue sits in status with the coordinator as
// assignee and routing agent, the worker as fallback, and the source task running.
func routingFailoverFixture(t *testing.T, status string) (*delegatedFailureFixture, *TaskService, *dbfx.Fixture) {
	t.Helper()
	fixture, svc := seedDelegatedFailureFixture(t)
	rows := dbfx.New(fixture.pool, fixture.workspaceID, fixture.userID)
	rows.Exec(t, `UPDATE issue SET status = $2, assignee_type = 'agent', assignee_id = $3 WHERE id = $1`,
		fixture.issueID, status, fixture.coordinator)
	rows.Exec(t, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, fixture.sourceTask)
	setRoutingAgent(t, rows, fixture, fixture.coordinator, fixture.worker)
	return fixture, svc, rows
}

func setRoutingAgent(t *testing.T, rows *dbfx.Fixture, f *delegatedFailureFixture, agent, fallback string) {
	t.Helper()
	rows.Exec(t, `UPDATE workspace SET settings = jsonb_build_object('routing_agent',
		jsonb_build_object('agent_id', $2::text, 'fallback_agent_id', $3::text)) WHERE id = $1`,
		f.workspaceID, agent, fallback)
}

func fallbackRuns(t *testing.T, rows *dbfx.Fixture, f *delegatedFailureFixture, agent string) int {
	t.Helper()
	return rows.Count(t, `SELECT count(*) FROM agent_task_queue
		WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'dispatched')`, f.issueID, agent)
}

func failSource(t *testing.T, svc *TaskService, f *delegatedFailureFixture, reason string) {
	t.Helper()
	if _, err := svc.FailTask(context.Background(), util.MustParseUUID(f.sourceTask), "provider returned 500", "", "", "", reason, false, "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestRoutingFailoverRunsTheFallbackAgentOnProviderFailure(t *testing.T) {
	f, svc, rows := routingFailoverFixture(t, "todo")
	failSource(t, svc, f, providerDown)
	if got := fallbackRuns(t, rows, f, f.worker); got != 1 {
		t.Fatalf("fallback runs = %d, want 1", got)
	}
	// The fallback must own the step, not observe it: the workflow brief gives
	// step instructions and status commands only to the issue's assignee.
	if got := rows.Count(t, `SELECT count(*) FROM issue WHERE id = $1 AND assignee_type = 'agent' AND assignee_id = $2`,
		f.issueID, f.worker); got != 1 {
		t.Fatalf("issue assigned to the fallback agent = %d, want 1", got)
	}
	if got := rows.Count(t, `SELECT count(*) FROM agent_task_queue t JOIN issue i ON i.id = t.issue_id
		WHERE t.issue_id = $1 AND t.status = 'queued' AND t.agent_id = i.assignee_id`, f.issueID); got != 1 {
		t.Fatalf("queued runs for the assignee = %d, want 1", got)
	}
	if got := rows.Count(t, `SELECT count(*) FROM issue WHERE id = $1 AND status = 'todo'`, f.issueID); got != 1 {
		t.Fatalf("issue left its routing status, count = %d", got)
	}
}

func TestRoutingFailoverIgnoresCasesThatAreNotARoutingProviderFailure(t *testing.T) {
	cases := []struct {
		name   string
		status string
		reason string
		setup  func(t *testing.T, rows *dbfx.Fixture, f *delegatedFailureFixture)
	}{
		{name: "agent error, not the provider", status: "todo", reason: "agent_error.process_failure"},
		{name: "not a routing status", status: "in_progress", reason: providerDown},
		{name: "routing off", status: "todo", reason: providerDown, setup: func(t *testing.T, rows *dbfx.Fixture, f *delegatedFailureFixture) {
			rows.Exec(t, `UPDATE workspace SET settings = '{}'::jsonb WHERE id = $1`, f.workspaceID)
		}},
		{name: "no fallback set", status: "todo", reason: providerDown, setup: func(t *testing.T, rows *dbfx.Fixture, f *delegatedFailureFixture) {
			setRoutingAgent(t, rows, f, f.coordinator, "")
		}},
		{name: "the failed agent is the fallback", status: "todo", reason: providerDown, setup: func(t *testing.T, rows *dbfx.Fixture, f *delegatedFailureFixture) {
			setRoutingAgent(t, rows, f, f.worker, f.coordinator)
		}},
		{name: "the failed agent is not the routing agent", status: "todo", reason: providerDown, setup: func(t *testing.T, rows *dbfx.Fixture, f *delegatedFailureFixture) {
			setRoutingAgent(t, rows, f, f.worker, "")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, svc, rows := routingFailoverFixture(t, tc.status)
			if tc.setup != nil {
				tc.setup(t, rows, f)
			}
			failSource(t, svc, f, tc.reason)
			if got := rows.Count(t, `SELECT count(*) FROM agent_task_queue
				WHERE issue_id = $1 AND status IN ('queued', 'dispatched')`, f.issueID); got != 0 {
				t.Fatalf("%d runs were queued, want none", got)
			}
			if got := rows.Count(t, `SELECT count(*) FROM issue WHERE id = $1 AND assignee_id = $2`, f.issueID, f.coordinator); got != 1 {
				t.Fatal("the issue was reassigned")
			}
		})
	}
}
