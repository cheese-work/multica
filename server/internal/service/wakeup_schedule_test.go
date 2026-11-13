package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Scheduled custom rules (CHE-1082 L11): the at, relative, every and cron
// trigger kinds, their instances, and their execution under the sweep, max
// fires, expiry and the capacity and aggregate gates.

func (k customEnv) scheduleRule(kind, schedule string) string {
	return `"enabled":true,"trigger":{"kind":"` + kind + `"},"schedule":` + schedule + `,` + k.target() + `,"instruction":"on schedule"`
}

func inFuture(d time.Duration) string { return time.Now().Add(d).UTC().Format(time.RFC3339) }

func TestScheduleDefinitionAcceptsEveryScheduleKind(t *testing.T) {
	k := newCustomEnv(t)
	for name, rule := range map[string]string{
		"at":       k.scheduleRule("at", `{"at":"`+inFuture(time.Hour)+`"}`),
		"relative": k.scheduleRule("relative", `{"after_seconds":3600}`),
		"every":    k.scheduleRule("every", `{"interval_seconds":3600}`),
		"cron":     k.scheduleRule("cron", `{"cron":"30 9 * * 1-5","timezone":"Asia/Ho_Chi_Minh"}`),
	} {
		view, err := k.save(WakeupScopeWorkspace, "", rule, t)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// The activation signal brings issues that become eligible later into the rule.
		if got := k.storedEventTypes(t, view.RuleKey); !slices.Contains(got, wakeupActivateEvent) {
			t.Errorf("%s selector = %v, want the activation signal", name, got)
		}
		repeating := name == "every" || name == "cron"
		if got := view.Patch.MaxFires; got.Set != repeating || repeating && got.Value != wakeupCustomFireDefault {
			t.Errorf("%s max_fires = %+v, want the default of %d on a repeating rule only", name, got, wakeupCustomFireDefault)
		}
	}
}

func TestScheduleDefinitionRefusesWhatCannotRun(t *testing.T) {
	k := newCustomEnv(t)
	for name, fields := range map[string]string{
		"schedule on an event trigger": k.eventRule(`["comment.created"]`) + `,"schedule":{"interval_seconds":3600}`,
		"schedule trigger no schedule": `"enabled":true,"trigger":{"kind":"every"},` + k.target() + `,"instruction":"x"`,
		"kind and schedule differ":     k.scheduleRule("every", `{"cron":"0 * * * *"}`),
		"two schedule fields":          k.scheduleRule("every", `{"interval_seconds":3600,"after_seconds":60}`),
		"empty schedule":               k.scheduleRule("every", `{}`),
		"unknown schedule field":       k.scheduleRule("every", `{"interval_seconds":3600,"every":1}`),
		"interval under a minute":      k.scheduleRule("every", `{"interval_seconds":59}`),
		"interval over a year":         k.scheduleRule("every", `{"interval_seconds":31536001}`),
		"delay under a minute":         k.scheduleRule("relative", `{"after_seconds":10}`),
		"at in the past":               k.scheduleRule("at", `{"at":"2020-01-01T00:00:00Z"}`),
		"at beyond a year":             k.scheduleRule("at", `{"at":"`+inFuture(400*24*time.Hour)+`"}`),
		"at not a time":                k.scheduleRule("at", `{"at":"tomorrow"}`),
		"cron not parseable":           k.scheduleRule("cron", `{"cron":"every day"}`),
		"cron without a timezone":      k.scheduleRule("cron", `{"cron":"0 * * * *"}`),
		"cron unknown timezone":        k.scheduleRule("cron", `{"cron":"0 * * * *","timezone":"Mars/Olympus"}`),
		"cron inline timezone":         k.scheduleRule("cron", `{"cron":"CRON_TZ=UTC 0 * * * *","timezone":"UTC"}`),
		"cron never fires":             k.scheduleRule("cron", `{"cron":"0 0 30 2 *","timezone":"UTC"}`),
		"timezone on an interval":      k.scheduleRule("every", `{"interval_seconds":3600,"timezone":"UTC"}`),
		"one-shot repeating":           k.scheduleRule("relative", `{"after_seconds":3600}`) + `,"mode":"continuous"`,
		"one-shot max fires":           k.scheduleRule("relative", `{"after_seconds":3600}`) + `,"max_fires":3`,
		"interval once":                k.scheduleRule("every", `{"interval_seconds":3600}`) + `,"mode":"once"`,
		"schedule on a built-in":       `"schedule":{"interval_seconds":3600}`,
	} {
		key := ""
		if name == "schedule on a built-in" {
			key = SystemRulePRMerged
		}
		if _, err := k.save(WakeupScopeWorkspace, key, fields, t); err == nil {
			t.Errorf("%s: accepted", name)
		} else if !errors.Is(err, ErrWakeupInput) {
			t.Errorf("%s: error %v is not an input refusal", name, err)
		}
	}
	if n := k.f.Count(t, `SELECT count(*) FROM issue_wakeup_definition WHERE workspace_id=$1`, k.f.WorkspaceID); n != 0 {
		t.Fatalf("a refused definition was stored: %d rows", n)
	}
}

