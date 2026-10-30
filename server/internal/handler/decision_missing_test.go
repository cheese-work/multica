package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestDecisionMissingRequestListScope(t *testing.T) {
	firstProject := dbfx.Project(t, "first guard project")
	secondProject := dbfx.Project(t, "second guard project")
	first := dbfx.Issue(t, "first scoped wait", testutil.Cols{"status": "need_decision", "project_id": firstProject})
	second := dbfx.Issue(t, "second scoped wait", testutil.Cols{"status": "need_decision", "project_id": secondProject})
	foreignWorkspace := dbfx.Workspace(t, "foreign guard workspace", "guard-foreign")
	foreign := dbfx.Issue(t, "foreign wait", testutil.Cols{"workspace_id": foreignWorkspace, "status": "need_decision"})
	for _, id := range []string{first, second, foreign} {
		ageMissingDecisionWait(t, id, "11 minutes")
	}
	for _, project := range []string{"", firstProject, secondProject} {
		var rows []map[string]any
		testutil.Call(t, testHandler.ListMissingDecisions, newRequest(http.MethodGet, "/api/decisions/missing?project_id="+project, nil)).Want(http.StatusOK).JSON(&rows)
		found := map[string]bool{}
		for _, row := range rows {
			found[row["issue_id"].(string)] = true
		}
		if found[foreign] || (project == firstProject && (!found[first] || found[second])) || (project == secondProject && (!found[second] || found[first])) || (project == "" && (!found[first] || !found[second])) {
			t.Fatalf("scope project=%q returned %+v", project, rows)
		}
	}
	testutil.Call(t, testHandler.ListMissingDecisions, newRequest(http.MethodGet, "/api/decisions/missing?project_id=invalid", nil)).Want(http.StatusBadRequest)
	request := newRequest(http.MethodGet, "/api/decisions/missing", nil)
	request.Header.Set("X-Workspace-ID", foreignWorkspace)
	testutil.Call(t, testHandler.ListMissingDecisions, request).Want(http.StatusNotFound)
}

