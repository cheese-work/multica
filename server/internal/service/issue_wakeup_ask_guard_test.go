package service

import (
	"testing"

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
