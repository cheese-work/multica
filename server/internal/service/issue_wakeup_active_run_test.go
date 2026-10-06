package service

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// Active-run behavior (CHE-1082 L8): a built-in rule either suppresses facts
// that arrive while its target is running (today's behavior, and the default)
// or defers them until the target is idle.

// runningRun is a started run of the fixture's agent on the issue, as another
// person's or this rule's earlier run would be.
func (e builtinEnv) runningRun(t *testing.T, originator string) string {
	t.Helper()
	return e.f.Task(t, e.agent, testutil.Cols{"issue_id": e.issue, "status": "running", "started_at": testutil.Raw("clock_timestamp()"),
		"runtime_id":         testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + e.agent + "')"),
		"originator_user_id": originator, "accountable_user_id": originator})
}

// unreserved counts the pending facts no run carries: a queued follow-up
// reserves its facts until it starts, so "handed to a run" reads as zero here.
func (e builtinEnv) unreserved(t *testing.T, key string) int {
	t.Helper()
	return e.f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.issue_id=$1 AND w.system_rule=$2 AND r.processed_at IS NULL AND r.task_id IS NULL`, e.issue, key)
}

func (e builtinEnv) endRun(t *testing.T, id string) {
	t.Helper()
	e.f.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=clock_timestamp() WHERE id=$1`, id)
}

// tick is the scheduler's pass over one rule.
func (e builtinEnv) tick(t *testing.T, key string) {
	t.Helper()
	if err := e.s.dispatchSystem(context.Background(), e.rule(t, key)); err != nil {
		t.Fatal(err)
	}
}

func (e builtinEnv) outcomeCount(t *testing.T, key, outcome string) int {
	t.Helper()
	n := 0
	for _, o := range e.outcomes(t, key) {
		if o == outcome {
			n++
		}
	}
	return n
}

func (e builtinEnv) prNote(t *testing.T, number int32) string {
	return fmt.Sprintf("pull/%d", number)
}

