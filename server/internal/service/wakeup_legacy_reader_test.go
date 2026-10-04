package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func defRow(scope string, scopeID pgtype.UUID, key, config string) db.IssueWakeupDefinition {
	return db.IssueWakeupDefinition{ScopeKind: scope, ScopeID: scopeID, RuleKey: key, Config: []byte(config), Revision: 1}
}

// The gate is the recovery floor for a rollback to this build: any stored
// definition it cannot execute with legacy semantics suspends legacy dispatch
// of that rule, and anything it can already express leaves dispatch unchanged.
func TestLegacyDispatchGateVerdicts(t *testing.T) {
	issue := parseTestUUIDRaw("11111111-1111-4111-8111-111111111111")
	project := parseTestUUIDRaw("22222222-2222-4222-8222-222222222222")
	ws := parseTestUUIDRaw("33333333-3333-4333-8333-333333333333")
	customized := pgtype.Timestamptz{Valid: true}
	plain := db.IssueWakeup{IssueID: issue, Enabled: true, SystemRule: systemRuleText(SystemRuleChildDone)}
	customOff := db.IssueWakeup{IssueID: issue, Enabled: false, SystemRule: systemRuleText(SystemRuleChildDone), CustomizedAt: customized, Instruction: "hi"}

	for _, tc := range []struct {
		name   string
		rule   string
		legacy db.IssueWakeup
		rows   []db.IssueWakeupDefinition
		want   LegacySuspendReason
	}{
		{"no definitions", SystemRuleChildDone, plain, nil, ""},
		{"workspace name label only", SystemRuleChildDone, plain, []db.IssueWakeupDefinition{defRow("workspace", ws, SystemRuleChildDone, `{"v":1,"name":"Parents"}`)}, ""},
		{"empty workspace patch", SystemRuleChildDone, plain, []db.IssueWakeupDefinition{defRow("workspace", ws, SystemRuleChildDone, `{"v":1}`)}, ""},
		{"backfilled issue definition equals the legacy row", SystemRuleChildDone, customOff, []db.IssueWakeupDefinition{defRow("issue", issue, SystemRuleChildDone, `{"v":1,"enabled":false,"instruction":"hi"}`)}, ""},
		{"null clear equals unset", SystemRuleChildDone, plain, []db.IssueWakeupDefinition{defRow("issue", issue, SystemRuleChildDone, `{"v":1,"instruction":null}`)}, ""},
		{"project definition", SystemRulePRMerged, plain, []db.IssueWakeupDefinition{defRow("project", project, SystemRulePRMerged, `{"v":1}`)}, LegacySuspendProjectScope},
		{"unreadable version", SystemRuleChildDone, plain, []db.IssueWakeupDefinition{defRow("issue", issue, SystemRuleChildDone, `{"v":99}`)}, LegacySuspendUnreadable},
		{"unknown field", SystemRuleChildDone, plain, []db.IssueWakeupDefinition{defRow("workspace", ws, SystemRuleChildDone, `{"v":1,"future":true}`)}, LegacySuspendUnreadable},
		{"unknown scope", SystemRuleChildDone, plain, []db.IssueWakeupDefinition{defRow("galaxy", ws, SystemRuleChildDone, `{"v":1}`)}, LegacySuspendUnreadable},
		{"workspace trigger", SystemRulePRMerged, plain, []db.IssueWakeupDefinition{defRow("workspace", ws, SystemRulePRMerged, `{"v":1,"trigger":{"type":"cron"}}`)}, LegacySuspendUnsupported},
		{"issue aggregate cap", SystemRulePRMerged, plain, []db.IssueWakeupDefinition{defRow("issue", issue, SystemRulePRMerged, `{"v":1,"aggregate_limit":3}`)}, LegacySuspendUnsupported},
		{"issue filters cleared still unsupported", SystemRulePRMerged, plain, []db.IssueWakeupDefinition{defRow("issue", issue, SystemRulePRMerged, `{"v":1,"filters":null}`)}, LegacySuspendUnsupported},
		{"workspace enabled conflicts with the alias", SystemRulePRMerged, plain, []db.IssueWakeupDefinition{defRow("workspace", ws, SystemRulePRMerged, `{"v":1,"enabled":false}`)}, LegacySuspendAliasConflict},
		{"child-done workspace instruction conflicts with the alias", SystemRuleChildDone, plain, []db.IssueWakeupDefinition{defRow("workspace", ws, SystemRuleChildDone, `{"v":1,"instruction":"x"}`)}, LegacySuspendAliasConflict},
		// PR text is always built from the receipt facts; a stored PR instruction, even a clear, cannot be executed.
		{"PR merged workspace instruction", SystemRulePRMerged, plain, []db.IssueWakeupDefinition{defRow("workspace", ws, SystemRulePRMerged, `{"v":1,"instruction":"x"}`)}, LegacySuspendUnsupported},
		{"PR checks workspace instruction", SystemRulePRChecksFailed, plain, []db.IssueWakeupDefinition{defRow("workspace", ws, SystemRulePRChecksFailed, `{"v":1,"instruction":"x"}`)}, LegacySuspendUnsupported},
		{"PR workspace instruction clear", SystemRulePRMerged, plain, []db.IssueWakeupDefinition{defRow("workspace", ws, SystemRulePRMerged, `{"v":1,"instruction":null}`)}, LegacySuspendUnsupported},
		{"PR issue instruction", SystemRulePRChecksFailed, plain, []db.IssueWakeupDefinition{defRow("issue", issue, SystemRulePRChecksFailed, `{"v":1,"instruction":"x"}`)}, LegacySuspendIssueDiverges},
		{"issue override the legacy row does not carry", SystemRuleChildDone, plain, []db.IssueWakeupDefinition{defRow("issue", issue, SystemRuleChildDone, `{"v":1,"enabled":false}`)}, LegacySuspendIssueDiverges},
		{"issue instruction differs from the legacy row", SystemRuleChildDone, customOff, []db.IssueWakeupDefinition{defRow("issue", issue, SystemRuleChildDone, `{"v":1,"enabled":false,"instruction":"other"}`)}, LegacySuspendIssueDiverges},
		{"one bad definition among good ones", SystemRuleChildDone, plain, []db.IssueWakeupDefinition{
			defRow("workspace", ws, SystemRuleChildDone, `{"v":1}`), defRow("issue", issue, SystemRuleChildDone, `{"v":2}`)}, LegacySuspendUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			legacy := tc.legacy
			legacy.SystemRule = systemRuleText(tc.rule)
			got := LegacyDispatchGate(tc.rule, legacy, tc.rows)
			if got.Allowed != (tc.want == "") || got.Reason != tc.want {
				t.Fatalf("verdict = %+v, want reason %q", got, tc.want)
			}
		})
	}
	// A default-derived instance (no system rule, default-origin metadata) is
	// a custom rule this build cannot execute: held whatever its definitions say.
	custom := "7f1d6a2e-3b44-4d8e-9a55-0c6b1f2e8d10"
	derived := db.IssueWakeup{IssueID: issue, Enabled: true, DefaultRuleKey: pgtype.Text{String: custom, Valid: true},
		DefaultScopeKind: pgtype.Text{String: "workspace", Valid: true}, DefaultScopeID: ws}
	for name, rows := range map[string][]db.IssueWakeupDefinition{
		"no definitions": nil, "unreadable": {defRow("workspace", ws, custom, `{"v":99}`)}, "readable": {defRow("workspace", ws, custom, `{"v":1,"enabled":true}`)},
	} {
		if v := LegacyDispatchGate(custom, derived, rows); v.Allowed || v.Reason != LegacySuspendDefaultInstance {
			t.Fatalf("default-derived instance with %s = %+v, want suspended", name, v)
		}
	}
	// A genuine local wakeup (no origin metadata) is unchanged.
	if v := LegacyDispatchGate("", db.IssueWakeup{IssueID: issue, Enabled: true}, nil); !v.Allowed {
		t.Fatalf("local wakeup gate = %+v, want allowed", v)
	}
}

