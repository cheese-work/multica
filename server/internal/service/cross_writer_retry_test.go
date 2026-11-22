package service

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-4581 (CHE-2804): cross-writer harness. Every writer that can mint a
// successor for one failed task goes through the same fence; this file runs the
// writers against the same task in every order and state and asserts one
// successor at most.
//
// Writers (generic retry reasons, see retryableReasons):
//   - failTask: FailTask's in-transaction retry (daemon fail / heartbeat path).
//   - maybeRetry: MaybeRetryFailedTask called directly (recover-orphans).
//   - watchdog: the sweeper, which marks the task failed and hands it to
//     HandleFailedTasks (runtime_sweeper.go, task_lifecycle.go).
// Writers (recovery reason provider_server_error) are the scheduled autopilot
// (run_only and create_issue) and the recurring wakeup. They reserve through the
// failed_task_id unique ledger row and are driven through their public dispatch
// in TestCrossWriterRecoveryMintsOneSuccessor and its race twin.

// providerFailureRecoveryReason is the one reason the recovery guard owns.
const providerFailureRecoveryReason = "agent_error.provider_server_error"

type retryWriter struct {
	name string
	// run acts on the task. It never fails the test for "no retry".
	run func(t *testing.T, o ownerFixture, id, reason string)
}

func crossWriters() []retryWriter {
	ctx := context.Background()
	return []retryWriter{
		{"failTask", func(t *testing.T, o ownerFixture, id, reason string) {
			t.Helper()
			// A replay on an already-terminal row is an idempotent no-op (or an error);
			// only the successor count is asserted.
			_, _ = o.svc.TaskSvc.FailTask(ctx, parseTestUUID(t, id), "boom", "", "", "", reason, false, "", "")
		}},
		{"maybeRetry", func(t *testing.T, o ownerFixture, id, reason string) {
			t.Helper()
			parent, err := o.q.GetAgentTask(ctx, parseTestUUID(t, id))
			if err != nil {
				t.Fatal(err)
			}
			if parent.Status != "failed" {
				// Nothing has failed this task yet; the caller of MaybeRetryFailedTask
				// always holds a failed row.
				o.Exec(t, "UPDATE agent_task_queue SET status='failed', failure_reason=$2, completed_at=now() WHERE id=$1 AND status='running'", id, reason)
				if parent, err = o.q.GetAgentTask(ctx, parseTestUUID(t, id)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := o.svc.TaskSvc.MaybeRetryFailedTask(ctx, parent); err != nil {
				t.Fatal(err)
			}
		}},
		{"watchdog", func(t *testing.T, o ownerFixture, id, reason string) {
			t.Helper()
			// The sweeper fails the row conditionally on running, then hands the
			// rows it won to HandleFailedTasks. A row it lost is not handed over.
			rows, err := o.Pool.Query(ctx,
				`UPDATE agent_task_queue SET status='failed', failure_reason=$2, error='swept', completed_at=now()
				 WHERE id=$1 AND status='running' RETURNING id`, id, reason)
			if err != nil {
				t.Fatal(err)
			}
			won := rows.Next()
			rows.Close()
			if !won {
				return
			}
			task, err := o.q.GetAgentTask(ctx, parseTestUUID(t, id))
			if err != nil {
				t.Fatal(err)
			}
			o.svc.TaskSvc.HandleFailedTasks(ctx, []db.AgentTaskQueue{task})
		}},
	}
}

// settle moves the successor out of the pending slot the way the daemon would.
func (o ownerFixture) settle(t *testing.T, parentID, state string) {
	t.Helper()
	switch state {
	case "pending":
	case "running":
		o.Exec(t, "UPDATE agent_task_queue SET status='running', started_at=now() WHERE retry_of_task_id=$1", parentID)
	case "completed":
		o.Exec(t, "UPDATE agent_task_queue SET status='completed', completed_at=now() WHERE retry_of_task_id=$1", parentID)
	default:
		t.Fatalf("unknown state %q", state)
	}
}

// Writer A mints the successor; the successor is then pending, running or done;
// writer B (any writer, including A again) acts on the same failed task. One
// successor in total.
func TestCrossWriterGenericRetryMintsOneSuccessor(t *testing.T) {
	o := newOwnerFixture(t)
	for _, reason := range sortedRetryableReasons() {
		for _, a := range crossWriters() {
			for _, state := range []string{"pending", "running", "completed"} {
				for _, b := range crossWriters() {
					name := fmt.Sprintf("%s/%s then %s then %s", reason, a.name, state, b.name)
					t.Run(name, func(t *testing.T) {
						id := o.runningTask(t, testutil.Cols{"max_attempts": 3})
						a.run(t, o, id, reason)
						if n := o.retryChildren(t, id); n > 1 {
							t.Fatalf("writer %s minted %d successors", a.name, n)
						}
						o.settle(t, id, state)
						b.run(t, o, id, reason)
						if n := o.retryChildren(t, id); n > 1 {
							t.Fatalf("successors=%d after %s then %s (%s), want at most 1", n, a.name, b.name, state)
						}
					})
				}
			}
		}
	}
}

// All writers race on one running task: exactly one wins the failure
// transition, so exactly one successor exists.
func TestCrossWriterConcurrentRaceMintsOneSuccessor(t *testing.T) {
	o := newOwnerFixture(t)
	for _, reason := range []string{"timeout", "runtime_recovery"} {
		t.Run(reason, func(t *testing.T) {
			id := o.runningTask(t, testutil.Cols{"max_attempts": 3})
			var wg sync.WaitGroup
			start := make(chan struct{})
			for range 3 {
				for _, w := range crossWriters() {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						w.run(t, o, id, reason)
					}()
				}
			}
			close(start)
			wg.Wait()
			if n := o.retryChildren(t, id); n != 1 {
				t.Fatalf("successors=%d, want exactly 1", n)
			}
		})
	}
}

// Two replays of the sweeper hand-off race on one already-failed task. The
// capacity reason defers its successor, so the pending-slot index does not
// dedupe it: the per-parent guard has to.
func TestCrossWriterConcurrentReplayMintsOneSuccessor(t *testing.T) {
	o := newOwnerFixture(t)
	ctx := context.Background()
	for _, reason := range []string{"timeout", "agent_error.provider_capacity_or_rate_limit"} {
		t.Run(reason, func(t *testing.T) {
			id := o.runningTask(t, testutil.Cols{"max_attempts": 3})
			o.Exec(t, "UPDATE agent_task_queue SET status='failed', failure_reason=$2, completed_at=now() WHERE id=$1", id, reason)
			parent, err := o.q.GetAgentTask(ctx, parseTestUUID(t, id))
			if err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			start := make(chan struct{})
			for range 6 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					if _, err := o.svc.TaskSvc.MaybeRetryFailedTask(ctx, parent); err != nil {
						t.Error(err)
					}
				}()
			}
			close(start)
			wg.Wait()
			if n := o.retryChildren(t, id); n != 1 {
				t.Fatalf("successors=%d, want exactly 1", n)
			}
		})
	}
}

