package service

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
)

// A comment a budget recovery run wrote must not start a run through an event
// wakeup, however late the scheduler delivers it: the tick has no request, so the
// recovery mark has to travel with the comment (CHE-2297 L3).
func TestCommentWakeupDropsACommentFromARecoveryRun(t *testing.T) {
	for _, recovery := range []bool{true, false} {
		f, s, issue, agent := wakeFixture(t)
		issueID := util.UUIDToString(issue)
		var source string
		kind := "NULL"
		if recovery {
			kind = "'budget_recovery'"
		}
		if err := f.Pool.QueryRow(t.Context(), `INSERT INTO agent_task_queue(agent_id, runtime_id, issue_id, status, priority, started_at, task_kind)
			SELECT id, runtime_id, $2, 'running', 0, now(), `+kind+` FROM agent WHERE id = $1 RETURNING id`, agent, issueID).Scan(&source); err != nil {
			t.Fatal(err)
		}
		f.Cleanup(t, "DELETE FROM comment WHERE issue_id=$1", issueID)
		w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", EventTypes: []string{"comment.created"}, Instruction: "read new comments"})
		f.Exec(t, `INSERT INTO comment(issue_id,workspace_id,author_type,author_id,content,type,source_task_id) VALUES($1,$2,'member',$3,'recorded why','comment',$4)`, issueID, f.WorkspaceID, f.UserID, source)
		wakeDispatch(t, s, w)
		var queued int
		if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM agent_task_queue WHERE context->>'wakeup_id' = $1 AND status = 'queued'`, util.UUIDToString(w.ID)).Scan(&queued); err != nil {
			t.Fatal(err)
		}
		if want := map[bool]int{true: 0, false: 1}[recovery]; queued != want {
			t.Fatalf("recovery=%v: %d wakeup runs queued, want %d", recovery, queued, want)
		}
	}
}
