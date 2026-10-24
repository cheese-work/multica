package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

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
	// Effective is the rule as it resolves at this scope. Callers judge who may
	// see the definition by its resolved target, since a sparse patch may inherit
	// the target from an ancestor. Nil means it could not be resolved.
	Effective *WakeupConfigPatch
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
	in.settings = ws.Settings
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

// authorizeResolvedTarget proves the member may use the target the proposal
// resolves to. A sparse patch (an instruction, a mode, a filter) inherits its
// target from an ancestor, and its text would run on that agent, so naming no
// target does not make it the writer's to steer. A rule the proposal leaves
// disabled runs nothing and needs no proof. An unreadable target fails closed.
func (s *IssueWakeupService) authorizeResolvedTarget(ctx context.Context, q *db.Queries, ws, member pgtype.UUID, eff EffectiveWakeupConfig) error {
	if !eff.Enabled() {
		return nil
	}
	kind, id := WakeupPatchTarget(eff.Config)
	if kind != "agent" && kind != "squad" {
		return nil
	}
	return s.authorizeWakeupRefs(ctx, q, ws, member, wakeupPatchRefs{targetType: kind, targetID: id})
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
	if create {
		withRootAggregateDefault(&patch)
	}
	if err := checkWakeupAggregateScope(ref.Kind, patch); err != nil {
		return WakeupDefinitionView{}, err
	}
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
	if aliased {
		// Every settings write updates this row, so holding it from the revision
		// check to the commit keeps a settings writer out of the middle.
		if err := q.LockWorkspaceSettingsForWakeupDefinition(ctx, ref.WorkspaceID); err != nil {
			return WakeupDefinitionView{}, err
		}
	}
	if aliased {
		if err := requireAliasSettings(ctx, q, ref.WorkspaceID, w.RuleKey, patch); err != nil {
			return WakeupDefinitionView{}, err
		}
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
	if err := s.authorizeWakeupRefs(ctx, q, ref.WorkspaceID, member, refs); err != nil {
		return WakeupDefinitionView{}, err
	}
	candidate := patch
	if aliased {
		if candidate, err = workspaceAliasReadback(in.settings, w.RuleKey, patch); err != nil {
			return WakeupDefinitionView{}, err
		}
	}
	eff, err := resolveWakeupProposal(in, ref, &WakeupDefinition{Scope: ref.Kind, ScopeID: ref.ID, Root: root, Patch: candidate})
	if err != nil {
		return WakeupDefinitionView{}, err
	}
	if err := s.authorizeResolvedTarget(ctx, q, ref.WorkspaceID, member, eff); err != nil {
		return WakeupDefinitionView{}, err
	}

	rest := patch
	if aliased {
		if rest, err = writeWorkspaceAliasesTx(ctx, tx, q, ref.WorkspaceID, w.RuleKey, patch); err != nil {
			return WakeupDefinitionView{}, err
		}
	}
	keep := len(setWakeupFields(rest)) > 0
	// The ceiling bounds stored records, so it applies only when this write
	// inserts one: an alias-only update of a workspace built-in stores nothing.
	// The scope lock is held, so the count cannot move before the insert, and a
	// refusal rolls the alias effects back with the transaction.
	if keep && !found {
		if n, err := q.CountWakeupDefinitionsInScope(ctx, db.CountWakeupDefinitionsInScopeParams{WorkspaceID: ref.WorkspaceID, ScopeKind: string(ref.Kind), ScopeID: ref.ID}); err != nil {
			return WakeupDefinitionView{}, err
		} else if n >= maxWakeupDefinitionsPerScope {
			return WakeupDefinitionView{}, wakeupDefinitionBad("a scope holds at most %d definitions", maxWakeupDefinitionsPerScope)
		}
	}
	raw, err := MarshalWakeupConfigPatch(rest)
	if err != nil {
		return WakeupDefinitionView{}, err
	}
	var row db.IssueWakeupDefinition
	switch {
	case keep && !found:
		row, err = q.InsertWakeupDefinition(ctx, db.InsertWakeupDefinitionParams{
			WorkspaceID: ref.WorkspaceID, ScopeKind: string(ref.Kind), ScopeID: ref.ID, RuleKey: w.RuleKey, Root: root, Config: raw, EventTypes: WakeupEventSelector(rest), Actor: member,
		})
	case keep:
		row, err = q.UpdateWakeupDefinition(ctx, db.UpdateWakeupDefinitionParams{
			WorkspaceID: ref.WorkspaceID, ScopeKind: string(ref.Kind), ScopeID: ref.ID, RuleKey: w.RuleKey, ExpectedRevision: existing.Revision, Config: raw, EventTypes: WakeupEventSelector(rest), Actor: member,
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
	var view WakeupDefinitionView
	if aliased {
		// Read inside the transaction: the alias values it just wrote are the
		// view, and a clear that leaves nothing stored is an empty definition.
		merged, err := s.workspaceBuiltinView(ctx, q, ref.WorkspaceID, w.RuleKey)
		if err != nil {
			return WakeupDefinitionView{}, err
		}
		if merged == nil {
			merged = &WakeupDefinitionView{Scope: WakeupScopeWorkspace, ScopeID: ref.WorkspaceID, RuleKey: w.RuleKey}
		}
		view = *merged
	} else if view, err = wakeupDefinitionView(row); err != nil {
		return WakeupDefinitionView{}, err
	}
	resolved := eff.Config
	view.Effective = &resolved
	if err := tx.Commit(ctx); err != nil {
		return WakeupDefinitionView{}, err
	}
	return view, nil
}

// writeWorkspaceAliasesTx applies the alias fields of a replacing patch through
// the settings path, inside the caller's transaction.
func writeWorkspaceAliasesTx(ctx context.Context, tx pgx.Tx, q *db.Queries, ws pgtype.UUID, key string, patch WakeupConfigPatch) (WakeupConfigPatch, error) {
	row, err := q.GetWorkspace(ctx, ws)
	if err != nil {
		return patch, err
	}
	alias, err := workspaceAliasWrite(row.Settings, key, patch)
	if err != nil {
		return patch, err
	}
	return applyWorkspaceWakeupAliasesTx(ctx, tx, q, ws, key, alias)
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
	if _, builtin := WakeupBuiltinBaseline(ruleKey); builtin && ref.Kind == WakeupScopeIssue {
		if err := retireLegacyIssueOverride(ctx, tx, q, ref, ruleKey); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// retireLegacyIssueOverride resets an issue's customized legacy row to
// inheritance. Without it, deleting the stored definition would let the same
// row project back as the issue's override. Pauses, fire counts and consumed
// state are untouched; enabled follows the inherited value only on a row
// nothing has paused or ended, and a changed child_done state is re-baselined
// (or its receipts discarded) exactly as a workspace default change does.
func retireLegacyIssueOverride(ctx context.Context, tx pgx.Tx, q *db.Queries, ref WakeupScopeRef, key string) error {
	if _, err := q.LockWakeupIssue(ctx, ref.ID); err != nil {
		return err
	}
	row, err := q.GetSystemWakeup(ctx, db.GetSystemWakeupParams{IssueID: ref.ID, SystemRule: systemRuleText(key)})
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !row.CustomizedAt.Valid {
		return nil
	} else if err != nil {
		return err
	}
	in, err := loadWakeupChain(ctx, q, ref, key)
	if err != nil {
		return err
	}
	in.Issue = nil
	inherited, err := ResolveWakeupConfig(in)
	if err != nil {
		return err
	}
	updated, err := q.RetireCustomizedSystemWakeup(ctx, db.RetireCustomizedSystemWakeupParams{ID: row.ID, Enabled: inherited.Enabled()})
	if err != nil {
		return err
	}
	if key != SystemRuleChildDone || updated.Enabled == row.Enabled {
		return nil
	}
	if updated.Enabled {
		return baselineChildDone(ctx, tx, q, updated)
	}
	return q.DiscardWakeupReceipts(ctx, updated.ID)
}

// afterWakeupListRows is a test seam: it runs between a list reading the
// stored definitions and resolving them, where a concurrent write could land.
var afterWakeupListRows func()

// ListWakeupDefinitions returns one scope's definitions. At workspace scope the
// built-ins whose enabled/instruction live in the settings aliases appear with
// those values, so the list shows the revision a write expects.
func (s *IssueWakeupService) ListWakeupDefinitions(ctx context.Context, ref WakeupScopeRef) ([]WakeupDefinitionView, error) {
	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	// One snapshot for the definitions and for every resolution of them: callers
	// judge who may see a patch by what it resolves to, so the two must be the
	// same revision.
	if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY"); err != nil {
		return nil, err
	}
	q := s.Tasks.Queries.WithTx(tx)
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
	if afterWakeupListRows != nil {
		afterWakeupListRows()
	}
	if ref.Kind == WakeupScopeWorkspace {
		if out, err = s.withWorkspaceBuiltins(ctx, q, ref, out); err != nil {
			return nil, err
		}
	}
	for i := range out {
		in, err := loadWakeupChain(ctx, q, ref, out[i].RuleKey)
		if err != nil {
			return nil, fmt.Errorf("load wakeup rule %s: %w", out[i].RuleKey, err)
		}
		eff, err := ResolveWakeupConfig(in)
		if err != nil {
			return nil, fmt.Errorf("resolve wakeup rule %s: %w", out[i].RuleKey, err)
		}
		resolved := eff.Config
		out[i].Effective = &resolved
	}
	return out, nil
}

// withWorkspaceBuiltins adds the built-ins whose settings aliases carry values,
// or replaces their stored views with the alias-merged ones.
func (s *IssueWakeupService) withWorkspaceBuiltins(ctx context.Context, q *db.Queries, ref WakeupScopeRef, out []WakeupDefinitionView) ([]WakeupDefinitionView, error) {
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
	if _, builtin := WakeupBuiltinBaseline(ruleKey); builtin && ref.Kind == WakeupScopeWorkspace && proposal != nil {
		patch := proposal.Patch
		normalizeWakeupPatch(&patch)
		if err := requireAliasSettings(ctx, q, ref.WorkspaceID, ruleKey, patch); err != nil {
			return WakeupEffectiveRule{}, err
		}
	}
	in, err := loadWakeupChain(ctx, q, ref, ruleKey)
	if err != nil {
		return WakeupEffectiveRule{}, err
	}
	scopeDef := *in.scopeDefinition(ref.Kind)
	if proposal != nil {
		patch := proposal.Patch
		normalizeWakeupPatch(&patch)
		if newRoot {
			withRootAggregateDefault(&patch)
		}
		if err := checkWakeupAggregateScope(ref.Kind, patch); err != nil {
			return WakeupEffectiveRule{}, err
		}
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
		if _, builtin := WakeupBuiltinBaseline(ruleKey); builtin && ref.Kind == WakeupScopeWorkspace {
			// Preview what saving would read back, not the literal proposal.
			if patch, err = workspaceAliasReadback(in.settings, ruleKey, patch); err != nil {
				return WakeupEffectiveRule{}, err
			}
		}
		candidate := &WakeupDefinition{Scope: ref.Kind, ScopeID: ref.ID, Root: root, Patch: patch}
		eff, err := resolveWakeupProposal(in, ref, candidate)
		if err != nil {
			return WakeupEffectiveRule{}, err
		}
		if err := s.authorizeResolvedTarget(ctx, q, ref.WorkspaceID, member, eff); err != nil {
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
