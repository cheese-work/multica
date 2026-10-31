package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

// hostSampleWarmup is the gap between the priming /proc/stat read and the first
// sample, so the first CPU percent is a real delta.
const hostSampleWarmup = time.Second

type cpuTimes struct{ idle, total uint64 }

type mountEntry struct{ Device, Path, FSType string }

type diskCandidate struct {
	protocol.DaemonHostDisk
	Total uint64
}

// parseCPUStat reads the aggregate "cpu" line of /proc/stat. idle includes
// iowait; total is the first eight fields (user..steal), guest time is already
// inside user/nice.
func parseCPUStat(r io.Reader) (cpuTimes, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		var t cpuTimes
		for i, v := range f[1:] {
			if i >= 8 {
				break
			}
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return cpuTimes{}, fmt.Errorf("parse /proc/stat: %w", err)
			}
			t.total += n
			if i == 3 || i == 4 {
				t.idle += n
			}
		}
		return t, nil
	}
	return cpuTimes{}, errors.New("parse /proc/stat: no cpu line")
}

func cpuPercent(prev, cur cpuTimes) float64 {
	if cur.total <= prev.total || cur.idle < prev.idle {
		return 0
	}
	busy := float64(cur.total-prev.total) - float64(cur.idle-prev.idle)
	return clampPercent(100 * busy / float64(cur.total-prev.total))
}

func clampPercent(p float64) float64 {
	switch {
	case p < 0:
		return 0
	case p > 100:
		return 100
	}
	return p
}

// parseMemInfoPercent returns used RAM as 100 * (1 - MemAvailable/MemTotal).
func parseMemInfoPercent(r io.Reader) (float64, error) {
	var total, avail float64
	var haveAvail bool
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			total, _ = strconv.ParseFloat(f[1], 64)
		case "MemAvailable:":
			avail, _ = strconv.ParseFloat(f[1], 64)
			haveAvail = true
		}
	}
	if total <= 0 || !haveAvail {
		return 0, errors.New("parse /proc/meminfo: no MemTotal/MemAvailable")
	}
	return clampPercent(100 * (1 - avail/total)), nil
}

// nonDiskFS are filesystems that never describe a real disk, even on a /dev node.
var nonDiskFS = map[string]bool{"squashfs": true, "tmpfs": true, "devtmpfs": true, "overlay": true, "proc": true, "sysfs": true, "iso9660": true}

// parseMounts returns real block-device mounts from /proc/self/mounts: a /dev/
// source, no pseudo/overlay/squashfs filesystem, not under /var/lib/docker or
// /snap. A device mounted twice (bind mount) counts once, at its first path.
func parseMounts(r io.Reader) []mountEntry {
	var out []mountEntry
	seen := map[string]bool{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 || !strings.HasPrefix(f[0], "/dev/") || nonDiskFS[f[2]] || seen[f[0]] {
			continue
		}
		path := strings.ReplaceAll(f[1], `\040`, " ")
		if strings.HasPrefix(path, "/var/lib/docker") || strings.HasPrefix(path, "/snap") {
			continue
		}
		seen[f[0]] = true
		out = append(out, mountEntry{Device: f[0], Path: path, FSType: f[2]})
	}
	return out
}

// selectDisks keeps "/" first, then the largest mounts by total size, up to
// protocol.MaxHostSampleDisks.
func selectDisks(c []diskCandidate) []protocol.DaemonHostDisk {
	sort.SliceStable(c, func(i, j int) bool {
		if (c[i].MountPath == "/") != (c[j].MountPath == "/") {
			return c[i].MountPath == "/"
		}
		return c[i].Total > c[j].Total
	})
	if len(c) > protocol.MaxHostSampleDisks {
		c = c[:protocol.MaxHostSampleDisks]
	}
	out := make([]protocol.DaemonHostDisk, len(c))
	for i, d := range c {
		out[i] = d.DaemonHostDisk
	}
	return out
}

// hostSampler keeps the newest host sample for heartbeats to attach.
type hostSampler struct {
	mu      sync.RWMutex
	last    *protocol.DaemonHostSample
	collect func() (*protocol.DaemonHostSample, error)
}

func newHostSampler(collect func() (*protocol.DaemonHostSample, error)) *hostSampler {
	return &hostSampler{collect: collect}
}

// Latest returns the newest sample, or nil before the first one. It is never
// refreshed on read: if Run stops, heartbeats keep carrying the old SampledAt
// and the server reads it as stale.
func (s *hostSampler) Latest() *protocol.DaemonHostSample {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.last
}

// Run samples once after warmup, then every interval, until ctx ends. A failed
// sample is logged and the previous one stays.
func (s *hostSampler) Run(ctx context.Context, warmup, interval time.Duration, log *slog.Logger) {
	wait := warmup
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = interval
		sample, err := s.collect()
		if err != nil {
			log.Debug("host sample failed", "error", err)
			continue
		}
		s.mu.Lock()
		s.last = sample
		s.mu.Unlock()
	}
}

// heartbeatRequest is the one heartbeat body for HTTP and WS, carrying the
// latest host sample when a sampler runs.
func (d *Daemon) heartbeatRequest(rid string) protocol.DaemonHeartbeatRequestPayload {
	inFlight := int(d.activeTasks.Load())
	req := protocol.DaemonHeartbeatRequestPayload{RuntimeID: rid, SupportsBatchImport: true, InFlightTasks: &inFlight}
	if d.hostSampler != nil {
		req.HostSample = d.hostSampler.Latest()
	}
	return req
}
