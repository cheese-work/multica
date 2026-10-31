package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemonws"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-2773 L2: the heartbeat carries one host sample per daemon; the server
// stores it once per (daemon_id, sampled_at, mount_path) however many runtimes
// or transports repeat it.

func testHostSample(at time.Time) *protocol.DaemonHostSample {
	return &protocol.DaemonHostSample{
		SampledAt:  at.UTC().Truncate(time.Minute),
		CPUPercent: 12.5,
		RAMPercent: 40,
		Disks:      []protocol.DaemonHostDisk{{MountPath: "/", Percent: 61}, {MountPath: "/mnt/data", Percent: 7}},
	}
}

func hostSampleRows(t *testing.T, daemonID string) int {
	t.Helper()
	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM host_sample WHERE daemon_id = $1`, daemonID).Scan(&n)
	return n
}

func cleanHostSamples(t *testing.T, daemonID string) {
	t.Helper()
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM host_sample WHERE daemon_id = $1`, daemonID)
	})
}

func httpHeartbeat(t *testing.T, runtimeID string, s *protocol.DaemonHostSample) {
	t.Helper()
	body := map[string]any{"runtime_id": runtimeID}
	if s != nil {
		body["host_sample"] = s
	}
	testutil.Decode[heartbeatJSON](t, testHandler.DaemonHeartbeat, newOwnerDaemonRequest(http.MethodPost, "/api/daemon/heartbeat", body), http.StatusOK)
}

func wsHeartbeat(t *testing.T, daemonID, runtimeID string, s *protocol.DaemonHostSample) {
	t.Helper()
	_, err := testHandler.HandleDaemonWSHeartbeat(context.Background(),
		daemonws.ClientIdentity{
			DaemonID:     daemonID,
			WorkspaceIDs: []string{testWorkspaceID},
			RuntimeLeases: map[string]*daemonws.RuntimeLease{
				runtimeID: daemonws.NewRuntimeLease(testWorkspaceID, "online", time.Now(), true),
			},
		},
		protocol.DaemonHeartbeatRequestPayload{RuntimeID: runtimeID, HostSample: s})
	if err != nil {
		t.Fatalf("HandleDaemonWSHeartbeat: %v", err)
	}
}

func seedTwoRuntimes(t *testing.T, daemonID string) (a, b string) {
	t.Helper()
	a = dbfx.Runtime(t, "Host sample A", testutil.Cols{"daemon_id": daemonID, "provider": "claude"})
	b = dbfx.Runtime(t, "Host sample B", testutil.Cols{"daemon_id": daemonID, "provider": "codex"})
	return a, b
}

func TestHostSample_TwoRuntimesOneDaemonStoreOneRowSet(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	const daemonID = "host-sample-two-runtimes"
	cleanHostSamples(t, daemonID)
	a, b := seedTwoRuntimes(t, daemonID)
	s := testHostSample(time.Now())

	httpHeartbeat(t, a, s)
	httpHeartbeat(t, b, s)
	wsHeartbeat(t, daemonID, a, s)
	wsHeartbeat(t, daemonID, b, s)

	// 1 CPU/RAM row + 2 disk rows, not 4x that.
	if got := hostSampleRows(t, daemonID); got != 3 {
		t.Fatalf("rows after the same sample from 2 runtimes x 2 transports = %d, want 3", got)
	}
	var cpu, ram float32
	dbfx.QueryRow(t, `SELECT cpu_percent, ram_percent FROM host_sample WHERE daemon_id = $1 AND mount_path = ''`, daemonID).Scan(&cpu, &ram)
	if cpu != 12.5 || ram != 40 {
		t.Fatalf("cpu/ram = %v/%v, want 12.5/40", cpu, ram)
	}
	var disk float32
	dbfx.QueryRow(t, `SELECT disk_percent FROM host_sample WHERE daemon_id = $1 AND mount_path = '/mnt/data'`, daemonID).Scan(&disk)
	if disk != 7 {
		t.Fatalf("disk /mnt/data = %v, want 7", disk)
	}

	// A new sample adds a new row set.
	httpHeartbeat(t, a, testHostSample(time.Now().Add(time.Minute)))
	if got := hostSampleRows(t, daemonID); got != 6 {
		t.Fatalf("rows after a second sample = %d, want 6", got)
	}
}

func TestHostSample_WSUsesAuthenticatedDaemonID(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	const daemonID = "host-sample-ws-only"
	cleanHostSamples(t, daemonID)
	a, _ := seedTwoRuntimes(t, daemonID)

	wsHeartbeat(t, daemonID, a, testHostSample(time.Now()))
	if got := hostSampleRows(t, daemonID); got != 3 {
		t.Fatalf("ws rows = %d, want 3", got)
	}
}

