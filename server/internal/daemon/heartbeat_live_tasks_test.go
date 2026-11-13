package daemon

import (
	"encoding/json"
	"log/slog"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// CHE-3114 part 2: the heartbeat names every task this daemon still holds, so
// the server can tell a cancelled run it no longer holds from one still running.
func TestHeartbeatRequestReportsLiveTaskIDs(t *testing.T) {
	d := &Daemon{logger: slog.Default()}

	req := d.heartbeatRequest("rt-1")
	if req.LiveTaskIDs == nil || len(*req.LiveTaskIDs) != 0 {
		t.Fatalf("idle daemon live_task_ids = %v, want empty non-nil", req.LiveTaskIDs)
	}

	a := d.inflight.begin("task-a", "")
	b := d.inflight.begin("task-b", "")
	got := *d.heartbeatRequest("rt-1").LiveTaskIDs
	sort.Strings(got)
	if want := []string{"task-a", "task-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("live_task_ids = %v, want %v", got, want)
	}

	d.inflight.end("task-a", a)
	d.inflight.end("task-b", b)
	if got := *d.heartbeatRequest("rt-1").LiveTaskIDs; len(got) != 0 {
		t.Fatalf("after both ended live_task_ids = %v, want empty", got)
	}
}

// A daemon that left processes behind cannot vouch that a missing task stopped.
func TestHeartbeatRequestWithholdsLiveTaskIDsAfterUnconfirmedCleanup(t *testing.T) {
	d := &Daemon{logger: slog.Default()}
	d.cleanupUnconfirmed.Store(true)
	if req := d.heartbeatRequest("rt-1"); req.LiveTaskIDs != nil {
		t.Fatalf("live_task_ids = %v, want nil after unconfirmed cleanup", *req.LiveTaskIDs)
	}
}

// The server tells a restarted daemon from a long-lived one by how long this
// process has been up; it must ride on the heartbeat next to the live set.
func TestHeartbeatRequestReportsDaemonUptime(t *testing.T) {
	d := &Daemon{logger: slog.Default(), startedAt: time.Now().Add(-90 * time.Second)}
	raw, err := json.Marshal(d.heartbeatRequest("rt-1"))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		UptimeMs *int64 `json:"daemon_uptime_ms"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.UptimeMs == nil || *got.UptimeMs < 90_000 || *got.UptimeMs > 120_000 {
		t.Fatalf("daemon_uptime_ms = %v, want about 90000", got.UptimeMs)
	}
}

// Cleanup turns uncertain between the heartbeat reading the flag and taking the
// live-set snapshot: the task sets the flag, then leaves the set. The snapshot
// must not be reported as proof that the task stopped (found in review of the
// first candidate; the interleaving is forced by holding the snapshot lock).
func TestHeartbeatRequestWithholdsLiveTaskIDsWhenCleanupTurnsUncertainDuringSnapshot(t *testing.T) {
	d := &Daemon{logger: slog.Default()}
	const id = "01a186db-50e4-7b7b-866f-5735ce467b3e"
	claim := d.inflight.begin(id, "")
	d.inflight.mu.Lock()
	locked := true
	defer func() {
		if locked {
			d.inflight.mu.Unlock()
		}
	}()
	result := make(chan *[]string, 1)
	go func() { result <- d.heartbeatRequest("rt-1").LiveTaskIDs }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		if strings.Contains(string(buf[:n]), "(*inflightClaims).ids(") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("heartbeat did not reach the snapshot lock")
		}
		runtime.Gosched()
	}
	// executeAndDrain stores the flag before the batch poller's deferred end
	// removes the task from the set.
	d.cleanupUnconfirmed.Store(true)
	delete(d.inflight.entries, id)
	_ = claim
	d.inflight.mu.Unlock()
	locked = false
	if live := <-result; live != nil {
		t.Fatalf("live_task_ids = %v, want nil: cleanup was unconfirmed when the task left the set", *live)
	}
}
