package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// Execution of custom event and condition rules through dispatch, join, claim
// and start (CHE-1082 L10).

func TestCustomConditionRuleActsOnFactsAlreadyTrueWhenItIsNew(t *testing.T) {
	k := newCustomEnv(t)
	view := k.mustSave(t, WakeupScopeProject, "", k.conditionRule(statusIsTodo))
	k.sweepAll(t)
	k.wantRuns(t, view.RuleKey, 0)
	k.tick(t)
	runs := k.wantRuns(t, view.RuleKey, 1)
	if got := util.UUIDToString(runs[0].OriginatorUserID); got != k.owner || util.UUIDToString(runs[0].AgentID) != k.agent {
		t.Fatalf("run = originator %s agent %s, want the authorizing member %s and the named agent %s", got, util.UUIDToString(runs[0].AgentID), k.owner, k.agent)
	}
	if !strings.Contains(runs[0].HandoffNote.String, "condition.met") {
		t.Fatalf("handoff note lacks the trigger fact: %q", runs[0].HandoffNote.String)
	}
	k.finish(t)
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 1) // the same facts never fire twice
}

func TestCustomEventRuleRunsOnTheFirstMatchingEventAndNotOnHistory(t *testing.T) {
	k := newCustomEnv(t)
	k.comment(t, "history before the rule exists")
	view := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`))
	k.sweepAll(t)
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 0)
	if n := k.f.Count(t, `SELECT count(*) FROM wakeup_scoped_event WHERE workspace_id=$1`, k.f.WorkspaceID); n != 0 {
		t.Fatalf("historic comments were replayed into the outbox: %d rows", n)
	}
	k.comment(t, "the first matching event")
	k.tick(t)
	runs := k.wantRuns(t, view.RuleKey, 1)
	if util.UUIDToString(runs[0].OriginatorUserID) != k.owner {
		t.Fatalf("run attributed to %s, want the authorizing member %s", util.UUIDToString(runs[0].OriginatorUserID), k.owner)
	}
}

func TestCustomOnceRuleRunsOnceAndAnEditNeverRearmsIt(t *testing.T) {
	k := newCustomEnv(t)
	view := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`)+`,"mode":"once"`)
	k.comment(t, "one")
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 1)
	k.finish(t)
	k.comment(t, "two")
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 1)
	// A different mode on an ancestor does not rearm the consumed instance.
	k.mustSave(t, WakeupScopeWorkspace, view.RuleKey, k.eventRule(`["comment.created"]`)+`,"mode":"continuous"`)
	k.comment(t, "three")
	k.tick(t)
	k.comment(t, "four")
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 1)
	if w := k.instanceRow(t, view.RuleKey); w.Enabled || !w.PausedReason.Valid {
		t.Fatalf("instance = enabled %v paused %v, want it to rest as consumed", w.Enabled, w.PausedReason)
	}
}

func TestCustomMaxFiresPausesAndAnEditNeverClearsThePause(t *testing.T) {
	k := newCustomEnv(t)
	view := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`)+`,"max_fires":2,"active_run":"suppress"`)
	for i := range 3 {
		k.comment(t, "event "+string(rune('a'+i)))
		k.tick(t)
		k.finish(t)
	}
	k.wantRuns(t, view.RuleKey, 2)
	if w := k.instanceRow(t, view.RuleKey); w.PausedReason.String != wakeupPausedMaxFires || w.FireCount != 2 {
		t.Fatalf("instance paused %q after %d fires, want max_fires after 2", w.PausedReason.String, w.FireCount)
	}
	k.mustSave(t, WakeupScopeWorkspace, view.RuleKey, k.eventRule(`["comment.created"]`)+`,"max_fires":50,"active_run":"suppress"`)
	k.comment(t, "after the edit")
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 2)
	if w := k.instanceRow(t, view.RuleKey); !w.PausedReason.Valid {
		t.Fatal("an ancestor edit cleared a safety pause")
	}
}

func TestCustomQueuedRunOfAnOldConfigurationIsNotClaimable(t *testing.T) {
	k := newCustomEnv(t)
	view := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`))
	k.comment(t, "fires")
	k.tick(t)
	run := k.wantRuns(t, view.RuleKey, 1)[0]
	if err := k.s.CheckClaim(context.Background(), run); err != nil {
		t.Fatalf("a current run must be claimable: %v", err)
	}
	k.mustSave(t, WakeupScopeWorkspace, view.RuleKey, strings.Replace(k.eventRule(`["comment.created"]`), "look at it", "a new instruction", 1))
	if err := k.s.CheckClaim(context.Background(), run); !errors.Is(err, ErrWakeupForbidden) {
		t.Fatalf("a run of the old configuration: %v, want it refused at claim", err)
	}
	k.comment(t, "a later event")
	k.tick(t)
	if got := k.ruleTasks(t, view.RuleKey); got[0].Status != "cancelled" {
		t.Fatalf("the old run is %s, want it withdrawn when the instance moved to the new configuration", got[0].Status)
	}
}

