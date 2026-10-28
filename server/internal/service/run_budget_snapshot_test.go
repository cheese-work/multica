package service

import (
	"reflect"
	"testing"
	"time"
)

func TestNewRunBudgetSnapshotStatesAndRaisePreview(t *testing.T) {
	start := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		used     int64
		limit    int
		state    string
		blocking []string
		canRaise bool
		preview  *RunBudgetPreview
	}{
		{"fresh", 3, 40, RunBudgetOK, []string{}, true, &RunBudgetPreview{AddRuns: 10, BalanceAfter: 47, LimitAfter: 50, MaxAllowedAdd: 960}},
		{"just below near", 31, 40, RunBudgetOK, []string{}, true, &RunBudgetPreview{AddRuns: 10, BalanceAfter: 19, LimitAfter: 50, MaxAllowedAdd: 960}},
		{"near at 80 percent", 32, 40, RunBudgetNear, []string{}, true, &RunBudgetPreview{AddRuns: 10, BalanceAfter: 18, LimitAfter: 50, MaxAllowedAdd: 960}},
		{"at the limit", 40, 40, RunBudgetAtLimit, []string{"runs"}, true, &RunBudgetPreview{AddRuns: 10, BalanceAfter: 10, LimitAfter: 50, MaxAllowedAdd: 960}},
		{"over the limit leaves 10", 45, 40, RunBudgetOver, []string{"runs"}, true, &RunBudgetPreview{AddRuns: 10, BalanceAfter: 10, LimitAfter: 55, MaxAllowedAdd: 955}},
		{"max edge", 995, 995, RunBudgetAtLimit, []string{"runs"}, true, &RunBudgetPreview{AddRuns: 5, BalanceAfter: 5, LimitAfter: 1000, MaxAllowedAdd: 5}},
		{"at the platform maximum", 1000, 1000, RunBudgetAtLimit, []string{"runs"}, false, nil},
		{"over the platform maximum", 1010, 1000, RunBudgetOver, []string{"runs"}, false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NewRunBudgetSnapshot(c.used, c.limit, "workflow", start)
			if !got.Budgeted || got.RunsUsed != c.used || got.RunsLimit != c.limit || got.RunsSource != "workflow" || !got.WindowStartedAt.Equal(start) {
				t.Fatalf("identity fields wrong: %+v", got)
			}
			if got.RunsState != c.state || got.State != c.state {
				t.Fatalf("state = %q/%q, want %q", got.RunsState, got.State, c.state)
			}
			if !reflect.DeepEqual(got.Blocking, c.blocking) {
				t.Fatalf("blocking = %#v, want %#v", got.Blocking, c.blocking)
			}
			if got.CanRaise != c.canRaise || !reflect.DeepEqual(got.RaisePreview, c.preview) {
				t.Fatalf("raise = %v %+v, want %v %+v", got.CanRaise, got.RaisePreview, c.canRaise, c.preview)
			}
		})
	}
}
