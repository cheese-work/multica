package service

import (
	"context"
	"log/slog"
	"reflect"
	"strings"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// LegacySuspendReason says why legacy dispatch of a platform rule is held.
type LegacySuspendReason string

const (
	// LegacySuspendUnreadable: a stored definition this build cannot decode
	// (a later version, unknown fields or scope).
	LegacySuspendUnreadable LegacySuspendReason = "unreadable_definition"
	// LegacySuspendProjectScope: the legacy path has no project scope.
	LegacySuspendProjectScope LegacySuspendReason = "project_definition"
	// LegacySuspendUnsupported: the definition sets fields the legacy path
	// cannot execute (trigger, target, schedule, caps, filters, ...).
	LegacySuspendUnsupported LegacySuspendReason = "unsupported_fields"
	// LegacySuspendAliasConflict: a stored workspace definition carries a field
	// the settings aliases own, a second possibly conflicting copy.
	LegacySuspendAliasConflict LegacySuspendReason = "alias_conflict"
	// LegacySuspendIssueDiverges: a stored issue definition says something
	// the legacy issue row does not.
	LegacySuspendIssueDiverges LegacySuspendReason = "issue_override_diverges"
	// LegacySuspendDefaultInstance: a runtime row derived from a scoped
	// (custom) rule; this build cannot execute the definition it came from.
	LegacySuspendDefaultInstance LegacySuspendReason = "default_derived_instance"
)

// LegacyDispatchVerdict is the answer of LegacyDispatchGate.
type LegacyDispatchVerdict struct {
	Allowed bool
	Reason  LegacySuspendReason
}

// LegacyDispatchGate is the emergency fail-closed recovery floor: a build
// that runs only the legacy path (an emergency rollback to this layer) must not
// keep dispatching a platform rule that scoped definitions it cannot execute
// now control, because it would ignore them. It is not support for those
// definitions. Allowed means every stored definition of the rule, at the
// issue's workspace, current project and itself, says nothing the legacy path
// does not already do; anything else suspends dispatch of that rule only.
// Definitions are only read, so their configuration is preserved untouched.
// A row derived from a scoped (custom) rule is always held: this build has no
// way to execute it. Other custom keys are not gated.
func LegacyDispatchGate(ruleKey string, legacy db.IssueWakeup, rows []db.IssueWakeupDefinition) LegacyDispatchVerdict {
	suspend := func(r LegacySuspendReason) LegacyDispatchVerdict { return LegacyDispatchVerdict{Reason: r} }
	if isDefaultDerivedWakeup(legacy) {
		return suspend(LegacySuspendDefaultInstance)
	}
	if _, builtin := WakeupBuiltinBaseline(ruleKey); !builtin {
		return LegacyDispatchVerdict{Allowed: true}
	}
	projected, _ := LegacyIssueWakeupDefinition(legacy)
	for _, row := range rows {
		def, err := WakeupDefinitionFromRow(row)
		if err != nil {
			return suspend(LegacySuspendUnreadable)
		}
		p := def.Patch
		if def.Scope == WakeupScopeProject {
			return suspend(LegacySuspendProjectScope)
		}
		if def.Scope == WakeupScopeIssue && !legacyIssueFieldsMatch(p, projected) {
			return suspend(LegacySuspendIssueDiverges)
		}
		// Whatever the definition stores must be something legacy execution
		// reads at that scope, whether it sets or clears the field.
		for _, field := range setWakeupFields(p) {
			switch {
			case legacyReadsStoredField(def.Scope, ruleKey, field):
			case def.Scope == WakeupScopeWorkspace && legacyAliasedField(ruleKey, field):
				return suspend(LegacySuspendAliasConflict)
			default:
				return suspend(LegacySuspendUnsupported)
			}
		}
	}
	return LegacyDispatchVerdict{Allowed: true}
}

// isDefaultDerivedWakeup reports whether a runtime row was materialized from a
// scoped rule. It has no system rule, so only its origin metadata tells it from
// a genuine local wakeup. Without a rule key it cannot be resolved here, so it
// is never executed.
func isDefaultDerivedWakeup(w db.IssueWakeup) bool {
	return !w.SystemRule.Valid && (w.DefaultRuleKey.Valid || w.DefaultScopeKind.Valid || w.DefaultScopeID.Valid)
}

// legacyReadsStoredField says whether legacy execution reads a stored
// definition's field at a scope. It reads exactly these, and nothing else:
//
//   - workspace: nothing from a stored definition. The enabled value of each
//     built-in and child_done's instruction come from the settings aliases.
//   - project: nothing; the legacy path has no project scope.
//   - issue: the legacy row's enabled (every rule) and instruction (child_done
//     only; PR text is built from the receipt facts and ignores a stored one).
//
// The name is a display label with no execution effect, so it is allowed at
// every scope. A field added to the patch later is unread until classified
// here; TestLegacyMatrixCoversEveryPatchField keeps the two in step.
func legacyReadsStoredField(scope WakeupScope, ruleKey, field string) bool {
	switch field {
	case "name":
		return true
	case "enabled":
		return scope == WakeupScopeIssue
	case "instruction":
		return scope == WakeupScopeIssue && ruleKey == SystemRuleChildDone
	}
	return false
}

// legacyAliasedField reports fields whose legacy value lives in the workspace
// settings aliases, so a stored workspace copy is a conflicting second one.
func legacyAliasedField(ruleKey, field string) bool {
	return field == "enabled" || (field == "instruction" && ruleKey == SystemRuleChildDone)
}

// setWakeupFields lists the JSON names of every field a patch sets or clears.
// It reflects over the patch, so a new field is covered the day it is added.
func setWakeupFields(p WakeupConfigPatch) []string {
	var names []string
	v, t := reflect.ValueOf(p), reflect.TypeOf(p)
	for i := range t.NumField() {
		if zero, ok := v.Field(i).Interface().(interface{ IsZero() bool }); ok && !zero.IsZero() {
			names = append(names, strings.Split(t.Field(i).Tag.Get("json"), ",")[0])
		}
	}
	return names
}

// legacyIssueFieldsMatch compares an issue definition's enabled and
// instruction with the legacy row's projection. An explicit null equals unset.
func legacyIssueFieldsMatch(p WakeupConfigPatch, projected *WakeupDefinition) bool {
	var want WakeupConfigPatch
	if projected != nil {
		want = projected.Patch
	}
	return sameWakeupField(p.Enabled, want.Enabled) && sameWakeupField(p.Instruction, want.Instruction)
}

// sameWakeupField compares two fields as inheritance sees them: unset and
// null are the same.
func sameWakeupField[T comparable](a, b wakeupField[T]) bool {
	aSet, bSet := a.Set && !a.Null, b.Set && !b.Null
	return aSet == bSet && (!aSet || a.Value == b.Value)
}

// legacyDispatchAllowed applies LegacyDispatchGate to one platform-rule row.
// Without any stored definition it is a single indexed lookup.
func legacyDispatchAllowed(ctx context.Context, q *db.Queries, issue db.Issue, w db.IssueWakeup) (bool, error) {
	if !w.SystemRule.Valid {
		if isDefaultDerivedWakeup(w) {
			slog.WarnContext(ctx, "legacy wakeup dispatch suspended for a default-derived instance this build cannot execute",
				"issue_id", uuidString(issue.ID), "wakeup_id", uuidString(w.ID), "rule", w.DefaultRuleKey.String, "reason", string(LegacySuspendDefaultInstance))
			return false, nil
		}
		return true, nil
	}
	rows, err := q.ListWakeupDefinitionsForRule(ctx, db.ListWakeupDefinitionsForRuleParams{
		WorkspaceID: w.WorkspaceID, RuleKey: w.SystemRule.String, ProjectID: issue.ProjectID, IssueID: issue.ID,
	})
	if err != nil || len(rows) == 0 {
		return err == nil, err
	}
	verdict := LegacyDispatchGate(w.SystemRule.String, w, rows)
	if !verdict.Allowed {
		slog.WarnContext(ctx, "legacy wakeup dispatch suspended by scoped definitions this build cannot execute",
			"issue_id", uuidString(issue.ID), "wakeup_id", uuidString(w.ID), "rule", w.SystemRule.String, "reason", string(verdict.Reason))
	}
	return verdict.Allowed, nil
}
