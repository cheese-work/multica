package ciwatch

import (
	"context"
	"testing"
	"time"
)

// CHE-4279 review round 2: P1 counts toward the breaker, issue creation needs the
// shared red-main claim, and closure time ignores later edits.

func (s *stormEnv) admitP1(t *testing.T, run int64, sig string) Admission {
	t.Helper()
	a, err := s.ledger.Admit(context.Background(), Failure{WorkspaceID: s.ws, RepoOwner: "o", RepoName: s.repo, RunID: run,
		SignatureHash: sig, Protocol: "P1", CheckName: "check-" + sig, RerunDueAt: s.clock.Now().Add(time.Hour)})
	if err != nil {
		t.Fatalf("admit P1 run %d: %v", run, err)
	}
	return a
}

func TestStormP1SchedulesARerunInTheAdmitTransaction(t *testing.T) {
	s := newStormEnv(t)
	if got := s.admitP1(t, 701, "p1A"); got.Outcome != OutcomeRerun {
		t.Fatalf("outcome = %q, want %q", got.Outcome, OutcomeRerun)
	}
	if got := s.actions(t, ActionRerun, StateScheduled); got != 1 {
		t.Fatalf("scheduled reruns = %d, want 1", got)
	}
	if got := s.events(t, "admit"); got != 1 {
		t.Fatalf("admit events = %d, want 1 (P1 counts toward the breaker)", got)
	}
}

func TestStormP1OnlyBurstTripsTheBreakerAndCancelsReruns(t *testing.T) {
	s := newStormEnv(t)
	for i := int64(0); i < StormThreshold; i++ {
		s.admitP1(t, 710+i, "p1B")
	}
	s.drain(t, false)
	if got := s.events(t, "breaker"); got != 1 {
		t.Fatalf("breaker trips = %d, want 1", got)
	}
	if sched, canc := s.actions(t, ActionRerun, StateScheduled), s.actions(t, ActionRerun, StateCancelled); sched != 0 || canc != StormThreshold-1 {
		t.Fatalf("reruns scheduled=%d cancelled=%d, want 0/%d", sched, canc, StormThreshold-1)
	}
	if got := s.count(t, "issue"); got != 1 {
		t.Fatalf("incident issues = %d, want 1", got)
	}
}

func TestStormMixedP1BurstTripsTheBreaker(t *testing.T) {
	s := newStormEnv(t)
	for i, p := range []string{"P1", "P2", "P1", "P3", "P1"} {
		if p == "P1" {
			s.admitP1(t, int64(720+i), "p1C")
			continue
		}
		s.admit(t, int64(720+i), "p1C", p)
	}
	s.drain(t, false)
	if got := s.events(t, "breaker"); got != 1 {
		t.Fatalf("breaker trips = %d, want 1", got)
	}
	if got := s.actions(t, ActionRerun, StateScheduled); got != 0 {
		t.Fatalf("reruns still scheduled after the trip = %d, want 0", got)
	}
}

func TestStormP1InsideThePauseSchedulesNoRerun(t *testing.T) {
	s := newStormEnv(t)
	for i := int64(0); i < StormThreshold; i++ {
		s.admitP1(t, 730+i, "p1D")
	}
	if got := s.admitP1(t, 740, "p1D"); got.Outcome != OutcomeSuppressed {
		t.Fatalf("P1 inside the pause = %q, want %q", got.Outcome, OutcomeSuppressed)
	}
	if got := s.actions(t, ActionRerun, StateScheduled); got != 0 {
		t.Fatalf("reruns scheduled inside the pause = %d, want 0", got)
	}
}

