package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Built-in execution (CHE-1082 L5): child_done, pr_merged and pr_checks_failed
// run the configuration their scoped definitions resolve to. With no stored
// definition for a rule on an issue nothing here resolves, and the legacy
// settings path runs exactly as before.
//
// An instance (the issue_wakeup row) keeps its identity, receipts, PR ledger,
// counters and pauses through every configuration change. What moves is its
// revision: the row records the fingerprint of the configuration it last
// captured, dispatched or claimed under, and a different resolution advances
// the revision, so inputs and unstarted runs of the old configuration stop
// matching while a running prompt is left alone.

// builtinExecutedFields are the patch fields execution applies; the held ones
// belong to later layers, and a rule that resolves any of them runs nothing.
// TestBuiltinExecutionClassifiesEveryPatchField keeps both lists in step with
// WakeupConfigPatch.
var (
	builtinExecutedFields = []string{"enabled", "name", "trigger", "target", "instruction", "mode", "max_fires", "expiry", "rate_limit", "aggregate_limit", "filters", "active_run"}
	builtinHeldFields     = []string{"schedule"}
)

// errWakeupConfigHeld marks a rule whose stored configuration this build cannot
// execute: unreadable, or using a control that has no execution yet. Dispatch
// leaves its inputs pending and runs nothing; the stored definitions are never
// rewritten.
var errWakeupConfigHeld = errors.New("wakeup configuration cannot be executed")

func heldConfig(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errWakeupConfigHeld, fmt.Sprintf(format, args...))
}

// builtinWakeup is a platform rule's resolved configuration for one issue.
type builtinWakeup struct {
	Eff EffectiveWakeupConfig
	// Authorizer is the member whose write supplied the target; they are asked
	// again whether they may invoke it.
	Authorizer pgtype.UUID
	target     wakeupTargetSpec
	filters    wakeupFiltersSpec
	expiry     wakeupExpirySpec
}

// loadBuiltinWakeup resolves a platform rule's definitions for the issue. It
// returns nil when none is stored (the legacy path applies), errWakeupConfigHeld
// when what is stored cannot be executed, and errMalformedPRWakeupSettings
// unchanged for malformed workspace settings. legacy is the issue's instance
// when it exists; its customization is the issue-scope input of a built-in.
func loadBuiltinWakeup(ctx context.Context, q *db.Queries, issue db.Issue, rule string, legacy *db.IssueWakeup) (*builtinWakeup, error) {
	rows, err := q.ListWakeupDefinitionsForRule(ctx, db.ListWakeupDefinitionsForRuleParams{
		WorkspaceID: issue.WorkspaceID, RuleKey: rule, ProjectID: issue.ProjectID, IssueID: issue.ID,
	})
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	in := WakeupResolveInput{RuleKey: rule, IssueProjectID: issue.ProjectID, IssueTarget: issue.AssigneeType.String + ":" + wakeupUUIDString(issue.AssigneeID)}
	var stored *WakeupDefinition
	for _, row := range rows {
		def, err := WakeupDefinitionFromRow(row)
		if err != nil {
			return nil, heldConfig("%v", err)
		}
		switch def.Scope {
		case WakeupScopeWorkspace:
			stored = &def
		case WakeupScopeProject:
			in.Project = &def
		case WakeupScopeIssue:
			in.Issue = &def
		}
	}
	ws, err := q.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if in.Workspace, err = WorkspaceWakeupDefinition(issue.WorkspaceID, stored, ws.Settings, rule); err != nil {
		return nil, err
	}
	if in.Issue == nil && legacy != nil {
		if in.Issue, err = LegacyIssueWakeupDefinition(executionLegacyRow(*legacy)); err != nil {
			return nil, heldConfig("%v", err)
		}
	}
	eff, err := ResolveWakeupConfig(in)
	if err != nil {
		return nil, heldConfig("%v", err)
	}
	b := &builtinWakeup{Eff: eff}
	c := eff.Config
	if c.Schedule.Set {
		return nil, heldConfig("schedule is not executed yet")
	}
	if c.ActiveRun.Set && !validWakeupActiveRun(c.ActiveRun.Value) {
		return nil, heldConfig("unreadable active_run")
	}
	for _, cap := range eff.AggregateCaps {
		if cap.Limit < 1 {
			return nil, heldConfig("unreadable aggregate limit")
		}
	}
	if c.Trigger.Set {
		var spec wakeupTriggerSpec
		if err := decodeWakeupSpec(c.Trigger.Value, &spec); err != nil || spec.Kind != rule {
			return nil, heldConfig("a built-in rule keeps its own trigger")
		}
	}
	if err := b.decodeSpecs(); err != nil {
		return nil, err
	}
	if src, ok := eff.Sources["target"]; ok {
		for _, def := range []*WakeupDefinition{in.Workspace, in.Project, in.Issue} {
			if def != nil && def.Scope == src {
				b.Authorizer = def.UpdatedBy
			}
		}
	}
	return b, nil
}

