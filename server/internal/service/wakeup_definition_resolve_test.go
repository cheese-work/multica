package service

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func defUUID(n byte) pgtype.UUID { return pgtype.UUID{Bytes: [16]byte{15: n}, Valid: true} }

func mustPatch(t *testing.T, raw string) WakeupConfigPatch {
	t.Helper()
	p, err := DecodeWakeupConfigPatch([]byte(raw))
	if err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return p
}

func testDef(t *testing.T, scope WakeupScope, id pgtype.UUID, rev int64, raw string) *WakeupDefinition {
	t.Helper()
	return &WakeupDefinition{Scope: scope, ScopeID: id, Revision: rev, Patch: mustPatch(t, raw)}
}

func rootDef(t *testing.T, scope WakeupScope, id pgtype.UUID, rev int64, raw string) *WakeupDefinition {
	d := testDef(t, scope, id, rev, raw)
	d.Root = true
	return d
}

func mustResolve(t *testing.T, in WakeupResolveInput) EffectiveWakeupConfig {
	t.Helper()
	out, err := ResolveWakeupConfig(in)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return out
}

var (
	wsID      = defUUID(1)
	projectID = defUUID(2)
	otherProj = defUUID(3)
	issueID   = defUUID(4)
	customKey = "7f1d6a2e-3b44-4d8e-9a55-0c6b1f2e8d10"
)

func TestResolveBuiltinBaselineEnabledWithoutDefinitions(t *testing.T) {
	for _, key := range []string{SystemRuleChildDone, SystemRulePRMerged, SystemRulePRChecksFailed} {
		got := mustResolve(t, WakeupResolveInput{RuleKey: key})
		if !got.Applicable || !got.Enabled() {
			t.Fatalf("%s: applicable=%v enabled=%v, want both true", key, got.Applicable, got.Enabled())
		}
		if len(got.AggregateCaps) != 0 || got.Config.Mode.Set || got.Config.MaxFires.Set {
			t.Fatalf("%s: baseline must add no cap, mode or fire cap: %+v", key, got)
		}
		if got.Sources["enabled"] != WakeupScopeBuiltin {
			t.Fatalf("%s: enabled source = %q", key, got.Sources["enabled"])
		}
	}
}

func TestResolvePrecedenceBuiltinWorkspaceProjectIssue(t *testing.T) {
	in := WakeupResolveInput{
		RuleKey:        SystemRuleChildDone,
		IssueProjectID: projectID,
		Workspace:      testDef(t, WakeupScopeWorkspace, wsID, 3, `{"v":1,"instruction":"ws","mode":"once"}`),
		Project:        testDef(t, WakeupScopeProject, projectID, 5, `{"v":1,"instruction":"project"}`),
		Issue:          testDef(t, WakeupScopeIssue, issueID, 7, `{"v":1,"instruction":"issue"}`),
	}
	got := mustResolve(t, in)
	if got.Config.Instruction.Value != "issue" || got.Sources["instruction"] != WakeupScopeIssue {
		t.Fatalf("instruction = %q from %q, want issue", got.Config.Instruction.Value, got.Sources["instruction"])
	}
	if got.Config.Mode.Value != "once" || got.Sources["mode"] != WakeupScopeWorkspace {
		t.Fatalf("mode = %q from %q, want workspace once", got.Config.Mode.Value, got.Sources["mode"])
	}
	in.Issue = nil
	got = mustResolve(t, in)
	if got.Config.Instruction.Value != "project" || got.Sources["instruction"] != WakeupScopeProject {
		t.Fatalf("after reset instruction = %q from %q, want project", got.Config.Instruction.Value, got.Sources["instruction"])
	}
}

func TestResolveSparsePatchInheritsOmittedFields(t *testing.T) {
	in := WakeupResolveInput{
		RuleKey:        SystemRulePRMerged,
		IssueProjectID: projectID,
		Workspace:      testDef(t, WakeupScopeWorkspace, wsID, 1, `{"v":1,"instruction":"ws","mode":"continuous","max_fires":4,"rate_limit":6,"active_run":"defer"}`),
		Project:        testDef(t, WakeupScopeProject, projectID, 1, `{"v":1,"instruction":"project"}`),
	}
	got := mustResolve(t, in)
	c := got.Config
	if c.Instruction.Value != "project" || c.Mode.Value != "continuous" || c.MaxFires.Value != 4 || c.RateLimit.Value != 6 || c.ActiveRun.Value != "defer" {
		t.Fatalf("sparse override lost inherited fields: %+v", c)
	}
	// Instruction-only override adds no cap, mode, expiry or fire-cap defaults.
	in.Workspace = testDef(t, WakeupScopeWorkspace, wsID, 1, `{"v":1}`)
	got = mustResolve(t, in)
	if got.Config.Mode.Set || got.Config.MaxFires.Set || got.Config.Expiry.Set || len(got.AggregateCaps) != 0 {
		t.Fatalf("instruction-only override inserted defaults: %+v", got)
	}
}

