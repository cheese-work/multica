package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Writing scoped definitions (CHE-1082 L4): typed validation, authorization of
// every workspace reference, revision-checked writes and the effective-rule
// read. Definitions are only stored here; executing them is later layers' work,
// so a field or trigger kind nothing runs yet is rejected rather than stored.

const (
	// maxWakeupDefinitionsPerScope bounds one workspace, project or issue.
	maxWakeupDefinitionsPerScope   = 32
	maxWakeupDefinitionInstruction = 12000
	maxWakeupDefinitionName        = 120
	maxWakeupDefinitionMaxFires    = 1000
	maxWakeupDefinitionRateLimit   = 12
	maxWakeupDefinitionLabels      = 20
)

// wakeupPreviewRuleKey stands in for the key of a new custom rule that a preview
// has not been given one for. It is a well-formed key that is never stored.
const wakeupPreviewRuleKey = "00000000-0000-4000-8000-000000000000"

var customWakeupRuleKey = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// WakeupScopeRef names the scope one definition API call acts on. ID is the
// workspace, project or issue id by Kind.
type WakeupScopeRef struct {
	Kind        WakeupScope
	ID          pgtype.UUID
	WorkspaceID pgtype.UUID
}

func (r WakeupScopeRef) lockKey() string {
	return fmt.Sprintf("wakeup-definition:%s:%s:%s", wakeupUUIDString(r.WorkspaceID), r.Kind, wakeupUUIDString(r.ID))
}

// WakeupDefinitionView is one stored definition as the API shows it.
type WakeupDefinitionView struct {
	Scope     WakeupScope
	ScopeID   pgtype.UUID
	RuleKey   string
	Root      bool
	Revision  int64
	Patch     WakeupConfigPatch
	UpdatedAt pgtype.Timestamptz
}

// WakeupDefinitionWrite is one create/replace request. An empty RuleKey creates
// a new custom root rule under a server-assigned key. Revision is the revision
// the caller observed: 0 means "does not exist yet". Patch replaces the whole
// definition at the scope.
type WakeupDefinitionWrite struct {
	RuleKey  string
	Revision int64
	Patch    WakeupConfigPatch
}

// WakeupEffectiveRule is a resolved rule plus the fields the viewed scope sets
// over an inherited value.
type WakeupEffectiveRule struct {
	EffectiveWakeupConfig
	Overrides []string
}

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
	for name, unsupported := range map[string]bool{"aggregate_limit": p.AggregateLimit.Set, "active_run": p.ActiveRun.Set, "schedule": p.Schedule.Set} {
		if unsupported {
			return refs, wakeupDefinitionBad("%s is not available yet", name)
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
	for name, branch := range map[string]*string{"base_branch": spec.BaseBranch, "head_branch": spec.HeadBranch} {
		if branch != nil && (strings.TrimSpace(*branch) == "" || len(*branch) > 255) {
			return nil, wakeupDefinitionBad("%s must be 1–255 bytes", name)
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

func wakeupDefinitionView(row db.IssueWakeupDefinition) (WakeupDefinitionView, error) {
	def, err := WakeupDefinitionFromRow(row)
	if err != nil {
		return WakeupDefinitionView{}, err
	}
	return WakeupDefinitionView{Scope: def.Scope, ScopeID: row.ScopeID, RuleKey: row.RuleKey, Root: row.Root, Revision: row.Revision, Patch: def.Patch, UpdatedAt: row.UpdatedAt}, nil
}

func wakeupDefinitionParams(ref WakeupScopeRef, key string) db.GetWakeupDefinitionParams {
	return db.GetWakeupDefinitionParams{WorkspaceID: ref.WorkspaceID, ScopeKind: string(ref.Kind), ScopeID: ref.ID, RuleKey: key}
}

// loadWakeupChain reads one rule's definitions for a scope's resolution: the
// workspace's (with the legacy settings aliases folded in), the project's and,
// for an issue, its own. The issue's legacy row stands in for a built-in's
// issue definition until one is stored.
func loadWakeupChain(ctx context.Context, q *db.Queries, ref WakeupScopeRef, key string) (WakeupResolveInput, error) {
	in := WakeupResolveInput{RuleKey: key}
	ws, err := q.GetWorkspace(ctx, ref.WorkspaceID)
	if err != nil {
		return in, err
	}
	var issue db.Issue
	switch ref.Kind {
	case WakeupScopeProject:
		in.IssueProjectID = ref.ID
	case WakeupScopeIssue:
		if issue, err = q.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: ref.ID, WorkspaceID: ref.WorkspaceID}); err != nil {
			return in, err
		}
		in.IssueProjectID = issue.ProjectID
		in.IssueTarget = issue.AssigneeType.String + ":" + wakeupUUIDString(issue.AssigneeID)
	}
	stored := func(scope WakeupScope, id pgtype.UUID) (*WakeupDefinition, error) {
		row, err := q.GetWakeupDefinition(ctx, wakeupDefinitionParams(WakeupScopeRef{Kind: scope, ID: id, WorkspaceID: ref.WorkspaceID}, key))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		} else if err != nil {
			return nil, err
		}
		def, err := WakeupDefinitionFromRow(row)
		return &def, err
	}
	var wsDef, projectDef, issueDef *WakeupDefinition
	if wsDef, err = stored(WakeupScopeWorkspace, ref.WorkspaceID); err != nil {
		return in, err
	}
	if in.Workspace, err = WorkspaceWakeupDefinition(ref.WorkspaceID, wsDef, ws.Settings, key); err != nil {
		return in, err
	}
	if in.IssueProjectID.Valid {
		if projectDef, err = stored(WakeupScopeProject, in.IssueProjectID); err != nil {
			return in, err
		}
		in.Project = projectDef
	}
	if ref.Kind == WakeupScopeIssue {
		if issueDef, err = stored(WakeupScopeIssue, ref.ID); err != nil {
			return in, err
		}
		if _, builtin := WakeupBuiltinBaseline(key); builtin && issueDef == nil {
			row, err := q.GetSystemWakeup(ctx, db.GetSystemWakeupParams{IssueID: ref.ID, SystemRule: systemRuleText(key)})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return in, err
			}
			if err == nil {
				if issueDef, err = LegacyIssueWakeupDefinition(row); err != nil {
					return in, err
				}
			}
		}
		in.Issue = issueDef
	}
	return in, nil
}

