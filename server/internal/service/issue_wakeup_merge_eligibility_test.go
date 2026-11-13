package service

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type fakeMergeFacts struct {
	facts MergeFacts
	err   error
	calls []MergeCandidate
}

func (f *fakeMergeFacts) MergeFacts(_ context.Context, c MergeCandidate) (MergeFacts, error) {
	f.calls = append(f.calls, c)
	return f.facts, f.err
}

const mergePolicy = `{"merge_eligibility":{"revision":"p1","max_fact_age_seconds":300,
	"security_gates":["success","disabled"],
	"escalation_path_patterns":[{"pattern":"(?i)(^|/)migrations?/"},{"pattern":"(?i)secur|token","except":"(?i)_test\\.go$"}],
	"escalation_labels":["spend","design"],
	"gate_path_patterns":["(^|/)\\.github/workflows/"]}}`

type mergeFix struct {
	f     principalFixture
	s     *IssueWakeupService
	issue pgtype.UUID
	agent string
	pr    string
	facts *fakeMergeFacts
	clock time.Time
	w     db.IssueWakeup
}

func eligibleFacts(observed time.Time) MergeFacts {
	return MergeFacts{
		Tier: ptr(1), ClassifierTrusted: ptr(true), CoverageComplete: ptr(true), PRBound: ptr(true),
		ChecksGreen: ptr(true), SecurityGate: "success", CrossLabPass: ptr(true),
		Paused: ptr(false), MainRedPaused: ptr(false), MergerIsAuthor: ptr(false),
		Paths: []string{"server/internal/service/foo.go"}, Labels: ptr([]string{}),
		ScopeClassified: ptr(true), GateVetoElapsed: ptr(true), GateVetoed: ptr(false),
		Evidence: "receipt-1", ObservedAt: observed,
	}
}

// newMergeFix registers a continuous merge_eligibility rule on an issue that
// links one open, green, mergeable PR whose facts are eligible.
func newMergeFix(t *testing.T, mutateFacts func(*MergeFacts), prClause string) *mergeFix {
	t.Helper()
	f, s, issue, agent := conditionFixture(t)
	s.Tasks.PRSnapshotsEnabled = true
	fix := &mergeFix{f: f, s: s, issue: issue, agent: agent, clock: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)}
	fix.facts = &fakeMergeFacts{facts: eligibleFacts(fix.clock)}
	if mutateFacts != nil {
		mutateFacts(&fix.facts.facts)
	}
	s.MergeFacts = fix.facts
	s.Now = func() time.Time { return fix.clock }
	f.Exec(t, "UPDATE workspace SET settings=settings||$2::jsonb WHERE id=$1", f.WorkspaceID, mergePolicy)
	fix.pr = f.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": f.WorkspaceID, "installation_id": 1,
		"repo_owner": "fixture", "repo_name": "merge", "pr_number": 9,
		"title": "PR", "state": "open", "html_url": "https://example.test/pr/9",
		"pr_created_at": "2026-01-01T00:00:00Z", "pr_updated_at": "2026-01-01T00:00:00Z",
		"head_sha": "a", "snapshot_head_sha": "a", "snapshot_base_ref": "main",
		"api_mergeable": "MERGEABLE", "checks_rollup_state": "SUCCESS",
		"snapshot_fetched_at": "2026-11-12T09:59:00Z",
	})
	f.Cleanup(t, "DELETE FROM github_dispatch_halt WHERE workspace_id=$1", f.WorkspaceID)
	f.Cleanup(t, "DELETE FROM issue_pull_request WHERE pull_request_id=$1", fix.pr)
	f.Exec(t, "INSERT INTO issue_pull_request(issue_id,pull_request_id) VALUES($1,$2)", issue, fix.pr)
	if prClause != "" {
		f.Exec(t, "UPDATE github_pull_request SET "+prClause+" WHERE id=$1", fix.pr)
	}
	fix.w = wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", Instruction: "Merge when eligible",
		Condition: condition(t, map[string]any{"type": "merge_eligibility"})})
	// Pause variables come from the refresher; start with both read as unset.
	s.PauseVars = &fakePauseVars{}
	f.Exec(t, "DELETE FROM merge_pause_state WHERE repo_owner='fixture'")
	f.Cleanup(t, "DELETE FROM merge_pause_state WHERE repo_owner='fixture'")
	f.Cleanup(t, "DELETE FROM merge_veto_deadline WHERE workspace_id=$1", f.WorkspaceID)
	if err := s.RefreshMergePauses(context.Background()); err != nil {
		t.Fatal(err)
	}
	return fix
}

func (m *mergeFix) tick(t *testing.T) db.IssueWakeup {
	t.Helper()
	return wakeTick(t, m.f, m.s, m.w.ID)
}

