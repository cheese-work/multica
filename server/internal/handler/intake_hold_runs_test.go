package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/dispatch"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-7075 (E2): a derived run on a held issue is refused with
// service.ErrIntakeHold and the dispatch reason intake_hold. A member's mention
// and a wakeup a member created still start a run. The fixture is the one the
// triage rule uses, so both rules are judged on the same targets.

// heldRunIssue is an issue the hold owns, assigned to the fixture agent.
func heldRunIssue(test *testing.T, f triageRunFixture, hold holdFixture, title string, over ...testutil.Cols) (string, db.Issue) {
	test.Helper()
	cols := testutil.Cols{"assignee_type": "agent", "assignee_id": f.agentID, "number": nextWorkspaceIssueNumber(test)}
	for _, extra := range over {
		for key, value := range extra {
			cols[key] = value
		}
	}
	issueID := hold.issue(test, title, cols)
	dbfx.Cleanup(test, `DELETE FROM comment WHERE issue_id = $1`, issueID)
	dbfx.Cleanup(test, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		test.Fatal(err)
	}
	return issueID, issue
}

// derivedRuns are the enqueue funnels a derived run reaches. Each one returns
// the enqueue error.
func derivedRuns(f triageRunFixture, issue db.Issue) map[string]func() error {
	ctx := context.Background()
	agent, leader, squad := parseUUID(f.agentID), parseUUID(f.leaderID), parseUUID(f.squadID)
	tasks := testHandler.TaskService
	return map[string]func() error{
		"assignment to an agent": func() error { _, err := tasks.EnqueueTaskForIssue(ctx, issue); return err },
		"agent mention or reply": func() error {
			_, err := tasks.EnqueueTaskForMention(ctx, issue, agent, pgtype.UUID{}, service.OriginDerived)
			return err
		},
		"assignment to a squad": func() error {
			_, err := tasks.EnqueueTaskForSquadLeaderByActor(ctx, issue, leader, squad, pgtype.UUID{})
			return err
		},
		"squad leader run": func() error {
			_, err := tasks.EnqueueTaskForSquadLeader(ctx, issue, leader, squad, pgtype.UUID{}, service.OriginDerived)
			return err
		},
		"rerun of the assignee": func() error {
			_, err := tasks.RerunIssue(ctx, issue.ID, pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, false, nil)
			return err
		},
	}
}

func TestIntakeHoldRefusesDerivedRuns(test *testing.T) {
	f := newTriageRunFixture(test)
	hold := newHoldFixture(test, nil)

	// The control: the same calls on an issue the hold does not own start a run,
	// so the refusals below are the hold's doing and not the fixture's.
	test.Run("control: an issue outside the hold runs", func(test *testing.T) {
		// One issue per funnel: a second enqueue on one issue meets the pending-task guard.
		for name := range derivedRuns(f, db.Issue{}) {
			issueID, issue := heldRunIssue(test, f, hold, "feat: control "+name, testutil.Cols{"status": "todo"})
			if err := derivedRuns(f, issue)[name](); err != nil {
				test.Fatalf("%s on an unheld issue = %v, want it admitted", name, err)
			}
			if tasksOn(test, issueID) == 0 {
				test.Fatalf("%s: no run on the unheld issue, so the refusals prove nothing", name)
			}
		}
	})

	for _, status := range []string{"backlog", "todo"} {
		test.Run("held in "+status, func(test *testing.T) {
			title := "feat: held " + status
			cols := testutil.Cols{"status": status}
			if status == "todo" {
				// A parked issue is held while the park row says it came from backlog.
				cols = testutil.Cols{"status": "need_decision"}
			}
			issueID, issue := heldRunIssue(test, f, hold, title, cols)
			if status == "todo" {
				dbfx.Exec(test, `INSERT INTO issue_decision_park (issue_id, workspace_id, prior_status, to_status) VALUES ($1, $2, 'backlog', 'need_decision')`, issueID, testWorkspaceID)
			}
			for name, call := range derivedRuns(f, issue) {
				if err := call(); !errors.Is(err, service.ErrIntakeHold) {
					test.Errorf("%s = %v, want ErrIntakeHold", name, err)
				}
			}
			if n := tasksOn(test, issueID); n != 0 {
				test.Fatalf("refused enqueues wrote %d task row(s)", n)
			}
		})
	}
}

