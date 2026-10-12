package governance

import (
	"context"
	"testing"
)

// TestReplayFixtureMatchesExpectations runs the public-safe fixture set
// (testdata/replay_scenarios.json, also shipped for cmd/govreplay) through
// the real Evaluate + jev.Client + local httptest wire path and checks
// every scenario against its declared expectation. This is the "first
// vertical proof" from the no-key test plan: a fake judgment goes in, one
// decision comes out, deterministically, with zero network access beyond
// the local test server.
func TestReplayFixtureMatchesExpectations(t *testing.T) {
	file, err := LoadReplayScenarios("testdata/replay_scenarios.json")
	if err != nil {
		t.Fatalf("LoadReplayScenarios: %v", err)
	}
	if len(file.Scenarios) == 0 {
		t.Fatal("fixture has no scenarios")
	}

	results, err := RunReplayScenarios(context.Background(), file)
	if err != nil {
		t.Fatalf("RunReplayScenarios: %v", err)
	}

	for _, r := range results {
		t.Run(r.ID, func(t *testing.T) {
			if r.EvalError != "" {
				t.Errorf("unexpected evaluation error: %s", r.EvalError)
			}
			if !r.MatchesExpected {
				t.Errorf("mismatch: %s (decision: %+v)", r.Mismatch, r.Decision)
			}
		})
	}
}

// Repeating the exact same scenario must produce the exact same decision:
// the replay harness proves determinism, not just a single lucky pass.
func TestReplayScenarioIsRepeatable(t *testing.T) {
	file, err := LoadReplayScenarios("testdata/replay_scenarios.json")
	if err != nil {
		t.Fatalf("LoadReplayScenarios: %v", err)
	}
	sc := file.Scenarios[0]

	first, err := RunReplayScenario(context.Background(), sc)
	if err != nil {
		t.Fatalf("RunReplayScenario (first): %v", err)
	}
	second, err := RunReplayScenario(context.Background(), sc)
	if err != nil {
		t.Fatalf("RunReplayScenario (second): %v", err)
	}
	if !first.MatchesExpected || !second.MatchesExpected {
		t.Fatalf("scenario %q did not match on both runs: first=%v second=%v", sc.ID, first, second)
	}
}
