package service

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Capacity-refused event inputs (CHE-1200, decision (b) of the L10 review): an
// input the default pool refused is delayed and retried, never dropped. It is
// delivered once capacity frees, inside the drain budget and the outbox
// retention; past retention it expires visibly. Delivery is idempotent and never
// rearms an instance that ended.

// fillIssuePool takes n of the issue's 32 default slots with instances of
// unrelated rules, so the next default instance on the issue is refused.
func (k customEnv) fillIssuePool(t *testing.T, n int) {
	t.Helper()
	k.f.Exec(t, `INSERT INTO issue_wakeup(id,workspace_id,issue_id,agent_id,instruction,kind,mode,default_rule_key,default_scope_kind,default_scope_id)
 SELECT gen_random_uuid(),$1,$2,$3,'filler','event','continuous',gen_random_uuid()::text,'workspace',$1 FROM generate_series(1,$4)`,
		k.f.WorkspaceID, k.issue, k.agent, n)
}

func (k customEnv) freePool(t *testing.T) {
	t.Helper()
	k.f.Exec(t, `DELETE FROM issue_wakeup WHERE issue_id=$1 AND instruction='filler'`, k.issue)
}

// inputState reads the outbox inputs of the fixture workspace as (pending, outcome).
func (k customEnv) inputState(t *testing.T) (pending int, retryInFuture int, outcomes []string) {
	t.Helper()
	pending = k.f.Count(t, `SELECT count(*) FROM wakeup_scoped_event WHERE workspace_id=$1 AND handled_at IS NULL`, k.f.WorkspaceID)
	retryInFuture = k.f.Count(t, `SELECT count(*) FROM wakeup_scoped_event WHERE workspace_id=$1 AND handled_at IS NULL AND retry_at>now()`, k.f.WorkspaceID)
	return pending, retryInFuture, k.outcomes(t)
}

func (k customEnv) receiptCount(t *testing.T, rule string) (receipts, coalesced int) {
	t.Helper()
	for _, r := range k.receipts(t, rule) {
		receipts++
		coalesced += r.Count
	}
	return receipts, coalesced
}

