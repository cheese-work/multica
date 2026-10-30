package handler

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func decisionWaitFixture(test *testing.T) (intentFixture, string, string) {
	test.Helper()
	fixture := newIntentFixture(test)
	leader := createHandlerTestAgent(test, "wait leader", []byte("[]"))
	squad := dbfx.Insert(test, "squad", testutil.Cols{"workspace_id": testWorkspaceID, "name": "wait squad", "creator_id": testUserID, "leader_id": leader})
	dbfx.Insert(test, "issue_status", testutil.Cols{"workspace_id": testWorkspaceID, "key": "need_decision", "name": "Need Decision", "category": "started", "color": "#112233"})
	dbfx.Exec(test, `UPDATE issue SET status = 'need_decision', assignee_type = 'squad', assignee_id = $1 WHERE id = $2`, squad, fixture.issueID)
	setWaitAge(test, fixture.issueID, 660)
	test.Cleanup(func() { dbfx.Exec(test, `DELETE FROM inbox_item WHERE issue_id = $1`, fixture.issueID) })
	return fixture, squad, leader
}

func setWaitAge(test *testing.T, issue string, seconds int) {
	test.Helper()
	dbfx.Exec(test, `UPDATE issue SET metadata = jsonb_build_object('decision_wait_started_at',
		to_char(clock_timestamp() AT TIME ZONE 'UTC' - make_interval(secs => $1), 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')) WHERE id = $2`, seconds, issue)
}

