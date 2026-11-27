package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// DB-backed coverage for queued-run start order across agents on one runtime
// (CHE-10088, plan CHE-10050 r4 step 3). Candidates are listed in
// `priority DESC, created_at ASC, id ASC`; the claim loops must start the
// oldest claimable row first, not each agent's oldest claimable row in
// candidate order. Every row carries an explicit created_at so the order never
// depends on insert timing. Each case runs on both claim loops.

// fifoPath is one claim loop under test. claim polls once and returns the rows
// that poll started.
type fifoPath struct {
	name  string
	claim func(t *testing.T, f *claimDecisionFixture) []db.AgentTaskQueue
	// path is the claim_decision_total path label the loop reports under.
	path string
}

func fifoPaths() []fifoPath {
	return []fifoPath{
		{
			name: "batch",
			path: claimPathBatch,
			claim: func(t *testing.T, f *claimDecisionFixture) []db.AgentTaskQueue {
				t.Helper()
				claimed, _, err := f.svc.ClaimTasksForRuntimes(context.Background(), []pgtype.UUID{f.runtimeID}, 8)
				if err != nil {
					t.Fatalf("ClaimTasksForRuntimes: %v", err)
				}
				return claimed
			},
		},
		{
			name: "runtime",
			path: claimPathRuntime,
			claim: func(t *testing.T, f *claimDecisionFixture) []db.AgentTaskQueue {
				t.Helper()
				task, err := f.svc.ClaimTaskForRuntime(context.Background(), f.runtimeID)
				if err != nil {
					t.Fatalf("ClaimTaskForRuntime: %v", err)
				}
				if task == nil {
					return nil
				}
				return []db.AgentTaskQueue{*task}
			},
		},
	}
}

func forEachFIFOPath(t *testing.T, run func(t *testing.T, p fifoPath)) {
	t.Helper()
	for _, p := range fifoPaths() {
		t.Run(p.name, func(t *testing.T) { run(t, p) })
	}
}

// fifoBase is a fixed origin well in the past, so every row is also older than
// any claim deadline measured from now().
func fifoBase() time.Time {
	return time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Microsecond)
}

func fifoAt(minutes int) time.Time {
	return fifoBase().Add(time.Duration(minutes) * time.Minute)
}

func (f *claimDecisionFixture) fifoAgent(t *testing.T, name string) string {
	t.Helper()
	return f.fx.Agent(t, name, util.UUIDToString(f.runtimeID), testutil.Cols{
		"owner_id":             f.fx.UserID,
		"max_concurrent_tasks": 5,
	})
}

// fifoRow queues a row for agentID at created_at `at`.
func (f *claimDecisionFixture) fifoRow(t *testing.T, agentID string, at time.Time, extra testutil.Cols) string {
	t.Helper()
	cols := testutil.Cols{
		"runtime_id": util.UUIDToString(f.runtimeID),
		"status":     "queued",
		"created_at": at,
	}
	for k, v := range extra {
		cols[k] = v
	}
	return f.fx.Task(t, agentID, cols)
}

// fifoRunning inserts an active run, which also takes a runtime slot.
func (f *claimDecisionFixture) fifoRunning(t *testing.T, agentID string, at time.Time, extra testutil.Cols) string {
	t.Helper()
	cols := testutil.Cols{
		"status":        "running",
		"dispatched_at": testutil.Raw("now()"),
		"started_at":    testutil.Raw("now()"),
	}
	for k, v := range extra {
		cols[k] = v
	}
	return f.fifoRow(t, agentID, at, cols)
}

func (f *claimDecisionFixture) fifoComplete(t *testing.T, taskID pgtype.UUID) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, taskID,
	); err != nil {
		t.Fatalf("complete task: %v", err)
	}
}

func wantOnly(t *testing.T, claimed []db.AgentTaskQueue, want string) {
	t.Helper()
	if len(claimed) != 1 {
		t.Fatalf("claimed %d rows, want exactly row %s", len(claimed), want)
	}
	if got := util.UUIDToString(claimed[0].ID); got != want {
		t.Fatalf("claimed row %s, want the older claimable row %s", got, want)
	}
}