func (m *mergeFix) runs(t *testing.T) int {
	t.Helper()
	return wakeRuns(t, m.f, m.w.ID)
}

func (m *mergeFix) finishRuns(t *testing.T) {
	t.Helper()
	m.f.Exec(t, "UPDATE agent_task_queue SET status='completed',completed_at=now() WHERE context->>'wakeup_id'=$1 AND status<>'completed'", util.UUIDToString(m.w.ID))
}

func TestMergeEligibilityDispatchesOnceWhenEligible(t *testing.T) {
	m := newMergeFix(t, nil, "")
	for i := 0; i < 4; i++ {
		m.tick(t)
	}
	if n := m.runs(t); n != 1 {
		t.Fatalf("%d runs after four polls of an eligible PR, want exactly 1", n)
	}
	queued := m.f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE context->>'wakeup_id'=$1 AND status IN ('queued','dispatched','running')", util.UUIDToString(m.w.ID))
	if queued != 1 {
		t.Fatalf("%d live tasks, want 1", queued)
	}
	if len(m.facts.calls) == 0 {
		t.Fatal("facts source was never asked")
	}
	c := m.facts.calls[0]
	if c.Owner != "fixture" || c.Repo != "merge" || c.Number != 9 || c.HeadSHA != "a" || c.BaseRef != "main" || util.UUIDToString(c.MergerAgentID) != m.agent {
		t.Fatalf("candidate = %+v", c)
	}
}

func TestMergeEligibilityRefusesWithZeroDispatches(t *testing.T) {
	cases := []struct {
		name   string
		facts  func(*MergeFacts)
		prSet  string
		status string
	}{
		{name: "checks fact false", facts: func(f *MergeFacts) { f.ChecksGreen = ptr(false) }},
		{name: "checks fact absent", facts: func(f *MergeFacts) { f.ChecksGreen = nil }},
		{name: "rollup failure", prSet: "checks_rollup_state='FAILURE'"},
		{name: "rollup error", prSet: "checks_rollup_state='ERROR'"},
		{name: "rollup pending", prSet: "checks_rollup_state='PENDING'"},
		{name: "rollup absent", prSet: "checks_rollup_state=NULL"},
		{name: "security verdict missing", facts: func(f *MergeFacts) { f.SecurityGate = "" }},
		{name: "security verdict failure", facts: func(f *MergeFacts) { f.SecurityGate = "failure" }},
		{name: "no cross-lab pass", facts: func(f *MergeFacts) { f.CrossLabPass = nil }},
		{name: "tier missing", facts: func(f *MergeFacts) { f.Tier = nil }},
		{name: "tier out of range", facts: func(f *MergeFacts) { f.Tier = ptr(3) }},
		{name: "classifier untrusted", facts: func(f *MergeFacts) { f.ClassifierTrusted = ptr(false) }},
		{name: "coverage incomplete", facts: func(f *MergeFacts) { f.CoverageComplete = ptr(false) }},
		{name: "receipt not bound to the PR", facts: func(f *MergeFacts) { f.PRBound = ptr(false) }},
		{name: "paused unread", facts: func(f *MergeFacts) { f.Paused = nil }},
		{name: "paused", facts: func(f *MergeFacts) { f.Paused = ptr(true) }},
		{name: "main red paused", facts: func(f *MergeFacts) { f.MainRedPaused = ptr(true) }},
		{name: "author equals merger", facts: func(f *MergeFacts) { f.MergerIsAuthor = ptr(true) }},
		{name: "author unknown", facts: func(f *MergeFacts) { f.MergerIsAuthor = nil }},
		{name: "scope not classified", facts: func(f *MergeFacts) { f.ScopeClassified = ptr(false) }},
		{name: "no paths", facts: func(f *MergeFacts) { f.Paths = nil }},
		{name: "labels missing", facts: func(f *MergeFacts) { f.Labels = nil }},
		{name: "escalation path", facts: func(f *MergeFacts) { f.Paths = []string{"server/migrations/001_x.sql"} }},
		{name: "security-named path", facts: func(f *MergeFacts) { f.Paths = []string{"server/internal/token_store.go"} }},
		{name: "escalation label", facts: func(f *MergeFacts) { f.Labels = ptr([]string{"Spend"}) }},
		{name: "gate path without veto window", facts: func(f *MergeFacts) {
			f.Paths, f.GateVetoElapsed = []string{".github/workflows/ci.yml"}, ptr(false)
		}},
		{name: "gate path vetoed", facts: func(f *MergeFacts) {
			f.Paths, f.GateVetoed = []string{".github/workflows/ci.yml"}, ptr(true)
		}},
		{name: "facts stale", facts: func(f *MergeFacts) { f.ObservedAt = f.ObservedAt.Add(-301 * time.Second) }},
		{name: "facts without a time", facts: func(f *MergeFacts) { f.ObservedAt = time.Time{} }},
		{name: "facts without evidence", facts: func(f *MergeFacts) { f.Evidence = "" }},
		{name: "unknown mergeability", prSet: "api_mergeable='UNKNOWN'"},
		{name: "conflicting", prSet: "api_mergeable='CONFLICTING'"},
		{name: "closed PR", prSet: "state='closed'"},
		{name: "merged PR", prSet: "state='merged'"},
		{name: "draft PR", prSet: "state='draft'"},
		{name: "snapshot for an older head", prSet: "head_sha='b'"},
		{name: "base branch unknown", prSet: "snapshot_base_ref=NULL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMergeFix(t, tc.facts, tc.prSet)
			m.tick(t)
			m.tick(t)
			if n := m.runs(t); n != 0 {
				t.Fatalf("%d runs, want 0", n)
			}
		})
	}
}

