package issueworkflow

import (
	"strings"
	"testing"
)

// CHE-7668: the intake_simple and qa_proof entry guards of the Quick workflow.

func simpleIntake() *IntakeFacts {
	return &IntakeFacts{Lines: map[string]string{
		"Estimate": "1h", "Repos": "1", "Schema change": "no", "Security change": "no", "Human decision": "no",
	}}
}

func with(f *IntakeFacts, key, value string) *IntakeFacts {
	lines := map[string]string{}
	for k, v := range f.Lines {
		lines[k] = v
	}
	if value == "" {
		delete(lines, key)
	} else {
		lines[key] = value
	}
	return &IntakeFacts{Lines: lines, OpenDecision: f.OpenDecision}
}

func intakeGate(mode string) []EntryGate {
	return []EntryGate{{Guard: GuardIntakeSimple, Mode: mode, Param: "1"}}
}

func TestIntakeSimpleHoldsForASimpleBlock(t *testing.T) {
	for _, mode := range []string{EntryModeOff, EntryModeWarn, EntryModeBlock} {
		if got := EvaluateEntry(intakeGate(mode), Facts{Intake: simpleIntake()}); len(got) != 0 {
			t.Errorf("mode %s: a simple block must pass, failed %+v", mode, got)
		}
	}
}

// Each exclusion fails the gate in warn and block mode and never in off mode.
func TestIntakeSimpleEachExclusionTimesMode(t *testing.T) {
	base := simpleIntake()
	open := simpleIntake()
	open.OpenDecision = true
	exclusions := []struct {
		name  string
		facts *IntakeFacts
		fact  string
	}{
		{"missing Estimate", with(base, "Estimate", ""), "Estimate"},
		{"missing Repos", with(base, "Repos", ""), "Repos"},
		{"missing Schema change", with(base, "Schema change", ""), "Schema change"},
		{"missing Security change", with(base, "Security change", ""), "Security change"},
		{"missing Human decision", with(base, "Human decision", ""), "Human decision"},
		{"estimate over Param", with(base, "Estimate", "1.5h"), "over"},
		{"two repos", with(base, "Repos", "2"), "repo"},
		{"two repos by name", with(base, "Repos", "cheese-work/multica, cheese-work/multica-dotfiles"), "repo"},
		{"schema change yes", with(base, "Schema change", "yes"), "Schema change"},
		{"security change yes", with(base, "Security change", "Yes"), "Security change"},
		{"human decision yes", with(base, "Human decision", "yes"), "Human decision"},
		{"open decision request", open, "decision request"},
	}
	for _, ex := range exclusions {
		for _, mode := range []string{EntryModeOff, EntryModeWarn, EntryModeBlock} {
			got := EvaluateEntry(intakeGate(mode), Facts{Intake: ex.facts})
			if mode == EntryModeOff {
				if len(got) != 0 {
					t.Errorf("%s / off: the gate must not run, got %+v", ex.name, got)
				}
				continue
			}
			if len(got) != 1 || got[0].Guard != GuardIntakeSimple || got[0].Mode != mode {
				t.Errorf("%s / %s: want one %s failure in that mode, got %+v", ex.name, mode, GuardIntakeSimple, got)
				continue
			}
			if !strings.Contains(got[0].Fact, ex.fact) || got[0].Fix == "" {
				t.Errorf("%s / %s: fact %q should mention %q and carry a fix", ex.name, mode, got[0].Fact, ex.fact)
			}
		}
	}
}

func TestIntakeSimpleEstimateEdges(t *testing.T) {
	cases := map[string]bool{
		"1": true, "1h": true, "1 h": true, "1 hour": true, "0.5h": true, "45m": true, "30 min": true, "1.0": true,
		"1.01h": false, "61m": false, "2": false,
		"soon": false, "-1h": false, "0": false, "0h": false, "1d": false, "h": false, "1h 30m": false, "NaN": false, "": false,
	}
	for estimate, ok := range cases {
		got := EvaluateEntry(intakeGate(EntryModeBlock), Facts{Intake: with(simpleIntake(), "Estimate", estimate)})
		if estimate == "" { // an empty value is a missing line
			ok = false
		}
		if (len(got) == 0) != ok {
			t.Errorf("Estimate %q: passes=%v, want %v (%+v)", estimate, len(got) == 0, ok, got)
		}
	}
}

