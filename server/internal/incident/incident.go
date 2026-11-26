// Package incident detects provider incidents from failed agent runs (CHE-9892).
// Pure logic only: callers load failed runs and act on the result.
package incident

import (
	"sort"
	"strings"
	"time"
)

const (
	ClassConcurrency = "concurrency_limit"
	ClassQuota       = "quota"
	ClassRateLimit   = "rate_limit_429"
	ClassAuth        = "auth"
	ClassStream      = "stream_disconnect"

	// Window, MinRuns and MinIssues are the CHE-9892 incident definition:
	// 3+ same-class failures within 30 minutes on 2+ issues.
	Window    = 30 * time.Minute
	MinRuns   = 3
	MinIssues = 2
)

type FailedRun struct {
	ID, IssueID, AgentID string
	At                   time.Time
	Error                string
}

type Incident struct {
	Class  string
	Runs   []FailedRun
	Issues []string
	Agents []string
}

// Order matters: quota is checked before 429 because quota errors often carry a 429.
var classes = []struct {
	class string
	subs  []string
}{
	{ClassConcurrency, []string{"concurrency limit"}},
	{ClassQuota, []string{"quota", "usage limit", "billing"}},
	{ClassRateLimit, []string{"429", "rate limit", "too many requests"}},
	{ClassAuth, []string{"401", "unauthorized", "invalid api key", "authentication"}},
	{ClassStream, []string{"stream disconnected", "stream error", "stream closed"}},
}

// Classify returns the provider error class of msg, or "" if it is not one.
func Classify(msg string) string {
	msg = strings.ToLower(msg)
	for _, c := range classes {
		for _, s := range c.subs {
			if strings.Contains(msg, s) {
				return c.class
			}
		}
	}
	return ""
}

// Detect returns one incident per error class that meets the threshold among
// runs failed in the Window ending at now. Output is sorted by class.
func Detect(runs []FailedRun, now time.Time) []Incident {
	byClass := map[string][]FailedRun{}
	for _, r := range runs {
		if r.At.After(now) || now.Sub(r.At) > Window {
			continue
		}
		if c := Classify(r.Error); c != "" {
			byClass[c] = append(byClass[c], r)
		}
	}
	var out []Incident
	for class, rs := range byClass {
		issues, agents := uniq(rs, func(r FailedRun) string { return r.IssueID }), uniq(rs, func(r FailedRun) string { return r.AgentID })
		if len(rs) >= MinRuns && len(issues) >= MinIssues {
			out = append(out, Incident{Class: class, Runs: rs, Issues: issues, Agents: agents})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Class < out[j].Class })
	return out
}

func uniq(rs []FailedRun, key func(FailedRun) string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rs {
		if k := key(r); !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
