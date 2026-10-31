package protocol

import (
	"testing"
	"time"
)

func TestHostSampleStale(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		age   time.Duration
		stale bool
	}{{0, false}, {HostSampleStaleAfter, false}, {HostSampleStaleAfter + time.Second, true}, {time.Hour, true}} {
		if got := HostSampleStale(now.Add(-tc.age), now); got != tc.stale {
			t.Errorf("age %v: stale = %v, want %v", tc.age, got, tc.stale)
		}
	}
}
