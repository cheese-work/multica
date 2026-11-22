package handler

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
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

func TestDecisionWaitGuardAdmitsLeaderTask(test *testing.T) {
	fixture, squad, leader := decisionWaitFixture(test)
	if err := testHandler.checkDecisionWaits(context.Background()); err != nil {
		test.Fatal(err)
	}
	if got := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue t JOIN issue i ON i.id = t.issue_id
		WHERE i.id = $1 AND t.agent_id = $2 AND t.squad_id = $3 AND t.is_leader_task
		AND t.status = 'queued' AND t.runtime_id IS NOT NULL AND t.accountable_user_id IS NOT NULL
		AND t.handoff_note LIKE 'Decision wait guard:%' AND i.metadata->>'decision_wait_alert_id' = t.id::text`, fixture.issueID, leader, squad); got != 1 {
		test.Fatalf("admitted leader tasks with atomic receipt = %d, want 1", got)
	}
	if got := dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE issue_id = $1`, fixture.issueID); got != 0 {
		test.Fatal("unread agent inbox alert remains")
	}
}

func TestDecisionWaitGuardAdmissionFailureRetries(test *testing.T) {
	fixture, _, leader := decisionWaitFixture(test)
	dbfx.Exec(test, `UPDATE agent SET runtime_id = NULL WHERE id = $1`, leader)
	if err := testHandler.checkDecisionWaits(context.Background()); err != nil {
		test.Fatal(err)
	}
	if dbfx.Count(test, `SELECT count(*) FROM issue WHERE id = $1
		AND metadata ? 'decision_wait_route_error' AND metadata->>'decision_wait_missing_request' = 'true'
		AND NOT metadata ? 'decision_wait_alert_id'`, fixture.issueID) != 1 {
		test.Fatal("refused admission must retain a retryable route error without delivery receipt")
	}
	dbfx.Exec(test, `UPDATE agent SET runtime_id = $1 WHERE id = $2`, handlerTestRuntimeID(test), leader)
	if err := testHandler.checkDecisionWaits(context.Background()); err != nil {
		test.Fatal(err)
	}
	if dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'`, fixture.issueID, leader) != 1 {
		test.Fatal("repaired runtime did not admit the leader task")
	}
	if dbfx.Count(test, `SELECT count(*) FROM issue WHERE id = $1 AND metadata ? 'decision_wait_route_error'`, fixture.issueID) != 0 {
		test.Fatal("successful retry retained route error")
	}
}