// running starts a run of the rule's agent on the issue, as a person's own
// request would.
func (k customEnv) running(t *testing.T) string {
	t.Helper()
	id := k.f.Task(t, k.agent, testutil.Cols{"issue_id": k.issue, "runtime_id": testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + k.agent + "')"),
		"originator_user_id": k.owner, "accountable_user_id": k.owner, "status": "running", "started_at": testutil.Raw("clock_timestamp()")})
	return id
}

func TestCustomRuleDefersFactsWhileTheTargetRunsAndFollowsUpOnce(t *testing.T) {
	k := newCustomEnv(t)
	view := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`))
	busy := k.running(t)
	k.comment(t, "arrives while the target runs")
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 0)
	if w := k.instanceRow(t, view.RuleKey); k.f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1 AND processed_at IS NULL`, w.ID) != 1 {
		t.Fatal("the deferred fact was not kept pending")
	}
	k.f.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=clock_timestamp() WHERE id=$1`, busy)
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 1)
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 1) // exactly one follow-up
}

func TestCustomRuleThatSuppressesDropsFactsThatArriveDuringARun(t *testing.T) {
	k := newCustomEnv(t)
	view := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`)+`,"active_run":"suppress"`)
	k.running(t)
	k.comment(t, "arrives while the target runs")
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 0)
	if w := k.instanceRow(t, view.RuleKey); k.f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1 AND processed_at IS NULL`, w.ID) != 0 {
		t.Fatal("a suppressed fact stayed pending")
	}
}

func TestCustomAggregateCapDelaysTheStartsOfAllIssuesOfARule(t *testing.T) {
	k := newCustomEnv(t)
	second := k.f.Issue(t, "second", testutil.Cols{"project_id": k.project})
	third := k.f.Issue(t, "third", testutil.Cols{"project_id": k.project})
	k.f.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id=ANY($1)", []string{second, third})
	view := k.mustSave(t, WakeupScopeProject, "", k.eventRule(`["comment.created"]`)+`,"aggregate_limit":2,"active_run":"suppress"`)
	for _, issue := range []string{k.issue, second, third} {
		k.f.Comment(t, issue, "event")
	}
	k.tick(t)
	if got := k.f.Count(t, `SELECT count(*) FROM agent_task_queue t JOIN issue_wakeup w ON w.id::text=t.context->>'wakeup_id' WHERE w.default_rule_key=$1`, view.RuleKey); got != 2 {
		t.Fatalf("%d runs started, want the cap of 2", got)
	}
	var delayed int
	if err := k.f.Pool.QueryRow(context.Background(), `SELECT count(*) FROM issue_wakeup WHERE default_rule_key=$1 AND aggregate_retry_at IS NOT NULL AND aggregate_blocked_scope_kind='project'`, view.RuleKey).Scan(&delayed); err != nil || delayed != 1 {
		t.Fatalf("delayed instances = %d (%v), want one that names the counter holding it back", delayed, err)
	}
	// Its fact is kept, not dropped.
	if n := k.f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.default_rule_key=$1 AND r.processed_at IS NULL`, view.RuleKey); n != 1 {
		t.Fatalf("%d facts pending, want the delayed instance's one", n)
	}
	// Room frees: the scheduler retries and starts it.
	k.f.Exec(t, `DELETE FROM wakeup_aggregate_reservation WHERE workspace_id=$1`, k.f.WorkspaceID)
	k.f.Exec(t, `UPDATE issue_wakeup SET aggregate_retry_at=now()-interval '1 second' WHERE default_rule_key=$1 AND aggregate_retry_at IS NOT NULL`, view.RuleKey)
	k.tick(t)
	if got := k.f.Count(t, `SELECT count(*) FROM agent_task_queue t JOIN issue_wakeup w ON w.id::text=t.context->>'wakeup_id' WHERE w.default_rule_key=$1`, view.RuleKey); got != 3 {
		t.Fatalf("%d runs after the retry, want all 3", got)
	}
}