func TestCustomCapacityRefusedEventStaysPendingAndIsDeliveredWhenThePoolFrees(t *testing.T) {
	k := newCustomEnv(t)
	k.fillIssuePool(t, 32)
	view := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`))
	k.comment(t, "refused: the issue's default pool is full")
	k.drain(t)

	pending, delayed, _ := k.inputState(t)
	if pending != 1 || delayed != 1 {
		t.Fatalf("pending=%d delayed=%d, want the refused input kept pending behind a retry time", pending, delayed)
	}
	if got := k.instanceRow(t, view.RuleKey); got.Enabled || !got.CapacityReason.Valid {
		t.Fatalf("instance enabled=%v capacity=%v, want it unapplied with its visible reason", got.Enabled, got.CapacityReason.Valid)
	}
	if n, _ := k.receiptCount(t, view.RuleKey); n != 0 {
		t.Fatalf("a full pool delivered %d receipts", n)
	}

	// Capacity frees. The scheduler's sweep applies the held instance, which also
	// brings the delayed input forward; the next drain delivers it.
	k.freePool(t)
	k.sweep(t)
	k.drain(t)
	if n, count := k.receiptCount(t, view.RuleKey); n != 1 || count != 1 {
		t.Fatalf("receipts=%d events=%d, want the refused event delivered once", n, count)
	}
	if pending, _, outcomes := k.inputState(t); pending != 0 || len(outcomes) != 1 || outcomes[0] != "delivered" {
		t.Fatalf("pending=%d outcomes=%v, want the input handled as delivered", pending, outcomes)
	}
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 1)
}

func TestCustomCapacityRetryDelayIsBoundedAndGrowsWithTheInputsAge(t *testing.T) {
	k := newCustomEnv(t)
	k.fillIssuePool(t, 32)
	k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`))
	k.comment(t, "fresh")
	k.drain(t)
	var fresh time.Duration
	if err := k.f.Pool.QueryRow(context.Background(), `SELECT retry_at-now() FROM wakeup_scoped_event WHERE workspace_id=$1 AND handled_at IS NULL`, k.f.WorkspaceID).Scan(&fresh); err != nil {
		t.Fatalf("a refused input must carry a retry time: %v", err)
	}
	if fresh < 30*time.Second || fresh > 5*time.Minute {
		t.Fatalf("fresh input retries in %v, want about a minute", fresh)
	}
	// While the pool stays full the input is not attempted again before its time.
	var before time.Time
	k.f.Pool.QueryRow(context.Background(), `SELECT retry_at FROM wakeup_scoped_event WHERE workspace_id=$1 AND handled_at IS NULL`, k.f.WorkspaceID).Scan(&before)
	k.drain(t)
	k.drain(t)
	var after time.Time
	if err := k.f.Pool.QueryRow(context.Background(), `SELECT retry_at FROM wakeup_scoped_event WHERE workspace_id=$1 AND handled_at IS NULL`, k.f.WorkspaceID).Scan(&after); err != nil || !after.Equal(before) {
		t.Fatalf("retry time moved from %v to %v (%v): a delayed input must not be attempted before its time", before, after, err)
	}

	// An old input backs off further, up to an hour, so a pool that stays full
	// costs a handful of attempts over the retention rather than thousands.
	k.f.Exec(t, `UPDATE wakeup_scoped_event SET captured_at=now()-interval '3 days',retry_at=now()-interval '1 second' WHERE workspace_id=$1 AND handled_at IS NULL`, k.f.WorkspaceID)
	k.drain(t)
	var old time.Duration
	if err := k.f.Pool.QueryRow(context.Background(), `SELECT retry_at-now() FROM wakeup_scoped_event WHERE workspace_id=$1 AND handled_at IS NULL`, k.f.WorkspaceID).Scan(&old); err != nil {
		t.Fatal(err)
	}
	if old < 30*time.Minute || old > 2*time.Hour {
		t.Fatalf("a 3-day-old input retries in %v, want about an hour", old)
	}
}

func TestCustomCapacityRefusedInputExpiresVisiblyPastRetentionAndNeverDelivers(t *testing.T) {
	k := newCustomEnv(t)
	k.fillIssuePool(t, 32)
	view := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`))
	k.comment(t, "refused and never served")
	k.f.Exec(t, `UPDATE wakeup_scoped_event SET captured_at=now()-interval '6 days' WHERE workspace_id=$1`, k.f.WorkspaceID)
	k.drain(t)
	if pending, _, _ := k.inputState(t); pending != 1 {
		t.Fatalf("pending=%d, want the input still waiting inside retention", pending)
	}

	// Past retention the input can only expire, even though capacity is free now.
	k.f.Exec(t, `UPDATE wakeup_scoped_event SET captured_at=now()-interval '8 days',retry_at=NULL WHERE workspace_id=$1`, k.f.WorkspaceID)
	k.freePool(t)
	k.sweep(t)
	if err := k.s.pruneScopedEvents(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	k.drain(t)
	if n, _ := k.receiptCount(t, view.RuleKey); n != 0 {
		t.Fatalf("an expired input delivered %d receipts", n)
	}
	if pending, _, outcomes := k.inputState(t); pending != 0 || len(outcomes) != 1 || outcomes[0] != "expired" {
		t.Fatalf("pending=%d outcomes=%v, want the input closed as expired", pending, outcomes)
	}
}

func TestCustomCapacityRetryDeliversOnceHoweverOftenItIsAttempted(t *testing.T) {
	k := newCustomEnv(t)
	k.fillIssuePool(t, 32)
	view := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`))
	k.comment(t, "first")
	k.comment(t, "second")
	k.drain(t)
	if pending, _, _ := k.inputState(t); pending != 2 {
		t.Fatalf("pending=%d, want both refused inputs kept", pending)
	}
	k.freePool(t)
	k.sweep(t)
	k.drain(t)
	if n, count := k.receiptCount(t, view.RuleKey); n != 1 || count != 2 {
		t.Fatalf("receipts=%d events=%d, want the two events coalesced into one receipt", n, count)
	}
	// A crash between delivery and the mark leaves the inputs pending again; the
	// replay must not count them twice.
	k.f.Exec(t, `UPDATE wakeup_scoped_event SET handled_at=NULL,outcome=NULL WHERE workspace_id=$1`, k.f.WorkspaceID)
	k.drain(t)
	k.drain(t)
	if n, count := k.receiptCount(t, view.RuleKey); n != 1 || count != 2 {
		t.Fatalf("after replay receipts=%d events=%d, want exactly one receipt of two events", n, count)
	}
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 1)
}

