package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// Scoped-event capture and drain (CHE-1082 L9). The ordinary event triggers
// write a reference-only row to wakeup_scoped_event, in the source transaction,
// when a definition selecting the event exists in the issue's workspace, current
// project or issue scope (migration 596). The scheduler drains those rows here:
// it resolves only the event's issue, ensures that issue's runtime instance of
// each matching rule and delivers the event into the existing wakeup receipts,
// atomically with marking the input handled. Nothing here defines an event, an
// instance default or a write path; definitions that select events are written
// by a later layer, and until then the outbox stays empty.

const (
	scopedDrainBatch = 50
	scopedPruneBatch = 200
	// scopedEventRetention matches the wakeup receipt retention. Past it a
	// pending input is closed as expired, with its accounting kept visible, and
	// a closed input is deleted.
	scopedEventRetention = 7 * 24 * time.Hour
	// scopedRetryDelay keeps an input that hit a real database error from being
	// reclaimed every pass and starving the rest of the batch.
	scopedRetryDelay = time.Minute
)

// The outcome recorded on a handled outbox input.
const (
	scopedOutcomeDelivered        = "delivered"
	scopedOutcomeIssueGone        = "issue_gone"
	scopedOutcomeIssueClosed      = "issue_closed"
	scopedOutcomeIssueDormant     = "issue_dormant"
	scopedOutcomeScopeChanged     = "scope_changed"
	scopedOutcomeDefinitionChange = "definition_changed"
	scopedOutcomeNoRule           = "no_rule"
	scopedOutcomeNoMatch          = "no_match"
	scopedOutcomeRuleDisabled     = "rule_disabled"
	scopedOutcomeInvalid          = "invalid_definition"
	scopedOutcomeNoTarget         = "no_target"
	scopedOutcomeNoInstruction    = "no_instruction"
	scopedOutcomeCapacity         = "default_capacity"
	scopedOutcomeInstanceOff      = "instance_disabled"
	scopedOutcomeExpired          = "expired"
)

// wakeupEventTriggerSpec is the trigger of an event rule: the event types it
// selects.
type wakeupEventTriggerSpec struct {
	Kind   string   `json:"kind"`
	Events []string `json:"events"`
}

const wakeupTriggerKindEvent = "event"

// eventTriggerEvents lists the event types an event trigger selects, sorted and
// unique. A trigger of another kind, a malformed one and an unknown event type
// select nothing.
func eventTriggerEvents(trigger wakeupObject) []string {
	if !trigger.Set || trigger.Null {
		return nil
	}
	var spec wakeupEventTriggerSpec
	if decodeWakeupSpec(trigger.Value, &spec) != nil || spec.Kind != wakeupTriggerKindEvent {
		return nil
	}
	var events []string
	for _, e := range spec.Events {
		if slices.Contains(WakeupEventTypes, e) {
			events = append(events, e)
		}
	}
	slices.Sort(events)
	return slices.Compact(events)
}

// WakeupEventSelector is what a stored definition records in its event_types
// column: the event types its own trigger selects, which the capture trigger
// probes by index. An override that sets no trigger records none; the root it
// overrides is probed in its own scope.
func WakeupEventSelector(p WakeupConfigPatch) []string {
	return append([]string{}, eventTriggerEvents(p.Trigger)...)
}

// DrainScopedEvents handles one bounded batch of pending outbox inputs of the
// given workspaces. Tests use it; the scheduler's tick drains every workspace.
func (s *IssueWakeupService) DrainScopedEvents(ctx context.Context, workspaceIDs ...pgtype.UUID) error {
	if len(workspaceIDs) == 0 {
		return nil
	}
	return s.drainScopedEvents(ctx, workspaceIDs)
}

// drainScopedEvents handles up to scopedDrainBatch pending inputs, one
// transaction each. A transaction claims the oldest unclaimed input with SKIP
// LOCKED, so concurrent schedulers take different inputs and never wait for
// each other, and holds the issue's lock only while that one input is handled.
// An input that fails on a real database error is rolled back alone, scheduled
// for a later retry and reported; the rest of the pass carries on.
func (s *IssueWakeupService) drainScopedEvents(ctx context.Context, workspaceIDs []pgtype.UUID) error {
	var errs []error
	now := time.Now()
	for range scopedDrainBatch {
		if ctx.Err() != nil {
			return errors.Join(append(errs, ctx.Err())...)
		}
		claimed, id, err := s.drainOneScopedEvent(ctx, workspaceIDs, now)
		if err != nil {
			errs = append(errs, err)
			if !id.Valid {
				return errors.Join(errs...)
			}
			if deferErr := s.Tasks.Queries.DeferWakeupScopedEvent(ctx, db.DeferWakeupScopedEventParams{ID: id, RetryAt: pgtype.Timestamptz{Time: now.Add(scopedRetryDelay), Valid: true}}); deferErr != nil {
				return errors.Join(append(errs, deferErr)...)
			}
			continue
		}
		if !claimed {
			break
		}
	}
	return errors.Join(errs...)
}