func TestCustomRuleFiltersByLabelAndPriority(t *testing.T) {
	k := newCustomEnv(t)
	view := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`)+`,"filters":{"priorities":["high"]}`)
	k.comment(t, "the issue is not high priority")
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 0)
	k.f.Exec(t, `UPDATE issue SET priority='high' WHERE id=$1`, k.issue)
	k.comment(t, "now it is")
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 1)
}

func TestCustomRuleEndsAtItsExpiry(t *testing.T) {
	k := newCustomEnv(t)
	view := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`)+`,"expiry":{"after_seconds":3600},"active_run":"suppress"`)
	k.comment(t, "before the end")
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 1)
	// The wait runs from the instance's own creation, never from an edit or another issue.
	k.f.Exec(t, `UPDATE issue_wakeup SET created_at=now()-interval '2 hours' WHERE issue_id=$1 AND default_rule_key=$2`, k.issue, view.RuleKey)
	k.f.Exec(t, `UPDATE issue_wakeup SET expires_at=now()-interval '1 hour' WHERE issue_id=$1 AND default_rule_key=$2`, k.issue, view.RuleKey)
	k.finish(t)
	k.tick(t) // the scheduler notices the deadline
	k.comment(t, "after the end")
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 1)
	if w := k.instanceRow(t, view.RuleKey); w.Enabled || !w.TimedOutAt.Valid {
		t.Fatalf("instance enabled=%v timed_out=%v, want it ended", w.Enabled, w.TimedOutAt.Valid)
	}
}

func TestCustomNamedSquadRunsItsLeaderWithTheSquadIdentity(t *testing.T) {
	k := newCustomEnv(t)
	leader := k.f.privateAgentOwnedBy(t, k.owner, "leader")
	squad := k.f.Squad(t, "custom squad", leader)
	k.f.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id=$1", k.issue)
	view := k.mustSave(t, WakeupScopeWorkspace, "", `"enabled":true,"trigger":{"kind":"event","events":["comment.created"]},"target":{"type":"squad","id":"`+squad+`"},"instruction":"lead it"`)
	k.comment(t, "wakes the squad")
	k.tick(t)
	run := k.wantRuns(t, view.RuleKey, 1)[0]
	if util.UUIDToString(run.AgentID) != leader || !run.IsLeaderTask || util.UUIDToString(run.SquadID) != squad {
		t.Fatalf("run = agent %s leader=%v squad %s, want the leader %s briefed as squad %s", util.UUIDToString(run.AgentID), run.IsLeaderTask, util.UUIDToString(run.SquadID), leader, squad)
	}
}

