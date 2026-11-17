package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// Per-machine update schedule (CHE-5465). One schedule per (owner, daemon_id),
// the same key as machine pause.

type updateScheduleJSON struct {
	Enabled       bool     `json:"enabled"`
	Cron          string   `json:"cron"`
	Timezone      string   `json:"timezone"`
	WindowMinutes int      `json:"window_minutes"`
	NextRuns      []string `json:"next_runs"`
}

func putUpdateSchedule(t *testing.T, actorID, runtimeID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	testHandler.PutUpdateSchedule(w, withURLParam(newRequestAs(actorID, http.MethodPut, "/api/runtimes/"+runtimeID+"/update-schedule", body), "runtimeId", runtimeID))
	return w
}

func getUpdateSchedule(t *testing.T, actorID, runtimeID string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	testHandler.GetUpdateSchedule(w, withURLParam(newRequestAs(actorID, http.MethodGet, "/api/runtimes/"+runtimeID+"/update-schedule", nil), "runtimeId", runtimeID))
	return w
}

func deleteUpdateSchedule(t *testing.T, actorID, runtimeID string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	testHandler.DeleteUpdateSchedule(w, withURLParam(newRequestAs(actorID, http.MethodDelete, "/api/runtimes/"+runtimeID+"/update-schedule", nil), "runtimeId", runtimeID))
	return w
}