// An override may change the schedule alone, but never leave a rule whose
// trigger and schedule disagree.
func TestScheduleOverrideCannotSplitTriggerAndSchedule(t *testing.T) {
	k := newCustomEnv(t)
	root := k.mustSave(t, WakeupScopeWorkspace, "", k.scheduleRule("every", `{"interval_seconds":3600}`))
	k.mustSave(t, WakeupScopeProject, root.RuleKey, `"schedule":{"interval_seconds":7200}`)
	if _, err := k.save(WakeupScopeProject, root.RuleKey, `"schedule":{"cron":"0 * * * *","timezone":"UTC"}`, t); !errors.Is(err, ErrWakeupInput) {
		t.Fatalf("an override that moves the schedule to another kind: %v, want an input refusal", err)
	}
}

type scheduledInstance struct {
	Issue, Kind, Timezone string
	Interval              *int64
	Cron                  *string
	Next                  *time.Time
	Enabled               bool
	CapacityReason        *string
}

func (k customEnv) scheduled(t *testing.T, rule string) map[string]scheduledInstance {
	t.Helper()
	rows, err := k.f.Pool.Query(context.Background(), `SELECT issue_id::text,kind,timezone,interval_seconds,cron_expression,next_fire_at,enabled,capacity_reason
 FROM issue_wakeup WHERE workspace_id=$1 AND default_rule_key=$2`, k.f.WorkspaceID, rule)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]scheduledInstance{}
	for rows.Next() {
		var i scheduledInstance
		if err := rows.Scan(&i.Issue, &i.Kind, &i.Timezone, &i.Interval, &i.Cron, &i.Next, &i.Enabled, &i.CapacityReason); err != nil {
			t.Fatal(err)
		}
		out[i.Issue] = i
	}
	return out
}

func (k customEnv) onlyScheduled(t *testing.T, rule string) scheduledInstance {
	t.Helper()
	got := k.scheduled(t, rule)
	i, ok := got[k.issue]
	if !ok || len(got) != 1 {
		t.Fatalf("instances of %s = %+v, want exactly the fixture issue's", rule, got)
	}
	return i
}

func TestScheduleSweepCreatesAnInstanceOfTheRightKindWithoutQueueingWork(t *testing.T) {
	k := newCustomEnv(t)
	at := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	before := time.Now()
	atRule := k.mustSave(t, WakeupScopeProject, "", k.scheduleRule("at", `{"at":"`+at.Format(time.RFC3339)+`"}`))
	relRule := k.mustSave(t, WakeupScopeProject, "", k.scheduleRule("relative", `{"after_seconds":600}`))
	everyRule := k.mustSave(t, WakeupScopeProject, "", k.scheduleRule("every", `{"interval_seconds":900}`))
	cronRule := k.mustSave(t, WakeupScopeProject, "", k.scheduleRule("cron", `{"cron":"0 9 * * *","timezone":"Asia/Ho_Chi_Minh"}`))
	k.sweepAll(t)

	a := k.onlyScheduled(t, atRule.RuleKey)
	if a.Kind != "at" || a.Next == nil || !a.Next.Equal(at) || !a.Enabled {
		t.Errorf("at instance = %+v, want kind at due exactly %s", a, at)
	}
	r := k.onlyScheduled(t, relRule.RuleKey)
	if r.Kind != "at" || r.Next == nil || r.Next.Before(before.Add(600*time.Second)) || r.Next.After(time.Now().Add(600*time.Second)) {
		t.Errorf("relative instance = %+v, want one delay of 600s after its activation", r)
	}
	e := k.onlyScheduled(t, everyRule.RuleKey)
	if e.Kind != "every" || e.Interval == nil || *e.Interval != 900 || e.Next == nil || e.Next.Before(before.Add(900*time.Second)) {
		t.Errorf("every instance = %+v, want kind every, 900s, first due one interval after activation", e)
	}
	c := k.onlyScheduled(t, cronRule.RuleKey)
	want, err := NextOccurrenceAfterUTC("0 9 * * *", "Asia/Ho_Chi_Minh", before)
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != "cron" || c.Cron == nil || *c.Cron != "0 9 * * *" || c.Timezone != "Asia/Ho_Chi_Minh" || c.Next == nil || c.Next.Sub(want).Abs() > 0 && !c.Next.After(before) {
		t.Errorf("cron instance = %+v, want kind cron with its expression and zone, due %s", c, want)
	}
	if n := k.taskCount(t); n != 0 {
		t.Fatalf("activation queued %d tasks, want none", n)
	}
	// Nothing is due yet, so a tick fires nothing.
	k.tick(t)
	k.wantRuns(t, atRule.RuleKey, 0)
}

