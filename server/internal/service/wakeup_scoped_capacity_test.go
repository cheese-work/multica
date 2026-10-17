package service

import (
	"context"
	"testing"
	"time"
)

// A default pool with no free slot must not cost an input its delivery (CHE-1082
// L9): the input stays pending behind a bounded retry time and is delivered once
// capacity frees, or expires visibly at retention.

// fillDefaultPool leaves free slots of the workspace's default-instance pool.
func (k drainKit) fillDefaultPool(t *testing.T, free int) {
	t.Helper()
	k.f.Exec(t, `INSERT INTO issue_wakeup(id,workspace_id,issue_id,agent_id,instruction,kind,mode,default_rule_key,default_scope_kind,default_scope_id)
 SELECT gen_random_uuid(),$1,$2,$3,'filler','event','continuous',gen_random_uuid()::text,'workspace',$1 FROM generate_series(1,$4::int)`, k.f.WorkspaceID, k.issue, k.agent, 32-free)
}

func (k drainKit) freeDefaultPool(t *testing.T) {
	t.Helper()
	k.f.Exec(t, `DELETE FROM issue_wakeup WHERE issue_id=$1 AND instruction='filler'`, k.issue)
}

func (k drainKit) makePendingDue(t *testing.T) {
	t.Helper()
	k.f.Exec(t, `UPDATE wakeup_scoped_event SET retry_at=now()-interval '1 second' WHERE workspace_id=$1 AND handled_at IS NULL`, k.f.WorkspaceID)
}

// wantHeld checks the only input is pending for capacity. L10 records the
// refusal as outcome default_capacity on the still-pending row.
func (k drainKit) wantHeld(t *testing.T) {
	t.Helper()
	k.wantOutcomes(t, "default_capacity")
	if k.pending(t) != 1 {
		t.Fatal("a capacity-refused input must stay pending")
	}
}

// retryDelay is how far ahead the only pending input's retry time lies.
func (k drainKit) retryDelay(t *testing.T) time.Duration {
	t.Helper()
	var secs *float64
	if err := k.f.Pool.QueryRow(context.Background(), `SELECT extract(epoch FROM retry_at-now()) FROM wakeup_scoped_event WHERE workspace_id=$1 AND handled_at IS NULL`, k.f.WorkspaceID).Scan(&secs); err != nil {
		t.Fatal(err)
	}
	if secs == nil {
		t.Fatal("the pending input has no retry time")
	}
	return time.Duration(*secs * float64(time.Second))
}

func TestScopedDrainCapacityRefusedInputStaysPendingAndDeliversWhenCapacityFrees(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	k.fillDefaultPool(t, 0)
	k.comment(t, "the pool is full")
	k.drain(t)
	k.wantHeld(t)
	if d := k.retryDelay(t); d <= 0 || d > time.Hour {
		t.Fatalf("retry time is %v ahead, want a bounded delay in the future", d)
	}
	if got := k.receipts(t, scopedRuleA); len(got) != 0 {
		t.Fatalf("a full pool delivered %+v", got)
	}

	// Capacity frees. L10 brings a held input forward when the pool frees
	// (wakeup_custom_capacity_test.go), so make it due either way.
	k.freeDefaultPool(t)
	k.makePendingDue(t)
	k.drain(t)
	k.wantOutcomes(t, "delivered")
	if got := k.receipts(t, scopedRuleA); len(got) != 1 || got[0].Count != 1 {
		t.Fatalf("receipts = %+v, want the held event delivered once", got)
	}
	if inst := k.instances(t, k.issue); len(inst) != 1 || !inst[0].Enabled {
		t.Fatalf("instances = %+v, want the rule's instance applied", inst)
	}
}

func TestScopedDrainCapacityRefusedInputExpiresVisiblyAtRetention(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	k.fillDefaultPool(t, 0)
	k.comment(t, "the pool stays full")
	k.drain(t)
	k.wantHeld(t)

	k.f.Exec(t, `UPDATE wakeup_scoped_event SET captured_at=now()-interval '8 days',retry_at=now()-interval '1 second' WHERE workspace_id=$1`, k.f.WorkspaceID)
	k.freeDefaultPool(t)
	if err := k.s.TickWorkspaces(context.Background(), parseTestUUID(t, k.f.WorkspaceID)); err != nil {
		t.Logf("tick: %v", err)
	}
	k.wantOutcomes(t, "expired")
	if len(k.receipts(t, scopedRuleA)) != 0 {
		t.Fatal("an input past retention was delivered")
	}
}

func TestScopedDrainPartialCapacityRefusalKeepsTheInputForTheRefusedRule(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleB, "comment.created")
	k.fillDefaultPool(t, 1) // rule A takes the last slot, rule B is refused
	k.comment(t, "one slot for two rules")
	k.drain(t)
	k.wantHeld(t)
	if got := k.receipts(t, scopedRuleA); len(got) != 1 || got[0].Count != 1 {
		t.Fatalf("rule A receipts = %+v, want the event delivered", got)
	}
	if got := k.receipts(t, scopedRuleB); len(got) != 0 {
		t.Fatalf("rule B receipts = %+v, want none while it is refused", got)
	}

	k.freeDefaultPool(t)
	k.makePendingDue(t)
	k.drain(t)
	k.wantOutcomes(t, "delivered")
	if got := k.receipts(t, scopedRuleB); len(got) != 1 || got[0].Count != 1 {
		t.Fatalf("rule B receipts = %+v, want the held event delivered once", got)
	}
	if got := k.receipts(t, scopedRuleA); len(got) != 1 || got[0].Count != 1 {
		t.Fatalf("rule A receipts = %+v, want the event counted once, not again on the retry", got)
	}
}

func TestScopedDrainRetryNeverRepeatsDeliveryAfterTheServedInstanceIsRebased(t *testing.T) {
	k := newDrainKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleB, "comment.created")
	k.fillDefaultPool(t, 1)
	k.comment(t, "one slot for two rules")
	k.drain(t)
	k.wantHeld(t)

	// Rule A's instance moves to another configuration between the passes.
	k.f.Exec(t, `UPDATE issue_wakeup SET revision=revision+1,config_fingerprint='rebased' WHERE issue_id=$1 AND default_rule_key=$2`, k.issue, scopedRuleA)
	k.freeDefaultPool(t)
	k.makePendingDue(t)
	k.drain(t)
	k.wantOutcomes(t, "delivered")
	if got := k.receipts(t, scopedRuleA); len(got) != 1 || got[0].Count != 1 {
		t.Fatalf("rule A receipts = %+v, want the event delivered once, not to the rebased revision again", got)
	}
	if got := k.receipts(t, scopedRuleB); len(got) != 1 || got[0].Count != 1 {
		t.Fatalf("rule B receipts = %+v, want the held event delivered once", got)
	}
}
