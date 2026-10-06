package service

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// customCondition is the normalized predicate of a rule whose trigger is a
// condition; nil for every other trigger.
func customCondition(trigger wakeupObject) []byte {
	spec, ok := triggerSpec(trigger)
	if !ok || spec.Kind != wakeupTriggerKindCondition {
		return nil
	}
	return spec.Condition
}

// sameCondition compares two stored predicates by value: jsonb does not keep a
// document's key order or spacing.
func sameCondition(a, b []byte) bool {
	return string(canonicalWakeupPayload(a)) == string(canonicalWakeupPayload(b))
}

// customExpiry is when an instance ends: a fixed instant, or a wait from the
// instance's own creation, so it never restarts because another issue fired or
// an ancestor was edited. Unset means no end.
func customExpiry(c WakeupConfigPatch, created time.Time) pgtype.Timestamptz {
	var spec wakeupExpirySpec
	if !c.Expiry.Set || c.Expiry.Null || decodeWakeupSpec(c.Expiry.Value, &spec) != nil {
		return pgtype.Timestamptz{}
	}
	switch {
	case spec.At != nil:
		return pgtype.Timestamptz{Time: *spec.At, Valid: true}
	case spec.AfterSeconds != nil:
		return pgtype.Timestamptz{Time: created.Add(time.Duration(*spec.AfterSeconds) * time.Second), Valid: true}
	}
	return pgtype.Timestamptz{}
}

// customInstanceEnded reports an instance that rests for good until a person
// rearms it: paused (max fires, once, loop, rate), timed out, or turned off by a
// person or by its issue closing. No configuration edit and no capacity that
// frees lifts it, whether or not a full default pool had held it back first.
func customInstanceEnded(w db.IssueWakeup) bool {
	return w.PausedReason.Valid || w.TimedOutAt.Valid || w.DisabledAt.Valid
}

// ensureScopedInstance finds or creates the issue's runtime instance of a rule
// and brings it to the configuration the rule now resolves to. A new instance
// is created disabled and enabled through the default capacity guard, so a full
// pool is a reason, never an error, and never fails the source write. An
// instance that rests (see customInstanceEnded) stays at rest, however the
// configuration changed.
//
// A condition rule's instance carries its predicate and the facts it starts
// from. An instance created for a new rule acts on facts that are already true,
// as a newly created local condition does; one created by a later edit, by the
// rule reaching an issue that became eligible, or by a rebase to a changed
// predicate starts from them (baseline), so only a change wakes it. A rebase
// that leaves the predicate as it was keeps the recorded facts, so an
// instruction-only edit never manufactures a satisfied condition.
func (s *IssueWakeupService) ensureScopedInstance(ctx context.Context, b *scopedIssueBatch, r *scopedRule, target wakeTarget, baseline bool) (db.IssueWakeup, string, error) {
	q, tx, issue := b.q, b.tx, b.issue
	cfg := r.eff.Config
	mode, maxFires := "continuous", pgtype.Int4{}
	if cfg.Mode.Set {
		mode = cfg.Mode.Value
	}
	if cfg.MaxFires.Set {
		maxFires = pgtype.Int4{Int32: int32(cfg.MaxFires.Value), Valid: true}
	}
	now := time.Now()
	var next pgtype.Timestamptz
	if r.condition != nil {
		next = pgtype.Timestamptz{Time: now, Valid: true}
	}
	r.events = append([]string{}, r.events...) // a condition without hint events subscribes to none
	existing, err := q.GetDefaultWakeupInstance(ctx, db.GetDefaultWakeupInstanceParams{IssueID: issue.ID, RuleKey: r.key})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		created, err := q.CreateDefaultWakeupInstance(ctx, db.CreateDefaultWakeupInstanceParams{
			ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, AgentID: target.Agent.ID, CreatedBy: r.member,
			Instruction: cfg.Instruction.Value, Mode: mode, EventTypes: r.events, MaxFires: maxFires,
			RuleKey: scopedText(r.key), ScopeKind: scopedText(string(r.root.Scope)), ScopeID: r.root.ScopeID, Fingerprint: scopedText(r.eff.Fingerprint),
			Condition: r.condition, NextFireAt: next, ExpiresAt: customExpiry(cfg, now),
		})
		if err != nil {
			return db.IssueWakeup{}, "", err
		}
		existing = created
		if r.condition != nil && (baseline || conditionFiresOnChange(r.condition)) {
			if err := baselineCondition(ctx, tx, q, existing, now); err != nil {
				return db.IssueWakeup{}, "", err
			}
		}
	case err != nil:
		return db.IssueWakeup{}, "", err
	case existing.ConfigFingerprint.String != r.eff.Fingerprint:
		if existing, err = b.rebase(ctx, existing, r, target, mode, maxFires, now); err != nil {
			return db.IssueWakeup{}, "", err
		}
	}
	if existing.Enabled {
		return existing, "", nil
	}
	if customInstanceEnded(existing) {
		return db.IssueWakeup{}, scopedOutcomeInstanceOff, nil
	}
	// A default pool that was full is retried, as capacity may have freed; an
	// instance a retired rule left idle is applied again like a new one.
	admission, err := ApplyDefaultWakeupInstance(ctx, tx, existing.ID)
	if err != nil {
		return db.IssueWakeup{}, "", err
	}
	if !admission.Applied {
		return db.IssueWakeup{}, scopedOutcomeCapacity, nil
	}
	return existing, "", nil
}