func TestIntakeHoldAdmitsAMemberNamedRun(test *testing.T) {
	f := newTriageRunFixture(test)
	hold := newHoldFixture(test, nil)
	issueID, issue := heldRunIssue(test, f, hold, "feat: named")
	ctx := context.Background()
	if _, err := testHandler.TaskService.EnqueueTaskForMention(ctx, issue, parseUUID(f.agentID), pgtype.UUID{}, service.OriginNamed); err != nil {
		test.Fatalf("a member's mention = %v, want it admitted", err)
	}
	if _, err := testHandler.TaskService.EnqueueTaskForThreadParent(ctx, issue, parseUUID(f.leaderID), pgtype.UUID{}); err != nil {
		test.Fatalf("a member's reply = %v, want it admitted", err)
	}
	if n := tasksOn(test, issueID); n != 2 {
		test.Fatalf("named runs wrote %d task row(s), want 2", n)
	}
	// The same through a comment: its trigger outcome is queued, not intake_hold.
	var body struct {
		TriggerOutcomes []struct {
			Status     string `json:"status"`
			ReasonCode string `json:"reason_code"`
		} `json:"trigger_outcomes"`
	}
	testutil.Call(test, testHandler.CreateComment, withURLParam(
		newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{
			"content": "[@Agent](mention://agent/" + f.agentID + ") look",
		}), "id", issueID)).Want(http.StatusCreated).JSON(&body)
	for _, outcome := range body.TriggerOutcomes {
		if outcome.ReasonCode == string(dispatch.ReasonIntakeHold) {
			test.Fatalf("a member's mention was refused: %+v", outcome)
		}
	}
}

// A release by a person and an effective escalation answer end the hold for the
// derived run too; an answer that is not yet effective does not. Each release is
// judged through both an assignment and a mention, on a fresh issue each, since a
// second enqueue on one issue meets the pending-task guard.
func TestIntakeHoldReleaseAdmitsDerivedRuns(test *testing.T) {
	f := newTriageRunFixture(test)
	hold := newHoldFixture(test, func(entry map[string]any) { entry["exempt_escalation_classes"] = []string{"spend"} })
	funnels := map[string]func(db.Issue) error{
		"assignment": func(issue db.Issue) error {
			_, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue)
			return err
		},
		"mention": func(issue db.Issue) error {
			_, err := testHandler.TaskService.EnqueueTaskForMention(context.Background(), issue, parseUUID(f.agentID), pgtype.UUID{}, service.OriginDerived)
			return err
		},
	}
	cases := []struct {
		name     string
		admitted bool
		release  func(test *testing.T, issueID string)
	}{
		{"form a: a person moved it out of backlog, now back", true, func(test *testing.T, issueID string) {
			statusRow(test, issueID, "member", "ui_or_api", "interactive_jwt", "backlog", "todo", nil)
		}},
		{"form b: release key, row older than the park decision", true, func(test *testing.T, issueID string) {
			statusRow(test, issueID, "member", "ui_or_api", "pat", "need_decision", "todo", map[string]any{"intake_release": "CHE-6911"})
			dbfx.Exec(test, `INSERT INTO issue_decision_park (issue_id, workspace_id, prior_status, to_status) VALUES ($1, $2, 'backlog', 'need_decision')`, issueID, testWorkspaceID)
		}},
		{"escalation answer not yet effective", false, func(test *testing.T, issueID string) {
			escalationAnswer(test, issueID, "spend", "now() + interval '10 minutes'")
		}},
		{"escalation answer after wake_at", true, func(test *testing.T, issueID string) {
			escalationAnswer(test, issueID, "spend", "now() - interval '1 second'")
		}},
	}
	for _, c := range cases {
		for name, enqueue := range funnels {
			test.Run(c.name+" / "+name, func(test *testing.T) {
				issueID, issue := heldRunIssue(test, f, hold, "feat: release "+name)
				c.release(test, issueID)
				err := enqueue(issue)
				switch {
				case c.admitted && err != nil:
					test.Fatalf("%s after the release = %v, want it admitted", name, err)
				case c.admitted && tasksOn(test, issueID) != 1:
					test.Fatalf("%s after the release wrote %d task row(s), want 1", name, tasksOn(test, issueID))
				case !c.admitted && !errors.Is(err, service.ErrIntakeHold):
					test.Fatalf("%s = %v, want ErrIntakeHold", name, err)
				case !c.admitted && tasksOn(test, issueID) != 0:
					test.Fatalf("refused %s wrote %d task row(s)", name, tasksOn(test, issueID))
				}
			})
		}
	}
}