func TestMergeEligibilityRefusesWhenFactsOrPolicyAreUnavailable(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*mergeFix, *testing.T)
		want  error
	}{
		{"facts source fails", func(m *mergeFix, _ *testing.T) { m.facts.err = context.DeadlineExceeded }, errMergeFactsUnavailable},
		{"no facts source", func(m *mergeFix, _ *testing.T) { m.s.MergeFacts = nil }, errMergeFactsUnavailable},
		{"snapshot client disabled", func(m *mergeFix, _ *testing.T) { m.s.Tasks.PRSnapshotsEnabled = false }, errPRSnapshotsUnavailable},
		{"snapshot never fetched", func(m *mergeFix, t *testing.T) {
			m.f.Exec(t, "UPDATE github_pull_request SET snapshot_fetched_at=NULL WHERE id=$1", m.pr)
		}, errPRSnapshotPending},
		{"snapshot expired", func(m *mergeFix, t *testing.T) {
			m.f.Exec(t, "UPDATE github_pull_request SET snapshot_fetched_at='2026-11-12T09:54:00Z' WHERE id=$1", m.pr)
		}, errPRSnapshotPending},
		{"snapshot dated in the future", func(m *mergeFix, t *testing.T) {
			m.f.Exec(t, "UPDATE github_pull_request SET snapshot_fetched_at='2026-11-12T10:05:00Z' WHERE id=$1", m.pr)
		}, errPRSnapshotPending},
		{"no policy", func(m *mergeFix, t *testing.T) {
			m.f.Exec(t, "UPDATE workspace SET settings=settings-'merge_eligibility' WHERE id=$1", m.f.WorkspaceID)
		}, errMergePolicyMissing},
		{"policy without a freshness bound", func(m *mergeFix, t *testing.T) {
			m.f.Exec(t, `UPDATE workspace SET settings=jsonb_set(settings,'{merge_eligibility,max_fact_age_seconds}','0') WHERE id=$1`, m.f.WorkspaceID)
		}, errMergePolicyMissing},
		{"policy with a bad pattern", func(m *mergeFix, t *testing.T) {
			m.f.Exec(t, `UPDATE workspace SET settings=jsonb_set(settings,'{merge_eligibility,gate_path_patterns}','["("]') WHERE id=$1`, m.f.WorkspaceID)
		}, errMergePolicyMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMergeFix(t, nil, "")
			tc.setup(m, t)
			w := m.tick(t)
			if n := m.runs(t); n != 0 {
				t.Fatalf("%d runs, want 0", n)
			}
			if !w.LastError.Valid || w.LastError.String != conditionDiagnosticPrefix+tc.want.Error() {
				t.Fatalf("last_error = %+v, want %q", w.LastError, tc.want)
			}
		})
	}
}

func TestMergeEligibilityNegativeControls(t *testing.T) {
	t.Run("issue cancelled", func(t *testing.T) {
		m := newMergeFix(t, nil, "")
		wakeSetStatus(t, m.f, m.issue, "cancelled")
		m.tick(t)
		if n := m.runs(t); n != 0 {
			t.Fatalf("%d runs on a cancelled issue, want 0", n)
		}
	})
	t.Run("dispatch halted for the repository", func(t *testing.T) {
		m := newMergeFix(t, nil, "")
		m.f.Exec(t, "INSERT INTO github_dispatch_halt(workspace_id,scope) VALUES($1,'*')", m.f.WorkspaceID)
		m.tick(t)
		if n := m.runs(t); n != 0 {
			t.Fatalf("%d runs while dispatch is halted, want 0", n)
		}
	})
	t.Run("no linked PR", func(t *testing.T) {
		m := newMergeFix(t, nil, "")
		m.f.Exec(t, "DELETE FROM issue_pull_request WHERE issue_id=$1", m.issue)
		m.tick(t)
		if n := m.runs(t); n != 0 {
			t.Fatalf("%d runs with no PR, want 0", n)
		}
	})
}

