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

// A rule whose stored configuration cannot be executed is held at every stage:
// the fact stays pending, no run is queued, nothing is reserved on a join, a
// queued run cannot be claimed, and the stored definition is left untouched.
func TestBuiltinHeldConfigurationBlocksDispatchClaimAndJoin(t *testing.T) {
	for _, bad := range []struct{ name, config string }{
		{"unreadable version", `{"v":99}`},
		{"unknown field", `{"v":1,"future":true}`},
		{"active run", `{"v":1,"active_run":"defer"}`},
	} {
		for _, phase := range []string{"dispatch", "claim", "join"} {
			t.Run(bad.name+"/"+phase, func(t *testing.T) {
				e := newBuiltinEnv(t)
				ctx := context.Background()
				var waiting string
				if phase == "join" {
					waiting = wakeWaitingRun(t, e.f, e.issue, e.agent, e.f.UserID)
				}
				if phase != "dispatch" {
					e.mergedPR(t, 400)
				}
				// The definition appears after the fact was captured (or before it, for dispatch).
				if err := insertDefinition(t, e.f, "issue", util.UUIDToString(e.issue), SystemRulePRMerged, false, bad.config); err != nil {
					t.Fatal(err)
				}
				switch phase {
				case "dispatch":
					e.mergedPR(t, 400)
					if e.tasks(t, SystemRulePRMerged) != 0 || e.pending(t, SystemRulePRMerged) != 1 {
						t.Fatalf("held rule: %d runs, %d pending; want 0 and 1", e.tasks(t, SystemRulePRMerged), e.pending(t, SystemRulePRMerged))
					}
				case "claim":
					if err := e.s.CheckClaim(ctx, e.lastTask(t, SystemRulePRMerged)); !errors.Is(err, ErrWakeupForbidden) {
						t.Fatalf("CheckClaim under a held configuration = %v, want ErrWakeupForbidden", err)
					}
				case "join":
					if notes := wakeClaim(t, e.f, e.s, waiting); notes != "" {
						t.Fatalf("a held rule joined the claim: %q", notes)
					}
					if got := e.f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.issue_id=$1 AND r.task_id IS NOT NULL AND r.processed_at IS NULL`, e.issue); got != 0 {
						t.Fatalf("a held rule reserved %d receipts", got)
					}
				}
				if got := e.f.Count(t, `SELECT count(*) FROM issue_wakeup_definition WHERE workspace_id=$1 AND config::text=$2::jsonb::text`, e.f.WorkspaceID, bad.config); got != 1 {
					t.Fatalf("the held definition was rewritten (%d unchanged copies)", got)
				}
			})
		}
	}
}

// A held child_done does not hold the PR rules on the same issue.
func TestBuiltinHeldRuleDoesNotHoldOtherRules(t *testing.T) {
	e := newBuiltinEnv(t)
	if err := insertDefinition(t, e.f, "workspace", e.f.WorkspaceID, SystemRuleChildDone, false, `{"v":1,"schedule":{"cron":"* * * * *"}}`); err != nil {
		t.Fatal(err)
	}
	e.mergedPR(t, 410)
	if e.tasks(t, SystemRulePRMerged) != 1 {
		t.Fatalf("an unrelated rule queued %d runs, want 1", e.tasks(t, SystemRulePRMerged))
	}
}

// A definition that equals what the legacy row already says changes nothing
// about what runs.
func TestBuiltinBackfilledDefinitionRunsLikeTheLegacyRow(t *testing.T) {
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
		t.Fatalf("a matching backfilled definition queued %d runs, want 1", n)
	}
}

// A default-derived runtime row has no system rule, so only its origin metadata
// tells it from a local wakeup. Custom rules execute from a later layer, so
// dispatch, claim and join all hold it.
func TestBuiltinDefaultDerivedInstancesStayHeld(t *testing.T) {
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
	t.Run("dispatch", func(t *testing.T) {
		f, s, issue, _, w := newRule(t)
		mark(t, f, w.ID)
		f.Comment(t, util.UUIDToString(issue), "input")
		wakeTick(t, f, s, w.ID)
		if n := wakeRuns(t, f, w.ID); n != 0 || pendingOf(t, f, w) != 1 {
			t.Fatalf("default-derived instance: %d runs, %d pending; want 0 and 1", n, pendingOf(t, f, w))
		}
	})
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
		if notes := wakeClaim(t, f, s, waiting); notes != "" || pendingOf(t, f, w) != 1 {
			t.Fatalf("default-derived instance joined the claim: %q (%d pending)", notes, pendingOf(t, f, w))
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

// A named target that is still authorized joins the run it is the agent of; the
// refusal test above is only meaningful next to this.
func TestBuiltinAuthorizedNamedTargetJoinsItsRun(t *testing.T) {
	e := newBuiltinEnv(t)
	named := e.f.privateAgentOwnedBy(t, e.f.UserID, "named-join-ok")
	e.f.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id=$1", e.issue)
	e.define(t, WakeupScopeWorkspace, SystemRulePRMerged, fmt.Sprintf(`"target":{"type":"agent","id":%q}`, named))
	waiting := wakeWaitingRun(t, e.f, e.issue, named, e.f.UserID)
	e.mergedPR(t, 420)
	if notes := wakeClaim(t, e.f, e.s, waiting); !strings.Contains(notes, "A linked pull request has merged") {
		t.Fatalf("an authorized named target did not join its waiting run: %q", notes)
	}
}