// A: cap 1, B's row older than A's. B starts first; once it finishes, A.
func TestRuntimeFIFOAcrossAgentsOlderRowFirst(t *testing.T) {
	forEachFIFOPath(t, func(t *testing.T, p fifoPath) {
		f := newClaimDecisionFixture(t, 5)
		f.setRuntimeCap(t, int32Ptr(1))
		agentB := f.fifoAgent(t, "fifo-a-b")
		aRow := f.fifoRow(t, util.UUIDToString(f.agentID), fifoAt(2), testutil.Cols{"issue_id": f.issueID})
		bRow := f.fifoRow(t, agentB, fifoAt(1), testutil.Cols{"issue_id": f.fx.Issue(t, "fifo-a-b issue")})

		first := p.claim(t, f)
		wantOnly(t, first, bRow)
		f.fifoComplete(t, first[0].ID)
		wantOnly(t, p.claim(t, f), aRow)
	})
}

// B1: A's oldest row is blocked by A's active run on the same issue. With one
// free slot, B's middle row starts, not A's newest row.
func TestRuntimeFIFOSkipsAgentOldestRowBlockedBySameIssueRun(t *testing.T) {
	forEachFIFOPath(t, func(t *testing.T, p fifoPath) {
		f := newClaimDecisionFixture(t, 5)
		agentA := util.UUIDToString(f.agentID)
		agentB := f.fifoAgent(t, "fifo-b1-b")
		f.fifoRunning(t, agentA, fifoAt(-5), testutil.Cols{"issue_id": f.issueID})
		f.setRuntimeCap(t, int32Ptr(2)) // one running + one free slot
		f.fifoRow(t, agentA, fifoAt(1), testutil.Cols{"issue_id": f.issueID})
		bRow := f.fifoRow(t, agentB, fifoAt(2), testutil.Cols{"issue_id": f.fx.Issue(t, "fifo-b1-b issue")})
		f.fifoRow(t, agentA, fifoAt(3), testutil.Cols{"issue_id": f.fx.Issue(t, "fifo-b1-a newest issue")})

		wantOnly(t, p.claim(t, f), bRow)
	})
}

// B2: as B1, A's oldest row is a DM turn whose DM already has an active run.
func TestRuntimeFIFOSkipsAgentOldestRowBlockedByDMRun(t *testing.T) {
	forEachFIFOPath(t, func(t *testing.T, p fifoPath) {
		f := newClaimDecisionFixture(t, 5)
		agentA := util.UUIDToString(f.agentID)
		agentB := f.fifoAgent(t, "fifo-b2-b")
		session := f.fx.ChatSession(t, agentA)
		f.fifoRunning(t, agentA, fifoAt(-5), testutil.Cols{"chat_session_id": session})
		f.setRuntimeCap(t, int32Ptr(2))
		f.fifoRow(t, agentA, fifoAt(1), testutil.Cols{"chat_session_id": session})
		bRow := f.fifoRow(t, agentB, fifoAt(2), testutil.Cols{"issue_id": f.fx.Issue(t, "fifo-b2-b issue")})
		f.fifoRow(t, agentA, fifoAt(3), testutil.Cols{"issue_id": f.fx.Issue(t, "fifo-b2-a newest issue")})

		wantOnly(t, p.claim(t, f), bRow)
	})
}

// B3: as B1, A's oldest row is a background row past its claim deadline.
func TestRuntimeFIFOSkipsAgentOldestExpiredBackgroundRow(t *testing.T) {
	forEachFIFOPath(t, func(t *testing.T, p fifoPath) {
		f := newClaimDecisionFixture(t, 5)
		agentA := util.UUIDToString(f.agentID)
		agentB := f.fifoAgent(t, "fifo-b3-b")
		f.setRuntimeCap(t, int32Ptr(1))
		f.fifoRow(t, agentA, fifoAt(1), testutil.Cols{
			"issue_id": f.fx.Issue(t, "fifo-b3-a expired issue"),
			"context":  testutil.Raw(`'{"type":"background","claim_deadline_seconds":1}'::jsonb`),
		})
		bRow := f.fifoRow(t, agentB, fifoAt(2), testutil.Cols{"issue_id": f.fx.Issue(t, "fifo-b3-b issue")})
		f.fifoRow(t, agentA, fifoAt(3), testutil.Cols{"issue_id": f.fx.Issue(t, "fifo-b3-a newest issue")})

		wantOnly(t, p.claim(t, f), bRow)
	})
}

