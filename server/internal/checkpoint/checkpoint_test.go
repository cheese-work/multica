package checkpoint

import (
	"testing"
	"time"
)

func TestDiff_UnchangedThread_NotReported(t *testing.T) {
	ts := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	cp := Checkpoint{
		Coverage: map[string]ThreadCoverage{
			"t1": {ThreadRootID: "t1", ReplyCount: 3, LastActivityAt: ts, Resolved: true},
		},
	}
	live := []ThreadCoverage{
		{ThreadRootID: "t1", ReplyCount: 3, LastActivityAt: ts, Resolved: true},
	}
	changed, stale := Diff(cp, live)
	if len(changed) != 0 || stale {
		t.Fatalf("expected no change, got changed=%v stale=%v", changed, stale)
	}
}

func TestDiff_NewReplyInvalidatesThread(t *testing.T) {
	ts := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	later := ts.Add(time.Hour)
	cp := Checkpoint{
		Coverage: map[string]ThreadCoverage{
			"t1": {ThreadRootID: "t1", ReplyCount: 3, LastActivityAt: ts, Resolved: false},
		},
	}
	live := []ThreadCoverage{
		{ThreadRootID: "t1", ReplyCount: 4, LastActivityAt: later, Resolved: false},
	}
	changed, stale := Diff(cp, live)
	if len(changed) != 1 || changed[0] != "t1" || !stale {
		t.Fatalf("expected t1 changed and stale, got changed=%v stale=%v", changed, stale)
	}
}

func TestDiff_NewlyResolvedThreadInvalidates(t *testing.T) {
	ts := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	cp := Checkpoint{
		Coverage: map[string]ThreadCoverage{
			"t1": {ThreadRootID: "t1", ReplyCount: 3, LastActivityAt: ts, Resolved: false},
		},
	}
	live := []ThreadCoverage{
		{ThreadRootID: "t1", ReplyCount: 3, LastActivityAt: ts, Resolved: true},
	}
	changed, stale := Diff(cp, live)
	if len(changed) != 1 || !stale {
		t.Fatalf("expected resolution flip to invalidate thread, got changed=%v stale=%v", changed, stale)
	}
}

func TestDiff_UncoveredThreadReportedChanged(t *testing.T) {
	ts := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	cp := Checkpoint{Coverage: map[string]ThreadCoverage{}}
	live := []ThreadCoverage{
		{ThreadRootID: "new-thread", ReplyCount: 0, LastActivityAt: ts, Resolved: false},
	}
	changed, stale := Diff(cp, live)
	if len(changed) != 1 || changed[0] != "new-thread" || !stale {
		t.Fatalf("expected new-thread reported as changed, got changed=%v stale=%v", changed, stale)
	}
}

func TestDiff_ThreadMissingFromLive_NotReported(t *testing.T) {
	// Comments are never deleted in this system, so a checkpoint-covered
	// thread vanishing from a live scan should not happen; if it does, the
	// diff must not treat it as a staleness signal.
	ts := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	cp := Checkpoint{
		Coverage: map[string]ThreadCoverage{
			"t1": {ThreadRootID: "t1", ReplyCount: 1, LastActivityAt: ts, Resolved: true},
		},
	}
	changed, stale := Diff(cp, nil)
	if len(changed) != 0 || stale {
		t.Fatalf("expected no false-positive staleness, got changed=%v stale=%v", changed, stale)
	}
}

func TestCandidateChanged(t *testing.T) {
	cp := Checkpoint{IssueRev: 5, CandidateID: "sha-a"}
	if CandidateChanged(cp, 5, "sha-a") {
		t.Fatal("expected unchanged")
	}
	if !CandidateChanged(cp, 6, "sha-a") {
		t.Fatal("expected issue rev change to invalidate")
	}
	if !CandidateChanged(cp, 5, "sha-b") {
		t.Fatal("expected candidate change to invalidate")
	}
}

func TestEvaluateAgainstScan_CandidateChanged_ExpandsEverything(t *testing.T) {
	ts := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	cp := Checkpoint{
		IssueRev:    5,
		CandidateID: "sha-a",
		Coverage: map[string]ThreadCoverage{
			"t1": {ThreadRootID: "t1", ReplyCount: 1, LastActivityAt: ts, Resolved: true},
		},
	}
	live := []ThreadCoverage{
		{ThreadRootID: "t1", ReplyCount: 1, LastActivityAt: ts, Resolved: true},
		{ThreadRootID: "t2", ReplyCount: 0, LastActivityAt: ts, Resolved: false},
	}
	changed, stale := EvaluateAgainstScan(cp, 5, "sha-b", live)
	if !stale || len(changed) != 2 {
		t.Fatalf("expected full expansion on candidate change, got changed=%v stale=%v", changed, stale)
	}
}

func TestEvaluateAgainstScan_NoCandidateChange_UsesPerThreadDiff(t *testing.T) {
	ts := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	cp := Checkpoint{
		IssueRev:    5,
		CandidateID: "sha-a",
		Coverage: map[string]ThreadCoverage{
			"t1": {ThreadRootID: "t1", ReplyCount: 1, LastActivityAt: ts, Resolved: true},
		},
	}
	live := []ThreadCoverage{
		{ThreadRootID: "t1", ReplyCount: 1, LastActivityAt: ts, Resolved: true},
	}
	changed, stale := EvaluateAgainstScan(cp, 5, "sha-a", live)
	if stale || len(changed) != 0 {
		t.Fatalf("expected no staleness, got changed=%v stale=%v", changed, stale)
	}
}

func TestRender_NeverTruncatesObligations(t *testing.T) {
	cp := Checkpoint{
		IssueID: "issue-1",
		AgentID: "agent-1",
		Obligations: []Obligation{
			{Description: "wait for human approval before merge", Kind: ObligationApproval, SourceThreadID: "t1"},
			{Description: "user asked to keep tests green", Kind: ObligationHumanBlock, SourceThreadID: "t2"},
			{Description: "follow up on flaky CI", Kind: ObligationGeneral, SourceThreadID: "t3"},
		},
	}
	out := Render(cp, nil)
	for _, o := range cp.Obligations {
		if !contains(out, o.Description) {
			t.Fatalf("expected obligation %q to be rendered in full, got:\n%s", o.Description, out)
		}
	}
	if !contains(out, "[approval restriction]") {
		t.Fatal("expected approval restriction to be labeled distinctly")
	}
}

func TestRender_ListsChangedThreadsForExpansion(t *testing.T) {
	cp := Checkpoint{IssueID: "issue-1"}
	out := Render(cp, []string{"thread-a", "thread-b"})
	if !contains(out, "thread-a") || !contains(out, "thread-b") {
		t.Fatalf("expected changed threads listed, got:\n%s", out)
	}
}

func TestRender_EmptyIssueID_ReturnsEmpty(t *testing.T) {
	if got := Render(Checkpoint{}, nil); got != "" {
		t.Fatalf("expected empty render for zero-value checkpoint, got %q", got)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