// decodeSpecs reads the target, filters and expiry the configuration sets; one
// this build cannot read holds the rule.
func (b *builtinWakeup) decodeSpecs() error {
	c := b.Eff.Config
	for _, spec := range []struct {
		field wakeupObject
		into  any
	}{{c.Target, &b.target}, {c.Filters, &b.filters}, {c.Expiry, &b.expiry}} {
		if spec.field.Set {
			if err := decodeWakeupSpec(spec.field.Value, spec.into); err != nil {
				return heldConfig("%v", err)
			}
		}
	}
	if c.Target.Set && !validBuiltinTarget(b.target) {
		return heldConfig("unreadable target")
	}
	return nil
}

func validBuiltinTarget(t wakeupTargetSpec) bool {
	id, err := wakeupUUID(t.ID)
	switch t.Type {
	case "assignee":
		return err == nil && !id.Valid
	case "agent", "squad":
		return err == nil && id.Valid
	}
	return false
}

// fingerprint is "" when no scoped definition applies.
func (b *builtinWakeup) fingerprint() string {
	if b == nil {
		return ""
	}
	return b.Eff.Fingerprint
}

func (b *builtinWakeup) enabled() bool { return b.Eff.Enabled() }

// rateLimit is the instance's own starts per hour: the hard 12, or a lower
// configured value.
func (b *builtinWakeup) rateLimit() int {
	if b != nil && b.Eff.Config.RateLimit.Set && b.Eff.Config.RateLimit.Value < wakeupHourlyRunLimit {
		return b.Eff.Config.RateLimit.Value
	}
	return wakeupHourlyRunLimit
}

// fireLimit is how many runs the instance may start in all: one for once mode,
// else max_fires. Nothing is set unless a definition says so. A joined firing
// counts when its carrier starts; until then dispatch treats it as holding one
// of the slots, so the limit is never passed.
func (b *builtinWakeup) fireLimit() (int32, bool) {
	if b == nil {
		return 0, false
	}
	c := b.Eff.Config
	limit, ok := int32(0), false
	if c.Mode.Set && c.Mode.Value == "once" {
		limit, ok = 1, true
	}
	if c.MaxFires.Set {
		if m := int32(c.MaxFires.Value); !ok || m < limit {
			limit, ok = m, true
		}
	}
	return limit, ok
}

// expired reports whether the instance's end has passed. A relative end runs
// from the instance's first activation (its creation), so it never restarts
// because another issue fired or an ancestor was edited.
func (b *builtinWakeup) expired(created, now time.Time) bool {
	if b == nil {
		return false
	}
	switch {
	case b.expiry.At != nil:
		return !now.Before(*b.expiry.At)
	case b.expiry.AfterSeconds != nil:
		return !now.Before(created.Add(time.Duration(*b.expiry.AfterSeconds) * time.Second))
	}
	return false
}

// matchesPR applies the filters a PR event's own facts decide. A fact the event
// does not carry never matches.
func (b *builtinWakeup) matchesPR(in PullRequestWakeupInput) bool {
	if b == nil {
		return true
	}
	f := b.filters
	if f.BaseBranch != nil && *f.BaseBranch != in.BaseBranch || f.HeadBranch != nil && *f.HeadBranch != in.HeadBranch {
		return false
	}
	if f.CI != nil && in.Rule == SystemRulePRChecksFailed {
		return *f.CI == "both" || *f.CI == "failure" && in.Conclusion == "FAILURE" || *f.CI == "error" && in.Conclusion == "ERROR"
	}
	return true
}