func TestScheduleOneFixedInstantAppliesToEveryMatchingIssue(t *testing.T) {
	k := newCustomEnv(t)
	k.bulkIssues(t, 4, k.project, "todo")
	at := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	rule := k.mustSave(t, WakeupScopeProject, "", k.scheduleRule("at", `{"at":"`+at.Format(time.RFC3339)+`"}`))
	k.sweepAll(t)
	got := k.scheduled(t, rule.RuleKey)
	if len(got) != 5 {
		t.Fatalf("%d instances, want one per issue (5)", len(got))
	}
	for issue, i := range got {
		if i.Next == nil || !i.Next.Equal(at) {
			t.Errorf("issue %s due %v, want the shared instant %s", issue, i.Next, at)
		}
	}
}

func TestScheduleLateEnteringIssueDoesNotReplayAnElapsedOneShot(t *testing.T) {
	k := newCustomEnv(t)
	rule := k.mustSave(t, WakeupScopeProject, "", k.scheduleRule("at", `{"at":"`+inFuture(time.Hour)+`"}`))
	k.sweepAll(t)
	k.onlyScheduled(t, rule.RuleKey)
	// The instant passes: a stored instant in the past is still a readable schedule.
	k.f.Exec(t, `UPDATE issue_wakeup_definition SET config=jsonb_set(config,'{schedule,at}',to_jsonb($3::text)) WHERE workspace_id=$1 AND rule_key=$2`, k.f.WorkspaceID, rule.RuleKey, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339))
	late := k.f.Issue(t, "late", testutil.Cols{"project_id": k.project})
	k.f.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id=$1", late)
	k.f.Exec(t, `UPDATE issue_wakeup_definition SET sweep_done=false WHERE workspace_id=$1 AND rule_key=$2`, k.f.WorkspaceID, rule.RuleKey)
	k.sweepAll(t)
	got := k.scheduled(t, rule.RuleKey)
	if _, ok := got[late]; ok || len(got) != 1 {
		t.Fatalf("instances = %+v, want none for the issue that entered after the instant", got)
	}
	k.tick(t)
	if n := k.f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, late); n != 0 {
		t.Fatalf("the late issue got %d runs from an elapsed one-shot", n)
	}
}

func TestScheduleRelativeDelayRunsFromEachIssuesOwnActivation(t *testing.T) {
	k := newCustomEnv(t)
	rule := k.mustSave(t, WakeupScopeProject, "", k.scheduleRule("relative", `{"after_seconds":600}`))
	k.sweepAll(t)
	first := k.onlyScheduled(t, rule.RuleKey)
	time.Sleep(1100 * time.Millisecond)
	late := k.f.Issue(t, "later", testutil.Cols{"project_id": k.project})
	k.f.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id=$1", late)
	k.f.Exec(t, `UPDATE issue_wakeup_definition SET sweep_done=false WHERE workspace_id=$1 AND rule_key=$2`, k.f.WorkspaceID, rule.RuleKey)
	k.sweepAll(t)
	got := k.scheduled(t, rule.RuleKey)
	if len(got) != 2 || got[late].Next == nil || !got[late].Next.After(first.Next.Add(time.Second)) {
		t.Fatalf("instances = %+v, want the late issue's delay to start at its own activation", got)
	}
}

