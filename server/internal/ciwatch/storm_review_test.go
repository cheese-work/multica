package ciwatch

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// CHE-4279 review round: closure, check ownership, breaker coverage, P3 priority.

func TestStormRealClosureMarksTheOwnerClosed(t *testing.T) {
	s := newStormEnv(t)
	s.admit(t, 201, "clA", "P2")
	s.drain(t, false)
	s.clock.Advance(time.Hour)
	s.closeIssue(t, "clA", 30*time.Minute)
	s.admit(t, 202, "clA", "P2")
	var closedAt *time.Time
	var state string
	if err := s.pool.QueryRow(context.Background(), `SELECT state, closed_at FROM ci_signature_owner WHERE workspace_id=$1
		AND signature_hash='clA'`, s.ws).Scan(&state, &closedAt); err != nil {
		t.Fatal(err)
	}
	if state != "closed" || closedAt == nil {
		t.Fatalf("owner after the issue closed = %s closed_at=%v, want closed with a time", state, closedAt)
	}
	if want := s.clock.Now().Add(-30 * time.Minute); closedAt.Sub(want).Abs() > time.Second {
		t.Fatalf("closed_at = %v, want the issue's closing time %v", closedAt, want)
	}
}

func TestStormBreakerCountsP4AndMixedBursts(t *testing.T) {
	for name, protocols := range map[string][]string{
		"p4 only": {"P4", "P4", "P4", "P4", "P4"},
		"mixed":   {"P2", "P4", "P3", "P4", "P4"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newStormEnv(t)
			for i, p := range protocols {
				s.admit(t, int64(300+i), "mixA", p)
			}
			s.drain(t, false)
			if got := s.events(t, "breaker"); got != 1 {
				t.Fatalf("breaker trips = %d, want 1", got)
			}
			if got := s.count(t, "issue"); got != 1 {
				t.Fatalf("incident issues = %d, want 1", got)
			}
		})
	}
}

func TestStormRejectsProtocolsItDoesNotAdmit(t *testing.T) {
	s := newStormEnv(t)
	_, err := s.ledger.Admit(context.Background(), Failure{WorkspaceID: s.ws, RepoOwner: "o", RepoName: s.repo,
		RunID: 400, SignatureHash: "p5A", Protocol: "P5"})
	if err == nil {
		t.Fatal("an unknown protocol was admitted; it must be rejected, not defaulted to P2")
	}
	if got := s.events(t, "admit"); got != 0 {
		t.Fatalf("admit events for a rejected protocol = %d, want 0", got)
	}
}

func TestStormP3IssueIsMediumAndWakesTheOwner(t *testing.T) {
	s := newStormEnv(t)
	s.admit(t, 501, "p3A", "P3")
	s.drain(t, false)
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if len(s.d.specs) != 1 || s.d.specs[0].Priority != "medium" || !s.d.specs[0].Wake {
		t.Fatalf("P3 issue spec = %+v, want medium priority with a wake", s.d.specs)
	}
}

func TestStormP3EscalatingToP2UpgradesPriorityAndWakes(t *testing.T) {
	s := newStormEnv(t)
	s.admit(t, 511, "esc", "P3")
	s.drain(t, false)
	s.admit(t, 512, "esc", "P2")
	s.drain(t, false)
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if len(s.d.updates) != 1 || s.d.updates[0].Priority != "urgent" || !s.d.updates[0].Wake {
		t.Fatalf("escalation update = %+v, want urgent with a wake", s.d.updates)
	}
	if got := s.events(t, "wake"); got != 2 {
		t.Fatalf("wake events = %d, want 2 (creation and escalation)", got)
	}
}

func TestStormP3EscalationHonoursTheWakeCap(t *testing.T) {
	s := newStormEnv(t)
	s.setCaps(t, 5, 1) // the P3 creation wake uses the only slot
	s.admit(t, 521, "escC", "P3")
	s.drain(t, false)
	s.admit(t, 522, "escC", "P2")
	s.drain(t, false)
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if len(s.d.updates) != 1 || s.d.updates[0].Priority != "urgent" || s.d.updates[0].Wake {
		t.Fatalf("capped escalation update = %+v, want urgent without a wake", s.d.updates)
	}
}

func (s *stormEnv) admitCheck(t *testing.T, run int64, sig, check string) {
	t.Helper()
	if _, err := s.ledger.Admit(context.Background(), Failure{WorkspaceID: s.ws, RepoOwner: "o", RepoName: s.repo,
		RunID: run, SignatureHash: sig, Protocol: "P2", CheckName: check}); err != nil {
		t.Fatalf("admit run %d: %v", run, err)
	}
}

func TestStormOneIssuePerCheck(t *testing.T) {
	s := newStormEnv(t)
	s.admitCheck(t, 601, "chkA", "Frontend Build")
	s.admitCheck(t, 602, "chkB", "frontend build") // another signature, same check key
	s.drain(t, false)
	if got := s.count(t, "issue"); got != 1 {
		t.Fatalf("issues for one red-main check = %d, want 1", got)
	}
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if want := "red-main " + s.repo + " frontend-build: "; len(s.d.specs) != 1 || len(s.d.specs[0].Title) < len(want) ||
		s.d.specs[0].Title[:len(want)] != want {
		t.Fatalf("issue spec = %+v, want a title starting %q", s.d.specs, want)
	}
}

func TestStormReusesAnExistingRedMainIssue(t *testing.T) {
	s := newStormEnv(t)
	existing := uuid.NewString()
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO issue (id, workspace_id, title, status, priority, creator_type,
		creator_id, number) VALUES ($1::uuid,$2::uuid,$3,'in_progress','urgent','member',$4::uuid,900)`,
		existing, s.ws, "red-main "+s.repo+" frontend-build: fix the build", uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	s.admitCheck(t, 611, "chkC", "frontend-build")
	s.drain(t, false)
	if got := s.count(t, "issue"); got != 0 {
		t.Fatalf("new issues while an open red-main owner exists = %d, want 0", got)
	}
	var issueID, state string
	if err := s.pool.QueryRow(context.Background(), `SELECT COALESCE(issue_id::text,''), state FROM ci_signature_owner
		WHERE workspace_id=$1 AND signature_hash='chkC'`, s.ws).Scan(&issueID, &state); err != nil {
		t.Fatal(err)
	}
	if issueID != existing || state != "open" {
		t.Fatalf("owner = issue %s state %s, want the existing issue %s open", issueID, state, existing)
	}
}

func TestStormClosedRedMainIssueDoesNotBlockANewOwner(t *testing.T) {
	s := newStormEnv(t)
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO issue (id, workspace_id, title, status, priority, creator_type,
		creator_id, number) VALUES ($1::uuid,$2::uuid,$3,'done','urgent','member',$4::uuid,901)`,
		uuid.NewString(), s.ws, "red-main "+s.repo+" lint: old", uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	s.admitCheck(t, 621, "chkD", "lint")
	s.drain(t, false)
	if got := s.count(t, "issue"); got != 1 {
		t.Fatalf("issues = %d, want 1 (a closed red-main issue is not an owner)", got)
	}
}
