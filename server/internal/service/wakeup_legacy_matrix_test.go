package service

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// legacyMatrixFields is every field a stored definition can carry, with a
// valid sample value. TestLegacyMatrixCoversEveryPatchField keeps it in step
// with WakeupConfigPatch, so a field added later cannot dodge the matrix.
var legacyMatrixFields = []struct{ name, sample string }{
	{"enabled", `true`}, {"name", `"label"`}, {"trigger", `{}`}, {"target", `{}`}, {"instruction", `"x"`},
	{"mode", `"once"`}, {"max_fires", `3`}, {"expiry", `{}`}, {"schedule", `{}`}, {"rate_limit", `5`},
	{"aggregate_limit", `4`}, {"filters", `{}`}, {"active_run", `"defer"`},
}

func TestLegacyMatrixCoversEveryPatchField(t *testing.T) {
	var want []string
	rt := reflect.TypeOf(WakeupConfigPatch{})
	for i := range rt.NumField() {
		want = append(want, strings.Split(rt.Field(i).Tag.Get("json"), ",")[0])
	}
	var got []string
	for _, f := range legacyMatrixFields {
		got = append(got, f.name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("matrix fields %v do not match the patch fields %v; classify the new field for legacy execution", got, want)
	}
}

// Legacy execution reads exactly these stored values, and only these:
//
//	workspace  enabled/instruction of child_done and enabled of the PR rules come
//	           from the settings aliases, never from a stored definition; nothing
//	           else is read
//	project    nothing: the legacy path has no project scope
//	issue      the legacy row's enabled (all rules) and instruction (child_done
//	           only; PR text is built from receipt facts), and nothing else
//
// The name is a display label with no execution effect. Any other stored field
// the legacy path ignores must hold the rule, set or cleared. This table is
// written out independently of the gate so the two cannot drift together.
func legacyMatrixWant(scope, rule, field string) LegacySuspendReason {
	legacyReadsIt := field == "enabled" || (field == "instruction" && rule == SystemRuleChildDone)
	switch {
	case scope == "project":
		return LegacySuspendProjectScope
	case field == "name":
		return ""
	case scope == "workspace" && legacyReadsIt:
		// Read only through the settings alias, so a stored copy conflicts.
		return LegacySuspendAliasConflict
	case scope == "issue" && legacyReadsIt:
		return ""
	}
	return LegacySuspendUnsupported
}

// legacyMatrixRow is the legacy row whose projection equals the one stored
// field, so only the capability check can hold the rule.
func legacyMatrixRow(rule, field string, null bool, issue pgtype.UUID) db.IssueWakeup {
	w := db.IssueWakeup{IssueID: issue, Enabled: true, SystemRule: systemRuleText(rule)}
	if null {
		return w
	}
	customized := pgtype.Timestamptz{Valid: true}
	switch field {
	case "enabled":
		w.CustomizedAt = customized
	case "instruction":
		// A paused row projects its instruction but no enabled value.
		w.CustomizedAt, w.Enabled, w.Instruction = customized, false, "x"
		w.PausedReason = pgtype.Text{String: wakeupPausedLoop, Valid: true}
	}
	return w
}

func TestLegacyGateFieldScopeRuleMatrix(t *testing.T) {
	issue := parseTestUUIDRaw("11111111-1111-4111-8111-111111111111")
	project := parseTestUUIDRaw("22222222-2222-4222-8222-222222222222")
	ws := parseTestUUIDRaw("33333333-3333-4333-8333-333333333333")
	scopeIDs := map[string]pgtype.UUID{"workspace": ws, "project": project, "issue": issue}
	for _, scope := range []string{"workspace", "project", "issue"} {
		for _, rule := range []string{SystemRuleChildDone, SystemRulePRMerged, SystemRulePRChecksFailed} {
			for _, f := range legacyMatrixFields {
				for _, null := range []bool{false, true} {
					value := f.sample
					if null {
						value = "null"
					}
					name := fmt.Sprintf("%s/%s/%s=%s", scope, rule, f.name, value)
					t.Run(name, func(t *testing.T) {
						row := defRow(scope, scopeIDs[scope], rule, fmt.Sprintf(`{"v":1,%q:%s}`, f.name, value))
						got := LegacyDispatchGate(rule, legacyMatrixRow(rule, f.name, null, issue), []db.IssueWakeupDefinition{row})
						want := legacyMatrixWant(scope, rule, f.name)
						if got.Allowed != (want == "") || got.Reason != want {
							t.Fatalf("verdict = %+v, want reason %q", got, want)
						}
					})
				}
			}
		}
	}
}

// The signed counterexample: a customized PR row whose instruction a matching
// issue definition mirrors. The mirror must not make the unexecuted PR
// instruction look supported.
func TestLegacyGateHoldsMatchingIssuePRInstruction(t *testing.T) {
	issue := parseTestUUIDRaw("11111111-1111-4111-8111-111111111111")
	for _, rule := range []string{SystemRulePRMerged, SystemRulePRChecksFailed} {
		row := db.IssueWakeup{IssueID: issue, Enabled: true, SystemRule: systemRuleText(rule), CustomizedAt: pgtype.Timestamptz{Valid: true}, Instruction: "required scoped instruction"}
		defs := []db.IssueWakeupDefinition{defRow("issue", issue, rule, `{"v":1,"enabled":true,"instruction":"required scoped instruction"}`)}
		if v := LegacyDispatchGate(rule, row, defs); v.Allowed || v.Reason != LegacySuspendUnsupported {
			t.Fatalf("%s verdict = %+v, want unsupported_fields", rule, v)
		}
		// The same definition on child_done is exactly what legacy executes.
		child := row
		child.SystemRule = systemRuleText(SystemRuleChildDone)
		defs = []db.IssueWakeupDefinition{defRow("issue", issue, SystemRuleChildDone, `{"v":1,"enabled":true,"instruction":"required scoped instruction"}`)}
		if v := LegacyDispatchGate(SystemRuleChildDone, child, defs); !v.Allowed {
			t.Fatalf("child_done verdict = %+v, want allowed", v)
		}
	}
}
