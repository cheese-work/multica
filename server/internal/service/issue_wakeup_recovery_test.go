package service

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
)

// An event a budget recovery run caused must not start a run through an event
// wakeup, however late the scheduler delivers it: the tick has no request, so the
// exclusion happens when the event is captured (CHE-2297 L3).

func recoveryTask(t *testing.T, f principalFixture, issueID string, agent string, recovery bool) string {
	t.Helper()
	kind := "NULL"
	if recovery {
		kind = "'budget_recovery'"
	}
	var id string
	if err := f.Pool.QueryRow(t.Context(), `INSERT INTO agent_task_queue(agent_id, runtime_id, issue_id, status, priority, started_at, task_kind)
		SELECT id, runtime_id, $2, 'running', 0, now(), `+kind+` FROM agent WHERE id = $1 RETURNING id`, agent, issueID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func queuedWakeupRuns(t *testing.T, f principalFixture, wakeupID string) int {
	t.Helper()
	var n int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM agent_task_queue WHERE context->>'wakeup_id' = $1 AND status = 'queued'`, wakeupID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEventWakeupDropsEveryEventTypeFromARecoveryRun(t *testing.T) {
	for _, eventType := range []string{"comment.created", "comment.updated", "issue.status_changed", "issue.metadata_changed"} {
		for _, recovery := range []bool{true, false} {
			f, s, issue, agent := wakeFixture(t)
			issueID := util.UUIDToString(issue)
			source := recoveryTask(t, f, issueID, agent, recovery)
			f.Cleanup(t, "DELETE FROM comment WHERE issue_id=$1", issueID)
			w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", EventTypes: []string{eventType}, Instruction: "read the change"})
			if eventType == "comment.created" {
				f.Exec(t, `INSERT INTO comment(issue_id,workspace_id,author_type,author_id,content,type,source_task_id) VALUES($1,$2,'member',$3,'recorded why','comment',$4)`, issueID, f.WorkspaceID, f.UserID, source)
			} else {
				f.Exec(t, `SELECT capture_issue_wakeup($1,$2,$3,NULL,$4,'{}'::jsonb)`, issueID, eventType, "k-"+eventType, source)
			}
			wakeDispatch(t, s, w)
			if want, got := map[bool]int{true: 0, false: 1}[recovery], queuedWakeupRuns(t, f, util.UUIDToString(w.ID)); got != want {
				t.Errorf("%s recovery=%v: %d wakeup runs queued, want %d", eventType, recovery, got, want)
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
		source := recoveryTask(t, f, issueID, agent, true)
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