func prGateFixture(t *testing.T) (principalFixture, *IssueWakeupService, pgtype.UUID, string, pgtype.UUID) {
	t.Helper()
	f, s, issue, agent := conditionFixture(t)
	assignPRWakeupIssue(t, f, issue, agent)
	project := f.Insert(t, "project", testutil.Cols{"workspace_id": f.WorkspaceID, "title": "gate project"})
	f.Exec(t, `UPDATE issue SET project_id=$2 WHERE id=$1`, issue, project)
	f.Cleanup(t, "DELETE FROM issue_wakeup_definition WHERE workspace_id=$1", f.WorkspaceID)
	return f, s, issue, agent, parseTestUUID(t, project)
}

var prMergedInput = PullRequestWakeupInput{Rule: SystemRulePRMerged, RepoOwner: "acme", RepoName: "widget", Number: 157, MergeCommit: "merge-sha", URL: "https://github.com/acme/widget/pull/157"}

func TestSuspendedLegacyDispatchHoldsReceiptsAndResumesWhenDefinitionIsGone(t *testing.T) {
	f, s, issue, _, project := prGateFixture(t)
	ctx := context.Background()
	if err := insertDefinition(t, f, "project", util.UUIDToString(project), SystemRulePRMerged, false, `{"v":1,"enabled":false}`); err != nil {
		t.Fatal(err)
	}
	if err := s.TriggerPullRequestWakeup(ctx, issue, prMergedInput); err != nil {
		t.Fatal(err)
	}
	if got := f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND context->>'wakeup_system'=$2`, issue, SystemRulePRMerged); got != 0 {
		t.Fatalf("suspended rule queued %d runs, want 0", got)
	}
	w, err := f.q.GetSystemWakeup(ctx, db.GetSystemWakeupParams{IssueID: issue, SystemRule: systemRuleText(SystemRulePRMerged)})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1 AND processed_at IS NULL`, w.ID); got != 1 {
		t.Fatalf("suspension must hold the captured fact, pending receipts = %d, want 1", got)
	}
	f.Exec(t, `DELETE FROM issue_wakeup_definition WHERE workspace_id=$1`, f.WorkspaceID)
	if err := s.dispatchSystem(ctx, w); err != nil {
		t.Fatal(err)
	}
	if got := f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND context->>'wakeup_system'=$2`, issue, SystemRulePRMerged); got != 1 {
		t.Fatalf("after the definition is gone the held fact must dispatch once, runs = %d", got)
	}
}

func TestSuspendedLegacyDispatchBlocksClaimAndJoin(t *testing.T) {
	for _, phase := range []string{"claim", "join"} {
		t.Run(phase, func(t *testing.T) {
			f, s, issue, agent, _ := prGateFixture(t)
			ctx := context.Background()
			var waiting string
			if phase == "join" {
				waiting = wakeWaitingRun(t, f, issue, agent, f.UserID)
			}
			if err := s.TriggerPullRequestWakeup(ctx, issue, prMergedInput); err != nil {
				t.Fatal(err)
			}
			// An unreadable issue definition appears after the fact was captured.
			if err := insertDefinition(t, f, "issue", util.UUIDToString(issue), SystemRulePRMerged, false, `{"v":99}`); err != nil {
				t.Fatal(err)
			}
			if phase == "claim" {
				var taskID pgtype.UUID
				if err := f.Pool.QueryRow(ctx, `SELECT id FROM agent_task_queue WHERE issue_id=$1 AND context->>'wakeup_system'=$2`, issue, SystemRulePRMerged).Scan(&taskID); err != nil {
					t.Fatal(err)
				}
				task, err := f.q.GetAgentTask(ctx, taskID)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.CheckClaim(ctx, task); !errors.Is(err, ErrWakeupForbidden) {
					t.Fatalf("CheckClaim under suspension = %v, want ErrWakeupForbidden", err)
				}
				return
			}
			if notes := wakeClaim(t, f, s, waiting); notes != "" {
				t.Fatalf("suspended rule joined the claim: %q", notes)
			}
			if got := f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.issue_id=$1 AND w.system_rule=$2 AND r.task_id IS NOT NULL AND r.processed_at IS NULL`, issue, SystemRulePRMerged); got != 0 {
				t.Fatalf("suspended receipt reservations = %d, want 0", got)
			}
		})
	}
}

