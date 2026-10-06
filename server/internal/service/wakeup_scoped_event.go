package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// Scoped-event capture and drain (CHE-1082 L9). The ordinary event triggers
// write a reference-only row to wakeup_scoped_event, in the source transaction,
// when a definition selecting the event exists in the issue's workspace, current
// project or issue scope (migration 597). The scheduler drains those rows here:
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

// DrainScopedEvents handles one bounded pass over the pending outbox inputs of
// the given workspaces. Tests use it; the scheduler's tick drains every
// workspace.
func (s *IssueWakeupService) DrainScopedEvents(ctx context.Context, workspaceIDs ...pgtype.UUID) error {
	if len(workspaceIDs) == 0 {
		return nil
	}
	return s.drainScopedEvents(ctx, workspaceIDs)
}

// A busy issue (held by another writer) is never waited for: the drain tries its
// lock with SKIP LOCKED, notes the issue as cooling and goes on to other work, so
// the issues at the head of the queue cannot hold back what is behind them. The
// cooldown lives in memory (the scheduler job keeps one service instance), costs
// no outbox write and leaves every input pending and retryable. It is longer
// than the scheduler's 30 s cadence, so the next tick moves past the issue
// instead of trying it again; a restart or a late tick only means one more try.
// scopedBusyAttempts bounds how many busy issues one pass notes before it leaves
// the rest to the next tick. A pass that reaches the bound has found a busy head
// longer than itself, so it parks the issues it noted for scopedBusyBackoff
// instead: the next ticks then move on to the issues behind them, and progress
// reaches the healthy work without depending on any busy issue clearing.
const (
	scopedBusyCooldown = 35 * time.Second
	scopedBusyBackoff  = 10 * time.Minute
	// scopedCoolingMax bounds the in-memory cooldown set.
	scopedCoolingMax = 1000
)

var scopedBusyAttempts = 50

// errScopedIssueBusy marks an issue another writer held when the drain tried it.
var errScopedIssueBusy = errors.New("issue held by another writer")

// scopedBusyState holds the issues the drain found busy and until when to leave
// them alone.
type scopedBusyState struct {
	mu    sync.Mutex
	until map[pgtype.UUID]time.Time
}

// cooling returns the issues still cooling at now, dropping the expired ones.
func (b *scopedBusyState) cooling(now time.Time) []pgtype.UUID {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]pgtype.UUID, 0, len(b.until))
	for id, until := range b.until {
		if until.After(now) {
			out = append(out, id)
		} else {
			delete(b.until, id)
		}
	}
	return out
}

func (b *scopedBusyState) cool(id pgtype.UUID, until time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.until == nil {
		b.until = map[pgtype.UUID]time.Time{}
	}
	if _, known := b.until[id]; !known && len(b.until) >= scopedCoolingMax {
		return // the in-pass skip list still covers this pass
	}
	b.until[id] = until
}

// scopedPass is what one transaction of the drain did. failed names the input a
// returned error belongs to; busy names the issue another writer held.
type scopedPass struct {
	handled      int
	failed, busy pgtype.UUID
}