// C: priority still outranks age: a newer urgent row starts before an older
// normal row.
func TestRuntimeFIFOUrgentNewerRowBeforeNormalOlderRow(t *testing.T) {
	forEachFIFOPath(t, func(t *testing.T, p fifoPath) {
		f := newClaimDecisionFixture(t, 5)
		agentB := f.fifoAgent(t, "fifo-c-b")
		f.setRuntimeCap(t, int32Ptr(1))
		bNormal := f.fifoRow(t, agentB, fifoAt(1), testutil.Cols{"issue_id": f.fx.Issue(t, "fifo-c-b issue")})
		aUrgent := f.fifoRow(t, util.UUIDToString(f.agentID), fifoAt(2), testutil.Cols{
			"issue_id": f.issueID,
			"priority": 4,
		})

		first := p.claim(t, f)
		wantOnly(t, first, aUrgent)
		f.fifoComplete(t, first[0].ID)
		wantOnly(t, p.claim(t, f), bNormal)
	})
}

// D: a runtime whose only queued row is blocked is not marked empty: the poll
// finds nothing to start, but the candidate was visible, so the loop attempted
// it and never took the empty-database exit that arms the empty-claim cache.
func TestRuntimeFIFOBlockedOnlyRowIsNotAnEmptyRuntime(t *testing.T) {
	forEachFIFOPath(t, func(t *testing.T, p fifoPath) {
		f := newClaimDecisionFixture(t, 5)
		agentA := util.UUIDToString(f.agentID)
		f.fifoRunning(t, agentA, fifoAt(-5), testutil.Cols{"issue_id": f.issueID})
		f.fifoRow(t, agentA, fifoAt(1), testutil.Cols{"issue_id": f.issueID})

		if got := p.claim(t, f); len(got) != 0 {
			t.Fatalf("claimed %d rows, want 0 (the only queued row is blocked)", len(got))
		}
		if got := claimDecisionCount(t, f.metrics, p.path, claimOutcomeEmptyDB, claimReasonNoVisibleCandidates); got != 0 {
			t.Fatalf("empty_db decisions = %v, want 0: a blocked row must not mark the runtime empty", got)
		}
		if got := claimDecisionCount(t, f.metrics, p.path, claimOutcomeNoTasks, claimReasonCandidatesUnclaimed); got != 1 {
			t.Fatalf("no_tasks/candidates_unclaimed decisions = %v, want 1 attempt on the blocked row", got)
		}
	})
}

// E: at the runtime cap a poll makes one attempt for the runtime, not one per
// agent.
func TestRuntimeFIFOAtRuntimeCapMakesOneAttemptPerRuntime(t *testing.T) {
	forEachFIFOPath(t, func(t *testing.T, p fifoPath) {
		f := newClaimDecisionFixture(t, 5)
		agentA := util.UUIDToString(f.agentID)
		f.fifoRunning(t, agentA, fifoAt(-5), testutil.Cols{"issue_id": f.fx.Issue(t, "fifo-e running issue")})
		f.setRuntimeCap(t, int32Ptr(1))
		for i, name := range []string{"fifo-e-b", "fifo-e-c", "fifo-e-d"} {
			agent := f.fifoAgent(t, name)
			f.fifoRow(t, agent, fifoAt(i+1), testutil.Cols{"issue_id": f.fx.Issue(t, name+" issue")})
		}
		f.fifoRow(t, agentA, fifoAt(5), testutil.Cols{"issue_id": f.fx.Issue(t, "fifo-e-a queued issue")})

		if got := p.claim(t, f); len(got) != 0 {
			t.Fatalf("claimed %d rows at the runtime cap, want 0", len(got))
		}
		if got := claimDecisionCount(t, f.metrics, p.path, claimOutcomeRuntimeNoCapacity, claimReasonRuntimeConcurrencyCap); got != 1 {
			t.Fatalf("runtime_no_capacity attempts = %v, want 1 per runtime (4 agents are queued)", got)
		}
	})
}

