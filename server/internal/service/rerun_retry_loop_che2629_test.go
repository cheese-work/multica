package service

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Deterministic coverage for RerunIssue's bounded retry loop over the two
// unique indexes its single insert can hit (CHE-485, CHE-2629). The existing
// concurrency tests race real goroutines, so whether the loop runs at all
// depends on timing. Here testHookBeforeRerunEnqueue commits a competing row
// right before an insert attempt, so each index is hit on purpose.

type rerunRetryFixture struct {
	svc     *TaskService
	issueID pgtype.UUID
	agentID string
	actorID pgtype.UUID
	source  db.AgentTaskQueue
	exec    func(sql string, args ...any) string
}

func newRerunRetryFixture(t *testing.T) rerunRetryFixture {
	t.Helper()
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	workspaceID, actorID, agentID, issueID := seedAttributionFixture(t, pool)
	svc := &TaskService{Queries: db.New(pool), TxStarter: pool, Bus: events.New()}
	source, err := svc.EnqueueTaskForIssue(ctx, db.Issue{
		ID:           util.MustParseUUID(issueID),
		AssigneeID:   util.MustParseUUID(agentID),
		Priority:     "medium",
		CreatorType:  "member",
		CreatorID:    util.MustParseUUID(actorID),
		WorkspaceID:  util.MustParseUUID(workspaceID),
		AssigneeType: pgtype.Text{String: "agent", Valid: true},
	})
	if err != nil {
		t.Fatalf("EnqueueTaskForIssue (source): %v", err)
	}
	return rerunRetryFixture{
		svc:     svc,
		issueID: util.MustParseUUID(issueID),
		agentID: agentID,
		actorID: util.MustParseUUID(actorID),
		source:  source,
		exec: func(sql string, args ...any) string {
			t.Helper()
			var out string
			if err := pool.QueryRow(ctx, sql, args...).Scan(&out); err != nil {
				t.Fatalf("query %q: %v", sql, err)
			}
			return out
		},
	}
}

func (f rerunRetryFixture) rerun() (*db.AgentTaskQueue, error) {
	allow := func(db.Agent) bool { return true }
	return f.svc.RerunIssue(context.Background(), f.issueID, f.source.ID, pgtype.UUID{}, f.actorID, false, allow)
}

// insertTask commits a task row for the fixture's issue and agent, outside the
// rerun's own transaction, and returns its id.
func (f rerunRetryFixture) insertTask(status string, rerunOf pgtype.UUID) string {
	return f.exec(`
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority,
			originator_user_id, accountable_user_id, originator_source, rerun_of_task_id)
		VALUES ($1, (SELECT runtime_id FROM agent WHERE id = $1), $2, $3, 0, $4, $4, 'delegation', $5)
		RETURNING id::text`, f.agentID, f.issueID, status, f.actorID, rerunOf)
}

func (f rerunRetryFixture) status(taskID string) string {
	return f.exec(`SELECT status FROM agent_task_queue WHERE id = $1`, taskID)
}

// TestRerunIssueRetryLoopReclaimsRepeatedPendingSlotCollision forces the
// insert to hit idx_one_pending_task_per_issue_agent_thread on two attempts in
// a row: before attempts 1 and 2 another queued task takes the same (issue,
// agent, thread) slot. The loop must reclaim the slot each time and admit the
// rerun on attempt 3. A single extra attempt is not enough here.
func TestRerunIssueRetryLoopReclaimsRepeatedPendingSlotCollision(t *testing.T) {
	f := newRerunRetryFixture(t)
	var occupants []string
	f.svc.testHookBeforeRerunEnqueue = func() {
		if len(occupants) < 2 {
			occupants = append(occupants, f.insertTask("queued", pgtype.UUID{}))
		}
	}

	task, err := f.rerun()
	if err != nil {
		t.Fatalf("RerunIssue after %d pending-slot collisions: %v", len(occupants), err)
	}
	if len(occupants) != 2 {
		t.Fatalf("hook placed %d occupants, want 2", len(occupants))
	}
	if util.UUIDToString(task.RerunOfTaskID) != util.UUIDToString(f.source.ID) {
		t.Errorf("admitted task rerun_of_task_id = %s, want source %s",
			util.UUIDToString(task.RerunOfTaskID), util.UUIDToString(f.source.ID))
	}
	if got := f.status(util.UUIDToString(task.ID)); got != "queued" {
		t.Errorf("admitted rerun status = %q, want queued", got)
	}
	for i, id := range occupants {
		if got := f.status(id); got != "cancelled" {
			t.Errorf("occupant %d status = %q, want cancelled by the reclaim", i+1, got)
		}
	}
}

// TestRerunIssueRetryLoopReturnsLineageWinner forces the insert to hit
// idx_one_live_rerun_per_source_task_actor only: before attempt 1 a running
// rerun of the same source by the same actor commits. A running row holds no
// pending slot, so only the lineage index rejects the insert. RerunIssue must
// return that winner, not an error and not a second rerun.
func TestRerunIssueRetryLoopReturnsLineageWinner(t *testing.T) {
	f := newRerunRetryFixture(t)
	var winner string
	f.svc.testHookBeforeRerunEnqueue = func() {
		if winner == "" {
			winner = f.insertTask("running", f.source.ID)
		}
	}

	task, err := f.rerun()
	if err != nil {
		t.Fatalf("RerunIssue after a lineage collision: %v", err)
	}
	if got := util.UUIDToString(task.ID); got != winner {
		t.Errorf("RerunIssue returned %s, want the lineage winner %s", got, winner)
	}
	if got := f.status(winner); got != "running" {
		t.Errorf("winner status = %q, want running", got)
	}
	if n := f.exec(`SELECT count(*)::text FROM agent_task_queue WHERE rerun_of_task_id = $1`, f.source.ID); n != "1" {
		t.Errorf("reruns of the source = %s, want only the winner", n)
	}
}
