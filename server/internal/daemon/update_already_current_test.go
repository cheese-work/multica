package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/multica-ai/multica/server/internal/cli"
)

// A manual Update that finds the daemon already on the server's build is a
// success, not a failure, and must not restart a healthy daemon.
func TestHandleUpdateAlreadyOnServerBuildCompletesWithoutRestart(t *testing.T) {
	var payloads []map[string]any
	d, _ := updateReportDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		payloads = append(payloads, p)
		w.WriteHeader(http.StatusOK)
	})
	var restarts atomic.Int32
	d.cancelFunc = func() { restarts.Add(1) }
	d.runUpdateFn = func(string) (string, error) {
		return "", fmt.Errorf("fork update unavailable, installed daemon unchanged: %w", cli.ErrForkUpToDate)
	}

	d.handleUpdate(context.Background(), "runtime-1", &PendingUpdate{ID: "update-1", TargetVersion: "0.7.1"})

	last := payloads[len(payloads)-1]
	if last["status"] != "completed" {
		t.Fatalf("final report = %v, want status completed", last)
	}
	if restarts.Load() != 0 || d.RestartBinary() != "" {
		t.Fatalf("daemon restarted although nothing was installed: restarts=%d", restarts.Load())
	}
	if d.updating.Load() || d.pauseClaims {
		t.Fatalf("update ownership not released: updating=%v pause=%v", d.updating.Load(), d.pauseClaims)
	}
}

// The manual path must hand the fork source the build its own server carries,
// whatever target the page sent.
func TestRunUpdateUpToDateIsReportedAsErrForkUpToDate(t *testing.T) {
	if !cli.ForkUpdateSupported(runtime.GOOS) {
		t.Skip("fork source is Linux/macOS only")
	}
	d, _ := newAutoUpdateTestDaemon(t, "v0.1.13")
	stubFork(t, nil, cli.ErrForkUpToDate)

	_, err := d.runUpdate("0.7.1")

	if !errors.Is(err, cli.ErrForkUpToDate) {
		t.Fatalf("err = %v, want ErrForkUpToDate in the chain", err)
	}
}
