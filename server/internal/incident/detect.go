package incident

import (
	"sort"
	"time"
)

// Config holds the thresholds. The defaults are the CHE-9892 rule: 3 failed runs
// of one class on 2 issues within 30 minutes opens; 30 quiet minutes closes.
type Config struct {
	MinRuns    int
	MinIssues  int
	Window     time.Duration
	CloseAfter time.Duration
}

func DefaultConfig() Config {
	return Config{MinRuns: 3, MinIssues: 2, Window: 30 * time.Minute, CloseAfter: 30 * time.Minute}
}

// Run is one failed agent run with a provider error class.
type Run struct {
	ID, AgentID, RuntimeID, IssueID string
	Class                           Class
	FailedAt                        time.Time
	Error                           string
}

// Cluster is the failed runs of one class inside the window.
type Cluster struct {
	Class                    Class
	Runs                     []Run // oldest first
	Agents, Runtimes, Issues []string
	First, Last              time.Time
}

// Detect returns one cluster per class that meets the thresholds, ordered by class.
func Detect(runs []Run, now time.Time, cfg Config) []Cluster {
	cutoff := now.Add(-cfg.Window)
	byClass := map[Class][]Run{}
	for _, r := range runs {
		if r.Class != "" && !r.FailedAt.Before(cutoff) && !r.FailedAt.After(now) {
			byClass[r.Class] = append(byClass[r.Class], r)
		}
	}
	var out []Cluster
	for class, rs := range byClass {
		sort.SliceStable(rs, func(i, j int) bool { return rs[i].FailedAt.Before(rs[j].FailedAt) })
		c := Cluster{Class: class, Runs: rs, First: rs[0].FailedAt, Last: rs[len(rs)-1].FailedAt,
			Agents:   distinct(rs, func(r Run) string { return r.AgentID }),
			Runtimes: distinct(rs, func(r Run) string { return r.RuntimeID }),
			Issues:   distinct(rs, func(r Run) string { return r.IssueID })}
		if len(rs) >= cfg.MinRuns && len(c.Issues) >= cfg.MinIssues {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Class < out[j].Class })
	return out
}

func distinct(rs []Run, key func(Run) string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rs {
		if k := key(r); k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