// wakeupOn creates an event wakeup for the fixture agent on issueID. With
// source true the rule is created from a task an agent is running, so the run
// it starts is derived; without, a person made it and the run is named.
func wakeupOn(test *testing.T, f triageRunFixture, issueID string, source bool) (*service.IssueWakeupService, db.IssueWakeup) {
	test.Helper()
	ctx := context.Background()
	svc := &service.IssueWakeupService{Tasks: testHandler.TaskService}
	wakeup, err := svc.Create(ctx, parseUUID(issueID), parseUUID(testUserID), pgtype.UUID{}, service.WakeupInput{
		AgentID: f.agentID, Kind: "event", EventTypes: []string{"issue.status_changed"}, Instruction: "look again",
	})
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM issue_wakeup_receipt WHERE wakeup_id = $1`, wakeup.ID)
		testPool.Exec(ctx, `DELETE FROM issue_wakeup WHERE issue_id = $1`, issueID)
	})
	if source {
		// The source run belongs to another agent, so it does not meet the
		// pending-task guard of the run the wakeup starts.
		sourceTask := createHandlerTestTaskForAgentOnIssue(test, handlerTestAgentID(test), issueID)
		dbfx.Exec(test, `UPDATE issue_wakeup SET source_task_id = $2 WHERE id = $1`, wakeup.ID, sourceTask)
	}
	return svc, wakeup
}

// A wakeup an agent created is derived and refused: no run starts and the rule
// records the reason. One a person created is named and starts its run.
func TestIntakeHoldWakeupDispatch(test *testing.T) {
	f := newTriageRunFixture(test)
	hold := newHoldFixture(test, nil)
	ctx := context.Background()
	fire := func(test *testing.T, source bool) (issueID, lastError string, wakeupRuns int) {
		test.Helper()
		issueID, _ = heldRunIssue(test, f, hold, "feat: wakeup")
		svc, wakeup := wakeupOn(test, f, issueID, source)
		if err := svc.Trigger(ctx, parseUUID(issueID), wakeup.ID, parseUUID(testUserID)); err != nil {
			test.Fatalf("trigger: %v", err)
		}
		var recorded *string
		dbfx.QueryRow(test, `SELECT last_error FROM issue_wakeup WHERE id = $1`, wakeup.ID).Scan(&recorded)
		if recorded != nil {
			lastError = *recorded
		}
		wakeupRuns = dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND context->>'wakeup_id' = $2`, issueID, uuidToString(wakeup.ID))
		return issueID, lastError, wakeupRuns
	}
	test.Run("created by an agent: no run, last_error intake_hold", func(test *testing.T) {
		_, lastError, runs := fire(test, true)
		if runs != 0 {
			test.Fatalf("the refused wakeup started %d run(s)", runs)
		}
		if !strings.HasPrefix(lastError, "intake_hold") {
			test.Fatalf("last_error = %q, want it to start with intake_hold", lastError)
		}
	})
	test.Run("created by a person: a queued run, no error", func(test *testing.T) {
		issueID, lastError, runs := fire(test, false)
		if runs != 1 {
			test.Fatalf("the wakeup started %d run(s), want 1", runs)
		}
		if lastError != "" {
			test.Fatalf("last_error = %q, want none", lastError)
		}
		if n := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued'`, issueID); n != 1 {
			test.Fatalf("queued runs = %d, want 1", n)
		}
	})
}

// The retry that follows a failed run is derived too; it stops without an error.
func TestIntakeHoldRefusesRetry(test *testing.T) {
	f := newTriageRunFixture(test)
	hold := newHoldFixture(test, nil)
	ctx := context.Background()
	issueID, _ := heldRunIssue(test, f, hold, "feat: retry")
	taskID := dbfx.Task(test, f.agentID, testutil.Cols{
		"runtime_id": f.runtimeID, "issue_id": issueID, "status": "failed", "failure_reason": "timeout",
		"attempt": 1, "max_attempts": 2,
		"started_at": testutil.Raw("now() - interval '2 minutes'"), "completed_at": testutil.Raw("now() - interval '1 minute'"),
	})
	parent, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(taskID))
	if err != nil {
		test.Fatal(err)
	}
	child, err := testHandler.TaskService.MaybeRetryFailedTask(ctx, parent)
	if err != nil || child != nil {
		test.Fatalf("auto-retry on a held issue = (%v, %v), want skipped", child, err)
	}
	if n := tasksOn(test, issueID); n != 1 {
		test.Fatalf("task rows after the refused retry = %d, want only the failed run", n)
	}
}

