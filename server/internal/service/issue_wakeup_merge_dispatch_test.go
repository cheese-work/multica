package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Slice C (CHE-5122): at most one queued or active merge run per candidate,
// stale queued work withdrawn, and the candidate judged again before start.

func (m *mergeFix) live(t *testing.T) int {
	t.Helper()
	return m.f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE context->>'wakeup_id'=$1 AND status IN ('queued','dispatched','running')", util.UUIDToString(m.w.ID))
}

func (m *mergeFix) cancelled(t *testing.T) int {
	t.Helper()
	return m.f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE context->>'wakeup_id'=$1 AND status='cancelled'", util.UUIDToString(m.w.ID))
}

func (m *mergeFix) lastTask(t *testing.T) db.AgentTaskQueue {
	t.Helper()
	var id string
	m.f.QueryRow(t, "SELECT id::text FROM agent_task_queue WHERE context->>'wakeup_id'=$1 ORDER BY created_at DESC LIMIT 1", util.UUIDToString(m.w.ID)).Scan(&id)
	task, err := m.f.q.GetAgentTask(context.Background(), parseTestUUID(t, id))
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// setPause makes the fake GitHub report a pause variable and runs the refresh
// after the interval, keeping the PR snapshot and the facts fresh.
func (m *mergeFix) setPause(t *testing.T, name string) {
	t.Helper()
	m.clock = m.clock.Add(2 * time.Minute)
	m.facts.facts.ObservedAt = m.clock
	m.f.Exec(t, "UPDATE github_pull_request SET snapshot_fetched_at=$2 WHERE id=$1", m.pr, m.clock.Add(-time.Minute))
	m.s.PauseVars = &fakePauseVars{names: []string{name}}
	if err := m.s.RefreshMergePauses(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (m *mergeFix) start(t *testing.T) {
	t.Helper()
	m.f.Exec(t, "UPDATE agent_task_queue SET status='running',started_at=now() WHERE context->>'wakeup_id'=$1 AND status='queued'", util.UUIDToString(m.w.ID))
}

func TestMergeDispatchOneLiveRunPerCandidateAcrossRules(t *testing.T) {
	// A parked run (provider hold, waiting for a local directory) still owns the candidate.
	for _, parked := range []string{"queued", "deferred", "waiting_local_directory"} {
		t.Run(parked, func(t *testing.T) {
			m := newMergeFix(t, nil, "")
			// A second issue links the same PR and carries its own rule for the same agent.
			other := m.f.Issue(t, "Second issue for the same PR")
			otherID := parseTestUUID(t, other)
			m.f.Cleanup(t, "DELETE FROM issue_wakeup WHERE issue_id=$1", other)
			m.f.Cleanup(t, "DELETE FROM issue_wakeup_receipt WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE issue_id=$1)", other)
			m.f.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id=$1", other)
			m.f.Cleanup(t, "DELETE FROM issue_pull_request WHERE issue_id=$1", other)
			m.f.Exec(t, "INSERT INTO issue_pull_request(issue_id,pull_request_id) VALUES($1,$2)", other, m.pr)
			second := wakeCreate(t, m.f, m.s, otherID, WakeupInput{AgentID: m.agent, Kind: "event", Mode: "continuous", Instruction: "Merge when eligible",
				Condition: condition(t, map[string]any{"type": "merge_eligibility"})})

			m.tick(t)
			m.f.Exec(t, "UPDATE agent_task_queue SET status=$2 WHERE context->>'wakeup_id'=$1", util.UUIDToString(m.w.ID), parked)
			wakeTick(t, m.f, m.s, second.ID)
			wakeTick(t, m.f, m.s, second.ID)
			m.tick(t)
			total := m.f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE context->>'wakeup_id' IN ($1,$2)", util.UUIDToString(m.w.ID), util.UUIDToString(second.ID))
			if total != 1 {
				t.Fatalf("%d runs for one candidate across two rules while the first is %s, want exactly 1", total, parked)
			}
		})
	}
}

// A running run is not withdrawn when the PR goes red, so the same candidate
// turning green again must not start a second one. (A queued run is withdrawn
// by the red poll, which TestMergeDispatchWithdrawsStaleQueuedRun covers.)
func TestMergeDispatchNoSecondRunWhileTheCandidateHasOne(t *testing.T) {
	for _, state := range []string{"running"} {
		t.Run(state, func(t *testing.T) {
			m := newMergeFix(t, nil, "")
			m.tick(t)
			m.start(t)
			// Red, then green again on the same head: a new episode of the same candidate.
			m.f.Exec(t, "UPDATE github_pull_request SET checks_rollup_state='FAILURE' WHERE id=$1", m.pr)
			m.tick(t)
			m.f.Exec(t, "UPDATE github_pull_request SET checks_rollup_state='SUCCESS' WHERE id=$1", m.pr)
			m.tick(t)
			m.tick(t)
			if n, live := m.runs(t), m.live(t); n != 1 || live != 1 {
				t.Fatalf("%d runs, %d live while the candidate still has a %s run, want 1 and 1", n, live, state)
			}
		})
	}
}

func TestMergeDispatchWithdrawsStaleQueuedRun(t *testing.T) {
	cases := []struct {
		name      string
		change    func(*mergeFix, *testing.T)
		wantLive  int // after the poll that sees the change
		wantTotal int
	}{
		{"head change with fresh facts replaces the run", func(m *mergeFix, t *testing.T) {
			m.f.Exec(t, "UPDATE github_pull_request SET head_sha='b',snapshot_head_sha='b' WHERE id=$1", m.pr)
		}, 1, 2},
		{"base change replaces the run", func(m *mergeFix, t *testing.T) {
			m.f.Exec(t, "UPDATE github_pull_request SET snapshot_base_ref='release' WHERE id=$1", m.pr)
		}, 1, 2},
		{"head change before the snapshot catches up", func(m *mergeFix, t *testing.T) {
			m.f.Exec(t, "UPDATE github_pull_request SET head_sha='b' WHERE id=$1", m.pr)
		}, 0, 1},
		{"revoked evidence", func(m *mergeFix, _ *testing.T) { m.facts.facts.CrossLabPass = ptr(false) }, 0, 1},
		{"checks turn red", func(m *mergeFix, t *testing.T) {
			m.f.Exec(t, "UPDATE github_pull_request SET checks_rollup_state='FAILURE' WHERE id=$1", m.pr)
		}, 0, 1},
		{"facts source fails", func(m *mergeFix, _ *testing.T) { m.facts.err = context.DeadlineExceeded }, 0, 1},
		{"a pause is set", func(m *mergeFix, t *testing.T) { m.setPause(t, "AGENT_MERGE_PAUSED") }, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMergeFix(t, nil, "")
			m.tick(t)
			if live := m.live(t); live != 1 {
				t.Fatalf("%d live runs before the change, want 1", live)
			}
			tc.change(m, t)
			m.tick(t)
			if live, total := m.live(t), m.runs(t); live != tc.wantLive || total != tc.wantTotal {
				t.Fatalf("after the change: %d live, %d total runs, want %d and %d", live, total, tc.wantLive, tc.wantTotal)
			}
			if tc.wantTotal-tc.wantLive != m.cancelled(t) {
				t.Fatalf("%d cancelled runs, want %d", m.cancelled(t), tc.wantTotal-tc.wantLive)
			}
		})
	}
}

func TestMergeDispatchNeverCancelsAStartedRun(t *testing.T) {
	m := newMergeFix(t, nil, "")
	m.tick(t)
	m.start(t)
	m.facts.facts.CrossLabPass = ptr(false)
	m.tick(t)
	if live, cancelled := m.live(t), m.cancelled(t); live != 1 || cancelled != 0 {
		t.Fatalf("%d live, %d cancelled after evidence was revoked mid-run, want 1 and 0", live, cancelled)
	}
}

func TestMergeDispatchRecoversWithOneRunAfterAFailedRead(t *testing.T) {
	m := newMergeFix(t, nil, "")
	m.tick(t)
	m.facts.err = context.DeadlineExceeded
	m.tick(t)
	m.tick(t)
	if live := m.live(t); live != 0 {
		t.Fatalf("%d live runs while the facts read fails, want 0", live)
	}
	m.facts.err = nil
	m.tick(t)
	m.tick(t)
	if live, total := m.live(t), m.runs(t); live != 1 || total != 2 {
		t.Fatalf("%d live, %d total after recovery, want 1 and 2", live, total)
	}
}

// The agent can claim long after the poll that queued it; the claim and the
// start each judge the candidate again against current state.
func TestMergeDispatchReevaluatesBeforeClaimAndStart(t *testing.T) {
	cases := []struct {
		name   string
		change func(*mergeFix, *testing.T)
		want   error
	}{
		{"unchanged", func(*mergeFix, *testing.T) {}, nil},
		{"head changed", func(m *mergeFix, t *testing.T) {
			m.f.Exec(t, "UPDATE github_pull_request SET head_sha='b',snapshot_head_sha='b' WHERE id=$1", m.pr)
		}, ErrWakeupForbidden},
		{"base changed", func(m *mergeFix, t *testing.T) {
			m.f.Exec(t, "UPDATE github_pull_request SET snapshot_base_ref='release' WHERE id=$1", m.pr)
		}, ErrWakeupForbidden},
		{"evidence revoked", func(m *mergeFix, _ *testing.T) { m.facts.facts.CrossLabPass = ptr(false) }, ErrWakeupForbidden},
		{"facts source fails", func(m *mergeFix, _ *testing.T) { m.facts.err = context.DeadlineExceeded }, ErrWakeupForbidden},
		{"PR closed", func(m *mergeFix, t *testing.T) {
			m.f.Exec(t, "UPDATE github_pull_request SET state='closed' WHERE id=$1", m.pr)
		}, ErrWakeupForbidden},
		{"pause set", func(m *mergeFix, t *testing.T) { m.setPause(t, "MAIN_RED_PAUSED") }, ErrWakeupForbidden},
		{"facts went stale", func(m *mergeFix, _ *testing.T) { m.clock = m.clock.Add(301 * time.Second) }, ErrWakeupForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMergeFix(t, nil, "")
			m.tick(t)
			task := m.lastTask(t)
			tc.change(m, t) // no poll in between: the claim is delayed
			ctx := context.Background()
			if err := m.s.CheckClaim(ctx, task); !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("CheckClaim = %v, want %v", err, tc.want)
			}
			if err := m.s.CheckStart(ctx, task); !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("CheckStart = %v, want %v", err, tc.want)
			}
		})
	}
}

// Claim-time facts come from the shared task service, because the daemon
// handler builds its own wakeup service without the scheduler's fields.
func TestMergeDispatchClaimReadsFactsFromTheTaskService(t *testing.T) {
	m := newMergeFix(t, nil, "")
	m.s.Tasks.MergeFacts = m.facts
	m.tick(t)
	task := m.lastTask(t)
	claimer := &IssueWakeupService{Tasks: m.s.Tasks, Now: m.s.Now}
	if err := claimer.CheckClaim(context.Background(), task); err != nil {
		t.Fatalf("CheckClaim with facts on the task service = %v, want nil", err)
	}
	m.s.Tasks.MergeFacts = nil
	if err := claimer.CheckClaim(context.Background(), task); !errors.Is(err, ErrWakeupForbidden) {
		t.Fatalf("CheckClaim with no facts source = %v, want ErrWakeupForbidden", err)
	}
}