func TestCustomCapacityRetryNeverRearmsAnInstanceThatEnded(t *testing.T) {
	t.Run("issue closed while the input waited", func(t *testing.T) {
		k := newCustomEnv(t)
		k.fillIssuePool(t, 32)
		view := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`))
		k.comment(t, "waits")
		k.drain(t)
		if pending, _, _ := k.inputState(t); pending != 1 {
			t.Fatalf("pending=%d, want the refused input kept", pending)
		}
		wakeSetStatus(t, k.f, parseTestUUID(t, k.issue), "done")
		k.freePool(t)
		k.sweep(t)
		k.f.Exec(t, `UPDATE wakeup_scoped_event SET retry_at=NULL WHERE workspace_id=$1`, k.f.WorkspaceID)
		k.drain(t)
		if n, _ := k.receiptCount(t, view.RuleKey); n != 0 {
			t.Fatalf("an input for a closed issue delivered %d receipts", n)
		}
		if w := k.instanceRow(t, view.RuleKey); w.Enabled {
			t.Fatal("a closed issue's instance was applied again")
		}
		if _, _, outcomes := k.inputState(t); len(outcomes) != 1 || outcomes[0] != "issue_closed" {
			t.Fatalf("outcomes=%v, want the input closed with the issue", outcomes)
		}
	})
	t.Run("consumed once instance", func(t *testing.T) {
		k := newCustomEnv(t)
		k.fillIssuePool(t, 32)
		view := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`)+`,"mode":"once"`)
		k.comment(t, "waits")
		k.drain(t)
		if pending, _, _ := k.inputState(t); pending != 1 {
			t.Fatalf("pending=%d, want the refused input kept", pending)
		}
		// The once instance is consumed meanwhile: it rests as ended.
		k.f.Exec(t, `UPDATE issue_wakeup SET paused_reason='max_fires',fire_count=1 WHERE issue_id=$1 AND default_rule_key=$2`, k.issue, view.RuleKey)
		k.freePool(t)
		k.sweep(t)
		k.f.Exec(t, `UPDATE wakeup_scoped_event SET retry_at=NULL WHERE workspace_id=$1`, k.f.WorkspaceID)
		k.drain(t)
		if n, _ := k.receiptCount(t, view.RuleKey); n != 0 {
			t.Fatalf("a consumed once instance received %d receipts", n)
		}
		if w := k.instanceRow(t, view.RuleKey); w.Enabled || !w.PausedReason.Valid {
			t.Fatalf("instance enabled=%v paused=%v, want it to rest as consumed", w.Enabled, w.PausedReason.Valid)
		}
		if _, _, outcomes := k.inputState(t); len(outcomes) != 1 || outcomes[0] != "instance_disabled" {
			t.Fatalf("outcomes=%v, want the input closed as instance_disabled", outcomes)
		}
	})
}