func TestSuspendedLegacyDispatchSuspendsChildDoneButNotOtherRules(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	ctx := context.Background()
	f.Cleanup(t, "DELETE FROM issue_wakeup_definition WHERE workspace_id=$1", f.WorkspaceID)
	f.Exec(t, "UPDATE issue SET status='in_progress',assignee_type='agent',assignee_id=$2 WHERE id=$1", issue, agent)
	child := f.Issue(t, "child", testutil.Cols{"parent_issue_id": issue, "status": "in_progress"})
	f.Cleanup(t, "DELETE FROM issue_child_event WHERE parent_id=$1", issue)
	if err := s.ProcessChildEvents(ctx, issue); err != nil {
		t.Fatal(err)
	}
	// Only child_done is controlled by this definition.
	if err := insertDefinition(t, f, "workspace", f.WorkspaceID, SystemRuleChildDone, false, `{"v":1,"schedule":{"cron":"* * * * *"}}`); err != nil {
		t.Fatal(err)
	}
	// A PR rule on the same issue is not controlled by it and still runs.
	if err := s.TriggerPullRequestWakeup(ctx, issue, prMergedInput); err != nil {
		t.Fatal(err)
	}
	if got := f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND context->>'wakeup_system'=$2`, issue, SystemRulePRMerged); got != 1 {
		t.Fatalf("an unrelated rule queued %d runs, want 1", got)
	}
	// The PR run finishes, so a child_done run would be a run of its own.
	f.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=clock_timestamp() WHERE issue_id=$1`, issue)
	f.Exec(t, "UPDATE issue SET status='done' WHERE id=$1", child)
	if err := s.ProcessChildEvents(ctx, issue); err != nil {
		t.Fatal(err)
	}
	w, err := f.q.GetSystemWakeup(ctx, db.GetSystemWakeupParams{IssueID: issue, SystemRule: systemRuleText(SystemRuleChildDone)})
	if err != nil {
		t.Fatal(err)
	}
	if n := wakeRuns(t, f, w.ID); n != 0 {
		t.Fatalf("suspended child_done queued %d runs, want 0", n)
	}
	// Deleting the definition resumes the held child_done fact.
	f.Exec(t, `DELETE FROM issue_wakeup_definition WHERE workspace_id=$1`, f.WorkspaceID)
	if err := s.dispatchSystem(ctx, w); err != nil {
		t.Fatal(err)
	}
	if n := wakeRuns(t, f, w.ID); n != 1 {
		t.Fatalf("resumed child_done runs = %d, want 1", n)
	}
}