// Nothing selects defer implicitly: no definition, an instruction-only
// override, and an explicit suppress all consume a fact that arrives while the
// agent runs, and say so.
func TestActiveRunSuppressStaysTheDefault(t *testing.T) {
	for _, tc := range []struct{ name, fields string }{
		{"legacy, no definition", ``},
		{"instruction-only override", `"instruction":"Look at the merge"`},
		{"explicit suppress", `"active_run":"suppress"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newBuiltinEnv(t)
			if tc.fields != "" {
				e.define(t, WakeupScopeProject, SystemRulePRMerged, tc.fields)
			}
			e.runningRun(t, e.f.UserID)
			e.mergedPR(t, 700)
			if e.tasks(t, SystemRulePRMerged) != 0 || e.pending(t, SystemRulePRMerged) != 0 {
				t.Fatalf("suppress: %d runs, %d pending; want 0 and 0", e.tasks(t, SystemRulePRMerged), e.pending(t, SystemRulePRMerged))
			}
			if e.outcomeCount(t, SystemRulePRMerged, "suppressed_active_run") != 1 {
				t.Fatalf("suppression is not visible: %v", e.outcomes(t, SystemRulePRMerged))
			}
		})
	}
}

// child_done has never gated on a running run; suppress keeps it that way.
func TestActiveRunSuppressLeavesChildDoneUngated(t *testing.T) {
	e := newBuiltinEnv(t)
	e.f.Cleanup(t, "DELETE FROM issue_child_event WHERE parent_id=$1", e.issue)
	child := e.f.Issue(t, "child", testutil.Cols{"parent_issue_id": e.issue, "status": "in_progress"})
	if err := e.s.ProcessChildEvents(context.Background(), e.issue); err != nil {
		t.Fatal(err)
	}
	e.runningRun(t, e.f.UserID)
	e.f.Exec(t, "UPDATE issue SET status='done' WHERE id=$1", child)
	if err := e.s.ProcessChildEvents(context.Background(), e.issue); err != nil {
		t.Fatal(err)
	}
	if e.tasks(t, SystemRuleChildDone) != 1 {
		t.Fatalf("child_done under suppress ran %d times, want 1", e.tasks(t, SystemRuleChildDone))
	}
}

// Defer keeps the facts while the agent runs, adds nothing while it still
// runs, and starts exactly one run, carrying every retained fact, once it is
// idle. A replay of the same events starts nothing more.
func TestActiveRunDeferRetainsThenFollowsUpOnce(t *testing.T) {
	e := newBuiltinEnv(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"active_run":"defer"`)
	run := e.runningRun(t, e.f.UserID)
	startedPrompt := e.f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE id=$1 AND handoff_note IS NULL`, run)

	e.mergedPR(t, 701)
	e.mergedPR(t, 702)
	e.tick(t, SystemRulePRMerged)
	e.tick(t, SystemRulePRMerged)
	if e.tasks(t, SystemRulePRMerged) != 0 || e.pending(t, SystemRulePRMerged) != 2 {
		t.Fatalf("while running: %d runs, %d pending; want 0 and 2", e.tasks(t, SystemRulePRMerged), e.pending(t, SystemRulePRMerged))
	}
	if got := e.outcomeCount(t, SystemRulePRMerged, "deferred_active_run"); got != 2 {
		t.Fatalf("deferral is visible %d times, want once per fact (2): %v", got, e.outcomes(t, SystemRulePRMerged))
	}
	// A running prompt is immutable.
	if got := e.f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE id=$1 AND handoff_note IS NULL`, run); got != startedPrompt {
		t.Fatal("a fact that arrived mid-run edited the running prompt")
	}

	e.endRun(t, run)
	e.tick(t, SystemRulePRMerged)
	if e.tasks(t, SystemRulePRMerged) != 1 || e.unreserved(t, SystemRulePRMerged) != 0 {
		t.Fatalf("at idle: %d runs, %d unreserved; want 1 and 0", e.tasks(t, SystemRulePRMerged), e.unreserved(t, SystemRulePRMerged))
	}
	note := e.lastTask(t, SystemRulePRMerged).HandoffNote.String
	for _, number := range []int32{701, 702} {
		if !strings.Contains(note, e.prNote(t, number)) {
			t.Fatalf("the follow-up lacks PR %d: %q", number, note)
		}
	}
	// Ticks and event redelivery after the follow-up add nothing.
	e.tick(t, SystemRulePRMerged)
	e.mergedPR(t, 701)
	e.tick(t, SystemRulePRMerged)
	if e.tasks(t, SystemRulePRMerged) != 1 {
		t.Fatalf("a replay started %d runs in all, want 1", e.tasks(t, SystemRulePRMerged))
	}
}

// Whatever a rule inherits, defer is a choice made by a definition: an
// instruction-only override keeps the ancestor's choice, a later scope may set
// suppress, and clearing the field returns to the default.
func TestActiveRunDeferIsInheritedAndOverridable(t *testing.T) {
	deferred := func(t *testing.T, e builtinEnv) bool {
		t.Helper()
		e.runningRun(t, e.f.UserID)
		e.mergedPR(t, 710)
		return e.pending(t, SystemRulePRMerged) == 1
	}
	t.Run("inherited through an instruction-only override", func(t *testing.T) {
		e := newBuiltinEnv(t)
		e.define(t, WakeupScopeWorkspace, SystemRulePRMerged, `"active_run":"defer"`)
		e.define(t, WakeupScopeProject, SystemRulePRMerged, `"instruction":"Handle the merge"`)
		if !deferred(t, e) {
			t.Fatal("an instruction-only project override dropped the workspace's defer")
		}
	})
	t.Run("overridden by a later scope", func(t *testing.T) {
		e := newBuiltinEnv(t)
		e.define(t, WakeupScopeWorkspace, SystemRulePRMerged, `"active_run":"defer"`)
		e.define(t, WakeupScopeProject, SystemRulePRMerged, `"active_run":"suppress"`)
		if deferred(t, e) {
			t.Fatal("a project suppress did not override the workspace defer")
		}
	})
	t.Run("cleared", func(t *testing.T) {
		e := newBuiltinEnv(t)
		e.define(t, WakeupScopeWorkspace, SystemRulePRMerged, `"active_run":"defer"`)
		e.define(t, WakeupScopeProject, SystemRulePRMerged, `"active_run":null`)
		if deferred(t, e) {
			t.Fatal("a cleared active_run did not return to suppress")
		}
	})
}