func TestScheduleEveryFiresOnceForMissedTicksAndMovesPastThem(t *testing.T) {
	k := newCustomEnv(t)
	rule := k.mustSave(t, WakeupScopeProject, "", k.scheduleRule("every", `{"interval_seconds":3600}`)+`,"active_run":"suppress"`)
	k.sweepAll(t)
	k.f.Exec(t, `UPDATE issue_wakeup SET next_fire_at=now()-interval '5 hours 10 minutes' WHERE default_rule_key=$1`, rule.RuleKey)
	k.tick(t)
	k.wantRuns(t, rule.RuleKey, 1) // five missed ticks are one run, never five
	i := k.onlyScheduled(t, rule.RuleKey)
	if i.Next == nil || !i.Next.After(time.Now()) || i.Next.After(time.Now().Add(time.Hour)) {
		t.Fatalf("next = %v, want the next future tick within an interval", i.Next)
	}
	k.finish(t)
	k.tick(t)
	k.wantRuns(t, rule.RuleKey, 1)
}

func TestScheduleOneShotFiresOnceAndStaysConsumed(t *testing.T) {
	k := newCustomEnv(t)
	rule := k.mustSave(t, WakeupScopeProject, "", k.scheduleRule("relative", `{"after_seconds":600}`))
	k.sweepAll(t)
	k.f.Exec(t, `UPDATE issue_wakeup SET next_fire_at=now()-interval '1 second' WHERE default_rule_key=$1`, rule.RuleKey)
	k.tick(t)
	k.wantRuns(t, rule.RuleKey, 1)
	k.finish(t)
	k.tick(t)
	// Neither another tick, a sweep, nor an edit of the rule rearms it.
	k.mustSave(t, WakeupScopeProject, rule.RuleKey, k.scheduleRule("relative", `{"after_seconds":900}`))
	k.sweepAll(t)
	k.tick(t)
	k.wantRuns(t, rule.RuleKey, 1)
	if w := k.instanceRow(t, rule.RuleKey); w.Enabled {
		t.Fatal("a consumed one-shot was applied again")
	}
}

func TestScheduleMaxFiresPausesAndAnEditNeverClearsThePause(t *testing.T) {
	k := newCustomEnv(t)
	rule := k.mustSave(t, WakeupScopeProject, "", k.scheduleRule("every", `{"interval_seconds":60}`)+`,"max_fires":2,"active_run":"suppress"`)
	k.sweepAll(t)
	for range 3 {
		k.f.Exec(t, `UPDATE issue_wakeup SET next_fire_at=now()-interval '1 second' WHERE default_rule_key=$1 AND enabled`, rule.RuleKey)
		k.tick(t)
		k.finish(t)
	}
	k.wantRuns(t, rule.RuleKey, 2)
	if w := k.instanceRow(t, rule.RuleKey); w.PausedReason.String != wakeupPausedMaxFires || w.FireCount != 2 {
		t.Fatalf("instance paused %q after %d fires, want max_fires after 2", w.PausedReason.String, w.FireCount)
	}
	k.mustSave(t, WakeupScopeProject, rule.RuleKey, k.scheduleRule("every", `{"interval_seconds":60}`)+`,"max_fires":50,"active_run":"suppress"`)
	k.sweepAll(t)
	k.f.Exec(t, `UPDATE issue_wakeup SET next_fire_at=now()-interval '1 second' WHERE default_rule_key=$1`, rule.RuleKey)
	k.tick(t)
	k.wantRuns(t, rule.RuleKey, 2)
}

func TestScheduleExpiryEndsTheInstanceFromItsOwnActivation(t *testing.T) {
	k := newCustomEnv(t)
	rule := k.mustSave(t, WakeupScopeProject, "", k.scheduleRule("every", `{"interval_seconds":60}`)+`,"expiry":{"after_seconds":3600},"active_run":"suppress"`)
	k.sweepAll(t)
	i := k.instanceRow(t, rule.RuleKey)
	if !i.ExpiresAt.Valid || i.ExpiresAt.Time.Sub(i.CreatedAt.Time) != time.Hour {
		t.Fatalf("expires_at = %v, want one hour after the instance's own creation %v", i.ExpiresAt, i.CreatedAt)
	}
	k.f.Exec(t, `UPDATE issue_wakeup SET expires_at=now()-interval '1 minute',next_fire_at=now()-interval '2 minutes' WHERE default_rule_key=$1`, rule.RuleKey)
	k.tick(t)
	k.wantRuns(t, rule.RuleKey, 0)
	if w := k.instanceRow(t, rule.RuleKey); w.Enabled || !w.TimedOutAt.Valid {
		t.Fatalf("instance enabled=%v timed_out=%v, want it ended by its expiry", w.Enabled, w.TimedOutAt.Valid)
	}
}

