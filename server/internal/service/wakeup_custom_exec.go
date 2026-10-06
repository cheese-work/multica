package service

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Custom rule execution (CHE-1082 L10). The instance of a custom event or
// condition rule is an ordinary issue_wakeup row, so receipts, the condition
// evaluator, runaway protection, joined runs and the claim checks are the ones
// local wakeups already use. What the scoped configuration adds is applied here:
// the rule is resolved for the issue at every step (dispatch, join, claim and
// start), and an instance whose configuration moved is rebased, never run under
// the new target or instruction with the old inputs.

// loadCustomWakeup resolves the rule of a default-derived instance for its
// issue. It returns nil, nil when no definition of the rule applies any more,
// and errWakeupConfigHeld for one this build cannot execute.
func loadCustomWakeup(ctx context.Context, q *db.Queries, issue db.Issue, w db.IssueWakeup) (*scopedRule, *builtinWakeup, error) {
	key := w.DefaultRuleKey.String
	rows, err := q.ListWakeupDefinitionsForRule(ctx, db.ListWakeupDefinitionsForRuleParams{WorkspaceID: issue.WorkspaceID, RuleKey: key, ProjectID: issue.ProjectID, IssueID: issue.ID})
	if err != nil || len(rows) == 0 {
		return nil, nil, err
	}
	chain := &scopedRuleChain{}
	for i := range rows {
		switch WakeupScope(rows[i].ScopeKind) {
		case WakeupScopeWorkspace:
			chain.workspace = &rows[i]
		case WakeupScopeProject:
			chain.project = &rows[i]
		case WakeupScopeIssue:
			chain.issue = &rows[i]
		}
	}
	r := newScopedRule(issue, key, chain)
	if r.invalid {
		return nil, nil, heldConfig("unreadable definition of rule %s", key)
	}
	b := &builtinWakeup{Eff: r.eff, Authorizer: r.member}
	c := r.eff.Config
	switch {
	case c.Schedule.Set:
		return nil, nil, heldConfig("schedule is not executed yet")
	case c.ActiveRun.Set && !validWakeupActiveRun(c.ActiveRun.Value):
		return nil, nil, heldConfig("unreadable active_run")
	case r.condition == nil && len(r.events) == 0 && c.Trigger.Set:
		return nil, nil, heldConfig("unreadable trigger")
	}
	for _, cap := range r.eff.AggregateCaps {
		if cap.Limit < 1 {
			return nil, nil, heldConfig("unreadable aggregate limit")
		}
	}
	if err := b.decodeSpecs(); err != nil {
		return nil, nil, err
	}
	return r, b, nil
}

// customDefers is the rule's active-run choice. A custom rule that names none
// defers: its facts wait for the target to go idle instead of being dropped.
func (b *builtinWakeup) customDefers() bool {
	return !b.Eff.Config.ActiveRun.Set || b.Eff.Config.ActiveRun.Value == wakeupActiveRunDefer
}

type customVerdict int

const (
	// customRun: the instance runs the configuration it holds.
	customRun customVerdict = iota
	// customHold: the stored configuration cannot be executed; inputs stay pending.
	customHold
	// customOff: the rule no longer runs for this issue; the instance was retired.
	customOff
	// customMoved: the instance moved to a new configuration; its old inputs and
	// queued runs were retired and the next pass runs the new one.
	customMoved
)