func TestIntakeSimpleMalformedValuesFailClosed(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"Repos", "many"}, {"Repos", "-1"}, {"Repos", "none, "}, {"Schema change", "maybe"}, {"Schema change", "n/a"},
		{"Security change", "nope"}, {"Human decision", "tbd"},
	} {
		got := EvaluateEntry(intakeGate(EntryModeBlock), Facts{Intake: with(simpleIntake(), tc.key, tc.value)})
		if len(got) != 1 || !strings.Contains(got[0].Fact, tc.key) {
			t.Errorf("%s=%q must fail naming the line, got %+v", tc.key, tc.value, got)
		}
	}
	// Facts that were not loaded fail closed.
	if got := EvaluateEntry(intakeGate(EntryModeBlock), Facts{}); len(got) != 1 {
		t.Errorf("unloaded facts must fail closed, got %+v", got)
	}
	if got := EvaluateEntry(intakeGate(EntryModeBlock), Facts{Intake: &IntakeFacts{Err: "comments unreadable"}}); len(got) != 1 || !strings.Contains(got[0].Fact, "unreadable") {
		t.Errorf("a failed read must fail closed with its reason, got %+v", got)
	}
}

func TestIntakeSimpleParamSetsTheEstimateCap(t *testing.T) {
	gate := []EntryGate{{Guard: GuardIntakeSimple, Mode: EntryModeBlock, Param: "2"}}
	if got := EvaluateEntry(gate, Facts{Intake: with(simpleIntake(), "Estimate", "2h")}); len(got) != 0 {
		t.Errorf("2h under Param 2 must pass: %+v", got)
	}
	if got := EvaluateEntry(gate, Facts{Intake: with(simpleIntake(), "Estimate", "2.5h")}); len(got) != 1 {
		t.Errorf("2.5h over Param 2 must fail: %+v", got)
	}
	// A gate without a usable Param cannot be evaluated: fail closed.
	for _, p := range []string{"", "x", "-1", "0"} {
		gate := []EntryGate{{Guard: GuardIntakeSimple, Mode: EntryModeBlock, Param: p}}
		if got := EvaluateEntry(gate, Facts{Intake: simpleIntake()}); len(got) != 1 {
			t.Errorf("Param %q must fail closed: %+v", p, got)
		}
	}
}

func TestIntakeCallFromEvaluation(t *testing.T) {
	if call, est := IntakeCall(EvaluateEntry(intakeGate(EntryModeBlock), Facts{Intake: simpleIntake()}), simpleIntake()); call != "simple" || est != "1h" {
		t.Errorf("simple call = %q %q", call, est)
	}
	bad := with(simpleIntake(), "Repos", "3")
	if call, est := IntakeCall(EvaluateEntry(intakeGate(EntryModeBlock), Facts{Intake: bad}), bad); call != "rejected" || est != "1h" {
		t.Errorf("rejected call = %q %q", call, est)
	}
	none := with(simpleIntake(), "Estimate", "")
	if _, est := IntakeCall(EvaluateEntry(intakeGate(EntryModeBlock), Facts{Intake: none}), none); est != "" {
		t.Errorf("a missing estimate reads empty, got %q", est)
	}
}

