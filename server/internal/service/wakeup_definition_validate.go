package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Validation and reference authorization of scoped wakeup definitions
// (CHE-1082 L4). A field or trigger kind nothing runs yet is rejected rather
// than stored.

// WakeupDefinitionTriggerKinds lists the trigger kinds a definition may use
// today: the three built-in presets. A later layer adds a kind when it
// implements its execution; unimplemented kinds are rejected, never stored.
func WakeupDefinitionTriggerKinds() []string { return slices.Clone(builtinWakeupRules) }

// builtinWakeupRules are the platform rules, which double as the trigger presets.
var builtinWakeupRules = []string{SystemRuleChildDone, SystemRulePRMerged, SystemRulePRChecksFailed}

// WakeupDefinitionFields lists the patch fields a definition may set today.
// aggregate_limit, active_run and schedule belong to later layers.
func WakeupDefinitionFields() []string {
	return []string{"enabled", "name", "trigger", "target", "instruction", "mode", "max_fires", "expiry", "rate_limit", "filters"}
}

type wakeupTriggerSpec struct {
	Kind string `json:"kind"`
}

type wakeupTargetSpec struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
}

type wakeupExpirySpec struct {
	At           *time.Time `json:"at,omitempty"`
	AfterSeconds *int64     `json:"after_seconds,omitempty"`
}

type wakeupFiltersSpec struct {
	BaseBranch *string  `json:"base_branch,omitempty"`
	HeadBranch *string  `json:"head_branch,omitempty"`
	CI         *string  `json:"ci,omitempty"`
	Labels     []string `json:"labels,omitempty"`
	Priorities []string `json:"priorities,omitempty"`
}

func decodeWakeupSpec(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data")
	}
	return nil
}

// WakeupPatchTarget returns the target type a patch names and, for an agent or
// squad, its id. A target this build cannot read reports as an agent with no
// id, so callers that gate on visibility fail closed.
func WakeupPatchTarget(p WakeupConfigPatch) (string, pgtype.UUID) {
	if !p.Target.Set || p.Target.Null {
		return "", pgtype.UUID{}
	}
	var spec wakeupTargetSpec
	if decodeWakeupSpec(p.Target.Value, &spec) != nil {
		return "agent", pgtype.UUID{}
	}
	id, _ := wakeupUUID(spec.ID)
	return spec.Type, id
}

// WakeupRedactedTarget and WakeupRedactedInstruction replace what a viewer may
// not see.
func WakeupRedactedTarget() wakeupObject {
	return wakeupObject{Set: true, Value: json.RawMessage(`{"redacted":true}`)}
}

func WakeupRedactedInstruction() wakeupField[string] { return wakeupField[string]{} }

func wakeupDefinitionBad(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrWakeupInput, fmt.Sprintf(format, args...))
}

func checkWakeupRuleKey(key string) error {
	if _, builtin := WakeupBuiltinBaseline(key); builtin || customWakeupRuleKey.MatchString(key) {
		return nil
	}
	return wakeupDefinitionBad("unknown rule %q", key)
}

// normalizeWakeupPatch trims the text fields a definition stores.
func normalizeWakeupPatch(p *WakeupConfigPatch) {
	if p.Name.Set && !p.Name.Null {
		p.Name.Value = strings.TrimSpace(p.Name.Value)
	}
	if p.Instruction.Set && !p.Instruction.Null {
		p.Instruction.Value = strings.TrimSpace(p.Instruction.Value)
	}
	// Branches match exactly, so a stored " main" could never match: store them
	// trimmed. An undecodable filter is left for validation to refuse.
	if p.Filters.Set && !p.Filters.Null {
		var spec wakeupFiltersSpec
		if decodeWakeupSpec(p.Filters.Value, &spec) == nil {
			for _, branch := range []*string{spec.BaseBranch, spec.HeadBranch} {
				if branch != nil {
					*branch = strings.TrimSpace(*branch)
				}
			}
			if raw, err := json.Marshal(spec); err == nil {
				p.Filters.Value = raw
			}
		}
	}
}

// wakeupPatchRefs are the workspace-scoped references a patch names, which the
// caller must still authorize.
type wakeupPatchRefs struct {
	targetType string
	targetID   pgtype.UUID
	labels     []pgtype.UUID
}

