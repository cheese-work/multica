package checkpoint

import (
	"testing"
	"time"
)

func TestToRowFromRow_RoundTrip(t *testing.T) {
	ts := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	cp := Checkpoint{
		IssueID:     "issue-1",
		AgentID:     "agent-1",
		IssueRev:    7,
		CandidateID: "sha-abc",
		Coverage: map[string]ThreadCoverage{
			"t1": {ThreadRootID: "t1", ReplyCount: 2, LastActivityAt: ts, Resolved: true},
		},
		AcceptedDecisions:   []string{"use table storage"},
		Obligations:         []Obligation{{Description: "wait for review", Kind: ObligationApproval, SourceThreadID: "t1"}},
		Blockers:            []string{"CI red"},
		NextPermittedAction: "ship it",
		Evidence:            []EvidenceLink{{Description: "CI run", Ref: "https://x", Identity: "native-ci"}},
		ResolvedThreads:     []ResolvedNote{{ThreadRootID: "t0", Resolution: "settled", EvidenceRef: "pr#1"}},
		BuiltAt:             ts,
	}

	row, err := cp.ToRow()
	if err != nil {
		t.Fatalf("ToRow failed: %v", err)
	}
	got, err := FromRow(cp.IssueID, cp.AgentID, row)
	if err != nil {
		t.Fatalf("FromRow failed: %v", err)
	}

	if got.IssueRev != cp.IssueRev || got.CandidateID != cp.CandidateID {
		t.Fatalf("identity mismatch: got %+v", got)
	}
	if len(got.Obligations) != 1 || got.Obligations[0].Description != "wait for review" {
		t.Fatalf("obligations not preserved: got %+v", got.Obligations)
	}
	if len(got.Blockers) != 1 || got.Blockers[0] != "CI red" {
		t.Fatalf("blockers not preserved: got %+v", got.Blockers)
	}
	if len(got.Evidence) != 1 || got.Evidence[0].Identity != "native-ci" {
		t.Fatalf("evidence not preserved: got %+v", got.Evidence)
	}
	if len(got.ResolvedThreads) != 1 || got.ResolvedThreads[0].Resolution != "settled" {
		t.Fatalf("resolved threads not preserved: got %+v", got.ResolvedThreads)
	}
	if len(got.Coverage) != 1 || got.Coverage["t1"].ReplyCount != 2 {
		t.Fatalf("coverage not preserved: got %+v", got.Coverage)
	}
	if !got.BuiltAt.Equal(ts) {
		t.Fatalf("built_at not preserved: got %v want %v", got.BuiltAt, ts)
	}
}

func TestFromRow_MalformedJSON_Fails(t *testing.T) {
	row := Row{Obligations: []byte("not json")}
	if _, err := FromRow("issue-1", "agent-1", row); err == nil {
		t.Fatal("expected malformed obligations JSON to fail the whole decode, not silently drop it")
	}
}

func TestBuildFromScan_CarriesOverContentContractFields(t *testing.T) {
	ts := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	carryOver := Checkpoint{
		Obligations:         []Obligation{{Description: "keep tests green", Kind: ObligationHumanBlock}},
		Blockers:            []string{"waiting on human"},
		NextPermittedAction: "wait",
	}
	live := []ThreadCoverage{
		{ThreadRootID: "t1", ReplyCount: 1, LastActivityAt: ts, Resolved: false},
	}

	next := BuildFromScan("issue-1", "agent-1", 9, "sha-new", live, carryOver)

	if next.IssueID != "issue-1" || next.AgentID != "agent-1" || next.IssueRev != 9 || next.CandidateID != "sha-new" {
		t.Fatalf("identity not set correctly: %+v", next)
	}
	if len(next.Coverage) != 1 || next.Coverage["t1"].ReplyCount != 1 {
		t.Fatalf("coverage cursor not built from live scan: %+v", next.Coverage)
	}
	if len(next.Obligations) != 1 || next.Obligations[0].Description != "keep tests green" {
		t.Fatalf("obligations not carried over: %+v", next.Obligations)
	}
	if len(next.Blockers) != 1 || next.Blockers[0] != "waiting on human" {
		t.Fatalf("blockers not carried over: %+v", next.Blockers)
	}
	if next.NextPermittedAction != "wait" {
		t.Fatalf("next permitted action not carried over: %q", next.NextPermittedAction)
	}
	if next.BuiltAt.IsZero() {
		t.Fatal("expected BuiltAt to be stamped")
	}
}

func TestBuildFromScan_ColdStart_NoCarryOver(t *testing.T) {
	ts := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	live := []ThreadCoverage{
		{ThreadRootID: "t1", ReplyCount: 0, LastActivityAt: ts, Resolved: false},
	}
	next := BuildFromScan("issue-1", "agent-1", 1, "", live, Checkpoint{})
	if len(next.Obligations) != 0 || len(next.Blockers) != 0 {
		t.Fatalf("expected no carry-over content on cold start, got %+v", next)
	}
	if len(next.Coverage) != 1 {
		t.Fatalf("expected coverage cursor built from live scan even on cold start: %+v", next.Coverage)
	}
}