// An agent's comment that mentions an agent is a derived run: its trigger
// outcome is blocked with intake_hold, and a person's comment is not.
func TestIntakeHoldCommentTriggerOutcomes(test *testing.T) {
	f := newTriageRunFixture(test)
	hold := newHoldFixture(test, nil)
	mention := "[@Agent](mention://agent/" + f.agentID + ") look"
	issueID, _ := heldRunIssue(test, f, hold, "feat: comment")
	authorID := handlerTestAgentID(test)
	sourceTask := createAgentCommentTaskForPolicyTest(test, authorID, issueID)
	before := tasksOn(test, issueID)

	outcomes := postAgentCommentForTriggerPolicyTest(test, issueID, authorID, sourceTask, mention)
	if len(outcomes) != 1 || outcomes[0].TargetID != f.agentID || outcomes[0].ReasonCode != ReasonIntakeHold || outcomes[0].Status != DispatchBlocked {
		test.Fatalf("agent comment outcomes = %+v, want one blocked intake_hold for %s", outcomes, f.agentID)
	}
	if got := tasksOn(test, issueID); got != before {
		test.Fatalf("the refused mention wrote %d task row(s)", got-before)
	}

	// The same words from a person start a run.
	outcomes = postCommentForTriggerPolicyTest(test, issueID, mention)
	if len(outcomes) != 1 || outcomes[0].Status != DispatchQueued {
		test.Fatalf("member comment outcomes = %+v, want one queued", outcomes)
	}
}

