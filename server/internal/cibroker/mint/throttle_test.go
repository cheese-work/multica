package mint

import (
	"fmt"
	"testing"
	"time"
)

func TestThrottlePerRunAttempt(t *testing.T) {
	clk := newClock()
	th := NewThrottle(clk.Now)
	for i := 0; i < MaxPerRun; i++ {
		if r := th.Check(1, "run-1", "1"); r != "" {
			t.Fatalf("issue %d refused: %s", i, r)
		}
		th.Record(1, "run-1", "1")
	}
	if r := th.Check(1, "run-1", "1"); r != "throttle_run" {
		t.Fatalf("5th = %q, want throttle_run", r)
	}
	if r := th.Check(1, "run-1", "2"); r != "" {
		t.Fatalf("new attempt = %q, want allowed", r)
	}
	if r := th.Check(1, "run-2", "1"); r != "" {
		t.Fatalf("other run = %q, want allowed", r)
	}
}

func TestThrottleCheckDoesNotCount(t *testing.T) {
	th := NewThrottle(newClock().Now)
	for i := 0; i < 10; i++ {
		if r := th.Check(1, "run-1", "1"); r != "" {
			t.Fatalf("check %d = %q", i, r)
		}
	}
}

func TestThrottlePerRepoHour(t *testing.T) {
	clk := newClock()
	th := NewThrottle(clk.Now)
	for i := 0; i < MaxPerRepoHour; i++ {
		run := fmt.Sprintf("run-%d", i)
		if r := th.Check(7, run, "1"); r != "" {
			t.Fatalf("issue %d refused: %s", i, r)
		}
		th.Record(7, run, "1")
		clk.Advance(time.Second)
	}
	if r := th.Check(7, "run-new", "1"); r != "throttle_repo" {
		t.Fatalf("61st = %q, want throttle_repo", r)
	}
	if r := th.Check(8, "run-new", "1"); r != "" {
		t.Fatalf("other repo = %q, want allowed", r)
	}
	// The first token was recorded at t0; one hour after it, a slot frees up.
	clk.Advance(time.Hour - MaxPerRepoHour*time.Second + time.Second)
	if r := th.Check(7, "run-new", "1"); r != "" {
		t.Fatalf("after window = %q, want allowed", r)
	}
}
