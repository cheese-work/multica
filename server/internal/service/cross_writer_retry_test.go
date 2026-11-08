package service

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
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
// Writers (recovery reason provider_server_error) are the three
// reserveProviderFailureRecovery call sites; they share the failed_task_id
// unique ledger row and are exercised in TestCrossWriterRecoveryReservation.

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

// Recovery writers (autopilot schedule, autopilot run, issue wakeup) reserve the
// same failed task through one ledger row. A second writer of any effect kind
// is refused; a generic writer on the same failed task mints nothing.
func TestCrossWriterRecoveryReservation(t *testing.T) {
	o := newOwnerFixture(t)
	ctx := context.Background()
	effects := []struct {
		name string
		make func(failed string) db.RecordProviderFailureRecoveryParams
	}{
		{"task_retry", o.taskRetryEffect},
		{"wakeup_rearm", func(failed string) db.RecordProviderFailureRecoveryParams {
			return o.wakeupRearmEffect(failed, o.wakeup(t, "at", "once", nil), 2)
		}},
	}
	for _, first := range effects {
		for _, second := range effects {
			t.Run(first.name+" then "+second.name, func(t *testing.T) {
				failed := o.failedTask(t, nil)
				for _, w := range crossWriters() {
					if n := o.retryChildren(t, failed); n != 0 {
						t.Fatalf("before %s: successors=%d", w.name, n)
					}
				}
				one, two := first.make(failed), second.make(failed)
				if rows, err := o.q.RecordProviderFailureRecovery(ctx, one); err != nil || rows != 1 {
					t.Fatalf("first reservation rows=%d err=%v", rows, err)
				}
				two.TriggerID = one.TriggerID
				two.TriggerKind = one.TriggerKind
				if rows, err := o.q.RecordProviderFailureRecovery(ctx, two); err != nil || rows != 0 {
					t.Fatalf("second reservation rows=%d err=%v, want refused", rows, err)
				}
			})
		}
	}
	t.Run("generic writers mint nothing for the recovery reason", func(t *testing.T) {
		for _, w := range crossWriters() {
			id := o.runningTask(t, testutil.Cols{"max_attempts": 3})
			w.run(t, o, id, providerFailureRecoveryReason)
			if n := o.retryChildren(t, id); n != 0 {
				t.Fatalf("%s minted %d successors for %s", w.name, n, providerFailureRecoveryReason)
			}
		}
	})
}

// writerInventory is every production function that mints a successor or
// reserves a recovery. A new function must be added here, with a harness case
// above, before it can ship.
var writerInventory = map[string]string{
	"task.go:failTask":                                          "CreateRetryTask: failTask",
	"task.go:MaybeRetryFailedTask":                              "CreateRetryTask: maybeRetry, watchdog",
	"autopilot.go:dispatchCreateIssue":                          "reserveProviderFailureRecovery",
	"autopilot.go:dispatchRunOnly":                              "reserveProviderFailureRecovery",
	"issue_wakeup.go:dispatch":                                  "reserveProviderFailureRecovery",
	"provider_failure_guard.go:reserveProviderFailureRecovery":  "RecordProviderFailureRecovery",
	"provider_failure_guard.go:applyProviderFailureReservation": "RecordProviderFailureRecovery",
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
	var missing, stale []string
	for k := range found {
		if _, ok := writerInventory[k]; !ok {
			missing = append(missing, k)
		}
	}
	for k := range writerInventory {
		if !found[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 || len(stale) > 0 {
		t.Fatalf("writer inventory drift: unlisted=%v stale=%v", missing, stale)
	}
}
