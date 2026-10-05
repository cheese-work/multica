package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// The child_done system rule wakes a parent's assignee when a stage of its
// sub-issues closes while a later stage waits, and once more when every
// sub-issue is closed. It is an ordinary issue_wakeup row with system_rule set:
// the shared condition, receipts, runaway protection, activity and run model
// apply. What differs is owned by the platform: it has no fixed agent or
// creator (the target is the parent's assignee when it fires, a squad's leader
// for a squad, an inbox notification for a member), it cannot be deleted or
// edited by agents, and it is evaluated when sub-issues change instead of on a
// polling schedule.
const (
	SystemRuleChildDone       = "child_done"
	SystemRulePRMerged        = "pr_merged"
	SystemRulePRChecksFailed  = "pr_checks_failed"
	WorkspaceSettingPRMerged  = "github_wake_on_pr_merge"
	WorkspaceSettingPRFailure = "github_wake_on_ci_failure"
	// MaxSystemWakeupInstruction bounds per-issue and workspace instructions.
	MaxSystemWakeupInstruction = 4000
	// WorkspaceSettingChildDone and WorkspaceSettingChildDoneInstruction hold
	// the rule's workspace default. Only an explicit false turns it off.
	WorkspaceSettingChildDone            = "system_wakeup_child_done"
	WorkspaceSettingChildDoneInstruction = "system_wakeup_child_done_instruction"
	// childChangeHint prompts an evaluation after sub-issues changed. Like
	// other hints it never becomes a run input.
	childChangeHint = "children.changed"
)

var childDoneCondition = json.RawMessage(`{"type":"children_done","each_stage":true}`)

type PullRequestWakeupInput struct {
	Rule        string
	RepoOwner   string
	RepoName    string
	Number      int32
	URL         string
	MergeCommit string
	HeadSHA     string
	Conclusion  string
	// BaseBranch and HeadBranch are the PR's branches when the event carries
	// them; a branch filter never matches a missing one.
	BaseBranch string
	HeadBranch string
}

var errMalformedPRWakeupSettings = errors.New("malformed PR wakeup workspace settings")

func malformedPRWakeupSettings(err error) error {
	return fmt.Errorf("%w: %w", errMalformedPRWakeupSettings, err)
}

func prWakeupSetting(rule string) (string, bool) {
	switch rule {
	case SystemRulePRMerged:
		return WorkspaceSettingPRMerged, true
	case SystemRulePRChecksFailed:
		return WorkspaceSettingPRFailure, true
	default:
		return "", false
	}
}

func PRWakeupEnabled(settings []byte, rule string) (bool, error) {
	setting, ok := prWakeupSetting(rule)
	if !ok {
		return false, fmt.Errorf("unknown pull request system wakeup rule %q", rule)
	}
	if len(settings) == 0 {
		return true, nil
	}
	var values prWakeupSettings
	if err := json.Unmarshal(settings, &values); err != nil {
		return false, malformedPRWakeupSettings(err)
	}
	if values.GitHubEnabled != nil && !*values.GitHubEnabled {
		return false, nil
	}
	enabled := values.WakeOnPRMerge
	if setting == WorkspaceSettingPRFailure {
		enabled = values.WakeOnCIFailure
	}
	return enabled == nil || *enabled, nil
}

