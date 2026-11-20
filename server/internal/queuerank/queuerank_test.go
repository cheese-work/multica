package queuerank

import (
	"testing"
	"time"
)

func ptr(s string) *string { return &s }

func TestBand(t *testing.T) {
	for in, want := range map[string]int{"urgent": 4, "high": 3, "medium": 2, "low": 1, "none": 0, "": 0, "bogus": 0} {
		if got := Band(in); got != want {
			t.Errorf("Band(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestIssueKeysAndBase(t *testing.T) {
	cases := []struct {
		name            string
		own             string
		project, parent *string
		keys            Keys
		base            int
	}{
		{"all urgent", "urgent", ptr("urgent"), ptr("urgent"), Keys{4, 4, 4}, 124},
		{"no project", "high", nil, ptr("low"), Keys{0, 1, 3}, 8},
		{"no parent repeats own", "medium", ptr("high"), nil, Keys{3, 2, 2}, 87},
		{"no project, no parent", "low", nil, nil, Keys{0, 1, 1}, 6},
		{"all none", "none", ptr("none"), ptr("none"), Keys{}, 0},
	}
	for _, c := range cases {
		k := IssueKeys(c.own, c.project, c.parent)
		if k != c.keys || k.Base() != c.base {
			t.Errorf("%s: keys %+v base %d, want %+v base %d", c.name, k, k.Base(), c.keys, c.base)
		}
	}
}

func TestFallbackKeys(t *testing.T) {
	for p, base := range map[int]int{0: 0, 1: 31, 2: 62, 3: 93, 4: 124, -3: 0, 9: 124} {
		if got := FallbackKeys(p).Base(); got != base {
			t.Errorf("FallbackKeys(%d).Base() = %d, want %d", p, got, base)
		}
	}
}

func TestEffectiveAging(t *testing.T) {
	const aging = 30 * time.Minute
	for _, c := range []struct {
		wait time.Duration
		want int
	}{
		{-time.Hour, 10}, {0, 10}, {aging - time.Microsecond, 10}, {aging, 35}, {2*aging + time.Second, 60}, {5 * aging, 135},
	} {
		if got := Effective(10, c.wait, aging); got != c.want {
			t.Errorf("Effective wait %v = %d, want %d", c.wait, got, c.want)
		}
	}
}

func TestSettings(t *testing.T) {
	for in, want := range map[string]string{"project_rank": ModeProjectRank, "priority": ModePriority, "": ModePriority, "PROJECT_RANK": ModePriority, "bogus": ModePriority} {
		if got := ModeFromSetting(in); got != want {
			t.Errorf("ModeFromSetting(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]int{"": 30, "abc": 30, "45.5": 30, "-5": 30, "5": 5, "45": 45, "720": 720, "1": 5, "4": 5, "721": 720, "999999": 720, "0": 5} {
		if got := AgingFromSetting(in); got != time.Duration(want)*time.Minute {
			t.Errorf("AgingFromSetting(%q) = %v, want %d min", in, got, want)
		}
	}
}

func TestPriorityModeScoresZero(t *testing.T) {
	if got := Score(ModePriority, Keys{4, 4, 4}, 100*time.Hour, 5*time.Minute); got != 0 {
		t.Fatalf("priority mode Score = %d, want 0", got)
	}
	if got := Score("", Keys{4, 4, 4}, time.Hour, time.Minute); got != 0 {
		t.Fatalf("unset mode Score = %d, want 0", got)
	}
}

// Starvation bound (plan r4 Approach 5): an item whose wait began 5 aging steps
// before another's beats it whatever their bases, because 25 x 5 = 125 > 124.
func TestStarvationBound(t *testing.T) {
	const aging = 30 * time.Minute
	fresh := Score(ModeProjectRank, IssueKeys("urgent", ptr("urgent"), ptr("urgent")), 0, aging)
	if fresh != 124 {
		t.Fatalf("fresh max item = %d, want 124", fresh)
	}
	oldest := Score(ModeProjectRank, IssueKeys("none", nil, ptr("none")), 5*aging, aging)
	if oldest != 125 || oldest <= fresh {
		t.Fatalf("5 steps older base-0 item = %d, must beat fresh %d", oldest, fresh)
	}
	four := Score(ModeProjectRank, IssueKeys("none", nil, ptr("none")), 4*aging, aging)
	if four >= fresh {
		t.Fatalf("4 steps older base-0 item = %d must not yet beat fresh %d: the bound is tight", four, fresh)
	}
	// Every pair of bases.
	for a := 0; a <= 124; a++ {
		for b := 0; b <= 124; b++ {
			if Effective(a, 5*aging, aging) <= Effective(b, 0, aging) {
				t.Fatalf("base %d aged 5 steps does not beat fresh base %d", a, b)
			}
		}
	}
}
