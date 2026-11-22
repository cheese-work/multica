package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestReportTaskUsageStoresCacheWrite1h(t *testing.T) {
	issueID := dbfx.Issue(t, "1h cache write usage")
	agentID := dbfx.Agent(t, "1h cache write agent", testRuntimeID)
	newTask := func() string {
		return dbfx.Task(t, agentID, testutil.Cols{"issue_id": issueID, "runtime_id": testRuntimeID, "status": "running"})
	}
	report := func(taskID string, entry map[string]any) int {
		t.Helper()
		req := withURLParam(newOwnerDaemonRequest(http.MethodPost, "/api/daemon/tasks/"+taskID+"/usage",
			map[string]any{"usage": []map[string]any{entry}}), "taskId", taskID)
		return testutil.Call(t, testHandler.ReportTaskUsage, req).Code
	}
	stored := func(taskID string) pgtype.Int8 {
		t.Helper()
		var v pgtype.Int8
		if err := testPool.QueryRow(context.Background(),
			`SELECT cache_write_1h_tokens FROM task_usage WHERE task_id = $1`, taskID).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	split := newTask()
	report(split, map[string]any{"provider": "anthropic", "model": "claude-sonnet-4-6", "cache_write_tokens": 300, "cache_write_1h_tokens": 120})
	if got := stored(split); !got.Valid || got.Int64 != 120 {
		t.Fatalf("stored 1h = %+v, want 120", got)
	}
	// A corrected report replaces the figure; one without a split resets it to NULL.
	report(split, map[string]any{"provider": "anthropic", "model": "claude-sonnet-4-6", "cache_write_tokens": 300})
	if got := stored(split); got.Valid {
		t.Fatalf("stored 1h after report without split = %+v, want NULL", got)
	}

	plain := newTask()
	report(plain, map[string]any{"provider": "anthropic", "model": "claude-sonnet-4-6", "cache_write_tokens": 50})
	if got := stored(plain); got.Valid {
		t.Fatalf("stored 1h with no split = %+v, want NULL", got)
	}

	zero := newTask()
	report(zero, map[string]any{"provider": "anthropic", "model": "claude-sonnet-4-6", "cache_write_tokens": 50, "cache_write_1h_tokens": 0})
	if got := stored(zero); !got.Valid || got.Int64 != 0 {
		t.Fatalf("stored 1h for a real zero = %+v, want 0", got)
	}

	bad := newTask()
	if code := report(bad, map[string]any{"provider": "anthropic", "model": "m", "cache_write_tokens": 50, "cache_write_1h_tokens": -1}); code != http.StatusBadRequest {
		t.Fatalf("negative 1h status = %d, want 400", code)
	}
	if code := report(bad, map[string]any{"provider": "anthropic", "model": "m", "cache_write_tokens": 50, "cache_write_1h_tokens": 51}); code != http.StatusBadRequest {
		t.Fatalf("1h above cache_write status = %d, want 400", code)
	}
}
