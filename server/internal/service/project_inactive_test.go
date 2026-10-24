package service

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestInactiveProjectResumeClearsWakeupError(test *testing.T) {
	fixture, wakeups, issue, agent := conditionFixture(test)
	projectID := fixture.Project(test, "Resume wakeup error", testutil.Cols{"status": "in_progress"})
	fixture.Exec(test, "UPDATE issue SET project_id=$2 WHERE id=$1", issue, projectID)
	rule := wakeCreate(test, fixture, wakeups, issue, WakeupInput{AgentID: agent, Kind: "event", EventTypes: []string{"comment.created"}, Instruction: "Fresh events only"})
	for _, status := range []string{"paused", "in_progress"} {
		_, err := wakeups.Tasks.UpdateProject(context.Background(), util.MustParseUUID(fixture.WorkspaceID), db.UpdateProjectParams{
			ID: util.MustParseUUID(projectID), Status: pgtype.Text{String: status, Valid: true},
		}, false)
		if err != nil {
			test.Fatal(err)
		}
		stored, err := fixture.q.LocklessWakeup(context.Background(), rule.ID)
		if err != nil {
			test.Fatal(err)
		}
		if status == "paused" && stored.LastError.String != "project_inactive" {
			test.Errorf("inactive wakeup error = %q", stored.LastError.String)
		}
		if status == "in_progress" && stored.LastError.Valid {
			test.Errorf("resumed wakeup retains error %q", stored.LastError.String)
		}
	}
}

func TestProjectAdmissionHoldsWorkspaceFence(test *testing.T) {
	fixture, _ := newPrincipalFixture(test)
	projectID := fixture.Project(test, "Project ownership fence", testutil.Cols{"status": "in_progress"})
	ctx := context.Background()
	admission, err := fixture.svc.TxStarter.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer admission.Rollback(ctx)
	if err := RequireActiveProject(ctx, fixture.q.WithTx(admission), util.MustParseUUID(fixture.WorkspaceID), util.MustParseUUID(projectID)); err != nil {
		test.Fatal(err)
	}
	teardown, err := fixture.svc.TxStarter.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer teardown.Rollback(ctx)
	_, err = teardown.Exec(ctx, "SELECT id FROM workspace WHERE id=$1 FOR UPDATE NOWAIT", fixture.WorkspaceID)
	var databaseError *pgconn.PgError
	if !errors.As(err, &databaseError) || databaseError.Code != "55P03" {
		test.Fatalf("project admission did not fence workspace teardown: %v", err)
	}
}

func TestInactiveProjectWakeupKinds(test *testing.T) {
	for _, status := range []string{"paused", "cancelled", "completed"} {
		for _, kind := range []string{"event", "at", "every", "cron", "condition", "manual"} {
			test.Run(status+"/"+kind, func(test *testing.T) {
				fixture, wakeups, issue, agent := conditionFixture(test)
				projectID := fixture.Project(test, "Inactive wakeup", testutil.Cols{"status": "in_progress"})
				fixture.Exec(test, "UPDATE issue SET project_id=$2 WHERE id=$1", issue, projectID)
				input := WakeupInput{AgentID: agent, Kind: kind, Instruction: "Only work in active projects"}
				switch kind {
				case "event", "manual":
					input.Kind = "event"
					input.EventTypes = []string{"comment.created"}
				case "condition":
					input.Kind = "event"
					input.Condition = condition(test, map[string]any{"type": "issue_field", "field": "status", "value": "todo"})
				case "at":
					input.AfterSeconds = 60
				case "every":
					input.IntervalSeconds = 600
				case "cron":
					input.CronExpression = "* * * * *"
					input.Timezone = "Asia/Bangkok"
				}
				w := wakeCreate(test, fixture, wakeups, issue, input)
				fixture.Exec(test, "UPDATE project SET status=$2 WHERE id=$1", projectID, status)
				fixture.Comment(test, util.UUIDToString(issue), "wake while inactive")
				if kind == "manual" {
					fixture.Exec(test, "UPDATE issue_wakeup_receipt SET event_type='wakeup.manual' WHERE wakeup_id=$1", w.ID)
				}
				got := wakeTick(test, fixture, wakeups, w.ID)
				if count := wakeRuns(test, fixture, w.ID); count != 0 {
					test.Errorf("%s wakeup started %d runs in a %s project", kind, count, status)
				}
				if got.LastError.String != "project_inactive" {
					test.Errorf("last_error = %q, want project_inactive", got.LastError.String)
				}
				if count := fixture.Count(test, "SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1 AND processed_at IS NULL", w.ID); count != 0 {
					test.Errorf("inactive project retained %d queued inputs", count)
				}
				fixture.Exec(test, "UPDATE project SET status='in_progress' WHERE id=$1", projectID)
				fresh, err := fixture.q.LocklessWakeup(context.Background(), w.ID)
				if err != nil {
					test.Fatal(err)
				}
				wakeDispatch(test, wakeups, fresh)
				if count := wakeRuns(test, fixture, w.ID); count != 0 {
					test.Errorf("resume replayed %d inactive-project wakeup runs", count)
				}
			})
		}
	}
}

func TestInactiveProjectAutopilotModes(test *testing.T) {
	for _, mode := range []string{"create_issue", "run_only"} {
		test.Run(mode, func(test *testing.T) {
			fixture, owner := newPrincipalFixture(test)
			agent := fixture.privateAgentOwnedBy(test, owner, "inactive autopilot")
			projectID := fixture.Project(test, "Inactive autopilot", testutil.Cols{"status": "paused"})
			apID, triggerID := fixture.autopilotWithTrigger(test, agent, owner, owner)
			fixture.Exec(test, "UPDATE autopilot SET execution_mode=$2, project_id=$3 WHERE id=$1", apID, mode, projectID)
			ap, err := fixture.q.GetAutopilot(context.Background(), util.MustParseUUID(apID))
			if err != nil {
				test.Fatal(err)
			}
			run, reason, err := fixture.svc.dispatchAutopilot(context.Background(), ap, util.MustParseUUID(triggerID), "schedule", nil, pgtype.Timestamptz{}, pgtype.UUID{}, pgtype.UUID{}, "")
			if err != nil {
				test.Fatal(err)
			}
			if string(reason) != "project_inactive" {
				test.Errorf("reason = %s, want project_inactive (run=%+v)", reason, run)
			}
			if count := fixture.Count(test, "SELECT count(*) FROM issue WHERE project_id=$1", projectID); count != 0 {
				test.Errorf("autopilot created %d issues in an inactive project", count)
			}
			if count := fixture.Count(test, "SELECT count(*) FROM agent_task_queue WHERE autopilot_run_id IN (SELECT id FROM autopilot_run WHERE autopilot_id=$1)", apID); count != 0 {
				test.Errorf("autopilot created %d runs in an inactive project", count)
			}
		})
	}
}
