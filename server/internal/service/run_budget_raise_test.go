package service

import (
	"errors"
	"testing"
)

// "Allow N more runs" always leaves N runs after the raise, whether the issue
// is overdrawn, at its limit or still has runs left (CHE-2339 plan rev 3).
func TestPreviewRunBudgetRaiseLeavesNRuns(t *testing.T) {
	cases := []struct {
		name                   string
		credited, used, add    int
		delta, after, balAfter int
	}{
		{"overdrawn 45/40", 40, 45, 10, 15, 55, 10},
		{"at limit 40/40", 40, 40, 10, 10, 50, 10},
		{"runs left 38/40", 40, 38, 10, 10, 50, 12},
		{"max edge 995/995", 995, 995, 5, 5, 1000, 5},
	}
	for _, c := range cases {
		got, err := PreviewRunBudgetRaise(c.credited, c.used, c.add)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got.Delta != c.delta || got.CreditedAfter != c.after || got.BalanceAfter != c.balAfter {
			t.Errorf("%s: got %+v, want delta %d credited_after %d balance_after %d", c.name, got, c.delta, c.after, c.balAfter)
		}
	}
}

func TestPreviewRunBudgetRaiseRespectsPlatformMaximum(t *testing.T) {
	if _, err := PreviewRunBudgetRaise(995, 995, 6); !errors.Is(err, ErrRunBudgetMaxReached) {
		t.Fatalf("995/995 +6 = %v, want ErrRunBudgetMaxReached", err)
	}
	if _, err := PreviewRunBudgetRaise(1000, 1000, 1); !errors.Is(err, ErrRunBudgetMaxReached) {
		t.Fatalf("1000/1000 +1 = %v, want ErrRunBudgetMaxReached", err)
	}
	// Named runs can push usage past the maximum; still no valid raise.
	if _, err := PreviewRunBudgetRaise(1000, 1010, 1); !errors.Is(err, ErrRunBudgetMaxReached) {
		t.Fatalf("1000/1010 +1 = %v, want ErrRunBudgetMaxReached", err)
	}
	if _, err := PreviewRunBudgetRaise(40, 40, 0); err == nil {
		t.Fatal("add 0 must be refused")
	}
}

func TestMaxAllowedRaiseNeverNegative(t *testing.T) {
	for _, c := range []struct{ credited, used, want int }{
		{995, 995, 5}, {1000, 1000, 0}, {1000, 1010, 0}, {40, 45, 955}, {1000, 400, 0},
	} {
		if got := MaxAllowedRaise(c.credited, c.used); got != c.want {
			t.Errorf("MaxAllowedRaise(%d, %d) = %d, want %d", c.credited, c.used, got, c.want)
		}
	}
}

func TestSuggestedRaiseIsClampedToTheMaximum(t *testing.T) {
	for _, c := range []struct{ credited, used, want int }{
		{40, 45, 10}, {995, 995, 5}, {1000, 1000, 0},
	} {
		if got := SuggestedRaise(c.credited, c.used); got != c.want {
			t.Errorf("SuggestedRaise(%d, %d) = %d, want %d", c.credited, c.used, got, c.want)
		}
	}
}

// The absolute form sets the limit to R. R at or below the runs already used
// would leave the issue blocked (the 45-of-45 trap), so it is refused.
func TestRunBudgetRaiseToRefusesATargetBelowUsed(t *testing.T) {
	if _, err := PreviewRunBudgetRaiseTo(40, 45, 45); !errors.Is(err, ErrRunBudgetBelowUsed) {
		t.Fatalf("--runs 45 at 45/40 = %v, want ErrRunBudgetBelowUsed", err)
	}
	got, err := PreviewRunBudgetRaiseTo(40, 45, 55)
	if err != nil {
		t.Fatal(err)
	}
	if got.Delta != 15 || got.BalanceAfter != 10 {
		t.Fatalf("--runs 55 at 45/40 = %+v, want delta 15, 10 left", got)
	}
	if _, err := PreviewRunBudgetRaiseTo(40, 40, 1001); !errors.Is(err, ErrRunBudgetMaxReached) {
		t.Fatalf("--runs 1001 = %v, want ErrRunBudgetMaxReached", err)
	}
}

func TestSuggestedRaiseTargetClearsTheRunsUsedOrIsZeroAtTheMaximum(t *testing.T) {
	for _, c := range []struct{ credited, used, want int }{
		{40, 40, 50},     // at the limit: 10 more
		{40, 45, 55},     // overdrawn: 10 more than used, not limit+5 = 45
		{995, 995, 1000}, // clamped to the platform maximum
		{1000, 1000, 0},  // no valid raise: nothing above the maximum
		{1000, 1010, 0},  // overdrawn past the maximum
	} {
		if got := SuggestedRaiseTarget(c.credited, c.used); got != c.want {
			t.Errorf("SuggestedRaiseTarget(%d, %d) = %d, want %d", c.credited, c.used, got, c.want)
		}
	}
}
