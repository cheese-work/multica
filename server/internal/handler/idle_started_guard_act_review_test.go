package handler

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// finding returns the stale scan result for the fixture issue.
func (fixture idleActFixture) finding(test *testing.T) idleGuardFinding {
	test.Helper()
	findings, err := testHandler.checkIdleStartedIssues(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	for _, finding := range findings {
		if uuidToString(finding.IssueID) == fixture.issue {
			return finding
		}
	}
	test.Fatal("fixture issue was not found idle")
	return idleGuardFinding{}
}

// neutralComment leaves no human-wait evidence in the newest comment.
func (fixture idleActFixture) neutralComment(test *testing.T) {
	test.Helper()
	dbfx.Exec(test, `UPDATE comment SET content = 'finished my part' WHERE issue_id = $1`, fixture.issue)
}

func (fixture idleActFixture) openRequest(test *testing.T) {
	test.Helper()
	session := dbfx.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": fixture.agent, "creator_id": testUserID, "title": "idle request"})
	ping := dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": session, "role": "assistant", "content": "Approve?", "message_kind": "decision_request", "sent_via": "task_token"})
	dbfx.Insert(test, "decision_request", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": fixture.issue, "asking_agent_id": fixture.agent, "human_user_id": testUserID,
		"chat_session_id": session, "ping_message_id": ping, "ask": "Approve?", "escalation_class": "reversible", "origin": "asked", "status": "open"})
	test.Cleanup(func() { dbfx.Exec(test, `DELETE FROM decision_request WHERE issue_id = $1`, fixture.issue) })
}

// workflow puts the fixture issue on a project workflow with the given steps.
func (fixture idleActFixture) workflow(test *testing.T, steps []issueworkflow.Step) {
	test.Helper()
	raw, err := issueworkflow.EncodeSteps(steps)
	if err != nil {
		test.Fatal(err)
	}
	flow := dbfx.Insert(test, "issue_workflow", testutil.Cols{"workspace_id": testWorkspaceID, "name": "idle " + workflowTestSuffix(), "initial_status_key": steps[0].StatusKey, "steps": string(raw)})
	test.Cleanup(func() {
		dbfx.Exec(test, `UPDATE project SET workflow_id = NULL WHERE workflow_id = $1`, flow)
		dbfx.Exec(test, `DELETE FROM issue_workflow WHERE id = $1`, flow)
	})
	project := createWorkflowTestProject(test, "idle guard project")
	dbfx.Exec(test, `UPDATE project SET workflow_id = $1 WHERE id = $2`, flow, project)
	dbfx.Exec(test, `UPDATE issue SET project_id = $1 WHERE id = $2`, project, fixture.issue)
}