func TestDecisionWaitGuardBoundaries(test *testing.T) {
	for _, scenario := range []string{"fresh", "before_threshold", "after_threshold", "resolved", "terminal", "disabled", "custom_threshold", "readiness_option", "archived_property"} {
		test.Run(scenario, func(test *testing.T) {
			fixture, _, _ := decisionWaitFixture(test)
			want := 0
			switch scenario {
			case "fresh":
				dbfx.Exec(test, `UPDATE issue SET metadata = '{}' WHERE id = $1`, fixture.issueID)
			case "before_threshold":
				setWaitAge(test, fixture.issueID, 599)
			case "after_threshold":
				setWaitAge(test, fixture.issueID, 601)
				want = 1
			case "resolved", "terminal":
				status := "in_progress"
				if scenario == "terminal" {
					status = "done"
				}
				dbfx.Exec(test, `UPDATE issue SET status = $1 WHERE id = $2`, status, fixture.issueID)
			case "disabled", "custom_threshold":
				settings := `{"decision_wait_guard":{"enabled":false}}`
				if scenario == "custom_threshold" {
					settings = `{"decision_wait_guard":{"after_minutes":20}}`
				}
				dbfx.Exec(test, `UPDATE workspace SET settings = settings || $1::jsonb WHERE id = $2`, settings, testWorkspaceID)
				test.Cleanup(func() {
					dbfx.Exec(test, `UPDATE workspace SET settings = settings - 'decision_wait_guard' WHERE id = $1`, testWorkspaceID)
				})
			case "readiness_option", "archived_property":
				property := dbfx.Insert(test, "issue_property", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Readiness", "type": "select", "config": `{"options":[{"id":"gate","name":"gated-human"}]}`})
				dbfx.Exec(test, `UPDATE issue SET status = 'in_progress', properties = jsonb_build_object($1::text, 'gate') WHERE id = $2`, property, fixture.issueID)
				want = 1
				if scenario == "archived_property" {
					dbfx.Exec(test, `UPDATE issue_property SET archived_at = clock_timestamp() WHERE id = $1`, property)
					want = 0
				}
			}
			if err := testHandler.checkDecisionWaits(context.Background()); err != nil {
				test.Fatal(err)
			}
			if got := dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1`, fixture.issueID); got != want {
				test.Fatalf("alerts = %d, want %d", got, want)
			}
			if scenario == "resolved" || scenario == "terminal" || scenario == "disabled" || scenario == "archived_property" {
				if got := dbfx.Count(test, `SELECT count(*) FROM issue WHERE id = $1 AND metadata ? 'decision_wait_started_at'`, fixture.issueID); got != 0 {
					test.Fatal("ineligible episode was not cleared")
				}
			}
			if scenario == "fresh" && dbfx.Count(test, `SELECT count(*) FROM issue WHERE id = $1 AND metadata ? 'decision_wait_started_at'`, fixture.issueID) != 1 {
				test.Fatal("fresh episode was not started")
			}
		})
	}
}

func TestDecisionWaitGuardOwnership(test *testing.T) {
	for _, scenario := range []string{"member_provenance", "agent_membership", "missing", "ambiguous", "foreign_squad", "archived_squad", "archived_leader"} {
		test.Run(scenario, func(test *testing.T) {
			fixture, squad, leader := decisionWaitFixture(test)
			want := 0
			switch scenario {
			case "member_provenance":
				dbfx.Exec(test, `UPDATE issue SET assignee_type = 'member', assignee_id = $1 WHERE id = $2`, fixture.humanID, fixture.issueID)
				dbfx.Exec(test, `UPDATE agent_task_queue SET agent_id = $1, squad_id = $2, is_leader_task = true WHERE id = $3`, leader, squad, fixture.taskID)
				want = 1
			case "agent_membership", "ambiguous":
				dbfx.Exec(test, `UPDATE issue SET assignee_type = 'agent', assignee_id = $1 WHERE id = $2`, fixture.agentID, fixture.issueID)
				dbfx.Exec(test, `INSERT INTO squad_member (squad_id, member_type, member_id, role) VALUES ($1, 'agent', $2, 'member')`, squad, fixture.agentID)
				want = 1
				if scenario == "ambiguous" {
					other := dbfx.Insert(test, "squad", testutil.Cols{"workspace_id": testWorkspaceID, "name": "other wait squad", "creator_id": testUserID, "leader_id": leader})
					dbfx.Exec(test, `INSERT INTO squad_member (squad_id, member_type, member_id, role) VALUES ($1, 'agent', $2, 'member')`, other, fixture.agentID)
					want = 0
				}
			case "missing":
				dbfx.Exec(test, `UPDATE issue SET assignee_type = NULL, assignee_id = NULL WHERE id = $1`, fixture.issueID)
			case "foreign_squad":
				foreign := dbfx.Workspace(test, "foreign guard", "foreign-guard-"+fixture.issueID)
				dbfx.Exec(test, `UPDATE squad SET workspace_id = $1 WHERE id = $2`, foreign, squad)
			case "archived_squad":
				dbfx.Exec(test, `UPDATE squad SET archived_at = clock_timestamp() WHERE id = $1`, squad)
			case "archived_leader":
				dbfx.Exec(test, `UPDATE agent SET archived_at = clock_timestamp() WHERE id = $1`, leader)
			}
			if err := testHandler.checkDecisionWaits(context.Background()); err != nil {
				test.Fatal(err)
			}
			if got := dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1 AND recipient_id = $2`, fixture.issueID, leader); got != want {
				test.Fatalf("alerts = %d, want %d", got, want)
			}
			if want == 0 && dbfx.Count(test, `SELECT count(*) FROM issue WHERE id = $1 AND metadata ? 'decision_wait_route_error' AND metadata->>'decision_wait_missing_request' = 'true'`, fixture.issueID) != 1 {
				test.Fatal("missing route was silently lost")
			}
		})
	}
}

func TestDecisionWaitGuardOpenRequest(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	if _, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input()); err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, fixture.issueID)
	// Readiness supplies the wait independently of the status catalog.
	property := dbfx.Insert(test, "issue_property", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Readiness", "type": "select", "config": `{"options":[{"id":"gate","name":"gated-human"}]}`})
	dbfx.Exec(test, `UPDATE issue SET properties = jsonb_build_object($1::text, 'gate') WHERE id = $2`, property, fixture.issueID)
	setWaitAge(test, fixture.issueID, 660)
	if err := testHandler.checkDecisionWaits(context.Background()); err != nil {
		test.Fatal(err)
	}
	if dbfx.Count(test, `SELECT count(*) FROM issue WHERE id = $1 AND metadata ? 'decision_wait_started_at'`, fixture.issueID) != 0 {
		test.Fatal("open request did not clear missing-request episode")
	}
}