// A definition that equals what the legacy row already says changes nothing.
func TestBackfilledDefinitionLeavesLegacyDispatchUnchanged(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	ctx := context.Background()
	f.Cleanup(t, "DELETE FROM issue_wakeup_definition WHERE workspace_id=$1", f.WorkspaceID)
	f.Exec(t, "UPDATE issue SET status='in_progress',assignee_type='agent',assignee_id=$2 WHERE id=$1", issue, agent)
	child := f.Issue(t, "child", testutil.Cols{"parent_issue_id": issue, "status": "in_progress"})
	f.Cleanup(t, "DELETE FROM issue_child_event WHERE parent_id=$1", issue)
	on, text := true, "same as before"
	customizeChildDone(t, s, issue, &on, &text)
	if _, err := s.RunLegacyWakeupBackfill(ctx, parseTestUUID(t, f.WorkspaceID), 10); err != nil {
		t.Fatal(err)
	}
	if got := f.Count(t, `SELECT count(*) FROM issue_wakeup_definition WHERE workspace_id=$1`, f.WorkspaceID); got != 1 {
		t.Fatalf("backfilled definitions = %d, want 1", got)
	}
	f.Exec(t, "UPDATE issue SET status='done' WHERE id=$1", child)
	if err := s.ProcessChildEvents(ctx, issue); err != nil {
		t.Fatal(err)
	}
	w, err := f.q.GetSystemWakeup(ctx, db.GetSystemWakeupParams{IssueID: issue, SystemRule: systemRuleText(SystemRuleChildDone)})
	if err != nil {
		t.Fatal(err)
	}
	if n := wakeRuns(t, f, w.ID); n != 1 {
		t.Fatalf("legacy dispatch with a matching backfilled definition queued %d runs, want 1", n)
	}
}