// drainOneScopedEvent claims, resolves and settles one input. claimed is false
// when nothing was available; id names the input a returned error belongs to.
func (s *IssueWakeupService) drainOneScopedEvent(ctx context.Context, workspaceIDs []pgtype.UUID, now time.Time) (claimed bool, id pgtype.UUID, err error) {
	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return false, id, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '200ms'"); err != nil {
		return false, id, err
	}
	q := s.Tasks.Queries.WithTx(tx)
	events, err := q.ClaimWakeupScopedEvents(ctx, db.ClaimWakeupScopedEventsParams{
		Now: pgtype.Timestamptz{Time: now, Valid: true}, WorkspaceIds: workspaceIDs, BatchSize: 1,
	})
	if err != nil || len(events) == 0 {
		return false, id, err
	}
	ev := events[0]
	fail := func(err error) (bool, pgtype.UUID, error) {
		return true, ev.ID, fmt.Errorf("scoped event %s: %w", util.UUIDToString(ev.ID), err)
	}
	outcome, err := s.resolveScopedEvent(ctx, tx, q, ev)
	if err != nil {
		return fail(err)
	}
	if err := q.MarkWakeupScopedEventHandled(ctx, db.MarkWakeupScopedEventHandledParams{ID: ev.ID, Now: pgtype.Timestamptz{Time: now, Valid: true}, Outcome: outcome}); err != nil {
		return fail(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fail(err)
	}
	return true, ev.ID, nil
}

func scopedText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

// scopedRuleChain is one rule's stored definitions in the issue's chain.
type scopedRuleChain struct{ workspace, project, issue *db.IssueWakeupDefinition }

func (c scopedRuleChain) all() []*db.IssueWakeupDefinition {
	var rows []*db.IssueWakeupDefinition
	for _, d := range []*db.IssueWakeupDefinition{c.workspace, c.project, c.issue} {
		if d != nil {
			rows = append(rows, d)
		}
	}
	return rows
}

// selects reports whether any definition of the chain records the event type in
// its selector, which is how capture decided the event was worth keeping.
func (c scopedRuleChain) selects(eventType string) bool {
	return slices.ContainsFunc(c.all(), func(d *db.IssueWakeupDefinition) bool { return slices.Contains(d.EventTypes, eventType) })
}

func loadScopedChains(ctx context.Context, q *db.Queries, issue db.Issue) (map[string]*scopedRuleChain, error) {
	chains := map[string]*scopedRuleChain{}
	scopes := []struct {
		kind string
		id   pgtype.UUID
		set  func(*scopedRuleChain, *db.IssueWakeupDefinition)
	}{
		{string(WakeupScopeWorkspace), issue.WorkspaceID, func(c *scopedRuleChain, d *db.IssueWakeupDefinition) { c.workspace = d }},
		{string(WakeupScopeProject), issue.ProjectID, func(c *scopedRuleChain, d *db.IssueWakeupDefinition) { c.project = d }},
		{string(WakeupScopeIssue), issue.ID, func(c *scopedRuleChain, d *db.IssueWakeupDefinition) { c.issue = d }},
	}
	for _, scope := range scopes {
		if !scope.id.Valid {
			continue
		}
		rows, err := q.ListWakeupDefinitionsInScope(ctx, db.ListWakeupDefinitionsInScopeParams{WorkspaceID: issue.WorkspaceID, ScopeKind: scope.kind, ScopeID: scope.id})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if chains[row.RuleKey] == nil {
				chains[row.RuleKey] = &scopedRuleChain{}
			}
			scope.set(chains[row.RuleKey], &row)
		}
	}
	return chains, nil
}

