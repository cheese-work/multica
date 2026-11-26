package incident

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 25, 6, 30, 0, 0, time.UTC) // 13:30 ICT

func run(id, issue, agent string, min int, err string) FailedRun {
	return FailedRun{ID: id, IssueID: issue, AgentID: agent, At: t0.Add(time.Duration(min) * time.Minute), Error: err}
}

func TestClassify(t *testing.T) {
	for want, errs := range map[string][]string{
		ClassConcurrency: {"Concurrency limit exceeded for account"},
		ClassQuota:       {"429 insufficient_quota", "You exceeded your current quota"},
		ClassRateLimit:   {"HTTP 429 Too Many Requests", "rate limit reached"},
		ClassAuth:        {"401 Unauthorized", "invalid api key"},
		ClassStream:      {"stream disconnected before completion"},
		"":               {"claude exited with error: exit status 143", ""},
	} {
		for _, e := range errs {
			if got := Classify(e); got != want {
				t.Errorf("Classify(%q) = %q, want %q", e, got, want)
			}
		}
	}
}

// Replay of 25/11: five Codex agents fail with the concurrency error on several
// issues, plus an unrelated exit-143 failure. Exactly one incident opens.
func TestReplay25Nov(t *testing.T) {
	const cc = "Concurrency limit exceeded for account"
	runs := []FailedRun{
		run("1", "CHE-9765", "sol-200k", 0, cc),
		run("2", "CHE-9766", "luna-2", 2, cc),
		run("3", "CHE-9825", "luna-2", 5, cc),
		run("4", "CHE-9825", "terra", 8, cc),
		run("5", "CHE-9775", "sol", 12, cc),
		run("6", "CHE-9755", "luna", 15, cc),
		run("7", "CHE-9892", "sonnet-200k", 3, "claude exited with error: exit status 143"),
	}
	got := Detect(runs, t0.Add(20*time.Minute))
	if len(got) != 1 {
		t.Fatalf("want 1 incident, got %d: %+v", len(got), got)
	}
	in := got[0]
	if in.Class != ClassConcurrency || len(in.Runs) != 6 || len(in.Issues) != 5 || len(in.Agents) != 5 {
		t.Fatalf("bad incident: class=%s runs=%d issues=%d agents=%d", in.Class, len(in.Runs), len(in.Issues), len(in.Agents))
	}
}

func TestDetectThresholds(t *testing.T) {
	cc := "Concurrency limit exceeded for account"
	now := t0.Add(40 * time.Minute)
	cases := map[string][]FailedRun{
		"two runs":              {run("1", "a", "x", 20, cc), run("2", "b", "x", 21, cc)},
		"one issue only":        {run("1", "a", "x", 20, cc), run("2", "a", "x", 21, cc), run("3", "a", "y", 22, cc)},
		"outside 30 min window": {run("1", "a", "x", 0, cc), run("2", "b", "x", 1, cc), run("3", "c", "x", 2, cc)},
		"non-provider error":    {run("1", "a", "x", 20, "exit status 143"), run("2", "b", "x", 21, "exit status 143"), run("3", "c", "x", 22, "exit status 143")},
	}
	for name, runs := range cases {
		if got := Detect(runs, now); len(got) != 0 {
			t.Errorf("%s: want no incident, got %+v", name, got)
		}
	}
}
