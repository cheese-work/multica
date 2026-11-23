package service

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// CHE-6990 (plan CHE-6971, step 4): a wakeup with an open live-join item holds
// its pass, and a delivered item settles as joined only when the host run
// ended completed. Items are seeded through the step-2 queries; the join that
// creates them is a later step.

type liveEnv struct {
	f        principalFixture
	s        *IssueWakeupService
	issue    pgtype.UUID
	agent    string
	w        db.IssueWakeup
	host     string
	receipts []pgtype.UUID
	item     db.TaskPrEventDelivery
}

func newLiveEnv(t *testing.T, in WakeupInput) *liveEnv {
	t.Helper()
	f, s, issue, agent := conditionFixture(t)
	in.AgentID, in.Kind = agent, "event"
	if in.EventTypes == nil {
		in.EventTypes = []string{"comment.created"}
	}
	if in.Instruction == "" {
		in.Instruction = "Check the condition"
	}
	w := wakeCreate(t, f, s, issue, in)
	f.Cleanup(t, `DELETE FROM task_pr_event_delivery WHERE issue_id=$1`, issue)
	f.Cleanup(t, `DELETE FROM task_pr_event_capability WHERE issue_id=$1`, issue)
	e := &liveEnv{f: f, s: s, issue: issue, agent: agent, w: w}
	e.host = e.run(t, agent)
	f.Exec(t, `INSERT INTO task_pr_event_capability(task_id,workspace_id,issue_id,capability) VALUES($1,$2,$3,'task-supplement-pr-event-v2')`, e.host, f.WorkspaceID, issue)
	return e
}

// run inserts a running run of the agent on the issue.
func (e *liveEnv) run(t *testing.T, agent string) string {
	t.Helper()
	return e.f.Task(t, agent, testutil.Cols{"issue_id": e.issue, "status": "running", "started_at": testutil.Raw("clock_timestamp()"),
		"runtime_id":         testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + agent + "')"),
		"originator_user_id": e.f.UserID, "accountable_user_id": e.f.UserID})
}

// receipt seeds one pending condition.met receipt of the wakeup.
func (e *liveEnv) receipt(t *testing.T) pgtype.UUID {
	t.Helper()
	id := dbid.NewV7()
	e.f.Exec(t, `INSERT INTO issue_wakeup_receipt(id,wakeup_id,revision,event_key,event_type,payload)
		VALUES($1,$2,$3,$4,'condition.met','{}')`, id, e.w.ID, e.w.Revision, util.UUIDToString(id))
	e.receipts = append(e.receipts, id)
	return id
}

// enqueue puts the given receipts in a pending wakeup item on the host run.
func (e *liveEnv) enqueue(t *testing.T, ids ...pgtype.UUID) {
	t.Helper()
	item, err := e.f.q.EnqueueTaskWakeupDelivery(context.Background(), db.EnqueueTaskWakeupDeliveryParams{
		WakeupID: e.w.ID, WakeupRevision: int32(e.w.Revision), ReceiptIds: ids, Note: "wakeup note", TaskID: parseTestUUID(t, e.host)})
	if err != nil {
		t.Fatalf("seed item: %v", err)
	}
	e.item = item
}

func (e *liveEnv) setItem(t *testing.T, status string) {
	t.Helper()
	e.f.Exec(t, `UPDATE task_pr_event_delivery SET status=$2, attempt_count=1, failure_reason=CASE WHEN $2='failed' THEN 'turn_ended' END WHERE event_id=$1`, e.item.EventID, status)
}

func (e *liveEnv) endHost(t *testing.T, status string) {
	t.Helper()
	e.f.Exec(t, `UPDATE agent_task_queue SET status=$2, completed_at=clock_timestamp() WHERE id=$1`, e.host, status)
}

func (e *liveEnv) pass(t *testing.T) db.IssueWakeup {
	t.Helper()
	return wakeTick(t, e.f, e.s, e.w.ID)
}