// drainScopedEvents handles at most scopedDrainBatch inputs, one transaction
// per issue. A transaction claims the oldest unclaimed input with SKIP LOCKED
// and with it that issue's other pending inputs, so concurrent schedulers take
// different issues and never wait for each other, a burst on one issue is
// resolved once, and its inputs coalesce in the receipts they are delivered to.
// An input that fails on a real database error rolls its transaction back, is
// scheduled for a later retry and reported; the issue's other inputs are
// claimed again by the next transaction. An issue another writer holds is
// contention, not a fault: the pass skips it, carries on with other work and
// leaves its inputs pending for the next pass, with no retry delay.
func (s *IssueWakeupService) drainScopedEvents(ctx context.Context, workspaceIDs []pgtype.UUID) error {
	var errs []error
	now := time.Now()
	cooling := s.scopedBusy.cooling(s.scopedNow())
	var noted []pgtype.UUID // the busy issues this pass found
	for budget := scopedDrainBatch; budget > 0; {
		if ctx.Err() != nil {
			return errors.Join(append(errs, ctx.Err())...)
		}
		pass, err := s.drainOneScopedIssue(ctx, workspaceIDs, now, budget, cooling)
		if scopedLockBusy(err) && pass.busy.Valid {
			s.scopedBusy.cool(pass.busy, s.scopedNow().Add(scopedBusyCooldown))
			cooling, noted = append(cooling, pass.busy), append(noted, pass.busy)
			if len(noted) >= scopedBusyAttempts {
				for _, id := range noted {
					s.scopedBusy.cool(id, s.scopedNow().Add(scopedBusyBackoff))
				}
				break
			}
			continue
		}
		if err != nil {
			errs = append(errs, err)
			if !pass.failed.Valid {
				return errors.Join(errs...)
			}
			if deferErr := s.Tasks.Queries.DeferWakeupScopedEvent(ctx, db.DeferWakeupScopedEventParams{ID: pass.failed, RetryAt: pgtype.Timestamptz{Time: now.Add(scopedRetryDelay), Valid: true}}); deferErr != nil {
				return errors.Join(append(errs, deferErr)...)
			}
			budget--
			continue
		}
		if pass.handled == 0 {
			break
		}
		budget -= pass.handled
	}
	return errors.Join(errs...)
}

// scopedNow is the drain's cooldown clock.
func (s *IssueWakeupService) scopedNow() time.Time {
	if s.scopedClock != nil {
		return s.scopedClock()
	}
	return time.Now()
}

// scopedLockBusy reports an issue another transaction held: found locked by the
// drain's no-wait try, a lock wait that gave up (lock_timeout) or a lost deadlock.
func scopedLockBusy(err error) bool {
	var pg *pgconn.PgError
	return errors.Is(err, errScopedIssueBusy) || errors.As(err, &pg) && (pg.Code == "55P03" || pg.Code == "40P01")
}

