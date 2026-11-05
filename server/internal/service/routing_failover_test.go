package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
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

func failedSource(t *testing.T, svc *TaskService, f *delegatedFailureFixture) db.AgentTaskQueue {
	t.Helper()
	task, err := svc.Queries.GetAgentTask(context.Background(), util.MustParseUUID(f.sourceTask))
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestRoutingFailoverSkipsAnIssueThatMovedAfterTheSnapshot(t *testing.T) {
	cases := map[string]string{
		"status changed":   `UPDATE issue SET status = 'in_review' WHERE id = $1`,
		"assignee changed": `UPDATE issue SET assignee_id = '%s' WHERE id = $1`,
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f, svc, rows := routingFailoverFixture(t, "todo")
			snapshot, err := svc.Queries.GetIssue(context.Background(), util.MustParseUUID(f.issueID))
			if err != nil {
				t.Fatal(err)
			}
			if name == "assignee changed" {
				change = strings.Replace(change, "%s", f.worker, 1)
			}
			rows.Exec(t, change, f.issueID)
			err = svc.handStepToFallback(context.Background(), snapshot, failedSource(t, svc, f), util.MustParseUUID(f.worker), "note")
			if !errors.Is(err, errRoutingStepMoved) {
				t.Fatalf("err = %v, want errRoutingStepMoved", err)
			}
			if got := fallbackRuns(t, rows, f, f.worker); got != 0 {
				t.Fatalf("fallback runs = %d, want 0", got)
			}
			if name == "status changed" {
				if got := rows.Count(t, `SELECT count(*) FROM issue WHERE id = $1 AND assignee_id = $2`, f.issueID, f.coordinator); got != 1 {
					t.Fatal("the issue was reassigned")
				}
			}
		})
	}
}

func TestRoutingFailoverKeepsFieldsChangedAfterTheSnapshot(t *testing.T) {
	f, svc, rows := routingFailoverFixture(t, "todo")
	snapshot, err := svc.Queries.GetIssue(context.Background(), util.MustParseUUID(f.issueID))
	if err != nil {
		t.Fatal(err)
	}
	rows.Exec(t, `UPDATE issue SET due_date = '2030-01-02', stage = 4 WHERE id = $1`, f.issueID)
	if err := svc.handStepToFallback(context.Background(), snapshot, failedSource(t, svc, f), util.MustParseUUID(f.worker), "note"); err != nil {
		t.Fatal(err)
	}
	if got := rows.Count(t, `SELECT count(*) FROM issue WHERE id = $1 AND due_date = '2030-01-02' AND stage = 4 AND assignee_id = $2`, f.issueID, f.worker); got != 1 {
		t.Fatalf("locked-row fields were overwritten or the issue was not handed over, count = %d", got)
	}
}

func TestRoutingFailoverKeepsTheAssigneeWhenTheEnqueueFails(t *testing.T) {
	f, svc, rows := routingFailoverFixture(t, "todo")
	// A queued run for the fallback already exists, so the insert is rejected.
	rows.Exec(t, `INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority)
		VALUES ($1, $2, $3, 'queued', 0)`, f.worker, f.runtimeID, f.issueID)
	failSource(t, svc, f, providerDown)
	if got := rows.Count(t, `SELECT count(*) FROM issue WHERE id = $1 AND assignee_id = $2`, f.issueID, f.coordinator); got != 1 {
		t.Fatal("the issue stayed with the fallback agent after the enqueue failed")
	}
}

// ownWorkerByAnotherMember makes the fallback agent a private agent of a second
// workspace member; the failed run's originator stays the first member.
func ownWorkerByAnotherMember(t *testing.T, rows *dbfx.Fixture, f *delegatedFailureFixture) string {
	t.Helper()
	var other string
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO "user" (name, email) VALUES ('Fallback Owner', $1) RETURNING id`,
		"fallback-owner-"+f.sourceTask+"@multica.test").Scan(&other); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.pool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, other) })
	rows.Exec(t, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'member')`, f.workspaceID, other)
	rows.Exec(t, `UPDATE agent SET owner_id = $2, permission_mode = 'private' WHERE id = $1`, f.worker, other)
	return other
}

func TestRoutingFailoverNeedsTheOriginatorToBeAbleToInvokeTheFallback(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T, rows *dbfx.Fixture, f *delegatedFailureFixture)
		wantRun bool
	}{
		{name: "private fallback of another member is refused", setup: func(t *testing.T, rows *dbfx.Fixture, f *delegatedFailureFixture) {
			ownWorkerByAnotherMember(t, rows, f)
		}},
		{name: "public_to fallback that does not target the originator is refused", setup: func(t *testing.T, rows *dbfx.Fixture, f *delegatedFailureFixture) {
			other := ownWorkerByAnotherMember(t, rows, f)
			rows.Exec(t, `UPDATE agent SET permission_mode = 'public_to' WHERE id = $1`, f.worker)
			rows.Exec(t, `INSERT INTO agent_invocation_target (agent_id, target_type, target_id) VALUES ($1, 'member', $2)`, f.worker, other)
		}},
		{name: "run with no human originator is refused", setup: func(t *testing.T, rows *dbfx.Fixture, f *delegatedFailureFixture) {
			rows.Exec(t, `UPDATE agent_task_queue SET originator_user_id = NULL WHERE id = $1`, f.sourceTask)
		}},
		{name: "public_to fallback that targets the originator runs", wantRun: true, setup: func(t *testing.T, rows *dbfx.Fixture, f *delegatedFailureFixture) {
			ownWorkerByAnotherMember(t, rows, f)
			rows.Exec(t, `UPDATE agent SET permission_mode = 'public_to' WHERE id = $1`, f.worker)
			rows.Exec(t, `INSERT INTO agent_invocation_target (agent_id, target_type, target_id) VALUES ($1, 'member', $2)`, f.worker, f.userID)
		}},
		{name: "fallback owned by the originator runs", wantRun: true, setup: func(*testing.T, *dbfx.Fixture, *delegatedFailureFixture) {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, svc, rows := routingFailoverFixture(t, "todo")
			tc.setup(t, rows, f)
			failSource(t, svc, f, providerDown)
			wantRuns, wantAssignee := 0, f.coordinator
			if tc.wantRun {
				wantRuns, wantAssignee = 1, f.worker
			}
			if got := fallbackRuns(t, rows, f, f.worker); got != wantRuns {
				t.Fatalf("fallback runs = %d, want %d", got, wantRuns)
			}
			if got := rows.Count(t, `SELECT count(*) FROM issue WHERE id = $1 AND assignee_id = $2`, f.issueID, wantAssignee); got != 1 {
				t.Fatalf("issue assignee is not %s", wantAssignee)
			}
		})
	}
}
