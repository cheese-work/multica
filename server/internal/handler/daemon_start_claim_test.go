package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

func startClaimFixture(t *testing.T, status string) (string, string, time.Time) {
	t.Helper()
	if testHandler == nil {
		t.Fatal("PostgreSQL is required for start claim tests")
	}
	var agentID string
	dbfx.QueryRow(t, `SELECT id FROM agent WHERE workspace_id=$1 LIMIT 1`, testWorkspaceID).Scan(&agentID)
	issueID := dbfx.Issue(t, "start claim ownership")
	generation := time.Date(2026, 9, 17, 9, 0, 0, 123456000, time.UTC)
	taskID := dbfx.Task(t, agentID, testutil.Cols{
		"issue_id": issueID, "runtime_id": testRuntimeID,
		"status": status, "dispatched_at": generation,
	})
	return taskID, testRuntimeID, generation
}

func startClaimRequest(taskID, runtimeID string, generation time.Time) *http.Request {
	return withURLParam(newDaemonTokenRequest("POST", "/api/daemon/tasks/"+taskID+"/start", map[string]string{
		"runtime_id": runtimeID, "dispatched_at": generation.Format(time.RFC3339Nano),
	}, testWorkspaceID, "start-claim-test"), "taskId", taskID)
}

func TestStartClaimLostResponseAndOwnership(t *testing.T) {
	for _, state := range []string{"dispatched", "waiting_local_directory"} {
		t.Run(state, func(t *testing.T) {
			id, runtimeID, generation := startClaimFixture(t, state)
			// A stale generation on the same runtime cannot win the FIRST start.
			testutil.Call(t, testHandler.StartTask, startClaimRequest(id, runtimeID, generation.Add(-time.Microsecond))).Want(http.StatusConflict)
			testutil.Call(t, testHandler.StartTask, startClaimRequest(id, "00000000-0000-0000-0000-000000000001", generation)).Want(http.StatusConflict)
			// Commit the first request, then discard its response (lost acknowledgement).
			testutil.Call(t, testHandler.StartTask, startClaimRequest(id, runtimeID, generation)).Want(http.StatusOK)
			var before pgtype.Timestamptz
			dbfx.QueryRow(t, `SELECT started_at FROM agent_task_queue WHERE id=$1`, id).Scan(&before)
			testutil.Call(t, testHandler.StartTask, startClaimRequest(id, runtimeID, generation)).Want(http.StatusOK)
			testutil.Call(t, testHandler.StartTask, startClaimRequest(id, runtimeID, generation.Add(-time.Microsecond))).Want(http.StatusConflict)
			testutil.Call(t, testHandler.StartTask, startClaimRequest(id, "00000000-0000-0000-0000-000000000001", generation)).Want(http.StatusConflict)
			var after pgtype.Timestamptz
			dbfx.QueryRow(t, `SELECT started_at FROM agent_task_queue WHERE id=$1`, id).Scan(&after)
			if !before.Valid || !before.Time.Equal(after.Time) {
				t.Fatalf("replay changed started_at: %v -> %v", before, after)
			}
		})
	}
}

func TestStartClaimReclaimedGeneration(t *testing.T) {
	id, runtimeID, generation := startClaimFixture(t, "dispatched")
	// Reclaim refreshes dispatched_at even if the runtime stays the same.
	newGeneration := generation.Add(time.Microsecond)
	dbfx.Exec(t, `UPDATE agent_task_queue SET dispatched_at=$2 WHERE id=$1`, id, newGeneration)
	testutil.Call(t, testHandler.StartTask, startClaimRequest(id, runtimeID, generation)).Want(http.StatusConflict)
	testutil.Call(t, testHandler.StartTask, startClaimRequest(id, runtimeID, newGeneration)).Want(http.StatusOK)
	testutil.Call(t, testHandler.StartTask, startClaimRequest(id, runtimeID, generation)).Want(http.StatusConflict)
}

func TestStartClaimConcurrentReplay(t *testing.T) {
	id, runtimeID, generation := startClaimFixture(t, "dispatched")
	const workers = 12
	results := make(chan *httptest.ResponseRecorder, workers)
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			w := httptest.NewRecorder()
			testHandler.StartTask(w, startClaimRequest(id, runtimeID, generation))
			results <- w
		}()
	}
	close(gate)
	wg.Wait()
	close(results)
	var started string
	for w := range results {
		if w.Code != http.StatusOK {
			t.Fatalf("concurrent start: %d %s", w.Code, w.Body.String())
		}
		var task AgentTaskResponse
		if err := json.Unmarshal(w.Body.Bytes(), &task); err != nil {
			t.Fatal(err)
		}
		if task.StartedAt == nil {
			t.Fatal("missing started_at")
		}
		if started != "" && started != *task.StartedAt {
			t.Fatal("multiple start timestamps")
		}
		started = *task.StartedAt
	}
}