func (e *liveEnv) pending(t *testing.T, id pgtype.UUID) bool {
	t.Helper()
	return e.f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE id=$1 AND processed_at IS NULL AND task_id IS NULL`, id) == 1
}

func (e *liveEnv) items(t *testing.T) int {
	t.Helper()
	return e.f.Count(t, `SELECT count(*) FROM task_pr_event_delivery WHERE wakeup_id=$1`, e.w.ID)
}

func (e *liveEnv) triggered(t *testing.T, outcome string) int {
	t.Helper()
	return e.f.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='wakeup_triggered' AND details->>'outcome'=$2`, e.issue, outcome)
}

// held asserts the pass changed nothing: no run, no second item, no count, the
// receipts still pending and unreserved, and last_error naming the host run.
func (e *liveEnv) held(t *testing.T, got db.IssueWakeup, ids ...pgtype.UUID) {
	t.Helper()
	if n := wakeRuns(t, e.f, e.w.ID); n != 0 {
		t.Fatalf("held wakeup started %d runs", n)
	}
	if n := e.items(t); n != 1 {
		t.Fatalf("items = %d, want 1", n)
	}
	if got.FireCount != 0 || got.LastTaskID.Valid {
		t.Fatalf("held wakeup counted: fire_count=%d last_task=%v", got.FireCount, got.LastTaskID)
	}
	for _, id := range ids {
		if !e.pending(t, id) {
			t.Fatalf("receipt %s was consumed or reserved", util.UUIDToString(id))
		}
	}
	if !strings.Contains(got.LastError.String, e.host) {
		t.Fatalf("last_error = %q, want it to name the host run %s", got.LastError.String, e.host)
	}
}

func TestOpenWakeupItemHoldsThePass(t *testing.T) {
	for _, status := range []string{"pending", "delivering", "failed"} {
		t.Run(status, func(t *testing.T) {
			e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
			r := e.receipt(t)
			e.enqueue(t, r)
			if status != "pending" {
				e.setItem(t, status)
			}
			e.held(t, e.pass(t), r)
		})
	}
}

func TestDeliveredItemHoldsWhileHostRuns(t *testing.T) {
	e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
	r := e.receipt(t)
	e.enqueue(t, r)
	e.setItem(t, "delivered")
	e.held(t, e.pass(t), r)
	e.held(t, e.pass(t), r)
}

func TestDeliveredItemSettlesJoinedWhenHostCompleted(t *testing.T) {
	e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
	r := e.receipt(t)
	e.enqueue(t, r)
	e.setItem(t, "delivered")
	e.endHost(t, "completed")
	got := e.pass(t)
	if n := wakeRuns(t, e.f, e.w.ID); n != 0 {
		t.Fatalf("a joined firing started %d runs", n)
	}
	var task pgtype.UUID
	var processed bool
	e.f.QueryRow(t, `SELECT task_id, processed_at IS NOT NULL FROM issue_wakeup_receipt WHERE id=$1`, r).Scan(&task, &processed)
	if !processed || util.UUIDToString(task) != e.host {
		t.Fatalf("receipt processed=%v task_id=%s, want consumed by host %s", processed, util.UUIDToString(task), e.host)
	}
	if got.FireCount != 1 || util.UUIDToString(got.LastTaskID) != e.host {
		t.Fatalf("fire_count=%d last_task_id=%s, want 1 and the host", got.FireCount, util.UUIDToString(got.LastTaskID))
	}
	if n := e.triggered(t, "joined_live"); n != 1 {
		t.Fatalf("joined_live rows = %d, want 1", n)
	}
	var firing, host string
	e.f.QueryRow(t, `SELECT details->>'firing_id', details->>'task_id' FROM activity_log WHERE issue_id=$1 AND action='wakeup_triggered'`, e.issue).Scan(&firing, &host)
	if firing != util.UUIDToString(e.item.EventID) || host != e.host {
		t.Fatalf("activity firing_id=%q task_id=%q, want %s and %s", firing, host, util.UUIDToString(e.item.EventID), e.host)
	}
	// Later passes settle nothing again.
	if again := e.pass(t); again.FireCount != 1 || e.triggered(t, "joined_live") != 1 {
		t.Fatalf("second pass counted again: fire_count=%d", again.FireCount)
	}
}