// matchesIssue applies the label (all selected) and priority (any selected)
// filters to the issue as it is now.
func (b *builtinWakeup) matchesIssue(ctx context.Context, q *db.Queries, issue db.Issue) (bool, error) {
	if b == nil {
		return true, nil
	}
	f := b.filters
	if len(f.Priorities) > 0 && !slices.Contains(f.Priorities, issue.Priority) {
		return false, nil
	}
	if len(f.Labels) == 0 {
		return true, nil
	}
	have, err := q.ListIssueLabelIDs(ctx, issue.ID)
	if err != nil {
		return false, err
	}
	held := map[string]bool{}
	for _, id := range have {
		held[util.UUIDToString(id)] = true
	}
	for _, want := range f.Labels {
		id, err := wakeupUUID(want)
		if err != nil || !held[util.UUIDToString(id)] {
			return false, nil
		}
	}
	return true, nil
}

// systemInstruction is the text a run is given. A configured instruction
// replaces the rule's own; the platform still appends the PR/head/merge/status
// facts as trigger facts. A cleared one falls back to the rule's default, not to
// an ancestor's value the clear removed.
func (b *builtinWakeup) systemInstruction(w db.IssueWakeup, settings []byte, receipts []db.IssueWakeupReceipt) string {
	if b == nil {
		return systemWakeupInstruction(w, settings, receipts)
	}
	if b.Eff.Config.Instruction.Set {
		return b.Eff.Config.Instruction.Value
	}
	if w.SystemRule.String == SystemRuleChildDone {
		return ChildDoneDefaultInstruction
	}
	return systemWakeupInstruction(w, nil, receipts)
}

// paused reports a platform or person pause on the instance, which no
// configuration edit lifts.
func systemWakeupPaused(w db.IssueWakeup) bool { return w.PausedReason.Valid || w.DisabledAt.Valid }

// builtinTarget resolves who the rule reaches: the issue's assignee unless the
// configuration names an agent or a squad. A named target is checked against
// the member who wrote it every time it is used, so a revoked, archived or
// runtime-less target refuses visibly (wakeTarget.Refused) instead of running.
func builtinTarget(ctx context.Context, svc *IssueWakeupService, q *db.Queries, issue db.Issue, b *builtinWakeup) (wakeTarget, error) {
	if b == nil || !b.Eff.Config.Target.Set || b.target.Type == "assignee" {
		return resolveWakeTarget(ctx, q, issue)
	}
	id, _ := wakeupUUID(b.target.ID)
	target := wakeTarget{Type: b.target.Type, ID: id}
	agentID := id
	if b.target.Type == "squad" {
		squad, err := q.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: id, WorkspaceID: issue.WorkspaceID})
		if errors.Is(err, pgx.ErrNoRows) || err == nil && squad.ArchivedAt.Valid {
			target.Refused = wakeupOutcomeTargetUnavailable
			return target, nil
		}
		if err != nil {
			return target, err
		}
		target.SquadID, agentID = squad.ID, squad.LeaderID
	}
	agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agentID, WorkspaceID: issue.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (agent.ArchivedAt.Valid || !agent.RuntimeID.Valid) {
		target.Refused = wakeupOutcomeTargetUnavailable
		return target, nil
	}
	if err != nil {
		return target, err
	}
	switch err := svc.authorize(ctx, q, issue.WorkspaceID, b.Authorizer, agent); {
	case errors.Is(err, ErrWakeupForbidden):
		target.Refused = wakeupOutcomeTargetUnauthorized
	case err != nil:
		return target, err
	default:
		target.Agent = agent
	}
	return target, nil
}

const (
	wakeupOutcomeTargetUnavailable  = "target_unavailable"
	wakeupOutcomeTargetUnauthorized = "target_unauthorized"
	wakeupOutcomeFiltered           = "filtered_out"
)