func TestResolveExplicitFalseDisablesAndLaterScopeReenables(t *testing.T) {
	in := WakeupResolveInput{
		RuleKey:        SystemRuleChildDone,
		IssueProjectID: projectID,
		Workspace:      testDef(t, WakeupScopeWorkspace, wsID, 1, `{"v":1,"enabled":false}`),
	}
	if got := mustResolve(t, in); got.Enabled() {
		t.Fatal("workspace enabled=false must disable")
	}
	in.Project = testDef(t, WakeupScopeProject, projectID, 1, `{"v":1,"enabled":true}`)
	got := mustResolve(t, in)
	if !got.Enabled() || got.Sources["enabled"] != WakeupScopeProject {
		t.Fatalf("project override must re-enable: enabled=%v from %q", got.Enabled(), got.Sources["enabled"])
	}
	in.Issue = testDef(t, WakeupScopeIssue, issueID, 1, `{"v":1,"enabled":false}`)
	if got = mustResolve(t, in); got.Enabled() {
		t.Fatal("issue enabled=false must disable")
	}
}

func TestResolveExplicitNullClearsOptionalInheritedValue(t *testing.T) {
	in := WakeupResolveInput{
		RuleKey:        SystemRulePRChecksFailed,
		IssueProjectID: projectID,
		Workspace:      testDef(t, WakeupScopeWorkspace, wsID, 1, `{"v":1,"max_fires":3,"expiry":{"after":"72h"}}`),
		Project:        testDef(t, WakeupScopeProject, projectID, 1, `{"v":1,"max_fires":null,"expiry":null}`),
	}
	got := mustResolve(t, in)
	if got.Config.MaxFires.Set || got.Config.Expiry.Set {
		t.Fatalf("explicit null must clear inherited optional values: %+v", got.Config)
	}
	if _, ok := got.Sources["max_fires"]; ok {
		t.Fatal("cleared field must not report a source")
	}
}

func TestResolveNestedObjectsReplaceAtomically(t *testing.T) {
	in := WakeupResolveInput{
		RuleKey:        SystemRulePRMerged,
		IssueProjectID: projectID,
		Workspace:      testDef(t, WakeupScopeWorkspace, wsID, 1, `{"v":1,"filters":{"base_branch":"main","priorities":["high"]},"target":{"kind":"agent","id":"a"}}`),
		Project:        testDef(t, WakeupScopeProject, projectID, 1, `{"v":1,"filters":{"labels":["x"]}}`),
	}
	got := mustResolve(t, in)
	if string(got.Config.Filters.Value) != `{"labels":["x"]}` {
		t.Fatalf("filters merged recursively: %s", got.Config.Filters.Value)
	}
	if string(got.Config.Target.Value) != `{"kind":"agent","id":"a"}` || got.Sources["target"] != WakeupScopeWorkspace {
		t.Fatalf("untouched nested target not inherited: %s from %q", got.Config.Target.Value, got.Sources["target"])
	}
}