func TestParseIntakeLines(t *testing.T) {
	text := "Intake\n- **Estimate:** 45 min\n> Repos: cheese-work/multica\nschema change: no\n  Security change : No\nHuman decision: no\nNot a line Estimate: 9h\nEstimate: 2h\n"
	got := ParseIntakeLines(text)
	want := map[string]string{"Estimate": "2h", "Repos": "cheese-work/multica", "Schema change": "no", "Security change": "No", "Human decision": "no"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
}

func qaFacts(post string, bodies ...string) *QAFacts {
	return &QAFacts{Bodies: bodies, PostCloseWork: post}
}

func qaGate(mode string) []EntryGate { return []EntryGate{{Guard: GuardQAProof, Mode: mode}} }

func TestQAProofHolds(t *testing.T) {
	url := "Done.\nQA: https://example.com/run/1\n"
	fenced := "QA:\n```\nok  \tgithub.com/x/y\t0.4s\n```\n"
	inline := "QA: see below\n```\nPASS\n```"
	for _, body := range []string{url, fenced, inline} {
		for _, post := range []string{"none", "follow-up CHE-1 for the docs"} {
			if got := EvaluateEntry(qaGate(EntryModeBlock), Facts{QA: qaFacts(post, "unrelated", body)}); len(got) != 0 {
				t.Errorf("QA body %q post_close_work %q must pass: %+v", body, post, got)
			}
		}
	}
}

func TestQAProofEachFailureTimesMode(t *testing.T) {
	good := "QA: https://example.com/run/1"
	cases := []struct {
		name  string
		facts *QAFacts
		fact  string
	}{
		{"no comment since entry", qaFacts("none"), "QA"},
		{"no QA line", qaFacts("none", "all done"), "QA"},
		{"QA line without proof", qaFacts("none", "QA: passed, trust me"), "QA"},
		{"QA with an empty fence", qaFacts("none", "QA:\n```\n```"), "QA"},
		{"QA with an unclosed fence", qaFacts("none", "QA:\n```\nok"), "QA"},
		{"QA url not http", qaFacts("none", "QA: ftp://example.com/x"), "QA"},
		{"post_close_work missing", qaFacts("", good), "post_close_work"},
		{"post_close_work blank", qaFacts("   ", good), "post_close_work"},
		{"facts not loaded", nil, "not loaded"},
		{"read failed", &QAFacts{Err: "comments unreadable"}, "unreadable"},
	}
	for _, c := range cases {
		for _, mode := range []string{EntryModeOff, EntryModeWarn, EntryModeBlock} {
			got := EvaluateEntry(qaGate(mode), Facts{QA: c.facts})
			if mode == EntryModeOff {
				if len(got) != 0 {
					t.Errorf("%s / off: must not run, got %+v", c.name, got)
				}
				continue
			}
			if len(got) != 1 || got[0].Guard != GuardQAProof || got[0].Mode != mode || !strings.Contains(got[0].Fact, c.fact) || got[0].Fix == "" {
				t.Errorf("%s / %s: got %+v, want a failure mentioning %q", c.name, mode, got, c.fact)
			}
		}
	}
}

func TestEntryGateParamValidationAndStorage(t *testing.T) {
	ok := func(g EntryGate) bool { return validateEntry(Step{Entry: []EntryGate{g}}) == nil }
	if !ok(EntryGate{Guard: GuardIntakeSimple, Mode: EntryModeBlock, Param: "1"}) || !ok(EntryGate{Guard: GuardIntakeSimple, Mode: EntryModeOff, Param: "0.5"}) {
		t.Fatal("intake_simple with a positive hour Param must validate")
	}
	if !ok(EntryGate{Guard: GuardQAProof, Mode: EntryModeWarn}) {
		t.Fatal("qa_proof without Param must validate")
	}
	for _, bad := range []EntryGate{
		{Guard: GuardIntakeSimple, Mode: EntryModeBlock},
		{Guard: GuardIntakeSimple, Mode: EntryModeBlock, Param: "abc"},
		{Guard: GuardIntakeSimple, Mode: EntryModeBlock, Param: "0"},
		{Guard: GuardIntakeSimple, Mode: EntryModeBlock, Param: "-1"},
		{Guard: GuardIntakeSimple, Mode: EntryModeBlock, Param: "1h"},
		{Guard: GuardIntakeSimple, Mode: EntryModeBlock, Param: "100"},
		{Guard: GuardQAProof, Mode: EntryModeBlock, Param: "1"},
		{Guard: "pr.ready", Mode: EntryModeBlock, Param: "1"},
	} {
		if ok(bad) {
			t.Errorf("%+v must be rejected", bad)
		}
	}
	// They are entry gates: an edge cannot use them.
	for _, g := range []string{GuardIntakeSimple, GuardQAProof} {
		step := Step{StatusKey: "todo", Edges: []Edge{{To: "done", Guard: g}}}
		if validateEdges(step, Definition{Steps: []Step{step, {StatusKey: "done"}}}) == nil {
			t.Errorf("%s must be rejected as an edge guard", g)
		}
	}
	norm := normalizeEntry([]EntryGate{{Guard: " intake_simple ", Mode: "block", Param: " 1 "}})
	if norm[0].Param != "1" {
		t.Fatalf("normalize must trim and keep Param: %+v", norm[0])
	}
	raw, err := EncodeSteps([]Step{{StatusKey: "in_progress", Entry: norm}})
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeSteps(raw)
	if err != nil || back[0].Entry[0].Param != "1" {
		t.Fatalf("Param lost in storage: %+v %v", back, err)
	}
	// Existing gates store no param key.
	if raw, _ := EncodeSteps([]Step{{StatusKey: "in_review", Entry: normalizeEntry(gates(EntryModeBlock, "pr.ready"))}}); strings.Contains(string(raw), "param") {
		t.Fatalf("a gate without Param must not store one: %s", raw)
	}
}

func TestMemberOverrideSurvivesOnNewGates(t *testing.T) {
	no := false
	got := EvaluateEntry([]EntryGate{{Guard: GuardQAProof, Mode: EntryModeBlock, MemberOverride: &no}}, Facts{QA: qaFacts("")})
	if len(got) != 1 || got[0].MemberOverride {
		t.Fatalf("member_override=false must reach the failure: %+v", got)
	}
	got = EvaluateEntry(intakeGate(EntryModeBlock), Facts{Intake: with(simpleIntake(), "Repos", "2")})
	if len(got) != 1 || !got[0].MemberOverride {
		t.Fatalf("member_override defaults to true: %+v", got)
	}
}
