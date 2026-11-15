// Package stagequeue holds the work-in-progress limits of the bottleneck stages
// a PR passes through (CHE-3611): cross-lab review, the device lane and the
// browser lane. A stage admits at most N PRs at a time, so work in the stage
// finishes before more starts. Authors are not capped.
//
// The package is pure: callers pass the PRs now in one slot owner's stage (one
// reviewer agent, one device lane, one browser lane) and read back who runs and
// who waits. A PR holds a slot only while its stage run is Active, so a waiting
// PR blocks nobody and a stack cannot deadlock on a slot (no hold-and-wait).
package stagequeue

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Stage is a bottleneck stage with a capacity limit.
type Stage string

const (
	StageReview  Stage = "review"
	StageDevice  Stage = "device"
	StageBrowser Stage = "browser"
)

// Limits maps a stage to N, the PRs it admits per slot owner.
type Limits map[Stage]int

// DefaultLimits is N = 2 per reviewer agent, 1 per device lane, 1 per browser lane.
func DefaultLimits() Limits {
	return Limits{StageReview: 2, StageDevice: 1, StageBrowser: 1}
}

// For returns the stage's N, or the default when it is not set to at least 1.
func (l Limits) For(s Stage) int {
	if n := l[s]; n >= 1 {
		return n
	}
	return DefaultLimits()[s]
}

// LimitsFromEnv reads MULTICA_STAGE_LIMIT_REVIEW, _DEVICE and _BROWSER. A value
// that is not an integer of at least 1 keeps the default.
func LimitsFromEnv(getenv func(string) string) Limits {
	l := DefaultLimits()
	for s := range l {
		raw := getenv("MULTICA_STAGE_LIMIT_" + strings.ToUpper(string(s)))
		if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && n >= 1 {
			l[s] = n
		}
	}
	return l
}

// Entry is one PR in a slot owner's stage. Active means its stage run is
// running or pending now; only an Active entry holds a slot.
type Entry struct {
	PR string
	// OpenGates is the number of gates the PR still has to pass; fewer means
	// closer to merge.
	OpenGates int
	Created   time.Time
	Active    bool
	// Ref is the caller's handle for the run to start: the id of the issue the
	// stage run belongs to. Plan keeps it from the PR's first entry.
	Ref string
}

// Decision splits a stage's PRs into those that run and those that wait, in
// queue order.
type Decision struct {
	Admitted []Entry
	Queued   []Entry
}

// Plan admits at most limit PRs. Active PRs keep their slots and are never
// evicted, even when the limit was lowered under them. Free slots go to waiting
// PRs by fewest open gates, then oldest PR. A PR listed twice counts once, and
// is Active if any of its entries is.
func Plan(entries []Entry, limit int) Decision {
	byPR := map[string]Entry{}
	var order []string
	for _, e := range entries {
		cur, seen := byPR[e.PR]
		if !seen {
			order = append(order, e.PR)
			byPR[e.PR] = e
			continue
		}
		cur.Active = cur.Active || e.Active
		byPR[e.PR] = cur
	}
	var active, waiting []Entry
	for _, pr := range order {
		if e := byPR[pr]; e.Active {
			active = append(active, e)
		} else {
			waiting = append(waiting, e)
		}
	}
	slices.SortStableFunc(waiting, func(a, b Entry) int {
		return cmp.Or(
			cmp.Compare(a.OpenGates, b.OpenGates),
			a.Created.Compare(b.Created),
			cmp.Compare(a.PR, b.PR),
		)
	})
	free := max(limit-len(active), 0)
	free = min(free, len(waiting))
	return Decision{Admitted: append(active, waiting[:free]...), Queued: waiting[free:]}
}

// Admit reports whether candidate may start its stage run now, given the PRs
// already in the stage. When it must wait, position is its 0-based place in the
// queue; when admitted, position is 0.
func Admit(entries []Entry, candidate Entry, limit int) (admitted bool, position int) {
	d := Plan(append(slices.Clone(entries), candidate), limit)
	for i, e := range d.Queued {
		if e.PR == candidate.PR {
			return false, i
		}
	}
	return true, 0
}

// GateStage names the lane stage a gate sub-issue queues in, from its title
// "<stage> gate: <KEY>" (the Default workflow's gate sub-issue convention).
// Only the device and browser lanes are bottleneck stages here; any other gate
// title is not queued.
func GateStage(title string) (Stage, bool) {
	t := strings.ToLower(strings.TrimSpace(title))
	for _, s := range []Stage{StageDevice, StageBrowser} {
		if strings.HasPrefix(t, string(s)+" gate:") {
			return s, true
		}
	}
	return "", false
}
