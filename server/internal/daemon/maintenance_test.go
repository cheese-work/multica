package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
)

func newMaintenanceTestDaemon() (*Daemon, *atomic.Int32) {
	var cancels atomic.Int32
	return &Daemon{
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		cancelFunc: func() { cancels.Add(1) },
	}, &cancels
}

func mustAcquire(t *testing.T, d *Daemon) string {
	t.Helper()
	tok, _, err := d.acquireMaintenance(context.Background(), time.Minute)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	return tok
}

// A poller that already decided to claim must be waited out, and the pollers
// that arrive during that wait must be refused — the admission race.
func TestAcquireMaintenance_WaitsForInFlightClaimAndBlocksNewOnes(t *testing.T) {
	d, _ := newMaintenanceTestDaemon()
	if !d.tryEnterClaim() {
		t.Fatal("setup: claim refused")
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := d.acquireMaintenance(context.Background(), time.Minute)
		done <- err
	}()

	deadline := time.Now().Add(2 * time.Second)
	for !claimsPaused(t, d) {
		if time.Now().After(deadline) {
			t.Fatal("barrier never raised while waiting on the in-flight claim")
		}
		time.Sleep(time.Millisecond)
	}
	if d.tryEnterClaim() {
		t.Fatal("a new claim slipped in during admission")
	}
	select {
	case err := <-done:
		t.Fatalf("acquire returned %v before the in-flight claim finished", err)
	case <-time.After(50 * time.Millisecond):
	}

	d.exitClaim()
	if err := <-done; err != nil {
		t.Fatalf("acquire after claim finished: %v", err)
	}
	if d.tryEnterClaim() {
		t.Fatal("claims allowed while window held")
	}
}

// The in-flight claim handed a task off: activeTasks rose before exitClaim, so
// acquisition must fail busy and leave no residue.
func TestAcquireMaintenance_BusyWhenClaimBecameATask(t *testing.T) {
	d, _ := newMaintenanceTestDaemon()
	d.tryEnterClaim()
	done := make(chan error, 1)
	go func() {
		_, _, err := d.acquireMaintenance(context.Background(), time.Minute)
		done <- err
	}()
	for !claimsPaused(t, d) {
		time.Sleep(time.Millisecond)
	}
	d.activeTasks.Add(1)
	d.exitClaim()

	if err := <-done; !errors.Is(err, errMaintenanceBusy) {
		t.Fatalf("err = %v, want busy", err)
	}
	assertNoMaintenanceResidue(t, d)
}

func TestAcquireMaintenance_BusyWithActiveTaskAndOnContextTimeout(t *testing.T) {
	d, _ := newMaintenanceTestDaemon()
	d.activeTasks.Store(1)
	if _, _, err := d.acquireMaintenance(context.Background(), time.Minute); !errors.Is(err, errMaintenanceBusy) {
		t.Fatalf("active task: err = %v, want busy", err)
	}
	assertNoMaintenanceResidue(t, d)

	d.activeTasks.Store(0)
	d.tryEnterClaim() // a claim that never finishes
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, _, err := d.acquireMaintenance(ctx, time.Minute); !errors.Is(err, errMaintenanceBusy) {
		t.Fatalf("stuck claim: err = %v, want busy", err)
	}
	d.exitClaim()
	assertNoMaintenanceResidue(t, d)
}

func assertNoMaintenanceResidue(t *testing.T, d *Daemon) {
	t.Helper()
	if claimsPaused(t, d) {
		t.Error("failed acquisition left claims paused")
	}
	if d.updating.Load() {
		t.Error("failed acquisition left update ownership held")
	}
	if held, _ := d.maintenanceStatus(); held {
		t.Error("failed acquisition left a lease")
	}
}

func TestMaintenance_ContentionTokenAndRelease(t *testing.T) {
	d, _ := newMaintenanceTestDaemon()
	tok := mustAcquire(t, d)

	if _, _, err := d.acquireMaintenance(context.Background(), time.Minute); !errors.Is(err, errMaintenanceUpdateRunning) {
		t.Fatalf("second acquire: %v, want update_running", err)
	}
	if err := d.releaseMaintenance("wrong"); !errors.Is(err, errMaintenanceTokenMismatch) {
		t.Fatalf("wrong token release: %v", err)
	}
	if held, _ := d.maintenanceStatus(); !held || !claimsPaused(t, d) {
		t.Fatal("a failed contender disturbed the owner's window")
	}
	if err := d.releaseMaintenance(tok); err != nil {
		t.Fatalf("release: %v", err)
	}
	assertNoMaintenanceResidue(t, d)
	if err := d.releaseMaintenance(tok); !errors.Is(err, errMaintenanceNotHeld) {
		t.Fatalf("double release: %v", err)
	}
	if !d.tryEnterClaim() {
		t.Fatal("claims did not resume after release")
	}
}

func TestAcquireMaintenance_RefusedWhenRestartScheduled(t *testing.T) {
	d, _ := newMaintenanceTestDaemon()
	d.restartBinary = "/bin/multica"
	if _, _, err := d.acquireMaintenance(context.Background(), time.Minute); !errors.Is(err, errMaintenanceRestarting) {
		t.Fatalf("err = %v, want restart_pending", err)
	}
	assertNoMaintenanceResidue(t, d)
}