func (s *IssueWakeupService) TriggerPullRequestWakeup(ctx context.Context, issueID pgtype.UUID, in PullRequestWakeupInput) error {
	_, ok := prWakeupSetting(in.Rule)
	if !ok || in.Number < 1 || in.RepoOwner == "" || in.RepoName == "" {
		return fmt.Errorf("invalid pull request wakeup input")
	}
	eventType := "pr.merged"
	eventKey := fmt.Sprintf("github:%s/%s#%d", strings.ToLower(in.RepoOwner), strings.ToLower(in.RepoName), in.Number)
	switch in.Rule {
	case SystemRulePRMerged:
		if in.MergeCommit == "" {
			in.MergeCommit = in.HeadSHA
		}
	case SystemRulePRChecksFailed:
		if in.Conclusion != "FAILURE" && in.Conclusion != "ERROR" || in.HeadSHA == "" {
			return fmt.Errorf("invalid failing pull request checks input")
		}
		eventType = "pr.checks_failed"
		eventKey += ":" + in.HeadSHA
	}

	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := s.Tasks.Queries.WithTx(tx)
	issue, err := q.LockWakeupIssue(ctx, issueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	active, err := systemWakeupIssueActive(ctx, q, issue, true)
	if err != nil {
		return err
	}
	if !active {
		return tx.Commit(ctx)
	}
	ws, err := q.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return err
	}
	w, err := q.GetSystemWakeup(ctx, db.GetSystemWakeupParams{IssueID: issue.ID, SystemRule: systemRuleText(in.Rule)})
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var legacy *db.IssueWakeup
	if exists {
		legacy = &w
	}
	config, err := loadBuiltinWakeup(ctx, q, issue, in.Rule, legacy)
	held := false
	switch {
	case errors.Is(err, errWakeupConfigHeld):
		// Captured under the instance's recorded configuration; dispatch holds
		// the input. A hold is not the absence of definitions, so nothing is
		// rebased.
		config, held, err = nil, true, nil
	case errors.Is(err, errMalformedPRWakeupSettings):
		// The check below reports the malformed settings the way it always has.
		config, err = nil, nil
	}
	if err != nil {
		return err
	}
	enabled, err := prRuleEnabled(ws.Settings, in.Rule, config != nil)
	if err != nil {
		if errors.Is(err, errMalformedPRWakeupSettings) {
			slog.WarnContext(ctx, "PR wakeup skipped because workspace settings are malformed", "issue_id", util.UUIDToString(issue.ID), "rule", in.Rule, "error", err)
			return nil
		}
		return err
	}
	if config != nil {
		enabled = enabled && config.enabled()
	}
	if !enabled {
		return tx.Commit(ctx)
	}
	created := time.Now()
	if exists {
		created = w.CreatedAt.Time
	}
	if config.expired(created, time.Now()) {
		return tx.Commit(ctx)
	}
	if !exists {
		w, err = q.CreateSystemWakeup(ctx, db.CreateSystemWakeupParams{
			ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID,
			EventTypes: []string{}, Enabled: true, SystemRule: systemRuleText(in.Rule),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			w, err = q.GetSystemWakeup(ctx, db.GetSystemWakeupParams{IssueID: issue.ID, SystemRule: systemRuleText(in.Rule)})
		}
		if err != nil {
			return err
		}
	}
	// What this input is captured under: a configuration that changed since the
	// instance last ran retires its older inputs and unstarted runs first.
	var withdrawn []db.AgentTaskQueue
	if !held {
		if w, withdrawn, err = rebaseBuiltinConfig(ctx, tx, q, issue, w, config); err != nil {
			return err
		}
	}
	tag, err := tx.Exec(ctx, `INSERT INTO issue_wakeup_pr_event(wakeup_id,event_key) VALUES($1,$2) ON CONFLICT DO NOTHING`, w.ID, eventKey)
	if err != nil {
		return err
	}
	announceWithdrawn := func() {
		for _, task := range withdrawn {
			s.Tasks.broadcastTaskEvent(ctx, protocol.EventTaskCancelled, task)
		}
	}
	if tag.RowsAffected() == 0 {
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		announceWithdrawn()
		return nil
	}
	if !config.matchesPR(in) {
		// The event's identity is spent: a redelivery never fires once the
		// filter changes. The refusal is visible on the timeline.
		a, err := recordWakeupActivity(ctx, q, w, wakeupActivityTriggered, "system", pgtype.UUID{}, map[string]any{
			"rule": in.Rule, "repo_owner": in.RepoOwner, "repo_name": in.RepoName, "pr_number": in.Number, "outcome": wakeupOutcomeFiltered,
		})
		if err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		announceWithdrawn()
		s.publishWakeupActivities(a)
		return nil
	}
	payload := map[string]any{
		"repo_owner": in.RepoOwner, "repo_name": in.RepoName, "pr_number": in.Number,
		"pr_url": in.URL, "issue_status": issue.Status,
	}
	if in.Rule == SystemRulePRMerged {
		payload["merge_commit_sha"] = in.MergeCommit
	} else {
		payload["head_sha"] = in.HeadSHA
		payload["conclusion"] = in.Conclusion
	}
	if held {
		// The filters cannot be read now, so dispatch applies them to these
		// facts once the hold lifts (splitDeferredPRFilters).
		payload["filters_deferred"], payload["base_branch"], payload["head_branch"] = true, in.BaseBranch, in.HeadBranch
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err = q.RecordWakeupReceipt(ctx, db.RecordWakeupReceiptParams{
		ID: dbid.NewV7(), WakeupID: w.ID, Revision: w.Revision, EventKey: eventKey, EventType: eventType, Payload: encoded,
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	announceWithdrawn()
	return s.dispatchSystem(ctx, w)
}

// ChildDoneDefaultInstruction is what the woken assignee is asked to do when
// neither the issue nor the workspace sets an instruction. The trigger facts
// say which stage closed, which one is next and how many were cancelled.
const ChildDoneDefaultInstruction = "Sub-issues of this issue have closed; the trigger facts list each stage and which one is next. " +
	"If a later stage is waiting, check that its dependencies are met, then move its sub-issues out of backlog so they start. " +
	"If a sub-issue in the closed stages was cancelled rather than finished, decide whether its work is still needed before advancing. " +
	"When every sub-issue is closed, bring their results together on this issue and move it forward, or mark it ready for review when nothing remains."

// SystemWakeupDefault is the rule's workspace default: whether it is on and
// the instruction set for the workspace ("" when none).
func SystemWakeupDefault(settings []byte) (bool, string) {
	var values map[string]json.RawMessage
	if json.Unmarshal(settings, &values) != nil {
		return true, ""
	}
	var instruction string
	_ = json.Unmarshal(values[WorkspaceSettingChildDoneInstruction], &instruction)
	return string(values[WorkspaceSettingChildDone]) != "false", strings.TrimSpace(instruction)
}

// ChildDoneInstruction is the instruction the rule's runs receive.
func ChildDoneInstruction(issueInstruction string, settings []byte) string {
	if s := strings.TrimSpace(issueInstruction); s != "" {
		return s
	}
	if _, s := SystemWakeupDefault(settings); s != "" {
		return s
	}
	return ChildDoneDefaultInstruction
}

func systemRuleText(rule string) pgtype.Text { return pgtype.Text{String: rule, Valid: true} }

// baselineChildDone records what already holds for a rule that was just
// turned on, so facts that became true while it was off do not wake anyone.
func baselineChildDone(ctx context.Context, tx pgx.Tx, q *db.Queries, rule db.IssueWakeup) error {
	children, err := loadSubIssues(ctx, tx, q, rule.IssueID, rule.WorkspaceID)
	if err != nil {
		return err
	}
	met, fingerprint, _ := stageProgress(children)
	if !met {
		fingerprint = ""
	}
	return q.SetWakeupConditionState(ctx, db.SetWakeupConditionStateParams{ID: rule.ID, ConditionState: fingerprint})
}

// childDoneBaseline is the condition state that records what already holds on a
// parent, treating sub-issues whose closing is recorded but not yet processed as
// still open, so that change still wakes the assignee.
func childDoneBaseline(ctx context.Context, tx pgx.Tx, q *db.Queries, parent db.Issue) (string, error) {
	children, err := loadSubIssues(ctx, tx, q, parent.ID, parent.WorkspaceID)
	if err != nil {
		return "", err
	}
	closing, err := q.ListUnprocessedClosedChildren(ctx, parent.ID)
	if err != nil {
		return "", err
	}
	asOpen := make(map[pgtype.UUID]bool, len(closing))
	for _, id := range closing {
		asOpen[id] = true
	}
	for i := range children {
		if asOpen[children[i].ID] {
			children[i].Closed, children[i].Cancelled = false, false
		}
	}
	met, fingerprint, _ := stageProgress(children)
	if !met {
		fingerprint = ""
	}
	return fingerprint, nil
}

// EnsureChildDoneRule returns the parent's rule, creating it when missing.
// The new rule's baseline treats sub-issues whose closing is recorded but not
// yet processed as still open, so that change still wakes the assignee,
// whichever writer creates the rule first.
func EnsureChildDoneRule(ctx context.Context, tx pgx.Tx, q *db.Queries, parent db.Issue) (db.IssueWakeup, error) {
	rule, err := q.GetSystemWakeup(ctx, db.GetSystemWakeupParams{IssueID: parent.ID, SystemRule: systemRuleText(SystemRuleChildDone)})
	if !errors.Is(err, pgx.ErrNoRows) {
		return rule, err
	}
	ws, err := q.GetWorkspace(ctx, parent.WorkspaceID)
	if err != nil {
		return rule, err
	}
	enabled, _ := SystemWakeupDefault(ws.Settings)
	fingerprint, err := childDoneBaseline(ctx, tx, q, parent)
	if err != nil {
		return rule, err
	}
	rule, err = q.CreateSystemWakeup(ctx, db.CreateSystemWakeupParams{
		ID: dbid.NewV7(), WorkspaceID: parent.WorkspaceID, IssueID: parent.ID,
		EventTypes: []string{"issue.status_changed"}, Condition: childDoneCondition, ConditionState: fingerprint,
		Enabled: enabled, SystemRule: systemRuleText(SystemRuleChildDone),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return q.GetSystemWakeup(ctx, db.GetSystemWakeupParams{IssueID: parent.ID, SystemRule: systemRuleText(SystemRuleChildDone)})
	}
	return rule, err
}

// ProcessChildEvents handles the sub-issue changes recorded on these parents:
// it creates each parent's child_done rule when missing, hands every
// sub-issue rule a hint, and evaluates them right away. The hints are the
// durable handoff; a rule the immediate pass could not dispatch is picked up
// by the scheduler.
func (s *IssueWakeupService) ProcessChildEvents(ctx context.Context, parentIDs ...pgtype.UUID) error {
	var errs []error
	seen := map[pgtype.UUID]bool{}
	for _, parentID := range parentIDs {
		if !parentID.Valid || seen[parentID] {
			continue
		}
		seen[parentID] = true
		if err := s.processChildEvents(ctx, parentID); err != nil {
			slog.Warn("sub-issue wakeups: processing failed; will retry", "error", err, "parent_id", util.UUIDToString(parentID))
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *IssueWakeupService) processChildEvents(ctx context.Context, parentID pgtype.UUID) error {
	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := s.Tasks.Queries.WithTx(tx)
	events, err := q.ClaimChildEvents(ctx, parentID)
	if err != nil || len(events) == 0 {
		return err
	}
	ids := make([]pgtype.UUID, 0, len(events))
	kinds := map[string]bool{}
	// The agent runs that made the changes that can satisfy a sub-issue
	// condition (a sub-issue closing, leaving, or changing stage), when every
	// such change came from one. Adding or reopening a sub-issue satisfies
	// nothing, so it does not count.
	var sources []string
	allSourced := true
	for _, e := range events {
		ids = append(ids, e.ID)
		kinds[e.Kind] = true
		if e.Kind == "attached" || e.Kind == "reopened" {
			continue
		}
		if e.SourceTaskID.Valid {
			sources = append(sources, util.UUIDToString(e.SourceTaskID))
		} else {
			allSourced = false
		}
	}
	finish := func() error {
		if err := q.FinishChildEvents(ctx, ids); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	// The same lock order as dispatch: the issue, then the instance. The rule is
	// read under both, so the configuration it is rebased from is the one it
	// has now, not one a concurrent dispatch already moved it past.
	parent, err := q.LockWakeupIssue(ctx, parentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return finish()
	}
	if err != nil {
		return err
	}
	active, err := wakeupIssueActive(ctx, q, parent)
	if err != nil {
		return err
	}
	if !active {
		return finish()
	}
	ensured, err := EnsureChildDoneRule(ctx, tx, q, parent)
	if err != nil {
		return err
	}
	if ensured, err = q.LockIssueWakeup(ctx, ensured.ID); err != nil {
		return err
	}
	rules, err := q.ListChildConditionWakeups(ctx, parent.ID)
	if err != nil {
		return err
	}
	rules, withdrawn, err := configureChildDoneRule(ctx, tx, q, parent, ensured, rules)
	if err != nil {
		return err
	}
	changes := make([]string, 0, len(kinds))
	for kind := range kinds {
		changes = append(changes, kind)
	}
	hint := map[string]any{"changes": changes}
	if allSourced && len(sources) > 0 {
		hint["source_task_ids"] = sources
	}
	payload, _ := json.Marshal(hint)
	key := "children:" + util.UUIDToString(events[len(events)-1].ID)
	for _, w := range rules {
		if _, err = q.RecordWakeupReceipt(ctx, db.RecordWakeupReceiptParams{ID: dbid.NewV7(), WakeupID: w.ID, Revision: w.Revision, EventKey: key, EventType: childChangeHint, Payload: payload}); err != nil {
			return err
		}
	}
	if err := finish(); err != nil {
		return err
	}
	for _, task := range withdrawn {
		s.Tasks.broadcastTaskEvent(ctx, protocol.EventTaskCancelled, task)
	}
	// People's and agents' rules first, so the system rule sees their run and
	// waits to join it instead of starting a second one for the same fact.
	var errs []error
	for _, w := range rules {
		if w.SystemRule.Valid {
			err = s.dispatchSystem(ctx, w)
		} else {
			err = s.dispatch(ctx, w)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("wakeup %s: %w", util.UUIDToString(w.ID), err))
		}
	}
	if len(errs) > 0 {
		slog.Info("sub-issue wakeups: immediate dispatch deferred to the scheduler", "parent_id", util.UUIDToString(parent.ID), "error", errors.Join(errs...))
	}
	return nil
}

// configureChildDoneRule settles the parent's child_done instance against the
// configuration it resolves to before hints are recorded on it: a changed
// configuration is rebased first, so the hint belongs to the new revision, and
// the rule takes part when its resolved value is on, even if the instance's own
// flag follows a workspace default that is off. Without a stored definition the
// instance is listed exactly when its own flag is on, as before.
func configureChildDoneRule(ctx context.Context, tx pgx.Tx, q *db.Queries, parent db.Issue, ensured db.IssueWakeup, rules []db.IssueWakeup) ([]db.IssueWakeup, []db.AgentTaskQueue, error) {
	config, err := loadBuiltinWakeup(ctx, q, parent, SystemRuleChildDone, &ensured)
	if errors.Is(err, errWakeupConfigHeld) {
		// Dispatch holds the rule; its hints are recorded as before.
		return rules, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	system, withdrawn, err := rebaseBuiltinConfig(ctx, tx, q, parent, ensured, config)
	if err != nil {
		return nil, nil, err
	}
	listed := false
	out := make([]db.IssueWakeup, 0, len(rules))
	for _, w := range rules {
		if w.ID == ensured.ID {
			listed = true
			continue
		}
		out = append(out, w)
	}
	if config != nil {
		listed = config.enabled() && !systemWakeupPaused(system)
	}
	if listed {
		out = append(out, system)
	}
	return out, withdrawn, nil
}

// SweepChildEvents processes changes that were recorded but not handled
// right after their write, and drops handled rows after a week.
func (s *IssueWakeupService) SweepChildEvents(ctx context.Context) error {
	parents, err := s.Tasks.Queries.ListStaleChildEventParents(ctx)
	if err != nil {
		return err
	}
	err = s.ProcessChildEvents(ctx, parents...)
	cleanupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, cleanupErr := s.Tasks.Queries.DeleteProcessedChildEvents(cleanupCtx, pgtype.Timestamptz{Time: time.Now().Add(-7 * 24 * time.Hour), Valid: true}); cleanupErr != nil {
		err = errors.Join(err, fmt.Errorf("expire sub-issue events: %w", cleanupErr))
	}
	return err
}

// BackfillChildDoneRules creates the rule on open parents that predate it,
// recording what already holds so nothing fires. It returns how many it
// created; zero means every open parent has its rule.
func (s *IssueWakeupService) BackfillChildDoneRules(ctx context.Context, limit int32) (int, error) {
	parents, err := s.Tasks.Queries.ListParentsWithoutSystemWakeup(ctx, db.ListParentsWithoutSystemWakeupParams{SystemRule: systemRuleText(SystemRuleChildDone), PageLimit: limit})
	if err != nil {
		return 0, err
	}
	created := 0
	for _, parent := range parents {
		if err := s.ensureRule(ctx, parent); err != nil {
			return created, err
		}
		created++
	}
	return created, nil
}

func (s *IssueWakeupService) ensureRule(ctx context.Context, parent db.Issue) error {
	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = EnsureChildDoneRule(ctx, tx, s.Tasks.Queries.WithTx(tx), parent); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// wakeTarget is who a system rule reaches: the parent's agent, its squad's
// leader, or its member assignee. Agent is set only when it can run.
type wakeTarget struct {
	Type    string // agent | squad | member | none
	ID      pgtype.UUID
	Agent   db.Agent
	SquadID pgtype.UUID
	// Refused is the outcome of a configured target that must not run: it is
	// gone, cannot run, or its writer may no longer invoke it.
	Refused string
}

func (t wakeTarget) key() string {
	return t.Type + ":" + util.UUIDToString(t.ID) + ":" + util.UUIDToString(t.Agent.ID) + ":" + util.UUIDToString(t.Agent.RuntimeID) + ":" + t.Refused
}

func resolveWakeTarget(ctx context.Context, q *db.Queries, issue db.Issue) (wakeTarget, error) {
	if !issue.AssigneeType.Valid || !issue.AssigneeID.Valid {
		return wakeTarget{Type: "none"}, nil
	}
	target := wakeTarget{Type: issue.AssigneeType.String, ID: issue.AssigneeID}
	usable := func(a db.Agent) bool { return a.RuntimeID.Valid && !a.ArchivedAt.Valid }
	switch target.Type {
	case "member":
		return target, nil
	case "agent":
		agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: issue.AssigneeID, WorkspaceID: issue.WorkspaceID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return target, err
		}
		if err == nil && usable(agent) {
			target.Agent = agent
		}
		return target, nil
	case "squad":
		squad, err := q.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: issue.AssigneeID, WorkspaceID: issue.WorkspaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return target, nil
		}
		if err != nil {
			return target, err
		}
		target.SquadID = squad.ID
		leader, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: squad.LeaderID, WorkspaceID: issue.WorkspaceID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return target, err
		}
		if err == nil && usable(leader) {
			target.Agent = leader
		}
		return target, nil
	}
	return wakeTarget{Type: "none"}, nil
}

// childDoneFacts summarizes the newest observation for the timeline entry and
// a member's notification: the closed stage (absent when every sub-issue
// closed) and how many sub-issues that covers.
func childDoneFacts(receipts []db.IssueWakeupReceipt) map[string]any {
	facts := map[string]any{"rule": SystemRuleChildDone}
	for i := len(receipts) - 1; i >= 0; i-- {
		if receipts[i].EventType != wakeupConditionEventType {
			continue
		}
		var payload struct {
			Observed struct {
				All    bool         `json:"all"`
				Stage  *int32       `json:"stage"`
				Total  int          `json:"total"`
				Stages []stageCount `json:"stages"`
			} `json:"observed"`
		}
		_ = json.Unmarshal(receipts[i].Payload, &payload)
		facts["total"] = payload.Observed.Total
		if !payload.Observed.All && payload.Observed.Stage != nil {
			facts["stage"] = *payload.Observed.Stage
			for _, sc := range payload.Observed.Stages {
				if sc.Stage == *payload.Observed.Stage {
					facts["total"] = sc.Total
				}
			}
		}
		break
	}
	return facts
}

func systemWakeupFacts(w db.IssueWakeup, receipts []db.IssueWakeupReceipt) map[string]any {
	if w.SystemRule.String == SystemRuleChildDone {
		return childDoneFacts(receipts)
	}
	facts := map[string]any{"rule": w.SystemRule.String}
	for i := len(receipts) - 1; i >= 0; i-- {
		if err := json.Unmarshal(receipts[i].Payload, &facts); err == nil {
			facts["rule"] = w.SystemRule.String
			return facts
		}
	}
	return facts
}

func systemWakeupInstruction(w db.IssueWakeup, settings []byte, receipts []db.IssueWakeupReceipt) string {
	if w.SystemRule.String == SystemRuleChildDone {
		return ChildDoneInstruction(w.Instruction, settings)
	}
	facts := systemWakeupFacts(w, receipts)
	prNumber, _ := facts["pr_number"].(float64)
	prURL, _ := facts["pr_url"].(string)
	issueStatus, _ := facts["issue_status"].(string)
	pr := fmt.Sprintf("PR #%d", int64(prNumber))
	if prURL != "" {
		pr += " (" + prURL + ")"
	}
	switch w.SystemRule.String {
	case SystemRulePRMerged:
		commit, _ := facts["merge_commit_sha"].(string)
		return fmt.Sprintf("A linked pull request has merged. Review %s at merge commit %s. The issue status when it merged was %s. Read the current issue and repository state, then decide what follow-up is needed.", pr, commit, issueStatus)
	case SystemRulePRChecksFailed:
		headSHA, _ := facts["head_sha"].(string)
		conclusion, _ := facts["conclusion"].(string)
		return fmt.Sprintf("Checks for linked %s finished with %s on head %s. The issue status when those checks finished was %s. Inspect the current check details and issue state, then decide what follow-up is needed.", pr, conclusion, headSHA, issueStatus)
	default:
		return w.Instruction
	}
}

func systemWakeupTriggerSummary(rule string) string {
	switch rule {
	case SystemRulePRMerged:
		return "Wakeup: linked PR merged"
	case SystemRulePRChecksFailed:
		return "Wakeup: linked PR checks failed"
	default:
		return "Wakeup: sub-issues closed"
	}
}

func systemWakeupIssueActive(ctx context.Context, q *db.Queries, issue db.Issue, prWakeup bool) (bool, error) {
	if !prWakeup {
		return wakeupIssueActive(ctx, q, issue)
	}
	category, err := issuestatus.CategoryWithError(ctx, q, issue.WorkspaceID, issue.Status)
	if err != nil {
		return false, err
	}
	return category != "closed", nil
}

// dispatchSystem evaluates a system rule and wakes, notifies or records its
// outcome. It mirrors dispatch: the same lock order, receipts, runaway
// protection and timeline entries.
func (s *IssueWakeupService) dispatchSystem(ctx context.Context, prev db.IssueWakeup) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	// Resolve the target and optional credentials before taking locks, then
	// confirm the target under them.
	snapshot, err := s.Tasks.Queries.GetIssue(ctx, prev.IssueID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	snapshotConfig, err := loadBuiltinWakeup(ctx, s.Tasks.Queries, snapshot, prev.SystemRule.String, &prev)
	if err != nil && !errors.Is(err, errWakeupConfigHeld) && !errors.Is(err, errMalformedPRWakeupSettings) {
		return err
	}
	target, err := builtinTarget(ctx, s, s.Tasks.Queries, snapshot, snapshotConfig)
	if err != nil {
		return err
	}
	headSHA := s.Tasks.ResolveIssueReviewSHAParam(ctx, prev.IssueID)
	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SET LOCAL lock_timeout = '50ms'"); err != nil {
		return err
	}
	q := s.Tasks.Queries.WithTx(tx)
	if target.Agent.ID.Valid {
		var fenced bool
		if err := tx.QueryRow(ctx, "SELECT lock_task_owner_rows($1,$2,$3)", target.Agent.ID, prev.IssueID, target.Agent.RuntimeID).Scan(&fenced); err != nil {
			return err
		}
		if !fenced {
			return pgx.ErrNoRows
		}
	} else if _, err = q.LockWorkspaceForChatSessionCreate(ctx, prev.WorkspaceID); err != nil {
		return err
	}
	issue, err := q.LockWakeupIssue(ctx, prev.IssueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return qCleanupMissingWakeup(ctx, tx, prev.ID)
	}
	if err != nil {
		return err
	}
	w, err := q.LockIssueWakeup(ctx, prev.ID)
	if err != nil {
		return err
	}
	if w.Revision != prev.Revision {
		return tx.Commit(ctx)
	}
	_, isPRWakeup := prWakeupSetting(w.SystemRule.String)
	discardMalformed := func(err error) error {
		slog.WarnContext(ctx, "pending PR wakeup discarded because workspace settings are malformed", "issue_id", util.UUIDToString(issue.ID), "wakeup_id", util.UUIDToString(w.ID), "rule", w.SystemRule.String, "error", err)
		if err := q.DiscardWakeupReceipts(ctx, w.ID); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	// Scoped definitions this build cannot execute hold the rule: its facts
	// stay pending, nothing runs.
	config, err := loadBuiltinWakeup(ctx, q, issue, w.SystemRule.String, &w)
	if errors.Is(err, errWakeupConfigHeld) {
		// What the instance's own flags already end (a closed parent, a rule
		// that is off) is still dropped; everything else waits for the hold to lift.
		active, activeErr := systemWakeupIssueActive(ctx, q, issue, isPRWakeup)
		if activeErr != nil {
			return activeErr
		}
		if !active || !w.Enabled && !(isPRWakeup && w.PausedReason.String == wakeupPausedRate) {
			if err := q.DiscardWakeupReceipts(ctx, w.ID); err != nil {
				return err
			}
		}
		logWakeupHeld(ctx, issue, w, err)
		return tx.Commit(ctx)
	}
	if errors.Is(err, errMalformedPRWakeupSettings) {
		return discardMalformed(err)
	}
	if err != nil {
		return err
	}
	// A configuration that changed since the last capture, dispatch or claim
	// retires what was captured under the old one.
	w, withdrawn, err := rebaseBuiltinConfig(ctx, tx, q, issue, w, config)
	if err != nil {
		return err
	}
	// Runs the rebase withdrew are announced once it commits, whichever way
	// this dispatch ends.
	commitWithdrawn := func() error {
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		for _, task := range withdrawn {
			s.Tasks.broadcastTaskEvent(ctx, protocol.EventTaskCancelled, task)
		}
		return nil
	}
	current, err := builtinTarget(ctx, s, q, issue, config)
	if err != nil {
		return err
	}
	if current.key() != target.key() {
		// The assignee changed after the read above; the pending receipts
		// stay for the next pass.
		return commitWithdrawn()
	}
	if _, err = tx.Exec(ctx, "UPDATE issue_wakeup_receipt SET processed_at=now() WHERE wakeup_id=$1 AND revision<>$2 AND processed_at IS NULL", w.ID, w.Revision); err != nil {
		return err
	}
	ratePaused := isPRWakeup && !w.Enabled && w.PausedReason.Valid && w.PausedReason.String == wakeupPausedRate
	active, err := systemWakeupIssueActive(ctx, q, issue, isPRWakeup)
	if err != nil {
		return err
	}
	// A closed parent or a rule that is off or paused keeps its state; a
	// system rule is never disabled by the platform for a closed issue. A
	// configured rule is on by its resolved enabled value; only a pause on the
	// instance itself (or the rate pause, which recovers) stops it.
	on := w.Enabled || ratePaused
	if config != nil {
		on = config.enabled() && (!systemWakeupPaused(w) || ratePaused)
	}
	if !active || !on {
		if err := q.DiscardWakeupReceipts(ctx, w.ID); err != nil {
			return err
		}
		return commitWithdrawn()
	}
	if isPRWakeup {
		ws, err := q.GetWorkspace(ctx, issue.WorkspaceID)
		if err != nil {
			return err
		}
		// Configured rules take their enabled value from the resolved chain
		// (the workspace setting is one input of it); the GitHub master switch
		// stays a veto at every scope.
		enabled, err := prRuleEnabled(ws.Settings, w.SystemRule.String, config != nil)
		if errors.Is(err, errMalformedPRWakeupSettings) {
			return discardMalformed(err)
		}
		if err != nil {
			return err
		}
		if !enabled {
			if err := q.DiscardWakeupReceipts(ctx, w.ID); err != nil {
				return err
			}
			return commitWithdrawn()
		}
	}
	if eligible, err := config.matchesIssue(ctx, q, issue); err != nil {
		return err
	} else if !eligible {
		// Not eligible now: facts that are point-in-time facts are dropped, a
		// sub-issue condition keeps its state until the issue qualifies.
		if isPRWakeup {
			err = q.DiscardWakeupReceipts(ctx, w.ID)
		} else {
			_, _, err = consumeConditionHints(ctx, tx, w.ID)
		}
		if err != nil {
			return err
		}
		return commitWithdrawn()
	}
	// A parked parent holds: nothing fires and nothing is marked as seen, so
	// a stage that closed meanwhile wakes the assignee once it leaves backlog.
	if issuestatus.Effective(ctx, q, issue.WorkspaceID, issue.Status) == "backlog" {
		if _, _, err = consumeConditionHints(ctx, tx, w.ID); err != nil {
			return err
		}
		return commitWithdrawn()
	}
	var now time.Time
	if err := tx.QueryRow(ctx, "SELECT now()").Scan(&now); err != nil {
		return err
	}
	if !isPRWakeup {
		_, causes, err := consumeConditionHints(ctx, tx, w.ID)
		if err != nil {
			return err
		}
		met, fingerprint, observed, err := evaluateCondition(ctx, tx, q, w)
		if err != nil {
			return err
		}
		state := w.ConditionState
		switch {
		case met && fingerprint != state:
			if err := recordConditionMet(ctx, q, w, observed, causes, now); err != nil {
				return err
			}
			state = fingerprint
		case !met:
			state = ""
		}
		if err := q.SetWakeupConditionState(ctx, db.SetWakeupConditionStateParams{ID: w.ID, ConditionState: state}); err != nil {
			return err
		}
	}
	receipts, err := q.ListPendingWakeupReceipts(ctx, db.ListPendingWakeupReceiptsParams{WakeupID: w.ID, Revision: w.Revision})
	if err != nil {
		return err
	}
	var activities []wakeupActivity
	var inbox []db.InboxItem
	note := func(action string, details map[string]any) error {
		a, err := recordWakeupActivity(ctx, q, w, action, "system", pgtype.UUID{}, details)
		if err == nil {
			activities = append(activities, a)
		}
		return err
	}
	commit := func() error {
		if err := commitWithdrawn(); err != nil {
			return err
		}
		s.publishWakeupActivities(activities...)
		for _, item := range inbox {
			s.publishSystemInbox(item, issue.Status)
		}
		return nil
	}
	// Facts a claimed run took along count once that run starts; until then
	// they wait with it, and they come back if it ends without starting.
	receipts, holding, taken, err := takenReceipts(ctx, q, receipts)
	if err != nil {
		return err
	}
	if isPRWakeup {
		var refused []db.IssueWakeupReceipt
		if receipts, refused = splitDeferredPRFilters(config, w.SystemRule.String, receipts); len(refused) > 0 {
			if err := q.ConsumeWakeupReceipts(ctx, db.ConsumeWakeupReceiptsParams{Ids: receiptIDs(refused)}); err != nil {
				return err
			}
			if err := note(wakeupActivityTriggered, map[string]any{"rule": w.SystemRule.String, "outcome": wakeupOutcomeFiltered, "filtered": len(refused)}); err != nil {
				return err
			}
		}
	}
	for _, run := range taken {
		if err := q.ConsumeWakeupReceipts(ctx, db.ConsumeWakeupReceiptsParams{Ids: receiptIDs(run.receipts), TaskID: run.task.ID}); err != nil {
			return err
		}
		if err := q.AdvanceIssueWakeup(ctx, db.AdvanceIssueWakeupParams{ID: w.ID, Enabled: true, LastTaskID: run.task.ID}); err != nil {
			return err
		}
		if err := q.CountWakeupFires(ctx, w.ID); err != nil {
			return err
		}
		w.FireCount++
		facts := systemWakeupFacts(w, run.receipts)
		facts["target_type"], facts["target_id"] = "agent", util.UUIDToString(run.task.AgentID)
		if current.Agent.ID == run.task.AgentID {
			facts["target_type"], facts["target_id"] = current.Type, util.UUIDToString(current.ID)
		}
		facts["outcome"], facts["task_id"] = wakeupOutcomeMerged, util.UUIDToString(run.task.ID)
		if err := note(wakeupActivityTriggered, facts); err != nil {
			return err
		}
	}
	// Once and max_fires end the instance for good, whichever run counted last.
	endAtLimit := func() error {
		if err := q.DiscardWakeupReceipts(ctx, w.ID); err != nil {
			return err
		}
		if systemWakeupPaused(w) {
			return nil
		}
		if err := q.PauseIssueWakeup(ctx, db.PauseIssueWakeupParams{ID: w.ID, PausedReason: pgtype.Text{String: wakeupPausedMaxFires, Valid: true}}); err != nil {
			return err
		}
		limit, _ := config.fireLimit()
		return note(wakeupActivityPaused, map[string]any{"rule": w.SystemRule.String, "reason": wakeupPausedMaxFires, "limit": limit})
	}
	fireLimit, limited := config.fireLimit()
	if len(taken) > 0 && limited && w.FireCount >= fireLimit {
		if err := endAtLimit(); err != nil {
			return err
		}
		return commit()
	}
	if len(receipts) == 0 {
		// Nothing is waiting, so no event will come to lift a rate pause;
		// the scheduler does it once the rolling limit has room.
		if ratePaused {
			if _, err := resumeRateLimited(ctx, q, &w, now, config.rateLimit()); err != nil {
				return err
			}
		}
		return commit()
	}
	ids := receiptIDs(receipts)
	if config.expired(w.CreatedAt.Time, now) {
		if err := q.DiscardWakeupReceipts(ctx, w.ID); err != nil {
			return err
		}
		if err := note(wakeupActivityTimedOut, map[string]any{"rule": w.SystemRule.String, "woke": false}); err != nil {
			return err
		}
		return commit()
	}
	if limited {
		switch {
		case w.FireCount >= fireLimit:
			if err := endAtLimit(); err != nil {
				return err
			}
			return commit()
		case w.FireCount+int32(holding) >= fireLimit:
			// Joined firings that have not started hold the remaining slots; a
			// carrier that never starts gives its inputs back, so they wait.
			return commit()
		}
	}
	facts := systemWakeupFacts(w, receipts)
	if current.Type != "none" {
		facts["target_type"], facts["target_id"] = current.Type, util.UUIDToString(current.ID)
	}
	// Recorded without a run: nobody to wake, a member to notify, or an
	// assignee whose agent cannot run.
	if current.Type == "member" || !current.Agent.ID.Valid {
		facts["outcome"] = "none"
		if current.Refused != "" {
			facts["outcome"] = current.Refused
		}
		if current.Type == "member" && !isPRWakeup {
			facts["outcome"] = "notified"
			details, _ := json.Marshal(facts)
			item, err := q.CreateInboxItem(ctx, db.CreateInboxItemParams{
				ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, RecipientType: "member", RecipientID: current.ID,
				Type: "children_done", Severity: "info", IssueID: issue.ID, Title: issue.Title,
				ActorType: pgtype.Text{String: "system", Valid: true}, Details: details,
			})
			if err != nil {
				return err
			}
			inbox = append(inbox, item)
		}
		if err := q.ConsumeWakeupReceipts(ctx, db.ConsumeWakeupReceiptsParams{Ids: ids}); err != nil {
			return err
		}
		if err := note(wakeupActivityTriggered, facts); err != nil {
			return err
		}
		return commit()
	}
	agent := current.Agent
	task, err := q.FindPendingWakeupTask(ctx, util.UUIDToString(w.ID))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	taskExists := err == nil
	if taskExists && task.Status == "dispatched" {
		// A claimed prompt is immutable; the new facts wait for the next run.
		return commitWithdrawn()
	}
	instruction := w
	if ws, e := q.GetWorkspace(ctx, issue.WorkspaceID); e == nil {
		instruction.Instruction = config.systemInstruction(w, ws.Settings, receipts)
	} else {
		instruction.Instruction = config.systemInstruction(w, nil, receipts)
	}
	noteText, evidence := mergeWakeupEvidence(instruction, task, receipts)
	if taskExists {
		task, err = q.ReplaceWakeupEvidence(ctx, db.ReplaceWakeupEvidenceParams{ID: task.ID, HandoffNote: pgtype.Text{String: noteText, Valid: true}, WakeupEvidence: evidence})
		if err != nil {
			return err
		}
		if err := q.ConsumeWakeupReceipts(ctx, db.ConsumeWakeupReceiptsParams{Ids: ids, TaskID: task.ID}); err != nil {
			return err
		}
		return commit()
	}
	// The agent's own unfinished run on the parent closed the stage: no second
	// run. A run of the agent waiting to start there takes the facts along
	// when it is claimed (JoinWaitingWakeups).
	self, err := acknowledgedBySelf(ctx, q, w, agent.ID, issue.ID, receipts)
	if err != nil {
		return err
	}
	if self {
		facts["outcome"] = wakeupOutcomeAcknowledged
		if err := q.ConsumeWakeupReceipts(ctx, db.ConsumeWakeupReceiptsParams{Ids: ids}); err != nil {
			return err
		}
		if err := note(wakeupActivityTriggered, facts); err != nil {
			return err
		}
		return commit()
	}
	attr, err := s.childDoneRunAs(ctx, issue, agent)
	if err != nil {
		return err
	}
	waiting, err := hasWaitingRunFor(ctx, q, issue.ID, agent.ID, attr.UserID, config, current)
	if err != nil {
		return err
	}
	if waiting {
		return commit()
	}
	if isPRWakeup {
		pendingTask, err := q.HasPendingIssueTaskForAgent(ctx, db.HasPendingIssueTaskForAgentParams{IssueID: issue.ID, AgentID: agent.ID})
		if err != nil {
			return err
		}
		if pendingTask {
			return commit()
		}
		startedAt, err := q.GetRunningTaskStartForIssueAndAgent(ctx, db.GetRunningTaskStartForIssueAndAgentParams{IssueID: issue.ID, AgentID: agent.ID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			var suppressed []db.IssueWakeupReceipt
			for _, receipt := range receipts {
				if !receipt.CreatedAt.Valid || !receipt.CreatedAt.Time.Before(startedAt.Time) {
					suppressed = append(suppressed, receipt)
				}
			}
			if len(suppressed) > 0 {
				activeFacts := systemWakeupFacts(w, suppressed)
				activeFacts["target_type"], activeFacts["target_id"] = current.Type, util.UUIDToString(current.ID)
				activeFacts["outcome"] = "suppressed_active_run"
				if err := q.ConsumeWakeupReceipts(ctx, db.ConsumeWakeupReceiptsParams{Ids: receiptIDs(suppressed)}); err != nil {
					return err
				}
				if err := note(wakeupActivityTriggered, activeFacts); err != nil {
					return err
				}
			}
			return commit()
		}
	}
	recent, err := q.CountRecentWakeupTasks(ctx, db.CountRecentWakeupTasksParams{WakeupID: util.UUIDToString(w.ID), IssueID: w.IssueID, Since: pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true}})
	if err != nil {
		return err
	}
	if rateLimit := config.rateLimit(); int(recent) >= rateLimit {
		if !ratePaused {
			if err := q.PauseIssueWakeup(ctx, db.PauseIssueWakeupParams{ID: w.ID, PausedReason: pgtype.Text{String: wakeupPausedRate, Valid: true}, BlockRuns: true}); err != nil {
				return err
			}
			if err := note(wakeupActivityPaused, map[string]any{"rule": w.SystemRule.String, "reason": wakeupPausedRate, "limit": rateLimit}); err != nil {
				return err
			}
		}
		return commit()
	}
	if ratePaused {
		resumed, err := resumeRateLimited(ctx, q, &w, now, config.rateLimit())
		if err != nil {
			return err
		}
		if !resumed {
			return commit()
		}
	}
	// A new paid start counts against every aggregate cap that applies. A full
	// counter delays the instance: its facts stay pending and it is looked at
	// again when the counter may have room.
	taskID := dbid.NewV7()
	if caps := config.aggregateCaps(); len(caps) > 0 {
		admission, err := admitAggregateStart(ctx, q, aggregateStart{WorkspaceID: issue.WorkspaceID, WakeupID: w.ID, TaskID: taskID, RuleKey: w.SystemRule.String}, caps, now)
		if aggregateCounterBusy(err) {
			return nil // another start holds the counter; this one is retried on a later pass
		}
		if err != nil {
			return err
		}
		if !admission.Admitted {
			if err := q.MarkWakeupAggregateBlocked(ctx, db.MarkWakeupAggregateBlockedParams{
				ID: w.ID, ScopeKind: pgtype.Text{String: string(admission.Blocked.Scope), Valid: true}, ScopeID: admission.Blocked.ScopeID,
				RetryAt: pgtype.Timestamptz{Time: admission.RetryAt, Valid: true},
			}); err != nil {
				return err
			}
			return commit()
		}
	}
	if w.AggregateRetryAt.Valid {
		if err := q.ClearWakeupAggregateBlocked(ctx, w.ID); err != nil {
			return err
		}
	}
	if err := guardIssueNotInTriage(ctx, q, issue.ID, OriginDerived); err != nil {
		return err
	}
	overlay := s.Tasks.buildRuntimeMCPOverlay(ctx, attr.UserID, agent)
	source, delegatedFrom, _, _ := attributionCreateParams(attr)
	contextJSON, _ := json.Marshal(map[string]any{"wakeup_id": util.UUIDToString(w.ID), "wakeup_revision": w.Revision, "wakeup_evidence": evidence, "wakeup_system": w.SystemRule.String})
	task, err = q.CreateWakeupTask(ctx, db.CreateWakeupTaskParams{
		ID: taskID, AgentID: agent.ID, RuntimeID: agent.RuntimeID, IssueID: issue.ID, Priority: priorityToInt(issue.Priority),
		TriggerSummary: pgtype.Text{String: systemWakeupTriggerSummary(w.SystemRule.String), Valid: true}, HandoffNote: pgtype.Text{String: noteText, Valid: true},
		IsLeaderTask: pgtype.Bool{Bool: current.Type == "squad", Valid: current.Type == "squad"}, SquadID: current.SquadID,
		OriginatorUserID: attr.UserID, AccountableUserID: attr.AccountableUserID, OriginatorSource: source, DelegatedFromTaskID: delegatedFrom,
		RuleVersionID: attr.RuleVersionID, TriggerEvidenceKind: pgtype.Text{String: "issue_wakeup", Valid: true}, TriggerEvidenceRefID: w.ID,
		HeadSha: headSHA, WakeupContext: contextJSON, RuntimeMcpOverlay: overlay.Overlay, RuntimeConnectedApps: overlay.ConnectedApps,
	})
	if err != nil {
		return err
	}
	if err := q.ConsumeWakeupReceipts(ctx, db.ConsumeWakeupReceiptsParams{Ids: ids, TaskID: task.ID}); err != nil {
		return err
	}
	if err := q.AdvanceIssueWakeup(ctx, db.AdvanceIssueWakeupParams{ID: w.ID, Enabled: true, LastTaskID: task.ID}); err != nil {
		return err
	}
	if err := q.CountWakeupFires(ctx, w.ID); err != nil {
		return err
	}
	facts["outcome"], facts["task_id"] = "woke", util.UUIDToString(task.ID)
	if err := note(wakeupActivityTriggered, facts); err != nil {
		return err
	}
	// The run that reaches the cap is legitimate; the instance ends after it.
	if limited && w.FireCount+1 >= fireLimit {
		if err := endAtLimit(); err != nil {
			return err
		}
	}
	if err := commit(); err != nil {
		return err
	}
	s.Tasks.broadcastTaskEvent(ctx, protocol.EventTaskQueued, task)
	s.Tasks.NotifyTaskEnqueued(ctx, task)
	return nil
}

// resumeRateLimited lifts a rate pause through the one guarded statement and
// mirrors it on w; false means the limit still leaves no room.
func resumeRateLimited(ctx context.Context, q *db.Queries, w *db.IssueWakeup, now time.Time, limit int) (bool, error) {
	n, err := q.ResumeRateLimitedSystemWakeup(ctx, db.ResumeRateLimitedSystemWakeupParams{ID: w.ID, Since: pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true}, MaxRuns: int64(limit)})
	if err != nil || n == 0 {
		return false, err
	}
	w.Enabled, w.PausedReason, w.DisabledAt = true, pgtype.Text{}, pgtype.Timestamptz{}
	return true, nil
}

func (s *IssueWakeupService) publishSystemInbox(item db.InboxItem, issueStatus string) {
	if s.Tasks == nil || s.Tasks.Bus == nil {
		return
	}
	s.Tasks.Bus.Publish(events.Event{
		Type: protocol.EventInboxNew, WorkspaceID: util.UUIDToString(item.WorkspaceID), ActorType: "system",
		Payload: map[string]any{"item": map[string]any{
			"id": util.UUIDToString(item.ID), "workspace_id": util.UUIDToString(item.WorkspaceID),
			"recipient_type": item.RecipientType, "recipient_id": util.UUIDToString(item.RecipientID),
			"type": item.Type, "severity": item.Severity, "issue_id": util.UUIDToPtr(item.IssueID),
			"title": item.Title, "body": util.TextToPtr(item.Body), "read": item.Read, "archived": item.Archived,
			"created_at": util.TimestampToString(item.CreatedAt), "actor_type": util.TextToPtr(item.ActorType),
			"actor_id": util.UUIDToPtr(item.ActorID), "details": json.RawMessage(item.Details), "issue_status": issueStatus,
		}},
	})
}

// SystemWakeupInput is a person's change to a system rule on one issue.
// Omitted fields keep their value.
type SystemWakeupInput struct {
	Enabled     *bool   `json:"enabled"`
	Instruction *string `json:"instruction"`
}

// UpdateChildDoneRule applies a person's change on one issue. From then on
// the rule no longer follows the workspace default. Turning it off withdraws
// runs that have not started.
func (s *IssueWakeupService) UpdateChildDoneRule(ctx context.Context, issueID pgtype.UUID, in SystemWakeupInput) (db.IssueWakeup, error) {
	var out db.IssueWakeup
	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	q := s.Tasks.Queries.WithTx(tx)
	issue, err := q.LockWakeupIssue(ctx, issueID)
	if err != nil {
		return out, err
	}
	rule, err := EnsureChildDoneRule(ctx, tx, q, issue)
	if err != nil {
		return out, err
	}
	if rule, err = q.LockIssueWakeup(ctx, rule.ID); err != nil {
		return out, err
	}
	enabled, instruction := rule.Enabled, rule.Instruction
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	if in.Instruction != nil {
		instruction = strings.TrimSpace(*in.Instruction)
	}
	if len(instruction) > MaxSystemWakeupInstruction {
		return out, fmt.Errorf("%w: instruction must be at most %d bytes", ErrWakeupInput, MaxSystemWakeupInstruction)
	}
	if out, err = q.CustomizeSystemWakeup(ctx, db.CustomizeSystemWakeupParams{ID: rule.ID, Enabled: enabled, Instruction: instruction}); err != nil {
		return out, err
	}
	if enabled && !rule.Enabled {
		if err := baselineChildDone(ctx, tx, q, out); err != nil {
			return out, err
		}
	}
	var cancelled []db.AgentTaskQueue
	if !enabled {
		if err := q.DiscardWakeupReceipts(ctx, rule.ID); err != nil {
			return out, err
		}
		if cancelled, err = q.CancelUnstartedWakeupTasks(ctx, util.UUIDToString(rule.ID)); err != nil {
			return out, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return out, err
	}
	for _, task := range cancelled {
		s.Tasks.broadcastTaskEvent(ctx, protocol.EventTaskCancelled, task)
	}
	return out, nil
}

// SetChildDoneDefault stores the rule's workspace default and applies it to
// every rule nobody customized. It returns how many rules changed.
func (s *IssueWakeupService) SetChildDoneDefault(ctx context.Context, workspaceID pgtype.UUID, enabled *bool, instruction *string) (int64, error) {
	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	changed, err := setChildDoneDefaultTx(ctx, tx, s.Tasks.Queries.WithTx(tx), workspaceID, enabled, instruction)
	if err != nil {
		return 0, err
	}
	return changed, tx.Commit(ctx)
}

// setChildDoneDefaultTx is SetChildDoneDefault inside the caller's transaction,
// so a caller can make the default and its own writes one atomic change.
func setChildDoneDefaultTx(ctx context.Context, tx pgx.Tx, q *db.Queries, workspaceID pgtype.UUID, enabled *bool, instruction *string) (int64, error) {
	patch := map[string]any{}
	if enabled != nil {
		patch[WorkspaceSettingChildDone] = *enabled
	}
	if instruction != nil {
		text := strings.TrimSpace(*instruction)
		if len(text) > MaxSystemWakeupInstruction {
			return 0, fmt.Errorf("%w: instruction must be at most %d bytes", ErrWakeupInput, MaxSystemWakeupInstruction)
		}
		patch[WorkspaceSettingChildDoneInstruction] = text
	}
	if len(patch) == 0 {
		return 0, nil
	}
	raw, _ := json.Marshal(patch)
	if err := q.MergeWorkspaceSettings(ctx, db.MergeWorkspaceSettingsParams{ID: workspaceID, Patch: raw}); err != nil {
		return 0, err
	}
	var changed []db.IssueWakeup
	var err error
	if enabled != nil {
		if changed, err = q.ApplySystemWakeupDefault(ctx, db.ApplySystemWakeupDefaultParams{WorkspaceID: workspaceID, SystemRule: systemRuleText(SystemRuleChildDone), Enabled: *enabled}); err != nil {
			return 0, err
		}
		for _, rule := range changed {
			if *enabled {
				if err := baselineChildDone(ctx, tx, q, rule); err != nil {
					return 0, err
				}
				continue
			}
			if err := q.DiscardWakeupReceipts(ctx, rule.ID); err != nil {
				return 0, err
			}
		}
	}
	return int64(len(changed)), nil
}