func TestScheduleFullPoolDelaysWithAReasonAndResumesAtTheNextFutureTick(t *testing.T) {
	k := newCustomEnv(t)
	k.fillIssuePool(t, 32)
	rule := k.mustSave(t, WakeupScopeProject, "", k.scheduleRule("every", `{"interval_seconds":3600}`))
	k.sweepAll(t)
	held := k.onlyScheduled(t, rule.RuleKey)
	if held.Enabled || held.CapacityReason == nil {
		t.Fatalf("instance = %+v, want it unapplied with its visible reason, not an error", held)
	}
	// It waited past several ticks.
	k.f.Exec(t, `UPDATE issue_wakeup SET next_fire_at=now()-interval '3 hours 5 minutes' WHERE default_rule_key=$1`, rule.RuleKey)
	k.freePool(t)
	k.sweep(t)
	got := k.onlyScheduled(t, rule.RuleKey)
	if !got.Enabled || got.Next == nil || !got.Next.After(time.Now()) {
		t.Fatalf("instance = %+v, want it applied and due at the next future tick, the missed ones not replayed", got)
	}
	k.tick(t)
	k.wantRuns(t, rule.RuleKey, 0)
}

func TestScheduleFullPoolOneShotIsDeliveredLateNotDropped(t *testing.T) {
	k := newCustomEnv(t)
	k.fillIssuePool(t, 32)
	rule := k.mustSave(t, WakeupScopeProject, "", k.scheduleRule("relative", `{"after_seconds":600}`))
	k.sweepAll(t)
	if held := k.onlyScheduled(t, rule.RuleKey); held.Enabled || held.CapacityReason == nil {
		t.Fatalf("instance = %+v, want it unapplied with its visible reason", held)
	}
	// The pool stayed full past the one-shot's instant.
	k.f.Exec(t, `UPDATE issue_wakeup SET next_fire_at=now()-interval '2 hours' WHERE default_rule_key=$1`, rule.RuleKey)
	k.freePool(t)
	k.sweep(t)
	if w := k.instanceRow(t, rule.RuleKey); !w.Enabled || w.TimedOutAt.Valid {
		t.Fatalf("instance enabled=%v timed_out=%v, want the delayed one-shot applied, not ended", w.Enabled, w.TimedOutAt.Valid)
	}
	k.tick(t)
	k.wantRuns(t, rule.RuleKey, 1)
}

