// Package queuerank scores a queued item for the project > parent > child run
// queue order (CHE-8749). SQL function queue_rank in migration 99d_queue_rank
// holds the same definition; contract_test.go keeps the two equal.
package queuerank

import "time"

const (
	// ModePriority is the workspace default: today's order, rank 0 for every row.
	ModePriority = "priority"
	// ModeProjectRank orders by project band, parent band, own band and wait.
	ModeProjectRank = "project_rank"

	DefaultAgingMinutes = 30
	MinAgingMinutes     = 5
	MaxAgingMinutes     = 720
)

// Keys are the three band keys of one item, each 0 (none) to 4 (urgent).
type Keys struct{ Project, Parent, Own int }

// Band maps an issue or project priority to its band.
func Band(priority string) int { return -1 }

// IssueKeys builds the keys of an issue run. A nil project gives band 0; a nil
// parent repeats the issue's own band.
func IssueKeys(own string, project, parent *string) Keys { return Keys{} }

// FallbackKeys builds the keys of a run with no issue (DM, quick-create,
// run_only autopilot): all three equal the queue row's own priority.
func FallbackKeys(priority int) Keys { return Keys{} }

// Base is 25 x project + 5 x parent + own, 0 to 124.
func (k Keys) Base() int { return -1 }

// Effective adds 25 per whole aging step waited.
func Effective(base int, wait, aging time.Duration) int { return -1 }

// ModeFromSetting returns the workspace mode; anything but project_rank is priority.
func ModeFromSetting(raw string) string { return "" }

// AgingFromSetting returns the aging step for a queue_aging_minutes setting.
func AgingFromSetting(raw string) time.Duration { return 0 }

// Score is the rank of one item: 0 unless the workspace mode is project_rank.
func Score(mode string, k Keys, wait, aging time.Duration) int { return -1 }