// A value that is neither choice holds the rule, as any stored configuration
// this build cannot read does; the write path refuses it before it is stored.
func TestActiveRunUnknownChoiceIsRefusedAndHeld(t *testing.T) {
	e := newBuiltinEnv(t)
	ref := WakeupScopeRef{Kind: WakeupScopeProject, ID: e.project, WorkspaceID: parseTestUUID(t, e.f.WorkspaceID)}
	_, err := e.s.SaveWakeupDefinition(context.Background(), ref, e.owner, WakeupDefinitionWrite{RuleKey: SystemRulePRMerged, Patch: patchOf(t, `"active_run":"queue"`)})
	if err == nil || !strings.Contains(err.Error(), "active_run") {
		t.Fatalf("an unknown active_run was accepted or unnamed: %v", err)
	}
	if err := insertDefinition(t, e.f, "project", util.UUIDToString(e.project), SystemRulePRMerged, false, `{"v":1,"active_run":"queue"}`); err != nil {
		t.Fatal(err)
	}
	e.mergedPR(t, 715)
	if e.tasks(t, SystemRulePRMerged) != 0 || e.pending(t, SystemRulePRMerged) != 1 {
		t.Fatalf("held rule: %d runs, %d pending; want 0 and 1", e.tasks(t, SystemRulePRMerged), e.pending(t, SystemRulePRMerged))
	}
}