// validateWakeupPatch checks one patch on its own: types, ranges and the fields
// and trigger kinds this build can run. Cross-field and cross-scope rules are
// checked on the resolved rule by validateEffectiveWakeup.
func validateWakeupPatch(ruleKey string, p WakeupConfigPatch, now time.Time) (wakeupPatchRefs, error) {
	var refs wakeupPatchRefs
	for _, unsupported := range []struct {
		name string
		set  bool
	}{{"aggregate_limit", p.AggregateLimit.Set}, {"active_run", p.ActiveRun.Set}, {"schedule", p.Schedule.Set}} {
		if unsupported.set {
			return refs, wakeupDefinitionBad("%s is not available yet", unsupported.name)
		}
	}
	if len(setWakeupFields(p)) == 0 {
		return refs, wakeupDefinitionBad("a definition sets at least one field; delete it to inherit")
	}
	live := func(f interface{ IsZero() bool }, null bool) bool { return !f.IsZero() && !null }
	if live(p.Name, p.Name.Null) && (p.Name.Value == "" || utf8.RuneCountInString(p.Name.Value) > maxWakeupDefinitionName) {
		return refs, wakeupDefinitionBad("name must be 1–%d characters", maxWakeupDefinitionName)
	}
	if live(p.Instruction, p.Instruction.Null) && (p.Instruction.Value == "" || len(p.Instruction.Value) > maxWakeupDefinitionInstruction) {
		return refs, wakeupDefinitionBad("instruction must be 1–%d bytes", maxWakeupDefinitionInstruction)
	}
	if live(p.Mode, p.Mode.Null) && p.Mode.Value != "once" && p.Mode.Value != "continuous" {
		return refs, wakeupDefinitionBad("mode must be once or continuous")
	}
	if live(p.MaxFires, p.MaxFires.Null) && (p.MaxFires.Value < 1 || p.MaxFires.Value > maxWakeupDefinitionMaxFires) {
		return refs, wakeupDefinitionBad("max_fires must be 1–%d", maxWakeupDefinitionMaxFires)
	}
	if live(p.RateLimit, p.RateLimit.Null) && (p.RateLimit.Value < 1 || p.RateLimit.Value > maxWakeupDefinitionRateLimit) {
		return refs, wakeupDefinitionBad("rate_limit must be 1–%d", maxWakeupDefinitionRateLimit)
	}
	if live(p.Trigger, p.Trigger.Null) {
		var spec wakeupTriggerSpec
		if err := decodeWakeupSpec(p.Trigger.Value, &spec); err != nil {
			return refs, wakeupDefinitionBad("invalid trigger: %v", err)
		}
		if !slices.Contains(WakeupDefinitionTriggerKinds(), spec.Kind) {
			return refs, wakeupDefinitionBad("trigger kind %q is not available yet", spec.Kind)
		}
		if _, builtin := WakeupBuiltinBaseline(ruleKey); builtin && spec.Kind != ruleKey {
			return refs, wakeupDefinitionBad("a built-in rule keeps its own trigger")
		}
	}
	if live(p.Target, p.Target.Null) {
		var spec wakeupTargetSpec
		if err := decodeWakeupSpec(p.Target.Value, &spec); err != nil {
			return refs, wakeupDefinitionBad("invalid target: %v", err)
		}
		id, err := wakeupUUID(spec.ID)
		switch {
		case err != nil:
			return refs, wakeupDefinitionBad("invalid target id")
		case spec.Type == "assignee" && id.Valid, (spec.Type == "agent" || spec.Type == "squad") && !id.Valid,
			spec.Type != "assignee" && spec.Type != "agent" && spec.Type != "squad":
			return refs, wakeupDefinitionBad("target must be the assignee, or an agent or squad with an id")
		}
		refs.targetType, refs.targetID = spec.Type, id
	}
	if live(p.Expiry, p.Expiry.Null) {
		var spec wakeupExpirySpec
		if err := decodeWakeupSpec(p.Expiry.Value, &spec); err != nil {
			return refs, wakeupDefinitionBad("invalid expiry: %v", err)
		}
		if (spec.At == nil) == (spec.AfterSeconds == nil) {
			return refs, wakeupDefinitionBad("expiry takes exactly one of at or after_seconds")
		}
		if spec.AfterSeconds != nil && (*spec.AfterSeconds < 60 || *spec.AfterSeconds > maxWakeupSeconds) {
			return refs, wakeupDefinitionBad("expiry after_seconds must be 60–%d", maxWakeupSeconds)
		}
		if spec.At != nil && (!spec.At.After(now) || spec.At.Sub(now) > maxWakeupSeconds*time.Second) {
			return refs, wakeupDefinitionBad("expiry at must be in the next year")
		}
	}
	if live(p.Filters, p.Filters.Null) {
		var err error
		if refs.labels, err = validateWakeupFilters(p.Filters.Value); err != nil {
			return refs, err
		}
	}
	return refs, nil
}