// One input can reach two rules. The one that was served keeps its delivery; only
// the refused one waits, and the served one is not counted again when it is retried.
func TestCustomCapacityRefusalOfOneRuleDoesNotRedeliverToTheOther(t *testing.T) {
	k := newCustomEnv(t)
	k.fillIssuePool(t, 31) // one slot left: the first rule takes it, the second is refused
	first := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`))
	second := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`))
	k.comment(t, "reaches both rules")
	k.drain(t)

	served, refused := first, second
	if n, _ := k.receiptCount(t, first.RuleKey); n == 0 {
		served, refused = second, first
	}
	if n, count := k.receiptCount(t, served.RuleKey); n != 1 || count != 1 {
		t.Fatalf("served rule: receipts=%d events=%d, want one delivery", n, count)
	}
	if n, _ := k.receiptCount(t, refused.RuleKey); n != 0 {
		t.Fatalf("refused rule received %d receipts", n)
	}
	if pending, _, _ := k.inputState(t); pending != 1 {
		t.Fatalf("pending=%d, want the input kept for the refused rule", pending)
	}

	k.freePool(t)
	k.sweep(t)
	k.drain(t)
	if n, count := k.receiptCount(t, refused.RuleKey); n != 1 || count != 1 {
		t.Fatalf("refused rule: receipts=%d events=%d, want it delivered once the pool freed", n, count)
	}
	if n, count := k.receiptCount(t, served.RuleKey); n != 1 || count != 1 {
		t.Fatalf("served rule: receipts=%d events=%d, want its delivery not repeated", n, count)
	}
	if pending, _, outcomes := k.inputState(t); pending != 0 || len(outcomes) != 1 || outcomes[0] != "delivered" {
		t.Fatalf("pending=%d outcomes=%v, want the input handled as delivered", pending, outcomes)
	}
}

// ---- review round 2 (OCR receipt 01a13d45-5620-70c7-82e3-bd05a1628e06) ------

// delayedInputs counts the fixture's pending inputs by whether a retry time still holds them back.
func (k customEnv) delayedInputs(t *testing.T) (delayed, due int) {
	t.Helper()
	delayed = k.f.Count(t, `SELECT count(*) FROM wakeup_scoped_event WHERE workspace_id=$1 AND handled_at IS NULL AND retry_at IS NOT NULL`, k.f.WorkspaceID)
	due = k.f.Count(t, `SELECT count(*) FROM wakeup_scoped_event WHERE workspace_id=$1 AND handled_at IS NULL AND retry_at IS NULL`, k.f.WorkspaceID)
	return delayed, due
}

// A served instance is rebased between attempts by something that is not a
// definition edit (here, the issue's assignee moves: it is part of the
// configuration fingerprint). The retry must still deliver to the refused rule
// only, never again to the rule that already received the event.
func TestCustomCapacityRetryDoesNotRedeliverToAServedInstanceThatWasRebased(t *testing.T) {
	k := newCustomEnv(t)
	k.fillIssuePool(t, 31)
	a := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`))
	b := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`))
	k.comment(t, "reaches both rules; one slot is left")
	k.drain(t)
	served, refused := a, b
	if n, _ := k.receiptCount(t, a.RuleKey); n == 0 {
		served, refused = b, a
	}
	if n, count := k.receiptCount(t, served.RuleKey); n != 1 || count != 1 {
		t.Fatalf("served rule: receipts=%d events=%d, want one delivery", n, count)
	}
	before := k.instanceRow(t, served.RuleKey).Revision

	// The issue's assignee changes; the rule definitions do not.
	k.f.Exec(t, `UPDATE issue SET assignee_type='agent',assignee_id=$2 WHERE id=$1`, k.issue, k.agent)
	k.freePool(t)
	k.sweep(t)
	k.drain(t)

	if after := k.instanceRow(t, served.RuleKey).Revision; after == before {
		t.Fatalf("the served instance was not rebased (revision %d): the test no longer exercises the rebase", after)
	}
	if n, count := k.receiptCount(t, served.RuleKey); n != 1 || count != 1 {
		t.Fatalf("served rule after its rebase: receipts=%d events=%d, want its single delivery, not a second one", n, count)
	}
	if n, count := k.receiptCount(t, refused.RuleKey); n != 1 || count != 1 {
		t.Fatalf("refused rule: receipts=%d events=%d, want it delivered once", n, count)
	}
	if pending, _, outcomes := k.inputState(t); pending != 0 || len(outcomes) != 1 || outcomes[0] != "delivered" {
		t.Fatalf("pending=%d outcomes=%v, want the input handled as delivered", pending, outcomes)
	}
}

