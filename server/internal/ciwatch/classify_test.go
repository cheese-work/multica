package ciwatch

import "testing"

func evOf(conclusion, evidence, partial string, failedJobs int, excerpts ...string) *FailureEvent {
	ev := &FailureEvent{Conclusion: conclusion, Evidence: evidence, PartialReason: partial, FailedJobCount: failedJobs}
	for i, x := range excerpts {
		ev.Jobs = append(ev.Jobs, JobEvidence{JobID: int64(i + 1), Name: "job", Excerpt: x})
	}
	return ev
}

func full(excerpts ...string) *FailureEvent {
	return evOf("failure", EvidenceFull, "", len(excerpts), excerpts...)
}

func TestClassify(t *testing.T) {
	runnerLost := full("")
	runnerLost.RunnerLost = true
	runnerLostPartial := evOf("failure", EvidencePartial, PartialLogTruncated, 1)
	runnerLostPartial.RunnerLost = true

	tests := []struct {
		name  string
		ev    *FailureEvent
		hist  History
		class Class
		proto string
		shad  bool
	}{
		// Row 1
		{"rate limit, run 42164833838 trace", full("gh: API rate limit exceeded for installation (HTTP 403)"), History{}, ClassRateLimit, "P1", false},
		{"rate limit, secondary", full("You have exceeded a secondary rate limit"), History{}, ClassRateLimit, "P1", false},
		{"rate limit, 429 text", full("HTTP 429: too many requests, rate limit hit"), History{}, ClassRateLimit, "P1", false},
		{"rate limit beats a test failure in the same log", full("--- FAIL: TestX\nAPI rate limit exceeded"), History{}, ClassRateLimit, "P1", false},
		{"rate limit beats flaky counter", full("API rate limit exceeded"), History{FlakeCount7d: 5}, ClassRateLimit, "P1", false},
		// Row 2
		{"config: missing input", full("Error: Input required and not supplied: token"), History{}, ClassConfig, "P4", false},
		{"config: 403 not accessible", full("Resource not accessible by integration"), History{}, ClassConfig, "P4", false},
		{"config: plain 403 forbidden", full("remote: HTTP 403 Forbidden"), History{}, ClassConfig, "P4", false},
		{"config: invalid workflow file", full("Invalid workflow file: .github/workflows/ci.yml#L12"), History{}, ClassConfig, "P4", false},
		{"config: startup_failure with zero jobs, partial evidence", evOf("startup_failure", EvidencePartial, PartialNoJobs, 0), History{}, ClassConfig, "P4", false},
		{"startup_failure never goes to P1", evOf("startup_failure", EvidencePartial, PartialNoJobs, 0), History{FlakeCount7d: 9}, ClassConfig, "P4", false},
		{"startup_failure, jobs unknown (budget) is not zero jobs", evOf("startup_failure", EvidencePartial, PartialBudget, 0), History{}, ClassUnknown, "P2", true},
		// Row 3
		{"transient: runner shutdown", full("The runner has received a shutdown signal"), History{}, ClassTransient, "P1", false},
		{"transient: conn reset", full("read tcp: connection reset by peer"), History{}, ClassTransient, "P1", false},
		{"transient: 502", full("fatal: unable to access: 502 Bad Gateway"), History{}, ClassTransient, "P1", false},
		{"transient: cache service", full("Failed to save cache: cache service responded with 503"), History{}, ClassTransient, "P1", false},
		{"transient: operation canceled without user cancel", full("##[error]The operation was canceled."), History{}, ClassTransient, "P1", false},
		{"transient: runner loss from job metadata, no text", runnerLost, History{}, ClassTransient, "P1", false},
		{"transient: runner loss metadata survives partial evidence", runnerLostPartial, History{}, ClassTransient, "P1", false},
		// Row 4
		{"flaky at 3", full("--- FAIL: TestX"), History{FlakeCount7d: 3}, ClassFlaky, "P3", false},
		{"flaky needs no log text", evOf("failure", EvidencePartial, PartialBudget, 1), History{FlakeCount7d: 3}, ClassFlaky, "P3", false},
		{"flake count 2 is not flaky", full("--- FAIL: TestX"), History{FlakeCount7d: 2}, ClassCode, "P2", false},
		{"infra beats flaky", full("connection reset by peer"), History{FlakeCount7d: 4}, ClassTransient, "P1", false},
		// Row 5
		{"code: go test", full("--- FAIL: TestFoo (0.00s)\nFAIL\tgithub.com/x/y"), History{}, ClassCode, "P2", false},
		{"code: ts type error", full("src/a.ts(3,5): error TS2322: Type 'string' is not assignable"), History{}, ClassCode, "P2", false},
		{"code: lint", full("golangci-lint run: 3 issues"), History{}, ClassCode, "P2", false},
		{"code: compile error", full("./main.go:4:2: undefined: foo"), History{}, ClassCode, "P2", false},
		// Partial evidence
		{"partial evidence never gives code_or_test", evOf("failure", EvidencePartial, PartialLogTruncated, 1, "--- FAIL: TestX"), History{}, ClassUnknown, "P2", true},
		{"partial evidence, text rules wait for full evidence", evOf("failure", EvidencePartial, PartialLogTruncated, 2, "API rate limit exceeded"), History{}, ClassUnknown, "P2", true},
		{"budget-partial, no text", evOf("failure", EvidencePartial, PartialBudget, 1), History{}, ClassUnknown, "P2", true},
		// Row 6
		{"unknown: nothing matched", full("something odd happened"), History{}, ClassUnknown, "P2", true},
		{"unknown: low confidence, 6 failed jobs and 2 inspected", evOf("failure", EvidenceFull, "", 6, "--- FAIL: TestX", "--- FAIL: TestY"), History{}, ClassUnknown, "P2", true},
		{"confidence ok at 3 failed jobs", evOf("failure", EvidenceFull, "", 3, "--- FAIL: TestX", "--- FAIL: TestY"), History{}, ClassCode, "P2", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.ev, tc.hist)
			if got.Class != tc.class || got.Protocol != tc.proto || got.ShadowOnly != tc.shad {
				t.Fatalf("got %+v, want class=%s protocol=%s shadow=%v", got, tc.class, tc.proto, tc.shad)
			}
			if got.Class == ClassUnknown && got.Route != "triage" {
				t.Errorf("unknown must record would-route triage, got %q", got.Route)
			}
		})
	}
}