// scopeDefinition is the input slot of the scope being acted on.
func (in *WakeupResolveInput) scopeDefinition(kind WakeupScope) **WakeupDefinition {
	switch kind {
	case WakeupScopeWorkspace:
		return &in.Workspace
	case WakeupScopeProject:
		return &in.Project
	}
	return &in.Issue
}

// resolveWakeupProposal resolves the chain with one scope's definition replaced
// (or, with a nil candidate, removed), and for a replacement validates the
// resolved rule.
func resolveWakeupProposal(in WakeupResolveInput, ref WakeupScopeRef, candidate *WakeupDefinition) (EffectiveWakeupConfig, error) {
	*in.scopeDefinition(ref.Kind) = candidate
	eff, err := ResolveWakeupConfig(in)
	if err != nil {
		return eff, wakeupDefinitionBad("%v", err)
	}
	if candidate == nil {
		return eff, nil
	}
	if !eff.Applicable {
		return eff, wakeupDefinitionBad("this custom rule has no root definition in the workspace or project")
	}
	return eff, validateEffectiveWakeup(eff)
}

// SaveWakeupDefinition creates or replaces one definition. member is the human
// the write acts for; every reference it names must be usable by them. The
// legacy alias fields of a workspace built-in go through the settings
// transaction, so the store never holds a second copy of them.
func (s *IssueWakeupService) SaveWakeupDefinition(ctx context.Context, ref WakeupScopeRef, member pgtype.UUID, w WakeupDefinitionWrite) (WakeupDefinitionView, error) {
	create := w.RuleKey == ""
	switch {
	case create && (ref.Kind == WakeupScopeIssue || w.Revision != 0):
		return WakeupDefinitionView{}, wakeupDefinitionBad("a new custom rule is created at a workspace or project, without a revision")
	case create:
		w.RuleKey = uuid.NewString()
	default:
		if err := checkWakeupRuleKey(w.RuleKey); err != nil {
			return WakeupDefinitionView{}, err
		}
	}
	patch := w.Patch
	normalizeWakeupPatch(&patch)
	refs, err := validateWakeupPatch(w.RuleKey, patch, time.Now())
	if err != nil {
		return WakeupDefinitionView{}, err
	}
	_, builtin := WakeupBuiltinBaseline(w.RuleKey)
	aliased := ref.Kind == WakeupScopeWorkspace && builtin

	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return WakeupDefinitionView{}, err
	}
	defer tx.Rollback(ctx)
	q := s.Tasks.Queries.WithTx(tx)
	if err := q.LockWakeupDefinitionScope(ctx, ref.lockKey()); err != nil {
		return WakeupDefinitionView{}, err
	}
	existing, err := q.GetWakeupDefinition(ctx, wakeupDefinitionParams(ref, w.RuleKey))
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return WakeupDefinitionView{}, err
	}
	in, err := loadWakeupChain(ctx, q, ref, w.RuleKey)
	if err != nil {
		return WakeupDefinitionView{}, err
	}
	// The revision a caller observes is the effective one: for a workspace
	// built-in it includes the settings aliases, which change without the row.
	current := *in.scopeDefinition(ref.Kind)
	observed := existing.Revision
	if aliased {
		observed = 0
		if current != nil {
			observed = current.Revision
		}
	}
	if observed != w.Revision {
		return WakeupDefinitionView{}, ErrWakeupConflict
	}
	root := create || found && existing.Root
	if root && (!patch.Trigger.Set || patch.Trigger.Null || !patch.Instruction.Set || patch.Instruction.Null) {
		return WakeupDefinitionView{}, wakeupDefinitionBad("a custom rule needs a trigger and an instruction")
	}
	if !found {
		if n, err := q.CountWakeupDefinitionsInScope(ctx, db.CountWakeupDefinitionsInScopeParams{WorkspaceID: ref.WorkspaceID, ScopeKind: string(ref.Kind), ScopeID: ref.ID}); err != nil {
			return WakeupDefinitionView{}, err
		} else if n >= maxWakeupDefinitionsPerScope {
			return WakeupDefinitionView{}, wakeupDefinitionBad("a scope holds at most %d definitions", maxWakeupDefinitionsPerScope)
		}
	}
	if err := s.authorizeWakeupRefs(ctx, q, ref.WorkspaceID, member, refs); err != nil {
		return WakeupDefinitionView{}, err
	}
	if _, err := resolveWakeupProposal(in, ref, &WakeupDefinition{Scope: ref.Kind, ScopeID: ref.ID, Root: root, Patch: patch}); err != nil {
		return WakeupDefinitionView{}, err
	}

	rest := patch
	if aliased {
		if rest, err = s.writeWorkspaceAliases(ctx, ref.WorkspaceID, w.RuleKey, patch); err != nil {
			return WakeupDefinitionView{}, err
		}
	}
	keep := len(setWakeupFields(rest)) > 0
	raw, err := MarshalWakeupConfigPatch(rest)
	if err != nil {
		return WakeupDefinitionView{}, err
	}
	var row db.IssueWakeupDefinition
	switch {
	case keep && !found:
		row, err = q.InsertWakeupDefinition(ctx, db.InsertWakeupDefinitionParams{
			WorkspaceID: ref.WorkspaceID, ScopeKind: string(ref.Kind), ScopeID: ref.ID, RuleKey: w.RuleKey, Root: root, Config: raw, Actor: member,
		})
	case keep:
		row, err = q.UpdateWakeupDefinition(ctx, db.UpdateWakeupDefinitionParams{
			WorkspaceID: ref.WorkspaceID, ScopeKind: string(ref.Kind), ScopeID: ref.ID, RuleKey: w.RuleKey, ExpectedRevision: existing.Revision, Config: raw, Actor: member,
		})
	case found:
		var n int64
		if n, err = q.DeleteWakeupDefinition(ctx, db.DeleteWakeupDefinitionParams{
			WorkspaceID: ref.WorkspaceID, ScopeKind: string(ref.Kind), ScopeID: ref.ID, RuleKey: w.RuleKey, ExpectedRevision: existing.Revision,
		}); err == nil && n == 0 {
			err = pgx.ErrNoRows
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return WakeupDefinitionView{}, ErrWakeupConflict
	} else if err != nil {
		return WakeupDefinitionView{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WakeupDefinitionView{}, err
	}
	if aliased {
		views, err := s.workspaceBuiltinView(ctx, s.Tasks.Queries, ref.WorkspaceID, w.RuleKey)
		if err != nil || views == nil {
			return WakeupDefinitionView{}, errors.Join(err, errors.New("saved definition not readable"))
		}
		return *views, nil
	}
	return wakeupDefinitionView(row)
}

// writeWorkspaceAliases applies the alias fields of a replacing patch through
// the settings path. A replace clears an alias the patch leaves out, and the
// legacy instruction limit applies to the alias.
func (s *IssueWakeupService) writeWorkspaceAliases(ctx context.Context, ws pgtype.UUID, key string, patch WakeupConfigPatch) (WakeupConfigPatch, error) {
	row, err := s.Tasks.Queries.GetWorkspace(ctx, ws)
	if err != nil {
		return patch, err
	}
	current, err := legacyWorkspacePatch(row.Settings, key)
	if err != nil {
		return patch, err
	}
	alias := patch
	if current.Enabled.Set && !patch.Enabled.Set {
		alias.Enabled = wakeupField[bool]{Set: true, Null: true}
	}
	if key == SystemRuleChildDone {
		if current.Instruction.Set && !patch.Instruction.Set {
			alias.Instruction = wakeupField[string]{Set: true, Null: true}
		}
		if alias.Instruction.Set && !alias.Instruction.Null && len(alias.Instruction.Value) > MaxSystemWakeupInstruction {
			return patch, wakeupDefinitionBad("this rule's workspace instruction must be at most %d bytes", MaxSystemWakeupInstruction)
		}
	}
	return s.ApplyWorkspaceWakeupAliases(ctx, ws, key, alias)
}

// workspaceBuiltinView is a workspace built-in's effective definition: the
// stored row with the settings aliases folded in. Nil when neither exists.
func (s *IssueWakeupService) workspaceBuiltinView(ctx context.Context, q *db.Queries, ws pgtype.UUID, key string) (*WakeupDefinitionView, error) {
	row, err := q.GetWakeupDefinition(ctx, wakeupDefinitionParams(WakeupScopeRef{Kind: WakeupScopeWorkspace, ID: ws, WorkspaceID: ws}, key))
	var stored *WakeupDefinition
	view := WakeupDefinitionView{}
	switch {
	case err == nil:
		if view, err = wakeupDefinitionView(row); err != nil {
			return nil, err
		}
		def, _ := WakeupDefinitionFromRow(row)
		stored = &def
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}
	workspace, err := q.GetWorkspace(ctx, ws)
	if err != nil {
		return nil, err
	}
	merged, err := WorkspaceWakeupDefinition(ws, stored, workspace.Settings, key)
	if err != nil || merged == nil {
		return nil, err
	}
	view.Scope, view.ScopeID, view.RuleKey, view.Revision, view.Patch = WakeupScopeWorkspace, ws, key, merged.Revision, merged.Patch
	return &view, nil
}

// DeleteWakeupDefinition removes one definition: a custom rule, or an override
// that resets to the inherited settings. A workspace built-in is disabled
// instead. Deleting a custom root retires the rule, and overrides below it
// stay stored but resolve as inapplicable.
func (s *IssueWakeupService) DeleteWakeupDefinition(ctx context.Context, ref WakeupScopeRef, ruleKey string, revision int64) error {
	if err := checkWakeupRuleKey(ruleKey); err != nil {
		return err
	}
	if _, builtin := WakeupBuiltinBaseline(ruleKey); builtin && ref.Kind == WakeupScopeWorkspace {
		return wakeupDefinitionBad("a built-in rule is disabled, not deleted")
	}
	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := s.Tasks.Queries.WithTx(tx)
	if err := q.LockWakeupDefinitionScope(ctx, ref.lockKey()); err != nil {
		return err
	}
	row, err := q.GetWakeupDefinition(ctx, wakeupDefinitionParams(ref, ruleKey))
	if err != nil {
		return err
	}
	if row.Revision != revision {
		return ErrWakeupConflict
	}
	if n, err := q.DeleteWakeupDefinition(ctx, db.DeleteWakeupDefinitionParams{
		WorkspaceID: ref.WorkspaceID, ScopeKind: string(ref.Kind), ScopeID: ref.ID, RuleKey: ruleKey, ExpectedRevision: revision,
	}); err != nil {
		return err
	} else if n == 0 {
		return ErrWakeupConflict
	}
	return tx.Commit(ctx)
}

// ListWakeupDefinitions returns one scope's definitions. At workspace scope the
// built-ins whose enabled/instruction live in the settings aliases appear with
// those values, so the list shows the revision a write expects.
func (s *IssueWakeupService) ListWakeupDefinitions(ctx context.Context, ref WakeupScopeRef) ([]WakeupDefinitionView, error) {
	q := s.Tasks.Queries
	rows, err := q.ListWakeupDefinitionsInScope(ctx, db.ListWakeupDefinitionsInScopeParams{WorkspaceID: ref.WorkspaceID, ScopeKind: string(ref.Kind), ScopeID: ref.ID})
	if err != nil {
		return nil, err
	}
	out := make([]WakeupDefinitionView, 0, len(rows))
	for _, row := range rows {
		view, err := wakeupDefinitionView(row)
		if err != nil {
			return nil, err
		}
		out = append(out, view)
	}
	if ref.Kind != WakeupScopeWorkspace {
		return out, nil
	}
	for _, key := range builtinWakeupRules {
		merged, err := s.workspaceBuiltinView(ctx, q, ref.WorkspaceID, key)
		if err != nil || merged == nil {
			if err != nil {
				return nil, err
			}
			continue
		}
		if i := slices.IndexFunc(out, func(v WakeupDefinitionView) bool { return v.RuleKey == key }); i >= 0 {
			out[i] = *merged
		} else {
			out = append(out, *merged)
		}
	}
	return out, nil
}

// EffectiveWakeupRule resolves one rule at a scope. A non-nil proposal is
// validated and resolved in place of the stored definition without being
// persisted; an empty proposal key previews a new custom root rule.
func (s *IssueWakeupService) EffectiveWakeupRule(ctx context.Context, ref WakeupScopeRef, member pgtype.UUID, ruleKey string, proposal *WakeupDefinitionWrite) (WakeupEffectiveRule, error) {
	newRoot := proposal != nil && ruleKey == ""
	if newRoot {
		if ref.Kind == WakeupScopeIssue {
			return WakeupEffectiveRule{}, wakeupDefinitionBad("a new custom rule is created at a workspace or project")
		}
		ruleKey = wakeupPreviewRuleKey
	}
	if err := checkWakeupRuleKey(ruleKey); err != nil {
		return WakeupEffectiveRule{}, err
	}
	q := s.Tasks.Queries
	in, err := loadWakeupChain(ctx, q, ref, ruleKey)
	if err != nil {
		return WakeupEffectiveRule{}, err
	}
	scopeDef := *in.scopeDefinition(ref.Kind)
	if proposal != nil {
		patch := proposal.Patch
		normalizeWakeupPatch(&patch)
		refs, err := validateWakeupPatch(ruleKey, patch, time.Now())
		if err != nil {
			return WakeupEffectiveRule{}, err
		}
		if err := s.authorizeWakeupRefs(ctx, q, ref.WorkspaceID, member, refs); err != nil {
			return WakeupEffectiveRule{}, err
		}
		root := newRoot || scopeDef != nil && scopeDef.Root
		if root && (!patch.Trigger.Set || patch.Trigger.Null || !patch.Instruction.Set || patch.Instruction.Null) {
			return WakeupEffectiveRule{}, wakeupDefinitionBad("a custom rule needs a trigger and an instruction")
		}
		candidate := &WakeupDefinition{Scope: ref.Kind, ScopeID: ref.ID, Root: root, Patch: patch}
		eff, err := resolveWakeupProposal(in, ref, candidate)
		if err != nil {
			return WakeupEffectiveRule{}, err
		}
		return WakeupEffectiveRule{EffectiveWakeupConfig: eff, Overrides: overriddenWakeupFields(in, ref, candidate)}, nil
	}
	eff, err := ResolveWakeupConfig(in)
	if err != nil {
		return WakeupEffectiveRule{}, err
	}
	return WakeupEffectiveRule{EffectiveWakeupConfig: eff, Overrides: overriddenWakeupFields(in, ref, scopeDef)}, nil
}

// overriddenWakeupFields lists the fields the scope's definition sets (or
// clears) over a value an ancestor supplies.
func overriddenWakeupFields(in WakeupResolveInput, ref WakeupScopeRef, scopeDef *WakeupDefinition) []string {
	out := []string{}
	if scopeDef == nil {
		return out
	}
	ancestors, err := ResolveWakeupConfig(func() WakeupResolveInput { *in.scopeDefinition(ref.Kind) = nil; return in }())
	if err != nil {
		return out
	}
	for _, field := range setWakeupFields(scopeDef.Patch) {
		if _, inherited := ancestors.Sources[field]; inherited {
			out = append(out, field)
		}
	}
	return out
}
