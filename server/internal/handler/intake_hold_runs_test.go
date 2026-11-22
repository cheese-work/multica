package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

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
// derived run too; an answer that is not yet effective does not.
func TestIntakeHoldReleaseAdmitsDerivedRuns(test *testing.T) {
	f := newTriageRunFixture(test)
	hold := newHoldFixture(test, func(entry map[string]any) { entry["exempt_escalation_classes"] = []string{"spend"} })
	admitted := func(test *testing.T, issue db.Issue) error {
		test.Helper()
		_, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue)
		return err
	}
	test.Run("form a: a person moved it out of backlog, now back", func(test *testing.T) {
		issueID, issue := heldRunIssue(test, f, hold, "feat: release a")
		statusRow(test, issueID, "member", "ui_or_api", "interactive_jwt", "backlog", "todo", nil)
		if err := admitted(test, issue); err != nil {
			test.Fatalf("assignment run after a release = %v", err)
		}
	})
	test.Run("form b: release key, row older than the park decision", func(test *testing.T) {
		issueID, issue := heldRunIssue(test, f, hold, "feat: release b")
		statusRow(test, issueID, "member", "ui_or_api", "pat", "need_decision", "todo", map[string]any{"intake_release": "CHE-6911"})
		dbfx.Exec(test, `INSERT INTO issue_decision_park (issue_id, workspace_id, prior_status, to_status) VALUES ($1, $2, 'backlog', 'need_decision')`, issueID, testWorkspaceID)
		if err := admitted(test, issue); err != nil {
			test.Fatalf("assignment run after a form b release = %v", err)
		}
	})
	test.Run("escalation answer not yet effective refuses", func(test *testing.T) {
		issueID, issue := heldRunIssue(test, f, hold, "feat: answer later")
		escalationAnswer(test, issueID, "spend", "now() + interval '10 minutes'")
		if err := admitted(test, issue); !errors.Is(err, service.ErrIntakeHold) {
			test.Fatalf("run before wake_at = %v, want ErrIntakeHold", err)
		}
	})
	test.Run("escalation answer after wake_at admits", func(test *testing.T) {
		issueID, issue := heldRunIssue(test, f, hold, "feat: answer now")
		escalationAnswer(test, issueID, "spend", "now() - interval '1 second'")
		if err := admitted(test, issue); err != nil {
			test.Fatalf("run after wake_at = %v", err)
		}
	})
}

// A wakeup an agent created is derived and refused; the error text names the
// reason, so last_error shows intake_hold. The retry paths skip quietly.
func TestIntakeHoldRefusesWakeupAndRetry(test *testing.T) {
	f := newTriageRunFixture(test)
	hold := newHoldFixture(test, nil)
	ctx := context.Background()
	issueID, issue := heldRunIssue(test, f, hold, "feat: retry")

	if got := service.ErrIntakeHold.Error(); got[:len("intake_hold")] != "intake_hold" {
		test.Fatalf("ErrIntakeHold text %q does not name the reason", got)
	}
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
	_ = issue
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
