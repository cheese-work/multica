package service

import (
	"encoding/json"
	"reflect"
	"strings"
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
			got := NewRunBudgetSnapshot(c.used, c.limit, int64(c.limit)-c.used, "workflow", start)
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

// The raise preview reads the admitted balance, not the task count: a grant or a
// refund moves the balance, and carried debt outlives the window's task count.
func TestNewRunBudgetSnapshotRaisePreviewFollowsTheBalance(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		used    int64
		limit   int
		left    int64
		preview RunBudgetPreview
	}{
		{"progress grant on a spent window", 40, 40, 6, RunBudgetPreview{AddRuns: 10, BalanceAfter: 16, LimitAfter: 50, MaxAllowedAdd: 960}},
		{"refund on a spent window", 40, 40, 1, RunBudgetPreview{AddRuns: 10, BalanceAfter: 11, LimitAfter: 50, MaxAllowedAdd: 960}},
		{"carried debt in a fresh window", 0, 40, -12, RunBudgetPreview{AddRuns: 10, BalanceAfter: 10, LimitAfter: 62, MaxAllowedAdd: 948}},
		{"balance above the cap", 3, 40, 45, RunBudgetPreview{AddRuns: 10, BalanceAfter: 55, LimitAfter: 50, MaxAllowedAdd: 960}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NewRunBudgetSnapshot(c.used, c.limit, c.left, "workflow", start)
			if !got.CanRaise || got.RaisePreview == nil || *got.RaisePreview != c.preview {
				t.Fatalf("raise = %v %+v, want %+v", got.CanRaise, got.RaisePreview, c.preview)
			}
		})
	}
}

// The suggested target and its promised balance come from one function, so the
// snapshot, the run prompt and the exhaustion notice cannot disagree.
func TestSuggestedRaiseForBalance(t *testing.T) {
	cases := []struct {
		cap     int
		balance int64
		target  int
		after   int
	}{
		{40, 0, 50, 10},
		{40, 6, 50, 16},
		{40, -12, 62, 10},
		{40, -5, 55, 10},
		{995, 0, 1000, 5},
		{1000, 0, 0, 0},
		{995, -3, 1000, 2},
	}
	for _, c := range cases {
		target, after := SuggestedRaiseForBalance(c.cap, c.balance)
		if target != c.target || after != c.after {
			t.Errorf("SuggestedRaiseForBalance(%d, %d) = %d, %d, want %d, %d", c.cap, c.balance, target, after, c.target, c.after)
		}
	}
}

// window_id is the exact RFC3339Nano string a raise action must echo back.
func TestNewRunBudgetSnapshotExposesTheWindowID(t *testing.T) {
	start := time.Date(2026, 11, 1, 5, 22, 39, 123456000, time.UTC)
	got := NewRunBudgetSnapshot(3, 40, 37, "workflow", start)
	if want := "2026-11-01T05:22:39.123456Z"; got.WindowID != want {
		t.Fatalf("window_id = %q, want %q", got.WindowID, want)
	}
	if body, _ := json.Marshal(got); !strings.Contains(string(body), `"window_id":"2026-11-01T05:22:39.123456Z"`) {
		t.Fatalf("snapshot JSON %s lacks window_id", body)
	}
}