// resolveScopedEvent decides what one captured event is worth now and delivers
// it. It returns the outcome to record; a non-nil error is a real failure that
// leaves the input pending.
func (s *IssueWakeupService) resolveScopedEvent(ctx context.Context, tx pgx.Tx, q *db.Queries, ev db.WakeupScopedEvent) (string, error) {
	issue, err := q.LockWakeupIssue(ctx, ev.IssueID)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && issue.WorkspaceID != ev.WorkspaceID {
		return scopedOutcomeIssueGone, nil
	} else if err != nil {
		return "", err
	}
	if active, err := wakeupIssueActive(ctx, q, issue); err != nil {
		return "", err
	} else if !active {
		return scopedOutcomeIssueClosed, nil
	}
	// Default events operate on open issues; backlog is dormant.
	if issuestatus.Effective(ctx, q, issue.WorkspaceID, issue.Status) == "backlog" {
		return scopedOutcomeIssueDormant, nil
	}
	// The event was captured under the project the issue was in. Another project's
	// rules never inherit it.
	if issue.ProjectID != ev.ProjectID {
		return scopedOutcomeScopeChanged, nil
	}
	chains, err := loadScopedChains(ctx, q, issue)
	if err != nil {
		return "", err
	}
	keys := make([]string, 0, len(chains))
	for key, chain := range chains {
		if chain.selects(ev.EventType) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	// One input may reach several rules; one that is refused does not stop the
	// others. When none is delivered the first reason is recorded.
	var instances []pgtype.UUID
	reason := scopedOutcomeNoRule
	for i, key := range keys {
		id, why, err := s.scopedInstanceFor(ctx, tx, q, issue, ev, key, chains[key])
		if err != nil {
			return "", err
		}
		if id.Valid {
			instances = append(instances, id)
		} else if i == 0 {
			reason = why
		}
	}
	if len(instances) == 0 {
		return reason, nil
	}
	if err := q.ReplayScopedWakeupEvent(ctx, db.ReplayScopedWakeupEventParams{
		IssueID: ev.IssueID, EventType: ev.EventType, EventKey: ev.EventKey, AgentID: ev.AgentID, SourceTaskID: ev.SourceTaskID,
		Payload: ev.Payload, CapturedAt: ev.CapturedAt, ActorType: ev.ActorType, ActorID: ev.ActorID, InstanceIds: instances,
	}); err != nil {
		return "", err
	}
	return scopedOutcomeDelivered, nil
}

// scopedInstanceFor resolves one rule for the issue and ensures its runtime
// instance. It returns the instance when the event may be delivered to it, or
// the reason it may not. Anything the stored configuration gets wrong is a
// reason (fail closed, isolated to this rule); only database failures are errors.
func (s *IssueWakeupService) scopedInstanceFor(ctx context.Context, tx pgx.Tx, q *db.Queries, issue db.Issue, ev db.WakeupScopedEvent, rule string, chain *scopedRuleChain) (pgtype.UUID, string, error) {
	in := WakeupResolveInput{RuleKey: rule, IssueProjectID: issue.ProjectID, IssueTarget: issue.AssigneeType.String + ":" + wakeupUUIDString(issue.AssigneeID)}
	var root *WakeupDefinition
	for _, row := range chain.all() {
		def, err := WakeupDefinitionFromRow(*row)
		if err != nil {
			return pgtype.UUID{}, scopedOutcomeInvalid, nil
		}
		// A definition written after the event was captured belongs to a newer
		// configuration: the event predates its activation and is not new work.
		if row.UpdatedAt.Time.After(ev.CapturedAt.Time) {
			return pgtype.UUID{}, scopedOutcomeDefinitionChange, nil
		}
		switch def.Scope {
		case WakeupScopeWorkspace:
			in.Workspace = &def
		case WakeupScopeProject:
			in.Project = &def
		case WakeupScopeIssue:
			in.Issue = &def
		}
		if def.Root {
			root = &def
		}
	}
	eff, err := ResolveWakeupConfig(in)
	if err != nil {
		return pgtype.UUID{}, scopedOutcomeInvalid, nil
	}
	if !eff.Applicable {
		return pgtype.UUID{}, scopedOutcomeNoRule, nil
	}
	if !eff.Enabled() {
		return pgtype.UUID{}, scopedOutcomeRuleDisabled, nil
	}
	events := eventTriggerEvents(eff.Config.Trigger)
	if !slices.Contains(events, ev.EventType) {
		return pgtype.UUID{}, scopedOutcomeNoMatch, nil
	}
	if !eff.Config.Instruction.Set || eff.Config.Instruction.Value == "" || root == nil {
		return pgtype.UUID{}, scopedOutcomeNoInstruction, nil
	}
	b := &builtinWakeup{Eff: eff}
	if eff.Config.Target.Set {
		if decodeWakeupSpec(eff.Config.Target.Value, &b.target) != nil || !validBuiltinTarget(b.target) {
			return pgtype.UUID{}, scopedOutcomeInvalid, nil
		}
	}
	// The member who wrote the layer that named the target answers for it; with
	// no named target, the member who wrote the most specific layer.
	layer, named := eff.Sources["target"]
	for _, def := range []*WakeupDefinition{in.Workspace, in.Project, in.Issue} {
		if def != nil && (!named || def.Scope == layer) {
			b.Authorizer = def.UpdatedBy
		}
	}
	target, err := builtinTarget(ctx, s, q, issue, b)
	if err != nil {
		return pgtype.UUID{}, "", err
	}
	if target.Refused != "" || !target.Agent.ID.Valid || !b.Authorizer.Valid {
		return pgtype.UUID{}, scopedOutcomeNoTarget, nil
	}
	return s.ensureScopedInstance(ctx, tx, q, issue, rule, root, eff, target, b.Authorizer, events)
}

// ensureScopedInstance finds or creates the issue's runtime instance of a rule
// and brings it to the configuration the rule now resolves to. A new instance
// is created disabled and enabled through the default capacity guard, so a full
// pool is a reason, never an error, and never fails the source write.
func (s *IssueWakeupService) ensureScopedInstance(ctx context.Context, tx pgx.Tx, q *db.Queries, issue db.Issue, rule string, root *WakeupDefinition,
	eff EffectiveWakeupConfig, target wakeTarget, member pgtype.UUID, events []string) (pgtype.UUID, string, error) {
	mode, maxFires := "continuous", pgtype.Int4{}
	if eff.Config.Mode.Set {
		mode = eff.Config.Mode.Value
	}
	if eff.Config.MaxFires.Set {
		maxFires = pgtype.Int4{Int32: int32(eff.Config.MaxFires.Value), Valid: true}
	}
	existing, err := q.GetDefaultWakeupInstance(ctx, db.GetDefaultWakeupInstanceParams{IssueID: issue.ID, RuleKey: rule})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		created, err := q.CreateDefaultWakeupInstance(ctx, db.CreateDefaultWakeupInstanceParams{
			ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, AgentID: target.Agent.ID, CreatedBy: member,
			Instruction: eff.Config.Instruction.Value, Mode: mode, EventTypes: events, MaxFires: maxFires,
			RuleKey: scopedText(rule), ScopeKind: scopedText(string(root.Scope)), ScopeID: root.ScopeID, Fingerprint: scopedText(eff.Fingerprint),
		})
		if err != nil {
			return pgtype.UUID{}, "", err
		}
		existing = created
	case err != nil:
		return pgtype.UUID{}, "", err
	case existing.ConfigFingerprint.String != eff.Fingerprint:
		if existing, err = q.RebaseDefaultWakeupInstance(ctx, db.RebaseDefaultWakeupInstanceParams{
			ID: existing.ID, AgentID: target.Agent.ID, CreatedBy: member, Instruction: eff.Config.Instruction.Value, Mode: mode,
			EventTypes: events, MaxFires: maxFires, Fingerprint: scopedText(eff.Fingerprint),
		}); err != nil {
			return pgtype.UUID{}, "", err
		}
	}
	if existing.Enabled {
		return existing.ID, "", nil
	}
	// Disabled by a pause or by a person: nothing a configuration edit lifts. A
	// default pool that was full is retried, as capacity may have freed.
	if !existing.CapacityReason.Valid && existing.DisabledAt.Valid || existing.PausedReason.Valid {
		return pgtype.UUID{}, scopedOutcomeInstanceOff, nil
	}
	admission, err := ApplyDefaultWakeupInstance(ctx, tx, existing.ID)
	if err != nil {
		return pgtype.UUID{}, "", err
	}
	if !admission.Applied {
		return pgtype.UUID{}, scopedOutcomeCapacity, nil
	}
	return existing.ID, "", nil
}

