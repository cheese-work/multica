package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Corrections from the signing review of L5.

func (e builtinEnv) task(t *testing.T, id string) db.AgentTaskQueue {
	t.Helper()
	task, err := e.f.q.GetAgentTask(context.Background(), parseTestUUID(t, id))
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// A built-in that joined an ordinary run is validated again when that run
// starts: the carrier has no wakeup_system of its own, so only its joined
// entries can tell that the configuration or the target changed.
func TestBuiltinJoinedRunIsRevalidatedAtStart(t *testing.T) {
	ctx := context.Background()
	t.Run("configuration edit", func(t *testing.T) {
		e := newBuiltinEnv(t)
		e.define(t, WakeupScopeProject, SystemRulePRMerged, `"instruction":"One"`)
		carrier := wakeWaitingRun(t, e.f, e.issue, e.agent, e.f.UserID)
		e.mergedPR(t, 500)
		if notes := wakeClaim(t, e.f, e.s, carrier); !strings.Contains(notes, "One") {
			t.Fatalf("the rule did not join: %q", notes)
		}
		if err := e.s.CheckStart(ctx, e.task(t, carrier)); err != nil {
			t.Fatalf("start of an unchanged joined run: %v", err)
		}
		e.define(t, WakeupScopeProject, SystemRulePRMerged, `"instruction":"Two"`)
		if err := e.s.CheckStart(ctx, e.task(t, carrier)); !errors.Is(err, ErrWakeupForbidden) {
			t.Fatalf("start of a run carrying a stale joined instruction = %v, want ErrWakeupForbidden", err)
		}
		// A run that is already running keeps its prompt.
		wakeStart(t, e.f, carrier)
		if err := e.s.CheckStart(ctx, e.task(t, carrier)); err != nil {
			t.Fatalf("a running carrier must stay valid: %v", err)
		}
	})
	t.Run("revoked target", func(t *testing.T) {
		e := newBuiltinEnv(t)
		newOwner := e.f.member(t, "joined-new-owner")
		named := e.f.privateAgentOwnedBy(t, e.f.UserID, "joined-named")
		e.f.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id=$1", e.issue)
		e.define(t, WakeupScopeWorkspace, SystemRulePRMerged, fmt.Sprintf(`"target":{"type":"agent","id":%q}`, named))
		carrier := wakeWaitingRun(t, e.f, e.issue, named, e.f.UserID)
		e.mergedPR(t, 501)
		if notes := wakeClaim(t, e.f, e.s, carrier); notes == "" {
			t.Fatal("the rule did not join")
		}
		e.f.Exec(t, `UPDATE agent SET owner_id=$2 WHERE id=$1`, named, newOwner)
		if err := e.s.CheckStart(ctx, e.task(t, carrier)); !errors.Is(err, ErrWakeupForbidden) {
			t.Fatalf("start after the writer lost the target = %v, want ErrWakeupForbidden", err)
		}
	})
	t.Run("legacy joined runs are not re-judged", func(t *testing.T) {
		e := newBuiltinEnv(t)
		carrier := wakeWaitingRun(t, e.f, e.issue, e.agent, e.f.UserID)
		e.mergedPR(t, 502)
		if notes := wakeClaim(t, e.f, e.s, carrier); notes == "" {
			t.Fatal("the rule did not join")
		}
		e.f.Exec(t, `UPDATE workspace SET settings='{"github_wake_on_pr_merge":false}'::jsonb WHERE id=$1`, e.f.WorkspaceID)
		if err := e.s.CheckStart(ctx, e.task(t, carrier)); err != nil {
			t.Fatalf("a legacy joined run changed behavior at start: %v", err)
		}
	})
}

// A joined firing that has started counts toward once/max_fires even when an
// ancestor edit retires the revision it was reserved under before the rule's
// own dispatch settled it.
func TestBuiltinStartedJoinedFiringSurvivesAncestorEdit(t *testing.T) {
	for _, tc := range []struct{ name, fields string }{
		{"once", `"mode":"once"`},
		{"max_fires 1", `"max_fires":1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newBuiltinEnv(t)
			e.define(t, WakeupScopeProject, SystemRulePRMerged, `"instruction":"One",`+tc.fields)
			carrier := wakeWaitingRun(t, e.f, e.issue, e.agent, e.f.UserID)
			e.mergedPR(t, 510)
			if notes := wakeClaim(t, e.f, e.s, carrier); !strings.Contains(notes, "One") {
				t.Fatalf("the rule did not join: %q", notes)
			}
			wakeStart(t, e.f, carrier)
			e.define(t, WakeupScopeProject, SystemRulePRMerged, `"instruction":"Two",`+tc.fields)
			e.mergedPR(t, 511)
			w := e.rule(t, SystemRulePRMerged)
			if w.FireCount != 1 || !w.PausedReason.Valid || w.PausedReason.String != wakeupPausedMaxFires {
				t.Fatalf("after the edit: fire_count=%d pause=%q; the started joined firing must stay counted and end the instance", w.FireCount, w.PausedReason.String)
			}
			if e.tasks(t, SystemRulePRMerged) != 0 {
				t.Fatalf("a consumed instance started %d own runs after an ancestor edit", e.tasks(t, SystemRulePRMerged))
			}
		})
	}
}

// Joined firings that have not started yet are counted against the limit, so a
// second input cannot start an own run past the cap.
func TestBuiltinInFlightJoinedFiringsCountAgainstTheLimit(t *testing.T) {
	e := newBuiltinEnv(t)
	ctx := context.Background()
	e.f.Cleanup(t, "DELETE FROM issue_child_event WHERE parent_id=$1", e.issue)
	e.define(t, WakeupScopeProject, SystemRuleChildDone, `"max_fires":1`)
	first := e.f.Issue(t, "first", testutil.Cols{"parent_issue_id": e.issue, "status": "in_progress"})
	if err := e.s.ProcessChildEvents(ctx, e.issue); err != nil {
		t.Fatal(err)
	}
	carrier := wakeWaitingRun(t, e.f, e.issue, e.agent, e.f.UserID)
	e.f.Exec(t, "UPDATE issue SET status='done' WHERE id=$1", first)
	if err := e.s.ProcessChildEvents(ctx, e.issue); err != nil {
		t.Fatal(err)
	}
	if notes := wakeClaim(t, e.f, e.s, carrier); notes == "" {
		t.Fatal("the rule did not join")
	}
	second := e.f.Issue(t, "second", testutil.Cols{"parent_issue_id": e.issue, "status": "in_progress"})
	if err := e.s.ProcessChildEvents(ctx, e.issue); err != nil {
		t.Fatal(err)
	}
	e.f.Exec(t, "UPDATE issue SET status='done' WHERE id=$1", second)
	if err := e.s.ProcessChildEvents(ctx, e.issue); err != nil {
		t.Fatal(err)
	}
	if e.tasks(t, SystemRuleChildDone) != 0 {
		t.Fatalf("an own run started while a joined firing already took the last slot: %d runs", e.tasks(t, SystemRuleChildDone))
	}
}

// A runtime pause is not a configuration change: the cap-reaching run of a
// customized legacy child_done rule must stay claimable and startable.
func TestBuiltinRuntimePauseKeepsTheFingerprint(t *testing.T) {
	e := newBuiltinEnv(t)
	ctx := context.Background()
	e.f.Cleanup(t, "DELETE FROM issue_child_event WHERE parent_id=$1", e.issue)
	child := e.f.Issue(t, "child", testutil.Cols{"parent_issue_id": e.issue, "status": "in_progress"})
	if err := e.s.ProcessChildEvents(ctx, e.issue); err != nil {
		t.Fatal(err)
	}
	on, text := true, "customized text"
	customizeChildDone(t, e.s, e.issue, &on, &text)
	e.define(t, WakeupScopeProject, SystemRuleChildDone, `"mode":"once"`)
	e.f.Exec(t, "UPDATE issue SET status='done' WHERE id=$1", child)
	if err := e.s.ProcessChildEvents(ctx, e.issue); err != nil {
		t.Fatal(err)
	}
	w := e.rule(t, SystemRuleChildDone)
	if !w.PausedReason.Valid || w.PausedReason.String != wakeupPausedMaxFires {
		t.Fatalf("the once rule did not end after its run: pause %q", w.PausedReason.String)
	}
	task := e.lastTask(t, SystemRuleChildDone)
	if err := e.s.CheckClaim(ctx, task); err != nil {
		t.Fatalf("the cap-reaching run lost its claim to the pause: %v", err)
	}
	if err := e.s.CheckStart(ctx, task); err != nil {
		t.Fatalf("the cap-reaching run lost its start to the pause: %v", err)
	}
	// The pause still vetoes: no later input runs.
	if got := e.loadBuiltin(t, SystemRuleChildDone); got == nil || got.Eff.Fingerprint != w.ConfigFingerprint.String {
		t.Fatalf("the paused instance resolves a different configuration than it ran under")
	}
}

// A squad target joins only a leader task of that squad; any other run of the
// same agent leaves its facts for the rule's own leader run.
func TestBuiltinSquadTargetJoinsOnlyItsLeaderTask(t *testing.T) {
	setup := func(t *testing.T) (builtinEnv, string, string) {
		e := newBuiltinEnv(t)
		leader := e.f.privateAgentOwnedBy(t, e.f.UserID, "join-leader")
		squad := e.f.Squad(t, "join squad", leader)
		e.f.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id=$1", e.issue)
		e.f.Cleanup(t, "DELETE FROM issue_child_event WHERE parent_id=$1", e.issue)
		e.define(t, WakeupScopeProject, SystemRuleChildDone, fmt.Sprintf(`"target":{"type":"squad","id":%q}`, squad))
		return e, leader, squad
	}
	queued := func(e builtinEnv, leader string, extra testutil.Cols) string {
		cols := testutil.Cols{"issue_id": e.issue, "runtime_id": testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + leader + "')"),
			"originator_user_id": e.f.UserID, "accountable_user_id": e.f.UserID}
		for k, v := range extra {
			cols[k] = v
		}
		return e.f.Task(t, leader, cols)
	}
	closeChild := func(t *testing.T, e builtinEnv) {
		t.Helper()
		child := e.f.Issue(t, "child", testutil.Cols{"parent_issue_id": e.issue, "status": "in_progress"})
		if err := e.s.ProcessChildEvents(context.Background(), e.issue); err != nil {
			t.Fatal(err)
		}
		e.f.Exec(t, "UPDATE issue SET status='done' WHERE id=$1", child)
		if err := e.s.ProcessChildEvents(context.Background(), e.issue); err != nil {
			t.Fatal(err)
		}
	}
	// Facts wait for the squad's own leader task; a run that is not one must
	// not take them along, whichever claims first.
	for name, extra := range map[string]func(e builtinEnv, leader string) testutil.Cols{
		"a bare run of the leader": func(builtinEnv, string) testutil.Cols { return nil },
		"a leader task of another squad": func(e builtinEnv, leader string) testutil.Cols {
			return testutil.Cols{"is_leader_task": true, "squad_id": e.f.Squad(t, "other squad", leader)}
		},
	} {
		t.Run(name+" is not a carrier", func(t *testing.T) {
			e, leader, squad := setup(t)
			mine := queued(e, leader, testutil.Cols{"is_leader_task": true, "squad_id": squad})
			closeChild(t, e) // the facts wait for `mine`
			e.f.Exec(t, `UPDATE agent_task_queue SET status='cancelled' WHERE id=$1`, mine)
			foreign := queued(e, leader, extra(e, leader))
			if notes := wakeClaim(t, e.f, e.s, foreign); notes != "" {
				t.Fatalf("%s took the squad rule along: %q", name, notes)
			}
			if e.pending(t, SystemRuleChildDone) != 1 {
				t.Fatalf("the facts did not stay with the rule: %d pending", e.pending(t, SystemRuleChildDone))
			}
		})
	}
	t.Run("an incompatible waiting run does not hold the facts back", func(t *testing.T) {
		e, leader, _ := setup(t)
		queued(e, leader, nil)
		closeChild(t, e)
		if e.tasks(t, SystemRuleChildDone) != 1 {
			t.Fatalf("the rule did not start its own leader run beside an incompatible waiting run: %d runs", e.tasks(t, SystemRuleChildDone))
		}
		if got := e.lastTask(t, SystemRuleChildDone); !got.IsLeaderTask {
			t.Fatal("the rule's own run is not a leader task")
		}
	})
	t.Run("a leader task of the squad is a carrier", func(t *testing.T) {
		e, leader, squad := setup(t)
		mine := queued(e, leader, testutil.Cols{"is_leader_task": true, "squad_id": squad})
		closeChild(t, e)
		if e.tasks(t, SystemRuleChildDone) != 0 {
			t.Fatalf("the rule queued its own run beside a compatible waiting leader task: %d", e.tasks(t, SystemRuleChildDone))
		}
		if notes := wakeClaim(t, e.f, e.s, mine); notes == "" {
			t.Fatal("the squad's leader task did not take the rule along")
		}
	})
}

// A queued run's expiry window and issue filters are judged again when it is
// claimed and started, not only when it was captured and dispatched.
func TestBuiltinClaimAndStartRecheckExpiryAndIssueFilters(t *testing.T) {
	ctx := context.Background()
	refused := func(t *testing.T, e builtinEnv, task db.AgentTaskQueue, what string) {
		t.Helper()
		for name, check := range map[string]func(context.Context, db.AgentTaskQueue) error{"claim": e.s.CheckClaim, "start": e.s.CheckStart} {
			if err := check(ctx, task); !errors.Is(err, ErrWakeupForbidden) {
				t.Fatalf("%s after %s = %v, want ErrWakeupForbidden", name, what, err)
			}
		}
	}
	t.Run("expiry", func(t *testing.T) {
		e := newBuiltinEnv(t)
		e.define(t, WakeupScopeProject, SystemRulePRMerged, `"expiry":{"after_seconds":3600}`)
		e.mergedPR(t, 520)
		task := e.lastTask(t, SystemRulePRMerged)
		if err := e.s.CheckClaim(ctx, task); err != nil {
			t.Fatalf("claim inside the window: %v", err)
		}
		e.f.Exec(t, `UPDATE issue_wakeup SET created_at=now()-interval '2 hours' WHERE issue_id=$1 AND system_rule=$2`, e.issue, SystemRulePRMerged)
		refused(t, e, task, "the window ended")
	})
	t.Run("labels and priority", func(t *testing.T) {
		e := newBuiltinEnv(t)
		label := e.f.Insert(t, "issue_label", testutil.Cols{"workspace_id": e.f.WorkspaceID, "name": "wk-claim", "color": "#333333"})
		e.f.Exec(t, `INSERT INTO issue_to_label(issue_id,label_id) VALUES($1,$2)`, e.issue, label)
		e.f.Cleanup(t, `DELETE FROM issue_to_label WHERE issue_id=$1`, e.issue)
		e.f.Exec(t, `UPDATE issue SET priority='high' WHERE id=$1`, e.issue)
		e.define(t, WakeupScopeProject, SystemRulePRMerged, fmt.Sprintf(`"filters":{"labels":[%q],"priorities":["high"]}`, label))
		e.mergedPR(t, 521)
		task := e.lastTask(t, SystemRulePRMerged)
		if err := e.s.CheckClaim(ctx, task); err != nil {
			t.Fatalf("claim while eligible: %v", err)
		}
		e.f.Exec(t, `UPDATE issue SET priority='low' WHERE id=$1`, e.issue)
		refused(t, e, task, "the priority left the selection")
		e.f.Exec(t, `UPDATE issue SET priority='high' WHERE id=$1`, e.issue)
		if err := e.s.CheckClaim(ctx, task); err != nil {
			t.Fatalf("claim after eligibility returned: %v", err)
		}
		e.f.Exec(t, `DELETE FROM issue_to_label WHERE issue_id=$1`, e.issue)
		refused(t, e, task, "the label was removed")
	})
}

// A held configuration is not "no definitions": capturing under a hold keeps
// the instance's recorded configuration, revision and queued runs, and the
// fact waits.
func TestBuiltinCaptureUnderHoldKeepsTheInstance(t *testing.T) {
	e := newBuiltinEnv(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"instruction":"One"`)
	e.mergedPR(t, 530)
	before := e.rule(t, SystemRulePRMerged)
	queued := e.lastTask(t, SystemRulePRMerged)
	if !before.ConfigFingerprint.Valid {
		t.Fatal("no recorded configuration to preserve")
	}
	e.f.Exec(t, `UPDATE issue_wakeup_definition SET config=jsonb_set(config,'{aggregate_limit}','3'),revision=revision+1 WHERE workspace_id=$1 AND scope_kind='project' AND rule_key=$2`, e.f.WorkspaceID, SystemRulePRMerged)
	e.mergedPR(t, 531)
	after := e.rule(t, SystemRulePRMerged)
	if after.Revision != before.Revision || after.ConfigFingerprint != before.ConfigFingerprint {
		t.Fatalf("capture under a hold rebased the instance: revision %d→%d, fingerprint %q→%q", before.Revision, after.Revision, before.ConfigFingerprint.String, after.ConfigFingerprint.String)
	}
	if got := e.task(t, util.UUIDToString(queued.ID)); got.Status != "queued" {
		t.Fatalf("capture under a hold withdrew the queued run: %s", got.Status)
	}
	if e.pending(t, SystemRulePRMerged) != 1 || e.tasks(t, SystemRulePRMerged) != 1 {
		t.Fatalf("the held fact: %d pending, %d runs; want 1 pending and the original run only", e.pending(t, SystemRulePRMerged), e.tasks(t, SystemRulePRMerged))
	}
	// Resolving again with the hold gone retires nothing it should keep.
	e.f.Exec(t, `UPDATE issue_wakeup_definition SET config=config-'aggregate_limit',revision=revision+1 WHERE workspace_id=$1 AND scope_kind='project' AND rule_key=$2`, e.f.WorkspaceID, SystemRulePRMerged)
	if err := e.s.dispatchSystem(context.Background(), e.rule(t, SystemRulePRMerged)); err != nil {
		t.Fatal(err)
	}
}

// base_branch is a merged-PR filter: the failing-checks event carries no base
// branch, so the combination is refused rather than stored and never matched.
func TestBuiltinBaseBranchFilterIsRefusedForFailingChecks(t *testing.T) {
	cfg := func(rule, filters string) EffectiveWakeupConfig {
		p := patchOf(t, `"filters":`+filters)
		eff, err := ResolveWakeupConfig(WakeupResolveInput{RuleKey: rule, Workspace: &WakeupDefinition{Scope: WakeupScopeWorkspace, ScopeID: pgtype.UUID{}, Revision: 1, Patch: p}})
		if err != nil {
			t.Fatal(err)
		}
		return eff
	}
	if err := validateEffectiveWakeup(cfg(SystemRulePRChecksFailed, `{"base_branch":"main"}`)); !errors.Is(err, ErrWakeupInput) {
		t.Fatalf("base_branch on failing checks = %v, want an input error", err)
	}
	for _, ok := range []struct{ rule, filters string }{
		{SystemRulePRMerged, `{"base_branch":"main"}`},
		{SystemRulePRChecksFailed, `{"head_branch":"feature","ci":"failure"}`},
	} {
		if err := validateEffectiveWakeup(cfg(ok.rule, ok.filters)); err != nil {
			t.Fatalf("%s %s refused: %v", ok.rule, ok.filters, err)
		}
	}
	e := newBuiltinEnv(t)
	ref := WakeupScopeRef{Kind: WakeupScopeWorkspace, ID: parseTestUUID(t, e.f.WorkspaceID), WorkspaceID: parseTestUUID(t, e.f.WorkspaceID)}
	_, err := e.s.SaveWakeupDefinition(context.Background(), ref, e.owner, WakeupDefinitionWrite{RuleKey: SystemRulePRChecksFailed, Patch: patchOf(t, `"filters":{"base_branch":"main"}`)})
	if !errors.Is(err, ErrWakeupInput) {
		t.Fatalf("saving base_branch for failing checks = %v, want an input error", err)
	}
}
