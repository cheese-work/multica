package service

import (
	"context"
	"encoding/json"
	"testing"

	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func leaderFailureFixture(t *testing.T) (*delegatedFailureFixture, *TaskService, *dbfx.Fixture) {
	t.Helper()
	fixture, svc := seedDelegatedFailureFixture(t)
	rows := dbfx.New(fixture.pool, fixture.workspaceID, fixture.userID)
	squadID := rows.Insert(t, "squad", dbfx.Cols{
		"workspace_id": fixture.workspaceID, "name": "failure-marker-squad",
		"leader_id": fixture.coordinator, "creator_id": fixture.userID,
	})
	rows.Exec(t, `UPDATE issue SET assignee_type = 'squad', assignee_id = $2,
		metadata = '{"keep":"value"}' WHERE id = $1`, fixture.issueID, squadID)
	rows.Exec(t, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, fixture.sourceTask)
	return fixture, svc, rows
}

func assertLeaderFailureMarker(t *testing.T, svc *TaskService, issueID string, want bool) db.Issue {
	t.Helper()
	issue, err := svc.Queries.GetIssue(context.Background(), util.MustParseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(issue.Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	if got := metadata["squad_leader_failed"] == true; got != want {
		t.Fatalf("squad_leader_failed = %v; want %v; metadata = %s", got, want, issue.Metadata)
	}
	if metadata["keep"] != "value" {
		t.Fatalf("unrelated metadata changed: %s", issue.Metadata)
	}
	return issue
}

func TestSquadLeaderFailureMarkerSetsWithoutDispatchInAnyStatus(t *testing.T) {
	for _, status := range []string{"todo", "in_progress", "in_review", "blocked", "done", "cancelled", "custom_status"} {
		t.Run(status, func(t *testing.T) {
			fixture, svc, rows := leaderFailureFixture(t)
			rows.Exec(t, `UPDATE issue SET status = $2 WHERE id = $1`, fixture.issueID, status)
			before := assertLeaderFailureMarker(t, svc, fixture.issueID, false)
			countBefore := rows.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, fixture.issueID)
			if _, err := svc.FailTask(context.Background(), util.MustParseUUID(fixture.sourceTask), "", "", "", "", "agent_error.process_failure", false, "", ""); err != nil {
				t.Fatal(err)
			}
			after := assertLeaderFailureMarker(t, svc, fixture.issueID, true)
			if after.Status != status || after.Revision != before.Revision+1 {
				t.Fatalf("marker changed status or missed revision: before=%+v after=%+v", before, after)
			}
			countAfter := rows.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, fixture.issueID)
			if countAfter != countBefore {
				t.Fatalf("marker dispatched a run: before=%d after=%d", countBefore, countAfter)
			}
			if _, err := svc.FailTask(context.Background(), util.MustParseUUID(fixture.sourceTask), "", "", "", "", "agent_error.process_failure", false, "", ""); err != nil {
				t.Fatal(err)
			}
			if replay := assertLeaderFailureMarker(t, svc, fixture.issueID, true); replay.Revision != after.Revision {
				t.Fatal("failure replay changed the marker revision")
			}
		})
	}
}

func TestSquadLeaderFailureMarkerRequiresLeaderAndNoActiveRun(t *testing.T) {
	for _, condition := range []string{"worker", "agent_assignee", "queued", "dispatched", "running", "waiting_local_directory", "retry"} {
		t.Run(condition, func(t *testing.T) {
			fixture, svc, rows := leaderFailureFixture(t)
			reason := "agent_error.process_failure"
			switch condition {
			case "worker":
				rows.Exec(t, `UPDATE agent_task_queue SET agent_id = $2 WHERE id = $1`, fixture.sourceTask, fixture.worker)
			case "agent_assignee":
				rows.Exec(t, `UPDATE issue SET assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, fixture.issueID, fixture.coordinator)
			case "retry":
				reason = "agent_error.provider_network"
			default:
				rows.Insert(t, "agent_task_queue", dbfx.Cols{
					"agent_id": fixture.worker, "runtime_id": fixture.runtimeID,
					"issue_id": fixture.issueID, "status": condition,
				})
			}
			if _, err := svc.FailTask(context.Background(), util.MustParseUUID(fixture.sourceTask), "process failed", "", "", "", reason, false, "", ""); err != nil {
				t.Fatal(err)
			}
			assertLeaderFailureMarker(t, svc, fixture.issueID, false)
		})
	}
}

func TestSquadLeaderFailureMarkerDeferredRunIsNotActive(t *testing.T) {
	fixture, svc, rows := leaderFailureFixture(t)
	rows.Insert(t, "agent_task_queue", dbfx.Cols{
		"agent_id": fixture.worker, "runtime_id": fixture.runtimeID,
		"issue_id": fixture.issueID, "status": "deferred",
	})
	if _, err := svc.FailTask(context.Background(), util.MustParseUUID(fixture.sourceTask), "", "", "", "", "agent_error.process_failure", false, "", ""); err != nil {
		t.Fatal(err)
	}
	assertLeaderFailureMarker(t, svc, fixture.issueID, true)
}

func TestSquadLeaderFailureMarkerClearsOnAnyLaterStart(t *testing.T) {
	for _, guarded := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "claimed"}[guarded], func(t *testing.T) {
			fixture, svc, rows := leaderFailureFixture(t)
			if _, err := svc.FailTask(context.Background(), util.MustParseUUID(fixture.sourceTask), "process failed", "", "", "", "agent_error.process_failure", false, "", ""); err != nil {
				t.Fatal(err)
			}
			assertLeaderFailureMarker(t, svc, fixture.issueID, true)
			successorID := rows.Insert(t, "agent_task_queue", dbfx.Cols{
				"agent_id": fixture.worker, "runtime_id": fixture.runtimeID,
				"issue_id": fixture.issueID, "status": "dispatched", "dispatched_at": dbfx.Raw("now()"),
			})
			assertLeaderFailureMarker(t, svc, fixture.issueID, true)
			successor, err := svc.Queries.GetAgentTask(context.Background(), util.MustParseUUID(successorID))
			if err != nil {
				t.Fatal(err)
			}
			if guarded {
				_, err = svc.StartTaskForClaim(context.Background(), db.LockAgentTaskStartClaimParams{
					ID: successor.ID, RuntimeID: successor.RuntimeID, DispatchedAt: successor.DispatchedAt,
				})
			} else {
				_, err = svc.StartTask(context.Background(), successor.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			cleared := assertLeaderFailureMarker(t, svc, fixture.issueID, false)
			if guarded {
				if _, err := svc.StartTaskForClaim(context.Background(), db.LockAgentTaskStartClaimParams{
					ID: successor.ID, RuntimeID: successor.RuntimeID, DispatchedAt: successor.DispatchedAt,
				}); err != nil {
					t.Fatal(err)
				}
				if replay := assertLeaderFailureMarker(t, svc, fixture.issueID, false); replay.Revision != cleared.Revision {
					t.Fatal("start replay changed the marker revision")
				}
			}
			rows.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, successorID)
			failed, err := svc.Queries.GetAgentTask(context.Background(), util.MustParseUUID(fixture.sourceTask))
			if err != nil {
				t.Fatal(err)
			}
			if err := svc.runInTx(context.Background(), func(queries *db.Queries) error {
				return SettleTerminalTaskState(context.Background(), queries, failed)
			}); err != nil {
				t.Fatal(err)
			}
			assertLeaderFailureMarker(t, svc, fixture.issueID, false)
		})
	}
}

func TestSquadLeaderFailureMarkerCoversBulkOrphanFailure(t *testing.T) {
	fixture, svc, _ := leaderFailureFixture(t)
	failed, err := svc.RecoverOrphanedTasksForRuntime(context.Background(), util.MustParseUUID(fixture.runtimeID))
	if err != nil || len(failed) != 1 {
		t.Fatalf("bulk orphan failure = %v, %v", failed, err)
	}
	assertLeaderFailureMarker(t, svc, fixture.issueID, true)
}

func TestSquadLeaderFailureMarkerDoesNotCaptureWakeups(t *testing.T) {
	fixture, svc, issue, agent := wakeFixture(t)
	wakeup := wakeCreate(t, fixture, svc, issue, WakeupInput{
		AgentID: agent, Kind: "event", Mode: "continuous",
		EventTypes: []string{"issue.updated", "issue.metadata_changed"}, Instruction: "Read current state",
	})
	fixture.Exec(t, `UPDATE issue SET metadata = metadata || '{"squad_leader_failed":true}' WHERE id = $1`, issue)
	fixture.Exec(t, `UPDATE issue SET metadata = metadata - 'squad_leader_failed' WHERE id = $1`, issue)
	if count := fixture.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id = $1`, wakeup.ID); count != 0 {
		t.Fatalf("marker-only changes captured %d wakeup receipts; want 0", count)
	}
	fixture.Exec(t, `UPDATE issue SET metadata = metadata || '{"squad_leader_failed":true,"ordinary":"changed"}' WHERE id = $1`, issue)
	if count := fixture.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id = $1`, wakeup.ID); count != 2 {
		t.Fatalf("ordinary metadata captured %d wakeup receipts; want 2", count)
	}
	if count := fixture.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id = $1
		AND payload->'changed_fields' ? 'squad_leader_failed'`, wakeup.ID); count != 0 {
		t.Fatal("marker leaked into ordinary metadata changed_fields")
	}
}