func TestScheduleAggregateCapDelaysDueInstancesAndSharesTurnsFairly(t *testing.T) {
	k := newCustomEnv(t)
	k.bulkIssues(t, 3, k.project, "todo")
	rule := k.mustSave(t, WakeupScopeProject, "", k.scheduleRule("relative", `{"after_seconds":600}`)+`,"aggregate_limit":1`)
	k.sweepAll(t)
	k.f.Exec(t, `UPDATE issue_wakeup SET next_fire_at=now()-interval '1 second' WHERE default_rule_key=$1`, rule.RuleKey)
	ran := map[string]bool{}
	for round := 1; round <= 4; round++ {
		k.tick(t)
		for _, task := range k.ruleTasks(t, rule.RuleKey) {
			ran[util.UUIDToString(task.IssueID)] = true
		}
		if n := len(k.ruleTasks(t, rule.RuleKey)); n != round {
			t.Fatalf("after round %d: %d runs, want %d (one start per hour under the cap)", round, n, round)
		}
		var delayed int
		if err := k.f.Pool.QueryRow(context.Background(), `SELECT count(*) FROM issue_wakeup WHERE default_rule_key=$1 AND aggregate_retry_at IS NOT NULL AND aggregate_blocked_scope_kind='project'`, rule.RuleKey).Scan(&delayed); err != nil {
			t.Fatal(err)
		}
		if want := 4 - round; delayed != want {
			t.Fatalf("after round %d: %d delayed instances naming the counter, want %d", round, delayed, want)
		}
		// The hour passes and the scheduler looks again.
		k.f.Exec(t, `DELETE FROM wakeup_aggregate_reservation WHERE workspace_id=$1`, k.f.WorkspaceID)
		k.f.Exec(t, `UPDATE issue_wakeup SET aggregate_retry_at=now()-interval '1 second' WHERE default_rule_key=$1 AND aggregate_retry_at IS NOT NULL`, rule.RuleKey)
		k.finish(t)
		k.f.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=clock_timestamp() WHERE issue_id IN (SELECT id FROM issue WHERE workspace_id=$1) AND status IN ('queued','dispatched','running')`, k.f.WorkspaceID)
	}
	if len(ran) != 4 {
		t.Fatalf("runs reached %d distinct issues, want every one of the 4 to get its turn", len(ran))
	}
}

func TestScheduleInstructionEditKeepsTheTickAndAScheduleEditMovesIt(t *testing.T) {
	k := newCustomEnv(t)
	rule := k.mustSave(t, WakeupScopeProject, "", k.scheduleRule("every", `{"interval_seconds":3600}`))
	k.sweepAll(t)
	first := k.onlyScheduled(t, rule.RuleKey)
	k.mustSave(t, WakeupScopeProject, rule.RuleKey, `"enabled":true,"trigger":{"kind":"every"},"schedule":{"interval_seconds":3600},`+k.target()+`,"instruction":"new words"`)
	k.sweepAll(t)
	same := k.onlyScheduled(t, rule.RuleKey)
	if same.Next == nil || !same.Next.Equal(*first.Next) {
		t.Fatalf("an instruction edit moved the tick from %v to %v", first.Next, same.Next)
	}
	k.mustSave(t, WakeupScopeProject, rule.RuleKey, `"enabled":true,"trigger":{"kind":"every"},"schedule":{"interval_seconds":900},`+k.target()+`,"instruction":"new words"`)
	k.sweepAll(t)
	moved := k.onlyScheduled(t, rule.RuleKey)
	if moved.Interval == nil || *moved.Interval != 900 || moved.Next == nil || !moved.Next.Before(first.Next.Add(-30*time.Minute)) {
		t.Fatalf("instance = %+v, want the new interval with the next tick one interval from now", moved)
	}
}

func TestScheduleRevisionChangeMidSweepRebasesTheEarlierBatchAndRestartsTheWalk(t *testing.T) {
	k := newCustomEnv(t)
	k.bulkIssues(t, 119, k.project, "todo")
	rule := k.mustSave(t, WakeupScopeProject, "", k.scheduleRule("every", `{"interval_seconds":3600}`))
	k.sweep(t)
	if n := len(k.scheduled(t, rule.RuleKey)); n != wakeupSweepBatch {
		t.Fatalf("first pass activated %d issues, want the batch of %d", n, wakeupSweepBatch)
	}
	// A new scheduler process knows only the database; the definition changes meanwhile.
	k.s = &IssueWakeupService{Tasks: k.f.svc.TaskSvc}
	k.mustSave(t, WakeupScopeProject, rule.RuleKey, k.scheduleRule("every", `{"interval_seconds":1800}`))
	k.sweepAll(t)
	got := k.scheduled(t, rule.RuleKey)
	if len(got) != 120 {
		t.Fatalf("%d instances, want 120", len(got))
	}
	for issue, i := range got {
		if i.Interval == nil || *i.Interval != 1800 {
			t.Fatalf("issue %s still on interval %v, want every instance on the revised 1800s", issue, i.Interval)
		}
	}
}

func TestScheduleDisabledRuleReturnsAtTheNextFutureTick(t *testing.T) {
	k := newCustomEnv(t)
	on := k.scheduleRule("every", `{"interval_seconds":3600}`)
	rule := k.mustSave(t, WakeupScopeProject, "", on)
	k.sweepAll(t)
	k.mustSave(t, WakeupScopeProject, rule.RuleKey, strings.Replace(on, `"enabled":true`, `"enabled":false`, 1))
	k.sweepAll(t)
	if off := k.onlyScheduled(t, rule.RuleKey); off.Enabled || off.Next != nil {
		t.Fatalf("instance = %+v, want it retired with no tick", off)
	}
	k.mustSave(t, WakeupScopeProject, rule.RuleKey, on)
	k.sweepAll(t)
	back := k.onlyScheduled(t, rule.RuleKey)
	if !back.Enabled || back.Next == nil || !back.Next.After(time.Now()) {
		t.Fatalf("instance = %+v, want it applied again with a future tick, so it never sits enabled and never fires", back)
	}
}

func TestScheduleBrokenSettingsFailClosedPerRule(t *testing.T) {
	k := newCustomEnv(t)
	good := k.mustSave(t, WakeupScopeProject, "", k.scheduleRule("every", `{"interval_seconds":3600}`))
	bad := k.mustSave(t, WakeupScopeProject, "", k.scheduleRule("cron", `{"cron":"0 9 * * *","timezone":"UTC"}`))
	// Settings this build cannot read, written behind the validator's back.
	k.f.Exec(t, `UPDATE issue_wakeup_definition SET config=jsonb_set(config,'{schedule}','{"cron":"nonsense","timezone":"UTC"}') WHERE workspace_id=$1 AND rule_key=$2`, k.f.WorkspaceID, bad.RuleKey)
	k.f.Exec(t, `UPDATE issue_wakeup_definition SET sweep_done=false WHERE workspace_id=$1`, k.f.WorkspaceID)
	k.sweepAll(t)
	if got := k.scheduled(t, bad.RuleKey); len(got) != 0 {
		t.Fatalf("a rule with an unreadable schedule made instances: %+v", got)
	}
	k.onlyScheduled(t, good.RuleKey)
}

func TestScheduleOutOfRangeStoredIntervalsAreIsolatedPerRule(t *testing.T) {
	k := newCustomEnv(t)
	good := k.mustSave(t, WakeupScopeProject, "", k.scheduleRule("every", `{"interval_seconds":3600}`))
	rules := map[string]WakeupDefinitionView{
		"zero":     k.mustSave(t, WakeupScopeProject, "", k.scheduleRule("every", `{"interval_seconds":7200}`)),
		"negative": k.mustSave(t, WakeupScopeProject, "", k.scheduleRule("every", `{"interval_seconds":10800}`)),
		"huge":     k.mustSave(t, WakeupScopeProject, "", k.scheduleRule("every", `{"interval_seconds":14400}`)),
		"delay":    k.mustSave(t, WakeupScopeProject, "", k.scheduleRule("relative", `{"after_seconds":3600}`)),
	}
	// Settings behind the validator's back: numbers the write would refuse.
	for name, stored := range map[string]string{"zero": `{"interval_seconds":0}`, "negative": `{"interval_seconds":-60}`, "huge": `{"interval_seconds":99999999999}`, "delay": `{"after_seconds":0}`} {
		k.f.Exec(t, `UPDATE issue_wakeup_definition SET config=jsonb_set(config,'{schedule}',$3::jsonb) WHERE workspace_id=$1 AND rule_key=$2`, k.f.WorkspaceID, rules[name].RuleKey, stored)
	}
	k.f.Exec(t, `UPDATE issue_wakeup_definition SET sweep_done=false WHERE workspace_id=$1`, k.f.WorkspaceID)
	k.sweepAll(t)
	for name, rule := range rules {
		if got := k.scheduled(t, rule.RuleKey); len(got) != 0 {
			t.Fatalf("%s: a rule with an out-of-range schedule made instances: %+v", name, got)
		}
	}
	k.onlyScheduled(t, good.RuleKey)
}

func TestScheduleOutOfRangeStoredInstanceEndsAloneWhenCapacityFrees(t *testing.T) {
	k := newCustomEnv(t)
	k.fillIssuePool(t, 32)
	bad := k.mustSave(t, WakeupScopeProject, "", k.scheduleRule("every", `{"interval_seconds":3600}`))
	good := k.mustSave(t, WakeupScopeProject, "", k.scheduleRule("every", `{"interval_seconds":7200}`))
	k.sweepAll(t)
	// An instance row with an interval the write would refuse.
	k.f.Exec(t, `UPDATE issue_wakeup SET interval_seconds=0 WHERE default_rule_key=$1`, bad.RuleKey)
	k.freePool(t)
	k.sweep(t)
	if w := k.instanceRow(t, bad.RuleKey); w.Enabled || !w.TimedOutAt.Valid {
		t.Fatalf("instance enabled=%v timed_out=%v, want it ended without a panic", w.Enabled, w.TimedOutAt.Valid)
	}
	if got := k.onlyScheduled(t, good.RuleKey); !got.Enabled {
		t.Fatalf("healthy instance = %+v, want it applied", got)
	}
}

func TestScheduleOccurrencesAcrossDaylightSavingTime(t *testing.T) {
	ny := "America/New_York"
	utc := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	next := func(expr string, from time.Time) time.Time {
		s, err := scheduleOf(wakeupTriggerKindCron, wakeupScheduleSpec{Cron: expr, Timezone: ny}, from, from)
		if err != nil || s.Elapsed || !s.Next.Valid {
			t.Fatalf("schedule of %q after %s = %+v, %v", expr, from, s, err)
		}
		return s.Next.Time
	}
	// Spring forward, 8 Mar 2026: 02:30 local does not exist that day, so the
	// daily 02:30 rule has no occurrence on it and resumes the next day.
	if got, want := next("30 2 * * *", utc("2026-03-07T08:00:00Z")), utc("2026-03-09T06:30:00Z"); !got.Equal(want) {
		t.Errorf("after the spring-forward gap: %s, want %s", got, want)
	}
	// Before the gap the rule still fires at its local time (EST, UTC-5).
	if got, want := next("30 2 * * *", utc("2026-03-06T08:00:00Z")), utc("2026-03-07T07:30:00Z"); !got.Equal(want) {
		t.Errorf("before the gap: %s, want %s", got, want)
	}
	// Fall back, 1 Nov 2026: 01:30 happens twice; a wall-clock rule fires at each
	// real instant and every later occurrence is strictly after the one before.
	first := next("30 1 * * *", utc("2026-10-31T12:00:00Z"))
	second := next("30 1 * * *", first)
	third := next("30 1 * * *", second)
	if !first.Equal(utc("2026-11-01T05:30:00Z")) || !second.Equal(utc("2026-11-01T06:30:00Z")) || !third.Equal(utc("2026-11-02T06:30:00Z")) {
		t.Errorf("fall-back overlap: %s %s %s", first, second, third)
	}
	// An hourly rule keeps one slot per real hour through the overlap.
	h := utc("2026-11-01T04:30:00Z")
	var hours []time.Time
	for range 4 {
		h = next("0 * * * *", h)
		hours = append(hours, h)
	}
	for i := 1; i < len(hours); i++ {
		if hours[i].Sub(hours[i-1]) != time.Hour {
			t.Errorf("hourly slots %v are not one real hour apart", hours)
		}
	}
}

func TestScheduleOfFixedKinds(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	created := now.Add(-10 * time.Minute)
	hour := int64(3600)
	delay := int64(900)
	future, past := now.Add(time.Hour), now.Add(-time.Hour)
	for name, c := range map[string]struct {
		kind    string
		spec    wakeupScheduleSpec
		next    time.Time
		elapsed bool
	}{
		"at in the future":          {wakeupTriggerKindAt, wakeupScheduleSpec{At: &future}, future, false},
		"at elapsed":                {wakeupTriggerKindAt, wakeupScheduleSpec{At: &past}, past, true},
		"relative from creation":    {wakeupTriggerKindRelative, wakeupScheduleSpec{AfterSeconds: &delay}, created.Add(15 * time.Minute), false},
		"relative already elapsed":  {wakeupTriggerKindRelative, wakeupScheduleSpec{AfterSeconds: new(int64(60))}, created.Add(time.Minute), true},
		"every skips past ticks":    {wakeupTriggerKindEvery, wakeupScheduleSpec{IntervalSeconds: &hour}, created.Add(time.Hour), false},
		"every from a long ago one": {wakeupTriggerKindEvery, wakeupScheduleSpec{IntervalSeconds: new(int64(300))}, created.Add(15 * time.Minute), false},
	} {
		s, err := scheduleOf(c.kind, c.spec, created, now)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !s.Next.Time.Equal(c.next) || s.Elapsed != c.elapsed {
			t.Errorf("%s: next %s elapsed %v, want %s %v", name, s.Next.Time, s.Elapsed, c.next, c.elapsed)
		}
	}
	if _, err := scheduleOf(wakeupTriggerKindCron, wakeupScheduleSpec{Cron: "bad", Timezone: "UTC"}, created, now); err == nil {
		t.Error("an unreadable cron was accepted")
	}
}

var _ = db.IssueWakeup{}
