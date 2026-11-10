package daemon

import (
	"log/slog"
	"reflect"
	"sort"
	"testing"
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