func TestStartClaimCancellationRace(t *testing.T) {
	for _, replay := range []bool{false, true} {
		t.Run(fmt.Sprintf("replay=%t", replay), func(t *testing.T) {
			id, runtimeID, generation := startClaimFixture(t, "dispatched")
			if replay {
				testutil.Call(t, testHandler.StartTask, startClaimRequest(id, runtimeID, generation)).Want(http.StatusOK)
			}
			ctx := context.Background()
			tx, err := testPool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `UPDATE agent_task_queue SET status='cancelled' WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
			result := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				w := httptest.NewRecorder()
				testHandler.StartTask(w, startClaimRequest(id, runtimeID, generation))
				result <- w
			}()
			// Observe the actual PostgreSQL row-lock wait before committing cancellation.
			deadline := time.Now().Add(5 * time.Second)
			for {
				var blocked bool
				dbfx.QueryRow(t, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '-- name: LockAgentTaskStartClaim%')`).Scan(&blocked)
				if blocked {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("start did not wait on cancellation lock")
				}
				time.Sleep(5 * time.Millisecond)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case w := <-result:
				if w.Code != http.StatusConflict {
					t.Fatalf("cancelled start: %d %s", w.Code, w.Body.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("start blocked after cancellation committed")
			}
			testutil.Call(t, testHandler.StartTask, startClaimRequest(id, runtimeID, generation)).Want(http.StatusConflict)
			var status string
			dbfx.QueryRow(t, `SELECT status FROM agent_task_queue WHERE id=$1`, id).Scan(&status)
			if status != "cancelled" {
				t.Fatalf("cancellation overwritten: %s", status)
			}
		})
	}
}

func TestStartClaimInvalidBodiesAndLegacy(t *testing.T) {
	id, runtimeID, generation := startClaimFixture(t, "dispatched")
	for _, body := range []any{
		map[string]string{"runtime_id": runtimeID},
		map[string]string{"dispatched_at": generation.Format(time.RFC3339Nano)},
		map[string]string{"runtime_id": "invalid", "dispatched_at": generation.Format(time.RFC3339Nano)},
		map[string]string{"runtime_id": runtimeID, "dispatched_at": generation.Add(time.Nanosecond).Format(time.RFC3339Nano)},
	} {
		req := withURLParam(newDaemonTokenRequest("POST", "/start", body, testWorkspaceID, "legacy-test"), "taskId", id)
		testutil.Call(t, testHandler.StartTask, req).Want(http.StatusBadRequest)
	}
	for _, code := range []int{http.StatusOK, http.StatusBadRequest} {
		req := withURLParam(newDaemonTokenRequest("POST", "/start", nil, testWorkspaceID, "legacy-test"), "taskId", id)
		testutil.Call(t, testHandler.StartTask, req).Want(code)
	}
}

func TestStartClaimWirePrecision(t *testing.T) {
	id, runtimeID, generation := startClaimFixture(t, "dispatched")
	task, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(id))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := testHandler.Queries.GetAgentRuntime(context.Background(), parseUUID(runtimeID))
	if err != nil {
		t.Fatal(err)
	}
	req := newDaemonTokenRequest("POST", "/claim", nil, testWorkspaceID, "claim-wire-test")
	// Exercise non-UTC database timestamp locations even on a UTC CI runner.
	for _, zone := range []*time.Location{time.UTC, time.FixedZone("UTC+08", 8*60*60), time.FixedZone("UTC-07", -7*60*60)} {
		t.Run(zone.String(), func(t *testing.T) {
			claimed := task
			claimed.DispatchedAt.Time = task.DispatchedAt.Time.In(zone)
			resp, _, _, _, _, failure := testHandler.buildClaimedTaskResponse(req, &claimed, runtime, runtimeID, testWorkspaceID)
			if failure != nil {
				t.Fatalf("build claim: %+v", failure)
			}
			want := generation.UTC().Format(time.RFC3339Nano)
			if !resp.StartClaimSupported || resp.DispatchedAt == nil {
				t.Fatalf("missing claim timestamp or capability: supported=%t timestamp=%v", resp.StartClaimSupported, resp.DispatchedAt)
			}
			if *resp.DispatchedAt != want {
				t.Fatalf("claim timestamp = %q, want canonical UTC with microseconds %q", *resp.DispatchedAt, want)
			}
		})
	}
}