// reasonOf is the reason of the action of one kind, or "" when there is none.
func (s *stormEnv) reasonOf(t *testing.T, kind, state string) string {
	t.Helper()
	var r string
	if err := s.pool.QueryRow(context.Background(), `SELECT COALESCE(max(reason),'') FROM ci_watch_action WHERE workspace_id=$1::uuid
		AND action_kind=$2 AND state=$3`, s.ws, kind, state).Scan(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestStormIssueCreateWithoutACheckKeyFailsClosed(t *testing.T) {
	s := newStormEnv(t)
	s.admitCheck(t, 801, "nokey", "") // empty
	s.admitCheck(t, 802, "badkey", "..")
	s.drain(t, true)
	if got := s.count(t, "issue"); got != 0 {
		t.Fatalf("issues without a usable check key = %d, want 0", got)
	}
	if got := s.actions(t, ActionIssueCreate, StateFailed); got != 2 {
		t.Fatalf("failed issue_create = %d, want 2", got)
	}
	if r := s.reasonOf(t, ActionIssueCreate, StateFailed); r != ReasonNoCheckKey {
		t.Fatalf("reason = %q, want %q", r, ReasonNoCheckKey)
	}
}

func TestStormIssueCreateWithoutTheSharedClaimStaysShadowOnly(t *testing.T) {
	s := newStormEnv(t)
	d := s.destinations()
	d.RedMain = nil
	s.ledger.RegisterStormExecutors(d)
	s.admit(t, 811, "shadow", "P2")
	s.drain(t, true)
	if got := s.count(t, "issue"); got != 0 {
		t.Fatalf("issues without a claimer = %d, want 0", got)
	}
	if r := s.reasonOf(t, ActionIssueCreate, StateFailed); r != ReasonNonTransactional {
		t.Fatalf("reason = %q, want %q", r, ReasonNonTransactional)
	}
}

func TestStormIssueCreateLosesToAWorkerHoldingTheCheckClaim(t *testing.T) {
	s := newStormEnv(t)
	s.claims.held[s.repo+"/check-held"] = "red-main-worker" // a worker took refs/heads/red-main/check-held first
	s.admit(t, 821, "held", "P2")
	s.drain(t, true)
	if got := s.count(t, "issue"); got != 0 {
		t.Fatalf("issues while a worker holds the check = %d, want 0", got)
	}
	if r := s.reasonOf(t, ActionIssueCreate, StateFailed); r != ReasonRedMainClaimed {
		t.Fatalf("reason = %q, want %q", r, ReasonRedMainClaimed)
	}
	var creating int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM ci_signature_owner WHERE workspace_id=$1::uuid
		AND state='creating'`, s.ws).Scan(&creating); err != nil || creating != 0 {
		t.Fatalf("owners still creating after the claim was lost = %d err=%v; they must be released", creating, err)
	}
}

func TestStormIssueCreateTakesTheClaimOnceAndRetriesAsTheSameHolder(t *testing.T) {
	s := newStormEnv(t)
	s.d.loseReply.Store(true) // the first creation rolls back after the claim
	s.admit(t, 831, "retry", "P2")
	s.drain(t, true)
	s.clock.Advance(2 * time.Minute)
	s.drain(t, false)
	if got := s.count(t, "issue"); got != 1 {
		t.Fatalf("issues after a retried creation = %d, want 1", got)
	}
	if got := s.claims.claims.Load(); got != 2 {
		t.Fatalf("claim calls = %d, want 2 (first try, retry)", got)
	}
}

func TestStormClosureTimeIgnoresEditsAfterTheIssueClosed(t *testing.T) {
	s := newStormEnv(t)
	s.admit(t, 901, "edit", "P2")
	s.drain(t, false)
	s.clock.Advance(23 * time.Hour)
	s.closeIssue(t, "edit", 22*time.Hour+30*time.Minute) // closed 30 minutes ago
	if _, err := s.pool.Exec(context.Background(), `UPDATE issue SET title=title||' (edited)', updated_at=$2 WHERE workspace_id=$1::uuid`,
		s.ws, s.clock.Now()); err != nil { // an edit just now must not restart the window
		t.Fatal(err)
	}
	s.admit(t, 902, "edit", "P2")
	var closedAt *time.Time
	if err := s.pool.QueryRow(context.Background(), `SELECT closed_at FROM ci_signature_owner WHERE workspace_id=$1::uuid
		AND signature_hash='edit'`, s.ws).Scan(&closedAt); err != nil || closedAt == nil {
		t.Fatalf("closed_at = %v err=%v", closedAt, err)
	}
	if want := s.clock.Now().Add(-22*time.Hour - 30*time.Minute + 0); closedAt.Sub(want).Abs() > time.Second {
		t.Fatalf("closed_at = %v, want the status-change time %v, not the edit time", closedAt, want)
	}
	s.clock.Advance(90 * time.Minute) // 24 h after the real closure, 1.5 h after the edit
	s.admit(t, 903, "edit", "P2")
	s.drain(t, false)
	if got := s.count(t, "issue"); got != 2 {
		t.Fatalf("issues after the window ran from the real closure = %d, want 2", got)
	}
}