// The generic writers and the recovery writers cover disjoint reasons, so no
// failed task is eligible for both families.
func TestCrossWriterGenericAndRecoveryReasonsAreDisjoint(t *testing.T) {
	if retryableReasons[providerFailureRecoveryReason] {
		t.Fatalf("%s is both generically retryable and recovery-guard owned", providerFailureRecoveryReason)
	}
}

// steadyProbe is the provider edge: the gateway answers 401, the signal the
// recovery guard reads as "recovered". It is safe for concurrent writers.
type steadyProbe struct{}

func (steadyProbe) Probe(context.Context) (ProviderFailureProbeEvidence, error) {
	return ProviderFailureProbeEvidence{CheckedAt: time.Now().UTC(), StatusCode: http.StatusUnauthorized}, nil
}

// recoveryScene is one failed provider_server_error task that the recovery
// writers can all see: a scheduled autopilot run failed it and, when
// withWakeup is set, a recurring wakeup owns it too.
type recoveryScene struct {
	f        principalFixture
	wake     *IssueWakeupService
	apID     string
	trigger  string
	wakeup   db.IssueWakeup
	agentID  string
	seedTask string
}

func newRecoveryScene(t *testing.T, mode string, withWakeup bool) *recoveryScene {
	t.Helper()
	ctx := context.Background()
	f, wake, issue, agentID := wakeFixture(t)
	f.svc.TaskSvc.ProviderFailureProbe = steadyProbe{}
	apID, trigger := f.autopilotWithTrigger(t, agentID, f.UserID, f.UserID)
	f.Exec(t, "UPDATE autopilot SET execution_mode=$2 WHERE id=$1", apID, mode)
	f.Cleanup(t, "DELETE FROM agent_task_queue WHERE agent_id=$1", agentID)
	f.Cleanup(t, "DELETE FROM provider_failure_recovery WHERE failed_task_id IN (SELECT id FROM agent_task_queue WHERE agent_id=$1)", agentID)
	f.Cleanup(t, "DELETE FROM autopilot_run WHERE autopilot_id=$1", apID)
	s := &recoveryScene{f: f, wake: wake, apID: apID, trigger: trigger, agentID: agentID}
	// Fixture issues carry fixed numbers; create_issue dispatch allocates from the counter.
	f.Exec(t, "UPDATE workspace SET issue_counter=(SELECT COALESCE(MAX(number),0) FROM issue WHERE workspace_id=$1) WHERE id=$1", f.WorkspaceID)

	// The real first dispatch creates the run and its task; the task then fails.
	run := f.dispatch(t, apID, trigger)
	if err := f.Pool.QueryRow(ctx, `SELECT id::text FROM agent_task_queue WHERE agent_id=$1`, agentID).Scan(&s.seedTask); err != nil {
		t.Fatalf("seed task of the first dispatch: %v", err)
	}
	f.Exec(t, `UPDATE agent_task_queue SET status='failed', failure_reason=$2, completed_at=now()-interval '61 minutes' WHERE id=$1`, s.seedTask, providerFailureRecoveryReason)
	f.Exec(t, `UPDATE autopilot_run SET status='failed', completed_at=now()-interval '61 minutes' WHERE id=$1`, util.UUIDToString(run.ID))
	if withWakeup {
		s.wakeup = wakeCreate(t, f, wake, issue, WakeupInput{
			AgentID: agentID, Kind: "every", Mode: "continuous", IntervalSeconds: 3600, Instruction: "Check current status",
		})
		f.Exec(t, `UPDATE agent_task_queue SET context = COALESCE(context, '{}'::jsonb) || jsonb_build_object('wakeup_id', $2::text) WHERE id=$1`,
			s.seedTask, util.UUIDToString(s.wakeup.ID))
	}
	return s
}

