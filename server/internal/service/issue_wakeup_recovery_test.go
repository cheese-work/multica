package service

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// An event a budget recovery run caused must not start a run through an event
// wakeup, however late the scheduler delivers it: the tick has no request, so the
// exclusion happens when the event is captured (CHE-2297 L3).

// recoveryTask inserts a running task for a second agent, so its writes are not
// the wakeup agent's own (which an event wakeup ignores to avoid loops).
func recoveryTask(t *testing.T, f principalFixture, issueID string, wakeAgent string, recovery bool) (task, agent string) {
	t.Helper()
	var owner string
	if err := f.Pool.QueryRow(t.Context(), `SELECT owner_id FROM agent WHERE id=$1`, wakeAgent).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	agent = f.privateAgentOwnedBy(t, owner, "recovery")
	kind := "NULL"
	if recovery {
		kind = "'budget_recovery'"
	}
	var id string
	if err := f.Pool.QueryRow(t.Context(), `INSERT INTO agent_task_queue(agent_id, runtime_id, issue_id, status, priority, started_at, task_kind)
		SELECT id, runtime_id, $2, 'running', 0, now(), `+kind+` FROM agent WHERE id = $1 RETURNING id`, agent, issueID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id, agent
}

func queuedWakeupRuns(t *testing.T, f principalFixture, wakeupID string) int {
	t.Helper()
	var n int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM agent_task_queue WHERE context->>'wakeup_id' = $1 AND status = 'queued'`, wakeupID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Both delivery paths: a direct dispatch, and the scheduler job's pass, which
// runs on a fresh service and a fresh context with no request behind it.
func TestEventWakeupDropsEveryEventTypeFromARecoveryRun(t *testing.T) {
	deliveries := map[string]func(t *testing.T, f principalFixture, s *IssueWakeupService, w db.IssueWakeup){
		"dispatch": func(t *testing.T, _ principalFixture, s *IssueWakeupService, w db.IssueWakeup) { wakeDispatch(t, s, w) },
		"scheduler tick": func(t *testing.T, f principalFixture, _ *IssueWakeupService, w db.IssueWakeup) {
			fresh := &IssueWakeupService{Tasks: f.svc.TaskSvc}
			if err := fresh.TickWorkspaces(context.Background(), parseTestUUID(t, f.WorkspaceID)); err != nil {
				t.Fatal(err)
			}
		},
	}
	for delivery, deliver := range deliveries {
		for _, eventType := range []string{"comment.created", "comment.updated", "issue.status_changed", "issue.metadata_changed"} {
			for _, recovery := range []bool{true, false} {
				f, s, issue, agent := wakeFixture(t)
				issueID := util.UUIDToString(issue)
				source, actor := recoveryTask(t, f, issueID, agent, recovery)
				f.Cleanup(t, "DELETE FROM comment WHERE issue_id=$1", issueID)
				existing := f.Comment(t, issueID, "original")
				w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", EventTypes: []string{eventType}, Instruction: "read the change"})
				if eventType == "comment.created" {
					f.Exec(t, `INSERT INTO comment(issue_id,workspace_id,author_type,author_id,content,type,source_task_id) VALUES($1,$2,'member',$3,'recorded why','comment',$4)`, issueID, f.WorkspaceID, f.UserID, source)
				} else {
					// A production write: the handler carries the source task into the
					// transaction the same way (beginWakeupWrite).
					tx, err := f.Pool.Begin(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					if _, err := tx.Exec(t.Context(), `SELECT set_config('multica.source_task_id',$1,true),set_config('multica.actor_type','agent',true),set_config('multica.actor_id',$2,true)`, source, actor); err != nil {
						t.Fatal(err)
					}
					write := map[string]string{
						"comment.updated":        "UPDATE comment SET content=content||'!' WHERE id='" + existing + "'",
						"issue.status_changed":   "UPDATE issue SET status='in_progress',revision=revision+1 WHERE id='" + issueID + "'",
						"issue.metadata_changed": "UPDATE issue SET metadata=jsonb_build_object('changed',gen_random_uuid()),revision=revision+1 WHERE id='" + issueID + "'",
					}[eventType]
					if _, err := tx.Exec(t.Context(), write); err != nil {
						t.Fatal(err)
					}
					if err := tx.Commit(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				deliver(t, f, s, w)
				if want, got := map[bool]int{true: 0, false: 1}[recovery], queuedWakeupRuns(t, f, util.UUIDToString(w.ID)); got != want {
					t.Errorf("%s via %s, recovery=%v: %d wakeup runs queued, want %d", eventType, delivery, recovery, got, want)
				}
			}
		}
	}
}

// Capture coalesces comments into one receipt per event type, so excluding a
// recovery comment must not take a member's comment with it, in either order.
func TestRecoveryCommentNeverErasesAMembersPendingComment(t *testing.T) {
	for _, recoveryFirst := range []bool{true, false} {
		f, s, issue, agent := wakeFixture(t)
		issueID := util.UUIDToString(issue)
		source, _ := recoveryTask(t, f, issueID, agent, true)
		f.Cleanup(t, "DELETE FROM comment WHERE issue_id=$1", issueID)
		w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", EventTypes: []string{"comment.created"}, Instruction: "read new comments"})
		member := func() {
			f.Exec(t, `INSERT INTO comment(issue_id,workspace_id,author_type,author_id,content,type) VALUES($1,$2,'member',$3,'member says','comment')`, issueID, f.WorkspaceID, f.UserID)
		}
		recorded := func() {
			f.Exec(t, `INSERT INTO comment(issue_id,workspace_id,author_type,author_id,content,type,source_task_id) VALUES($1,$2,'member',$3,'recorded why','comment',$4)`, issueID, f.WorkspaceID, f.UserID, source)
		}
		if recoveryFirst {
			recorded()
			member()
		} else {
			member()
			recorded()
		}
		wakeDispatch(t, s, w)
		if got := queuedWakeupRuns(t, f, util.UUIDToString(w.ID)); got != 1 {
			t.Fatalf("recoveryFirst=%v: %d wakeup runs queued for the member's comment, want 1", recoveryFirst, got)
		}
	}
}