// Freeing capacity must not rewrite an issue's whole delayed backlog in one
// transaction: a pass brings a bounded batch forward and the next passes resume.
func TestCustomCapacityBringForwardIsBoundedAndResumable(t *testing.T) {
	k := newCustomEnv(t)
	k.fillIssuePool(t, 32)
	view := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`))
	const events = 120
	k.f.Exec(t, `INSERT INTO comment(issue_id,workspace_id,author_type,author_id,content,type) SELECT $1,$2,'member',$3,'c '||g,'comment' FROM generate_series(1,$4) g`,
		k.issue, k.f.WorkspaceID, k.f.UserID, events)
	for range 6 { // the drain takes 50 inputs a pass; every one is refused and delayed
		k.drain(t)
	}
	if delayed, due := k.delayedInputs(t); delayed != events || due != 0 {
		t.Fatalf("delayed=%d due=%d, want all %d refused inputs held behind a retry time", delayed, due, events)
	}

	k.freePool(t)
	k.sweep(t)
	delayed, due := k.delayedInputs(t)
	if due == 0 || due > 50 {
		t.Fatalf("one pass brought %d inputs forward (delayed %d), want a bounded batch of at most %d", due, delayed, 50)
	}
	for range 10 {
		k.sweep(t)
	}
	if delayed, due := k.delayedInputs(t); delayed != 0 || due != events {
		t.Fatalf("after the passes delayed=%d due=%d, want the backlog resumed to completion", delayed, due)
	}
	for range 6 {
		k.drain(t)
	}
	if n, count := k.receiptCount(t, view.RuleKey); n != 1 || count != events {
		t.Fatalf("receipts=%d events=%d, want all %d events delivered once, coalesced", n, count, events)
	}
}

// An input past retention is not brought forward: it can only expire.
func TestCustomCapacityBringForwardSkipsInputsPastRetention(t *testing.T) {
	k := newCustomEnv(t)
	k.fillIssuePool(t, 32)
	k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`))
	k.comment(t, "waits")
	k.drain(t)
	k.f.Exec(t, `UPDATE wakeup_scoped_event SET captured_at=now()-interval '8 days' WHERE workspace_id=$1`, k.f.WorkspaceID)
	k.freePool(t)
	k.sweep(t)
	k.sweep(t)
	if delayed, due := k.delayedInputs(t); delayed != 1 || due != 0 {
		t.Fatalf("delayed=%d due=%d, want an input past retention left alone for the prune", delayed, due)
	}
}

// The bring-forward pick reads the partial index of delayed inputs, not the
// outbox or the whole workspace, on a fixture where most inputs are not delayed.
func TestCustomCapacityBringForwardQueryUsesTheDelayedIndex(t *testing.T) {
	k := newCustomEnv(t)
	k.f.Cleanup(t, "DELETE FROM wakeup_scoped_event WHERE event_key LIKE 'explain-r2-%'")
	k.f.Exec(t, `INSERT INTO wakeup_scoped_event(workspace_id,issue_id,event_type,event_key,payload,captured_at)
 SELECT gen_random_uuid(),gen_random_uuid(),'comment.created','explain-r2-'||g,'{}'::jsonb,now() FROM generate_series(1,6000) g`)
	k.f.Exec(t, `INSERT INTO wakeup_scoped_event(workspace_id,issue_id,event_type,event_key,payload,captured_at,retry_at)
 SELECT gen_random_uuid(),gen_random_uuid(),'comment.created','explain-r2-d'||g,'{}'::jsonb,now(),now()+interval '1 hour' FROM generate_series(1,20) g`)
	k.f.Exec(t, "ANALYZE wakeup_scoped_event")
	plan := k.plan(t, `SELECT e.id FROM wakeup_scoped_event e
 WHERE e.handled_at IS NULL AND e.retry_at IS NOT NULL AND e.outcome='default_capacity' AND e.captured_at >= now()-interval '7 days'
  AND ($1::uuid[] IS NULL OR e.workspace_id = ANY($1::uuid[]))
  AND NOT EXISTS (SELECT 1 FROM issue_wakeup w WHERE w.issue_id=e.issue_id AND w.capacity_reason IS NOT NULL AND NOT w.enabled)
 ORDER BY e.captured_at, e.id LIMIT 50`, nil)
	if !strings.Contains(plan, "wakeup_scoped_event_delayed_idx") || strings.Contains(plan, "Seq Scan on wakeup_scoped_event") {
		t.Fatalf("the pick must use the delayed-input index:\n%s", plan)
	}
}