func TestDecisionWaitGuardScanSkipsInactiveWaits(test *testing.T) {
	for _, status := range []string{"in_review", "done", "cancelled", "custom_done"} {
		test.Run(status, func(test *testing.T) {
			fixture, _, _ := decisionWaitFixture(test)
			if status == "custom_done" {
				dbfx.Insert(test, "issue_status", testutil.Cols{"workspace_id": testWorkspaceID, "key": status, "name": "Custom Done", "category": "done", "color": "#112233"})
			}
			property := dbfx.Insert(test, "issue_property", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Readiness", "type": "select", "config": `{"options":[{"id":"gate","name":"gated-human"}]}`})
			dbfx.Exec(test, `UPDATE issue SET status = $1, properties = jsonb_build_object($2::text, 'gate') WHERE id = $3`, status, property, fixture.issueID)
			if err := testHandler.checkDecisionWaits(context.Background()); err != nil {
				test.Fatal(err)
			}
			issues, err := testHandler.Queries.ListDecisionWaitGuardIssues(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			for _, issue := range issues {
				if uuidToString(issue.ID) == fixture.issueID {
					test.Fatal("inactive wait still scanned after episode cleanup")
				}
			}
			if dbfx.Count(test, `SELECT count(*) FROM issue WHERE id = $1 AND metadata ? 'decision_wait_started_at'`, fixture.issueID) != 0 {
				test.Fatal("inactive episode not cleared")
			}
		})
	}
}

func TestDecisionWaitGuardHonorsAdmissionGates(test *testing.T) {
	for _, gate := range []string{"project_paused", "run_budget", "triage", "pending_task"} {
		test.Run(gate, func(test *testing.T) {
			fixture, squad, leader := decisionWaitFixture(test)
			switch gate {
			case "project_paused":
				project := dbfx.Project(test, "guard paused project", testutil.Cols{"status": "paused"})
				dbfx.Exec(test, `UPDATE issue SET project_id = $1 WHERE id = $2`, project, fixture.issueID)
			case "run_budget":
				workflow := dbfx.Insert(test, "issue_workflow", testutil.Cols{"workspace_id": testWorkspaceID, "name": "guard budget", "initial_status_key": "need_decision", "steps": `[{"status_key":"need_decision"}]`, "max_runs_per_issue": 1, "run_budget_since": testutil.Raw("clock_timestamp() - interval '1 hour'")})
				dbfx.Exec(test, `UPDATE issue SET workflow_id = $1 WHERE id = $2`, workflow, fixture.issueID)
			case "triage":
				dbfx.Exec(test, `UPDATE issue SET triage_state = 'pending' WHERE id = $1`, fixture.issueID)
			case "pending_task":
				dbfx.Task(test, leader, testutil.Cols{"issue_id": fixture.issueID, "status": "queued", "runtime_id": handlerTestRuntimeID(test), "squad_id": squad, "is_leader_task": true})
			}
			if err := testHandler.checkDecisionWaits(context.Background()); err != nil {
				test.Fatal(err)
			}
			if dbfx.Count(test, `SELECT count(*) FROM issue WHERE id = $1 AND metadata ? 'decision_wait_route_error'
				AND NOT metadata ? 'decision_wait_alert_id'`, fixture.issueID) != 1 {
				test.Fatal("admission gate bypassed or failure lost")
			}
			if dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND handoff_note LIKE 'Decision wait guard:%'`, fixture.issueID) != 0 {
				test.Fatal("refused alert admitted a task")
			}
		})
	}
}

func TestDecisionWaitGuardBoundaries(test *testing.T) {
	for _, scenario := range []string{"fresh", "before_threshold", "after_threshold", "resolved", "terminal", "disabled", "custom_threshold", "readiness_option", "archived_property", "merge_wait"} {
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
			case "readiness_option", "archived_property", "merge_wait":
				property := dbfx.Insert(test, "issue_property", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Readiness", "type": "select", "config": `{"options":[{"id":"gate","name":"gated-human"}]}`})
				dbfx.Exec(test, `UPDATE issue SET status = 'in_progress', properties = jsonb_build_object($1::text, 'gate') WHERE id = $2`, property, fixture.issueID)
				want = 1
				if scenario == "merge_wait" {
					dbfx.Exec(test, `UPDATE issue SET status = 'in_review' WHERE id = $1`, fixture.issueID)
					want = 0
				}
				if scenario == "archived_property" {
					dbfx.Exec(test, `UPDATE issue_property SET archived_at = clock_timestamp() WHERE id = $1`, property)
					want = 0
				}
			}
			if err := testHandler.checkDecisionWaits(context.Background()); err != nil {
				test.Fatal(err)
			}
			if got := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND handoff_note LIKE 'Decision wait guard:%'`, fixture.issueID); got != want {
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
			if got := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND handoff_note LIKE 'Decision wait guard:%'`, fixture.issueID, leader); got != want {
				test.Fatalf("alerts = %d, want %d", got, want)
			}
			if scenario == "ambiguous" {
				// CHE-6101: an ambiguous squad route falls back to the agent assignee.
				if dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND squad_id IS NULL AND handoff_note LIKE 'Decision wait guard:%'`, fixture.issueID, fixture.agentID) != 1 {
					test.Fatal("ambiguous route did not alert the assignee")
				}
				return
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
	handler.Bus.SubscribeAll(func(event events.Event) {
		if event.Type == protocol.EventTaskQueued {
			published.Add(1)
		}
	})
	if err := handler.checkDecisionWaits(context.Background()); err == nil {
		test.Fatal("expected commit failure")
	}
	if published.Load() != 0 || dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND handoff_note LIKE 'Decision wait guard:%'`, fixture.issueID) != 0 || dbfx.Count(test, `SELECT count(*) FROM issue WHERE id = $1 AND metadata ? 'decision_wait_alert_id'`, fixture.issueID) != 0 {
		test.Fatal("rollback leaked alert, receipt or event")
	}
	handler.TxStarter = testPool
	if err := handler.checkDecisionWaits(context.Background()); err != nil {
		test.Fatal(err)
	}
	if published.Load() != 1 || dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND handoff_note LIKE 'Decision wait guard:%'`, fixture.issueID) != 1 {
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
	count := `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND is_leader_task AND handoff_note LIKE 'Decision wait guard:%'`
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
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE issue_id = $1 AND agent_id = $2`, fixture.issueID, leaderID)
	dbfx.Exec(test, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, fixture.issueID)
	decisionPass(test, restarted)
	dbfx.Exec(test, `UPDATE issue SET status = 'need_decision', metadata = jsonb_build_object('decision_wait_started_at', to_char(clock_timestamp() AT TIME ZONE 'UTC' - interval '11 minutes', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')) WHERE id = $1`, fixture.issueID)
	decisionPass(test, restarted)
	if got := dbfx.Count(test, count, fixture.issueID, leaderID); got != 2 {
		test.Fatalf("new wait episode alerts = %d, want 2", got)
	}
}

// seedWaitRequest inserts a human decision request on issueID in the given status.
func seedWaitRequest(test *testing.T, fixture intentFixture, issueID, status string) string {
	test.Helper()
	session := dbfx.ChatSession(test, fixture.agentID, testutil.Cols{"creator_id": fixture.humanID})
	id := dbfx.Insert(test, "decision_request", testutil.Cols{
		"workspace_id": testWorkspaceID, "issue_id": issueID, "asking_agent_id": fixture.agentID, "human_user_id": fixture.humanID,
		"chat_session_id": session, "ask": "Approve?", "escalation_class": "reversible", "origin": "asked", "status": status,
	})
	dbfx.Cleanup(test, `DELETE FROM decision_request WHERE id = $1`, id)
	return id
}

func decisionWaitAlerts(test *testing.T, issueID string) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND handoff_note LIKE 'Decision wait guard:%'`, issueID)
}

func requireNoWaitFlag(test *testing.T, issueID string) {
	test.Helper()
	if got := dbfx.Count(test, `SELECT count(*) FROM issue WHERE id = $1 AND metadata ?| ARRAY['decision_wait_started_at','decision_wait_alert_id','decision_wait_missing_request','decision_wait_route_error']`, issueID); got != 0 {
		test.Fatal("issue still carries decision wait metadata")
	}
}

// CHE-4029 criterion 1: a grouped request lives on the parent.
func TestDecisionWaitGuardCountsParentGroupedRequest(test *testing.T) {
	fixture, _, _ := decisionWaitFixture(test)
	parent := dbfx.Issue(test, "grouped parent", nil)
	dbfx.Exec(test, `UPDATE issue SET parent_issue_id = $1 WHERE id = $2`, parent, fixture.issueID)
	seedWaitRequest(test, fixture, parent, "open")
	if err := testHandler.checkDecisionWaits(context.Background()); err != nil {
		test.Fatal(err)
	}
	if got := decisionWaitAlerts(test, fixture.issueID); got != 0 {
		test.Fatalf("child of a parent with an open grouped request was alerted %d times", got)
	}
	requireNoWaitFlag(test, fixture.issueID)
}

// A parent request that is no longer open does not cover the child.
func TestDecisionWaitGuardIgnoresClosedParentRequest(test *testing.T) {
	fixture, _, _ := decisionWaitFixture(test)
	parent := dbfx.Issue(test, "grouped parent", nil)
	dbfx.Exec(test, `UPDATE issue SET parent_issue_id = $1 WHERE id = $2`, parent, fixture.issueID)
	seedWaitRequest(test, fixture, parent, "cancelled")
	if err := testHandler.checkDecisionWaits(context.Background()); err != nil {
		test.Fatal(err)
	}
	if got := decisionWaitAlerts(test, fixture.issueID); got != 1 {
		test.Fatalf("alerts = %d, want 1", got)
	}
}

// CHE-4029 criteria 2 and 3: an answered request is not flagged, including the
// pass right after the last open request is answered.
func TestDecisionWaitGuardAnsweredRequest(test *testing.T) {
	test.Run("answered_only", func(test *testing.T) {
		fixture, _, _ := decisionWaitFixture(test)
		seedWaitRequest(test, fixture, fixture.issueID, "answered")
		if err := testHandler.checkDecisionWaits(context.Background()); err != nil {
			test.Fatal(err)
		}
		if got := decisionWaitAlerts(test, fixture.issueID); got != 0 {
			test.Fatalf("answered request alerted %d times", got)
		}
		requireNoWaitFlag(test, fixture.issueID)
	})
	test.Run("last_open_answered", func(test *testing.T) {
		fixture, _, _ := decisionWaitFixture(test)
		request := seedWaitRequest(test, fixture, fixture.issueID, "open")
		if err := testHandler.checkDecisionWaits(context.Background()); err != nil {
			test.Fatal(err)
		}
		requireNoWaitFlag(test, fixture.issueID)
		dbfx.Exec(test, `UPDATE decision_request SET status = 'answered' WHERE id = $1`, request)
		setWaitAge(test, fixture.issueID, 660)
		if err := testHandler.checkDecisionWaits(context.Background()); err != nil {
			test.Fatal(err)
		}
		if got := decisionWaitAlerts(test, fixture.issueID); got != 0 {
			test.Fatalf("answered last request alerted %d times", got)
		}
		requireNoWaitFlag(test, fixture.issueID)
	})
	test.Run("cancelled_only_still_flagged", func(test *testing.T) {
		fixture, _, _ := decisionWaitFixture(test)
		seedWaitRequest(test, fixture, fixture.issueID, "cancelled")
		if err := testHandler.checkDecisionWaits(context.Background()); err != nil {
			test.Fatal(err)
		}
		if got := decisionWaitAlerts(test, fixture.issueID); got != 1 {
			test.Fatalf("alerts = %d, want 1", got)
		}
	})
}

// CHE-4029 criterion 4: a squad lookup miss falls back to the project's owning
// squad (the squad its lead agent leads) before a route error is recorded.
func TestDecisionWaitGuardProjectSquadFallback(test *testing.T) {
	for _, scenario := range []string{"member_assignee", "multi_squad_agent", "no_project_squad"} {
		test.Run(scenario, func(test *testing.T) {
			fixture, squad, leader := decisionWaitFixture(test)
			dbfx.Exec(test, `UPDATE issue SET assignee_type = 'member', assignee_id = $1 WHERE id = $2`, fixture.humanID, fixture.issueID)
			dbfx.Exec(test, `UPDATE squad SET archived_at = clock_timestamp() WHERE id = $1`, squad)
			projectLeader := createHandlerTestAgent(test, "project leader", []byte("[]"))
			owning := dbfx.Insert(test, "squad", testutil.Cols{"workspace_id": testWorkspaceID, "name": "project squad", "creator_id": testUserID, "leader_id": projectLeader})
			lead := projectLeader
			if scenario == "no_project_squad" {
				lead = fixture.agentID
			}
			project := dbfx.Project(test, "guard project", testutil.Cols{"lead_type": "agent", "lead_id": lead})
			dbfx.Exec(test, `UPDATE issue SET project_id = $1 WHERE id = $2`, project, fixture.issueID)
			if scenario == "multi_squad_agent" {
				other := dbfx.Insert(test, "squad", testutil.Cols{"workspace_id": testWorkspaceID, "name": "other squad", "creator_id": testUserID, "leader_id": leader})
				for _, id := range []string{owning, other} {
					dbfx.Exec(test, `INSERT INTO squad_member (squad_id, member_type, member_id, role) VALUES ($1, 'agent', $2, 'member')`, id, fixture.agentID)
				}
				dbfx.Exec(test, `UPDATE issue SET assignee_type = 'agent', assignee_id = $1 WHERE id = $2`, fixture.agentID, fixture.issueID)
			}
			if err := testHandler.checkDecisionWaits(context.Background()); err != nil {
				test.Fatal(err)
			}
			if scenario == "no_project_squad" {
				if dbfx.Count(test, `SELECT count(*) FROM issue WHERE id = $1 AND metadata ? 'decision_wait_route_error'`, fixture.issueID) != 1 {
					test.Fatal("no project squad must still record the route error")
				}
				return
			}
			if dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND squad_id = $3 AND is_leader_task AND handoff_note LIKE 'Decision wait guard:%'`, fixture.issueID, projectLeader, owning) != 1 {
				test.Fatal("project owning squad leader was not alerted")
			}
			if dbfx.Count(test, `SELECT count(*) FROM issue WHERE id = $1 AND metadata ? 'decision_wait_route_error'`, fixture.issueID) != 0 {
				test.Fatal("route error recorded despite a project squad")
			}
		})
	}
}