// rebaseBuiltinConfig moves the instance to the configuration it now resolves
// to when that differs from the one it last ran under. Pending inputs and runs
// that have not started belong to the old configuration: the revision change
// retires the inputs, and the queued runs are withdrawn (returned so the caller
// can announce them after commit). A child_done condition is re-baselined, so a
// stage that was already complete is not replayed by the change.
func rebaseBuiltinConfig(ctx context.Context, tx pgx.Tx, q *db.Queries, issue db.Issue, w db.IssueWakeup, b *builtinWakeup) (db.IssueWakeup, []db.AgentTaskQueue, error) {
	fingerprint := b.fingerprint()
	if w.ConfigFingerprint.String == fingerprint {
		return w, nil, nil
	}
	// Firings that already started on a carrier run are consumed work: settle
	// them against the old revision before it is retired, so an ancestor edit
	// cannot erase their count (and with it a once/max_fires ending).
	pending, err := q.ListPendingWakeupReceipts(ctx, db.ListPendingWakeupReceiptsParams{WakeupID: w.ID, Revision: w.Revision})
	if err != nil {
		return w, nil, err
	}
	_, _, taken, err := takenReceipts(ctx, q, pending)
	if err != nil {
		return w, nil, err
	}
	for _, run := range taken {
		if err := q.ConsumeWakeupReceipts(ctx, db.ConsumeWakeupReceiptsParams{Ids: receiptIDs(run.receipts), TaskID: run.task.ID}); err != nil {
			return w, nil, err
		}
		if err := q.AdvanceIssueWakeup(ctx, db.AdvanceIssueWakeupParams{ID: w.ID, Enabled: true, LastTaskID: run.task.ID}); err != nil {
			return w, nil, err
		}
		if err := q.CountWakeupFires(ctx, w.ID); err != nil {
			return w, nil, err
		}
		facts := systemWakeupFacts(w, run.receipts)
		facts["outcome"], facts["task_id"] = wakeupOutcomeMerged, util.UUIDToString(run.task.ID)
		// Published with the next timeline refresh; no live event from here.
		if _, err := recordWakeupActivity(ctx, q, w, wakeupActivityTriggered, "system", pgtype.UUID{}, facts); err != nil {
			return w, nil, err
		}
	}
	rebased, err := q.RebaseSystemWakeupConfig(ctx, db.RebaseSystemWakeupConfigParams{ID: w.ID, Fingerprint: fingerprint})
	if err != nil {
		return w, nil, err
	}
	// A started joined firing ends the instance under the limit it was joined
	// under; the new configuration's limit may only tighten that.
	exhausted := false
	for _, run := range taken {
		exhausted = exhausted || joinedFireLimit(run.task, w.ID) > 0 && rebased.FireCount >= joinedFireLimit(run.task, w.ID)
	}
	if limit, limited := b.fireLimit(); limited && rebased.FireCount >= limit {
		exhausted = exhausted || len(taken) > 0
	}
	if exhausted && !systemWakeupPaused(rebased) {
		if err := q.PauseIssueWakeup(ctx, db.PauseIssueWakeupParams{ID: w.ID, PausedReason: pgtype.Text{String: wakeupPausedMaxFires, Valid: true}}); err != nil {
			return w, nil, err
		}
		rebased.Enabled, rebased.PausedReason = false, pgtype.Text{String: wakeupPausedMaxFires, Valid: true}
	}
	cancelled, err := q.CancelUnstartedWakeupTasks(ctx, util.UUIDToString(w.ID))
	if err != nil {
		return w, nil, err
	}
	if rebased.SystemRule.String == SystemRuleChildDone {
		state, err := childDoneBaseline(ctx, tx, q, issue)
		if err != nil {
			return w, nil, err
		}
		if err := q.SetWakeupConditionState(ctx, db.SetWakeupConditionStateParams{ID: w.ID, ConditionState: state}); err != nil {
			return w, nil, err
		}
		rebased.ConditionState = state
	}
	return rebased, cancelled, nil
}

// builtinConfigCurrent reports whether the instance still runs the
// configuration its issue resolves to: the check made at join, claim and start.
func builtinConfigCurrent(w db.IssueWakeup, b *builtinWakeup) bool {
	return w.ConfigFingerprint.String == b.fingerprint()
}

func logWakeupHeld(ctx context.Context, issue db.Issue, w db.IssueWakeup, err error) {
	slog.WarnContext(ctx, "wakeup dispatch held: its scoped configuration cannot be executed",
		"issue_id", util.UUIDToString(issue.ID), "wakeup_id", util.UUIDToString(w.ID), "rule", w.SystemRule.String, "error", err)
}

// isDefaultDerivedWakeup reports whether a runtime row was materialized from a
// scoped (custom) rule. It has no system rule, so only its origin metadata tells
// it from a genuine local wakeup; the scoped configuration decides what it runs.
func isDefaultDerivedWakeup(w db.IssueWakeup) bool {
	return !w.SystemRule.Valid && (w.DefaultRuleKey.Valid || w.DefaultScopeKind.Valid || w.DefaultScopeID.Valid)
}