// rebase moves an instance to the configuration its rule now resolves to. The
// queued runs of the old configuration are withdrawn.
func (b *scopedIssueBatch) rebase(ctx context.Context, w db.IssueWakeup, r *scopedRule, target wakeTarget, mode string, maxFires pgtype.Int4, now time.Time) (db.IssueWakeup, error) {
	cfg := r.eff.Config
	state, rebaseline := w.ConditionState, r.condition != nil && !sameCondition(w.Condition, r.condition)
	if rebaseline || r.condition == nil {
		state = ""
	}
	var next pgtype.Timestamptz
	if r.condition != nil {
		next = pgtype.Timestamptz{Time: now, Valid: true}
	}
	out, err := b.q.RebaseDefaultWakeupInstance(ctx, db.RebaseDefaultWakeupInstanceParams{
		ID: w.ID, AgentID: target.Agent.ID, CreatedBy: r.member, Instruction: cfg.Instruction.Value, Mode: mode,
		EventTypes: r.events, MaxFires: maxFires, Fingerprint: scopedText(r.eff.Fingerprint),
		Condition: r.condition, ConditionState: state, NextFireAt: next, ExpiresAt: customExpiry(cfg, w.CreatedAt.Time),
	})
	if err != nil {
		return w, err
	}
	cancelled, err := b.q.CancelUnstartedWakeupTasks(ctx, util.UUIDToString(w.ID))
	if err != nil {
		return w, err
	}
	b.withdrawn = append(b.withdrawn, cancelled...)
	if rebaseline {
		err = baselineCondition(ctx, b.tx, b.q, out, now)
	}
	return out, err
}

// retire takes an instance whose rule no longer runs for the issue (disabled,
// deleted, or the issue moved to another project) out of its pool and drops what
// it was waiting on. Its identity, history and once state stay, so the rule
// returning later finds the same instance.
func (b *scopedIssueBatch) retire(ctx context.Context, w db.IssueWakeup) error {
	if !w.Enabled {
		return nil
	}
	if _, err := b.tx.Exec(ctx, `UPDATE issue_wakeup SET enabled=false,next_fire_at=NULL,updated_at=clock_timestamp() WHERE id=$1`, w.ID); err != nil {
		return err
	}
	if err := b.q.DiscardWakeupReceipts(ctx, w.ID); err != nil {
		return err
	}
	cancelled, err := b.q.CancelUnstartedWakeupTasks(ctx, util.UUIDToString(w.ID))
	b.withdrawn = append(b.withdrawn, cancelled...)
	return err
}

// publishWithdrawn announces the queued runs a committed transaction cancelled.
func (s *IssueWakeupService) publishWithdrawn(ctx context.Context, tasks []db.AgentTaskQueue) {
	for _, task := range tasks {
		s.Tasks.broadcastTaskEvent(ctx, protocol.EventTaskCancelled, task)
	}
}