func (fixture idleActFixture) dueDate(test *testing.T) bool {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM issue WHERE id = $1 AND due_date IS NOT NULL`, fixture.issue) == 1
}

func TestIdleStartedGuardActKeepsFieldEditsThatLandBeforeTheMove(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	found := fixture.finding(test)
	idleGuardBeforeMove = func() {
		dbfx.Exec(test, `UPDATE issue SET due_date = now() + interval '3 days', revision = revision + 1 WHERE id = $1`, fixture.issue)
	}
	test.Cleanup(func() { idleGuardBeforeMove = func() {} })
	if err := testHandler.actOnIdleFinding(context.Background(), idleGuardSettings{}, found); err != nil {
		test.Fatal(err)
	}
	idleGuardBeforeMove = func() {}
	if got := fixture.status(test); got != "in_progress" {
		test.Fatalf("status = %q, a move built from a stale read must not commit", got)
	}
	if !fixture.dueDate(test) {
		test.Fatal("the due date edit was overwritten by the stale move")
	}
	fixture.sweep(test)
	if got := fixture.status(test); got != "need_decision" || !fixture.dueDate(test) {
		test.Fatalf("next pass: status = %q, due date kept = %v, want need_decision and true", got, fixture.dueDate(test))
	}
}

func TestIdleStartedGuardActDoesNotMoveWhenATaskStartsDuringTheMove(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	found := fixture.finding(test)
	var actErr error
	raceUntilBlocked(test, func(tx pgx.Tx) {
		queries := testHandler.Queries.WithTx(tx)
		if _, err := queries.LockIssueRunBudget(context.Background(), parseUUID(fixture.issue)); err != nil {
			test.Fatal(err)
		}
		issue, err := queries.GetIssue(context.Background(), parseUUID(fixture.issue))
		if err != nil {
			test.Fatal(err)
		}
		if _, err := testHandler.TaskService.EnqueueGuardLeaderTaskInTx(context.Background(), queries, issue, parseUUID(fixture.leader), parseUUID(fixture.squad), "start", "race", "race"); err != nil {
			test.Fatal(err)
		}
	}, func() { actErr = testHandler.actOnIdleFinding(context.Background(), idleGuardSettings{}, found) })
	if actErr != nil {
		test.Fatal(actErr)
	}
	if got := fixture.status(test); got != "in_progress" || fixture.receipt(test, "state") != "" {
		test.Fatalf("status = %q, receipt = %q: a task that started during the move must keep the issue where it is", got, fixture.receipt(test, "state"))
	}
}

func TestIdleStartedGuardActOpenDecisionRequestWaitsWithoutATask(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	fixture.neutralComment(test)
	fixture.openRequest(test)
	fixture.sweep(test)
	if fixture.status(test) != "need_decision" || fixture.receipt(test, "kind") != "open_request" {
		test.Fatalf("status = %q, kind = %q, want need_decision and open_request", fixture.status(test), fixture.receipt(test, "kind"))
	}
	if fixture.systemComments(test) != 1 || fixture.leaderTasks(test) != 0 {
		test.Fatalf("system comments = %d, leader tasks = %d, want 1 and 0", fixture.systemComments(test), fixture.leaderTasks(test))
	}
}

func TestIdleStartedGuardActGatedHumanReadinessWaits(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	fixture.neutralComment(test)
	property := dbfx.Insert(test, "issue_property", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Readiness", "type": "select", "config": `{"options":[{"id":"gate","name":"gated-human"}]}`})
	dbfx.Exec(test, `UPDATE issue SET properties = jsonb_build_object($1::text, 'gate') WHERE id = $2`, property, fixture.issue)
	fixture.sweep(test)
	if got := fixture.status(test); got != "need_decision" {
		test.Fatalf("status = %q, want need_decision for a gated-human issue", got)
	}
}

func TestIdleStartedGuardActWorkflowHumanHandlerWaits(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	fixture.neutralComment(test)
	steps := bareSteps("backlog", "in_progress", "need_decision", "blocked", "done")
	steps[1].Handler = issueworkflow.Handler{Type: issueworkflow.HandlerMember, ID: testUserID}
	fixture.workflow(test, steps)
	fixture.sweep(test)
	if got := fixture.status(test); got != "need_decision" {
		test.Fatalf("status = %q, want need_decision for a step handled by a person", got)
	}
}

func TestIdleStartedGuardActFallsBackToBlockedWithoutNeedDecision(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	steps := bareSteps("backlog", "in_progress", "blocked", "done")
	fixture.workflow(test, steps)
	fixture.sweep(test)
	if fixture.status(test) != "blocked" || fixture.receipt(test, "kind") != "human_wait" {
		test.Fatalf("status = %q, kind = %q, want blocked and human_wait", fixture.status(test), fixture.receipt(test, "kind"))
	}
}

func TestIdleStartedGuardActNoticeSkipsIssueThatResumed(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	dbfx.Exec(test, `UPDATE squad SET archived_at = clock_timestamp() WHERE id = $1`, fixture.squad)
	fixture.sweepFailing(test)
	dbfx.Exec(test, `UPDATE squad SET archived_at = NULL WHERE id = $1`, fixture.squad)
	dbfx.Comment(test, fixture.issue, "back at it", testutil.Cols{"author_type": "member", "author_id": testUserID})
	fixture.sweep(test)
	if fixture.leaderTasks(test) != 0 {
		test.Fatal("an issue that resumed after the move still alerted the leader")
	}
	if got := fixture.receipt(test, "state"); got != "resumed" {
		test.Fatalf("state = %q, want resumed", got)
	}
}

func TestIdleStartedGuardActNoticeReusesTheDecisionWaitAlert(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	dbfx.Exec(test, `UPDATE squad SET archived_at = clock_timestamp() WHERE id = $1`, fixture.squad)
	fixture.sweepFailing(test)
	dbfx.Exec(test, `UPDATE squad SET archived_at = NULL WHERE id = $1`, fixture.squad)
	alert := dbfx.Task(test, fixture.leader, testutil.Cols{"issue_id": fixture.issue, "status": "completed", "runtime_id": handlerTestRuntimeID(test)})
	dbfx.Exec(test, `UPDATE issue SET metadata = jsonb_set(metadata, '{decision_wait_alert_id}', to_jsonb($1::text)) WHERE id = $2`, alert, fixture.issue)
	fixture.sweep(test)
	if fixture.leaderTasks(test) != 0 {
		test.Fatal("a second leader task was admitted beside the decision wait alert")
	}
	if fixture.receipt(test, "state") != "notified" || fixture.receipt(test, "alert_id") != alert {
		test.Fatalf("state = %q, alert = %q, want notified and %s", fixture.receipt(test, "state"), fixture.receipt(test, "alert_id"), alert)
	}
}

func TestIdleStartedGuardActKeepsTheStatusExplanationWhenAdmissionFails(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	dbfx.Exec(test, `UPDATE squad SET archived_at = clock_timestamp() WHERE id = $1`, fixture.squad)
	fixture.sweepFailing(test)
	if fixture.systemComments(test) != 1 || fixture.receipt(test, "comment_id") == "" {
		test.Fatalf("system comments = %d, comment id = %q: the status explanation was lost with the failed admission", fixture.systemComments(test), fixture.receipt(test, "comment_id"))
	}
	for range 4 {
		_ = testHandler.SweepIdleStartedIssues(context.Background())
	}
	if fixture.receipt(test, "state") != "gave_up" || fixture.receipt(test, "error") == "" {
		test.Fatalf("state = %q, error = %q, want gave_up with the last error", fixture.receipt(test, "state"), fixture.receipt(test, "error"))
	}
	if fixture.systemComments(test) != 2 {
		test.Fatalf("system comments = %d, want the explanation and one give-up note", fixture.systemComments(test))
	}
}

func TestIdleStartedGuardActRefusedMoveGivesUpVisibly(test *testing.T) {
	fixture := newIdleActFixture(test, 5)
	// A workflow that lists neither target status refuses every move.
	fixture.workflow(test, bareSteps("backlog", "in_progress", "done"))
	for range 4 {
		_ = testHandler.SweepIdleStartedIssues(context.Background())
	}
	if fixture.status(test) != "in_progress" {
		test.Fatalf("status = %q, the refused move must not land", fixture.status(test))
	}
	if fixture.receipt(test, "state") != "gave_up" || fixture.receipt(test, "attempts") != "3" || fixture.receipt(test, "error") == "" {
		test.Fatalf("state = %q, attempts = %q, error = %q, want gave_up, 3 and the reason", fixture.receipt(test, "state"), fixture.receipt(test, "attempts"), fixture.receipt(test, "error"))
	}
	if fixture.systemComments(test) != 1 || fixture.leaderTasks(test) != 1 {
		test.Fatalf("system comments = %d, leader tasks = %d, want a give-up note and one alert", fixture.systemComments(test), fixture.leaderTasks(test))
	}
}
