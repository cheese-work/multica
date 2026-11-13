package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type pauseAgent struct {
	id, runtimeID pgtype.UUID
	taskID        string
}

type pauseFixture struct {
	pool        *pgxpool.Pool
	svc         *TaskService
	workspaceID pgtype.UUID
	actorID     pgtype.UUID
	agents      []pauseAgent
	fx          *testutil.Fixture
}

// newPauseFixture builds one workspace with n agents, each on its own online
// runtime and holding one queued task.
func newPauseFixture(t *testing.T, n int) pauseFixture {
	t.Helper()
	pool := sharedTestPool(t)
	suffix := time.Now().UnixNano()
	bootstrap := testutil.New(pool, "", "")
	userID := bootstrap.User(t, fmt.Sprintf("pause-%d", suffix), fmt.Sprintf("pause-%d@example.com", suffix))
	workspaceID := bootstrap.Workspace(t, fmt.Sprintf("pause-%d", suffix), fmt.Sprintf("pause-%d", suffix))
	fx := testutil.New(pool, workspaceID, userID)
	fx.Member(t, workspaceID, userID, "owner")

	f := pauseFixture{
		pool:        pool,
		svc:         &TaskService{Queries: db.New(pool), TxStarter: pool},
		workspaceID: util.MustParseUUID(workspaceID),
		actorID:     util.MustParseUUID(userID),
		fx:          fx,
	}
	for i := 0; i < n; i++ {
		rt := fx.Runtime(t, fmt.Sprintf("rt-%d", i), testutil.Cols{"visibility": "public", "owner_id": userID})
		ag := fx.Agent(t, fmt.Sprintf("agent-%d", i), rt, testutil.Cols{"owner_id": userID, "max_concurrent_tasks": 5})
		issue := fx.Issue(t, fmt.Sprintf("pause issue %d", i))
		task := fx.Task(t, ag, testutil.Cols{"runtime_id": rt, "issue_id": issue, "status": "queued"})
		f.agents = append(f.agents, pauseAgent{id: util.MustParseUUID(ag), runtimeID: util.MustParseUUID(rt), taskID: task})
	}
	return f
}

// candidates counts the queued claim candidates for the agent's runtime and
// checks the singular and batch queries agree.
func (f pauseFixture) candidates(t *testing.T, a pauseAgent) int {
	t.Helper()
	ctx := context.Background()
	q := db.New(f.pool)
	single, err := q.ListQueuedClaimCandidatesByRuntime(ctx, a.runtimeID)
	if err != nil {
		t.Fatalf("list candidates: %v", err)
	}
	batch, err := q.ListQueuedClaimCandidatesByRuntimes(ctx, []pgtype.UUID{a.runtimeID})
	if err != nil {
		t.Fatalf("list batch candidates: %v", err)
	}
	if len(single) != len(batch) {
		t.Fatalf("singular candidates = %d, batch = %d; fences drifted", len(single), len(batch))
	}
	return len(single)
}