// ---- review round 3 (signed HOLD 01a13d8d-8565-715e-acb5-ee622e12a1b0) ------

// A rule that live capture served keeps its served identity when another rule's
// capacity refusal defers the input, so an assignee-driven rebase of the served
// instance does not make the retry deliver the event to it again.
func TestCustomCapacityRetryDoesNotRedeliverToAnInstanceLiveCaptureServed(t *testing.T) {
	k := newCustomEnv(t)
	k.fillIssuePool(t, 31)
	a := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`))
	k.comment(t, "creates A's instance in the last slot")
	k.drain(t)
	if !k.instanceRow(t, a.RuleKey).Enabled {
		t.Fatal("setup: A's instance is not applied")
	}
	b := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`))
	k.comment(t, "A is served live; B has no slot")
	k.drain(t)
	if n, count := k.receiptCount(t, a.RuleKey); n != 1 || count != 2 {
		t.Fatalf("setup: A receipts=%d events=%d, want both events on one receipt", n, count)
	}
	if n, _ := k.receiptCount(t, b.RuleKey); n != 0 {
		t.Fatalf("setup: B received %d receipts while the pool is full", n)
	}
	before := k.instanceRow(t, a.RuleKey).Revision

	k.f.Exec(t, `UPDATE issue SET assignee_type='agent',assignee_id=$2 WHERE id=$1`, k.issue, k.agent)
	k.freePool(t)
	k.sweep(t)
	k.drain(t)

	if after := k.instanceRow(t, a.RuleKey).Revision; after == before {
		t.Fatalf("A was not rebased (revision %d): the test no longer exercises the rebase", after)
	}
	if n, count := k.receiptCount(t, a.RuleKey); n != 1 || count != 2 {
		t.Fatalf("A after its rebase: receipts=%d events=%d, want its two events once, not a replay", n, count)
	}
	if n, count := k.receiptCount(t, b.RuleKey); n != 1 || count != 1 {
		t.Fatalf("B: receipts=%d events=%d, want the one event delivered", n, count)
	}
}

// A delay a database error caused is not a capacity wait: bringing capacity
// deferred inputs forward leaves it to its own backoff.
func TestCustomCapacityBringForwardLeavesADatabaseErrorBackoffAlone(t *testing.T) {
	k := newCustomEnv(t)
	view := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`))
	k.comment(t, "hits a database error")
	heal := k.failInstanceWrites(t)
	_ = k.s.DrainScopedEvents(context.Background(), parseTestUUID(t, k.f.WorkspaceID)) // reports the injected error
	heal()
	if delayed, due := k.delayedInputs(t); delayed != 1 || due != 0 {
		t.Fatalf("delayed=%d due=%d, want the failed input held behind the error backoff", delayed, due)
	}
	k.sweep(t)
	k.sweep(t)
	if delayed, due := k.delayedInputs(t); delayed != 1 || due != 0 {
		t.Fatalf("delayed=%d due=%d after the sweeps, want the error backoff kept (only capacity waits are brought forward)", delayed, due)
	}
	// The backoff ends on its own clock and the input is delivered.
	k.f.Exec(t, `UPDATE wakeup_scoped_event SET retry_at=now()-interval '1 second' WHERE workspace_id=$1 AND handled_at IS NULL`, k.f.WorkspaceID)
	k.drain(t)
	if n, count := k.receiptCount(t, view.RuleKey); n != 1 || count != 1 {
		t.Fatalf("receipts=%d events=%d, want the input delivered once its backoff ended", n, count)
	}
}
