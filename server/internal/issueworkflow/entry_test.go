package issueworkflow

import (
	"reflect"
	"strings"
	"testing"
)

func gates(mode string, guards ...string) []EntryGate {
	out := make([]EntryGate, len(guards))
	for i, g := range guards {
		out[i] = EntryGate{Guard: g, Mode: mode}
	}
	return out
}

func guardNames(f []EntryFailure) []string {
	var out []string
	for _, e := range f {
		out = append(out, e.Guard)
	}
	return out
}

func TestEvaluateEntryOpenPRFamilyIsPerPR(t *testing.T) {
	red := PRFact{State: "open", HeadFresh: true, ChecksRollup: "FAILURE", HeadSHA: "abcdef123456"}
	greenDraft := PRFact{State: "draft", HeadFresh: true, ChecksRollup: "SUCCESS"}
	got := EvaluateEntry(gates(EntryModeBlock, "pr.ready", "pr.checks_green"), Facts{PRs: []PRFact{red, greenDraft}})
	if want := []string{"pr.ready", "pr.checks_green"}; !reflect.DeepEqual(guardNames(got), want) {
		t.Fatalf("a red PR must not hide behind a green draft: failed %v, want %v", guardNames(got), want)
	}
	if !strings.Contains(got[1].Fact, "FAILURE") || !strings.Contains(got[1].Fact, "abcdef1") || got[1].Fix == "" {
		t.Fatalf("failure should name the checks and head and carry a fix: %+v", got[1])
	}
	// A draft-only issue fails pr.ready per PR, not "no PR".
	if got := EvaluateEntry(gates(EntryModeBlock, "pr.ready"), Facts{PRs: []PRFact{greenDraft}}); len(got) != 1 || !strings.Contains(got[0].Fact, "draft") {
		t.Fatalf("draft PR on pr.ready: %+v", got)
	}
}

func TestEvaluateEntryNeedsAnOpenPR(t *testing.T) {
	merged := Facts{PRs: []PRFact{{State: "merged"}}}
	for _, g := range []string{"pr.ready", "pr.checks_green", "pr.checks_red", "pr.review_approved", "pr.review_changes_requested"} {
		if got := EvaluateEntry(gates(EntryModeBlock, g), merged); len(got) != 1 {
			t.Errorf("%s with only a merged PR: failed %v, want itself", g, guardNames(got))
		}
		if got := EvaluateEntry(gates(EntryModeBlock, g), Facts{}); len(got) != 1 {
			t.Errorf("%s with no PR: failed %v, want itself", g, guardNames(got))
		}
	}
}

func TestEvaluateEntryTerminalAndOtherUseFactsHolds(t *testing.T) {
	if got := EvaluateEntry(gates(EntryModeBlock, "pr.merged"), Facts{PRs: []PRFact{{State: "merged"}}}); len(got) != 0 {
		t.Errorf("merged PR on pr.merged: %v", got)
	}
	if got := EvaluateEntry(gates(EntryModeBlock, "pr.merged"), Facts{PRs: []PRFact{{State: "merged"}, {State: "open"}}}); len(got) != 1 {
		t.Errorf("an open PR beside a merged one must fail pr.merged")
	}
	if got := EvaluateEntry(gates(EntryModeBlock, "pr.closed_unmerged"), Facts{PRs: []PRFact{{State: "closed"}}}); len(got) != 0 {
		t.Errorf("closed PR on pr.closed_unmerged: %v", got)
	}
	if got := EvaluateEntry(gates(EntryModeBlock, "children_done"), Facts{}); len(got) != 1 {
		t.Errorf("children_done without done children must fail")
	}
	if got := EvaluateEntry(gates(EntryModeBlock, "issue_status(MUL-1,done)"), Facts{IssueStatuses: map[string]string{"MUL-1": "todo"}}); len(got) != 1 || !strings.Contains(got[0].Fact, "todo") {
		t.Errorf("issue_status gate: %+v", got)
	}
}

func TestEvaluateEntrySkipsOffAndKeepsOrderAndMode(t *testing.T) {
	in := []EntryGate{
		{Guard: "pr.ready", Mode: EntryModeOff},
		{Guard: "pr.merged", Mode: EntryModeWarn},
		{Guard: "children_done", Mode: EntryModeBlock},
	}
	got := EvaluateEntry(in, Facts{})
	if want := []string{"pr.merged", "children_done"}; !reflect.DeepEqual(guardNames(got), want) {
		t.Fatalf("failed %v, want %v", guardNames(got), want)
	}
	if got[0].Mode != EntryModeWarn || got[1].Mode != EntryModeBlock || !got[0].MemberOverride {
		t.Fatalf("modes or override lost: %+v", got)
	}
}

func TestEntryMemberOverrideDefaultsToTrue(t *testing.T) {
	no := false
	if !(EntryGate{}).Overridable() || (EntryGate{MemberOverride: &no}).Overridable() {
		t.Fatal("nil member_override means true, false means false")
	}
	norm := normalizeEntry([]EntryGate{{Guard: " pr.ready ", Mode: " block "}})
	if norm[0].Guard != "pr.ready" || norm[0].Mode != "block" || norm[0].MemberOverride == nil || !*norm[0].MemberOverride {
		t.Fatalf("normalize = %+v", norm[0])
	}
	if normalizeEntry(nil) != nil {
		t.Fatal("omitted entry must stay nil so a save keeps the stored gates")
	}
	if got := normalizeEntry([]EntryGate{}); got == nil || len(got) != 0 {
		t.Fatalf("empty entry must stay empty (clears): %#v", got)
	}
}

func TestValidateEntry(t *testing.T) {
	ok := func(g ...EntryGate) bool { return validateEntry(Step{Entry: g}) == nil }
	if !ok(gates(EntryModeBlock, "pr.ready", "children_done", "pr.merged")...) {
		t.Fatal("catalog fact guards must validate")
	}
	for _, bad := range []EntryGate{
		{Guard: "agent_done", Mode: EntryModeBlock},
		{Guard: "agent_verdict(approved)", Mode: EntryModeBlock},
		{Guard: "webhook(ci)", Mode: EntryModeBlock},
		{Guard: "nope", Mode: EntryModeBlock},
		{Guard: "pr.ready", Mode: "sometimes"},
		{Guard: "pr.ready"},
	} {
		if ok(bad) {
			t.Errorf("%+v must be rejected", bad)
		}
	}
	if ok(gates(EntryModeBlock, "pr.ready", "pr.ready", "pr.ready", "pr.ready", "pr.ready", "pr.ready", "pr.ready", "pr.ready", "pr.ready")...) {
		t.Error("more than 8 gates must be rejected")
	}
}

func TestEntryRoundTripsThroughStoredSteps(t *testing.T) {
	no := false
	steps := []Step{{StatusKey: "in_review", Entry: []EntryGate{{Guard: "pr.ready", Mode: EntryModeBlock, MemberOverride: &no}}}}
	raw, err := EncodeSteps(steps)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeSteps(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 1 || len(back[0].Entry) != 1 || back[0].Entry[0].Overridable() || back[0].Entry[0].Guard != "pr.ready" {
		t.Fatalf("entry lost in storage: %+v", back)
	}
}