// prRuleEnabled is whether GitHub-driven rule may run on a workspace's
// settings. A rule with scoped configuration takes its enabled value from the
// resolved chain, of which the legacy setting is one input, so only the master
// switch is read here; malformed settings fail closed either way.
func prRuleEnabled(settings []byte, rule string, configured bool) (bool, error) {
	if !configured {
		return PRWakeupEnabled(settings, rule)
	}
	if len(settings) == 0 {
		return true, nil
	}
	var values prWakeupSettings
	if err := json.Unmarshal(settings, &values); err != nil {
		return false, malformedPRWakeupSettings(err)
	}
	return values.GitHubEnabled == nil || *values.GitHubEnabled, nil
}

// executionLegacyRow is the issue's customized legacy row as configuration sees
// it: a runtime pause (max_fires, loop, rate) is not a configuration change, so a
// paused row projects the enabled value it ran with. The pause itself stays a
// veto, applied from the row (systemWakeupPaused).
func executionLegacyRow(w db.IssueWakeup) db.IssueWakeup {
	if w.PausedReason.Valid {
		w.Enabled, w.PausedReason, w.DisabledAt = true, pgtype.Text{}, pgtype.Timestamptz{}
	}
	return w
}

// carrierMatchesTarget reports whether a waiting run can carry the rule's
// facts. A configured squad target is briefed through a leader task of that
// squad; a bare run of the leader, or a leader task of another squad, would
// keep the wrong role and never receive the briefing. Other targets and the
// legacy path accept any run of the target agent.
func (b *builtinWakeup) carrierMatchesTarget(target wakeTarget, task db.AgentTaskQueue) bool {
	if b == nil || !b.Eff.Config.Target.Set || !target.SquadID.Valid {
		return true
	}
	return task.IsLeaderTask && task.SquadID == target.SquadID
}

// hasWaitingRunFor is hasWaitingRun for the run that may carry this rule's
// facts: for a configured squad target only a leader task of that squad.
func hasWaitingRunFor(ctx context.Context, q *db.Queries, issueID, agentID, runAs pgtype.UUID, b *builtinWakeup, target wakeTarget) (bool, error) {
	if b == nil || !b.Eff.Config.Target.Set || !target.SquadID.Valid {
		return hasWaitingRun(ctx, q, issueID, agentID, runAs)
	}
	if !runAs.Valid {
		return false, nil
	}
	_, err := q.FindWaitingIssueLeaderRun(ctx, db.FindWaitingIssueLeaderRunParams{IssueID: issueID, AgentID: agentID, OriginatorUserID: runAs, SquadID: target.SquadID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// runEligible applies the predicates that can change after a run was queued:
// the expiry window and the issue's labels and priority.
func (b *builtinWakeup) runEligible(ctx context.Context, q *db.Queries, issue db.Issue, w db.IssueWakeup) (bool, error) {
	if b.expired(w.CreatedAt.Time, time.Now()) {
		return false, nil
	}
	return b.matchesIssue(ctx, q, issue)
}

// joinedFireLimit is the limit the carrier run recorded when the wakeup joined it.
func joinedFireLimit(carrier db.AgentTaskQueue, wakeupID pgtype.UUID) int32 {
	var stored struct {
		Joined []joinedWakeup `json:"wakeup_joined"`
	}
	if json.Unmarshal(carrier.Context, &stored) != nil {
		return 0
	}
	for _, entry := range stored.Joined {
		if id, err := util.ParseUUID(entry.WakeupID); err == nil && id == wakeupID {
			return entry.FireLimit
		}
	}
	return 0
}

// splitDeferredPRFilters applies the PR filters to receipts captured while the
// configuration was held, which carry their branch and CI facts (and the
// filters_deferred mark) for exactly this. Receipts without the mark were
// filtered at capture and pass.
func splitDeferredPRFilters(b *builtinWakeup, rule string, receipts []db.IssueWakeupReceipt) (kept, refused []db.IssueWakeupReceipt) {
	for _, r := range receipts {
		var facts struct {
			Deferred   bool   `json:"filters_deferred"`
			BaseBranch string `json:"base_branch"`
			HeadBranch string `json:"head_branch"`
			Conclusion string `json:"conclusion"`
		}
		if json.Unmarshal(r.Payload, &facts) == nil && facts.Deferred &&
			!b.matchesPR(PullRequestWakeupInput{Rule: rule, BaseBranch: facts.BaseBranch, HeadBranch: facts.HeadBranch, Conclusion: facts.Conclusion}) {
			refused = append(refused, r)
			continue
		}
		kept = append(kept, r)
	}
	return kept, refused
}