// customGate decides what a default-derived instance may do now, inside the
// dispatch transaction. withdrawn are runs it cancelled, for the caller to
// announce after commit. target is who the rule reaches for this issue now.
func (s *IssueWakeupService) customGate(ctx context.Context, tx pgx.Tx, q *db.Queries, issue db.Issue, w db.IssueWakeup) (cfg *builtinWakeup, target wakeTarget, verdict customVerdict, withdrawn []db.AgentTaskQueue, err error) {
	r, cfg, err := loadCustomWakeup(ctx, q, issue, w)
	if errors.Is(err, errWakeupConfigHeld) {
		logWakeupHeld(ctx, issue, w, err)
		return nil, target, customHold, nil, nil
	}
	if err != nil {
		return nil, target, customHold, nil, err
	}
	b := &scopedIssueBatch{s: s, tx: tx, q: q, issue: issue}
	if r == nil || !r.eff.Applicable || !r.eff.Enabled() {
		err = b.retire(ctx, w)
		return nil, target, customOff, b.withdrawn, err
	}
	if target, err = builtinTarget(ctx, s, q, issue, cfg); err != nil {
		return nil, target, customHold, nil, err
	}
	// The fingerprint covers the definitions, the project and the assignee; a
	// squad's leader is not in it, so the agent the instance holds is compared too.
	if w.ConfigFingerprint.String == r.eff.Fingerprint && (!target.Agent.ID.Valid || target.Agent.ID == w.AgentID) {
		return cfg, target, customRun, nil, nil
	}
	if target.Refused != "" || !target.Agent.ID.Valid {
		// The new configuration cannot run: its target is gone or no longer
		// allowed for its author. The instance rests; an edit or the next
		// activation applies it again.
		if err = q.NoteWakeupFailure(ctx, db.NoteWakeupFailureParams{ID: w.ID, LastError: pgtype.Text{String: "Wakeup target unavailable: " + target.Refused, Valid: true}}); err == nil {
			err = b.retire(ctx, w)
		}
		return nil, target, customOff, b.withdrawn, err
	}
	mode, maxFires := scopedInstanceFields(r.eff.Config)
	if _, err = b.rebase(ctx, w, r, target, mode, maxFires, time.Now()); err != nil {
		return nil, target, customHold, nil, err
	}
	return nil, target, customMoved, b.withdrawn, nil
}

// customPrecheck stops a custom instance's pass before its inputs are looked at
// when none of them may run: the issue is parked in backlog, does not match the
// rule's filters, or the fire limit (which an edit may have lowered) is already
// spent. Facts that cannot run are dropped
// with a visible outcome; they never wait for the issue to qualify.
func customPrecheck(ctx context.Context, q *db.Queries, issue db.Issue, w db.IssueWakeup, cfg *builtinWakeup,
	note func(string, map[string]any) error) (stop bool, err error) {
	outcome := ""
	limit, limited := cfg.fireLimit()
	switch {
	case issuestatus.Effective(ctx, q, issue.WorkspaceID, issue.Status) == "backlog":
		outcome = scopedOutcomeIssueDormant
	case limited && w.FireCount >= limit:
		outcome = wakeupPausedMaxFires
	default:
		eligible, err := cfg.matchesIssue(ctx, q, issue)
		if err != nil || eligible {
			return false, err
		}
		outcome = wakeupOutcomeFiltered
	}
	if err := q.DiscardWakeupReceipts(ctx, w.ID); err != nil {
		return false, err
	}
	if outcome == wakeupPausedMaxFires {
		if !systemWakeupPaused(w) {
			if err := q.PauseIssueWakeup(ctx, db.PauseIssueWakeupParams{ID: w.ID, PausedReason: pgtype.Text{String: wakeupPausedMaxFires, Valid: true}}); err != nil {
				return false, err
			}
			err = note(wakeupActivityPaused, map[string]any{"rule": w.DefaultRuleKey.String, "reason": wakeupPausedMaxFires, "limit": limit})
		}
		return true, err
	}
	// Dormancy is the issue's state, not the rule's: nothing worth recording.
	if outcome != scopedOutcomeIssueDormant {
		err = note(wakeupActivityTriggered, map[string]any{"rule": w.DefaultRuleKey.String, "outcome": outcome})
	}
	return true, err
}

// scopedInstanceFields are the mode and fire cap an instance takes from its
// resolved configuration.
func scopedInstanceFields(c WakeupConfigPatch) (string, pgtype.Int4) {
	mode, maxFires := "continuous", pgtype.Int4{}
	if c.Mode.Set {
		mode = c.Mode.Value
	}
	if c.MaxFires.Set {
		maxFires = pgtype.Int4{Int32: int32(c.MaxFires.Value), Valid: true}
	}
	return mode, maxFires
}

// customCurrent resolves a default-derived instance for the checks made outside
// dispatch (join, claim, start): the configuration it runs, only when the rule
// is on for the issue and the instance still holds what the rule resolves to now.
func (s *IssueWakeupService) customCurrent(ctx context.Context, q *db.Queries, issue db.Issue, w db.IssueWakeup) (*builtinWakeup, bool, error) {
	_, cfg, err := loadCustomWakeup(ctx, q, issue, w)
	if errors.Is(err, errWakeupConfigHeld) {
		logWakeupHeld(ctx, issue, w, err)
		return nil, false, nil
	}
	if err != nil || cfg == nil {
		return nil, false, err
	}
	if !cfg.Eff.Applicable || !cfg.enabled() || w.ConfigFingerprint.String != cfg.Eff.Fingerprint {
		return nil, false, nil
	}
	return cfg, true, nil
}