func TestDecisionMissingRequestCreationResetsEpisode(t *testing.T) {
	resetDecisionLease(t)
	dbfx.Insert(t, "issue_status", testutil.Cols{"workspace_id": testWorkspaceID, "key": "need_decision", "name": "Need Decision", "category": "started", "color": "#123456"})
	fixture := newRequestCoreFixture(t, false)
	dbfx.Exec(t, `UPDATE issue SET status = 'need_decision' WHERE id = $1`, fixture.issueID)
	ageMissingDecisionWait(t, fixture.issueID, "11 minutes")
	// A foreign-workspace request with the same issue id cannot suppress this wait.
	foreign := dbfx.Workspace(t, "foreign request workspace", "guard-request-foreign")
	foreignRequest := dbfx.Insert(t, "decision_request", testutil.Cols{"workspace_id": foreign, "issue_id": fixture.issueID,
		"asking_agent_id": fixture.agentID, "human_user_id": fixture.humanID, "chat_session_id": fixture.sessionID, "escalation_class": "deploy", "origin": "asked"})
	if count := dbfx.Count(t, `SELECT count(*) FROM decision_missing_wait WHERE issue_id = $1`, fixture.issueID); count != 1 {
		t.Fatal("foreign request suppressed wait")
	}
	dbfx.Exec(t, `DELETE FROM decision_request WHERE id = $1`, foreignRequest)
	created, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input())
	if err != nil {
		t.Fatal(err)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM decision_missing_wait WHERE issue_id = $1`, fixture.issueID); count != 0 {
		t.Fatal("request did not clear eligibility")
	}
	// Cancelling a request while still waiting starts a full new grace period.
	dbfx.Exec(t, `UPDATE decision_request SET status = 'cancelled', cancel_reason = 'agent_withdrawn' WHERE id = $1`, created.Request.ID)
	if count := dbfx.Count(t, `SELECT count(*) FROM decision_missing_wait WHERE issue_id = $1 AND started_at > clock_timestamp() - interval '1 minute'`, fixture.issueID); count != 1 {
		t.Fatal("next wait episode did not restart the clock")
	}
}

func TestDecisionMissingRequestRollback(t *testing.T) {
	resetDecisionLease(t)
	leader := createHandlerTestAgent(t, "guard rollback leader", []byte("[]"))
	squad := dbfx.Squad(t, "guard rollback squad", leader)
	issue := dbfx.Issue(t, "guard rollback", testutil.Cols{"status": "need_decision", "assignee_type": "squad", "assignee_id": squad})
	ageMissingDecisionWait(t, issue, "11 minutes")
	handler, worker := newDecisionRunnerFixture(t)
	handler.TxStarter = decisionFailCommit{testPool}
	if err := handler.deliverMissingDecisionAlert(context.Background(), parseUUID(testWorkspaceID), parseUUID(issue)); err == nil {
		t.Fatal("commit failure was hidden")
	}
	for _, table := range []string{"comment", "inbox_item", "agent_task_queue"} {
		if count := dbfx.Count(t, `SELECT count(*) FROM `+table+` WHERE issue_id = $1`, issue); count != 0 {
			t.Fatalf("rollback left %d %s rows", count, table)
		}
	}
	handler.TxStarter = testPool
	decisionPass(t, worker)
	if count := dbfx.Count(t, `SELECT count(*) FROM inbox_item WHERE issue_id = $1`, issue); count != 1 {
		t.Fatalf("retry deliveries = %d, want 1", count)
	}
}

func TestDecisionMissingRequestPrivateLeader(t *testing.T) {
	resetDecisionLease(t)
	requester := dbfx.User(t, "guard requester", "guard-requester@test.invalid")
	dbfx.Member(t, testWorkspaceID, requester, "member")
	leader := createHandlerTestAgent(t, "private guard leader", []byte("[]"))
	dbfx.Exec(t, `UPDATE agent SET permission_mode = 'private', visibility = 'private' WHERE id = $1`, leader)
	dbfx.Exec(t, `DELETE FROM agent_invocation_target WHERE agent_id = $1`, leader)
	squad := dbfx.Squad(t, "private leader squad", leader)
	issue := dbfx.Issue(t, "private leader wait", testutil.Cols{"status": "need_decision", "assignee_type": "squad", "assignee_id": squad, "creator_id": requester, "requested_by_id": requester})
	ageMissingDecisionWait(t, issue, "11 minutes")
	_, worker := newDecisionRunnerFixture(t)
	decisionPass(t, worker)
	if count := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issue); count != 0 {
		t.Fatalf("guard invoked a private leader without requester access: tasks=%d", count)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM inbox_item WHERE issue_id = $1 AND recipient_id = $2`, issue, leader); count != 1 {
		t.Fatalf("leader notifications = %d, want 1", count)
	}
}

// The old worker has no episode store. Keep the baseline runnable so the red
// receipt fails on the missing alert rather than on missing schema.
func ageMissingDecisionWait(t *testing.T, issueID, age string) {
	t.Helper()
	var exists bool
	dbfx.QueryRow(t, `SELECT to_regclass('decision_missing_wait') IS NOT NULL`).Scan(&exists)
	if exists {
		dbfx.Exec(t, `UPDATE decision_missing_wait SET started_at = clock_timestamp() - $2::interval WHERE issue_id = $1`, issueID, age)
	}
}

