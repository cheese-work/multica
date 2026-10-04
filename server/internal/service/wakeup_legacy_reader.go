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
// Custom rules are not executed by the legacy path and are not gated.
func LegacyDispatchGate(ruleKey string, legacy db.IssueWakeup, rows []db.IssueWakeupDefinition) LegacyDispatchVerdict {
	if _, builtin := WakeupBuiltinBaseline(ruleKey); !builtin {
		return LegacyDispatchVerdict{Allowed: true}
	}
	suspend := func(r LegacySuspendReason) LegacyDispatchVerdict { return LegacyDispatchVerdict{Reason: r} }
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
