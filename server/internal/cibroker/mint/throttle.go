package mint

import (
	"time"
)

// Throttle limits (rule 7).
const (
	MaxPerRun      = 4
	MaxPerRepoHour = 60
	repoWindow     = time.Hour
	// runTTL outlives any job (self-hosted jobs run up to 5 days), so a run
	// attempt is never forgotten while it can still ask for a token.
	runTTL = 7 * 24 * time.Hour
)

type runKey struct {
	repoID  int64
	run     string
	attempt string
}

type runCount struct {
	n    int
	last time.Time
}

// Throttle counts issued tokens per run attempt and per repo. The JWT has no
// job claim, so the run limit is per run attempt. State is in memory; callers
// must serialize it (the broker's mutex does).
type Throttle struct {
	now   func() time.Time
	runs  map[runKey]*runCount
	repos map[int64][]time.Time
}

// NewThrottle builds a Throttle on a clock.
func NewThrottle(now func() time.Time) *Throttle {
	return &Throttle{now: now, runs: map[runKey]*runCount{}, repos: map[int64][]time.Time{}}
}

// Check returns "" or the refusal reason: "throttle_run" or "throttle_repo".
func (t *Throttle) Check(repoID int64, runID, runAttempt string) string {
	if c := t.runs[runKey{repoID, runID, runAttempt}]; c != nil && c.n >= MaxPerRun {
		return "throttle_run"
	}
	if len(t.recent(repoID, t.now())) >= MaxPerRepoHour {
		return "throttle_repo"
	}
	return ""
}

// Record counts one issued token and drops expired state.
func (t *Throttle) Record(repoID int64, runID, runAttempt string) {
	now := t.now()
	k := runKey{repoID, runID, runAttempt}
	c := t.runs[k]
	if c == nil {
		c = &runCount{}
		t.runs[k] = c
	}
	c.n++
	c.last = now
	t.repos[repoID] = append(t.recent(repoID, now), now)
	for k, c := range t.runs {
		if now.Sub(c.last) > runTTL {
			delete(t.runs, k)
		}
	}
}

// recent returns the repo's issue times inside the window, oldest first.
func (t *Throttle) recent(repoID int64, now time.Time) []time.Time {
	ts := t.repos[repoID]
	i := 0
	for i < len(ts) && now.Sub(ts[i]) >= repoWindow {
		i++
	}
	if i > 0 {
		ts = ts[i:]
		t.repos[repoID] = ts
	}
	return ts
}
