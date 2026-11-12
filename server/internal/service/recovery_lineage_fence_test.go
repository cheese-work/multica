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
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-1773 (CHE-1732 M0d): a recovery-lineage task gets no generic retry for any
// reason, on every entry point that can mint one.

func sortedRetryableReasons() []string {
	out := make([]string, 0, len(retryableReasons))
	for r := range retryableReasons {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// runningTask queues a running task that can be failed with reason.
func (o ownerFixture) runningTask(t *testing.T, over testutil.Cols) string {
	t.Helper()
	cols := testutil.Cols{
		"status": "running", "failure_reason": nil, "completed_at": nil,
		"started_at": testutil.Raw("now()"), "accountable_user_id": o.UserID, "max_attempts": 2,
	}
	for k, v := range over {
		cols[k] = v
	}
	id := o.failedTask(t, cols)
	o.Cleanup(t, "DELETE FROM inbox_item WHERE id=$1", id)
	// A pending retry child blocks the next retry for this issue and agent.
	o.Cleanup(t, "DELETE FROM agent_task_queue WHERE parent_task_id=$1 OR retry_of_task_id=$1", id)
	return id
}

func (o ownerFixture) retryChildren(t *testing.T, parentID string) int {
	t.Helper()
	var n int
	if err := o.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM agent_task_queue WHERE parent_task_id=$1 OR retry_of_task_id=$1`, parentID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// recoveryTask seeds a recovery retry task (recovery_task_id of a task_retry row).
func (o ownerFixture) recoveryTask(t *testing.T, over testutil.Cols) string {
	t.Helper()
	recovery := o.runningTask(t, over)
	o.recordRecovery(t, o.failedTask(t, nil), db.RecordProviderFailureRecoveryParams{RecoveryTaskID: parseTestUUID(t, recovery)})
	return recovery
}

func TestRecoveryLineageGetsNoGenericRetry(t *testing.T) {
	o := newOwnerFixture(t)
	ctx := context.Background()
	for _, reason := range sortedRetryableReasons() {
		for _, subject := range []string{"recovery task", "retry child of recovery"} {
			for _, entry := range []string{"failTask", "MaybeRetryFailedTask"} {
				t.Run(fmt.Sprintf("%s/%s/%s", reason, subject, entry), func(t *testing.T) {
					recovery := o.recoveryTask(t, nil)
					failed := recovery
					if subject != "recovery task" {
						failed = o.runningTask(t, testutil.Cols{"retry_of_task_id": recovery})
					}
					switch entry {
					case "failTask":
						if _, err := o.svc.TaskSvc.FailTask(ctx, parseTestUUID(t, failed), "boom", "", "", "", reason, false, "", ""); err != nil {
							t.Fatal(err)
						}
					default:
						o.Exec(t, "UPDATE agent_task_queue SET status='failed', failure_reason=$2, completed_at=now() WHERE id=$1", failed, reason)
						parent, err := o.q.GetAgentTask(ctx, parseTestUUID(t, failed))
						if err != nil {
							t.Fatal(err)
						}
						child, err := o.svc.TaskSvc.MaybeRetryFailedTask(ctx, parent)
						if err != nil || child != nil {
							t.Fatalf("MaybeRetryFailedTask child=%v err=%v, want none", child, err)
						}
					}
					if n := o.retryChildren(t, failed); n != 0 {
						t.Fatalf("retry children=%d, want 0", n)
					}
					if n := o.countNotices(t, failed); n != 1 {
						t.Fatalf("owner notices=%d, want 1", n)
					}
				})
			}
		}
	}
}

// Ordinary tasks keep the generic schedule. provider_server_error left this
// table in CHE-1736 M1.2: its one retry is held (provider_held_retry_integration_test.go). Expected values are fixed literals
// taken from the schedule before CHE-1773, not read back from the helpers.
func TestNonRecoveryTaskKeepsGenericRetry(t *testing.T) {
	o := newOwnerFixture(t)
	ctx := context.Background()
	const network = "agent_error.provider_network"
	for _, tc := range []struct {
		name, reason     string
		attempt, taskMax int
		retried          bool
		childMax         int
		delay            time.Duration
	}{
		{"runtime_offline", "runtime_offline", 1, 2, true, 2, time.Second},
		{"runtime_recovery", "runtime_recovery", 1, 2, true, 2, 0},
		{"timeout", "timeout", 1, 2, true, 2, 0},
		{"codex_semantic_inactivity", "codex_semantic_inactivity", 1, 2, true, 2, 0},
		{"skill_bundle_unavailable", "skill_bundle_unavailable", 1, 2, true, 2, 0},
		{"provider_capacity_or_rate_limit", "agent_error.provider_capacity_or_rate_limit", 1, 2, true, 2, 10 * time.Minute},
		{"provider_network tier 1", network, 1, 2, true, 3, 0},
		{"provider_network tier 2", network, 2, 2, true, 3, 5 * time.Second},
		{"provider_network tier 3 is exhausted", network, 3, 3, false, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := o.runningTask(t, testutil.Cols{"attempt": tc.attempt, "max_attempts": tc.taskMax})
			before := time.Now()
			if _, err := o.svc.TaskSvc.FailTask(ctx, parseTestUUID(t, id), "boom", "", "", "", tc.reason, false, "", ""); err != nil {
				t.Fatal(err)
			}
			after := time.Now()
			if n := o.countNotices(t, id); n != 0 {
				t.Fatalf("owner notices=%d, want 0", n)
			}
			if !tc.retried {
				if n := o.retryChildren(t, id); n != 0 {
					t.Fatalf("retry children=%d, want 0", n)
				}
				return
			}
			var attempt, maxAttempts int
			var status string
			var fireAt pgtype.Timestamptz
			if err := o.Pool.QueryRow(ctx,
				`SELECT attempt, max_attempts, status, fire_at FROM agent_task_queue WHERE parent_task_id=$1 OR retry_of_task_id=$1`, id).Scan(&attempt, &maxAttempts, &status, &fireAt); err != nil {
				t.Fatalf("no retry child: %v", err)
			}
			wantStatus := "queued"
			if tc.delay > 0 {
				wantStatus = "deferred"
			}
			if attempt != tc.attempt+1 || maxAttempts != tc.childMax || status != wantStatus || fireAt.Valid != (tc.delay > 0) {
				t.Fatalf("child attempt=%d max=%d status=%s fire_at set=%t, want attempt=%d max=%d status=%s fire_at set=%t",
					attempt, maxAttempts, status, fireAt.Valid, tc.attempt+1, tc.childMax, wantStatus, tc.delay > 0)
			}
			if tc.delay > 0 {
				slack := time.Second
				if fireAt.Time.Before(before.Add(tc.delay-slack)) || fireAt.Time.After(after.Add(tc.delay+slack)) {
					t.Fatalf("fire_at=%s, want %s after the failure", fireAt.Time, tc.delay)
				}
			}
		})
	}
}

// A run of a re-armed wakeup at the new revision is in the lineage; an earlier
// run (older revision) is not.
func TestRecoveryLineageWakeupRevision(t *testing.T) {
	o := newOwnerFixture(t)
	ctx := context.Background()
	wakeupID := o.wakeup(t, "at", "once", nil)
	o.recordRecovery(t, o.failedTask(t, nil), db.RecordProviderFailureRecoveryParams{
		RecoveryWakeupID: parseTestUUID(t, wakeupID), RecoveryWakeupRevision: pgtype.Int8{Int64: 5, Valid: true},
	})
	for _, tc := range []struct {
		revision int
		retried  bool
	}{{4, true}, {5, false}, {6, false}} {
		t.Run(fmt.Sprintf("revision %d", tc.revision), func(t *testing.T) {
			id := o.runningTask(t, testutil.Cols{"context": testutil.Raw(fmt.Sprintf(`'{"wakeup_id":%q,"wakeup_revision":%d}'::jsonb`, wakeupID, tc.revision))})
			if _, err := o.svc.TaskSvc.FailTask(ctx, parseTestUUID(t, id), "boom", "", "", "", "timeout", false, "", ""); err != nil {
				t.Fatal(err)
			}
			if got := o.retryChildren(t, id) == 1; got != tc.retried {
				t.Fatalf("retried=%t, want %t", got, tc.retried)
			}
		})
	}
}

// A chain longer than the depth cap is a lineage hit; a cycle terminates.
func TestRecoveryLineageDepthCapAndCycle(t *testing.T) {
	o := newOwnerFixture(t)
	ctx := context.Background()

	chain := func(n int) (head, tail string) {
		tail = o.recoveryTask(t, nil)
		head = tail
		for i := 0; i < n; i++ {
			head = o.runningTask(t, testutil.Cols{"retry_of_task_id": head})
		}
		return head, tail
	}
	failTimeout := func(t *testing.T, id string) {
		t.Helper()
		if _, err := o.svc.TaskSvc.FailTask(ctx, parseTestUUID(t, id), "boom", "", "", "", "timeout", false, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("chain within cap reaches the recovery task", func(t *testing.T) {
		head, _ := chain(30)
		failTimeout(t, head)
		if n := o.retryChildren(t, head); n != 0 {
			t.Fatalf("retry children=%d, want 0", n)
		}
	})
	t.Run("chain longer than cap is a hit", func(t *testing.T) {
		head, _ := chain(40)
		failTimeout(t, head)
		if n := o.retryChildren(t, head); n != 0 {
			t.Fatalf("retry children=%d, want 0", n)
		}
	})
	t.Run("cycle without recovery is no hit", func(t *testing.T) {
		a := o.runningTask(t, nil)
		b := o.runningTask(t, testutil.Cols{"retry_of_task_id": a})
		o.Exec(t, "UPDATE agent_task_queue SET retry_of_task_id=$2 WHERE id=$1", a, b)
		t.Cleanup(func() { o.Exec(t, "UPDATE agent_task_queue SET retry_of_task_id=NULL WHERE id IN ($1,$2)", a, b) })
		// The walk ends on the cycle and is no cap. b is already a's one retry
		// child (CHE-4581), so failing a mints no second one.
		fence, err := o.svc.TaskSvc.fenceRecoveryLineage(ctx, o.q, parseTestUUID(t, a))
		if err != nil || fence.Capped() {
			t.Fatalf("fence capped=%t err=%v, want no cap", fence.Capped(), err)
		}
		failTimeout(t, a)
		if n := o.retryChildren(t, a); n != 1 {
			t.Fatalf("retry children=%d, want 1 (b only)", n)
		}
	})
}

// Every CreateRetryTask call in production code sits in a function that consults
// the fence. A third call site without it fails here.
func TestEveryCreateRetryTaskCallSiteIsFenced(t *testing.T) {
	allow := map[string]bool{} // named exceptions: file:func
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	sites := 0
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
			creates, fenced := false, false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
						creates = creates || sel.Sel.Name == "CreateRetryTask"
						fenced = fenced || sel.Sel.Name == "fenceRecoveryLineage"
					}
				}
				return true
			})
			if creates {
				sites++
				key := filepath.Base(path) + ":" + fn.Name.Name
				if !fenced && !allow[key] {
					t.Errorf("%s calls CreateRetryTask without fenceRecoveryLineage", key)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if sites != 2 {
		t.Fatalf("CreateRetryTask call sites=%d, want 2 (failTask, MaybeRetryFailedTask)", sites)
	}
}
