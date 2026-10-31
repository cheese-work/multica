package service

import (
	"reflect"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
)

// Comment-created wakeups carry the taint of the comments that triggered them
// (CHE-2216 plan 3.7, T9), whether they start a task or merge into a queued one,
// and even when receipt coalescing leaves only the newest comment in the payload.

func taintedAgentTask(t *testing.T, f principalFixture, issue, agent string) string {
	t.Helper()
	var id string
	if err := f.Pool.QueryRow(t.Context(), `INSERT INTO agent_task_queue(agent_id, runtime_id, issue_id, status, priority, started_at, taint_ids)
		SELECT id, runtime_id, $2, 'running', 0, now(), ARRAY['t1'] FROM agent WHERE id = $1 RETURNING id`, agent, issue).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func wakeTaint(t *testing.T, f principalFixture, wakeupID string) []string {
	t.Helper()
	var ids []string
	if err := f.Pool.QueryRow(t.Context(), `SELECT taint_ids FROM agent_task_queue WHERE context->>'wakeup_id' = $1 AND status = 'queued'`, wakeupID).Scan(&ids); err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestCommentWakeupInheritsEventCommentTaint(t *testing.T) {
	cases := []struct {
		name   string
		before bool // a clean comment starts the task first, so the tainted one merges
		after  bool // a clean comment follows, coalescing over the tainted receipt
	}{
		{"new task from a tainted comment", false, false},
		{"merge into a queued task", true, false},
		{"coalesced behind a later clean comment", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, s, issue, agent := wakeFixture(t)
			issueID := util.UUIDToString(issue)
			source := taintedAgentTask(t, f, issueID, agent)
			f.Cleanup(t, "DELETE FROM comment WHERE issue_id=$1", issueID)
			w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", EventTypes: []string{"comment.created"}, Instruction: "read new comments"})
			wid := util.UUIDToString(w.ID)
			if c.before {
				f.Comment(t, issueID, "clean first")
				wakeDispatch(t, s, w)
				if got := wakeTaint(t, f, wid); len(got) != 0 {
					t.Fatalf("task from a clean comment is tainted: %v", got)
				}
			}
			f.Exec(t, `INSERT INTO comment(issue_id,workspace_id,author_type,author_id,content,type,source_task_id) VALUES($1,$2,'member',$3,'from a tainted run','comment',$4)`, issueID, f.WorkspaceID, f.UserID, source)
			if c.after {
				f.Comment(t, issueID, "clean later")
			}
			wakeDispatch(t, s, w)
			if got := wakeTaint(t, f, wid); !reflect.DeepEqual(got, []string{"t1"}) {
				t.Fatalf("wakeup task taint = %v, want [t1]", got)
			}
		})
	}
}