func TestCustomRuleFailsClosedWhenItsTargetIsRevoked(t *testing.T) {
	k := newCustomEnv(t)
	view := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`)+`,"active_run":"suppress"`)
	k.comment(t, "first")
	k.tick(t)
	run := k.wantRuns(t, view.RuleKey, 1)[0]
	// The author loses the right to invoke the agent: its owner changes.
	other := k.f.member(t, "custom-new-owner")
	// The new owner's row is deleted before the agent, so hand the agent back first.
	k.f.Cleanup(t, `UPDATE agent SET owner_id=$2 WHERE id=$1`, k.agent, k.owner)
	k.f.Exec(t, `UPDATE agent SET owner_id=$2 WHERE id=$1`, k.agent, other)
	if err := k.s.CheckClaim(context.Background(), run); !errors.Is(err, ErrWakeupForbidden) {
		t.Fatalf("claim of a run whose target was revoked: %v", err)
	}
	k.finish(t)
	k.comment(t, "second")
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 1)
	if got := k.outcomes2(t, view.RuleKey); len(got) == 0 || got[len(got)-1] != "target_unauthorized" {
		t.Fatalf("the refusal is not visible on the timeline: %v", got)
	}
	if n := k.f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.default_rule_key=$1 AND r.processed_at IS NULL`, view.RuleKey); n != 0 {
		t.Fatal("the refused input stayed pending")
	}
}

