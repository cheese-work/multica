package service

import (
	"context"
	"log/slog"

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
		switch def.Scope {
		case WakeupScopeProject:
			return suspend(LegacySuspendProjectScope)
		case WakeupScopeWorkspace:
			if p.Enabled.Set || (ruleKey == SystemRuleChildDone && p.Instruction.Set) {
				return suspend(LegacySuspendAliasConflict)
			}
			// Only child_done runs a stored instruction; PR text is always
			// built from the receipt facts, so a stored one, even a clear,
			// would be silently ignored.
			if p.Instruction.Set {
				return suspend(LegacySuspendUnsupported)
			}
		case WakeupScopeIssue:
			if !legacyIssueFieldsMatch(p, projected) {
				return suspend(LegacySuspendIssueDiverges)
			}
		}
		if legacyUnsupportedFields(p) {
			return suspend(LegacySuspendUnsupported)
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

// legacyUnsupportedFields reports whether a patch sets (or clears) anything
// beyond the label, enabled and instruction fields the legacy path knows.
func legacyUnsupportedFields(p WakeupConfigPatch) bool {
	return p.Trigger.Set || p.Target.Set || p.Mode.Set || p.MaxFires.Set || p.Expiry.Set || p.Schedule.Set ||
		p.RateLimit.Set || p.AggregateLimit.Set || p.Filters.Set || p.ActiveRun.Set
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