// customRunEligible applies what can change after a run was queued: the expiry
// window, the issue's labels and priority, backlog, and whether the target still
// resolves to the run's agent and may be used by the rule's author.
func (s *IssueWakeupService) customRunEligible(ctx context.Context, q *db.Queries, issue db.Issue, w db.IssueWakeup, cfg *builtinWakeup, task db.AgentTaskQueue) (bool, error) {
	if w.ExpiresAt.Valid && !w.ExpiresAt.Time.After(time.Now()) || w.DisabledAt.Valid {
		return false, nil
	}
	if ok, err := cfg.matchesIssue(ctx, q, issue); err != nil || !ok {
		return false, err
	}
	target, err := builtinTarget(ctx, s, q, issue, cfg)
	if err != nil || target.Refused != "" || target.Agent.ID != task.AgentID {
		return false, err
	}
	return cfg.carrierMatchesTarget(target, task), nil
}

// customActiveRun holds a custom rule's facts back while its target is running
// on the issue: it defers them until the target is idle (the default) or, when
// the rule says suppress, drops what arrived after the run started. The running
// prompt is never edited. It reports whether the pass is over.
func (s *IssueWakeupService) customActiveRun(ctx context.Context, q *db.Queries, w db.IssueWakeup, cfg *builtinWakeup, target wakeTarget,
	receipts []db.IssueWakeupReceipt, deferring bool, now time.Time, next pgtype.Timestamptz, note func(string, map[string]any) error) (bool, error) {
	startedAt, err := q.GetRunningTaskStartForIssueAndAgent(ctx, db.GetRunningTaskStartForIssueAndAgentParams{IssueID: w.IssueID, AgentID: w.AgentID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Timers advance either way.
	if err := q.AdvanceIssueWakeup(ctx, db.AdvanceIssueWakeupParams{ID: w.ID, Enabled: w.Enabled, NextFireAt: next}); err != nil {
		return false, err
	}
	if deferring {
		return true, deferForActiveRun(ctx, q, w, target, receipts, now, note)
	}
	var suppressed []db.IssueWakeupReceipt
	for _, r := range receipts {
		if !r.CreatedAt.Valid || !r.CreatedAt.Time.Before(startedAt.Time) {
			suppressed = append(suppressed, r)
		}
	}
	if len(suppressed) == 0 {
		return true, nil
	}
	facts := systemWakeupFacts(w, suppressed)
	facts["target_type"], facts["target_id"], facts["outcome"] = target.Type, util.UUIDToString(target.ID), "suppressed_active_run"
	if err := q.ConsumeWakeupReceipts(ctx, db.ConsumeWakeupReceiptsParams{Ids: receiptIDs(suppressed)}); err != nil {
		return false, err
	}
	return true, note(wakeupActivityTriggered, facts)
}

// admitCustomStart counts a new paid start against every aggregate cap that
// applies. A full counter delays the instance: its facts stay pending and it is
// looked at again when the counter may have room.
func (s *IssueWakeupService) admitCustomStart(ctx context.Context, q *db.Queries, issue db.Issue, w db.IssueWakeup, cfg *builtinWakeup, taskID pgtype.UUID,
	receipts []db.IssueWakeupReceipt, now time.Time) (bool, error) {
	if caps := cfg.aggregateCaps(); len(caps) > 0 {
		admission, err := admitAggregateStart(ctx, q, aggregateStart{WorkspaceID: issue.WorkspaceID, WakeupID: w.ID, TaskID: taskID, RuleKey: w.DefaultRuleKey.String, FactAge: oldestReceiptAge(receipts)}, caps, now)
		if aggregateCounterBusy(err) {
			return false, nil // another start holds the counter; this one is retried on a later pass
		}
		if err != nil {
			return false, err
		}
		if !admission.Admitted {
			return false, q.MarkWakeupAggregateBlocked(ctx, db.MarkWakeupAggregateBlockedParams{
				ID: w.ID, ScopeKind: pgtype.Text{String: string(admission.Blocked.Scope), Valid: true}, ScopeID: admission.Blocked.ScopeID,
				RetryAt: pgtype.Timestamptz{Time: admission.RetryAt, Valid: true},
			})
		}
	}
	if w.AggregateRetryAt.Valid {
		return true, q.ClearWakeupAggregateBlocked(ctx, w.ID)
	}
	return true, nil
}

// checkCustomClaim revalidates a custom rule's queued run when a daemon claims
// it: the instance is the one the run was captured under, the rule is still on
// and resolves to what the instance holds, the issue still qualifies, and the
// target is the run's agent and still usable by the rule's author. A run of an
// older configuration is refused, never run under a newer target or instruction.
func (s *IssueWakeupService) checkCustomClaim(ctx context.Context, task db.AgentTaskQueue, w db.IssueWakeup) error {
	if w.AgentID != task.AgentID || w.CreatedBy != task.OriginatorUserID {
		return ErrWakeupForbidden
	}
	issue, err := s.Tasks.Queries.GetIssue(ctx, w.IssueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrWakeupForbidden
	}
	if err != nil {
		return err
	}
	cfg, ok, err := s.customCurrent(ctx, s.Tasks.Queries, issue, w)
	if err != nil || !ok {
		return errors.Join(ErrWakeupForbidden, err)
	}
	// The run that reached the fire cap is legitimate: it stays claimable though
	// the instance is paused. The pauses that block runs also set disabled_at.
	eligible, err := s.customRunEligible(ctx, s.Tasks.Queries, issue, w, cfg, task)
	if err != nil {
		return err
	}
	if !eligible {
		return ErrWakeupForbidden
	}
	return nil
}

// checkCustomJoinedAtStart judges one joined entry of a custom rule the way its
// own run would be, and holds a carrier back when the firings settled so far and
// the carriers ahead of it already spend the limit the firing was joined under.
func (s *IssueWakeupService) checkCustomJoinedAtStart(ctx context.Context, task db.AgentTaskQueue, entry joinedWakeup, w db.IssueWakeup) error {
	issue, err := s.Tasks.Queries.GetIssue(ctx, w.IssueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrWakeupForbidden
	}
	if err != nil {
		return err
	}
	cfg, ok, err := s.customCurrent(ctx, s.Tasks.Queries, issue, w)
	if err != nil || !ok {
		return errors.Join(ErrWakeupForbidden, err)
	}
	if eligible, err := s.customRunEligible(ctx, s.Tasks.Queries, issue, w, cfg, task); err != nil {
		return err
	} else if !eligible {
		return ErrWakeupForbidden
	}
	limit := entry.FireLimit
	if cfgLimit, limited := cfg.fireLimit(); limited && (limit == 0 || cfgLimit < limit) {
		limit = cfgLimit
	}
	if limit > 0 {
		return s.checkJoinedFireCap(ctx, task, entry, limit)
	}
	return nil
}

// customMayJoin reports whether a custom rule's pending facts may ride a run
// that is waiting to start: the rule is on and the instance current, the issue
// qualifies, the carrier is the run its own would be (same agent, and for a squad
// its leader task), and neither the fire limit nor a deferring rule's isolation
// forbids it.
func (s *IssueWakeupService) customMayJoin(ctx context.Context, q *db.Queries, issue db.Issue, task db.AgentTaskQueue, w db.IssueWakeup, carrier carrierOwner) (bool, error) {
	cfg, ok, err := s.customCurrent(ctx, q, issue, w)
	if err != nil || !ok || !w.Enabled {
		return false, err
	}
	if carrier.other && cfg.customDefers() || issuestatus.Effective(ctx, q, issue.WorkspaceID, issue.Status) == "backlog" {
		return false, nil
	}
	if eligible, err := s.customRunEligible(ctx, q, issue, w, cfg, task); err != nil || !eligible {
		return false, err
	}
	if limit, limited := cfg.fireLimit(); limited {
		// Inputs other runs already took are firings that count too.
		others, err := otherCarrierSlots(ctx, q, w, task.ID, false)
		if err != nil || w.FireCount+others >= limit {
			return false, err
		}
	}
	return true, nil
}
