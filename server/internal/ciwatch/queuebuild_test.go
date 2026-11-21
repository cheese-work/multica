package ciwatch

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/mergequeue"
)

const goLog = `2026-11-18T10:00:00.1234567Z === RUN   TestA
2026-11-18T10:00:00.1234567Z --- FAIL: TestA (0.01s)
2026-11-18T10:00:00.1234567Z     --- FAIL: TestA/sub (0.00s)
2026-11-18T10:00:00.1234567Z FAIL
2026-11-18T10:00:00.1234567Z FAIL	example.com/p1	0.020s
2026-11-18T10:00:00.1234567Z --- PASS: TestOK (0.00s)
2026-11-18T10:00:00.1234567Z ok  	example.com/p2	0.010s
2026-11-18T10:00:00.1234567Z --- FAIL: TestB (0.01s)
2026-11-18T10:00:00.1234567Z FAIL	example.com/p3	0.020s
2026-11-18T10:00:00.1234567Z FAIL	example.com/p4 [build failed]
2026-11-18T10:00:00.1234567Z --- FAIL: TestNoPackage (0.01s)
`

func TestParseFailingTestsAttributesEachTestToItsPackage(t *testing.T) {
	lines := splitLines(goLog)
	got := ParseFailingTests(lines)
	want := []mergequeue.TestID{
		{Package: "example.com/p1", Name: "TestA"},
		{Package: "example.com/p1", Name: "TestA/sub"},
		{Package: "example.com/p3", Name: "TestB"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v; want %v", got, want)
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return out
}

func queueRun() Run {
	return Run{WorkspaceID: "ws", InstallationID: 7, RepoOwner: "cheese-work", RepoName: "multica", RunID: 99, RunAttempt: 1, WorkflowName: "CI", Event: "merge_group", Conclusion: "failure"}
}

func TestQueueBuildCollectsFailingTestsChecksAndSignature(t *testing.T) {
	gh := &fakeGH{
		jobsByPage: [][]Job{{
			failedJob(1, "backend-test-shards (3)", t0),
			{ID: 2, Name: "lint", Conclusion: "success", CompletedAt: t0},
			{ID: 3, Name: "e2e", Conclusion: "timed_out", CompletedAt: t0.Add(time.Minute)},
		}},
		logs: map[int64]func(context.Context) io.Reader{1: text(goLog), 3: text("timed out\n")},
	}
	b := &fakeBudget{}
	c, _ := newCollector(gh, b, fakeRuns{})
	got, err := c.QueueBuild(context.Background(), queueRun())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Failing) != 3 || got.Failing[0] != (mergequeue.TestID{Package: "example.com/p1", Name: "TestA"}) {
		t.Errorf("failing = %v; want the three tests of the first log", got.Failing)
	}
	wantChecks := []mergequeue.FailedCheck{{Name: "backend-test-shards (3)", Conclusion: "failure"}, {Name: "e2e", Conclusion: "timed_out"}}
	if !reflect.DeepEqual(got.Checks, wantChecks) {
		t.Errorf("checks = %v; want %v", got.Checks, wantChecks)
	}
	if got.Signature == "" {
		t.Error("signature is empty")
	}
	if b.outstanding != 0 || b.reserved != 3 { // one job list and two logs
		t.Errorf("budget: outstanding %d, reserved %d; want 0 and 3", b.outstanding, b.reserved)
	}
}

func TestQueueBuildReturnsTheBudgetErrorSoTheCallerRetries(t *testing.T) {
	gh := &fakeGH{jobsByPage: [][]Job{{failedJob(1, "x", t0)}}, logs: map[int64]func(context.Context) io.Reader{1: text(goLog)}}
	c, _ := newCollector(gh, &fakeBudget{err: ErrNoBudget}, fakeRuns{})
	if _, err := c.QueueBuild(context.Background(), queueRun()); !errors.Is(err, ErrNoBudget) {
		t.Errorf("err = %v; want ErrNoBudget", err)
	}
	if gh.listCalls != 0 {
		t.Errorf("listed jobs %d times with no budget; want 0", gh.listCalls)
	}
}