func TestDecisionMissingRequestAlert(t *testing.T) {
	resetDecisionLease(t)
	leader := createHandlerTestAgent(t, "missing-request leader", []byte("[]"))
	squad := dbfx.Squad(t, "missing-request owner", leader)
	issue := dbfx.Issue(t, "human wait without a request", testutil.Cols{
		"status": "need_decision", "assignee_type": "squad", "assignee_id": squad,
	})
	_, worker := newDecisionRunnerFixture(t)
	count := func(want int) {
		t.Helper()
		if got := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content LIKE 'Decision request missing:%'`, issue); got != want {
			t.Fatalf("missing-request alerts = %d, want %d", got, want)
		}
	}
	decisionPass(t, worker)
	count(0)
	ageMissingDecisionWait(t, issue, "9 minutes 59 seconds")
	decisionPass(t, worker)
	count(0)
	ageMissingDecisionWait(t, issue, "10 minutes 1 second")
	decisionPass(t, worker)
	count(1)
	if got := dbfx.Count(t, `SELECT count(*) FROM inbox_item WHERE issue_id = $1 AND recipient_type = 'agent' AND recipient_id = $2 AND details->>'kind' = 'decision_request_missing'`, issue, leader); got != 1 {
		t.Fatalf("leader alert deliveries = %d, want 1", got)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND squad_id = $3 AND is_leader_task AND originator_user_id = $4`, issue, leader, squad, testUserID); got != 1 {
		t.Fatalf("leader role/provenance deliveries = %d, want 1", got)
	}
	decisionPass(t, worker)
	count(1)
	dbfx.Exec(t, `UPDATE decision_worker_lease SET expires_at = clock_timestamp() - interval '1 second'`)
	_, restarted := newDecisionRunnerFixture(t)
	decisionPass(t, restarted)
	count(1)
	// End/re-enter without an intervening scan: this must be a fresh episode.
	dbfx.Exec(t, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, issue)
	dbfx.Exec(t, `UPDATE issue SET status = 'need_decision' WHERE id = $1`, issue)
	decisionPass(t, restarted)
	count(1)
	ageMissingDecisionWait(t, issue, "11 minutes")
	decisionPass(t, restarted)
	count(2)
}