func TestCustomClosedIssueRestsUntilARearmNoEditLifts(t *testing.T) {
	k := newCustomEnv(t)
	view := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`)+`,"active_run":"suppress"`)
	k.comment(t, "creates the instance")
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 1)
	k.finish(t)
	wakeSetStatus(t, k.f, parseTestUUID(t, k.issue), "done")
	k.f.Exec(t, `UPDATE issue SET status='todo' WHERE id=$1`, k.issue)
	k.mustSave(t, WakeupScopeWorkspace, view.RuleKey, strings.Replace(k.eventRule(`["comment.created"]`), "look at it", "edited", 1)+`,"active_run":"suppress"`)
	k.comment(t, "after reopening and editing")
	k.tick(t)
	k.drain(t)
	k.wantRuns(t, view.RuleKey, 1)
	if w := k.instanceRow(t, view.RuleKey); w.Enabled || !w.DisabledAt.Valid {
		t.Fatalf("instance enabled=%v disabled_at=%v, want it at rest after its issue closed", w.Enabled, w.DisabledAt.Valid)
	}
}

func TestCustomBacklogIsDormant(t *testing.T) {
	k := newCustomEnv(t)
	view := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`)+`,"active_run":"suppress"`)
	k.comment(t, "creates the instance")
	k.tick(t)
	k.finish(t)
	k.f.Exec(t, `UPDATE issue SET status='backlog' WHERE id=$1`, k.issue)
	k.comment(t, "while parked")
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 1)
	if n := k.f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.default_rule_key=$1 AND r.processed_at IS NULL`, view.RuleKey); n != 0 {
		t.Fatalf("%d facts held for a parked issue, want none", n)
	}
	k.f.Exec(t, `UPDATE issue SET status='todo' WHERE id=$1`, k.issue)
	k.comment(t, "after it leaves backlog")
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 2)
}

func TestCustomRuleStopsWhenDisabledAndFreesItsSlotThenResumes(t *testing.T) {
	k := newCustomEnv(t)
	view := k.mustSave(t, WakeupScopeProject, "", k.conditionRule(statusIsTodo))
	k.sweepAll(t)
	if !k.instanceRow(t, view.RuleKey).Enabled {
		t.Fatal("no instance applied")
	}
	k.mustSave(t, WakeupScopeProject, view.RuleKey, strings.Replace(k.conditionRule(statusIsTodo), `"enabled":true`, `"enabled":false`, 1))
	k.sweepAll(t)
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 0)
	if w := k.instanceRow(t, view.RuleKey); w.Enabled || w.CapacityReason.Valid {
		t.Fatalf("a disabled rule's instance: enabled=%v capacity=%v, want it out of the pool", w.Enabled, w.CapacityReason.Valid)
	}
	// Re-enabled: the same instance is applied again and baselines the facts, so the edit alone wakes nothing.
	id := k.instanceRow(t, view.RuleKey).ID
	k.mustSave(t, WakeupScopeProject, view.RuleKey, k.conditionRule(statusIsTodo))
	k.sweepAll(t)
	again := k.instanceRow(t, view.RuleKey)
	if again.ID != id || !again.Enabled {
		t.Fatalf("instance %v enabled=%v, want the same instance applied again", again.ID, again.Enabled)
	}
	if !again.NextFireAt.Valid {
		t.Fatal("an applied condition instance has no next evaluation, so the scheduler would never look at it")
	}
}

func TestCustomMovingAnIssueSwitchesProjectInstances(t *testing.T) {
	k := newCustomEnv(t)
	other := k.f.Project(t, "destination")
	oldRule := k.mustSave(t, WakeupScopeProject, "", k.conditionRule(statusIsTodo))
	k.sweepAll(t)
	oldInstance := k.instanceRow(t, oldRule.RuleKey)
	// The destination has its own rule.
	ws := parseTestUUID(t, k.f.WorkspaceID)
	newRef := WakeupScopeRef{Kind: WakeupScopeProject, ID: parseTestUUID(t, other), WorkspaceID: ws}
	newView, err := k.s.SaveWakeupDefinition(context.Background(), newRef, parseTestUUID(t, k.owner), WakeupDefinitionWrite{Patch: patchOf(t, k.conditionRule(statusIsTodo))})
	if err != nil {
		t.Fatal(err)
	}
	k.f.Exec(t, `UPDATE issue SET project_id=$2 WHERE id=$1`, k.issue, other)
	k.drain(t)
	k.tick(t)
	if runs := k.ruleTasks(t, oldRule.RuleKey); len(runs) != 0 {
		t.Fatalf("the old project's rule ran for the moved issue: %d runs", len(runs))
	}
	if w := k.instanceRow(t, oldRule.RuleKey); w.Enabled || w.ID != oldInstance.ID {
		t.Fatalf("old instance enabled=%v: want it retired, history kept", w.Enabled)
	}
	moved := k.instanceRow(t, newView.RuleKey)
	if !moved.Enabled || moved.DefaultScopeID != newRef.ID || moved.ConditionState != "status:todo" {
		t.Fatalf("destination instance = %+v, want it applied from this point forward (facts already true baselined)", moved)
	}
	k.wantRuns(t, newView.RuleKey, 0)
	// Moving back finds the old instance again.
	k.f.Exec(t, `UPDATE issue SET project_id=$2 WHERE id=$1`, k.issue, k.project)
	k.drain(t)
	if w := k.instanceRow(t, oldRule.RuleKey); w.ID != oldInstance.ID || !w.Enabled || !w.NextFireAt.Valid {
		t.Fatalf("after moving back: id %v enabled=%v next evaluation %v, want the same instance applied again and due for evaluation", w.ID, w.Enabled, w.NextFireAt.Valid)
	}
}

func TestCustomConditionHintWakesAnInstanceThatExists(t *testing.T) {
	k := newCustomEnv(t)
	view := k.mustSave(t, WakeupScopeProject, "", k.conditionRule(`{"type":"issue_field","field":"status","value":"in_review"}`))
	k.sweepAll(t)
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 0)
	k.f.Exec(t, `UPDATE issue SET status='in_review' WHERE id=$1`, k.issue)
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 1)
}

func (k customEnv) outcomes2(t *testing.T, rule string) []string {
	t.Helper()
	rows, err := k.f.Pool.Query(context.Background(), `SELECT COALESCE(details->>'outcome','') FROM activity_log WHERE issue_id=$1 AND details->>'rule'=$2 ORDER BY created_at,id`, k.issue, rule)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			t.Fatal(err)
		}
		out = append(out, o)
	}
	return out
}

func TestCustomChildrenConditionRunsForANewParent(t *testing.T) {
	k := newCustomEnv(t)
	view := k.mustSave(t, WakeupScopeProject, "", k.conditionRule(`{"type":"children_done"}`))
	k.sweepAll(t)
	child := k.f.Issue(t, "child", testutil.Cols{"project_id": k.project, "parent_issue_id": k.issue})
	k.drain(t)
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 0)
	k.f.Exec(t, `UPDATE issue SET status='done' WHERE id=$1`, child)
	// Sub-issue progress has no hint event: the scheduler polls it.
	k.f.Exec(t, `UPDATE issue_wakeup SET next_fire_at=now()-interval '1 second' WHERE issue_id=$1 AND default_rule_key=$2`, k.issue, view.RuleKey)
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 1)
}

// Overriding an event rule's text is a configuration edit: the condition sweep
// that follows it must leave the rule's event instances alone.
func TestCustomOverrideOfAnEventRuleKeepsItsInstances(t *testing.T) {
	k := newCustomEnv(t)
	view := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`)+`,"active_run":"suppress"`)
	k.comment(t, "creates the instance")
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 1)
	k.mustSave(t, WakeupScopeProject, view.RuleKey, `"instruction":"project text"`)
	k.sweepAll(t)
	if w := k.instanceRow(t, view.RuleKey); !w.Enabled {
		t.Fatal("a sweep retired an event instance")
	}
	k.finish(t)
	k.comment(t, "after the override")
	k.tick(t)
	runs := k.wantRuns(t, view.RuleKey, 2)
	if !strings.Contains(runs[1].TriggerSummary.String, "project text") {
		t.Fatalf("the second run does not carry the project override: %q", runs[1].TriggerSummary.String)
	}
}

