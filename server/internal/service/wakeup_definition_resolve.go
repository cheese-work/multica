package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// WakeupScope names where a definition lives, and so its precedence:
// built-in baseline, then workspace, then the issue's current project, then
// the issue itself.
type WakeupScope string

const (
	WakeupScopeBuiltin   WakeupScope = "builtin"
	WakeupScopeWorkspace WakeupScope = "workspace"
	WakeupScopeProject   WakeupScope = "project"
	WakeupScopeIssue     WakeupScope = "issue"
)

// WakeupInapplicableReason says why a rule resolves but cannot run for an issue.
type WakeupInapplicableReason string

const (
	// WakeupInapplicableNoRoot: a custom rule has overrides but no root
	// definition in this issue's workspace/project chain, e.g. the root was
	// deleted or the issue moved to another project. The overrides are retired,
	// not promoted, and do not follow the issue.
	WakeupInapplicableNoRoot WakeupInapplicableReason = "no_root"
)

// WakeupDefinition is one stored scoped definition, decoded.
type WakeupDefinition struct {
	Scope    WakeupScope
	ScopeID  pgtype.UUID
	Revision int64
	// Root marks the definition that created a custom rule.
	Root  bool
	Patch WakeupConfigPatch
}

// WakeupDefinitionFromRow decodes a stored row. A scope or config this build
// cannot read is an error, never a silent empty patch.
func WakeupDefinitionFromRow(row db.IssueWakeupDefinition) (WakeupDefinition, error) {
	scope := WakeupScope(row.ScopeKind)
	if scope != WakeupScopeWorkspace && scope != WakeupScopeProject && scope != WakeupScopeIssue {
		return WakeupDefinition{}, fmt.Errorf("wakeup definition %s: unknown scope kind %q", row.RuleKey, row.ScopeKind)
	}
	patch, err := DecodeWakeupConfigPatch(row.Config)
	if err != nil {
		return WakeupDefinition{}, fmt.Errorf("wakeup definition %s: %w", row.RuleKey, err)
	}
	return WakeupDefinition{Scope: scope, ScopeID: row.ScopeID, Revision: row.Revision, Root: row.Root, Patch: patch}, nil
}

// WakeupResolveInput carries one rule's definitions for one issue. The caller
// loads only the issue's current project's definition, so a project move
// changes the input rather than being detected here.
type WakeupResolveInput struct {
	RuleKey        string
	IssueProjectID pgtype.UUID
	// IssueTarget is the issue's resolved target, opaque here; it only feeds
	// the fingerprint.
	IssueTarget string
	Workspace   *WakeupDefinition
	Project     *WakeupDefinition
	Issue       *WakeupDefinition
}

// WakeupAggregateCap is one scope's own starts/hour cap shared by every
// instance below it. Caps are additive: all of them apply.
type WakeupAggregateCap struct {
	Scope   WakeupScope
	ScopeID pgtype.UUID
	Limit   int
}

// EffectiveWakeupConfig is the resolved configuration of one rule for one
// issue. Enabled is configuration state only: integration shutdown,
// authorization failures and safety pauses are vetoes the caller applies.
type EffectiveWakeupConfig struct {
	RuleKey    string
	Applicable bool
	// Inapplicable is set when Applicable is false.
	Inapplicable WakeupInapplicableReason
	// Config holds the merged fields; a field that is not Set was never set or
	// was cleared. AggregateLimit is never merged, see AggregateCaps.
	Config WakeupConfigPatch
	// Sources maps a Set field's JSON name to the scope that supplied it.
	Sources map[string]WakeupScope
	// AggregateCaps lists every applicable cap, broadest scope first.
	AggregateCaps []WakeupAggregateCap
	// Fingerprint identifies the inputs the resolution depends on; see
	// ResolveWakeupConfig.
	Fingerprint string
}

// Enabled reports the configured on/off state. A rule that never set it, a
// custom rule without its root, and an inapplicable rule are all off.
func (e EffectiveWakeupConfig) Enabled() bool {
	return e.Applicable && e.Config.Enabled.Set && e.Config.Enabled.Value
}

// WakeupBuiltinBaseline returns the baseline of a built-in rule. Missing
// settings keep today's default-on behavior, and the baseline sets nothing
// else: no mode, expiry, fire cap or aggregate cap.
func WakeupBuiltinBaseline(key string) (WakeupConfigPatch, bool) {
	switch key {
	case SystemRuleChildDone, SystemRulePRMerged, SystemRulePRChecksFailed:
		var p WakeupConfigPatch
		p.Enabled = wakeupField[bool]{Set: true, Value: true}
		return p, true
	}
	return WakeupConfigPatch{}, false
}