func TestMergeEligibilityGateExemptions(t *testing.T) {
	t.Run("gate disabled needs no veto window", func(t *testing.T) {
		m := newMergeFix(t, func(f *MergeFacts) {
			f.SecurityGate, f.GateVetoElapsed = "disabled", ptr(false)
			f.Paths = []string{".github/workflows/ci.yml"}
		}, "")
		m.tick(t)
		if n := m.runs(t); n != 1 {
			t.Fatalf("%d runs, want 1", n)
		}
	})
	t.Run("security-named test file is exempt", func(t *testing.T) {
		m := newMergeFix(t, func(f *MergeFacts) { f.Paths = []string{"server/internal/token_store_test.go"} }, "")
		m.tick(t)
		if n := m.runs(t); n != 1 {
			t.Fatalf("%d runs, want 1", n)
		}
	})
}

func TestMergeEligibilityRepeatsOnlyOnANewEligibleEpisode(t *testing.T) {
	m := newMergeFix(t, nil, "")
	m.tick(t)
	m.finishRuns(t)
	m.tick(t)
	if n := m.runs(t); n != 1 {
		t.Fatalf("%d runs after an unchanged candidate, want 1", n)
	}
	// Checks go red, then green again on the same head: a new episode.
	m.f.Exec(t, "UPDATE github_pull_request SET checks_rollup_state='FAILURE' WHERE id=$1", m.pr)
	m.tick(t)
	m.f.Exec(t, "UPDATE github_pull_request SET checks_rollup_state='SUCCESS' WHERE id=$1", m.pr)
	m.tick(t)
	if n := m.runs(t); n != 2 {
		t.Fatalf("%d runs after a new episode, want 2", n)
	}
	m.finishRuns(t)
	// A new head with fresh eligible facts is a different candidate.
	m.f.Exec(t, "UPDATE github_pull_request SET head_sha='b',snapshot_head_sha='b' WHERE id=$1", m.pr)
	m.tick(t)
	if n := m.runs(t); n != 3 {
		t.Fatalf("%d runs after a new head, want 3", n)
	}
}

// A PR leaving the eligible set must not replay the candidates that stay: the
// agent already ran for them in this episode.
func TestMergeEligibilityIgnoresAPRLeavingTheEligibleSet(t *testing.T) {
	m := newMergeFix(t, nil, "")
	prB := m.f.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": m.f.WorkspaceID, "installation_id": 1,
		"repo_owner": "fixture", "repo_name": "merge", "pr_number": 10,
		"title": "PR B", "state": "open", "html_url": "https://example.test/pr/10",
		"pr_created_at": "2026-01-01T00:00:00Z", "pr_updated_at": "2026-01-01T00:00:00Z",
		"head_sha": "b", "snapshot_head_sha": "b", "snapshot_base_ref": "main",
		"api_mergeable": "MERGEABLE", "checks_rollup_state": "SUCCESS",
		"snapshot_fetched_at": "2026-11-12T09:59:00Z",
	})
	m.f.Cleanup(t, "DELETE FROM issue_pull_request WHERE pull_request_id=$1", prB)
	m.f.Exec(t, "INSERT INTO issue_pull_request(issue_id,pull_request_id) VALUES($1,$2)", m.issue, prB)

	m.tick(t)
	if n := m.runs(t); n != 1 {
		t.Fatalf("%d runs for two eligible PRs, want 1", n)
	}
	m.finishRuns(t)

	m.f.Exec(t, "UPDATE github_pull_request SET checks_rollup_state='FAILURE' WHERE id=$1", prB)
	m.tick(t)
	m.tick(t)
	if n := m.runs(t); n != 1 {
		t.Fatalf("%d runs after PR B left the set, want 1: PR A is unchanged", n)
	}
	if live := m.f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE context->>'wakeup_id'=$1 AND status IN ('queued','dispatched','running')", util.UUIDToString(m.w.ID)); live != 0 {
		t.Fatalf("%d live tasks after PR B left the set, want 0", live)
	}

	// PR B going green again is news, and only B.
	m.f.Exec(t, "UPDATE github_pull_request SET checks_rollup_state='SUCCESS' WHERE id=$1", prB)
	m.tick(t)
	if n := m.runs(t); n != 2 {
		t.Fatalf("%d runs after PR B returned, want 2", n)
	}
}