func TestDecisionMissingRequestControls(t *testing.T) {
	for _, control := range []string{"request", "done", "cancelled", "custom-terminal", "custom-closed", "readiness", "agent-owner", "member-owner", "task-owner", "ambiguous-owner", "foreign-owner", "archived-owner", "no-owner"} {
		t.Run(control, func(t *testing.T) {
			resetDecisionLease(t)
			fixture := newIntentFixture(t)
			leader := createHandlerTestAgent(t, "guard control leader", []byte("[]"))
			squad := dbfx.Squad(t, "guard control squad", leader)
			dbfx.Exec(t, `UPDATE issue SET status = 'need_decision', assignee_type = 'squad', assignee_id = $2 WHERE id = $1`, fixture.issueID, squad)
			switch control {
			case "request":
				session := dbfx.Insert(t, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": fixture.agentID, "creator_id": fixture.humanID})
				dbfx.Insert(t, "decision_request", testutil.Cols{
					"workspace_id": testWorkspaceID, "issue_id": fixture.issueID, "asking_agent_id": fixture.agentID,
					"human_user_id": fixture.humanID, "chat_session_id": session,
					"escalation_class": "deploy", "ask": "request is present", "status": "open", "origin": "asked",
				})
			case "done", "cancelled":
				dbfx.Exec(t, `UPDATE issue SET status = $2 WHERE id = $1`, fixture.issueID, control)
			case "custom-terminal", "custom-closed":
				category := "done"
				if control == "custom-closed" {
					category = "closed"
				}
				dbfx.Insert(t, "issue_status", testutil.Cols{"workspace_id": testWorkspaceID, "key": "guard_finished", "name": "Finished", "category": category, "color": "#123456"})
				dbfx.Exec(t, `UPDATE issue SET status = 'guard_finished' WHERE id = $1`, fixture.issueID)
			case "readiness":
				property := dbfx.Insert(t, "issue_property", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Readiness", "type": "select", "config": `{"options":[{"id":"human","name":"gated-human"}]}`})
				dbfx.Exec(t, `UPDATE issue SET status = 'in_progress', properties = jsonb_build_object($2::text, 'human') WHERE id = $1`, fixture.issueID, property)
			case "agent-owner", "member-owner":
				typ, id := "agent", fixture.agentID
				if control == "member-owner" {
					typ, id = "member", testUserID
				}
				dbfx.SquadMember(t, squad, typ, id)
				dbfx.Exec(t, `UPDATE issue SET assignee_type = $2, assignee_id = $3 WHERE id = $1`, fixture.issueID, typ, id)
			case "ambiguous-owner":
				other := dbfx.Squad(t, "another guard squad", leader)
				dbfx.SquadMember(t, squad, "agent", fixture.agentID)
				dbfx.SquadMember(t, other, "agent", fixture.agentID)
				dbfx.Exec(t, `UPDATE issue SET assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, fixture.issueID, fixture.agentID)
			case "task-owner":
				dbfx.Task(t, fixture.agentID, testutil.Cols{"issue_id": fixture.issueID, "squad_id": squad, "status": "completed", "runtime_id": handlerTestRuntimeID(t)})
				dbfx.Exec(t, `UPDATE issue SET assignee_type = 'member', assignee_id = $2 WHERE id = $1`, fixture.issueID, fixture.humanID)
			case "foreign-owner":
				foreign := dbfx.Workspace(t, "foreign owner workspace", "guard-owner-foreign")
				dbfx.Exec(t, `UPDATE squad SET workspace_id = $2 WHERE id = $1`, squad, foreign)
			case "archived-owner":
				dbfx.Exec(t, `UPDATE squad SET archived_at = clock_timestamp() WHERE id = $1`, squad)
			case "no-owner":
				dbfx.Exec(t, `UPDATE issue SET assignee_type = 'member', assignee_id = $2 WHERE id = $1`, fixture.issueID, fixture.humanID)
			}
			if control == "done" || control == "cancelled" || control == "custom-terminal" || control == "custom-closed" {
				property := dbfx.Insert(t, "issue_property", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Readiness", "type": "select", "config": `{"options":[{"id":"human","name":"gated-human"}]}`})
				dbfx.Exec(t, `UPDATE issue SET properties = jsonb_build_object($2::text, 'human') WHERE id = $1`, fixture.issueID, property)
			}
			ageMissingDecisionWait(t, fixture.issueID, "11 minutes")
			_, worker := newDecisionRunnerFixture(t)
			decisionPass(t, worker)
			want := 1
			if control == "request" || control == "done" || control == "cancelled" || control == "custom-terminal" || control == "custom-closed" {
				want = 0
			}
			if got := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content LIKE 'Decision request missing:%'`, fixture.issueID); got != want {
				t.Fatalf("alerts = %d, want %d", got, want)
			}
			if control == "ambiguous-owner" || control == "no-owner" || control == "foreign-owner" || control == "archived-owner" {
				if got := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content LIKE '%owning squad is unresolved%'`, fixture.issueID); got != 1 {
					t.Fatalf("ownership reports = %d, want 1", got)
				}
				decisionPass(t, worker)
				if got := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content LIKE 'Decision request missing:%'`, fixture.issueID); got != 1 {
					t.Fatalf("duplicate ownership reports = %d", got)
				}
				if control == "no-owner" {
					dbfx.SquadMember(t, squad, "member", fixture.humanID)
					decisionPass(t, worker)
					if got := dbfx.Count(t, `SELECT count(*) FROM inbox_item WHERE issue_id = $1 AND recipient_id = $2`, fixture.issueID, leader); got != 1 {
						t.Fatalf("repaired-owner deliveries = %d, want 1", got)
					}
				}
			} else if want == 1 {
				if got := dbfx.Count(t, `SELECT count(*) FROM inbox_item WHERE issue_id = $1 AND recipient_id = $2`, fixture.issueID, leader); got != 1 {
					t.Fatalf("leader deliveries = %d, want 1", got)
				}
			}
		})
	}
}
