package service

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// Corrections from the second signing review of L5: concurrent writers of one
// instance serialize, and a once/max_fires limit holds across carriers.

// rebaseProbe runs hook the first time a transaction reads the parent's
// sub-issue rules, which is after the child-event path has read the rule it is
// about to rebase.
type rebaseProbe struct {
	TxStarter
	armed *atomic.Bool
	hook  func()
}

func (p rebaseProbe) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := p.TxStarter.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return rebaseProbeTx{Tx: tx, p: p}, nil
}

type rebaseProbeTx struct {
	pgx.Tx
	p rebaseProbe
}

func (tx rebaseProbeTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "-- name: ListChildConditionWakeups") && tx.p.armed.CompareAndSwap(true, false) {
		tx.p.hook()
	}
	return tx.Tx.Query(ctx, sql, args...)
}

// Two writers that both judged the instance against the old configuration must
// not both rebase it: the second would bump the revision again, retiring inputs
// and withdrawing work the first already captured under the current one.
func TestBuiltinChildEventRebaseIsSerializedWithDispatch(t *testing.T) {
	ctx := context.Background()
	e := newBuiltinEnv(t)
	e.f.Cleanup(t, "DELETE FROM issue_child_event WHERE parent_id=$1", e.issue)
	first := e.f.Issue(t, "first child", testutil.Cols{"parent_issue_id": e.issue, "status": "in_progress"})
	if err := e.s.ProcessChildEvents(ctx, e.issue); err != nil {
		t.Fatal(err)
	}
	e.define(t, WakeupScopeProject, SystemRuleChildDone, `"instruction":"One"`)
	rule := e.rule(t, SystemRuleChildDone)
	e.f.Exec(t, "UPDATE issue SET status='done' WHERE id=$1", first)

	var armed atomic.Bool
	armed.Store(true)
	rival := make(chan error, 1)
	rivalFinishedEarly := false
	original := e.s.Tasks.TxStarter
	t.Cleanup(func() { e.s.Tasks.TxStarter = original })
	e.s.Tasks.TxStarter = rebaseProbe{TxStarter: original, armed: &armed, hook: func() {
		// The scheduler dispatches the same instance, judged against the same
		// old configuration, while the child-event path sits between reading
		// the rule and rebasing it. It either rebases first (before the fix) or
		// finds the instance held and leaves it to the next pass.
		go func() { rival <- e.s.dispatchSystem(ctx, rule) }()
		select {
		case err := <-rival:
			rivalFinishedEarly = err == nil
			rival <- err
		case <-time.After(400 * time.Millisecond):
		}
	}}
	if err := e.s.ProcessChildEvents(ctx, e.issue); err != nil {
		t.Fatal(err)
	}
	select {
	case <-rival:
	case <-time.After(10 * time.Second):
		t.Fatal("the scheduler pass never finished")
	}
	if armed.Load() {
		t.Fatal("the interleaving point was not reached")
	}
	if got := e.rule(t, SystemRuleChildDone).Revision; got != rule.Revision+1 {
		t.Fatalf("revision moved %d -> %d (the other writer rebased while this one held the rule: %v); one configuration change is one rebase", rule.Revision, got, rivalFinishedEarly)
	}
}

// secondCarrier queues another run of the same agent on the issue: the queue
// allows one per comment thread, so it answers a different thread.
func (e builtinEnv) secondCarrier(t *testing.T) string {
	t.Helper()
	thread := e.f.Comment(t, util.UUIDToString(e.issue), "another thread")
	return e.f.Task(t, e.agent, testutil.Cols{"issue_id": e.issue, "runtime_id": testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + e.agent + "')"),
		"originator_user_id": e.f.UserID, "accountable_user_id": e.f.UserID, "trigger_comment_id": thread})
}

// Two carriers must not both take a firing of a rule with one slot left. The
// second claim sees the first one's reservation and leaves its input alone.
func TestBuiltinJoinCountsOtherCarriersReservations(t *testing.T) {
	e := newBuiltinEnv(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"instruction":"One","max_fires":1`)
	a := wakeWaitingRun(t, e.f, e.issue, e.agent, e.f.UserID)
	e.mergedPR(t, 600)
	if notes := wakeClaim(t, e.f, e.s, a); !strings.Contains(notes, "One") {
		t.Fatalf("the first carrier did not join: %q", notes)
	}
	b := e.secondCarrier(t)
	e.mergedPR(t, 601)
	if notes := wakeClaim(t, e.f, e.s, b); notes != "" {
		t.Fatalf("a second carrier joined the last slot: %q", notes)
	}
	if n := e.f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE task_id=$1`, b); n != 0 {
		t.Fatalf("the second carrier reserved %d inputs", n)
	}
}