// A run of a platform rule that was captured under a configuration which has
// changed since does not start; one whose configuration still matches does.
func TestStartTaskRefusesRunCapturedUnderChangedWakeupConfig(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale=%t", stale), func(t *testing.T) {
			id, runtimeID, generation := startClaimFixture(t, "dispatched")
			var issueID, workspaceID pgtype.UUID
			dbfx.QueryRow(t, `SELECT issue_id,workspace_id FROM agent_task_queue t JOIN issue i ON i.id=t.issue_id WHERE t.id=$1`, id).Scan(&issueID, &workspaceID)
			w, err := testHandler.Queries.CreateSystemWakeup(context.Background(), db.CreateSystemWakeupParams{
				ID: dbid.NewV7(), WorkspaceID: workspaceID, IssueID: issueID, EventTypes: []string{}, Enabled: true,
				SystemRule: pgtype.Text{String: "pr_merged", Valid: true},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM issue_wakeup WHERE id=$1`, w.ID) })
			if stale {
				// The instance last ran under a configuration no stored definition produces any more.
				dbfx.Exec(t, `UPDATE issue_wakeup SET config_fingerprint='older-configuration' WHERE id=$1`, w.ID)
			}
			dbfx.Exec(t, `UPDATE agent_task_queue SET context=$2::jsonb WHERE id=$1`, id,
				fmt.Sprintf(`{"wakeup_id":%q,"wakeup_revision":%d,"wakeup_system":"pr_merged"}`, util.UUIDToString(w.ID), w.Revision))
			want := http.StatusOK
			if stale {
				want = http.StatusConflict
			}
			if stale {
				// A delivery that is not this claim neither starts nor fails the run.
				testutil.Call(t, testHandler.StartTask, startClaimRequest(id, runtimeID, generation.Add(-time.Microsecond))).Want(http.StatusConflict)
				testutil.Call(t, testHandler.StartTask, startClaimRequest(id, "00000000-0000-0000-0000-000000000001", generation)).Want(http.StatusConflict)
				var untouched string
				dbfx.QueryRow(t, `SELECT status FROM agent_task_queue WHERE id=$1`, id).Scan(&untouched)
				if untouched != "dispatched" {
					t.Fatalf("a stale delivery changed the claimed run to %q", untouched)
				}
			}
			testutil.Call(t, testHandler.StartTask, startClaimRequest(id, runtimeID, generation)).Want(want)
			var status string
			dbfx.QueryRow(t, `SELECT status FROM agent_task_queue WHERE id=$1`, id).Scan(&status)
			if stale && status != "failed" || !stale && status != "running" {
				t.Fatalf("task status after start = %q (stale=%t)", status, stale)
			}
		})
	}
}

// If recording the rejection itself fails, the caller gets a retryable server
// error, not a 409 that tells it the claim is settled while the run is still
// dispatched.
func TestStartTaskReportsAFailureWriteErrorAsRetryable(t *testing.T) {
	id, runtimeID, generation := startClaimFixture(t, "dispatched")
	var issueID, workspaceID pgtype.UUID
	dbfx.QueryRow(t, `SELECT issue_id,workspace_id FROM agent_task_queue t JOIN issue i ON i.id=t.issue_id WHERE t.id=$1`, id).Scan(&issueID, &workspaceID)
	w, err := testHandler.Queries.CreateSystemWakeup(context.Background(), db.CreateSystemWakeupParams{
		ID: dbid.NewV7(), WorkspaceID: workspaceID, IssueID: issueID, EventTypes: []string{}, Enabled: true,
		SystemRule: pgtype.Text{String: "pr_merged", Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM issue_wakeup WHERE id=$1`, w.ID) })
	dbfx.Exec(t, `UPDATE issue_wakeup SET config_fingerprint='older-configuration' WHERE id=$1`, w.ID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET context=$2::jsonb WHERE id=$1`, id,
		fmt.Sprintf(`{"wakeup_id":%q,"wakeup_revision":%d,"wakeup_system":"pr_merged"}`, util.UUIDToString(w.ID), w.Revision))
	// A test-owned trigger makes the failure write raise.
	fn := "che1140_refuse_fail_" + strings.ReplaceAll(id, "-", "")
	dbfx.Exec(t, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'refused'; END $$`, fn))
	dbfx.Exec(t, fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE ON agent_task_queue FOR EACH ROW WHEN (OLD.id='%s' AND NEW.status='failed') EXECUTE FUNCTION %s()`, fn, id, fn))
	t.Cleanup(func() {
		dbfx.Exec(t, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON agent_task_queue`, fn))
		dbfx.Exec(t, fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))
	})
	testutil.Call(t, testHandler.StartTask, startClaimRequest(id, runtimeID, generation)).Want(http.StatusServiceUnavailable)
	var status string
	dbfx.QueryRow(t, `SELECT status FROM agent_task_queue WHERE id=$1`, id).Scan(&status)
	if status != "dispatched" {
		t.Fatalf("task status after a failed rejection write = %q, want it left dispatched", status)
	}
}