// drainOneScopedIssue claims, resolves and settles the pending inputs of one
// issue, at most budget of them, leaving the issues in skip alone.
func (s *IssueWakeupService) drainOneScopedIssue(ctx context.Context, workspaceIDs []pgtype.UUID, now time.Time, budget int, skip []pgtype.UUID) (pass scopedPass, err error) {
	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return pass, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '200ms'"); err != nil {
		return pass, err
	}
	q := s.Tasks.Queries.WithTx(tx)
	stamp := pgtype.Timestamptz{Time: now, Valid: true}
	// An input past retention is never claimed: it can only expire.
	oldest := pgtype.Timestamptz{Time: now.Add(-scopedEventRetention), Valid: true}
	first, err := q.ClaimWakeupScopedEvents(ctx, db.ClaimWakeupScopedEventsParams{Now: stamp, Oldest: oldest, WorkspaceIds: workspaceIDs, SkipIssues: skip, BatchSize: 1})
	if err != nil || len(first) == 0 {
		return pass, err
	}
	events := first
	if budget > 1 {
		rest, err := q.ClaimWakeupScopedEventsOfIssue(ctx, db.ClaimWakeupScopedEventsOfIssueParams{IssueID: first[0].IssueID, ExceptID: first[0].ID, Now: stamp, Oldest: oldest, BatchSize: int32(budget - 1)})
		if err != nil {
			// A deadlock here is contention on this issue like any other.
			pass.busy = first[0].IssueID
			return pass, err
		}
		events = append(events, rest...)
	}
	batch, err := s.newScopedIssueBatch(ctx, tx, q, first[0])
	if err != nil {
		pass.failed, pass.busy = first[0].ID, first[0].IssueID
		return pass, fmt.Errorf("scoped event %s: %w", util.UUIDToString(first[0].ID), err)
	}
	for _, ev := range events {
		outcome, err := batch.resolve(ctx, ev)
		if err == nil {
			err = q.MarkWakeupScopedEventHandled(ctx, db.MarkWakeupScopedEventHandledParams{ID: ev.ID, Now: stamp, Outcome: outcome})
		}
		if err != nil {
			pass.failed, pass.busy = ev.ID, ev.IssueID
			return scopedPass{failed: ev.ID, busy: ev.IssueID}, fmt.Errorf("scoped event %s: %w", util.UUIDToString(ev.ID), err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return pass, err
	}
	pass.handled = len(events)
	return pass, nil
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

// scopedRule is one rule resolved once for the issue and reused by every input
// of the transaction. ensured caches the instance, or the reason there is none,
// after the first input that may be delivered.
type scopedRule struct {
	key     string
	chain   *scopedRuleChain
	invalid bool
	// definitions is the rule's stored chain in the form capture records it.
	definitions []string
	eff         EffectiveWakeupConfig
	root        *WakeupDefinition
	member      pgtype.UUID
	events      []string
	ensured     bool
	instance    db.IssueWakeup
	reason      string
}

// scopedDefinitionStamp is one stored definition as capture records it:
// scope:rule:revision:updated-micros (migration 597).
func scopedDefinitionStamp(d *db.IssueWakeupDefinition) string {
	return d.ScopeKind + ":" + d.RuleKey + ":" + strconv.FormatInt(d.Revision, 10) + ":" + strconv.FormatInt(d.UpdatedAt.Time.UnixMicro(), 10)
}

// capturedUnderThisChain reports whether the definitions the event was captured
// under, for this rule, are exactly the ones stored now.
func (r *scopedRule) capturedUnderThisChain(ev db.WakeupScopedEvent) bool {
	var then []string
	for _, stamp := range ev.Chain {
		if parts := strings.SplitN(stamp, ":", 3); len(parts) == 3 && parts[1] == r.key {
			then = append(then, stamp)
		}
	}
	now := slices.Clone(r.definitions)
	slices.Sort(then)
	slices.Sort(now)
	return slices.Equal(then, now)
}

// scopedIssueBatch is the state of draining one issue's inputs in one
// transaction: the locked issue and its rules, resolved once.
type scopedIssueBatch struct {
	s     *IssueWakeupService
	tx    pgx.Tx
	q     *db.Queries
	issue db.Issue
	// outcome is set when the issue itself settles every input the same way.
	outcome string
	keys    []string
	rules   map[string]*scopedRule
}

func (s *IssueWakeupService) newScopedIssueBatch(ctx context.Context, tx pgx.Tx, q *db.Queries, first db.WakeupScopedEvent) (*scopedIssueBatch, error) {
	b := &scopedIssueBatch{s: s, tx: tx, q: q, rules: map[string]*scopedRule{}}
	issue, err := q.TryLockWakeupIssue(ctx, first.IssueID)
	if errors.Is(err, pgx.ErrNoRows) {
		// No row: the issue is held by another writer, or gone.
		exists, existsErr := q.WakeupIssueExists(ctx, first.IssueID)
		if existsErr != nil {
			return nil, existsErr
		}
		if exists {
			return nil, errScopedIssueBusy
		}
		b.outcome = scopedOutcomeIssueGone
		return b, nil
	} else if err != nil {
		return nil, err
	} else if issue.WorkspaceID != first.WorkspaceID {
		b.outcome = scopedOutcomeIssueGone
		return b, nil
	}
	b.issue = issue
	if active, err := wakeupIssueActive(ctx, q, issue); err != nil {
		return nil, err
	} else if !active {
		b.outcome = scopedOutcomeIssueClosed
		return b, nil
	}
	// Default events operate on open issues; backlog is dormant.
	if issuestatus.Effective(ctx, q, issue.WorkspaceID, issue.Status) == "backlog" {
		b.outcome = scopedOutcomeIssueDormant
		return b, nil
	}
	chains, err := loadScopedChains(ctx, q, issue)
	if err != nil {
		return nil, err
	}
	for key, chain := range chains {
		b.keys = append(b.keys, key)
		b.rules[key] = newScopedRule(issue, key, chain)
	}
	slices.Sort(b.keys)
	return b, nil
}

// newScopedRule resolves a rule's configuration from its stored definitions. A
// definition this build cannot read makes the whole rule invalid (fail closed,
// isolated to this rule).
func newScopedRule(issue db.Issue, key string, chain *scopedRuleChain) *scopedRule {
	r := &scopedRule{key: key, chain: chain}
	in := WakeupResolveInput{RuleKey: key, IssueProjectID: issue.ProjectID, IssueTarget: issue.AssigneeType.String + ":" + wakeupUUIDString(issue.AssigneeID)}
	for _, row := range chain.all() {
		def, err := WakeupDefinitionFromRow(*row)
		if err != nil {
			r.invalid = true
			return r
		}
		r.definitions = append(r.definitions, scopedDefinitionStamp(row))
		switch def.Scope {
		case WakeupScopeWorkspace:
			in.Workspace = &def
		case WakeupScopeProject:
			in.Project = &def
		case WakeupScopeIssue:
			in.Issue = &def
		}
		if def.Root {
			r.root = &def
		}
	}
	eff, err := ResolveWakeupConfig(in)
	if err != nil {
		r.invalid = true
		return r
	}
	r.eff, r.events = eff, eventTriggerEvents(eff.Config.Trigger)
	// The member who wrote the layer that named the target answers for it; with
	// no named target, the member who wrote the most specific layer.
	layer, named := eff.Sources["target"]
	for _, def := range []*WakeupDefinition{in.Workspace, in.Project, in.Issue} {
		if def != nil && (!named || def.Scope == layer) {
			r.member = def.UpdatedBy
		}
	}
	return r
}

// resolve decides what one captured event is worth now and delivers it into the
// issue's instances. It returns the outcome to record; a non-nil error is a real
// failure that leaves the input pending.
func (b *scopedIssueBatch) resolve(ctx context.Context, ev db.WakeupScopedEvent) (string, error) {
	if b.outcome != "" {
		return b.outcome, nil
	}
	// The event was captured under the project the issue was in. Another project's
	// rules never inherit it.
	if b.issue.ProjectID != ev.ProjectID {
		return scopedOutcomeScopeChanged, nil
	}
	// One input may reach several rules; one that is refused does not stop the
	// others. When none is delivered the first reason is recorded.
	var replay []pgtype.UUID
	served := 0
	reason := scopedOutcomeNoRule
	first := true
	for _, key := range b.keys {
		r := b.rules[key]
		if !r.chain.selects(ev.EventType) {
			continue
		}
		inst, why, err := b.deliverable(ctx, r, ev)
		if err != nil {
			return "", err
		}
		switch {
		case !inst.ID.Valid:
			if first {
				reason = why
			}
		case slices.Contains(ev.Delivered, util.UUIDToString(inst.ID)+":"+strconv.FormatInt(inst.Revision, 10)):
			// Live capture delivered it to this configuration of the instance;
			// replaying would count it twice and make an older event the latest.
			served++
		default:
			replay = append(replay, inst.ID)
		}
		first = false
	}
	if len(replay) > 0 {
		if err := b.q.ReplayScopedWakeupEvent(ctx, db.ReplayScopedWakeupEventParams{
			IssueID: ev.IssueID, EventType: ev.EventType, EventKey: ev.EventKey, AgentID: ev.AgentID, SourceTaskID: ev.SourceTaskID,
			Payload: ev.Payload, CapturedAt: ev.CapturedAt, ActorType: ev.ActorType, ActorID: ev.ActorID, InstanceIds: replay,
		}); err != nil {
			return "", err
		}
	}
	if len(replay)+served == 0 {
		return reason, nil
	}
	return scopedOutcomeDelivered, nil
}

// deliverable returns the instance an event may be delivered to for one rule,
// or the reason it may not. Anything the stored configuration gets wrong is a
// reason; only database failures are errors.
func (b *scopedIssueBatch) deliverable(ctx context.Context, r *scopedRule, ev db.WakeupScopedEvent) (db.IssueWakeup, string, error) {
	switch {
	case r.invalid:
		return db.IssueWakeup{}, scopedOutcomeInvalid, nil
	case !r.capturedUnderThisChain(ev):
		// The rule's definitions were written, reset, deleted or recreated after the
		// event was captured: it belongs to another configuration, predates its
		// activation and is not new work. Comparing what capture recorded survives a
		// deletion, which leaves no newer definition behind to find.
		return db.IssueWakeup{}, scopedOutcomeDefinitionChange, nil
	case !r.eff.Applicable:
		return db.IssueWakeup{}, scopedOutcomeNoRule, nil
	case !r.eff.Enabled():
		return db.IssueWakeup{}, scopedOutcomeRuleDisabled, nil
	case !slices.Contains(r.events, ev.EventType):
		return db.IssueWakeup{}, scopedOutcomeNoMatch, nil
	}
	if !r.ensured {
		var err error
		if r.instance, r.reason, err = b.ensure(ctx, r); err != nil {
			return db.IssueWakeup{}, "", err
		}
		r.ensured = true
	}
	return r.instance, r.reason, nil
}

// ensure resolves the rule's target and ensures the issue's runtime instance.
func (b *scopedIssueBatch) ensure(ctx context.Context, r *scopedRule) (db.IssueWakeup, string, error) {
	c := r.eff.Config
	if !c.Instruction.Set || c.Instruction.Value == "" || r.root == nil {
		return db.IssueWakeup{}, scopedOutcomeNoInstruction, nil
	}
	// What an instance can hold is checked before any database write: a value the
	// table would refuse is this rule's invalid definition, not a database error
	// that rolls back every other rule's delivery.
	if (c.Mode.Set && c.Mode.Value != "once" && c.Mode.Value != "continuous") ||
		(c.MaxFires.Set && (c.MaxFires.Value < 1 || c.MaxFires.Value > maxWakeupDefinitionMaxFires)) ||
		len(c.Instruction.Value) > maxWakeupDefinitionInstruction {
		return db.IssueWakeup{}, scopedOutcomeInvalid, nil
	}
	w := &builtinWakeup{Eff: r.eff, Authorizer: r.member}
	if c.Target.Set {
		if decodeWakeupSpec(c.Target.Value, &w.target) != nil || !validBuiltinTarget(w.target) {
			return db.IssueWakeup{}, scopedOutcomeInvalid, nil
		}
	}
	target, err := builtinTarget(ctx, b.s, b.q, b.issue, w)
	if err != nil {
		return db.IssueWakeup{}, "", err
	}
	if target.Refused != "" || !target.Agent.ID.Valid || !r.member.Valid {
		return db.IssueWakeup{}, scopedOutcomeNoTarget, nil
	}
	return b.s.ensureScopedInstance(ctx, b.tx, b.q, b.issue, r.key, r.root, r.eff, target, r.member, r.events)
}

// ensureScopedInstance finds or creates the issue's runtime instance of a rule
// and brings it to the configuration the rule now resolves to. A new instance
// is created disabled and enabled through the default capacity guard, so a full
// pool is a reason, never an error, and never fails the source write.
func (s *IssueWakeupService) ensureScopedInstance(ctx context.Context, tx pgx.Tx, q *db.Queries, issue db.Issue, rule string, root *WakeupDefinition,
	eff EffectiveWakeupConfig, target wakeTarget, member pgtype.UUID, events []string) (db.IssueWakeup, string, error) {
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
			return db.IssueWakeup{}, "", err
		}
		existing = created
	case err != nil:
		return db.IssueWakeup{}, "", err
	case existing.ConfigFingerprint.String != eff.Fingerprint:
		if existing, err = q.RebaseDefaultWakeupInstance(ctx, db.RebaseDefaultWakeupInstanceParams{
			ID: existing.ID, AgentID: target.Agent.ID, CreatedBy: member, Instruction: eff.Config.Instruction.Value, Mode: mode,
			EventTypes: events, MaxFires: maxFires, Fingerprint: scopedText(eff.Fingerprint),
		}); err != nil {
			return db.IssueWakeup{}, "", err
		}
	}
	if existing.Enabled {
		return existing, "", nil
	}
	// Disabled by a pause or by a person: nothing a configuration edit lifts. A
	// default pool that was full is retried, as capacity may have freed.
	if !existing.CapacityReason.Valid && existing.DisabledAt.Valid || existing.PausedReason.Valid {
		return db.IssueWakeup{}, scopedOutcomeInstanceOff, nil
	}
	admission, err := ApplyDefaultWakeupInstance(ctx, tx, existing.ID)
	if err != nil {
		return db.IssueWakeup{}, "", err
	}
	if !admission.Applied {
		return db.IssueWakeup{}, scopedOutcomeCapacity, nil
	}
	return existing, "", nil
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
