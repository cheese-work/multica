package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// A report that cites a repo file is a refused plain comment (CHE-5753): no
// member, no question, one no-member block event.
const notifyClearReport = "Please read the result in `server/internal/handler/comment.go` before the next step."

func eventClearedByComment(t *testing.T, eventID string) (cleared bool, comment string) {
	t.Helper()
	var by *string
	dbfx.QueryRow(t, `SELECT cleared_at IS NOT NULL, cleared_by_comment_id::text FROM ask_guard_event WHERE id = $1`, eventID).Scan(&cleared, &by)
	if by != nil {
		comment = *by
	}
	return cleared, comment
}

func openBlockEventID(t *testing.T, f askGuardCommentFixture) string {
	t.Helper()
	var id string
	dbfx.QueryRow(t, `SELECT id::text FROM ask_guard_event WHERE issue_id = $1 AND mode = 'block' ORDER BY id LIMIT 1`, f.issueID).Scan(&id)
	return id
}

func commentID(t *testing.T, res *testutil.Response) string {
	t.Helper()
	var out struct {
		ID string `json:"id"`
	}
	res.JSON(&out)
	return out.ID
}

// Work item 1: the --notify-only post that follows the refusal clears the
// no-member event the refusal opened, and records the comment on the event.
func TestNotifyOnlyPostClearsNoMemberEvent(t *testing.T) {
	setAskGuardMode(t, "block")
	f := newAskGuardCommentFixture(t)
	f.post(t, map[string]any{"content": notifyClearReport}).Want(http.StatusUnprocessableEntity)
	event := openBlockEventID(t, f)
	if cleared, _ := eventClearedByComment(t, event); cleared {
		t.Fatal("the refusal itself must leave the event open")
	}

	posted := f.post(t, map[string]any{"content": notifyClearReport, "notify_only": true}).Want(http.StatusCreated)
	cleared, by := eventClearedByComment(t, event)
	if !cleared {
		t.Fatal("a --notify-only post must clear the no-member event its refused attempt opened")
	}
	if want := commentID(t, posted); by != want {
		t.Fatalf("cleared_by_comment_id = %q, want the posted comment %q", by, want)
	}
}

// Work item 2: a cleared event starts no recovery and no repair run.
func TestClearedNoMemberEventStartsNoRepair(t *testing.T) {
	setAskGuardMode(t, "block")
	f := newAskGuardCommentFixture(t)
	f.post(t, map[string]any{"content": notifyClearReport}).Want(http.StatusUnprocessableEntity)
	event := openBlockEventID(t, f)
	f.post(t, map[string]any{"content": notifyClearReport, "notify_only": true}).Want(http.StatusCreated)

	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, f.taskID)
	deliverRecoveries(t)
	admitRepairs(t)
	settleRepairs(t)

	if ids, _ := repairTasks(t, f.intentFixture); len(ids) != 0 {
		t.Fatalf("repair runs = %d, want 0", len(ids))
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM ask_repair_root WHERE id = $1`, event); n != 0 {
		t.Fatalf("repair roots = %d, want 0", n)
	}
	if got := systemComments(t, f.intentFixture); len(got) != 0 {
		t.Fatalf("system comments = %v, want none", got)
	}
}

// Work item 2, admitted root: the repair run answers with --notify-only. Its
// post clears the root's no-member event, so the root settles with no failure.
func TestNotifyOnlyFromRepairRunSettlesRoot(t *testing.T) {
	setAskGuardMode(t, "block")
	f := newAskGuardCommentFixture(t)
	event := repairEvent(t, f.intentFixture, "comment_ask", nil, notifyClearReport)
	admitRepairs(t)
	repairTask := onlyRepairTask(t, f.intentFixture)

	f.postAs(t, repairTask, map[string]any{"content": notifyClearReport, "notify_only": true}).Want(http.StatusCreated)
	if cleared, _ := eventClearedByComment(t, event); !cleared {
		t.Fatal("the admitted repair run's --notify-only post must clear the root's no-member event")
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, repairTask)
	settleRepairs(t)
	if state, reason := rootState(t, event); state != "settled" || reason != "" {
		t.Fatalf("root = %s/%s, want settled", state, reason)
	}
	if got := systemComments(t, f.intentFixture); len(got) != 0 {
		t.Fatalf("system comments = %v, want none", got)
	}
}

// A refused comment edit opens the same no-member event; the --notify-only
// post of the content clears it.
func TestNotifyOnlyPostClearsNoMemberEditEvent(t *testing.T) {
	setAskGuardMode(t, "block")
	f := newAskGuardCommentFixture(t)
	event := insertRepairEvent(t, f.intentFixture, f.taskID, "comment_edit", nil, notifyClearReport)
	f.post(t, map[string]any{"content": notifyClearReport, "notify_only": true}).Want(http.StatusCreated)
	if cleared, _ := eventClearedByComment(t, event); !cleared {
		t.Fatal("a --notify-only post must clear the no-member edit event of the same task")
	}
}

// Work item 3 and scope: only this task's no-member events on this issue clear.
func TestNotifyOnlyPostLeavesOtherEventsOpen(t *testing.T) {
	setAskGuardMode(t, "block")
	f := newAskGuardCommentFixture(t)
	withMember := insertRepairEvent(t, f.intentFixture, f.taskID, "comment", f.humanID, f.mention())
	otherTask := dbfx.Task(t, f.agentID, map[string]any{"issue_id": f.issueID, "status": "running", "runtime_id": handlerTestRuntimeID(t)})
	otherTaskEvent := insertRepairEvent(t, f.intentFixture, otherTask, "comment_ask", nil, notifyClearReport)
	waitingMove := insertRepairEvent(t, f.intentFixture, f.taskID, "waiting_move", nil, nil)
	twoMembers := insertRepairEvent(t, f.intentFixture, f.taskID, "comment", nil, f.mention()+" "+f.mention())

	f.post(t, map[string]any{"content": "plain report", "notify_only": true}).Want(http.StatusCreated)
	for name, id := range map[string]string{"event with a member": withMember, "other task's event": otherTaskEvent, "waiting move event": waitingMove, "event whose source names members": twoMembers} {
		if cleared, _ := eventClearedByComment(t, id); cleared {
			t.Errorf("%s was cleared by a --notify-only post", name)
		}
	}
}