// pruneScopedEvents closes pending inputs past retention with a visible outcome
// and deletes closed ones, each in a bounded batch, so an input is never
// dropped silently and the table stays bounded without a long sweep.
func (s *IssueWakeupService) pruneScopedEvents(ctx context.Context, now time.Time) error {
	before := pgtype.Timestamptz{Time: now.Add(-scopedEventRetention), Valid: true}
	_, expireErr := s.Tasks.Queries.ExpireWakeupScopedEvents(ctx, db.ExpireWakeupScopedEventsParams{
		Now: pgtype.Timestamptz{Time: now, Valid: true}, Before: before, BatchSize: scopedPruneBatch,
	})
	_, deleteErr := s.Tasks.Queries.DeleteHandledWakeupScopedEvents(ctx, db.DeleteHandledWakeupScopedEventsParams{Before: before, BatchSize: scopedPruneBatch})
	return errors.Join(expireErr, deleteErr)
}

// ScopedEventAccounting counts a workspace's retained outbox inputs by outcome;
// an input not yet handled counts as "pending".
func (s *IssueWakeupService) ScopedEventAccounting(ctx context.Context, workspaceID pgtype.UUID) (map[string]int64, error) {
	rows, err := s.Tasks.Queries.CountWakeupScopedEventsByOutcome(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(rows))
	for _, r := range rows {
		out[r.Outcome] = r.Total
	}
	return out, nil
}
