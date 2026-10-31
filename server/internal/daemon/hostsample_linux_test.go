//go:build linux

package daemon

import (
	"testing"
	"time"
)

// Smoke: the real collector reads this host's /proc and statfs.
func TestProcHostCollectorLive(t *testing.T) {
	collect := newProcHostCollector()
	time.Sleep(50 * time.Millisecond)
	s, err := collect()
	if err != nil {
		t.Fatal(err)
	}
	if s.RAMPercent <= 0 || s.RAMPercent > 100 || s.CPUPercent < 0 || s.CPUPercent > 100 || s.SampledAt.IsZero() {
		t.Fatalf("sample out of range: %+v", s)
	}
	t.Logf("live sample: %+v", s)
}