func TestHostSample_StaleAfterThreeMinutesAndRecovers(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	const daemonID = "host-sample-stale"
	cleanHostSamples(t, daemonID)
	a, _ := seedTwoRuntimes(t, daemonID)
	ctx := context.Background()

	latest := func() time.Time {
		at, err := testHandler.Queries.GetLatestHostSampleAt(ctx, daemonID)
		if err != nil {
			t.Fatalf("GetLatestHostSampleAt: %v", err)
		}
		return at.Time
	}

	// Sampler stopped 4 min ago; heartbeats keep arriving without a fresh sample.
	httpHeartbeat(t, a, testHostSample(time.Now().Add(-4*time.Minute)))
	httpHeartbeat(t, a, nil)
	httpHeartbeat(t, a, testHostSample(time.Now().Add(-4*time.Minute))) // daemon re-attaches the same stale sample
	if !protocol.HostSampleStale(latest(), time.Now()) {
		t.Fatalf("latest sample %v not stale 4 min after, heartbeat healthy", latest())
	}
	var status string
	dbfx.QueryRow(t, `SELECT status FROM agent_runtime WHERE id = $1`, a).Scan(&status)
	if status != "online" {
		t.Fatalf("runtime status = %q, want online (heartbeat healthy)", status)
	}

	// Sampler resumes.
	httpHeartbeat(t, a, testHostSample(time.Now()))
	if protocol.HostSampleStale(latest(), time.Now()) {
		t.Fatalf("latest sample %v still stale after the sampler resumed", latest())
	}
}

func TestHostSample_RejectsBadSamples(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	const daemonID = "host-sample-bad"
	cleanHostSamples(t, daemonID)
	a, _ := seedTwoRuntimes(t, daemonID)
	now := time.Now()

	future := testHostSample(now.Add(time.Hour))
	hot := testHostSample(now)
	hot.CPUPercent = 101
	neg := testHostSample(now.Add(-time.Second))
	neg.Disks[0].Percent = -1
	many := testHostSample(now.Add(-2 * time.Second))
	for i := 0; i < protocol.MaxHostSampleDisks; i++ {
		many.Disks = append(many.Disks, protocol.DaemonHostDisk{MountPath: "/m" + string(rune('a'+i)), Percent: 1})
	}
	emptyMount := testHostSample(now.Add(-3 * time.Second))
	emptyMount.Disks[1].MountPath = ""
	zero := &protocol.DaemonHostSample{CPUPercent: 1, RAMPercent: 1}

	for name, s := range map[string]*protocol.DaemonHostSample{"future": future, "cpu>100": hot, "disk<0": neg, "too many disks": many, "empty mount": emptyMount, "zero time": zero} {
		httpHeartbeat(t, a, s) // the heartbeat itself must still succeed
		if got := hostSampleRows(t, daemonID); got != 0 {
			t.Fatalf("%s sample stored %d rows, want 0", name, got)
		}
	}
}

func TestHostSample_SameMinuteSharesOneRowSet(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	const daemonID = "host-sample-same-minute"
	cleanHostSamples(t, daemonID)
	a, _ := seedTwoRuntimes(t, daemonID)
	base := time.Now().UTC().Truncate(time.Minute).Add(-2 * time.Minute)

	// A sampler restart inside one minute produces a second timestamp in that minute.
	first, second := testHostSample(base), testHostSample(base)
	first.SampledAt = base.Add(5 * time.Second)
	second.SampledAt = base.Add(40 * time.Second)
	httpHeartbeat(t, a, first)
	httpHeartbeat(t, a, second)
	if got := hostSampleRows(t, daemonID); got != 3 {
		t.Fatalf("rows for two samples in one minute = %d, want 3 (one minute key)", got)
	}
}

// A locked host_sample table must not hold the heartbeat (and, on WS, the read
// pump) past the ingest budget; the sample is dropped and the heartbeat acks.
func TestHostSample_BlockedStorageDoesNotStallHeartbeat(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	const daemonID = "host-sample-blocked"
	cleanHostSamples(t, daemonID)
	a, _ := seedTwoRuntimes(t, daemonID)
	ctx := context.Background()

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			tx.Rollback(ctx)
		}
	}
	defer release()
	if _, err := tx.Exec(ctx, `LOCK TABLE host_sample IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	// Safety net so a regression shows as a slow heartbeat, not a hung suite.
	timer := time.AfterFunc(hostSampleIngestTimeout+3*time.Second, release)
	defer timer.Stop()

	start := time.Now()
	httpHeartbeat(t, a, testHostSample(time.Now()))
	wsHeartbeat(t, daemonID, a, testHostSample(time.Now().Add(-time.Minute)))
	if took := time.Since(start); took > 2*hostSampleIngestTimeout+time.Second {
		t.Fatalf("two heartbeats took %v with host_sample locked, want under %v", took, 2*hostSampleIngestTimeout+time.Second)
	}
	release()
	if got := hostSampleRows(t, daemonID); got != 0 {
		t.Fatalf("rows = %d, want 0 (timed-out samples dropped)", got)
	}
}
