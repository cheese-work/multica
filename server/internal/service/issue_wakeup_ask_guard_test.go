package service

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/askguard"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestIssueWakeupAgentOnlyFilterMatchesAnyAgent(t *testing.T) {
	f, s, issue, agent := wakeFixture(t)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", EventTypes: []string{"comment.created"}, FilterActorType: "agent", Instruction: "react to agents"})
	if w.FilterActorType.String != "agent" || w.FilterActorID.Valid {
		t.Fatalf("filter not stored as type only: %+v", w)
	}
	receipts := func() int {
		return f.Count(t, "SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1", w.ID)
	}
	f.Exec(t, `INSERT INTO comment(issue_id,workspace_id,author_type,author_id,content,type) VALUES($1,$2,'member',$3,'member reply','comment')`, issue, f.WorkspaceID, f.UserID)
	if n := receipts(); n != 0 {
		t.Fatalf("a member comment fired the agent-only rule: %d receipts", n)
	}
	f.Exec(t, `INSERT INTO comment(issue_id,workspace_id,author_type,author_id,content,type) VALUES($1,$2,'agent',$3,'agent reply','comment')`, issue, f.WorkspaceID, agent)
	if n := receipts(); n != 1 {
		t.Fatalf("an agent comment receipts = %d, want 1", n)
	}
}

func TestIssueWakeupObserveFireAuditsMemberCause(t *testing.T) {
	f, s, issue, agent := wakeFixture(t)
	f.Exec(t, `UPDATE workspace SET settings = '{"ask_guard_mode":"block"}' WHERE id = $1`, f.WorkspaceID)
	f.Cleanup(t, "DELETE FROM ask_guard_event WHERE issue_id=$1", issue)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", EventTypes: []string{"comment.created"}, Purpose: "observe", Instruction: "observe replies"})
	f.Exec(t, `INSERT INTO comment(issue_id,workspace_id,author_type,author_id,content,type) VALUES($1,$2,'member',$3,'reply','comment')`, issue, f.WorkspaceID, f.UserID)
	wakeDispatch(t, s, w)
	if n := f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE context->>'wakeup_id'=$1", util.UUIDToString(w.ID)); n != 1 {
		t.Fatalf("member comment did not fire: %d runs", n)
	}
	// Audit only: a fire is never refused, so the event is a warn even in block mode.
	if n := f.Count(t, "SELECT count(*) FROM ask_guard_event WHERE issue_id=$1 AND operation_kind='wakeup_observe_fire' AND mode='warn'", issue); n != 1 {
		t.Fatalf("member-caused fire audits = %d, want 1", n)
	}
}

// A member's event stays the cause of the fire when an agent's event
// coalesces into the same receipt afterwards.
func TestIssueWakeupObserveFireAuditsCoalescedMemberCause(t *testing.T) {
	f, s, issue, agent := wakeFixture(t)
	f.Exec(t, `UPDATE workspace SET settings = '{"ask_guard_mode":"block"}' WHERE id = $1`, f.WorkspaceID)
	other := f.privateAgentOwnedBy(t, f.UserID, "other")
	f.Cleanup(t, "DELETE FROM ask_guard_event WHERE issue_id=$1", issue)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", EventTypes: []string{"comment.created"}, Purpose: "observe", Instruction: "observe replies"})
	f.Exec(t, `INSERT INTO comment(issue_id,workspace_id,author_type,author_id,content,type) VALUES($1,$2,'member',$3,'reply','comment')`, issue, f.WorkspaceID, f.UserID)
	f.Exec(t, `INSERT INTO comment(issue_id,workspace_id,author_type,author_id,content,type) VALUES($1,$2,'agent',$3,'later','comment')`, issue, f.WorkspaceID, other)
	wakeDispatch(t, s, w)
	if n := f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE context->>'wakeup_id'=$1", util.UUIDToString(w.ID)); n != 1 {
		t.Fatalf("coalesced comments did not fire: %d runs", n)
	}
	if n := f.Count(t, "SELECT count(*) FROM ask_guard_event WHERE issue_id=$1 AND operation_kind='wakeup_observe_fire'", issue); n != 1 {
		t.Fatalf("coalesced member-caused fire audits = %d, want 1", n)
	}
}