// claim reports whether the agent can claim a queued task right now, and how
// many candidates the lists offered before the claim.
func (f pauseFixture) claim(t *testing.T, a pauseAgent) (claimed bool, candidates int) {
	t.Helper()
	candidates = f.candidates(t, a)
	_, err := db.New(f.pool).ClaimAgentTask(context.Background(), db.ClaimAgentTaskParams{
		AgentID: a.id, RuntimeID: a.runtimeID,
		PrepareLeaseSecs: 60, RuntimeStaleSecs: RuntimeClaimFreshnessSeconds,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("claim: %v", err)
	}
	return err == nil, candidates
}

func TestAgentSchedulingPauseBlocksClaimNotEnqueue(t *testing.T) {
	ctx := context.Background()
	f := newPauseFixture(t, 1)
	a := f.agents[0]

	// A task already running when the pause lands must stay untouched.
	issue3 := f.fx.Issue(t, "already running")
	running := f.fx.Task(t, util.UUIDToString(a.id), testutil.Cols{
		"runtime_id": util.UUIDToString(a.runtimeID), "issue_id": issue3, "status": "running",
	})

	if _, _, err := f.svc.PauseAgentScheduling(ctx, f.workspaceID, a.id, f.actorID, pgtype.Text{}); err != nil {
		t.Fatalf("pause: %v", err)
	}

	// Enqueue keeps working while paused: a second queued row lands fine.
	issue2 := f.fx.Issue(t, "enqueued while paused")
	f.fx.Task(t, util.UUIDToString(a.id), testutil.Cols{
		"runtime_id": util.UUIDToString(a.runtimeID), "issue_id": issue2, "status": "queued",
	})
	claimed, candidates := f.claim(t, a)
	if claimed {
		t.Fatal("paused agent claimed a queued task")
	}
	if candidates != 0 {
		t.Fatalf("paused agent still has %d claim candidates", candidates)
	}
	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM agent_task_queue WHERE id = $1`, running).Scan(&status); err != nil {
		t.Fatalf("read running task: %v", err)
	}
	if status != "running" {
		t.Fatalf("running task status = %q after pause, want running", status)
	}
}

func TestWorkspaceSchedulingPauseBlocksEveryAgent(t *testing.T) {
	ctx := context.Background()
	f := newPauseFixture(t, 2)

	if _, _, err := f.svc.PauseWorkspaceScheduling(ctx, f.workspaceID, f.actorID); err != nil {
		t.Fatalf("pause workspace: %v", err)
	}
	for i, a := range f.agents {
		if claimed, candidates := f.claim(t, a); claimed || candidates != 0 {
			t.Fatalf("agent %d under paused workspace: claimed=%v candidates=%d, want none", i, claimed, candidates)
		}
	}

	// A different workspace is unaffected.
	other := newPauseFixture(t, 1)
	if claimed, _ := other.claim(t, other.agents[0]); !claimed {
		t.Fatal("agent in an unpaused workspace could not claim")
	}
}

func TestSchedulingResumeRestoresClaim(t *testing.T) {
	ctx := context.Background()

	t.Run("agent", func(t *testing.T) {
		f := newPauseFixture(t, 1)
		a := f.agents[0]
		if _, _, err := f.svc.PauseAgentScheduling(ctx, f.workspaceID, a.id, f.actorID, pgtype.Text{}); err != nil {
			t.Fatalf("pause: %v", err)
		}
		if candidates := f.candidates(t, a); candidates != 0 {
			t.Fatalf("precondition: paused agent has %d claim candidates", candidates)
		}
		agent, _, err := f.svc.ResumeAgentScheduling(ctx, f.workspaceID, a.id)
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if agent.SchedulingPausedAt.Valid || agent.SchedulingPausedBy.Valid {
			t.Fatal("resume left the pause columns set")
		}
		if claimed, _ := f.claim(t, a); !claimed {
			t.Fatal("agent could not claim after resume")
		}
	})

	t.Run("workspace", func(t *testing.T) {
		f := newPauseFixture(t, 2)
		if _, _, err := f.svc.PauseWorkspaceScheduling(ctx, f.workspaceID, f.actorID); err != nil {
			t.Fatalf("pause: %v", err)
		}
		for i, a := range f.agents {
			if candidates := f.candidates(t, a); candidates != 0 {
				t.Fatalf("precondition: agent %d has %d claim candidates under a paused workspace", i, candidates)
			}
		}
		ws, _, err := f.svc.ResumeWorkspaceScheduling(ctx, f.workspaceID)
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if ws.SchedulingPausedAt.Valid || ws.SchedulingPausedBy.Valid {
			t.Fatal("resume left the pause columns set")
		}
		for i, a := range f.agents {
			if claimed, _ := f.claim(t, a); !claimed {
				t.Fatalf("agent %d could not claim after workspace resume", i)
			}
		}
	})
}

func TestSchedulingResumeBumpsEmptyClaimVersion(t *testing.T) {
	ctx := context.Background()
	cache := NewEmptyClaimCache(newRedisTestClient(t))

	check := func(t *testing.T, f pauseFixture, resume func() error) {
		t.Helper()
		f.svc.EmptyClaim = cache
		for _, a := range f.agents {
			key := util.UUIDToString(a.runtimeID)
			cache.MarkEmpty(ctx, key, cache.CurrentVersion(ctx, key))
			if !cache.IsEmpty(ctx, key) {
				t.Fatal("precondition: empty verdict should be trusted before resume")
			}
		}
		if err := resume(); err != nil {
			t.Fatalf("resume: %v", err)
		}
		for i, a := range f.agents {
			if cache.IsEmpty(ctx, util.UUIDToString(a.runtimeID)) {
				t.Fatalf("runtime of agent %d still trusts a pre-resume empty verdict", i)
			}
		}
	}

	t.Run("agent", func(t *testing.T) {
		f := newPauseFixture(t, 1)
		check(t, f, func() error {
			_, _, err := f.svc.ResumeAgentScheduling(ctx, f.workspaceID, f.agents[0].id)
			return err
		})
	})
	t.Run("workspace", func(t *testing.T) {
		f := newPauseFixture(t, 2)
		check(t, f, func() error {
			_, _, err := f.svc.ResumeWorkspaceScheduling(ctx, f.workspaceID)
			return err
		})
	})
}

// failingQuery makes any statement containing match fail, so a test can break
// one step of a multi-step service method.
type failingQuery struct {
	db.DBTX
	match string
}

func (f failingQuery) Query(ctx context.Context, sql string, args ...interface{}) (pgx.Rows, error) {
	if strings.Contains(sql, f.match) {
		return nil, errors.New("injected query failure")
	}
	return f.DBTX.Query(ctx, sql, args...)
}

// A failed workspace resume must leave the workspace paused: resuming first and
// then failing to list runtimes would return an error with the state changed
// and no runtime woken or invalidated.
func TestWorkspaceResumeFailureLeavesWorkspacePaused(t *testing.T) {
	ctx := context.Background()
	f := newPauseFixture(t, 1)
	if _, _, err := f.svc.PauseWorkspaceScheduling(ctx, f.workspaceID, f.actorID); err != nil {
		t.Fatalf("pause: %v", err)
	}

	f.svc.Queries = db.New(failingQuery{DBTX: f.pool, match: "FROM agent_runtime"})
	if _, _, err := f.svc.ResumeWorkspaceScheduling(ctx, f.workspaceID); err == nil {
		t.Fatal("resume succeeded although runtime listing failed")
	}

	var paused bool
	if err := f.pool.QueryRow(ctx, `SELECT scheduling_paused_at IS NOT NULL FROM workspace WHERE id = $1`, f.workspaceID).Scan(&paused); err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	if !paused {
		t.Fatal("failed resume cleared the pause")
	}
}

func TestSecondPauseKeepsFirstActorAndTime(t *testing.T) {
	ctx := context.Background()
	f := newPauseFixture(t, 1)
	a := f.agents[0]
	otherActor := util.MustParseUUID(f.fx.User(t, fmt.Sprintf("second-%d", time.Now().UnixNano()), fmt.Sprintf("second-%d@example.com", time.Now().UnixNano())))

	first, _, err := f.svc.PauseAgentScheduling(ctx, f.workspaceID, a.id, f.actorID, pgtype.Text{})
	if err != nil {
		t.Fatalf("first pause: %v", err)
	}
	second, _, err := f.svc.PauseAgentScheduling(ctx, f.workspaceID, a.id, otherActor, pgtype.Text{})
	if err != nil {
		t.Fatalf("second pause: %v", err)
	}
	if second.SchedulingPausedBy != first.SchedulingPausedBy || !second.SchedulingPausedAt.Time.Equal(first.SchedulingPausedAt.Time) {
		t.Fatalf("second pause overwrote first: by %v->%v at %v->%v",
			first.SchedulingPausedBy, second.SchedulingPausedBy, first.SchedulingPausedAt.Time, second.SchedulingPausedAt.Time)
	}

	w1, _, err := f.svc.PauseWorkspaceScheduling(ctx, f.workspaceID, f.actorID)
	if err != nil {
		t.Fatalf("first workspace pause: %v", err)
	}
	w2, _, err := f.svc.PauseWorkspaceScheduling(ctx, f.workspaceID, otherActor)
	if err != nil {
		t.Fatalf("second workspace pause: %v", err)
	}
	if w2.SchedulingPausedBy != w1.SchedulingPausedBy || !w2.SchedulingPausedAt.Time.Equal(w1.SchedulingPausedAt.Time) {
		t.Fatal("second workspace pause overwrote the first")
	}
}