func TestJoinedSettlementAppliesOnceAndMaxFires(t *testing.T) {
	t.Run("once", func(t *testing.T) {
		e := newLiveEnv(t, WakeupInput{Mode: "once"})
		r := e.receipt(t)
		e.enqueue(t, r)
		e.setItem(t, "delivered")
		// A once wakeup stays enabled until the firing is settled.
		if got := e.pass(t); !got.Enabled {
			t.Fatal("a once wakeup must stay enabled while its item is open")
		}
		e.endHost(t, "completed")
		got := e.pass(t)
		if got.Enabled || got.FireCount != 1 {
			t.Fatalf("once after settlement: enabled=%v fire_count=%d, want disabled and 1", got.Enabled, got.FireCount)
		}
	})
	t.Run("max_fires", func(t *testing.T) {
		e := newLiveEnv(t, WakeupInput{Mode: "continuous", MaxFires: 1})
		r := e.receipt(t)
		e.enqueue(t, r)
		e.setItem(t, "delivered")
		e.endHost(t, "completed")
		got := e.pass(t)
		if got.Enabled || got.PausedReason.String != wakeupPausedMaxFires || got.FireCount != 1 {
			t.Fatalf("max_fires after settlement: enabled=%v paused=%q fire_count=%d", got.Enabled, got.PausedReason.String, got.FireCount)
		}
	})
}

func TestDiscardedReceiptsEndTheHoldAndNeverCount(t *testing.T) {
	e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
	r := e.receipt(t)
	e.enqueue(t, r)
	if err := e.f.q.DiscardWakeupReceipts(context.Background(), e.w.ID); err != nil {
		t.Fatal(err)
	}
	// Nothing pending: the pass neither holds on the item nor runs.
	got := e.pass(t)
	if n := wakeRuns(t, e.f, e.w.ID); n != 0 || got.LastError.Valid && strings.Contains(got.LastError.String, e.host) {
		t.Fatalf("discarded inputs: runs=%d last_error=%q", n, got.LastError.String)
	}
	e.setItem(t, "delivered")
	e.endHost(t, "completed")
	if got := e.pass(t); got.FireCount != 0 || e.triggered(t, "joined_live") != 0 {
		t.Fatalf("a delivered item with discarded receipts counted: fire_count=%d", got.FireCount)
	}
}

