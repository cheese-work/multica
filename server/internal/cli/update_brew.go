package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const brewFormula = "multica-ai/tap/multica"

// brewPollInterval is how often the download size is sampled.
var brewPollInterval = 200 * time.Millisecond

// UpdateViaBrewWithProgress is UpdateViaBrew with measured progress. Without a
// terminal on p it is exactly UpdateViaBrew: one `brew upgrade`, nothing printed.
// On a terminal it runs `brew update` first, then samples the size of the
// downloads the upgrade owns and draws it as an unknown-size line.
func UpdateViaBrewWithProgress(p *Progress) (string, error) {
	if !p.Active() {
		return UpdateViaBrew()
	}
	p.Phase("Updating Homebrew")
	_ = exec.Command("brew", "update").Run() // a failed update must not stop the upgrade

	paths := brewOwnedPaths()
	hits := make(map[string]bool, len(paths))
	for _, path := range paths {
		if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
			hits[path] = true
		}
	}

	p.Phase("Downloading and installing via Homebrew")
	cmd := exec.Command("brew", "upgrade", brewFormula)
	cmd.Env = append(os.Environ(), "HOMEBREW_NO_AUTO_UPDATE=1")
	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := cmd.CombinedOutput()
		done <- result{out, err}
	}()

	var sampler brewSampler
	tick := time.NewTicker(brewPollInterval)
	defer tick.Stop()
	for {
		select {
		case r := <-done:
			if r.err != nil {
				p.Finish()
				return string(r.out), fmt.Errorf("brew upgrade failed: %w", r.err)
			}
			p.Phase("Installed")
			return string(r.out), nil
		case <-tick.C:
			if n := brewDownloadedBytes(paths, hits); sampler.report(n) {
				p.Set(n, 0)
			}
		}
	}
}

// brewOwnedPaths returns the cache paths of the formula and its dependencies,
// or nil when brew cannot say (no size line is drawn then).
func brewOwnedPaths() []string {
	names := []string{brewFormula}
	if out, err := exec.Command("brew", "deps", brewFormula).Output(); err == nil {
		names = append(names, strings.Fields(string(out))...)
	}
	out, err := exec.Command("brew", append([]string{"--cache"}, names...)...).Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// brewDownloadedBytes sums the bytes on disk for the owned download paths: the
// partial file if present, else the finished file. Checking the partial file
// first means a rename between the two stats never drops the count. A path that
// was already cached before the upgrade (hits) counts only through a partial
// file, which appears if brew re-downloads it.
func brewDownloadedBytes(paths []string, hits map[string]bool) int64 {
	var sum int64
	for _, path := range paths {
		if st, err := os.Stat(path + ".incomplete"); err == nil {
			sum += st.Size()
		} else if st, err := os.Stat(path); err == nil && !hits[path] {
			sum += st.Size()
		}
	}
	return sum
}

// brewSampler decides which samples reach Progress: none until the first
// nonzero one, then all, so a drop to zero (a retry deleted the only partial
// file) still resets the rate baseline.
type brewSampler struct{ seen bool }

func (s *brewSampler) report(n int64) bool {
	s.seen = s.seen || n > 0
	return s.seen
}