func TestCustomRunIsRefusedAtStartWhenItsConfigurationChangedMeanwhile(t *testing.T) {
	k := newCustomEnv(t)
	view := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`))
	k.comment(t, "fires")
	k.tick(t)
	run := k.wantRuns(t, view.RuleKey, 1)[0]
	k.f.Exec(t, `UPDATE agent_task_queue SET status='dispatched',dispatched_at=clock_timestamp() WHERE id=$1`, run.ID)
	run, err := k.f.q.GetAgentTask(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.s.CheckStart(context.Background(), run); err != nil {
		t.Fatalf("a current run must start: %v", err)
	}
	k.mustSave(t, WakeupScopeWorkspace, view.RuleKey, strings.Replace(k.eventRule(`["comment.created"]`), "look at it", "changed after the claim", 1))
	if err := k.s.CheckStart(context.Background(), run); !errors.Is(err, ErrWakeupForbidden) {
		t.Fatalf("a run of an older configuration: %v, want it refused at start", err)
	}
}

func TestCustomRuleHonorsItsOwnRateLimit(t *testing.T) {
	k := newCustomEnv(t)
	view := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`)+`,"rate_limit":1,"active_run":"suppress"`)
	k.comment(t, "first")
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 1)
	k.finish(t)
	k.comment(t, "second, within the hour")
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 1)
	if w := k.instanceRow(t, view.RuleKey); w.PausedReason.String != wakeupPausedRate {
		t.Fatalf("paused %q, want the rate pause at the rule's own limit of 1", w.PausedReason.String)
	}
}

func TestCustomMovingAnIssueStopsAnEventRuleRootedInItsOldProject(t *testing.T) {
	k := newCustomEnv(t)
	other := k.f.Project(t, "event destination")
	view := k.mustSave(t, WakeupScopeProject, "", k.eventRule(`["comment.created"]`)+`,"active_run":"suppress"`)
	k.comment(t, "creates the instance")
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 1)
	k.finish(t)
	k.f.Exec(t, `UPDATE issue SET project_id=$2 WHERE id=$1`, k.issue, other)
	k.comment(t, "after the move")
	k.tick(t)
	k.wantRuns(t, view.RuleKey, 1)
	if w := k.instanceRow(t, view.RuleKey); w.Enabled {
		t.Fatal("the old project's instance still runs for the moved issue")
	}
}
