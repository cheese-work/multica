package service

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
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
