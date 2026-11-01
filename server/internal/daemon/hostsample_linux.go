//go:build linux

package daemon

import (
	"os"
	"syscall"
	"time"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

// newProcHostCollector primes the CPU counters and returns a collector reading
// /proc and statfs. No new dependencies; Linux only.
func newProcHostCollector() func() (*protocol.DaemonHostSample, error) {
	prev, _ := readCPUTimes()
	return func() (*protocol.DaemonHostSample, error) {
		cur, err := readCPUTimes()
		if err != nil {
			return nil, err
		}
		cpu := cpuPercent(prev, cur)
		prev = cur
		f, err := os.Open("/proc/meminfo")
		if err != nil {
			return nil, err
		}
		defer f.Close()
		ram, err := parseMemInfoPercent(f)
		if err != nil {
			return nil, err
		}
		return &protocol.DaemonHostSample{SampledAt: time.Now().UTC().Truncate(time.Minute), CPUPercent: cpu, RAMPercent: ram, Disks: readDisks()}, nil
	}
}

func readCPUTimes() (cpuTimes, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuTimes{}, err
	}
	defer f.Close()
	return parseCPUStat(f)
}

func readDisks() []protocol.DaemonHostDisk {
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		return nil
	}
	defer f.Close()
	var cands []diskCandidate
	for _, m := range parseMounts(f) {
		var st syscall.Statfs_t
		if syscall.Statfs(m.Path, &st) != nil || st.Blocks == 0 {
			continue
		}
		// df semantics: used / (used + available to unprivileged users).
		used := float64(st.Blocks - st.Bfree)
		pct := clampPercent(100 * used / (used + float64(st.Bavail)))
		cands = append(cands, diskCandidate{protocol.DaemonHostDisk{MountPath: m.Path, Percent: pct}, st.Blocks * uint64(st.Bsize)})
	}
	return selectDisks(cands)
}