// FP-01 (REQUIRED_TEST, plan hash
// a61eb51aef60fd143be0208500afdfc888d0c7e4bdfda46748d58c3d38de32fb): 64
// blocked rows alternate between agents A and B, then agent C has an eligible
// row 65, and agent A has a newer claimable row 66 behind it. With one free
// slot one poll must start row 65, not A's newer row 66, on both loops, and
// repeated polls must keep reaching the head of the queue, never stalling on
// the same blocked prefix. There is no attempt ceiling.
func TestRuntimeFIFOBlockedPrefixNeverStarvesLaterAgent(t *testing.T) {
	forEachFIFOPath(t, func(t *testing.T, p fifoPath) {
		f := newClaimDecisionFixture(t, 5)
		f.setRuntimeCap(t, int32Ptr(1))
		agentA := util.UUIDToString(f.agentID)
		agentB := f.fifoAgent(t, "fifo-fp01-b")
		agentC := f.fifoAgent(t, "fifo-fp01-c")
		expired := testutil.Raw(`'{"type":"background","claim_deadline_seconds":1}'::jsonb`)
		for i := 0; i < 64; i++ {
			agent := agentA
			if i%2 == 1 {
				agent = agentB
			}
			f.fifoRow(t, agent, fifoAt(i), testutil.Cols{
				"issue_id": f.fx.Issue(t, fmt.Sprintf("fifo-fp01 blocked %d", i)),
				"context":  expired,
			})
		}
		row65 := f.fifoRow(t, agentC, fifoAt(64), testutil.Cols{"issue_id": f.fx.Issue(t, "fifo-fp01 eligible 65")})

		row66 := f.fifoRow(t, agentA, fifoAt(65), testutil.Cols{"issue_id": f.fx.Issue(t, "fifo-fp01 newer A row 66")})

		first := p.claim(t, f)
		wantOnly(t, first, row65)

		// Repeated polls: A's row 66 is next, then each new eligible row behind
		// the same 64 blocked rows is reached on its own poll.
		f.fifoComplete(t, first[0].ID)
		second := p.claim(t, f)
		wantOnly(t, second, row66)
		prev := second[0].ID
		for round := 0; round < 3; round++ {
			f.fifoComplete(t, prev)
			next := f.fifoRow(t, agentC, fifoAt(66+round), testutil.Cols{"issue_id": f.fx.Issue(t, fmt.Sprintf("fifo-fp01 eligible %d", 67+round))})
			got := p.claim(t, f)
			wantOnly(t, got, next)
			prev = got[0].ID
		}
	})
}

// AG (CHE-9926 aging): the claim bound compares the aged priority, not the
// stored one. A's oldest row is stored at priority 0 but has aged to high and is
// blocked; A's newer row is stored high. B's row sits between them. The bound of
// A's first group must exclude A's newer row (equal aged priority, newer), so
// B's row starts.
func TestRuntimeFIFOBoundUsesAgedPriority(t *testing.T) {
	forEachFIFOPath(t, func(t *testing.T, p fifoPath) {
		f := newClaimDecisionFixture(t, 5)
		agentA := util.UUIDToString(f.agentID)
		agentB := f.fifoAgent(t, "fifo-ag-b")
		now := time.Now().UTC().Truncate(time.Microsecond)
		f.fifoRunning(t, agentA, now.Add(-4*time.Hour), testutil.Cols{"issue_id": f.issueID})
		f.setRuntimeCap(t, int32Ptr(2)) // one running + one free slot
		f.fifoRow(t, agentA, now.Add(-170*time.Minute), testutil.Cols{"issue_id": f.issueID, "priority": 0})
		bRow := f.fifoRow(t, agentB, now.Add(-100*time.Minute), testutil.Cols{"issue_id": f.fx.Issue(t, "fifo-ag-b issue"), "priority": 3})
		f.fifoRow(t, agentA, now.Add(-50*time.Minute), testutil.Cols{"issue_id": f.fx.Issue(t, "fifo-ag-a newer issue"), "priority": 3})

		wantOnly(t, p.claim(t, f), bRow)
	})
}