// heldAutopilot is a create_issue autopilot on projectID with a schedule trigger.
func heldAutopilot(test *testing.T, f triageRunFixture, projectID, label string) (db.Autopilot, pgtype.UUID, string) {
	test.Helper()
	title := fmt.Sprintf("held autopilot %s %d", label, time.Now().UnixNano())
	var created AutopilotResponse
	testutil.Call(test, testHandler.CreateAutopilot, newRequest("POST", "/api/autopilots?workspace_id="+testWorkspaceID, map[string]any{
		"title": title, "assignee_id": f.agentID, "execution_mode": "create_issue", "issue_title_template": title, "project_id": projectID,
	})).Want(http.StatusCreated).JSON(&created)
	test.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM autopilot_run WHERE autopilot_id = $1`, created.ID)
		testPool.Exec(context.Background(), `DELETE FROM autopilot WHERE id = $1`, created.ID)
	})
	autopilot, err := testHandler.Queries.GetAutopilot(context.Background(), parseUUID(created.ID))
	if err != nil {
		test.Fatal(err)
	}
	trigger := dbfx.Insert(test, "autopilot_trigger", testutil.Cols{
		"autopilot_id": created.ID, "kind": "schedule", "cron_expression": "0 * * * *", "created_by_type": "member", "created_by_id": testUserID,
	})
	return autopilot, parseUUID(trigger), title
}

// An autopilot that creates its issue in backlog starts a derived run on it. On
// a project the hold owns the run is refused and the issue stays in backlog.
func TestIntakeHoldAutopilotRuns(test *testing.T) {
	seedTestCatalog(test)
	f := newTriageRunFixture(test)
	hold := newHoldFixture(test, nil)
	workflow := createTestWorkflow(test, "Held start "+workflowTestSuffix())
	patchWorkflow(test, workflow.ID, map[string]any{"start_status_key": "backlog"}).Want(http.StatusOK)
	dbfx.Exec(test, `UPDATE project SET workflow_id = $1 WHERE id = $2`, workflow.ID, hold.projectID)
	freeProject := createWorkflowTestProject(test, "no hold "+workflowTestSuffix())
	dbfx.Exec(test, `UPDATE project SET workflow_id = $1 WHERE id = $2`, workflow.ID, freeProject)
	ctx := context.Background()

	created := func(test *testing.T, title string) (issueID, status string, runs int) {
		test.Helper()
		if err := testPool.QueryRow(ctx, `SELECT id::text, status FROM issue WHERE workspace_id = $1 AND title = $2`, testWorkspaceID, title).Scan(&issueID, &status); err != nil {
			test.Fatalf("the autopilot created no issue titled %q: %v", title, err)
		}
		test.Cleanup(func() {
			testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
			deleteTestIssue(test, issueID)
		})
		return issueID, status, tasksOn(test, issueID)
	}
	reasonOf := func(run *db.AutopilotRun, err error) string {
		text := ""
		if err != nil {
			text = err.Error()
		}
		if run != nil && run.FailureReason.Valid {
			text += " " + run.FailureReason.String
		}
		return text
	}

	test.Run("control: a project with no hold starts the run", func(test *testing.T) {
		autopilot, trigger, title := heldAutopilot(test, f, freeProject, "control")
		if _, err := testHandler.AutopilotService.DispatchAutopilot(ctx, autopilot, trigger, "schedule", nil); err != nil {
			test.Fatalf("dispatch = %v", err)
		}
		if _, status, runs := created(test, title); status != "backlog" || runs != 1 {
			test.Fatalf("control issue = status %q with %d run(s), want backlog with 1", status, runs)
		}
	})
	test.Run("schedule: refused, the issue waits in backlog", func(test *testing.T) {
		autopilot, trigger, title := heldAutopilot(test, f, hold.projectID, "schedule")
		run, err := testHandler.AutopilotService.DispatchAutopilot(ctx, autopilot, trigger, "schedule", nil)
		if got := reasonOf(run, err); !strings.Contains(got, "intake_hold") {
			test.Fatalf("dispatch outcome = %q, want intake_hold", got)
		}
		if _, status, runs := created(test, title); status != "backlog" || runs != 0 {
			test.Fatalf("held issue = status %q with %d run(s), want backlog with 0", status, runs)
		}
	})
	test.Run("manual run now: refused too", func(test *testing.T) {
		autopilot, _, title := heldAutopilot(test, f, hold.projectID, "manual")
		run, _, err := testHandler.AutopilotService.DispatchAutopilotManual(ctx, autopilot, pgtype.UUID{}, nil, parseUUID(testUserID))
		if got := reasonOf(run, err); !strings.Contains(got, "intake_hold") {
			test.Fatalf("dispatch outcome = %q, want intake_hold", got)
		}
		if _, _, runs := created(test, title); runs != 0 {
			test.Fatalf("held issue started %d run(s)", runs)
		}
	})
}

// No caller reports internal_error for ErrIntakeHold (acceptance 10): one table
// over the mapping functions that type ErrIssueInTriage.
func TestIntakeHoldIsTypedByEveryMapper(test *testing.T) {
	wrapped := fmt.Errorf("enqueue: %w", service.ErrIntakeHold)
	for _, err := range []error{service.ErrIntakeHold, wrapped} {
		if got := commentEnqueueFailureReason(err); got != ReasonIntakeHold {
			test.Errorf("commentEnqueueFailureReason = %q, want intake_hold", got)
		}
		if reason, ok := routeRefusal(err); !ok || reason != "intake_hold" {
			test.Errorf("routeRefusal = (%q, %v), want intake_hold", reason, ok)
		}
		if !handoffRunRefused(err) {
			test.Error("handoffRunRefused does not type ErrIntakeHold")
		}
		if !isRunRefusal(err) {
			test.Error("isRunRefusal does not type ErrIntakeHold")
		}
	}
	if ReasonIntakeHold != "intake_hold" || dispatch.ReasonIntakeHold != "intake_hold" {
		test.Fatalf("reason code = %q", dispatch.ReasonIntakeHold)
	}
	if dispatchBlockedFallbackMessage(ReasonIntakeHold) == dispatchBlockedFallbackMessage(ReasonInternalError) {
		test.Error("intake_hold has no text of its own")
	}
}

// A manual rerun of an earlier run is refused with the reason (acceptance 9).
func TestIntakeHoldRefusesRerunWithAReason(test *testing.T) {
	f := newTriageRunFixture(test)
	hold := newHoldFixture(test, nil)
	issueID, _ := heldRunIssue(test, f, hold, "feat: rerun")
	resp := testutil.Call(test, testHandler.RerunIssue, withURLParam(
		newRequest(http.MethodPost, "/api/issues/"+issueID+"/rerun", nil), "id", issueID))
	if got := resp.Want(http.StatusConflict).Map()["reason_code"]; got != string(dispatch.ReasonIntakeHold) {
		test.Fatalf("reason_code = %v, want intake_hold: %s", got, resp.Text())
	}
	if n := tasksOn(test, issueID); n != 0 {
		test.Fatalf("refused rerun wrote %d task row(s)", n)
	}
}