func validateWakeupFilters(raw json.RawMessage) ([]pgtype.UUID, error) {
	var spec wakeupFiltersSpec
	if err := decodeWakeupSpec(raw, &spec); err != nil {
		return nil, wakeupDefinitionBad("invalid filters: %v", err)
	}
	for _, b := range []struct {
		name   string
		branch *string
	}{{"base_branch", spec.BaseBranch}, {"head_branch", spec.HeadBranch}} {
		if b.branch != nil && (strings.TrimSpace(*b.branch) == "" || len(strings.TrimSpace(*b.branch)) > 255) {
			return nil, wakeupDefinitionBad("%s must be 1–255 bytes", b.name)
		}
	}
	if spec.CI != nil && !slices.Contains([]string{"failure", "error", "both"}, *spec.CI) {
		return nil, wakeupDefinitionBad("ci must be failure, error or both")
	}
	if len(spec.Priorities) > 5 {
		return nil, wakeupDefinitionBad("too many priorities")
	}
	for _, p := range spec.Priorities {
		if !slices.Contains([]string{"urgent", "high", "medium", "low", "none"}, p) {
			return nil, wakeupDefinitionBad("unknown priority %q", p)
		}
	}
	if len(spec.Labels) > maxWakeupDefinitionLabels {
		return nil, wakeupDefinitionBad("at most %d labels", maxWakeupDefinitionLabels)
	}
	labels := make([]pgtype.UUID, 0, len(spec.Labels))
	for _, l := range spec.Labels {
		id, err := wakeupUUID(l)
		if err != nil || !id.Valid {
			return nil, wakeupDefinitionBad("invalid label id")
		}
		labels = append(labels, id)
	}
	return labels, nil
}

// validateEffectiveWakeup checks the complete resolved rule, because a patch is
// only valid in the chain it lands in: a custom rule must stay runnable, and a
// filter must fit the trigger it narrows.
func validateEffectiveWakeup(eff EffectiveWakeupConfig) error {
	_, builtin := WakeupBuiltinBaseline(eff.RuleKey)
	if !builtin && (!eff.Config.Trigger.Set || !eff.Config.Instruction.Set) {
		return wakeupDefinitionBad("a custom rule needs a trigger and an instruction")
	}
	if !eff.Config.Filters.Set {
		return nil
	}
	kind := eff.RuleKey
	if eff.Config.Trigger.Set {
		var spec wakeupTriggerSpec
		_ = decodeWakeupSpec(eff.Config.Trigger.Value, &spec)
		kind = spec.Kind
	}
	var spec wakeupFiltersSpec
	_ = decodeWakeupSpec(eff.Config.Filters.Value, &spec)
	pr := kind == SystemRulePRMerged || kind == SystemRulePRChecksFailed
	if (spec.BaseBranch != nil || spec.HeadBranch != nil) && !pr {
		return wakeupDefinitionBad("branch filters apply to pull request triggers only")
	}
	// The failing-checks event carries no base branch, so the filter could
	// never match: refuse it instead of storing a rule that silently never runs.
	if spec.BaseBranch != nil && kind == SystemRulePRChecksFailed {
		return wakeupDefinitionBad("the base_branch filter applies to merged pull requests only")
	}
	if spec.CI != nil && kind != SystemRulePRChecksFailed {
		return wakeupDefinitionBad("the ci filter applies to failed pull request checks only")
	}
	return nil
}

// authorizeWakeupRefs proves the member may use every workspace reference of a
// patch. A reference outside the workspace, an archived or runtime-less agent
// and an agent the member may not invoke all read as forbidden, so a caller
// learns nothing about another workspace. A squad runs through its leader.
func (s *IssueWakeupService) authorizeWakeupRefs(ctx context.Context, q *db.Queries, ws, member pgtype.UUID, refs wakeupPatchRefs) error {
	agentID := refs.targetID
	switch refs.targetType {
	case "squad":
		squad, err := q.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: refs.targetID, WorkspaceID: ws})
		if errors.Is(err, pgx.ErrNoRows) || err == nil && squad.ArchivedAt.Valid {
			return ErrWakeupForbidden
		} else if err != nil {
			return err
		}
		agentID = squad.LeaderID
		fallthrough
	case "agent":
		agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agentID, WorkspaceID: ws})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrWakeupForbidden
		} else if err != nil {
			return err
		}
		if err := s.authorize(ctx, q, ws, member, agent); err != nil {
			return err
		}
	}
	for _, label := range refs.labels {
		if _, err := q.GetLabel(ctx, db.GetLabelParams{ID: label, WorkspaceID: ws}); errors.Is(err, pgx.ErrNoRows) {
			return ErrWakeupForbidden
		} else if err != nil {
			return err
		}
	}
	return nil
}