// Defer applies to every built-in: child_done facts that arrive while the
// agent runs wait too. A closing the agent's own run made is the self-actor
// case: it is acknowledged and consumed, not retained; one from anyone else is
// retained and followed up at idle.
func TestActiveRunDeferChildDoneAndSelfAcknowledgement(t *testing.T) {
	setup := func(t *testing.T) (builtinEnv, string) {
		e := newBuiltinEnv(t)
		e.f.Cleanup(t, "DELETE FROM issue_child_event WHERE parent_id=$1", e.issue)
		e.define(t, WakeupScopeProject, SystemRuleChildDone, `"active_run":"defer"`)
		child := e.f.Issue(t, "child", testutil.Cols{"parent_issue_id": e.issue, "status": "in_progress"})
		if err := e.s.ProcessChildEvents(context.Background(), e.issue); err != nil {
			t.Fatal(err)
		}
		return e, child
	}
	closeAs := func(t *testing.T, e builtinEnv, child, run string) {
		t.Helper()
		ctx := context.Background()
		tx, err := e.f.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if run != "" {
			if _, err = tx.Exec(ctx, "SELECT set_config('multica.source_task_id',$1,true),set_config('multica.actor_type','agent',true),set_config('multica.actor_id',$2,true)", run, e.agent); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = tx.Exec(ctx, "UPDATE issue SET status='done' WHERE id=$1", child); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := e.s.ProcessChildEvents(ctx, e.issue); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("closed by the agent's own run", func(t *testing.T) {
		e, child := setup(t)
		run := e.runningRun(t, e.f.UserID)
		closeAs(t, e, child, run)
		if e.tasks(t, SystemRuleChildDone) != 0 || e.pending(t, SystemRuleChildDone) != 0 || e.outcomeCount(t, SystemRuleChildDone, wakeupOutcomeAcknowledged) != 1 {
			t.Fatalf("self-closed: %d runs, %d pending, outcomes %v; want acknowledged and consumed", e.tasks(t, SystemRuleChildDone), e.pending(t, SystemRuleChildDone), e.outcomes(t, SystemRuleChildDone))
		}
	})
	t.Run("closed by someone else", func(t *testing.T) {
		e, child := setup(t)
		run := e.runningRun(t, e.f.UserID)
		closeAs(t, e, child, "")
		if e.tasks(t, SystemRuleChildDone) != 0 || e.pending(t, SystemRuleChildDone) == 0 {
			t.Fatalf("external closing: %d runs, %d pending; want it retained while the agent runs", e.tasks(t, SystemRuleChildDone), e.pending(t, SystemRuleChildDone))
		}
		e.endRun(t, run)
		e.tick(t, SystemRuleChildDone)
		if e.tasks(t, SystemRuleChildDone) != 1 || e.unreserved(t, SystemRuleChildDone) != 0 {
			t.Fatalf("at idle: %d runs, %d unreserved; want 1 and 0", e.tasks(t, SystemRuleChildDone), e.unreserved(t, SystemRuleChildDone))
		}
	})
}

// Facts of different rules, and a run of another originator, are never merged
// or consumed together: each rule keeps its own facts and starts its own run as
// the rule's own originator.
func TestActiveRunDeferIsolatesRulesAndOriginators(t *testing.T) {
	e := newBuiltinEnv(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"active_run":"defer"`)
	e.define(t, WakeupScopeProject, SystemRulePRChecksFailed, `"active_run":"defer"`)
	stranger := e.f.member(t, "active-run-stranger")
	run := e.runningRun(t, stranger)
	e.mergedPR(t, 720)
	e.failedChecks(t, "iso-head", "FAILURE")
	if e.pending(t, SystemRulePRMerged) != 1 || e.pending(t, SystemRulePRChecksFailed) != 1 {
		t.Fatalf("pending merged=%d checks=%d; want 1 each, nothing consumed by the other's run", e.pending(t, SystemRulePRMerged), e.pending(t, SystemRulePRChecksFailed))
	}
	e.endRun(t, run)
	e.tick(t, SystemRulePRMerged)
	merged := e.lastTask(t, SystemRulePRMerged)
	if strings.Contains(merged.HandoffNote.String, "pull/300") || !strings.Contains(merged.HandoffNote.String, e.prNote(t, 720)) {
		t.Fatalf("the merged run carries another rule's facts: %q", merged.HandoffNote.String)
	}
	if util.UUIDToString(merged.OriginatorUserID) == stranger {
		t.Fatal("the follow-up ran as the originator of the run that was active")
	}
	// The checks rule is still waiting for its own pass: the merged run is
	// now a queued same-agent run, which holds its facts rather than consuming them.
	e.tick(t, SystemRulePRChecksFailed)
	if e.pending(t, SystemRulePRChecksFailed) != 1 {
		t.Fatalf("the checks rule's fact was consumed with the merged rule's: pending %d", e.pending(t, SystemRulePRChecksFailed))
	}
}

// Retained facts do not wait for ever: past the retention window they are
// dropped with a visible outcome, not delivered late and not lost silently.
func TestActiveRunDeferRetentionExpiryIsVisible(t *testing.T) {
	e := newBuiltinEnv(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"active_run":"defer"`)
	e.runningRun(t, e.f.UserID)
	e.mergedPR(t, 730)
	e.f.Exec(t, `UPDATE issue_wakeup_receipt r SET created_at=now()-interval '25 hours' FROM issue_wakeup w WHERE w.id=r.wakeup_id AND w.issue_id=$1`, e.issue)
	e.mergedPR(t, 731)
	e.tick(t, SystemRulePRMerged)
	if e.pending(t, SystemRulePRMerged) != 1 || e.outcomeCount(t, SystemRulePRMerged, "defer_expired") != 1 {
		t.Fatalf("pending %d, outcomes %v; want the stale fact expired visibly and the fresh one kept", e.pending(t, SystemRulePRMerged), e.outcomes(t, SystemRulePRMerged))
	}
}

// A configuration edit retires facts held under the old one, as it retires any
// pending fact (L5): nothing is delivered later for them.
func TestActiveRunDeferFactsAreRetiredByAConfigurationEdit(t *testing.T) {
	e := newBuiltinEnv(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"active_run":"defer"`)
	run := e.runningRun(t, e.f.UserID)
	e.mergedPR(t, 740)
	if e.pending(t, SystemRulePRMerged) != 1 {
		t.Fatalf("the fact was not held while the agent ran: %d pending", e.pending(t, SystemRulePRMerged))
	}
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"active_run":"defer","instruction":"Edited"`)
	e.endRun(t, run)
	e.tick(t, SystemRulePRMerged)
	if e.tasks(t, SystemRulePRMerged) != 0 || e.pending(t, SystemRulePRMerged) != 0 {
		t.Fatalf("after an edit: %d runs, %d pending; want the held fact retired", e.tasks(t, SystemRulePRMerged), e.pending(t, SystemRulePRMerged))
	}
}

// The follow-up goes through the gates a fresh fact does. The hourly rate and
// an aggregate counter delay it; neither discards it.
func TestActiveRunDeferFollowUpRespectsRateAndAggregateGates(t *testing.T) {
	t.Run("rate", func(t *testing.T) {
		e := newBuiltinEnv(t)
		e.define(t, WakeupScopeProject, SystemRulePRMerged, `"active_run":"defer","rate_limit":1`)
		e.mergedPR(t, 750)
		e.finishTasks(t)
		run := e.runningRun(t, e.f.UserID)
		e.mergedPR(t, 751)
		e.endRun(t, run)
		e.tick(t, SystemRulePRMerged)
		w := e.rule(t, SystemRulePRMerged)
		if e.tasks(t, SystemRulePRMerged) != 1 || e.pending(t, SystemRulePRMerged) != 1 || w.PausedReason.String != wakeupPausedRate {
			t.Fatalf("over the rate: %d runs, %d pending, pause %q; want the follow-up delayed, not discarded", e.tasks(t, SystemRulePRMerged), e.pending(t, SystemRulePRMerged), w.PausedReason.String)
		}
		e.f.Exec(t, `UPDATE agent_task_queue SET created_at=now()-interval '2 hours' WHERE issue_id=$1 AND context->>'wakeup_system'='pr_merged'`, e.issue)
		e.tick(t, SystemRulePRMerged)
		if e.tasks(t, SystemRulePRMerged) != 2 || e.unreserved(t, SystemRulePRMerged) != 0 {
			t.Fatalf("after the window: %d runs, %d unreserved; want the delayed follow-up to run", e.tasks(t, SystemRulePRMerged), e.unreserved(t, SystemRulePRMerged))
		}
	})
	t.Run("aggregate", func(t *testing.T) {
		e := newBuiltinEnv(t)
		e.cleanAggregate(t)
		e.define(t, WakeupScopeProject, SystemRulePRMerged, `"active_run":"defer","aggregate_limit":1`)
		other := e.issueIn(t, e.project)
		e.mergedOn(t, other, 760) // takes the only slot
		run := e.runningRun(t, e.f.UserID)
		e.mergedPR(t, 761)
		e.endRun(t, run)
		e.tick(t, SystemRulePRMerged)
		if e.runs(t, e.issue) != 0 || e.pendingOn(t, e.issue) != 1 {
			t.Fatalf("over the counter: %d runs, %d pending; want the follow-up delayed, not discarded", e.runs(t, e.issue), e.pendingOn(t, e.issue))
		}
		e.cancelRuns(t, other)
		e.redispatch(t, e.issue)
		if e.runs(t, e.issue) != 1 || e.unreserved(t, SystemRulePRMerged) != 0 {
			t.Fatalf("after a slot freed: %d runs, %d unreserved; want 1 and 0", e.runs(t, e.issue), e.unreserved(t, SystemRulePRMerged))
		}
	})
}

// A target with a run waiting to start still holds the facts for that run; an
// idle target with none starts one. Both are unchanged by the choice.
func TestActiveRunDeferDoesNotConsumeForAnyAgentRun(t *testing.T) {
	e := newBuiltinEnv(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"active_run":"defer"`)
	// A run of another agent on the issue is not the target's.
	other := e.f.privateAgentOwnedBy(t, e.f.UserID, "active-run-other-agent")
	e.f.Task(t, other, testutil.Cols{"issue_id": e.issue, "status": "running", "started_at": testutil.Raw("clock_timestamp()"),
		"runtime_id": testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + other + "')")})
	e.mergedPR(t, 770)
	if e.tasks(t, SystemRulePRMerged) != 1 || e.unreserved(t, SystemRulePRMerged) != 0 {
		t.Fatalf("another agent's run held the fact: %d runs, %d unreserved; want 1 and 0", e.tasks(t, SystemRulePRMerged), e.unreserved(t, SystemRulePRMerged))
	}
}

// ---- review corrections (CHE-1195 signing review of 921d07b2) ---------------

func (e builtinEnv) reservedFor(t *testing.T, key string, task pgtype.UUID) int {
	t.Helper()
	return e.f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.issue_id=$1 AND w.system_rule=$2 AND r.task_id=$3 AND r.processed_at IS NULL`, e.issue, key, task)
}

// An unstarted follow-up only reserves the facts it carries. Cancelled before
// it starts, it gives them back and a replacement attempt carries them.
func TestActiveRunDeferFollowUpKeepsFactsUntilItStarts(t *testing.T) {
	e := newBuiltinEnv(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"active_run":"defer"`)
	run := e.runningRun(t, e.f.UserID)
	e.mergedPR(t, 880)
	e.endRun(t, run)
	e.tick(t, SystemRulePRMerged)
	followup := e.lastTask(t, SystemRulePRMerged)
	if followup.Status != "queued" || e.reservedFor(t, SystemRulePRMerged, followup.ID) != 1 || e.pending(t, SystemRulePRMerged) != 1 {
		t.Fatalf("queued follow-up: status %s, %d reserved, %d pending; want the fact reserved, not consumed", followup.Status, e.reservedFor(t, SystemRulePRMerged, followup.ID), e.pending(t, SystemRulePRMerged))
	}
	if n := e.rule(t, SystemRulePRMerged).FireCount; n != 0 {
		t.Fatalf("fire_count %d before the follow-up started, want 0", n)
	}
	if _, err := e.s.Tasks.CancelTaskWithReason(context.Background(), followup.ID, "failed before start", "test_before_start"); err != nil {
		t.Fatal(err)
	}
	e.tick(t, SystemRulePRMerged)
	again := e.lastTask(t, SystemRulePRMerged)
	if again.ID == followup.ID || e.reservedFor(t, SystemRulePRMerged, again.ID) != 1 || !strings.Contains(again.HandoffNote.String, e.prNote(t, 880)) {
		t.Fatalf("no replacement attempt carries the fact: last task %s, %d reserved", util.UUIDToString(again.ID), e.reservedFor(t, SystemRulePRMerged, again.ID))
	}
}

// Claim and start settle the reservation once: the fact is consumed with the
// run that started, the firing counts once, and replays add nothing.
func TestActiveRunDeferFollowUpSettlesOnceAtStart(t *testing.T) {
	e := newBuiltinEnv(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"active_run":"defer"`)
	run := e.runningRun(t, e.f.UserID)
	e.mergedPR(t, 890)
	e.endRun(t, run)
	e.tick(t, SystemRulePRMerged)
	followup := e.lastTask(t, SystemRulePRMerged)
	id := util.UUIDToString(followup.ID)
	// The claim does not release the rule's own reservation.
	wakeClaim(t, e.f, e.s, id)
	if e.reservedFor(t, SystemRulePRMerged, followup.ID) != 1 {
		t.Fatal("claiming the follow-up released the facts it carries")
	}
	wakeStart(t, e.f, id)
	e.tick(t, SystemRulePRMerged)
	e.tick(t, SystemRulePRMerged)
	if e.pending(t, SystemRulePRMerged) != 0 || e.f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.issue_id=$1 AND r.task_id=$2 AND r.processed_at IS NOT NULL`, e.issue, followup.ID) != 1 {
		t.Fatalf("after start: %d pending; want the fact consumed with the started run", e.pending(t, SystemRulePRMerged))
	}
	if n := e.rule(t, SystemRulePRMerged).FireCount; n != 1 {
		t.Fatalf("fire_count %d after start and replays, want 1", n)
	}
	if e.tasks(t, SystemRulePRMerged) != 1 || e.outcomeCount(t, SystemRulePRMerged, wakeupOutcomeMerged) != 1 {
		t.Fatalf("%d runs, outcomes %v; want one run and one settlement", e.tasks(t, SystemRulePRMerged), e.outcomes(t, SystemRulePRMerged))
	}
}

// A fact that arrives while the follow-up waits to start joins it (the
// follow-up is the same rule's run) and is reserved with it, not consumed.
func TestActiveRunDeferFollowUpExtensionReservesTheNewFact(t *testing.T) {
	e := newBuiltinEnv(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"active_run":"defer"`)
	run := e.runningRun(t, e.f.UserID)
	e.mergedPR(t, 895)
	e.endRun(t, run)
	e.tick(t, SystemRulePRMerged)
	e.mergedPR(t, 896)
	followup := e.lastTask(t, SystemRulePRMerged)
	if e.tasks(t, SystemRulePRMerged) != 1 || e.reservedFor(t, SystemRulePRMerged, followup.ID) != 2 || !strings.Contains(followup.HandoffNote.String, e.prNote(t, 896)) {
		t.Fatalf("%d runs, %d reserved; want one follow-up carrying both facts", e.tasks(t, SystemRulePRMerged), e.reservedFor(t, SystemRulePRMerged, followup.ID))
	}
	if _, err := e.s.Tasks.CancelTaskWithReason(context.Background(), followup.ID, "failed before start", "test_before_start"); err != nil {
		t.Fatal(err)
	}
	e.tick(t, SystemRulePRMerged)
	if e.pending(t, SystemRulePRMerged) != 2 {
		t.Fatalf("%d pending after the cancel, want both facts back", e.pending(t, SystemRulePRMerged))
	}
}

// max_fires counts a deferred follow-up when it starts, and the held slot keeps
// later facts from passing it meanwhile.
func TestActiveRunDeferFollowUpCountsAgainstMaxFiresAtStart(t *testing.T) {
	e := newBuiltinEnv(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"active_run":"defer","max_fires":1`)
	run := e.runningRun(t, e.f.UserID)
	e.mergedPR(t, 897)
	e.endRun(t, run)
	e.tick(t, SystemRulePRMerged)
	followup := e.lastTask(t, SystemRulePRMerged)
	if w := e.rule(t, SystemRulePRMerged); w.PausedReason.Valid || w.FireCount != 0 || e.reservedFor(t, SystemRulePRMerged, followup.ID) != 1 {
		t.Fatalf("before start: pause %q, fire_count %d, %d reserved; want the fact kept until start", w.PausedReason.String, w.FireCount, e.reservedFor(t, SystemRulePRMerged, followup.ID))
	}
	wakeStart(t, e.f, util.UUIDToString(followup.ID))
	e.tick(t, SystemRulePRMerged)
	if w := e.rule(t, SystemRulePRMerged); w.FireCount != 1 || w.PausedReason.String != wakeupPausedMaxFires {
		t.Fatalf("after start: fire_count %d, pause %q; want 1 and the max_fires ending", w.FireCount, w.PausedReason.String)
	}
}

// Rules never merge: a follow-up of one rule does not carry another rule's
// facts at claim, and the other rule does not consume with it at start.
func TestActiveRunDeferDifferentRulesStaySeparateThroughClaimAndStart(t *testing.T) {
	for _, tc := range []struct {
		name         string
		checksFields string
	}{
		{"both defer", `"active_run":"defer"`},
		{"joiner suppresses", `"instruction":"Fix the build"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newBuiltinEnv(t)
			e.define(t, WakeupScopeProject, SystemRulePRMerged, `"active_run":"defer"`)
			e.define(t, WakeupScopeProject, SystemRulePRChecksFailed, tc.checksFields)
			run := e.runningRun(t, e.f.UserID)
			e.mergedPR(t, 881)
			e.endRun(t, run)
			e.tick(t, SystemRulePRMerged)
			merged := e.lastTask(t, SystemRulePRMerged)
			// The checks fact arrives while the merged follow-up waits to start.
			e.failedChecks(t, "isolation-head", "FAILURE")
			e.tick(t, SystemRulePRChecksFailed)
			if joined := wakeClaim(t, e.f, e.s, util.UUIDToString(merged.ID)); strings.Contains(joined, "isolation-head") {
				t.Fatalf("checks facts joined the merged follow-up: %s", joined)
			}
			wakeStart(t, e.f, util.UUIDToString(merged.ID))
			e.tick(t, SystemRulePRChecksFailed)
			if got := e.f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.issue_id=$1 AND w.system_rule=$2 AND r.task_id=$3`, e.issue, SystemRulePRChecksFailed, merged.ID); got != 0 {
				t.Fatalf("the checks rule reserved or consumed %d receipts with the merged run", got)
			}
			if e.pending(t, SystemRulePRChecksFailed) != 1 {
				t.Fatalf("the checks fact is not pending: %d", e.pending(t, SystemRulePRChecksFailed))
			}
			// Once the merged run is over the checks rule makes its own attempt.
			e.endRun(t, util.UUIDToString(merged.ID))
			e.tick(t, SystemRulePRChecksFailed)
			if e.tasks(t, SystemRulePRChecksFailed) != 1 || strings.Contains(e.lastTask(t, SystemRulePRChecksFailed).HandoffNote.String, "pull/881") {
				t.Fatalf("checks rule: %d runs; want its own run without the merged fact", e.tasks(t, SystemRulePRChecksFailed))
			}
		})
	}
}

// Ordinary carriers keep their contract: a plain waiting run still takes the
// facts of two different deferring rules, as it does for legacy rules.
func TestActiveRunDeferStillJoinsAnOrdinaryWaitingRun(t *testing.T) {
	e := newBuiltinEnv(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"active_run":"defer"`)
	waiting := wakeWaitingRun(t, e.f, e.issue, e.agent, e.f.UserID)
	e.mergedPR(t, 898)
	if joined := wakeClaim(t, e.f, e.s, waiting); !strings.Contains(joined, "pull/898") {
		t.Fatalf("an ordinary waiting run did not carry the fact: %q", joined)
	}
}

// Every expired fact is accounted for, once, with its receipt.
func TestActiveRunDeferBatchExpiryAccountsForEveryFact(t *testing.T) {
	e := newBuiltinEnv(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"active_run":"defer"`)
	e.runningRun(t, e.f.UserID)
	e.mergedPR(t, 883)
	e.mergedPR(t, 884)
	e.f.Exec(t, `UPDATE issue_wakeup_receipt r SET created_at=now()-interval '25 hours' FROM issue_wakeup w WHERE w.id=r.wakeup_id AND w.issue_id=$1`, e.issue)
	e.tick(t, SystemRulePRMerged)
	e.tick(t, SystemRulePRMerged)
	if e.pending(t, SystemRulePRMerged) != 0 || e.outcomeCount(t, SystemRulePRMerged, "defer_expired") != 2 {
		t.Fatalf("%d pending, outcomes %v; want both facts expired with one account each", e.pending(t, SystemRulePRMerged), e.outcomes(t, SystemRulePRMerged))
	}
	var withIDs, withPRs int
	e.f.QueryRow(t, `SELECT count(*) FILTER (WHERE details->>'receipt_id' IS NOT NULL), count(DISTINCT details->>'pr_number') FROM activity_log WHERE issue_id=$1 AND details->>'outcome'='defer_expired'`, e.issue).Scan(&withIDs, &withPRs)
	if withIDs != 2 || withPRs != 2 {
		t.Fatalf("expiry accounts: %d with a receipt id, %d distinct PRs; want 2 and 2", withIDs, withPRs)
	}
}