func TestSettlementNeverConsumesAnotherRunsReceipts(t *testing.T) {
	e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
	mine, other := e.receipt(t), e.receipt(t)
	e.enqueue(t, mine)
	// A claimed run that has not started holds the other receipt.
	claimed := wakeWaitingRun(t, e.f, e.issue, e.agent, e.f.UserID)
	e.f.Exec(t, `UPDATE agent_task_queue SET status='dispatched', dispatched_at=clock_timestamp() WHERE id=$1`, claimed)
	e.f.Exec(t, `UPDATE issue_wakeup_receipt SET task_id=$2 WHERE id=$1`, other, claimed)
	e.setItem(t, "delivered")
	e.endHost(t, "completed")
	e.pass(t)
	var task pgtype.UUID
	var processed bool
	e.f.QueryRow(t, `SELECT task_id, processed_at IS NOT NULL FROM issue_wakeup_receipt WHERE id=$1`, other).Scan(&task, &processed)
	if processed || util.UUIDToString(task) != claimed {
		t.Fatalf("another run's receipt: processed=%v task_id=%s, want untouched and held by %s", processed, util.UUIDToString(task), claimed)
	}
	if e.f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE id=$1 AND processed_at IS NOT NULL AND task_id=$2`, mine, e.host) != 1 {
		t.Fatal("the item's own receipt must be consumed with the host run")
	}
}

func TestWaitingRunLeavesHeldReceiptsOfAnItem(t *testing.T) {
	for _, tc := range []struct {
		status string
		takes  bool
	}{{"pending", false}, {"delivering", false}, {"delivered", false}, {"failed", false}, {"fallback", true}} {
		t.Run(tc.status, func(t *testing.T) {
			e := newLiveEnv(t, WakeupInput{Mode: "continuous", Instruction: "Reserve me"})
			r := e.receipt(t)
			e.enqueue(t, r)
			if tc.status != "pending" {
				e.f.Exec(t, `UPDATE task_pr_event_delivery SET status=$2 WHERE event_id=$1`, e.item.EventID, tc.status)
			}
			// The host run is another run than the one that waits.
			waiting := wakeWaitingRun(t, e.f, e.issue, e.agent, e.f.UserID)
			notes := wakeClaim(t, e.f, e.s, waiting)
			var reservedBy pgtype.UUID
			e.f.QueryRow(t, `SELECT task_id FROM issue_wakeup_receipt WHERE id=$1`, r).Scan(&reservedBy)
			if got := strings.Contains(notes, "Reserve me"); got != tc.takes {
				t.Fatalf("claim joined=%v, want %v (notes %q)", got, tc.takes, notes)
			}
			if got := reservedBy.Valid && util.UUIDToString(reservedBy) == waiting; got != tc.takes {
				t.Fatalf("receipt reserved by the waiting run = %v, want %v", got, tc.takes)
			}
		})
	}
}

func TestWaitingRunJoinsWithoutItems(t *testing.T) {
	e := newLiveEnv(t, WakeupInput{Mode: "continuous", Instruction: "Reserve me"})
	r := e.receipt(t)
	waiting := wakeWaitingRun(t, e.f, e.issue, e.agent, e.f.UserID)
	if notes := wakeClaim(t, e.f, e.s, waiting); !strings.Contains(notes, "Reserve me") {
		t.Fatalf("with no items the waiting run must join as before: %q", notes)
	}
	var reservedBy pgtype.UUID
	e.f.QueryRow(t, `SELECT task_id FROM issue_wakeup_receipt WHERE id=$1`, r).Scan(&reservedBy)
	if util.UUIDToString(reservedBy) != waiting {
		t.Fatalf("receipt reserved by %s, want %s", util.UUIDToString(reservedBy), waiting)
	}
}

func TestTakenReceiptsReturnsHeldReceiptsAsFree(t *testing.T) {
	e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
	r := e.receipt(t)
	e.enqueue(t, r)
	pending, err := e.f.q.ListPendingWakeupReceipts(context.Background(), db.ListPendingWakeupReceiptsParams{WakeupID: e.w.ID, Revision: e.w.Revision})
	if err != nil {
		t.Fatal(err)
	}
	free, holding, taken, err := takenReceipts(context.Background(), e.f.q, pending)
	if err != nil || len(free) != 1 || holding != 0 || len(taken) != 0 {
		t.Fatalf("takenReceipts = free %d holding %d taken %d err %v, want 1 free and nothing taken", len(free), holding, len(taken), err)
	}
}

func TestHandoffConsumeLeavesConditionReceiptsAlone(t *testing.T) {
	e := newLiveEnv(t, WakeupInput{Mode: "continuous"})
	r := e.receipt(t)
	e.f.Exec(t, `UPDATE issue_wakeup_receipt SET payload='{"handoff_write":"w1"}' WHERE id=$1`, r)
	if err := e.f.q.ConsumeHandoffWakeupReceipts(context.Background(), db.ConsumeHandoffWakeupReceiptsParams{
		IssueID: e.issue, AgentID: parseTestUUID(t, e.agent), Write: "w1"}); err != nil {
		t.Fatal(err)
	}
	if !e.pending(t, r) {
		t.Fatal("a condition.met receipt must not be consumed by a handoff write")
	}
}
