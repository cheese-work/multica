package service

import (
	"context"
	"errors"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
)

// CHE-9123 (plan CHE-6971, integration review of f468f6e8): the live join keeps
// two guards the claim/start path already holds. It never joins a run that
// carries another rule's isolated firing, and it never joins a merge_eligibility
// firing, because the candidate is judged again at claim and start (CHE-5122).

// hostContext sets the context of the running run R.
func (e *liveEnv) hostContext(t *testing.T, ctx string) {
	t.Helper()
	e.f.Exec(t, `UPDATE agent_task_queue SET context=$2::jsonb WHERE id=$1`, e.host, ctx)
}

func TestLiveJoinRefusesRunCarryingAnotherRulesIsolatedFiring(t *testing.T) {
	const otherRule = `{"wakeup_id":"00000000-0000-4000-8000-000000000001","wakeup_isolated":true}`
	t.Run("warn_notes_nothing_and_runs_today", func(t *testing.T) {
		e := joinEnv(t)
		e.setMode(t, "warn")
		e.hostContext(t, otherRule)
		r := e.receipt(t)
		e.ranToday(t, e.pass(t), r)
		if n := e.wouldJoin(t); n != 0 {
			t.Fatalf("would_join_live rows = %d, want 0: an isolated host is never a join target", n)
		}
	})
	t.Run("block_takes_the_ordinary_path", func(t *testing.T) {
		e := joinEnv(t)
		e.hostContext(t, otherRule)
		r := e.receipt(t)
		e.ranToday(t, e.pass(t), r)
	})
}

// The guard is the isolated flag of another rule, nothing wider.
func TestLiveJoinStillJoinsAnEligibleHost(t *testing.T) {
	cases := map[string]func(e *liveEnv) string{
		"no_context":              func(e *liveEnv) string { return `{}` },
		"other_rule_not_isolated": func(e *liveEnv) string { return `{"wakeup_id":"00000000-0000-4000-8000-000000000001"}` },
		"own_rule_context":        func(e *liveEnv) string { return `{"wakeup_id":"` + util.UUIDToString(e.w.ID) + `"}` },
		"own_rule_isolated_context": func(e *liveEnv) string {
			return `{"wakeup_id":"` + util.UUIDToString(e.w.ID) + `","wakeup_isolated":true}`
		},
	}
	for name, ctxOf := range cases {
		t.Run(name, func(t *testing.T) {
			e := joinEnv(t)
			e.hostContext(t, ctxOf(e))
			e.receipt(t)
			e.pass(t)
			// The host may carry this rule's own id, so count the runs besides it.
			if n := e.f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE context->>'wakeup_id'=$1 AND id<>$2`, util.UUIDToString(e.w.ID), e.host); n != 0 {
				t.Fatalf("runs besides the host = %d, want 0: the firing joins R", n)
			}
			if n := e.items(t); n != 1 {
				t.Fatalf("items = %d, want 1", n)
			}
		})
	}
}

// A merge_eligibility firing never joins a live run. It takes the ordinary path,
// whose run carries wakeup_merge, so CheckClaim and CheckStart refuse a stale
// candidate. Nothing is injected into R.
func TestLiveJoinLeavesMergeEligibilityToClaimAndStart(t *testing.T) {
	for _, mode := range []string{"warn", "block"} {
		t.Run(mode, func(t *testing.T) {
			m := newMergeFix(t, nil, "")
			e := &liveEnv{f: m.f, s: m.s, issue: m.issue, agent: m.agent, w: m.w}
			e.host = e.run(t, m.agent)
			m.f.Exec(t, `INSERT INTO task_pr_event_capability(task_id,workspace_id,issue_id,capability) VALUES($1,$2,$3,'task-supplement-pr-event-v2')`, e.host, m.f.WorkspaceID, m.issue)
			m.f.Cleanup(t, `DELETE FROM task_pr_event_delivery WHERE issue_id=$1`, m.issue)
			m.f.Cleanup(t, `DELETE FROM task_pr_event_capability WHERE issue_id=$1`, m.issue)
			e.agentCreated(t)
			// merge settings stay in place; only the join mode is added.
			m.f.Exec(t, `UPDATE workspace SET settings=settings||$2::jsonb WHERE id=$1`, m.f.WorkspaceID, `{"wakeup_live_join":{"mode":"`+mode+`"}}`)

			got := m.tick(t)
			if n := e.items(t); n != 0 {
				t.Fatalf("items = %d, want 0: nothing is injected into R", n)
			}
			if mode == "warn" && e.wouldJoin(t) != 0 {
				t.Fatal("warn counted would_join_live for a merge_eligibility firing")
			}
			if n := m.runs(t); n != 1 || got.FireCount != 1 {
				t.Fatalf("runs = %d fire_count = %d, want the ordinary run", n, got.FireCount)
			}
			task := m.lastTask(t)
			if err := m.s.CheckClaim(context.Background(), task); err != nil {
				t.Fatalf("CheckClaim on the fresh candidate = %v, want nil", err)
			}
			// The candidate goes stale before the run is claimed.
			m.f.Exec(t, "UPDATE github_pull_request SET head_sha='b',snapshot_head_sha='b' WHERE id=$1", m.pr)
			if err := m.s.CheckClaim(context.Background(), task); !errors.Is(err, ErrWakeupForbidden) {
				t.Fatalf("CheckClaim on a stale candidate = %v, want ErrWakeupForbidden", err)
			}
			if err := m.s.CheckStart(context.Background(), task); !errors.Is(err, ErrWakeupForbidden) {
				t.Fatalf("CheckStart on a stale candidate = %v, want ErrWakeupForbidden", err)
			}
		})
	}
}
