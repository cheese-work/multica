package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

func near(got, want float64) bool { return math.Abs(got-want) < 0.01 }

func TestParseCPUStatAndPercent(t *testing.T) {
	a, err := parseCPUStat(strings.NewReader("cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 1 1 1 1 1 1 1 1 1 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	// idle = idle+iowait = 800; total = sum of first 8 fields = 1000
	if a.idle != 800 || a.total != 1000 {
		t.Fatalf("parse = %+v, want idle 800 total 1000", a)
	}
	b, _ := parseCPUStat(strings.NewReader("cpu  150 0 150 750 150 0 0 0 0 0\n"))
	// delta total 200, delta idle 100 -> 50% busy
	if got := cpuPercent(a, b); !near(got, 50) {
		t.Fatalf("cpuPercent = %v, want 50", got)
	}
	if got := cpuPercent(b, b); got != 0 {
		t.Fatalf("cpuPercent with no delta = %v, want 0", got)
	}
	if _, err := parseCPUStat(strings.NewReader("garbage")); err == nil {
		t.Fatal("parseCPUStat(garbage) = nil error")
	}
}

func TestParseMemInfoPercent(t *testing.T) {
	got, err := parseMemInfoPercent(strings.NewReader("MemTotal:       1000 kB\nMemFree:         100 kB\nMemAvailable:    250 kB\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !near(got, 75) {
		t.Fatalf("ram percent = %v, want 75 (1 - available/total)", got)
	}
	if _, err := parseMemInfoPercent(strings.NewReader("MemTotal: 0 kB\nMemAvailable: 0 kB\n")); err == nil {
		t.Fatal("zero MemTotal = nil error")
	}
}

func TestParseMountsKeepsRealBlockDevicesOnly(t *testing.T) {
	in := `/dev/nvme0n1p2 / ext4 rw 0 0
proc /proc proc rw 0 0
sysfs /sys sysfs rw 0 0
tmpfs /run tmpfs rw 0 0
devtmpfs /dev devtmpfs rw 0 0
overlay /var/lib/docker/overlay2/x/merged overlay rw 0 0
/dev/nvme0n1p2 /var/lib/docker/volumes/v ext4 rw 0 0
/dev/loop3 /snap/core/1 squashfs ro 0 0
/dev/loop4 /mnt/img squashfs ro 0 0
/dev/sdb1 /mnt/ssd2 xfs rw 0 0
/dev/sdc1 /mnt/with\040space ext4 rw 0 0
/dev/nvme0n1p2 /bind ext4 rw 0 0
`
	got := parseMounts(strings.NewReader(in))
	var paths []string
	for _, m := range got {
		paths = append(paths, m.Path)
	}
	want := "/,/mnt/ssd2,/mnt/with space"
	if strings.Join(paths, ",") != want {
		t.Fatalf("mount paths = %q, want %q", strings.Join(paths, ","), want)
	}
}

func TestSelectDisksKeepsRootAndLargestUpToMax(t *testing.T) {
	var c []diskCandidate
	c = append(c, diskCandidate{protocol.DaemonHostDisk{MountPath: "/", Percent: 1}, 1}) // smallest, still kept
	for i := 0; i < 10; i++ {
		c = append(c, diskCandidate{protocol.DaemonHostDisk{MountPath: "/m" + string(rune('a'+i)), Percent: 2}, uint64(100 + i)})
	}
	got := selectDisks(c)
	if len(got) != protocol.MaxHostSampleDisks {
		t.Fatalf("len = %d, want %d", len(got), protocol.MaxHostSampleDisks)
	}
	if got[0].MountPath != "/" {
		t.Fatalf("first = %q, want /", got[0].MountPath)
	}
	for _, d := range got {
		if d.MountPath == "/ma" || d.MountPath == "/mb" || d.MountPath == "/mc" {
			t.Fatalf("smallest mount %q kept", d.MountPath)
		}
	}
}

func TestHostSamplerRunPublishesLatest(t *testing.T) {
	calls := 0
	s := newHostSampler(func() (*protocol.DaemonHostSample, error) {
		calls++
		return &protocol.DaemonHostSample{SampledAt: time.Unix(int64(calls), 0).UTC(), CPUPercent: float64(calls)}, nil
	})
	if s.Latest() != nil {
		t.Fatal("Latest before Run != nil")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx, time.Millisecond, 5*time.Millisecond, slog.Default()); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for s.Latest() == nil || s.Latest().CPUPercent < 2 {
		if time.Now().After(deadline) {
			t.Fatal("sampler never published two samples")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}

func TestHeartbeatRequestAttachesSampleOnHTTPAndWS(t *testing.T) {
	old := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Second)
	sample := &protocol.DaemonHostSample{SampledAt: old, CPUPercent: 12, RAMPercent: 34, Disks: []protocol.DaemonHostDisk{{MountPath: "/", Percent: 56}}}
	d := &Daemon{logger: slog.Default(), hostSampler: newHostSampler(nil)}
	d.hostSampler.last = sample // sampler stopped: stale sample must still ride the heartbeat unchanged

	var got protocol.DaemonHeartbeatRequestPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	}))
	defer srv.Close()
	if _, err := NewClient(srv.URL).SendHeartbeat(context.Background(), d.heartbeatRequest("rt-1")); err != nil {
		t.Fatal(err)
	}
	if got.HostSample == nil || !got.HostSample.SampledAt.Equal(old) || got.HostSample.CPUPercent != 12 || len(got.HostSample.Disks) != 1 {
		t.Fatalf("http host_sample = %+v, want %+v", got.HostSample, sample)
	}

	writes := make(chan *wsOutbound, 1)
	d.sendWSHeartbeats(context.Background(), []string{"rt-1"}, writes)
	var msg protocol.Message
	if err := json.Unmarshal((<-writes).data, &msg); err != nil {
		t.Fatal(err)
	}
	var ws protocol.DaemonHeartbeatRequestPayload
	if err := json.Unmarshal(msg.Payload, &ws); err != nil {
		t.Fatal(err)
	}
	if ws.HostSample == nil || !ws.HostSample.SampledAt.Equal(old) || ws.HostSample.RAMPercent != 34 {
		t.Fatalf("ws host_sample = %+v, want %+v", ws.HostSample, sample)
	}
}

func TestHeartbeatRequestWithoutSampler(t *testing.T) {
	d := &Daemon{logger: slog.Default()}
	if req := d.heartbeatRequest("rt-1"); req.HostSample != nil || req.RuntimeID != "rt-1" {
		t.Fatalf("req = %+v, want runtime only", req)
	}
}
