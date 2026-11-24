package handler

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Both handoff preflights (agent: EnqueueIssueRunInTx; squad leader:
// EnqueueSquadLeaderRunInTx) look at the slot the insert will take: the handoff
// comment's thread, else the NULL slot. A run in another slot never blocks the
// step run; a run in the effective slot is ErrDuplicatePendingTask with its id,
// never a unique-index violation that aborts the caller's transaction.

const slotHandoffBrief = "slot preflight step brief"

type slotFixture struct {
	issue    db.Issue
	agentID  string // the run target: the agent assignee, or the squad leader
	squad    bool
	threadT  string // root comment of the thread the handoff comment is in
	handoff  string // the status move's handoff comment, a reply in threadT
	otherCmt string // root comment of a different thread
}

func newSlotFixture(t *testing.T, squad bool) slotFixture {
	t.Helper()
	f := slotFixture{squad: squad}
	if squad {
		sq := newSquadCommentTriggerFixture(t)
		f.issue, f.agentID = sq.Issue, sq.LeaderID
		setIssueAssigneeDirect(t, util.UUIDToString(f.issue.ID), "squad", sq.SquadID)
	} else {
		f.agentID = createHandlerTestAgent(t, "slot preflight agent", nil)
		id := dbfx.Issue(t, "slot preflight issue", testutil.Cols{"status": "in_progress", "assignee_type": "agent", "assignee_id": f.agentID})
		t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, id) })
		issue, err := testHandler.Queries.GetIssue(context.Background(), util.MustParseUUID(id))
		if err != nil {
			t.Fatal(err)
		}
		f.issue = issue
	}
	issueID := util.UUIDToString(f.issue.ID)
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID) })
	issue, err := testHandler.Queries.GetIssue(context.Background(), f.issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.issue = issue
	f.threadT = dbfx.Comment(t, issueID, "thread T root")
	f.handoff = dbfx.Comment(t, issueID, slotHandoffBrief, testutil.Cols{"parent_id": f.threadT})
	f.otherCmt = dbfx.Comment(t, issueID, "another thread root")
	return f
}

// holder queues a pending run of the target in a slot: trigger is a comment id
// (its thread is the slot) or "" for the NULL slot.
func (f slotFixture) holder(t *testing.T, trigger string, over ...testutil.Cols) string {
	t.Helper()
	cols := testutil.Cols{"issue_id": util.UUIDToString(f.issue.ID), "runtime_id": handlerTestRuntimeID(t)}
	if trigger != "" {
		cols["trigger_comment_id"] = trigger
	}
	for _, o := range over {
		for k, v := range o {
			cols[k] = v
		}
	}
	return dbfx.Task(t, f.agentID, cols)
}