func TestResolveAggregateCapsAreScopeOwnedAndNeverRaiseAncestor(t *testing.T) {
	in := WakeupResolveInput{
		RuleKey:        SystemRulePRMerged,
		IssueProjectID: projectID,
		Workspace:      testDef(t, WakeupScopeWorkspace, wsID, 1, `{"v":1,"aggregate_limit":10}`),
		Project:        testDef(t, WakeupScopeProject, projectID, 1, `{"v":1,"aggregate_limit":6}`),
	}
	got := mustResolve(t, in)
	if len(got.AggregateCaps) != 2 || got.AggregateCaps[0].Scope != WakeupScopeWorkspace || got.AggregateCaps[0].Limit != 10 ||
		got.AggregateCaps[1].Scope != WakeupScopeProject || got.AggregateCaps[1].Limit != 6 || got.AggregateCaps[1].ScopeID != projectID {
		t.Fatalf("both caps must apply, ancestor first: %+v", got.AggregateCaps)
	}
	// A larger local cap does not replace the ancestor's: both stay in force.
	in.Project = testDef(t, WakeupScopeProject, projectID, 2, `{"v":1,"aggregate_limit":50}`)
	if got = mustResolve(t, in); len(got.AggregateCaps) != 2 || got.AggregateCaps[0].Limit != 10 {
		t.Fatalf("local cap must not raise ancestor cap: %+v", got.AggregateCaps)
	}
	// Clearing the local cap removes only that scope's cap.
	in.Project = testDef(t, WakeupScopeProject, projectID, 3, `{"v":1,"aggregate_limit":null}`)
	got = mustResolve(t, in)
	if len(got.AggregateCaps) != 1 || got.AggregateCaps[0].Scope != WakeupScopeWorkspace {
		t.Fatalf("clearing local cap must keep ancestor cap: %+v", got.AggregateCaps)
	}
	// Instruction-only override inherits the ancestor's cap, or none.
	in.Project = testDef(t, WakeupScopeProject, projectID, 4, `{"v":1,"instruction":"x"}`)
	if got = mustResolve(t, in); len(got.AggregateCaps) != 1 {
		t.Fatalf("instruction-only override changed caps: %+v", got.AggregateCaps)
	}
	in.Workspace = nil
	if got = mustResolve(t, in); len(got.AggregateCaps) != 0 {
		t.Fatalf("no ancestor cap must stay uncapped: %+v", got.AggregateCaps)
	}
}

func TestResolveCustomRuleRequiresRootAndOrphanOverrideIsInapplicable(t *testing.T) {
	root := rootDef(t, WakeupScopeProject, projectID, 1, `{"v":1,"enabled":true,"instruction":"custom","aggregate_limit":12}`)
	override := testDef(t, WakeupScopeIssue, issueID, 1, `{"v":1,"instruction":"local"}`)
	in := WakeupResolveInput{RuleKey: customKey, IssueProjectID: projectID, Project: root, Issue: override}
	got := mustResolve(t, in)
	if !got.Applicable || !got.Enabled() || got.Config.Instruction.Value != "local" || len(got.AggregateCaps) != 1 {
		t.Fatalf("custom rule with root: %+v", got)
	}

	// The issue moved to another project: the old project's root is no longer
	// loaded, so the override must not silently follow the issue.
	in = WakeupResolveInput{RuleKey: customKey, IssueProjectID: otherProj, Issue: override}
	got = mustResolve(t, in)
	if got.Applicable || got.Inapplicable != WakeupInapplicableNoRoot || got.Enabled() {
		t.Fatalf("orphan override must be inapplicable: %+v", got)
	}

	// Without any definition a custom key has no baseline either.
	got = mustResolve(t, WakeupResolveInput{RuleKey: customKey})
	if got.Applicable || got.Inapplicable != WakeupInapplicableNoRoot {
		t.Fatalf("custom key without root: %+v", got)
	}
}

func TestResolveCustomWorkspaceRootInheritedByProjectAndIssue(t *testing.T) {
	in := WakeupResolveInput{
		RuleKey:        customKey,
		IssueProjectID: projectID,
		Workspace:      rootDef(t, WakeupScopeWorkspace, wsID, 1, `{"v":1,"enabled":true,"instruction":"ws","max_fires":20}`),
		Project:        testDef(t, WakeupScopeProject, projectID, 1, `{"v":1,"enabled":false}`),
	}
	got := mustResolve(t, in)
	if !got.Applicable || got.Enabled() || got.Config.MaxFires.Value != 20 {
		t.Fatalf("project disable of workspace rule: %+v", got)
	}
	// Root absent at workspace but project override present: retired, not promoted.
	in.Workspace = nil
	if got = mustResolve(t, in); got.Applicable {
		t.Fatalf("override without root must not be promoted to a root: %+v", got)
	}
}

