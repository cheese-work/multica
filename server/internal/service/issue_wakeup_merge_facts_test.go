package service

import (
	"context"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
)

// Slice D (CHE-5594): the production MergeFactsSource. These tests build the
// task service the way cmd/server/main.go does (NewTaskService, then
// IssueWakeupService{Tasks: taskSvc}) and never set a facts source, so they fail
// if the constructor path leaves merge_eligibility without one.

// mainStyle swaps the fixture's service for one built through main's path.
func mainStyle(t *testing.T, m *mergeFix) {
	t.Helper()
	tasks := NewTaskService(m.f.q, m.f.Pool, nil, events.New())
	tasks.PRSnapshotsEnabled = true
	m.s = &IssueWakeupService{Tasks: tasks, Now: func() time.Time { return m.clock }}
}

// bindAuthor records who authored the PR the trusted way: the workspace binding.
func (m *mergeFix) bindAuthor(t *testing.T, agentID string) {
	t.Helper()
	m.f.Exec(t, `UPDATE workspace SET settings=settings || jsonb_build_object('github_review_authors',jsonb_build_object('fixture/merge#9',$2::text)) WHERE id=$1`, m.f.WorkspaceID, agentID)
}

// stored records the receipt facts of an eligible PR through the producer, and
// names an author other than the merger. mutate adjusts the stored row.
func (m *mergeFix) stored(t *testing.T, mutate string) {
	t.Helper()
	m.bindAuthor(t, m.f.privateAgentOwnedBy(t, m.f.UserID, "author"))
	m.f.Cleanup(t, "DELETE FROM merge_receipt_fact WHERE workspace_id=$1", m.f.WorkspaceID)
	if err := RecordMergeReceipt(context.Background(), m.f.Pool, parseTestUUID(t, m.f.WorkspaceID), MergeReceiptInput{
		Owner: "fixture", Repo: "merge", Number: 9, HeadSHA: "a", BaseRef: "main",
		Tier: ptr(1), ClassifierTrusted: ptr(true), CoverageComplete: ptr(true), PRBound: ptr(true), SecurityGate: "success",
		CrossLabPass: ptr(true), ScopeClassified: ptr(true), GateVetoed: ptr(false),
		Paths: []string{"server/internal/service/foo.go"}, Labels: []string{}, Evidence: "receipt-1",
	}, m.clock.Add(-10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if mutate != "" {
		m.f.Exec(t, "UPDATE merge_receipt_fact SET "+mutate+" WHERE workspace_id=$1", m.f.WorkspaceID)
	}
}

func TestStoredMergeFactsDispatchesOneEligiblePR(t *testing.T) {
	m := newMergeFix(t, nil, "")
	mainStyle(t, m)
	m.stored(t, "")
	for i := 0; i < 4; i++ {
		m.tick(t)
	}
	if n := m.runs(t); n != 1 {
		t.Fatalf("%d queued runs for an eligible PR through main's wiring, want 1", n)
	}
	m.start(t)
	for i := 0; i < 3; i++ {
		m.tick(t)
	}
	if n, started := m.runs(t), m.f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE context->>'wakeup_id'=$1 AND status='running'", util.UUIDToString(m.w.ID)); n != 1 || started != 1 {
		t.Fatalf("runs=%d started=%d after repeated events, want 1 and 1", n, started)
	}
}

func TestStoredMergeFactsHoldsEveryIneligiblePR(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*testing.T, *mergeFix)
	}{
		{"red checks", func(t *testing.T, m *mergeFix) {
			m.f.Exec(t, "UPDATE github_pull_request SET checks_rollup_state='FAILURE' WHERE id=$1", m.pr)
		}},
		{"checks pending", func(t *testing.T, m *mergeFix) {
			m.f.Exec(t, "UPDATE github_pull_request SET checks_rollup_state='PENDING' WHERE id=$1", m.pr)
		}},
		{"paused", func(t *testing.T, m *mergeFix) { m.setPause(t, "AGENT_MERGE_PAUSED") }},
		{"main red paused", func(t *testing.T, m *mergeFix) { m.setPause(t, "MAIN_RED_PAUSED") }},
		{"pause state unknown", func(t *testing.T, m *mergeFix) {
			m.f.Exec(t, "UPDATE merge_pause_state SET is_set=NULL,error='boom' WHERE repo_owner='fixture'")
		}},
		{"vetoed gate-path PR", func(t *testing.T, m *mergeFix) {
			m.f.Exec(t, "UPDATE merge_receipt_fact SET paths=ARRAY['.github/workflows/ci.yml'],gate_vetoed=true,last_pass_at=$2 WHERE workspace_id=$1", m.f.WorkspaceID, m.clock.Add(-48*time.Hour))
		}},
		{"gate-path PR inside the veto window", func(t *testing.T, m *mergeFix) {
			m.f.Exec(t, "UPDATE merge_receipt_fact SET paths=ARRAY['.github/workflows/ci.yml'],last_pass_at=$2 WHERE workspace_id=$1", m.f.WorkspaceID, m.clock.Add(-time.Hour))
		}},
		{"stale facts", func(t *testing.T, m *mergeFix) {
			m.f.Exec(t, "UPDATE merge_receipt_fact SET observed_at=$2 WHERE workspace_id=$1", m.f.WorkspaceID, m.clock.Add(-301*time.Second))
		}},
		{"no facts for the current head", func(t *testing.T, m *mergeFix) {
			m.f.Exec(t, "UPDATE github_pull_request SET head_sha='b', snapshot_head_sha='b' WHERE id=$1", m.pr)
		}},
		{"missing cross-lab PASS", func(t *testing.T, m *mergeFix) {
			m.f.Exec(t, "UPDATE merge_receipt_fact SET cross_lab_pass=NULL WHERE workspace_id=$1", m.f.WorkspaceID)
		}},
		{"security verdict missing", func(t *testing.T, m *mergeFix) {
			m.f.Exec(t, "UPDATE merge_receipt_fact SET security_gate='' WHERE workspace_id=$1", m.f.WorkspaceID)
		}},
		{"escalation path", func(t *testing.T, m *mergeFix) {
			m.f.Exec(t, "UPDATE merge_receipt_fact SET paths=ARRAY['server/migrations/001_x.sql'] WHERE workspace_id=$1", m.f.WorkspaceID)
		}},
		{"retargeted without a new head", func(t *testing.T, m *mergeFix) {
			m.f.Exec(t, "UPDATE github_pull_request SET snapshot_base_ref='release' WHERE id=$1", m.pr)
		}},
		{"author equals merger", func(t *testing.T, m *mergeFix) { m.bindAuthor(t, m.agent) }},
		{"misleading branch names another agent", func(t *testing.T, m *mergeFix) {
			m.bindAuthor(t, m.agent)
			m.f.Exec(t, "UPDATE github_pull_request SET branch='agent/someone-else/che-1-x' WHERE id=$1", m.pr)
		}},
		{"author unknown", func(t *testing.T, m *mergeFix) {
			m.f.Exec(t, `UPDATE workspace SET settings=settings-'github_review_authors' WHERE id=$1`, m.f.WorkspaceID)
			m.f.Exec(t, "UPDATE github_pull_request SET branch='feature' WHERE id=$1", m.pr)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMergeFix(t, nil, "")
			mainStyle(t, m)
			m.stored(t, "")
			tc.setup(t, m)
			for i := 0; i < 2; i++ {
				m.tick(t)
			}
			if n := m.runs(t); n != 0 {
				t.Fatalf("%d runs, want 0", n)
			}
		})
	}
}

