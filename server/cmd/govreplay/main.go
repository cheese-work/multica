// govreplay is an offline, fake-HTTP replay harness for the missing-mention
// governance evaluator (server/internal/governance). It runs a fixture of
// scripted TypeSafe System One answers through the real evaluator, the real
// [github.com/multica-ai/multica/server/pkg/jev.Client] transport, and a
// local httptest server, and prints one machine-readable action/abstention
// result per scenario.
//
// It proves software behavior end to end — request shape, strict-Choice
// decoding, threshold and cross-answer logic — never model accuracy or
// corpus viability. Every "answer" here is a fixture a test author wrote,
// not an observed model output; D02V (CHE-697) owns the deterministic-
// baseline comparison and corpus census that consumes this command's fixed
// interface. There is no live API key, no network access beyond the local
// test server this command starts, and no agent CLI invoked.
//
// Usage (from the server/ directory):
//
//	go run ./cmd/govreplay
//	go run ./cmd/govreplay --fixture internal/governance/testdata/replay_scenarios.json --json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/multica-ai/multica/server/internal/governance"
)

const defaultFixturePath = "internal/governance/testdata/replay_scenarios.json"

func main() {
	fixturePath := flag.String("fixture", defaultFixturePath, "path to a replay scenario fixture (JSON)")
	jsonOutput := flag.Bool("json", false, "print machine-readable JSON instead of a text table")
	flag.Parse()

	ok, err := run(*fixturePath, *jsonOutput, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "govreplay:", err)
		os.Exit(2)
	}
	if !ok {
		os.Exit(1)
	}
}

// run loads the fixture, replays every scenario, and prints the results. It
// returns ok=false (without an error) when every scenario ran but at least
// one did not match its declared expectation — the caller should still
// print the results in that case, then exit non-zero.
func run(fixturePath string, jsonOutput bool, out io.Writer) (bool, error) {
	file, err := governance.LoadReplayScenarios(fixturePath)
	if err != nil {
		return false, err
	}
	if len(file.Scenarios) == 0 {
		return false, fmt.Errorf("fixture %s has no scenarios", fixturePath)
	}

	results, err := governance.RunReplayScenarios(context.Background(), file)
	if err != nil {
		return false, err
	}

	if jsonOutput {
		if err := printJSON(out, results); err != nil {
			return false, err
		}
	} else {
		printTable(out, results)
	}

	allMatch := true
	for _, r := range results {
		if !r.MatchesExpected {
			allMatch = false
		}
	}
	return allMatch, nil
}

func printJSON(out io.Writer, results []governance.ReplayResult) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(results)
}

func printTable(out io.Writer, results []governance.ReplayResult) {
	matched := 0
	for _, r := range results {
		outcome := "ABSTAIN"
		detail := string(r.Decision.AbstainReason)
		if r.Decision.Action != nil {
			outcome = "ACTION"
			detail = fmt.Sprintf("%s -> %s", r.Decision.Action.Kind, r.Decision.Action.Candidate.ID)
		}

		status := "match"
		if !r.MatchesExpected {
			status = "MISMATCH: " + r.Mismatch
		} else {
			matched++
		}

		fmt.Fprintf(out, "%-40s %-8s %-60s %s\n", r.ID, outcome, detail, status)
		if r.EvalError != "" {
			fmt.Fprintf(out, "%-40s   eval error: %s\n", "", r.EvalError)
		}
	}
	fmt.Fprintf(out, "\n%d/%d scenarios matched their expected decision.\n", matched, len(results))
}