// enqueue runs the preflight under the status move's ctx (handoff comment set
// when withHandoff) in one transaction and reports the run id and the error.
// After the call it proves the transaction is not aborted, then commits.
func (f slotFixture) enqueue(t *testing.T, withHandoff bool, trigger string) (string, error) {
	t.Helper()
	ctx := context.Background()
	if withHandoff {
		ctx = service.WithStatusHandoffComment(ctx, util.MustParseUUID(f.handoff))
	}
	tx, err := testHandler.TxStarter.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	q := testHandler.Queries.WithTx(tx)
	var trig pgtype.UUID
	if trigger != "" {
		trig = util.MustParseUUID(trigger)
	}
	var (
		id    pgtype.UUID
		after func()
	)
	if f.squad {
		id, after, err = testHandler.onQueries(q).enqueueSquadLeaderRunAsInTx(ctx, q, f.issue, trig, "system", "")
	} else {
		id, after, err = testHandler.onQueries(q).TaskService.EnqueueIssueRunInTx(ctx, q, f.issue, trig, pgtype.UUID{}, nil)
	}
	var alive int
	if qerr := tx.QueryRow(ctx, `SELECT 1`).Scan(&alive); qerr != nil {
		t.Fatalf("the caller's transaction is aborted after the preflight (err=%v): %v", err, qerr)
	}
	if err != nil {
		return util.UUIDToString(id), err
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if after != nil {
		after()
	}
	return util.UUIDToString(id), nil
}

// claimedPrompt reads the inserted run as the daemon's claim returns it and
// renders the prompt the agent starts with.
func claimedPrompt(t *testing.T, runID string) (db.AgentTaskQueue, string) {
	t.Helper()
	task, err := testHandler.Queries.GetAgentTask(context.Background(), util.MustParseUUID(runID))
	if err != nil {
		t.Fatal(err)
	}
	resp := taskToResponse(task, testWorkspaceID)
	if task.TriggerCommentID.Valid && resp.TriggerCommentContent == "" {
		var content string
		if err := testPool.QueryRow(context.Background(), `SELECT content FROM comment WHERE id = $1`, task.TriggerCommentID).Scan(&content); err != nil {
			t.Fatal(err)
		}
		resp.TriggerCommentContent = content
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var claimed daemon.Task
	if err := json.Unmarshal(raw, &claimed); err != nil {
		t.Fatal(err)
	}
	return task, daemon.BuildPrompt(claimed, "claude")
}

func (f slotFixture) wantStepRun(t *testing.T, runID string, slot string) {
	t.Helper()
	task, prompt := claimedPrompt(t, runID)
	if util.UUIDToString(task.AgentID) != f.agentID {
		t.Errorf("run agent = %s, want %s", util.UUIDToString(task.AgentID), f.agentID)
	}
	if slot == "" && task.CommentThreadID.Valid {
		t.Errorf("run is in thread %s, want the NULL slot", util.UUIDToString(task.CommentThreadID))
	}
	if slot != "" && util.UUIDToString(task.CommentThreadID) != slot {
		t.Errorf("run thread = %s, want %s", util.UUIDToString(task.CommentThreadID), slot)
	}
	if slot != "" {
		if task.TriggerKind.String != service.TriggerKindStatusHandoff {
			t.Errorf("trigger_kind = %q, want status_handoff", task.TriggerKind.String)
		}
		if !strings.Contains(prompt, slotHandoffBrief) || !strings.Contains(prompt, "Handoff context") {
			t.Errorf("the step prompt lost the handoff brief:\n%s", prompt)
		}
	}
}

func (f slotFixture) wantDuplicate(t *testing.T, gotID string, err error, holder string) {
	t.Helper()
	if !errors.Is(err, service.ErrDuplicatePendingTask) {
		t.Fatalf("err = %v, want ErrDuplicatePendingTask", err)
	}
	if gotID != holder {
		t.Errorf("duplicate names run %s, want the holder %s", gotID, holder)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, util.UUIDToString(f.issue.ID), f.agentID); n != 1 {
		t.Errorf("%d runs after the refused insert, want only the holder", n)
	}
}

func forEachTarget(t *testing.T, name string, fn func(t *testing.T, f slotFixture)) {
	for _, squad := range []bool{false, true} {
		label := name + "/agent"
		if squad {
			label = name + "/squad"
		}
		t.Run(label, func(t *testing.T) {
			if testHandler == nil {
				t.Skip("database not available")
			}
			fn(t, newSlotFixture(t, squad))
		})
	}
}

// (1) A run in another thread no longer blocks a step entry with no handoff.
func TestSlotPreflight_NoHandoff_RunInAnotherThreadDoesNotBlock(t *testing.T) {
	forEachTarget(t, "cross-thread", func(t *testing.T, f slotFixture) {
		side := f.holder(t, f.otherCmt)
		id, err := f.enqueue(t, false, "")
		if err != nil {
			t.Fatalf("step run refused: %v", err)
		}
		f.wantStepRun(t, id, "")
		if id == side {
			t.Fatal("the step entry reused the side run")
		}
		var status string
		dbfx.QueryRow(t, `SELECT status FROM agent_task_queue WHERE id = $1`, side).Scan(&status)
		if status != "queued" {
			t.Errorf("side run status = %s, want it untouched", status)
		}
	})
}

// (2), (4) A handoff into thread T held by another run: the holder is named
// and the transaction survives.
func TestSlotPreflight_Handoff_ThreadHeld_NullFree(t *testing.T) {
	forEachTarget(t, "T-held", func(t *testing.T, f slotFixture) {
		holder := f.holder(t, f.threadT)
		id, err := f.enqueue(t, true, "")
		f.wantDuplicate(t, id, err, holder)
	})
}

// (3), (5) A handoff into a free thread T runs there even when NULL is held.
func TestSlotPreflight_Handoff_ThreadFree_NullHeld(t *testing.T) {
	forEachTarget(t, "T-free", func(t *testing.T, f slotFixture) {
		nullHolder := f.holder(t, "")
		id, err := f.enqueue(t, true, "")
		if err != nil {
			t.Fatalf("step run refused: %v", err)
		}
		if id == nullHolder {
			t.Fatal("the step run is the NULL holder")
		}
		f.wantStepRun(t, id, f.threadT)
	})
}

// (6) A media-pending deferred run holds its slot.
func TestSlotPreflight_MediaPendingHolderIsNamed(t *testing.T) {
	forEachTarget(t, "media-pending", func(t *testing.T, f slotFixture) {
		holder := f.holder(t, f.threadT, testutil.Cols{
			"status":  "deferred",
			"context": testutil.Raw(`'{"channel_issue_media_pending":"true"}'::jsonb`),
		})
		id, err := f.enqueue(t, true, "")
		f.wantDuplicate(t, id, err, holder)
	})
}

// (9) A step run already in the effective slot is the duplicate.
func TestSlotPreflight_StepHolderInSameSlot(t *testing.T) {
	forEachTarget(t, "same-slot", func(t *testing.T, f slotFixture) {
		holder := f.holder(t, f.handoff, testutil.Cols{"trigger_kind": service.TriggerKindStatusHandoff})
		id, err := f.enqueue(t, true, "")
		f.wantDuplicate(t, id, err, holder)
	})
}

// (10) A holder stamped with another head still holds the slot: the unique
// index has no head filter, so the preflight has none either.
func TestSlotPreflight_DifferentHeadInSlotStillHolds(t *testing.T) {
	forEachTarget(t, "other-head", func(t *testing.T, f slotFixture) {
		holder := f.holder(t, f.threadT, testutil.Cols{"context": testutil.Raw(`'{"head_sha":"0000000000000000000000000000000000000000"}'::jsonb`)})
		id, err := f.enqueue(t, true, "")
		f.wantDuplicate(t, id, err, holder)
	})
}

// (8) A resumed run with a trigger comment (no handoff ctx): only its own
// thread holds it.
func TestSlotPreflight_TriggerComment_OnlyItsThreadHolds(t *testing.T) {
	forEachTarget(t, "trigger-thread-held", func(t *testing.T, f slotFixture) {
		holder := f.holder(t, f.threadT)
		id, err := f.enqueue(t, false, f.handoff)
		f.wantDuplicate(t, id, err, holder)
	})
	forEachTarget(t, "trigger-other-slots-held", func(t *testing.T, f slotFixture) {
		f.holder(t, f.otherCmt)
		f.holder(t, "")
		id, err := f.enqueue(t, false, f.handoff)
		if err != nil {
			t.Fatalf("resumed run refused: %v", err)
		}
		task, _ := claimedPrompt(t, id)
		if util.UUIDToString(task.CommentThreadID) != f.threadT {
			t.Errorf("resumed run thread = %s, want %s", util.UUIDToString(task.CommentThreadID), f.threadT)
		}
	})
}