// An input a started run took along counts as a fire of the rule too.
func TestIssueWakeupObserveFireAuditsMergedRun(t *testing.T) {
	f, s, issue, agent := wakeFixture(t)
	f.Exec(t, `UPDATE workspace SET settings = '{"ask_guard_mode":"block"}' WHERE id = $1`, f.WorkspaceID)
	f.Cleanup(t, "DELETE FROM ask_guard_event WHERE issue_id=$1", issue)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", EventTypes: []string{"comment.created"}, Purpose: "observe", Instruction: "observe replies"})
	f.Exec(t, `INSERT INTO comment(issue_id,workspace_id,author_type,author_id,content,type) VALUES($1,$2,'member',$3,'reply','comment')`, issue, f.WorkspaceID, f.UserID)
	run := wakeWaitingRun(t, f, issue, agent, f.UserID)
	f.Exec(t, "UPDATE agent_task_queue SET status='running', started_at=now() WHERE id=$1", run)
	f.Exec(t, "UPDATE issue_wakeup_receipt SET task_id=$2 WHERE wakeup_id=$1", w.ID, run)
	wakeDispatch(t, s, w)
	if n := f.Count(t, "SELECT count(*) FROM ask_guard_event WHERE issue_id=$1 AND operation_kind='wakeup_observe_fire'", issue); n != 1 {
		t.Fatalf("merged-run fire audits = %d, want 1", n)
	}
}

func TestWakeupOperationPayloadIgnoresEventOrder(t *testing.T) {
	a := WakeupInput{Kind: "event", EventTypes: []string{"comment.created", "reaction.added"}}
	b := WakeupInput{Kind: "event", EventTypes: []string{"reaction.added", "comment.created"}}
	if string(wakeupOperationPayload(db.Issue{}, a)) != string(wakeupOperationPayload(db.Issue{}, b)) {
		t.Error("payload depends on event order")
	}
	if a.EventTypes[0] != "comment.created" {
		t.Error("payload reordered the caller's slice")
	}
}

// lockProbeDB runs probe before each single-row read, which is where the guard
// starts: a guard that runs inside save's transaction fails the probe.
type lockProbeDB struct {
	db.DBTX
	probe func()
}

func (d lockProbeDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	d.probe()
	return d.DBTX.QueryRow(ctx, sql, args...)
}

// The guard commits its own event on a second connection, so it must run
// before save takes the issue row lock, never while it is held.
func TestIssueWakeupAskGuardRunsOutsideIssueLock(t *testing.T) {
	f, s, issue, agent := wakeFixture(t)
	f.Exec(t, `UPDATE workspace SET settings = '{"ask_guard_mode":"block"}' WHERE id = $1`, f.WorkspaceID)
	f.Cleanup(t, "DELETE FROM ask_guard_event WHERE issue_id=$1", issue)
	run := wakeWaitingRun(t, f, issue, agent, f.UserID)
	var probes int
	var lockErr error
	probe := func() {
		probes++
		tx, err := f.Pool.Begin(context.Background())
		if err != nil {
			lockErr = err
			return
		}
		defer tx.Rollback(context.Background())
		_, lockErr = tx.Exec(context.Background(), "SELECT 1 FROM issue WHERE id=$1 FOR UPDATE NOWAIT", issue)
	}
	s.AskGuard = &WakeupAskGuard{Guard: askguard.Guard{Queries: db.New(lockProbeDB{DBTX: f.Pool, probe: probe})}, TaskID: parseTestUUID(t, run)}
	_, err := s.Create(context.Background(), issue, parseTestUUID(t, f.UserID), pgtype.UUID{}, WakeupInput{AgentID: agent, Kind: "event", EventTypes: []string{"comment.created"}, Instruction: "wait for the member"})
	if !errors.Is(err, ErrWakeupAskGuardBlocked) {
		t.Fatalf("err = %v, want ErrWakeupAskGuardBlocked", err)
	}
	if probes == 0 {
		t.Fatal("the guard never read the database")
	}
	if lockErr != nil {
		t.Fatalf("issue row was locked while the guard ran: %v", lockErr)
	}
}