func TestResolveRejectsInconsistentInputs(t *testing.T) {
	cases := map[string]WakeupResolveInput{
		"builtin root":          {RuleKey: SystemRuleChildDone, Workspace: rootDef(t, WakeupScopeWorkspace, wsID, 1, `{"v":1}`)},
		"root below override":   {RuleKey: customKey, IssueProjectID: projectID, Workspace: testDef(t, WakeupScopeWorkspace, wsID, 1, `{"v":1}`), Project: rootDef(t, WakeupScopeProject, projectID, 1, `{"v":1}`)},
		"issue root":            {RuleKey: customKey, Issue: rootDef(t, WakeupScopeIssue, issueID, 1, `{"v":1}`)},
		"wrong scope slot":      {RuleKey: SystemRuleChildDone, Workspace: testDef(t, WakeupScopeProject, projectID, 1, `{"v":1}`)},
		"project not current":   {RuleKey: SystemRuleChildDone, IssueProjectID: otherProj, Project: testDef(t, WakeupScopeProject, projectID, 1, `{"v":1}`)},
		"project without issue": {RuleKey: SystemRuleChildDone, Project: testDef(t, WakeupScopeProject, projectID, 1, `{"v":1}`)},
		"empty rule key":        {},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ResolveWakeupConfig(in); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestFingerprintTracksRevisionsProjectAndTarget(t *testing.T) {
	base := func() WakeupResolveInput {
		return WakeupResolveInput{
			RuleKey:        SystemRulePRMerged,
			IssueProjectID: projectID,
			IssueTarget:    "agent:a",
			Workspace:      testDef(t, WakeupScopeWorkspace, wsID, 1, `{"v":1}`),
			Project:        testDef(t, WakeupScopeProject, projectID, 1, `{"v":1}`),
			Issue:          testDef(t, WakeupScopeIssue, issueID, 1, `{"v":1}`),
		}
	}
	ref := mustResolve(t, base()).Fingerprint
	if ref == "" || mustResolve(t, base()).Fingerprint != ref {
		t.Fatal("fingerprint must be non-empty and deterministic")
	}
	mutations := map[string]func(*WakeupResolveInput){
		"workspace revision":   func(in *WakeupResolveInput) { in.Workspace.Revision++ },
		"project revision":     func(in *WakeupResolveInput) { in.Project.Revision++ },
		"issue revision":       func(in *WakeupResolveInput) { in.Issue.Revision++ },
		"issue override reset": func(in *WakeupResolveInput) { in.Issue = nil },
		"project removed":      func(in *WakeupResolveInput) { in.Project = nil },
		"project moved": func(in *WakeupResolveInput) {
			in.IssueProjectID = otherProj
			in.Project = testDef(t, WakeupScopeProject, otherProj, 1, `{"v":1}`)
		},
		"target changed": func(in *WakeupResolveInput) { in.IssueTarget = "agent:b" },
		"rule key":       func(in *WakeupResolveInput) { in.RuleKey = SystemRulePRChecksFailed },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			in := base()
			mutate(&in)
			if got := mustResolve(t, in).Fingerprint; got == ref {
				t.Fatal("fingerprint did not change")
			}
		})
	}
	t.Run("patch content alone does not change it", func(t *testing.T) {
		in := base()
		in.Issue.Patch = mustPatch(t, `{"v":1,"instruction":"edited without a revision bump"}`)
		if got := mustResolve(t, in).Fingerprint; got != ref {
			t.Fatal("fingerprint is revision-based; content with the same revision must not differ")
		}
	})
}

func TestFingerprintFieldsAreUnambiguous(t *testing.T) {
	a := mustResolve(t, WakeupResolveInput{RuleKey: "ab", IssueTarget: "c"})
	b := mustResolve(t, WakeupResolveInput{RuleKey: "a", IssueTarget: "bc"})
	if a.Fingerprint == b.Fingerprint {
		t.Fatal("field boundary collided")
	}
}

func TestDecodePatchStrictness(t *testing.T) {
	bad := map[string]string{
		"missing version":  `{"instruction":"x"}`,
		"future version":   `{"v":2}`,
		"unknown field":    `{"v":1,"surprise":true}`,
		"wrong type":       `{"v":1,"enabled":"yes"}`,
		"not an object":    `[]`,
		"trailing garbage": `{"v":1} {"v":1}`,
		"object field":     `{"v":1,"filters":5}`,
	}
	for name, raw := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeWakeupConfigPatch([]byte(raw)); err == nil {
				t.Fatal("expected decode error")
			}
		})
	}
}

func TestPatchRoundTripKeepsAbsentNullAndValueDistinct(t *testing.T) {
	raw := `{"v":1,"enabled":false,"max_fires":null,"instruction":"hi","filters":{"a":1}}`
	p := mustPatch(t, raw)
	if !p.Enabled.Set || p.Enabled.Null || p.Enabled.Value || !p.MaxFires.Null || p.Mode.Set {
		t.Fatalf("decoded states wrong: %+v", p)
	}
	out, err := MarshalWakeupConfigPatch(p)
	if err != nil {
		t.Fatal(err)
	}
	var want, got map[string]any
	_ = json.Unmarshal([]byte(raw), &want)
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(slices.Sorted(maps.Keys(want)), slices.Sorted(maps.Keys(got))) || got["max_fires"] != nil {
		t.Fatalf("round trip changed shape: %s", out)
	}
	if _, present := got["mode"]; present {
		t.Fatalf("absent field must stay absent: %s", out)
	}
}