// Automatic reload / auto-update / server update must not run, and must not
// steal or clear the window.
func TestMaintenance_BlocksAutomaticAndServerUpdates(t *testing.T) {
	d, restarts := newSelfReloadTestDaemon(t, "0.3.7")
	d.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	probes := stubSelfVersion(t, "0.3.8", nil)
	tok := mustAcquire(t, d)

	d.trySelfReload(context.Background())
	if probes.Load() != 0 || restarts.Load() != 0 {
		t.Fatalf("auto-reload ran under the window: probes=%d restarts=%d", probes.Load(), restarts.Load())
	}

	d.cfg.AutoUpdateEnabled = true
	withStubRelease(t, &cli.GitHubRelease{TagName: "v9.9.9"}, nil)
	d.runUpdateFn = func(string) (string, error) {
		t.Error("update executed under the window")
		return "", nil
	}
	d.tryAutoUpdate(context.Background())
	if restarts.Load() != 0 {
		t.Fatal("auto-update restarted under the window")
	}

	if got := d.tryBeginServerUpdate(context.Background()); got != serverUpdateAlreadyRunning {
		t.Fatalf("server update acquire = %v, want already-running", got)
	}
	if d.trySetClaimBarrier() {
		t.Fatal("runtime demotion barrier acquired under the window")
	}
	if held, _ := d.maintenanceStatus(); !held || !claimsPaused(t, d) || !d.updating.Load() {
		t.Fatal("window state was disturbed")
	}
	if err := d.releaseMaintenance(tok); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenance_ShutdownGate(t *testing.T) {
	d, cancels := newMaintenanceTestDaemon()
	shutdown := func(token string) int {
		req := httptest.NewRequest(http.MethodPost, "/shutdown", nil)
		if token != "" {
			req.Header.Set(maintenanceTokenHeader, token)
		}
		rec := httptest.NewRecorder()
		d.shutdownHandler().ServeHTTP(rec, req)
		time.Sleep(20 * time.Millisecond) // handler cancels asynchronously
		return rec.Code
	}

	// No window: a token is a stale-ownership claim and is refused; no token
	// keeps the historical behavior.
	if code := shutdown("stale-token"); code != http.StatusConflict || cancels.Load() != 0 {
		t.Fatalf("token without window: code=%d cancels=%d", code, cancels.Load())
	}

	tok := mustAcquire(t, d)
	if code := shutdown(""); code != http.StatusForbidden || cancels.Load() != 0 {
		t.Fatalf("no token under window: code=%d cancels=%d", code, cancels.Load())
	}
	if code := shutdown("wrong"); code != http.StatusForbidden || cancels.Load() != 0 {
		t.Fatalf("wrong token: code=%d cancels=%d", code, cancels.Load())
	}
	if code := shutdown(tok); code != http.StatusOK || cancels.Load() != 1 {
		t.Fatalf("owner token: code=%d cancels=%d", code, cancels.Load())
	}
	// Frozen: claims stay paused until exit, release is refused.
	if err := d.releaseMaintenance(tok); !errors.Is(err, errMaintenanceClosing) {
		t.Fatalf("release after accepted shutdown: %v", err)
	}
	if d.tryEnterClaim() {
		t.Fatal("claims resumed during shutdown")
	}
}

// Lease expiry (owner vanished) resumes claiming, and the owner's later
// shutdown attempt with the dead token is refused so it can reconcile.
func TestMaintenance_LeaseExpiryReleasesAndInvalidatesToken(t *testing.T) {
	d, cancels := newMaintenanceTestDaemon()
	tok, _, err := d.acquireMaintenance(context.Background(), 40*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if held, _ := d.maintenanceStatus(); !held {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lease never expired")
		}
		time.Sleep(5 * time.Millisecond)
	}
	assertNoMaintenanceResidue(t, d)
	if !d.tryEnterClaim() {
		t.Fatal("claims did not resume after expiry")
	}
	if err := d.authorizeShutdown(tok); !errors.Is(err, errMaintenanceNotHeld) || cancels.Load() != 0 {
		t.Fatalf("shutdown with expired token: %v", err)
	}
}

// Full HTTP surface on the real mux: acquire, health visibility without
// leaking the token, contention, release.
func TestMaintenance_HTTPLifecycle(t *testing.T) {
	d, _ := newMaintenanceTestDaemon()
	srv := httptest.NewServer(d.healthMux(time.Now()))
	defer srv.Close()

	post := func(path, body string) (int, map[string]string) {
		resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	code, acq := post("/maintenance/acquire", `{"ttl_seconds":60}`)
	if code != http.StatusOK || acq["token"] == "" {
		t.Fatalf("acquire: %d %v", code, acq)
	}
	if code, out := post("/maintenance/acquire", ``); code != http.StatusConflict || out["error"] != "update_running" {
		t.Fatalf("contender: %d %v", code, out)
	}

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), `"maintenance_held":true`) || strings.Contains(string(raw), acq["token"]) {
		t.Fatalf("health: %s", raw)
	}

	if code, _ := post("/maintenance/release", `{"token":"nope"}`); code != http.StatusForbidden {
		t.Fatalf("wrong token release: %d", code)
	}
	if code, _ := post("/maintenance/release", `{"token":"`+acq["token"]+`"}`); code != http.StatusOK {
		t.Fatalf("release: %d", code)
	}
	if code, out := post("/maintenance/release", `{"token":"`+acq["token"]+`"}`); code != http.StatusConflict || out["error"] != "not_held" {
		t.Fatalf("second release: %d %v", code, out)
	}
	if code, _ := post("/maintenance/acquire", `{"ttl_seconds":-1}`); code != http.StatusBadRequest {
		t.Fatalf("negative ttl: %d", code)
	}
}