// successors counts the tasks every writer created for the failed task: the
// scene's agent only ever owns the seed and what the writers minted.
func (s *recoveryScene) successors(t *testing.T) int {
	t.Helper()
	var n int
	if err := s.f.Pool.QueryRow(context.Background(), `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND id<>$2`, s.agentID, s.seedTask).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// reservations counts ledger rows over every task of the scene: a second row
// for the seed or for its successor is a second recovery.
func (s *recoveryScene) reservations(t *testing.T) int {
	t.Helper()
	var n int
	if err := s.f.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM provider_failure_recovery WHERE failed_task_id IN (SELECT id FROM agent_task_queue WHERE agent_id=$1)`, s.agentID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// retryTasks counts tasks linked as a generic retry of another task. The
// recovery writers never mint those.
func (s *recoveryScene) retryTasks(t *testing.T) int {
	t.Helper()
	var n int
	if err := s.f.Pool.QueryRow(context.Background(), `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND retry_of_task_id IS NOT NULL`, s.agentID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// settle moves what the first writer created to the given state.
func (s *recoveryScene) settle(t *testing.T, state string) {
	t.Helper()
	switch state {
	case "active":
	case "completed":
		s.f.Exec(t, `UPDATE agent_task_queue SET status='completed', completed_at=now() WHERE agent_id=$1 AND id<>$2`, s.agentID, s.seedTask)
		s.f.Exec(t, `UPDATE autopilot_run SET status='completed', completed_at=now() WHERE trigger_id=$1 AND status IN ('running','issue_created')`, s.trigger)
	case "failed":
		// The recovery itself fails with the same provider error: the lineage cap.
		s.f.Exec(t, `UPDATE agent_task_queue SET status='failed', failure_reason=$3, completed_at=now()-interval '61 minutes' WHERE agent_id=$1 AND id<>$2`, s.agentID, s.seedTask, providerFailureRecoveryReason)
		s.f.Exec(t, `UPDATE autopilot_run SET status='failed', completed_at=now()-interval '61 minutes' WHERE trigger_id=$1 AND status IN ('running','issue_created')`, s.trigger)
	default:
		t.Fatalf("unknown state %q", state)
	}
}

// recoveryWriters are the production entry points that reserve a recovery,
// each driven through its public dispatch.
var recoveryWriters = map[string]func(t *testing.T, s *recoveryScene){
	"autopilotRunOnly":     dispatchScheduledAutopilot,
	"autopilotCreateIssue": dispatchScheduledAutopilot,
	"wakeup": func(t *testing.T, s *recoveryScene) {
		t.Helper()
		ctx := context.Background()
		s.f.Exec(t, "UPDATE issue_wakeup SET next_fire_at=now()-interval '1 second' WHERE id=$1", s.wakeup.ID)
		w, err := s.f.q.GetIssueWakeup(ctx, db.GetIssueWakeupParams{ID: s.wakeup.ID, WorkspaceID: s.wakeup.WorkspaceID})
		if err != nil {
			t.Error(err)
			return
		}
		if err := s.wake.dispatch(ctx, w); err != nil {
			// A racing loser can time out on the winner's row lock; the tick
			// loop records that and retries. Any other error is a failure.
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
				return
			}
			t.Error(err)
		}
	},
}

func dispatchScheduledAutopilot(t *testing.T, s *recoveryScene) {
	t.Helper()
	ctx := context.Background()
	ap, err := s.f.q.GetAutopilot(ctx, util.MustParseUUID(s.apID))
	if err != nil {
		t.Error(err)
		return
	}
	if _, err := s.f.svc.DispatchAutopilot(ctx, ap, util.MustParseUUID(s.trigger), "schedule", nil); err != nil {
		t.Error(err)
	}
}

// recoveryShapes lists, per kind of failed task, the recovery writers that can
// see it. A failed run_only autopilot task is also a wakeup task here, so the
// two writers compete for it; a create_issue task has only its autopilot.
var recoveryShapes = []struct {
	name       string
	mode       string
	withWakeup bool
	writers    []string
}{
	{"run_only autopilot and wakeup", "run_only", true, []string{"autopilotRunOnly", "wakeup"}},
	{"create_issue autopilot", "create_issue", false, []string{"autopilotCreateIssue"}},
}

// Writer A reserves the recovery; its effect is then active, completed or failed
// with the same provider error; writer B (any writer, including A again) acts on
// the same failed task. One successor and one ledger row in total.
func TestCrossWriterRecoveryMintsOneSuccessor(t *testing.T) {
	for _, shape := range recoveryShapes {
		for _, a := range shape.writers {
			for _, state := range []string{"active", "completed", "failed"} {
				for _, b := range shape.writers {
					t.Run(fmt.Sprintf("%s/%s then %s then %s", shape.name, a, state, b), func(t *testing.T) {
						s := newRecoveryScene(t, shape.mode, shape.withWakeup)
						recoveryWriters[a](t, s)
						if n, r := s.successors(t), s.reservations(t); n != 1 || r != 1 {
							t.Fatalf("after %s: successors=%d reservations=%d, want 1 and 1", a, n, r)
						}
						s.settle(t, state)
						recoveryWriters[b](t, s)
						n, r := s.successors(t), s.reservations(t)
						// A completed recovery ends the failure: the next scheduled
						// tick is ordinary work, not a retry, so one extra task is
						// legitimate there. The ledger still holds exactly one
						// recovery, and no task is a retry of another.
						maxTasks := 1
						if state == "completed" {
							maxTasks = 2
						}
						if n < 1 || n > maxTasks || r != 1 || s.retryTasks(t) != 0 {
							t.Fatalf("after %s, %s, %s: successors=%d (max %d) reservations=%d retry_tasks=%d, want reservations 1 and no retry tasks",
								a, state, b, n, maxTasks, r, s.retryTasks(t))
						}
					})
				}
			}
		}
	}
}

// Every recovery writer of a shape races on the same failed task.
func TestCrossWriterRecoveryConcurrentRaceMintsOneSuccessor(t *testing.T) {
	for _, shape := range recoveryShapes {
		t.Run(shape.name, func(t *testing.T) {
			s := newRecoveryScene(t, shape.mode, shape.withWakeup)
			var wg sync.WaitGroup
			start := make(chan struct{})
			for range 3 {
				for _, name := range shape.writers {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						recoveryWriters[name](t, s)
					}()
				}
			}
			close(start)
			wg.Wait()
			if n, r := s.successors(t), s.reservations(t); n != 1 || r != 1 {
				t.Fatalf("successors=%d reservations=%d, want 1 and 1", n, r)
			}
		})
	}
}

// The generic writers and the recovery writers cover disjoint reasons, so a
// generic writer mints nothing for a provider_server_error failure.
func TestCrossWriterGenericWritersMintNothingForRecoveryReason(t *testing.T) {
	o := newOwnerFixture(t)
	for _, w := range crossWriters() {
		t.Run(w.name, func(t *testing.T) {
			id := o.runningTask(t, testutil.Cols{"max_attempts": 3})
			w.run(t, o, id, providerFailureRecoveryReason)
			// CHE-1736 M1.2: failTask owns the one held retry for this reason;
			// every other generic writer still mints nothing.
			want := 0
			if w.name == "failTask" {
				want = 1
			}
			if n := o.retryChildren(t, id); n != want {
				t.Fatalf("%s minted %d successors for %s, want %d", w.name, n, providerFailureRecoveryReason, want)
			}
			if want == 1 {
				var held bool
				if err := o.Pool.QueryRow(context.Background(), `SELECT context ? 'provider_hold' FROM agent_task_queue WHERE retry_of_task_id=$1 OR parent_task_id=$1`, id).Scan(&held); err != nil || !held {
					t.Fatalf("%s successor is not stamped with the provider_hold marker", w.name)
				}
			}
		})
	}
}

// writerInventory is every production function that mints a successor or
// reserves a recovery, mapped to the harness writers that drive it. A new
// function fails until it is listed; a listed writer fails until a table above
// runs it.
var writerInventory = map[string][]string{
	"task.go:failTask":                                          {"failTask"},
	"task.go:MaybeRetryFailedTask":                              {"maybeRetry", "watchdog"},
	"autopilot.go:dispatchCreateIssue":                          {"autopilotCreateIssue"},
	"autopilot.go:dispatchRunOnly":                              {"autopilotRunOnly"},
	"issue_wakeup.go:dispatch":                                  {"wakeup"},
	"provider_failure_guard.go:reserveProviderFailureRecovery":  {"autopilotRunOnly", "autopilotCreateIssue", "wakeup"},
	"provider_failure_guard.go:applyProviderFailureReservation": {"autopilotRunOnly", "autopilotCreateIssue", "wakeup"},
}

// exercisedWriters is what the tables run: crossWriters() and the recovery
// shapes' writers, each of which must also have a runner.
func exercisedWriters(t *testing.T) map[string]bool {
	t.Helper()
	ran := map[string]bool{}
	for _, w := range crossWriters() {
		ran[w.name] = true
	}
	for _, shape := range recoveryShapes {
		for _, name := range shape.writers {
			if recoveryWriters[name] == nil {
				t.Fatalf("recovery shape %q names writer %q without a runner", shape.name, name)
			}
			ran[name] = true
		}
	}
	return ran
}

func TestCrossWriterInventoryIsComplete(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	found := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
			strings.Contains(filepath.ToSlash(path), "pkg/db/generated/") {
			return err
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			writes := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
						switch sel.Sel.Name {
						case "CreateRetryTask", "reserveProviderFailureRecovery", "RecordProviderFailureRecovery":
							writes = true
						}
					}
				}
				return true
			})
			if writes {
				found[filepath.Base(path)+":"+fn.Name.Name] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ran := exercisedWriters(t)
	var missing, stale, unexercised []string
	for k := range found {
		if _, ok := writerInventory[k]; !ok {
			missing = append(missing, k)
		}
	}
	for k, writers := range writerInventory {
		if !found[k] {
			stale = append(stale, k)
		}
		for _, w := range writers {
			if !ran[w] {
				unexercised = append(unexercised, k+" -> "+w)
			}
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	sort.Strings(unexercised)
	if len(missing) > 0 || len(stale) > 0 || len(unexercised) > 0 {
		t.Fatalf("writer inventory drift: unlisted=%v stale=%v without a harness case=%v", missing, stale, unexercised)
	}
}
