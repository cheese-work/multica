package service

import (
	"context"
	"errors"
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

// record is what the trusted producer does: write the receipt facts of an
// eligible PR for its current head and base, stamped at the fixture clock.
func (m *mergeFix) record(t *testing.T, evidence string) {
	t.Helper()
	if err := RecordMergeReceipt(context.Background(), m.f.Pool, parseTestUUID(t, m.f.WorkspaceID), MergeReceiptInput{
		Owner: "fixture", Repo: "merge", Number: 9, HeadSHA: "a", BaseRef: "main",
		Tier: ptr(1), ClassifierTrusted: ptr(true), CoverageComplete: ptr(true), PRBound: ptr(true), SecurityGate: "success",
		CrossLabPass: ptr(true), ScopeClassified: ptr(true), GateVetoed: ptr(false),
		Paths: []string{"server/internal/service/foo.go"}, Labels: []string{}, Evidence: evidence,
	}, m.clock.Add(-10*time.Second)); err != nil {
		t.Fatal(err)
	}
}

// stored records the receipt facts of an eligible PR through the producer, and
// names an author other than the merger. mutate adjusts the stored row.
func (m *mergeFix) stored(t *testing.T, mutate string) {
	t.Helper()
	m.bindAuthor(t, m.f.privateAgentOwnedBy(t, m.f.UserID, "author"))
	m.f.Cleanup(t, "DELETE FROM merge_receipt_fact WHERE workspace_id=$1", m.f.WorkspaceID)
	m.f.Cleanup(t, "DELETE FROM merge_veto WHERE workspace_id=$1", m.f.WorkspaceID)
	m.record(t, "receipt-1")
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
		{"stale PR snapshot", func(t *testing.T, m *mergeFix) {
			m.f.Exec(t, "UPDATE github_pull_request SET snapshot_fetched_at=$2 WHERE id=$1", m.pr, m.clock.Add(-301*time.Second))
		}},
		{"stale pause read", func(t *testing.T, m *mergeFix) {
			m.f.Exec(t, "UPDATE merge_pause_state SET fetched_at=$1 WHERE repo_owner='fixture'", m.clock.Add(-301*time.Second))
		}},
		{"review revoked after the receipt", func(t *testing.T, m *mergeFix) { m.revoke(t) }},
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

// revoke stores what GitHub intake stores when a reviewer requests changes or
// dismisses a review after the receipt was recorded.
func (m *mergeFix) revoke(t *testing.T) {
	t.Helper()
	m.f.Exec(t, "UPDATE github_pull_request SET review_state='changes_requested',review_head_sha='a',review_id=7,review_at=$2 WHERE id=$1", m.pr, m.clock)
}

// refresh moves the clock and re-reads the inputs the refreshers keep current:
// the PR snapshot and the pause variables.
func (m *mergeFix) refresh(t *testing.T, d time.Duration) {
	t.Helper()
	m.clock = m.clock.Add(d)
	m.f.Exec(t, "UPDATE github_pull_request SET snapshot_fetched_at=$2 WHERE id=$1", m.pr, m.clock.Add(-time.Minute))
	m.s.PauseVars = &fakePauseVars{}
	if err := m.s.RefreshMergePauses(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A revoked review withdraws the queued run and stops the claim, with no
// person recording a new receipt.
func TestStoredMergeFactsRevocationWithdrawsQueuedWorkAndStopsClaim(t *testing.T) {
	m := newMergeFix(t, nil, "")
	mainStyle(t, m)
	m.stored(t, "")
	m.tick(t)
	if n := m.live(t); n != 1 {
		t.Fatalf("%d live runs before the revocation, want 1", n)
	}
	task := m.lastTask(t)
	m.revoke(t)
	if err := m.s.CheckClaim(context.Background(), task); !errors.Is(err, ErrWakeupForbidden) {
		t.Fatalf("CheckClaim after the revocation = %v, want ErrWakeupForbidden", err)
	}
	m.tick(t)
	if live, cancelled := m.live(t), m.cancelled(t); live != 0 || cancelled != 1 {
		t.Fatalf("live=%d cancelled=%d after the revocation, want 0 and 1", live, cancelled)
	}
}

// A PR retargeted main -> release -> main on one head gets no old receipt back.
func TestStoredMergeFactsRetargetRoundTripDoesNotReviveTheReceipt(t *testing.T) {
	m := newMergeFix(t, nil, "")
	mainStyle(t, m)
	m.stored(t, "")
	m.tick(t)
	task := m.lastTask(t)
	for _, base := range []string{"release", "main"} {
		m.f.Exec(t, "UPDATE github_pull_request SET snapshot_base_ref=$2 WHERE id=$1", m.pr, base)
	}
	if err := m.s.CheckClaim(context.Background(), task); !errors.Is(err, ErrWakeupForbidden) {
		t.Fatalf("CheckClaim after the round trip = %v, want ErrWakeupForbidden", err)
	}
	m.tick(t)
	if live := m.live(t); live != 0 {
		t.Fatalf("%d live runs after the round trip, want 0", live)
	}
	m.finishRuns(t)
	m.tick(t)
	if n := m.runs(t); n != 1 {
		t.Fatalf("%d runs in total after the round trip, want the 1 from before", n)
	}
}

// The receipt carries its own freshness bound. A lost webhook leaves it
// un-invalidated, but it stops counting once it is older than the policy bound,
// even while the PR snapshot and the pause reads are fresh.
func TestStoredMergeFactsMissedEventLeavesNoLiveReceiptAtClaim(t *testing.T) {
	m := newMergeFix(t, nil, "")
	mainStyle(t, m)
	m.stored(t, "")
	m.tick(t)
	task := m.lastTask(t)
	m.refresh(t, 10*time.Minute)
	if err := m.s.CheckClaim(context.Background(), task); !errors.Is(err, ErrWakeupForbidden) {
		t.Fatalf("CheckClaim with a 10 minute old receipt and fresh inputs = %v, want ErrWakeupForbidden", err)
	}
	m.tick(t)
	if live, cancelled := m.live(t), m.cancelled(t); live != 0 || cancelled != 1 {
		t.Fatalf("live=%d cancelled=%d after the receipt aged out, want 0 and 1", live, cancelled)
	}
}

// The trusted producer re-records on its own schedule, so a PR whose receipt
// aged out or was invalidated recovers with no person. The veto deadline of a
// gate-path PR passes 24 h after the last PASS and dispatches once.
func TestStoredMergeFactsProducerRefreshRecoversAndDeadlineDispatches(t *testing.T) {
	m := newMergeFix(t, nil, "")
	mainStyle(t, m)
	m.stored(t, "")
	pass := m.clock.Add(-time.Hour)
	gate := func(evidence string) {
		m.record(t, evidence)
		m.f.Exec(t, "UPDATE merge_receipt_fact SET paths=ARRAY['.github/workflows/ci.yml'],last_pass_at=$2 WHERE workspace_id=$1", m.f.WorkspaceID, pass)
	}
	gate("receipt-1")
	m.tick(t)
	if n := m.runs(t); n != 0 {
		t.Fatalf("%d runs inside the veto window, want 0", n)
	}
	m.refresh(t, 22*time.Hour)
	gate("receipt-1")
	m.tick(t)
	if n := m.runs(t); n != 0 {
		t.Fatalf("%d runs one hour before the deadline, want 0", n)
	}
	m.refresh(t, 2*time.Hour)
	m.tick(t)
	if n := m.runs(t); n != 0 {
		t.Fatalf("%d runs at the deadline with a 2 hour old receipt, want 0", n)
	}
	gate("receipt-1")
	m.tick(t)
	m.tick(t)
	if n := m.runs(t); n != 1 {
		t.Fatalf("%d runs after the deadline once the producer re-recorded, want 1", n)
	}
}

// A veto recorded only in Multica holds the queued run at claim and keeps the
// PR held while a producer keeps writing receipts. Clearing it releases the PR
// only after the producer writes a new receipt.
func TestStoredMergeFactsMulticaOnlyVetoStopsClaimUntilClearedAndRecorded(t *testing.T) {
	m := newMergeFix(t, nil, "")
	mainStyle(t, m)
	m.stored(t, "")
	m.tick(t)
	task := m.lastTask(t)
	ws := parseTestUUID(t, m.f.WorkspaceID)
	if err := SetMergeVeto(context.Background(), m.f.Pool, ws, "fixture", "merge", 9, parseTestUUID(t, m.f.UserID), "hold for Cheese", m.clock); err != nil {
		t.Fatal(err)
	}
	if err := m.s.CheckClaim(context.Background(), task); !errors.Is(err, ErrWakeupForbidden) {
		t.Fatalf("CheckClaim after a Multica-only veto = %v, want ErrWakeupForbidden", err)
	}
	m.tick(t)
	if live, cancelled := m.live(t), m.cancelled(t); live != 0 || cancelled != 1 {
		t.Fatalf("live=%d cancelled=%d after the veto, want 0 and 1", live, cancelled)
	}
	if err := RecordMergeReceipt(context.Background(), m.f.Pool, ws, MergeReceiptInput{Owner: "fixture", Repo: "merge", Number: 9, HeadSHA: "a", BaseRef: "main", Evidence: "receipt-2"}, m.clock); !IsMergeReceiptConflict(err) {
		t.Fatalf("RecordMergeReceipt under a veto = %v, want a conflict", err)
	}
	m.tick(t)
	if live := m.live(t); live != 0 {
		t.Fatalf("%d live runs while the veto stands, want 0", live)
	}
	if err := ClearMergeVeto(context.Background(), m.f.Pool, ws, "fixture", "merge", 9); err != nil {
		t.Fatal(err)
	}
	m.tick(t)
	if live := m.live(t); live != 0 {
		t.Fatalf("%d live runs after the clear and before a new receipt, want 0", live)
	}
	m.record(t, "receipt-3")
	m.tick(t)
	m.tick(t)
	if live := m.live(t); live != 1 {
		t.Fatalf("%d live runs after the clear and a new receipt, want 1", live)
	}
}
