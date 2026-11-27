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
	if got := EvaluateEntry(gates(EntryModeBlock, "webhook(ci)"), Facts{Webhooks: map[string]bool{"ci": true}}); len(got) != 0 {
		t.Errorf("received webhook on webhook(ci): %v", got)
	}
	if got := EvaluateEntry(gates(EntryModeBlock, "webhook(ci)"), Facts{}); len(got) != 1 {
		t.Errorf("webhook(ci) without a received webhook must fail")
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
	if !ok(gates(EntryModeBlock, "pr.ready", "children_done", "pr.merged", "webhook(ci)")...) {
		t.Fatal("catalog fact guards, webhook included, must validate")
	}
	if !ok(EntryGate{Guard: "webhook(ci)", Mode: EntryModeOff}) {
		t.Fatal("webhook in mode off must validate")
	}
	if ok(EntryGate{Guard: "agent_done", Mode: EntryModeOff}) {
		t.Fatal("agent_done must be rejected even in mode off")
	}
	for _, bad := range []EntryGate{
		{Guard: "agent_done", Mode: EntryModeBlock},
		{Guard: "agent_verdict(approved)", Mode: EntryModeBlock},
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

// CHE-6831: a child issue must not enter a step that only root issues leave.
func TestEvaluateEntryRootIssueRefusesAChild(t *testing.T) {
	if got := EvaluateEntry(gates(EntryModeBlock, "root_issue"), Facts{}); len(got) != 0 {
		t.Fatalf("a root issue must pass root_issue: %v", guardNames(got))
	}
	got := EvaluateEntry(gates(EntryModeBlock, "root_issue"), Facts{HasParent: true})
	if len(got) != 1 || !strings.Contains(got[0].Fact, "parent") || got[0].Fix == "" {
		t.Fatalf("a child must fail root_issue with a parent fact and a fix: %+v", got)
	}
	if err := validateEntry(Step{StatusKey: "investigating", Entry: gates(EntryModeBlock, "root_issue")}); err != nil {
		t.Fatalf("root_issue is a valid entry gate: %v", err)
	}
}

// pr.none_open (CHE-8771): an agent may close an issue only when no open or
// draft PR is linked. Merged and closed PRs do not hold it back.
func TestEvaluateEntryPRNoneOpen(t *testing.T) {
	cases := []struct {
		name string
		prs  []PRFact
		want bool // the gate holds
	}{
		{"no PR", nil, true},
		{"merged only", []PRFact{{State: "merged"}}, true},
		{"closed only", []PRFact{{State: "closed"}}, true},
		{"merged and closed", []PRFact{{State: "merged"}, {State: "closed"}}, true},
		{"open", []PRFact{{State: "open", Ref: "cheese-work/multica#12"}}, false},
		{"draft", []PRFact{{State: "draft", Ref: "cheese-work/multica#13"}}, false},
		{"open beside merged", []PRFact{{State: "merged"}, {State: "open", Ref: "cheese-work/multica#14"}}, false},
	}
	for _, c := range cases {
		got := EvaluateEntry(gates(EntryModeBlock, "pr.none_open"), Facts{PRs: c.prs})
		if holds := len(got) == 0; holds != c.want {
			t.Errorf("%s: gate holds = %v, want %v (failed %+v)", c.name, holds, c.want, got)
		}
	}
}

func TestPRNoneOpenRefusalNamesEveryOpenPR(t *testing.T) {
	got := EvaluateEntry(gates(EntryModeBlock, "pr.none_open"), Facts{PRs: []PRFact{
		{State: "open", Ref: "cheese-work/multica#12"},
		{State: "merged", Ref: "cheese-work/multica#9"},
		{State: "draft", Ref: "cheese-work/multica#13"},
	}})
	if len(got) != 1 {
		t.Fatalf("want one failure, got %+v", got)
	}
	for _, want := range []string{"cheese-work/multica#12", "cheese-work/multica#13"} {
		if !strings.Contains(got[0].Fact, want) {
			t.Errorf("fact %q must name %s", got[0].Fact, want)
		}
	}
	if strings.Contains(got[0].Fact, "#9") {
		t.Errorf("fact %q must not name the merged PR", got[0].Fact)
	}
	if got[0].Fix == "" || got[0].Fix == "make the condition hold" {
		t.Errorf("fix should say what to do, got %q", got[0].Fix)
	}
}

func TestPRNoneOpenIsAnEntryGateNotAnEdgeGuard(t *testing.T) {
	if err := validateEntry(Step{StatusKey: "agent_accepted", Entry: gates(EntryModeBlock, "pr.none_open")}); err != nil {
		t.Fatalf("pr.none_open must validate as an entry gate: %v", err)
	}
}

func TestParseGuardPluginTakesPluginAndHookKey(t *testing.T) {
	g, err := ParseGuard("plugin(com.example.guard, qa-proof)")
	if err != nil || g.Kind != GuardPlugin || len(g.Args) != 2 || g.Args[0] != "com.example.guard" || g.Args[1] != "qa-proof" {
		t.Fatalf("ParseGuard = %+v, %v", g, err)
	}
	for _, bad := range []string{"plugin", "plugin(a)", "plugin(a, b, c)", "plugin(a, )"} {
		if _, err := ParseGuard(bad); err == nil {
			t.Errorf("ParseGuard(%q) accepted a malformed plugin guard", bad)
		}
	}
}

func TestPluginGuardIsAnEntryGateNotAnEdgeGuard(t *testing.T) {
	def := devDefinition()
	step := def.Steps[0]
	step.Edges = []Edge{{To: def.Steps[1].StatusKey, Guard: "plugin(com.example.guard, qa-proof)"}}
	if err := validateEdges(step, def); err == nil || !strings.Contains(err.Error(), "entry gate") {
		t.Fatalf("validateEdges = %v, want a refusal naming the entry gate", err)
	}
	step.Edges = nil
	step.Entry = gates(EntryModeBlock, "plugin(com.example.guard, qa-proof)")
	if err := validateEntry(step); err != nil {
		t.Fatalf("validateEntry refused a plugin entry gate: %v", err)
	}
}

func TestEvaluateEntryPluginGateFollowsTheVerdict(t *testing.T) {
	const guard = "plugin(com.example.guard, qa-proof)"
	key := PluginGateKey(Guard{Kind: GuardPlugin, Args: []string{"com.example.guard", "qa-proof"}})
	allow := Facts{Plugin: map[string]PluginVerdict{key: {Allow: true}}}
	if got := EvaluateEntry(gates(EntryModeBlock, guard), allow); len(got) != 0 {
		t.Fatalf("an allow verdict failed the gate: %+v", got)
	}
	deny := Facts{Plugin: map[string]PluginVerdict{key: {Reason: "no QA line", Overridable: true}}}
	got := EvaluateEntry(gates(EntryModeBlock, guard), deny)
	if len(got) != 1 || got[0].Guard != guard || got[0].Mode != EntryModeBlock || !strings.Contains(got[0].Fact, "no QA line") || !got[0].MemberOverride || got[0].Fix == "" {
		t.Fatalf("deny verdict: %+v", got)
	}
	// The hook's own overridable flag governs the override, and the gate's
	// member_override can still turn it off.
	deny.Plugin[key] = PluginVerdict{Reason: "no QA line", Overridable: false}
	if got := EvaluateEntry(gates(EntryModeBlock, guard), deny); len(got) != 1 || got[0].MemberOverride {
		t.Fatalf("a non-overridable hook must not allow an override: %+v", got)
	}
	// warn mode still reports the failure; off skips it.
	if got := EvaluateEntry(gates(EntryModeWarn, guard), deny); len(got) != 1 || got[0].Mode != EntryModeWarn {
		t.Fatalf("warn mode: %+v", got)
	}
	if got := EvaluateEntry(gates(EntryModeOff, guard), deny); len(got) != 0 {
		t.Fatalf("off mode: %+v", got)
	}
	// No verdict at all fails closed.
	if got := EvaluateEntry(gates(EntryModeBlock, guard), Facts{}); len(got) != 1 {
		t.Fatalf("missing verdict must fail the gate: %+v", got)
	}
}