// Whatever got two carriers their reservations, the limit is judged again when
// each starts, under the instance lock: exactly one of two carriers fits the
// last slot, and a started one keeps it.
func TestBuiltinTwoCarriersCannotBothStartPastTheLimit(t *testing.T) {
	ctx := context.Background()
	e := newBuiltinEnv(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"instruction":"One","max_fires":1`)
	a := wakeWaitingRun(t, e.f, e.issue, e.agent, e.f.UserID)
	e.mergedPR(t, 610)
	if notes := wakeClaim(t, e.f, e.s, a); !strings.Contains(notes, "One") {
		t.Fatalf("the first carrier did not join: %q", notes)
	}
	b := e.secondCarrier(t)
	e.mergedPR(t, 611)
	// A reservation the join gate would now refuse, as an older head could have made.
	e.f.Exec(t, `UPDATE issue_wakeup_receipt SET task_id=$1 WHERE task_id IS NULL AND processed_at IS NULL AND wakeup_id=$2`, b, e.rule(t, SystemRulePRMerged).ID)
	e.f.Exec(t, `UPDATE agent_task_queue SET status='dispatched',dispatched_at=clock_timestamp(),
		context=jsonb_set(COALESCE(context,'{}'::jsonb),'{wakeup_joined}',(SELECT context->'wakeup_joined' FROM agent_task_queue WHERE id=$2)) WHERE id=$1`, b, a)
	errs := map[string]error{a: e.s.CheckStart(ctx, e.task(t, a)), b: e.s.CheckStart(ctx, e.task(t, b))}
	var winner, loser string
	for id, err := range errs {
		switch {
		case err == nil && winner == "":
			winner = id
		case errors.Is(err, ErrWakeupForbidden) && loser == "":
			loser = id
		default:
			t.Fatalf("carrier check results %v: want exactly one start and one ErrWakeupForbidden", errs)
		}
	}
	if winner == "" || loser == "" {
		t.Fatalf("carrier check results %v: want exactly one start and one ErrWakeupForbidden", errs)
	}
	wakeStart(t, e.f, winner)
	if err := e.s.CheckStart(ctx, e.task(t, loser)); !errors.Is(err, ErrWakeupForbidden) {
		t.Fatalf("a carrier started after the last slot was taken = %v, want ErrWakeupForbidden", err)
	}
}

// crowdReceipts adds free pending inputs that sort ahead of every reservation
// the rule holds, so a read of the first page of pending inputs never reaches
// the reserved ones.
func (e builtinEnv) crowdReceipts(t *testing.T, key string, n int) {
	t.Helper()
	w := e.rule(t, key)
	e.f.Exec(t, `INSERT INTO issue_wakeup_receipt(id,wakeup_id,revision,event_key,event_type,payload,created_at)
		SELECT gen_random_uuid(),$1,$2,'crowd:'||g,'pr.merged','{}'::jsonb,now()-interval '1 day'+g*interval '1 second' FROM generate_series(1,$3) g`, w.ID, w.Revision, n)
}

// More pending inputs than one page holds must not hide another carrier's
// reservation from the join gate.
func TestBuiltinJoinCountsReservationsBeyondOnePageOfInputs(t *testing.T) {
	e := newBuiltinEnv(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"instruction":"One","max_fires":1`)
	a := wakeWaitingRun(t, e.f, e.issue, e.agent, e.f.UserID)
	e.mergedPR(t, 620)
	if notes := wakeClaim(t, e.f, e.s, a); !strings.Contains(notes, "One") {
		t.Fatalf("the first carrier did not join: %q", notes)
	}
	e.crowdReceipts(t, SystemRulePRMerged, 120)
	b := e.secondCarrier(t)
	if notes := wakeClaim(t, e.f, e.s, b); notes != "" {
		t.Fatalf("a second carrier joined the last slot behind %d unreserved inputs: %q", 120, notes)
	}
}

// The same at start: both carriers hold reservations past the first page.
func TestBuiltinTwoCarriersCannotBothStartWithReservationsBeyondOnePage(t *testing.T) {
	ctx := context.Background()
	e := newBuiltinEnv(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"instruction":"One","max_fires":1`)
	a := wakeWaitingRun(t, e.f, e.issue, e.agent, e.f.UserID)
	e.mergedPR(t, 630)
	if notes := wakeClaim(t, e.f, e.s, a); !strings.Contains(notes, "One") {
		t.Fatalf("the first carrier did not join: %q", notes)
	}
	e.crowdReceipts(t, SystemRulePRMerged, 120)
	b := e.secondCarrier(t)
	e.mergedPR(t, 631)
	e.f.Exec(t, `UPDATE issue_wakeup_receipt SET task_id=$1 WHERE event_key LIKE 'github:%631%' AND processed_at IS NULL AND task_id IS NULL AND wakeup_id=$2`, b, e.rule(t, SystemRulePRMerged).ID)
	e.f.Exec(t, `UPDATE agent_task_queue SET status='dispatched',dispatched_at=clock_timestamp(),
		context=jsonb_set(COALESCE(context,'{}'::jsonb),'{wakeup_joined}',(SELECT context->'wakeup_joined' FROM agent_task_queue WHERE id=$2)) WHERE id=$1`, b, a)
	passed := 0
	for _, id := range []string{a, b} {
		switch err := e.s.CheckStart(ctx, e.task(t, id)); {
		case err == nil:
			passed++
		case !errors.Is(err, ErrWakeupForbidden):
			t.Fatal(err)
		}
	}
	if passed != 1 {
		t.Fatalf("%d of two carriers may start the last slot, want exactly 1", passed)
	}
}