func TestSuspendedLegacyDispatchHoldsPRWorkspaceInstruction(t *testing.T) {
	for _, rule := range []string{SystemRulePRMerged, SystemRulePRChecksFailed} {
		for _, phase := range []string{"dispatch", "claim", "join"} {
			t.Run(rule+"/"+phase, func(t *testing.T) {
				f, s, issue, agent := conditionFixture(t)
				assignPRWakeupIssue(t, f, issue, agent)
				f.Cleanup(t, "DELETE FROM issue_wakeup_definition WHERE workspace_id=$1", f.WorkspaceID)
				ctx := context.Background()
				input := prMergedInput
				input.Rule = rule
				if rule == SystemRulePRChecksFailed {
					input.MergeCommit, input.HeadSHA, input.Conclusion = "", "head-sha", "FAILURE"
				}
				var waiting string
				if phase == "join" {
					waiting = wakeWaitingRun(t, f, issue, agent, f.UserID)
				}
				if phase != "dispatch" {
					if err := s.TriggerPullRequestWakeup(ctx, issue, input); err != nil {
						t.Fatal(err)
					}
				}
				if err := insertDefinition(t, f, "workspace", f.WorkspaceID, rule, false, `{"v":1,"instruction":"scoped text"}`); err != nil {
					t.Fatal(err)
				}
				pending := `SELECT count(*) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.issue_id=$1 AND w.system_rule=$2 AND r.processed_at IS NULL`
				switch phase {
				case "dispatch":
					if err := s.TriggerPullRequestWakeup(ctx, issue, input); err != nil {
						t.Fatal(err)
					}
					if got := f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND context->>'wakeup_system'=$2`, issue, rule); got != 0 {
						t.Fatalf("held rule queued %d runs", got)
					}
					if got := f.Count(t, pending, issue, rule); got != 1 {
						t.Fatalf("pending receipts = %d, want 1", got)
					}
				case "claim":
					var taskID pgtype.UUID
					if err := f.Pool.QueryRow(ctx, `SELECT id FROM agent_task_queue WHERE issue_id=$1 AND context->>'wakeup_system'=$2`, issue, rule).Scan(&taskID); err != nil {
						t.Fatal(err)
					}
					task, err := f.q.GetAgentTask(ctx, taskID)
					if err != nil {
						t.Fatal(err)
					}
					if err := s.CheckClaim(ctx, task); !errors.Is(err, ErrWakeupForbidden) {
						t.Fatalf("CheckClaim = %v, want ErrWakeupForbidden", err)
					}
				case "join":
					if notes := wakeClaim(t, f, s, waiting); notes != "" {
						t.Fatalf("held rule joined the claim: %q", notes)
					}
					if got := f.Count(t, pending, issue, rule); got != 1 {
						t.Fatalf("pending receipts = %d, want 1", got)
					}
				}
			})
		}
	}
}

// A default-derived runtime row has no system rule, so only its origin
// metadata tells it from a local wakeup. This build cannot execute the scoped
// definition it came from, so dispatch, claim and join all hold it.
func TestSuspendedLegacyDispatchHoldsDefaultDerivedInstances(t *testing.T) {
	custom := "7f1d6a2e-3b44-4d8e-9a55-0c6b1f2e8d10"
	mark := func(t *testing.T, f principalFixture, id pgtype.UUID) {
		t.Helper()
		f.Exec(t, `UPDATE issue_wakeup SET default_rule_key=$2,default_scope_kind='workspace',default_scope_id=workspace_id WHERE id=$1`, id, custom)
	}
	newRule := func(t *testing.T) (principalFixture, *IssueWakeupService, pgtype.UUID, string, db.IssueWakeup) {
		t.Helper()
		f, s, issue, agent := conditionFixture(t)
		f.Cleanup(t, "DELETE FROM issue_wakeup_definition WHERE workspace_id=$1", f.WorkspaceID)
		w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", EventTypes: []string{"comment.created"}, Instruction: "derived text"})
		return f, s, issue, agent, w
	}
	pendingOf := func(t *testing.T, f principalFixture, w db.IssueWakeup) int {
		t.Helper()
		return f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1 AND processed_at IS NULL`, w.ID)
	}
	for _, defn := range []string{`{"v":99}`, `{"v":1,"enabled":false}`} {
		t.Run("dispatch/"+defn, func(t *testing.T) {
			f, s, issue, _, w := newRule(t)
			mark(t, f, w.ID)
			if err := insertDefinition(t, f, "workspace", f.WorkspaceID, custom, true, defn); err != nil {
				t.Fatal(err)
			}
			f.Comment(t, util.UUIDToString(issue), "input")
			wakeTick(t, f, s, w.ID)
			if n := wakeRuns(t, f, w.ID); n != 0 {
				t.Fatalf("default-derived instance queued %d runs, want 0", n)
			}
			if got := pendingOf(t, f, w); got != 1 {
				t.Fatalf("pending receipts = %d, want 1", got)
			}
		})
	}
	t.Run("claim", func(t *testing.T) {
		f, s, issue, _, w := newRule(t)
		f.Comment(t, util.UUIDToString(issue), "input")
		wakeTick(t, f, s, w.ID)
		var taskID pgtype.UUID
		if err := f.Pool.QueryRow(context.Background(), `SELECT id FROM agent_task_queue WHERE context->>'wakeup_id'=$1`, util.UUIDToString(w.ID)).Scan(&taskID); err != nil {
			t.Fatal(err)
		}
		task, err := f.q.GetAgentTask(context.Background(), taskID)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.CheckClaim(context.Background(), task); err != nil {
			t.Fatalf("local wakeup claim = %v, want allowed", err)
		}
		mark(t, f, w.ID)
		if err := s.CheckClaim(context.Background(), task); !errors.Is(err, ErrWakeupForbidden) {
			t.Fatalf("default-derived claim = %v, want ErrWakeupForbidden", err)
		}
	})
	t.Run("join", func(t *testing.T) {
		f, s, issue, agent, w := newRule(t)
		mark(t, f, w.ID)
		waiting := wakeWaitingRun(t, f, issue, agent, f.UserID)
		f.Comment(t, util.UUIDToString(issue), "input")
		wakeTick(t, f, s, w.ID)
		if notes := wakeClaim(t, f, s, waiting); notes != "" {
			t.Fatalf("default-derived instance joined the claim: %q", notes)
		}
		if got := pendingOf(t, f, w); got != 1 {
			t.Fatalf("pending receipts = %d, want 1", got)
		}
	})
	t.Run("local wakeups are unchanged", func(t *testing.T) {
		f, s, issue, agent, w := newRule(t)
		waiting := wakeWaitingRun(t, f, issue, agent, f.UserID)
		f.Comment(t, util.UUIDToString(issue), "input")
		wakeTick(t, f, s, w.ID)
		if notes := wakeClaim(t, f, s, waiting); !strings.Contains(notes, "derived text") {
			t.Fatalf("a local wakeup must still join: %q", notes)
		}
	})
}
