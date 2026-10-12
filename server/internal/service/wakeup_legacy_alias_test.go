package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var legacyTestWorkspace = parseTestUUIDRaw("33333333-3333-4333-8333-333333333333")

func aliasDef(t *testing.T, settings, key string) *WakeupDefinition {
	t.Helper()
	def, err := WorkspaceWakeupDefinition(legacyTestWorkspace, nil, []byte(settings), key)
	if err != nil {
		t.Fatalf("%s %s: %v", key, settings, err)
	}
	return def
}

// The four settings are canonical aliases: resolving the projected workspace
// definition must give exactly the legacy readers' answer for every shape of
// settings, including missing, null, non-boolean and unreadable values.
func TestLegacyWorkspaceAliasesMatchLegacyReaders(t *testing.T) {
	shapes := []string{
		``, `{}`, `null`, `not json`, `{"other":1}`,
		`{"github_wake_on_pr_merge":true}`, `{"github_wake_on_pr_merge":false}`, `{"github_wake_on_pr_merge":null}`,
		`{"github_wake_on_ci_failure":true}`, `{"github_wake_on_ci_failure":false}`,
		`{"github_wake_on_pr_merge":false,"github_wake_on_ci_failure":true}`,
		`{"github_wake_on_pr_merge":"false"}`, `{"github_wake_on_ci_failure":0}`,
		`{"system_wakeup_child_done":false}`, `{"system_wakeup_child_done":true}`, `{"system_wakeup_child_done":"false"}`,
		`{"system_wakeup_child_done":null}`, `{"system_wakeup_child_done":"no"}`,
		`{"system_wakeup_child_done_instruction":"  go  "}`, `{"system_wakeup_child_done_instruction":""}`, `{"system_wakeup_child_done_instruction":7}`,
		`{"system_wakeup_child_done":false,"system_wakeup_child_done_instruction":"x","github_wake_on_pr_merge":false}`,
	}
	for _, settings := range shapes {
		for _, key := range []string{SystemRulePRMerged, SystemRulePRChecksFailed, SystemRuleChildDone} {
			t.Run(key+"/"+settings, func(t *testing.T) {
				def, err := WorkspaceWakeupDefinition(legacyTestWorkspace, nil, []byte(settings), key)
				var (
					wantEnabled     bool
					wantErr         error
					wantInstruction string
				)
				if key == SystemRuleChildDone {
					wantEnabled, wantInstruction = SystemWakeupDefault([]byte(settings))
				} else {
					wantEnabled, wantErr = PRWakeupEnabled([]byte(settings), key)
				}
				if wantErr != nil {
					if !errors.Is(err, errMalformedPRWakeupSettings) {
						t.Fatalf("malformed settings error = %v, want errMalformedPRWakeupSettings", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				got, err := ResolveWakeupConfig(WakeupResolveInput{RuleKey: key, Workspace: def})
				if err != nil {
					t.Fatal(err)
				}
				if got.Enabled() != wantEnabled {
					t.Fatalf("resolved enabled = %v, legacy = %v", got.Enabled(), wantEnabled)
				}
				if got.Config.Instruction.Value != wantInstruction {
					t.Fatalf("resolved instruction = %q, legacy = %q", got.Config.Instruction.Value, wantInstruction)
				}
			})
		}
	}
}

// github_enabled=false stays an integration veto at every scope: it is not an
// alias, so the projection ignores it and the legacy PR reader still applies it.
func TestLegacyWorkspaceAliasLeavesGitHubMasterSwitchAsVeto(t *testing.T) {
	settings := `{"github_enabled":false,"github_wake_on_pr_merge":true}`
	if enabled, err := PRWakeupEnabled([]byte(settings), SystemRulePRMerged); err != nil || enabled {
		t.Fatalf("legacy reader with master off = %v %v, want disabled", enabled, err)
	}
	def := aliasDef(t, settings, SystemRulePRMerged)
	if def == nil || !def.Patch.Enabled.Set || !def.Patch.Enabled.Value {
		t.Fatalf("alias projection must carry only the wake_on_pr_merge value: %+v", def)
	}
}

func TestLegacyWorkspaceAliasIgnoresUnrelatedAndCustomKeys(t *testing.T) {
	if def := aliasDef(t, `{"github_wake_on_pr_merge":false}`, "7f1d6a2e-3b44-4d8e-9a55-0c6b1f2e8d10"); def != nil {
		t.Fatalf("a custom rule key must not read legacy settings: %+v", def)
	}
	if def := aliasDef(t, `{"github_wake_on_pr_merge":false}`, SystemRulePRChecksFailed); def != nil {
		t.Fatalf("the CI setting must not read the merge setting: %+v", def)
	}
	if def := aliasDef(t, `{}`, SystemRuleChildDone); def != nil {
		t.Fatalf("missing settings must project nothing (default-on is the baseline): %+v", def)
	}
}

func TestLegacyWorkspaceAliasFieldsAreCanonicalOverStoredDefinition(t *testing.T) {
	stored := &WakeupDefinition{Scope: WakeupScopeWorkspace, Revision: 3, Patch: WakeupConfigPatch{
		Enabled:   wakeupField[bool]{Set: true, Value: true},
		RateLimit: wakeupField[int]{Set: true, Value: 5},
	}}
	def, err := WorkspaceWakeupDefinition(legacyTestWorkspace, stored, []byte(`{"github_wake_on_pr_merge":false}`), SystemRulePRMerged)
	if err != nil {
		t.Fatal(err)
	}
	if !def.Patch.Enabled.Set || def.Patch.Enabled.Value || def.Patch.RateLimit.Value != 5 {
		t.Fatalf("alias must win and keep stored extras: %+v", def.Patch)
	}
	if stored.Patch.Enabled.Value != true {
		t.Fatal("the stored definition must not be mutated")
	}
	// Without an alias value the stored definition is returned as it is.
	same, err := WorkspaceWakeupDefinition(legacyTestWorkspace, stored, []byte(`{}`), SystemRulePRMerged)
	if err != nil || same != stored {
		t.Fatalf("no alias value must return the stored definition: %v %v", same, err)
	}
}

// A legacy write changes the revision, so the effective-config fingerprint and
// every claim gate keyed on it see the change.
func TestLegacyWorkspaceAliasWriteChangesFingerprint(t *testing.T) {
	fp := func(settings string) string {
		t.Helper()
		got, err := ResolveWakeupConfig(WakeupResolveInput{RuleKey: SystemRuleChildDone, Workspace: aliasDef(t, settings, SystemRuleChildDone)})
		if err != nil {
			t.Fatal(err)
		}
		return got.Fingerprint
	}
	off, on := fp(`{"system_wakeup_child_done":false}`), fp(`{"system_wakeup_child_done":true}`)
	text := fp(`{"system_wakeup_child_done_instruction":"a"}`)
	if off == on || off == text || on == text {
		t.Fatal("different legacy values must produce different fingerprints")
	}
	if off != fp(`{"system_wakeup_child_done":false,"unrelated":1}`) {
		t.Fatal("an unrelated settings key must not change the fingerprint")
	}
}

func TestApplyWorkspaceWakeupAliasesWritesSettingsAndKeepsUnknownKeys(t *testing.T) {
	f, s, _, _ := wakeFixture(t)
	ctx := context.Background()
	ws := parseTestUUID(t, f.WorkspaceID)
	f.Exec(t, `UPDATE workspace SET settings='{"future_key":{"a":[1,2]},"github_enabled":true}'::jsonb WHERE id=$1`, f.WorkspaceID)
	settings := func() map[string]json.RawMessage {
		t.Helper()
		var raw []byte
		if err := f.Pool.QueryRow(ctx, `SELECT settings FROM workspace WHERE id=$1`, f.WorkspaceID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	rest, err := s.ApplyWorkspaceWakeupAliases(ctx, ws, SystemRulePRMerged, WakeupConfigPatch{
		Enabled:   wakeupField[bool]{Set: true, Value: false},
		RateLimit: wakeupField[int]{Set: true, Value: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rest.Enabled.Set || !rest.RateLimit.Set {
		t.Fatalf("aliased field must be consumed and other fields returned: %+v", rest)
	}
	m := settings()
	if string(m["github_wake_on_pr_merge"]) != "false" || string(m["future_key"]) != `{"a": [1, 2]}` || string(m["github_enabled"]) != "true" {
		t.Fatalf("settings after PR alias write: %v", m)
	}

	text := "  do it  "
	if _, err := s.ApplyWorkspaceWakeupAliases(ctx, ws, SystemRuleChildDone, WakeupConfigPatch{
		Enabled:     wakeupField[bool]{Set: true, Value: false},
		Instruction: wakeupField[string]{Set: true, Value: text},
	}); err != nil {
		t.Fatal(err)
	}
	m = settings()
	if string(m["system_wakeup_child_done"]) != "false" || string(m["system_wakeup_child_done_instruction"]) != `"do it"` || m["github_wake_on_pr_merge"] == nil {
		t.Fatalf("settings after child-done alias write: %v", m)
	}
	// Clearing returns to inherit: on, no instruction.
	if _, err := s.ApplyWorkspaceWakeupAliases(ctx, ws, SystemRuleChildDone, WakeupConfigPatch{
		Enabled:     wakeupField[bool]{Set: true, Null: true},
		Instruction: wakeupField[string]{Set: true, Null: true},
	}); err != nil {
		t.Fatal(err)
	}
	if enabled, instruction := SystemWakeupDefault(mustSettings(t, f, ctx)); !enabled || instruction != "" {
		t.Fatalf("cleared aliases = %v %q, want on and empty", enabled, instruction)
	}
	// The legacy 4,000-byte instruction contract holds for the alias.
	_, err = s.ApplyWorkspaceWakeupAliases(ctx, ws, SystemRuleChildDone, WakeupConfigPatch{Instruction: wakeupField[string]{Set: true, Value: strings.Repeat("x", MaxSystemWakeupInstruction+1)}})
	if !errors.Is(err, ErrWakeupInput) {
		t.Fatalf("oversized alias instruction = %v, want ErrWakeupInput", err)
	}
	// A patch without aliased fields touches no settings.
	rest, err = s.ApplyWorkspaceWakeupAliases(ctx, ws, SystemRulePRMerged, WakeupConfigPatch{Instruction: wakeupField[string]{Set: true, Value: "pr text"}})
	if err != nil || !rest.Instruction.Set {
		t.Fatalf("PR instruction is not aliased and must be returned: %+v %v", rest, err)
	}
}

func mustSettings(t *testing.T, f principalFixture, ctx context.Context) []byte {
	t.Helper()
	var raw []byte
	if err := f.Pool.QueryRow(ctx, `SELECT settings FROM workspace WHERE id=$1`, f.WorkspaceID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

// Legacy writes (the workspace default and the per-issue child-done endpoints)
// are visible to the definition model at once and change its fingerprint.
func TestLegacyWritesReachTheDefinitionModel(t *testing.T) {
	f, s, issue, _ := wakeFixture(t)
	ctx := context.Background()
	ws := parseTestUUID(t, f.WorkspaceID)
	resolve := func() EffectiveWakeupConfig {
		t.Helper()
		wsDef, err := WorkspaceWakeupDefinition(ws, nil, mustSettings(t, f, ctx), SystemRuleChildDone)
		if err != nil {
			t.Fatal(err)
		}
		var issueDef *WakeupDefinition
		if row, err := f.q.GetSystemWakeup(ctx, db.GetSystemWakeupParams{IssueID: issue, SystemRule: systemRuleText(SystemRuleChildDone)}); err == nil {
			if issueDef, err = LegacyIssueWakeupDefinition(row); err != nil {
				t.Fatal(err)
			}
		}
		got, err := ResolveWakeupConfig(WakeupResolveInput{RuleKey: SystemRuleChildDone, Workspace: wsDef, Issue: issueDef})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	base := resolve()
	if !base.Enabled() && base.Config.Enabled.Set {
		t.Fatalf("a fresh workspace must resolve to the default: %+v", base.Config)
	}
	off, on, text, empty := false, true, "issue text", ""
	if _, err := s.SetChildDoneDefault(ctx, ws, &off, nil); err != nil {
		t.Fatal(err)
	}
	wsOff := resolve()
	if wsOff.Enabled() || wsOff.Fingerprint == base.Fingerprint {
		t.Fatalf("workspace default off: enabled=%v, fingerprint changed=%v", wsOff.Enabled(), wsOff.Fingerprint != base.Fingerprint)
	}
	customizeChildDone(t, s, issue, &on, &text)
	issueOn := resolve()
	if !issueOn.Enabled() || issueOn.Config.Instruction.Value != text || issueOn.Sources["enabled"] != WakeupScopeIssue || issueOn.Fingerprint == wsOff.Fingerprint {
		t.Fatalf("issue override: %+v sources=%v", issueOn.Config, issueOn.Sources)
	}
	customizeChildDone(t, s, issue, nil, &empty)
	inherited := resolve()
	if inherited.Config.Instruction.Set || inherited.Fingerprint == issueOn.Fingerprint {
		t.Fatalf("an empty legacy instruction must mean inherit: %+v", inherited.Config)
	}
}
