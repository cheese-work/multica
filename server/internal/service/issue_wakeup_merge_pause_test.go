package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakePauseVars struct {
	names []string
	err   error
	reads int
}

func (f *fakePauseVars) RepoVariableNames(context.Context, int64, string, string) ([]string, error) {
	f.reads++
	return slices.Clone(f.names), f.err
}

// pauseFix is a mergeFix whose pause variables come from a fake GitHub.
type pauseFix struct {
	*mergeFix
	vars *fakePauseVars
}

func newPauseFix(t *testing.T, mutate func(*MergeFacts), names ...string) *pauseFix {
	t.Helper()
	m := newMergeFix(t, mutate, "")
	// newMergeFix already refreshed once against an empty fake; replace it.
	v := &fakePauseVars{names: names}
	m.s.PauseVars = v
	m.f.Exec(t, "DELETE FROM merge_pause_state WHERE repo_owner='fixture'")
	return &pauseFix{mergeFix: m, vars: v}
}

func (p *pauseFix) refresh(t *testing.T) {
	t.Helper()
	if err := p.s.RefreshMergePauses(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// advance moves the clock and keeps the PR snapshot and the facts fresh, so
// only the pause state and the veto deadline decide eligibility.
func (p *pauseFix) advance(t *testing.T, d time.Duration) {
	t.Helper()
	p.clock = p.clock.Add(d)
	p.facts.facts.ObservedAt = p.clock
	p.f.Exec(t, "UPDATE github_pull_request SET snapshot_fetched_at=$2 WHERE id=$1", p.pr, p.clock.Add(-time.Minute))
}

func TestMergePauseHoldsDispatchUntilFreshReadsClear(t *testing.T) {
	p := newPauseFix(t, nil, "AGENT_MERGE_PAUSED", "MAIN_RED_PAUSED", "OTHER")
	p.refresh(t)
	p.tick(t)
	if n := p.runs(t); n != 0 {
		t.Fatalf("%d runs while both pauses are set, want 0", n)
	}
	// Pause stays set for 6 h, re-read every hour: still zero dispatches.
	for i := 0; i < 6; i++ {
		p.advance(t, time.Hour)
		p.refresh(t)
		p.tick(t)
	}
	if n := p.runs(t); n != 0 {
		t.Fatalf("%d runs after 6 h of pause, want 0", n)
	}
	// Both clear on GitHub, but the stored read is past the freshness bound:
	// it is unknown, so nothing is queued until the refresh runs.
	p.vars.names = []string{"OTHER"}
	p.advance(t, 6*time.Minute)
	p.tick(t)
	if n := p.runs(t); n != 0 {
		t.Fatalf("%d runs on a stale pause read, want 0", n)
	}
	p.refresh(t)
	p.tick(t)
	p.tick(t)
	if n := p.runs(t); n != 1 {
		t.Fatalf("%d runs after fresh reads and full eligibility, want 1", n)
	}
}

func TestMergePauseOneSetVariableHolds(t *testing.T) {
	for _, name := range mergePauseVariables {
		t.Run(name, func(t *testing.T) {
			p := newPauseFix(t, nil, name)
			p.refresh(t)
			p.tick(t)
			if n := p.runs(t); n != 0 {
				t.Fatalf("%d runs with %s set, want 0", n, name)
			}
		})
	}
}

func TestMergePauseUnknownWhenNeverReadOrReadFails(t *testing.T) {
	p := newPauseFix(t, nil)
	// Never read.
	if got := p.tick(t).LastError.String; !strings.Contains(got, errMergePauseUnknown.Error()) {
		t.Fatalf("last_error = %q, want the unknown-pause diagnostic", got)
	}
	// The read fails: unknown with the cause shown, never an empty set.
	p.vars.err = errors.New("403 Variables permission missing")
	if err := p.s.RefreshMergePauses(context.Background()); err == nil {
		t.Fatal("RefreshMergePauses hid a failed read")
	}
	got := p.tick(t).LastError.String
	if p.runs(t) != 0 || !strings.Contains(got, errMergePauseUnknown.Error()) || !strings.Contains(got, "Variables permission missing") {
		t.Fatalf("runs=%d last_error=%q, want 0 and the read error", p.runs(t), got)
	}
	// Recovery: a successful read of an empty set dispatches once.
	p.vars.err = nil
	p.advance(t, 2*time.Minute)
	p.refresh(t)
	p.tick(t)
	if n := p.runs(t); n != 1 {
		t.Fatalf("%d runs after recovery, want 1", n)
	}
}

func TestMergePauseRefreshIsSharedAndBounded(t *testing.T) {
	p := newPauseFix(t, nil)
	p.refresh(t)
	p.refresh(t)
	p.advance(t, 30*time.Second)
	p.refresh(t)
	if p.vars.reads != 1 {
		t.Fatalf("%d reads inside the refresh interval, want 1", p.vars.reads)
	}
	p.advance(t, 2*time.Minute)
	p.refresh(t)
	if p.vars.reads != 2 {
		t.Fatalf("%d reads after the interval, want 2", p.vars.reads)
	}
	// A repository no enabled merge_eligibility rule watches is not read.
	p.f.Exec(t, "UPDATE issue_wakeup SET enabled=false,disabled_at=now() WHERE id=$1", p.w.ID)
	p.advance(t, 10*time.Minute)
	p.refresh(t)
	if p.vars.reads != 2 {
		t.Fatalf("%d reads for an unwatched repository, want 2", p.vars.reads)
	}
}

func gatePathFacts(f *MergeFacts) { f.Paths = []string{".github/workflows/ci.yml"} }

func (p *pauseFix) passAt(at time.Time) { p.facts.facts.LastPassAt = &at }

func (p *pauseFix) dueAt(t *testing.T) time.Time {
	t.Helper()
	var due time.Time
	p.f.QueryRow(t, "SELECT due_at FROM merge_veto_deadline WHERE workspace_id=$1", p.f.WorkspaceID).Scan(&due)
	return due
}

func TestMergeVetoDeadlineIsLastPassPlus24h(t *testing.T) {
	p := newPauseFix(t, gatePathFacts)
	pass := p.clock
	p.passAt(pass)
	p.refresh(t)
	p.tick(t)
	if n := p.runs(t); n != 0 {
		t.Fatalf("%d runs inside the veto window, want 0", n)
	}
	if due := p.dueAt(t); !due.Equal(pass.Add(24 * time.Hour)) {
		t.Fatalf("due_at = %s, want %s", due, pass.Add(24*time.Hour))
	}
	p.advance(t, 24*time.Hour-time.Minute)
	p.refresh(t)
	p.tick(t)
	if n := p.runs(t); n != 0 {
		t.Fatalf("%d runs one minute before the deadline, want 0", n)
	}
	p.advance(t, time.Minute)
	p.refresh(t)
	p.tick(t)
	p.tick(t)
	if n := p.runs(t); n != 1 {
		t.Fatalf("%d runs at the deadline, want 1", n)
	}
}

func TestMergeVetoDeadlineArrivingWhilePausedOrRedDispatchesNothing(t *testing.T) {
	cases := map[string]func(*pauseFix, *testing.T){
		"paused":   func(p *pauseFix, _ *testing.T) { p.vars.names = []string{"AGENT_MERGE_PAUSED"} },
		"main red": func(p *pauseFix, _ *testing.T) { p.vars.names = []string{"MAIN_RED_PAUSED"} },
		"checks red": func(p *pauseFix, t *testing.T) {
			p.f.Exec(t, "UPDATE github_pull_request SET checks_rollup_state='FAILURE' WHERE id=$1", p.pr)
		},
	}
	for name, block := range cases {
		t.Run(name, func(t *testing.T) {
			p := newPauseFix(t, gatePathFacts)
			p.passAt(p.clock)
			p.refresh(t)
			p.tick(t)
			block(p, t)
			p.advance(t, 25*time.Hour) // the deadline passes while blocked
			p.refresh(t)
			p.tick(t)
			if n := p.runs(t); n != 0 {
				t.Fatalf("%d runs with the deadline past and the PR %s, want 0", n, name)
			}
			// Unblocked, the passed deadline needs no new wait: exactly one run.
			p.vars.names = nil
			p.f.Exec(t, "UPDATE github_pull_request SET checks_rollup_state='SUCCESS' WHERE id=$1", p.pr)
			p.advance(t, 2*time.Minute)
			p.refresh(t)
			p.tick(t)
			p.tick(t)
			if n := p.runs(t); n != 1 {
				t.Fatalf("%d runs once unblocked, want 1", n)
			}
		})
	}
}

func TestMergeVetoNewerPassMovesTheDeadline(t *testing.T) {
	p := newPauseFix(t, gatePathFacts)
	first := p.clock
	p.passAt(first)
	p.refresh(t)
	p.tick(t)
	p.advance(t, 23*time.Hour)
	second := p.clock
	p.passAt(second)
	p.refresh(t)
	p.tick(t)
	if due := p.dueAt(t); !due.Equal(second.Add(24 * time.Hour)) {
		t.Fatalf("due_at = %s after a newer PASS, want %s", due, second.Add(24*time.Hour))
	}
	// Past the first deadline, inside the second: still zero.
	p.advance(t, 2*time.Hour)
	p.refresh(t)
	p.tick(t)
	if n := p.runs(t); n != 0 {
		t.Fatalf("%d runs after the first deadline but before the moved one, want 0", n)
	}
	// An older PASS never pulls the deadline back.
	p.passAt(first)
	p.tick(t)
	if due := p.dueAt(t); !due.Equal(second.Add(24 * time.Hour)) {
		t.Fatalf("due_at = %s after a stale PASS, want %s", due, second.Add(24*time.Hour))
	}
}