func cleanupUpdateSchedule(t *testing.T, daemonID string) {
	t.Helper()
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM daemon_update_schedule WHERE daemon_id = $1`, daemonID)
	})
}

func decodeSchedule(t *testing.T, w *httptest.ResponseRecorder) updateScheduleJSON {
	t.Helper()
	var got updateScheduleJSON
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode schedule %s: %v", w.Body.String(), err)
	}
	return got
}

func TestDaemonUpdateSchedule_SetGetClearRoundTrip(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	const daemonID = "update-schedule-roundtrip"
	cleanupUpdateSchedule(t, daemonID)
	runtimeA := dbfx.Runtime(t, "Schedule A", testutil.Cols{"daemon_id": daemonID})
	// A second runtime on the same machine reads the same schedule (D2).
	runtimeB := dbfx.Runtime(t, "Schedule B", testutil.Cols{"daemon_id": daemonID, "provider": "handler_test_runtime_b"})

	// Nothing saved: defaults, disabled.
	got := decodeSchedule(t, getUpdateSchedule(t, testUserID, runtimeA))
	if got.Enabled || got.Timezone != "Asia/Ho_Chi_Minh" || got.WindowMinutes != 30 || got.Cron != "" || len(got.NextRuns) != 0 {
		t.Fatalf("default schedule = %+v, want disabled, Asia/Ho_Chi_Minh, 30 min, no cron", got)
	}

	w := putUpdateSchedule(t, testUserID, runtimeA, map[string]any{"enabled": true, "cron": "0 2 * * *"})
	if w.Code != http.StatusOK {
		t.Fatalf("set = %d, want 200: %s", w.Code, w.Body.String())
	}
	set := decodeSchedule(t, w)
	if !set.Enabled || set.Cron != "0 2 * * *" || set.Timezone != "Asia/Ho_Chi_Minh" || set.WindowMinutes != 30 {
		t.Fatalf("set response = %+v, want cron 0 2 * * *, default timezone and window", set)
	}
	if len(set.NextRuns) != 3 {
		t.Fatalf("next_runs = %v, want 3", set.NextRuns)
	}

	// Read back through the sibling runtime: values equal.
	read := decodeSchedule(t, getUpdateSchedule(t, testUserID, runtimeB))
	if read.Enabled != set.Enabled || read.Cron != set.Cron || read.Timezone != set.Timezone || read.WindowMinutes != set.WindowMinutes {
		t.Fatalf("read back = %+v, want %+v", read, set)
	}

	// Replace with explicit values.
	w = putUpdateSchedule(t, testUserID, runtimeB, map[string]any{"enabled": false, "cron": "30 3 * * 1", "timezone": "UTC", "window_minutes": 10})
	if w.Code != http.StatusOK {
		t.Fatalf("replace = %d, want 200: %s", w.Code, w.Body.String())
	}
	read = decodeSchedule(t, getUpdateSchedule(t, testUserID, runtimeA))
	if read.Enabled || read.Cron != "30 3 * * 1" || read.Timezone != "UTC" || read.WindowMinutes != 10 {
		t.Fatalf("replaced schedule = %+v", read)
	}

	// The runtime list carries the schedule and a nullable last outcome.
	listed := listRuntimesForTest(t)[runtimeA]
	if listed.UpdateSchedule == nil || listed.UpdateSchedule.Cron != "30 3 * * 1" || listed.UpdateSchedule.WindowMinutes != 10 {
		t.Fatalf("listed schedule = %+v, want the saved one", listed.UpdateSchedule)
	}
	if listed.LastUpdateOutcome != nil {
		t.Fatalf("last_update_outcome = %v, want null before any cycle", *listed.LastUpdateOutcome)
	}

	if w := deleteUpdateSchedule(t, testUserID, runtimeA); w.Code != http.StatusNoContent {
		t.Fatalf("clear = %d, want 204: %s", w.Code, w.Body.String())
	}
	if w := deleteUpdateSchedule(t, testUserID, runtimeA); w.Code != http.StatusNoContent {
		t.Fatalf("second clear = %d, want 204 (idempotent)", w.Code)
	}
	if got := decodeSchedule(t, getUpdateSchedule(t, testUserID, runtimeA)); got.Enabled || got.Cron != "" {
		t.Fatalf("after clear = %+v, want defaults", got)
	}
	if listed := listRuntimesForTest(t)[runtimeA]; listed.UpdateSchedule != nil {
		t.Fatalf("listed schedule after clear = %+v, want nil", listed.UpdateSchedule)
	}
}

func TestDaemonUpdateSchedule_Validation(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	const daemonID = "update-schedule-validation"
	cleanupUpdateSchedule(t, daemonID)
	runtimeID := dbfx.Runtime(t, "Schedule validation", testutil.Cols{"daemon_id": daemonID})

	bad := []struct {
		name string
		body map[string]any
	}{
		{"cron with 6 fields", map[string]any{"enabled": true, "cron": "0 0 2 * * *"}},
		{"cron garbage", map[string]any{"enabled": true, "cron": "not a cron"}},
		{"cron descriptor", map[string]any{"enabled": true, "cron": "@daily"}},
		{"cron TZ prefix", map[string]any{"enabled": true, "cron": "TZ=UTC 0 2 * * *"}},
		{"cron missing", map[string]any{"enabled": true}},
		{"unknown time zone", map[string]any{"enabled": true, "cron": "0 2 * * *", "timezone": "Mars/Olympus"}},
		{"window 9", map[string]any{"enabled": true, "cron": "0 2 * * *", "window_minutes": 9}},
		{"window 31", map[string]any{"enabled": true, "cron": "0 2 * * *", "window_minutes": 31}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			w := putUpdateSchedule(t, testUserID, runtimeID, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s = %d, want 400: %s", tc.name, w.Code, w.Body.String())
			}
			var e struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || e.Error == "" {
				t.Fatalf("%s body = %s, want a clear error message", tc.name, w.Body.String())
			}
		})
	}

	for _, minutes := range []int{10, 30} {
		w := putUpdateSchedule(t, testUserID, runtimeID, map[string]any{"enabled": true, "cron": "0 2 * * *", "window_minutes": minutes})
		if w.Code != http.StatusOK {
			t.Fatalf("window %d = %d, want 200: %s", minutes, w.Code, w.Body.String())
		}
	}

	// A rejected write leaves the saved schedule alone.
	putUpdateSchedule(t, testUserID, runtimeID, map[string]any{"enabled": false, "cron": "0 4 * * *", "window_minutes": 15})
	putUpdateSchedule(t, testUserID, runtimeID, map[string]any{"enabled": true, "cron": "bad"})
	if got := decodeSchedule(t, getUpdateSchedule(t, testUserID, runtimeID)); got.Cron != "0 4 * * *" || got.WindowMinutes != 15 {
		t.Fatalf("schedule after rejected write = %+v, want the earlier one", got)
	}
}

func TestDaemonUpdateSchedule_DesktopLaunchedDaemonIsRefused(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	const daemonID = "update-schedule-desktop"
	cleanupUpdateSchedule(t, daemonID)
	runtimeID := dbfx.Runtime(t, "Schedule desktop", testutil.Cols{
		"daemon_id": daemonID,
		"metadata":  testutil.Raw(`'{"launched_by":"desktop"}'::jsonb`),
	})
	w := putUpdateSchedule(t, testUserID, runtimeID, map[string]any{"enabled": true, "cron": "0 2 * * *"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("desktop-launched set = %d, want 400: %s", w.Code, w.Body.String())
	}
}

func TestDaemonUpdateSchedule_RuntimeWithoutDaemonIsRefused(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	cloudRuntime := dbfx.Runtime(t, "Schedule cloud", nil)
	if w := putUpdateSchedule(t, testUserID, cloudRuntime, map[string]any{"enabled": true, "cron": "0 2 * * *"}); w.Code != http.StatusBadRequest {
		t.Fatalf("cloud runtime set = %d, want 400: %s", w.Code, w.Body.String())
	}
}

// Same permission as machine pause: the runtime owner and the workspace owner.
func TestDaemonUpdateSchedule_NonOwnerNonAdminIsRefused(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	const daemonID = "update-schedule-authz"
	cleanupUpdateSchedule(t, daemonID)
	runtimeID, runtimeOwnerID, plainMemberID := runtimeVisibilityFixture(t)
	dbfx.Exec(t, `UPDATE agent_runtime SET daemon_id = $2, visibility = 'public' WHERE id = $1`, runtimeID, daemonID)
	var adminID string
	if err := testPool.QueryRow(context.Background(), `INSERT INTO "user" (name, email) VALUES ('Schedule Admin', 'schedule-admin@multica.test') RETURNING id`).Scan(&adminID); err != nil {
		t.Fatalf("create admin user: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM "user" WHERE email = 'schedule-admin@multica.test'`)
	})
	dbfx.Exec(t, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'admin')`, testWorkspaceID, adminID)

	body := map[string]any{"enabled": true, "cron": "0 2 * * *"}
	for name, actor := range map[string]string{"plain member": plainMemberID, "admin who is not owner": adminID} {
		if w := putUpdateSchedule(t, actor, runtimeID, body); w.Code != http.StatusForbidden {
			t.Fatalf("%s set = %d, want 403: %s", name, w.Code, w.Body.String())
		}
		if w := getUpdateSchedule(t, actor, runtimeID); w.Code != http.StatusForbidden {
			t.Fatalf("%s get = %d, want 403: %s", name, w.Code, w.Body.String())
		}
		if w := deleteUpdateSchedule(t, actor, runtimeID); w.Code != http.StatusForbidden {
			t.Fatalf("%s clear = %d, want 403: %s", name, w.Code, w.Body.String())
		}
	}
	if w := putUpdateSchedule(t, runtimeOwnerID, runtimeID, body); w.Code != http.StatusOK {
		t.Fatalf("runtime owner set = %d, want 200: %s", w.Code, w.Body.String())
	}
	if w := deleteUpdateSchedule(t, testUserID, runtimeID); w.Code != http.StatusNoContent {
		t.Fatalf("workspace owner clear = %d, want 204: %s", w.Code, w.Body.String())
	}
}