func TestRecordMergeReceiptStoresOnlyTheCurrentHeadAndBase(t *testing.T) {
	m := newMergeFix(t, nil, "")
	ws := parseTestUUID(t, m.f.WorkspaceID)
	m.f.Cleanup(t, "DELETE FROM merge_receipt_fact WHERE workspace_id=$1", m.f.WorkspaceID)
	base := MergeReceiptInput{Owner: "fixture", Repo: "merge", Number: 9, HeadSHA: "a", BaseRef: "main", Evidence: "r"}
	for name, mutate := range map[string]func(*MergeReceiptInput){
		"old head":   func(in *MergeReceiptInput) { in.HeadSHA = "z" },
		"other base": func(in *MergeReceiptInput) { in.BaseRef = "release" },
		"unknown PR": func(in *MergeReceiptInput) { in.Number = 99 },
	} {
		in := base
		mutate(&in)
		if err := RecordMergeReceipt(context.Background(), m.f.Pool, ws, in, m.clock); !IsMergeReceiptConflict(err) {
			t.Fatalf("%s: err = %v, want a conflict", name, err)
		}
	}
	in := base
	in.Evidence = ""
	if err := RecordMergeReceipt(context.Background(), m.f.Pool, ws, in, m.clock); !IsMergeReceiptInvalid(err) {
		t.Fatalf("no evidence: err = %v, want invalid", err)
	}
	if n := m.f.Count(t, "SELECT count(*) FROM merge_receipt_fact WHERE workspace_id=$1", m.f.WorkspaceID); n != 0 {
		t.Fatalf("%d rows stored from refused receipts, want 0", n)
	}
	if err := RecordMergeReceipt(context.Background(), m.f.Pool, ws, base, m.clock); err != nil {
		t.Fatal(err)
	}
	m.f.Exec(t, "UPDATE github_pull_request SET head_sha='b', snapshot_head_sha='b' WHERE id=$1", m.pr)
	next := base
	next.HeadSHA = "b"
	if err := RecordMergeReceipt(context.Background(), m.f.Pool, ws, next, m.clock); err != nil {
		t.Fatal(err)
	}
	if n := m.f.Count(t, "SELECT count(*) FROM merge_receipt_fact WHERE workspace_id=$1 AND head_sha='a'", m.f.WorkspaceID); n != 0 {
		t.Fatalf("%d rows kept for the old head, want 0", n)
	}
}
