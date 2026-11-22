// Package queuerank scores a queued item for the project > parent > child run
// queue order (CHE-8749). SQL function queue_rank in migration 99d_queue_rank
// holds the same definition; contract_test.go keeps the two equal.
package queuerank

import (
	"strconv"
	"time"
)

const (
	// ModePriority is the workspace default: today's order, rank 0 for every row.
	ModePriority = "priority"
	// ModeProjectRank orders by project band, parent band, own band and wait.
	ModeProjectRank = "project_rank"

	DefaultAgingMinutes = 30
	MinAgingMinutes     = 5
	MaxAgingMinutes     = 720

	projectWeight = 25
	parentWeight  = 5
	// agingStep is one aging step's worth of rank. It exceeds the largest base
	// (124), so an item waiting 5 steps longer than another always wins.
	agingStep = 25
)

// Keys are the three band keys of one item, each 0 (none) to 4 (urgent).
type Keys struct{ Project, Parent, Own int }

// Band maps an issue or project priority to its band.
func Band(priority string) int {
	switch priority {
	case "urgent":
		return 4
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	}
	return 0
}

// IssueKeys builds the keys of an issue run. A nil project gives band 0; a nil
// parent repeats the issue's own band.
func IssueKeys(own string, project, parent *string) Keys {
	k := Keys{Own: Band(own)}
	k.Parent = k.Own
	if project != nil {
		k.Project = Band(*project)
	}
	if parent != nil {
		k.Parent = Band(*parent)
	}
	return k
}

// FallbackKeys builds the keys of a run with no issue (DM, quick-create,
// run_only autopilot): all three equal the queue row's own priority.
func FallbackKeys(priority int) Keys {
	b := min(max(priority, 0), 4)
	return Keys{b, b, b}
}

// Base is 25 x project + 5 x parent + own, 0 to 124.
func (k Keys) Base() int { return projectWeight*k.Project + parentWeight*k.Parent + k.Own }

// Effective adds 25 per whole aging step waited.
func Effective(base int, wait, aging time.Duration) int {
	if wait <= 0 || aging <= 0 {
		return base
	}
	return base + agingStep*int(wait/aging)
}

// ModeFromSetting returns the workspace mode; anything but project_rank is priority.
func ModeFromSetting(raw string) string {
	if raw == ModeProjectRank {
		return ModeProjectRank
	}
	return ModePriority
}

// AgingFromSetting returns the aging step for a queue_aging_minutes setting.
//
// The value is clamped to 5..720 minutes; anything that is not a plain
// non-negative integer gives the 30 minute default. SQL queue_rank_at applies
// the same rule to workspace.settings.
func AgingFromSetting(raw string) time.Duration {
	minutes := DefaultAgingMinutes
	if len(raw) > 0 && len(raw) <= 6 {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 && raw[0] != '+' && raw[0] != '-' {
			minutes = min(max(n, MinAgingMinutes), MaxAgingMinutes)
		}
	}
	return time.Duration(minutes) * time.Minute
}

// Score is the rank of one item: 0 unless the workspace mode is project_rank.
func Score(mode string, k Keys, wait, aging time.Duration) int {
	if mode != ModeProjectRank {
		return 0
	}
	return Effective(k.Base(), wait, aging)
}