func TestDecisionWaitGuardRollback(test *testing.T) {
	fixture, _, _ := decisionWaitFixture(test)
	handler, _ := newDecisionRunnerFixture(test)
	handler.TxStarter = decisionFailCommit{testPool}
	var published atomic.Int32
	handler.Bus.SubscribeAll(func(events.Event) { published.Add(1) })
	if err := handler.checkDecisionWaits(context.Background()); err == nil {
		test.Fatal("expected commit failure")
	}
	if published.Load() != 0 || dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1`, fixture.issueID) != 0 || dbfx.Count(test, `SELECT count(*) FROM issue WHERE id = $1 AND metadata ? 'decision_wait_alert_id'`, fixture.issueID) != 0 {
		test.Fatal("rollback leaked alert, receipt or event")
	}
	handler.TxStarter = testPool
	if err := handler.checkDecisionWaits(context.Background()); err != nil {
		test.Fatal(err)
	}
	if published.Load() != 1 || dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1`, fixture.issueID) != 1 {
		test.Fatal("retry did not commit one alert")
	}
}

func TestDecisionWorkerAlertsMissingRequestOnce(test *testing.T) {
	resetDecisionLease(test)
	fixture := newIntentFixture(test)
	leaderID := createHandlerTestAgent(test, "Decision wait leader", []byte("[]"))
	squadID := dbfx.Insert(test, "squad", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Decision guard squad", "creator_id": testUserID, "leader_id": leaderID})
	dbfx.Insert(test, "issue_status", testutil.Cols{"workspace_id": testWorkspaceID, "key": "need_decision", "name": "Need Decision", "category": "started", "color": "#112233"})
	dbfx.Exec(test, `UPDATE issue SET status = 'need_decision', assignee_type = 'squad', assignee_id = $1,
		metadata = jsonb_build_object('decision_wait_started_at', to_char(clock_timestamp() AT TIME ZONE 'UTC' - interval '11 minutes', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')) WHERE id = $2`, squadID, fixture.issueID)
	test.Cleanup(func() { dbfx.Exec(test, `DELETE FROM inbox_item WHERE issue_id = $1`, fixture.issueID) })
	_, runner := newDecisionRunnerFixture(test)
	decisionPass(test, runner)
	count := `SELECT count(*) FROM inbox_item WHERE issue_id = $1 AND recipient_type = 'agent' AND recipient_id = $2 AND details->>'kind' = 'missing_decision_request'`
	if got := dbfx.Count(test, count, fixture.issueID, leaderID); got != 1 {
		test.Fatalf("missing-request alerts = %d, want 1", got)
	}
	decisionPass(test, runner)
	// A new worker after lease expiry must retain the durable episode receipt.
	dbfx.Exec(test, `UPDATE decision_worker_lease SET expires_at = clock_timestamp() - interval '1 second'`)
	_, restarted := newDecisionRunnerFixture(test)
	decisionPass(test, restarted)
	if got := dbfx.Count(test, count, fixture.issueID, leaderID); got != 1 {
		test.Fatalf("duplicate alerts after restart = %d, want 1", got)
	}
	dbfx.Exec(test, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, fixture.issueID)
	decisionPass(test, restarted)
	dbfx.Exec(test, `UPDATE issue SET status = 'need_decision', metadata = jsonb_build_object('decision_wait_started_at', to_char(clock_timestamp() AT TIME ZONE 'UTC' - interval '11 minutes', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')) WHERE id = $1`, fixture.issueID)
	decisionPass(test, restarted)
	if got := dbfx.Count(test, count, fixture.issueID, leaderID); got != 2 {
		test.Fatalf("new wait episode alerts = %d, want 2", got)
	}
}