// ResolveWakeupConfig merges built-in baseline, workspace, current project and
// issue definitions of one rule, later scopes winning. Omitted fields inherit,
// an explicit false disables (a later scope may re-enable), an explicit null
// clears an optional value, and nested objects replace as a whole. Aggregate
// caps are not merged: each scope owns its own, a null removes only that
// scope's cap, and an override can never lift an ancestor's.
//
// The fingerprint covers the rule key, the issue's project and target, and the
// scope and revision of every definition present, so any edit, reset, move or
// retarget changes it. It deliberately ignores patch content: callers must
// bump a definition's revision on every write.
func ResolveWakeupConfig(in WakeupResolveInput) (EffectiveWakeupConfig, error) {
	if in.RuleKey == "" {
		return EffectiveWakeupConfig{}, errors.New("wakeup rule key is required")
	}
	layers := []struct {
		slot WakeupScope
		def  *WakeupDefinition
	}{{WakeupScopeWorkspace, in.Workspace}, {WakeupScopeProject, in.Project}, {WakeupScopeIssue, in.Issue}}
	if in.Project != nil && (!in.IssueProjectID.Valid || in.Project.ScopeID != in.IssueProjectID) {
		return EffectiveWakeupConfig{}, errors.New("wakeup project definition is not the issue's current project")
	}
	baseline, builtin := WakeupBuiltinBaseline(in.RuleKey)

	out := EffectiveWakeupConfig{RuleKey: in.RuleKey, Applicable: true, Sources: map[string]WakeupScope{}}
	out.Config = mergeWakeupPatch(out.Config, baseline, WakeupScopeBuiltin, out.Sources)
	seen := false
	for _, l := range layers {
		if l.def == nil {
			continue
		}
		if l.def.Scope != l.slot {
			return EffectiveWakeupConfig{}, fmt.Errorf("wakeup %s definition supplied as %s", l.def.Scope, l.slot)
		}
		if l.def.Root && (builtin || l.slot == WakeupScopeIssue || seen) {
			return EffectiveWakeupConfig{}, fmt.Errorf("wakeup %s definition of %s cannot be a root", l.slot, in.RuleKey)
		}
		if !seen && !builtin && !l.def.Root {
			out.Applicable, out.Inapplicable = false, WakeupInapplicableNoRoot
		}
		seen = true
		out.Config = mergeWakeupPatch(out.Config, l.def.Patch, l.slot, out.Sources)
		if p := l.def.Patch.AggregateLimit; p.Set && !p.Null {
			out.AggregateCaps = append(out.AggregateCaps, WakeupAggregateCap{Scope: l.slot, ScopeID: l.def.ScopeID, Limit: p.Value})
		}
	}
	if !seen && !builtin {
		out.Applicable, out.Inapplicable = false, WakeupInapplicableNoRoot
	}
	if !out.Applicable {
		out.AggregateCaps = nil
	}
	out.Fingerprint = wakeupFingerprint(in)
	return out, nil
}

func mergeWakeupPatch(dst, src WakeupConfigPatch, scope WakeupScope, sources map[string]WakeupScope) WakeupConfigPatch {
	dst.Enabled = mergeWakeupField(dst.Enabled, src.Enabled, "enabled", scope, sources)
	dst.Name = mergeWakeupField(dst.Name, src.Name, "name", scope, sources)
	dst.Trigger = mergeWakeupField(dst.Trigger, src.Trigger, "trigger", scope, sources)
	dst.Target = mergeWakeupField(dst.Target, src.Target, "target", scope, sources)
	dst.Instruction = mergeWakeupField(dst.Instruction, src.Instruction, "instruction", scope, sources)
	dst.Mode = mergeWakeupField(dst.Mode, src.Mode, "mode", scope, sources)
	dst.MaxFires = mergeWakeupField(dst.MaxFires, src.MaxFires, "max_fires", scope, sources)
	dst.Expiry = mergeWakeupField(dst.Expiry, src.Expiry, "expiry", scope, sources)
	dst.Schedule = mergeWakeupField(dst.Schedule, src.Schedule, "schedule", scope, sources)
	dst.RateLimit = mergeWakeupField(dst.RateLimit, src.RateLimit, "rate_limit", scope, sources)
	dst.Filters = mergeWakeupField(dst.Filters, src.Filters, "filters", scope, sources)
	dst.ActiveRun = mergeWakeupField(dst.ActiveRun, src.ActiveRun, "active_run", scope, sources)
	return dst
}

func mergeWakeupField[T any](dst, src wakeupField[T], name string, scope WakeupScope, sources map[string]WakeupScope) wakeupField[T] {
	switch {
	case !src.Set:
		return dst
	case src.Null:
		delete(sources, name)
		return wakeupField[T]{}
	}
	sources[name] = scope
	return src
}

func wakeupFingerprint(in WakeupResolveInput) string {
	h := sha256.New()
	// Length-prefixed fields keep distinct inputs from concatenating alike.
	put := func(s string) {
		h.Write([]byte(strconv.Itoa(len(s)) + ":" + s))
	}
	put("wakeup-config/v1")
	put(in.RuleKey)
	put(wakeupUUIDString(in.IssueProjectID))
	put(in.IssueTarget)
	for _, d := range []*WakeupDefinition{in.Workspace, in.Project, in.Issue} {
		if d == nil {
			put("-")
			continue
		}
		put(string(d.Scope) + ":" + wakeupUUIDString(d.ScopeID) + ":" + strconv.FormatInt(d.Revision, 10))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func wakeupUUIDString(id pgtype.UUID) string {
	if !id.Valid {
		return "-"
	}
	return hex.EncodeToString(id.Bytes[:])
}
