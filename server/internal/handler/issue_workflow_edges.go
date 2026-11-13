package handler

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
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The workflow edge engine (CHE-2152). A trigger on issue.status keeps one
// workflow_edges wakeup per issue sitting on a step whose explicit edges read
// stored facts; the scheduler runs evaluateWorkflowEdges when it is due. The
// check is level-triggered: it reads the facts as they are now, so a PR that
// merged before the issue entered the step moves it on the first pass.

// edgeRetryDelay is how soon a watch looks again after a move lost a race or
// was refused.
const edgeRetryDelay = 30 * time.Second

// evaluateWorkflowEdges decides one due watch: it moves the issue along the
// first edge whose guard holds, or on its deadline, or leaves the watch to look
// again. A watch whose issue no longer sits on a step with fact edges is
// stale and dropped; the status trigger normally removes it first.
func (h *Handler) evaluateWorkflowEdges(ctx context.Context, w db.IssueWakeup) error {
	issue, err := h.Queries.GetIssue(ctx, w.IssueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return h.Queries.DeleteWorkflowEdges(ctx, w.ID)
	}
	if err != nil {
		return err
	}
	if issue.Status != w.ConditionState || issue.TriageState.Valid ||
		issuestatus.Category(ctx, h.Queries, issue.WorkspaceID, issue.Status) == issuestatus.CategoryClosed {
		return h.Queries.DeleteWorkflowEdges(ctx, w.ID)
	}
	def, _, err := issueworkflow.ForIssue(ctx, h.Queries, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
	if err != nil && !errors.Is(err, issueworkflow.ErrOverrideMissing) {
		h.watchAgainIn(ctx, w, edgeRetryDelay)
		return err
	}
	var edges []issueworkflow.Edge
	var sub *issueworkflow.SubWorkflow
	if def != nil {
		if step, ok := def.Step(issue.Status); ok {
			edges = issueworkflow.FactEdges(step.Edges)
			sub = step.SubWorkflow
		}
	}
	if len(edges) == 0 {
		return h.Queries.DeleteWorkflowEdges(ctx, w.ID)
	}
	// A paused or finished project holds its issues where they are.
	if issue.ProjectID.Valid {
		if status, err := h.Queries.GetIssueProjectStatus(ctx, issue.ID); err == nil && service.ProjectInactive(status) {
			h.watchAgainIn(ctx, w, issueworkflow.EdgePollInterval)
			return nil
		}
	}
	if sub != nil {
		switch err := h.spawnSubWorkflowChild(ctx, issue, *sub); {
		case errors.Is(err, errSubWorkflowMissing):
			// No retry can fix this: the join finds no child, so the step's
			// deadline edge, if it has one, still moves the issue.
			slog.Warn("workflow edge: sub-workflow is not in this workspace", "issue_id", uuidToString(issue.ID), "workflow_id", sub.WorkflowID)
		case errors.Is(err, errSubWorkflowRefused):
			slog.Warn("workflow edge: sub-workflow child refused", "issue_id", uuidToString(issue.ID), "workflow_id", sub.WorkflowID, "error", err)
		case err != nil:
			h.watchAgainIn(ctx, w, edgeRetryDelay)
			return err
		}
	}
	facts, err := h.loadEdgeFacts(ctx, h.Queries, issue, edges, def, w.CreatedAt.Time, sub)
	if err != nil {
		h.watchAgainIn(ctx, w, edgeRetryDelay)
		return err
	}

	decision, move, next := issueworkflow.Decide(edges, facts, w.CreatedAt.Time, time.Now(), def.Has(issuestatus.Blocked))
	claimed := ""
	for range facts.PRs {
		if !move || decision.Guard != issueworkflow.GuardPRChecksGreen || decision.Reason != issueworkflow.ReasonEdge {
			break
		}
		head := greenHead(facts)
		n, err := h.Queries.ClaimIssueCIAdvance(ctx, db.ClaimIssueCIAdvanceParams{IssueID: issue.ID, HeadSha: head})
		if err != nil {
			h.watchAgainIn(ctx, w, edgeRetryDelay)
			return err
		}
		if n > 0 {
			claimed = head
			break
		}
		// This head already advanced the issue once: it is spent, so the guard
		// waits for a new head and the deadline still counts.
		facts = spendHead(facts, head)
		decision, move, next = issueworkflow.Decide(edges, facts, w.CreatedAt.Time, time.Now(), def.Has(issuestatus.Blocked))
	}
	if !move {
		if next.IsZero() {
			return h.Queries.DeleteWorkflowEdges(ctx, w.ID)
		}
		return h.Queries.ScheduleWorkflowEdges(ctx, db.ScheduleWorkflowEdgesParams{ID: w.ID, NextFireAt: pgtype.Timestamptz{Time: next, Valid: true}})
	}
	moved, _ := h.moveIssueOnEdge(ctx, issue, decision.To, "workflow_edge", map[string]any{"guard": decision.Guard, "reason": decision.Reason}, true)
	if !moved {
		if claimed != "" {
			if err := h.Queries.ReleaseIssueCIAdvance(ctx, db.ReleaseIssueCIAdvanceParams{IssueID: issue.ID, HeadSha: claimed}); err != nil {
				slog.Warn("workflow edge: release claim failed", "err", err, "issue_id", uuidToString(issue.ID))
			}
		}
		h.watchAgainIn(ctx, w, edgeRetryDelay)
	}
	return nil
}

func (h *Handler) watchAgainIn(ctx context.Context, w db.IssueWakeup, d time.Duration) {
	err := h.Queries.ScheduleWorkflowEdges(ctx, db.ScheduleWorkflowEdgesParams{ID: w.ID, NextFireAt: pgtype.Timestamptz{Time: time.Now().Add(d), Valid: true}})
	if err != nil {
		slog.Warn("workflow edge: reschedule failed", "err", err, "wakeup_id", uuidToString(w.ID))
	}
}

// greenHead is the head of the first PR whose checks pass on its newest head.
func greenHead(f issueworkflow.Facts) string {
	for _, p := range f.PRs {
		if (p.State == "open" || p.State == "draft") && p.HeadFresh && p.ChecksRollup == "SUCCESS" {
			return p.HeadSHA
		}
	}
	return ""
}

// spendHead returns the facts with the PRs on head no longer green.
func spendHead(f issueworkflow.Facts, head string) issueworkflow.Facts {
	prs := make([]issueworkflow.PRFact, len(f.PRs))
	for i, p := range f.PRs {
		if p.HeadSHA == head {
			p.HeadFresh = false
		}
		prs[i] = p
	}
	f.PRs = prs
	return f
}

// loadEdgeFacts reads what the edges' guards need: the PRs of the issue and,
// when includeChildren, of its direct children; whether every child is done
// (on a sub-workflow step, every child of the step's stage); the status of
// each issue an issue_status guard names, and which webhook guards have a
// success event since the issue entered the step.
func (h *Handler) loadEdgeFacts(ctx context.Context, q *db.Queries, issue db.Issue, edges []issueworkflow.Edge, def *issueworkflow.Definition, entered time.Time, sub *issueworkflow.SubWorkflow) (issueworkflow.Facts, error) {
	includeChildren := def.IncludeChildPRs
	var facts issueworkflow.Facts
	children, err := q.ListChildIssues(ctx, issue.ID)
	if err != nil {
		return facts, err
	}
	ids := []pgtype.UUID{issue.ID}
	joined := 0
	facts.ChildrenDone = true
	for _, c := range children {
		if includeChildren {
			ids = append(ids, c.ID)
		}
		if sub != nil && (!c.Stage.Valid || c.Stage.Int32 != int32(sub.Stage)) {
			continue
		}
		joined++
		if facts.ChildrenDone {
			cat := issuestatus.Category(ctx, q, c.WorkspaceID, c.Status)
			facts.ChildrenDone = cat == issuestatus.CategoryDone || cat == issuestatus.CategoryClosed
		}
	}
	facts.ChildrenDone = facts.ChildrenDone && joined > 0
	prs, err := q.ListEdgePullRequests(ctx, ids)
	if err != nil {
		return facts, err
	}
	for _, p := range prs {
		facts.PRs = append(facts.PRs, issueworkflow.PRFact{State: p.State, ChecksRollup: p.ChecksRollupState, HeadFresh: p.HeadFresh, HeadSHA: p.HeadSha, PRHead: p.PrHeadSha, Review: p.Review, RedCause: p.RedCause})
	}
	if facts.IssueStatuses, err = h.guardedIssueStatuses(ctx, q, issue.WorkspaceID, edges); err != nil {
		return facts, err
	}
	if hasGuard(edges, issueworkflow.GuardAdoptionLinked) {
		facts.Adoption = h.loadAdoptionFacts(ctx, q, issue)
	}
	facts.Webhooks, err = h.guardedWebhooks(ctx, issue, def.ID, edges, entered)
	return facts, err
}

func hasGuard(edges []issueworkflow.Edge, kind string) bool {
	for _, e := range edges {
		if g, err := issueworkflow.ParseGuard(e.Guard); err == nil && g.Kind == kind {
			return true
		}
	}
	return false
}

// guardedIssueStatuses resolves the identifiers named by issue_status guards
// in the workspace; an identifier that names no issue here is left out, so its
// guard never holds.
func (h *Handler) guardedIssueStatuses(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, edges []issueworkflow.Edge) (map[string]string, error) {
	var out map[string]string
	prefix := ""
	for _, e := range edges {
		g, err := issueworkflow.ParseGuard(e.Guard)
		if err != nil || g.Kind != issueworkflow.GuardIssueStatus {
			continue
		}
		parts := splitIdentifier(g.Args[0])
		if parts == nil {
			continue
		}
		if prefix == "" {
			prefix = h.getIssuePrefix(ctx, workspaceID)
		}
		if !strings.EqualFold(parts.prefix, prefix) {
			continue
		}
		target, err := q.GetIssueByNumber(ctx, db.GetIssueByNumberParams{WorkspaceID: workspaceID, Number: parts.number})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if out == nil {
			out = map[string]string{}
		}
		out[g.Args[0]] = target.Status
	}
	return out, nil
}

// onRunBudgetExhausted is the exhaustion hook: three idempotent transactions
// over one refusal row (run_budget_refusal_hook.go). R records the refusal, B
// blocks the issue, N writes the run-budget notice (one comment and inbox item
// per budget window) and the recovery run. B and N each re-check that the refusal
// still applies and retire the hook if not, so a finish written in between
// (CHE-2226) or a person's move stays. An issue that is already blocked and one
// whose workflow has no blocked status get the notice alone, and an issue in a
// review status is blocked only by a refusal of its reviewer. A restart sweep
// finishes a hook this callback did not.
func (h *Handler) onRunBudgetExhausted(ctx context.Context, e service.RunBudgetExhausted) {
	// The auto-approver settles a window that made progress before the issue
	// stops: the top-up resumes the refused run, and nothing is blocked.
	if h.autoApproveExhausted(ctx, e) {
		return
	}
	if id, generation, ok := h.recordRefusalIntent(ctx, e); ok {
		h.advanceRefusalHook(ctx, id, generation, e)
	}
}

// prepareBudgetRecovery reads the blocked step's handler's recovery run for
// the notice's transaction to claim. Only an agent handler gets one, only for an
// issue the budget blocked in this window (this callback's block, or an earlier
// callback's that exited before its notice), and the claim allows one per
// window. It returns nil when the issue is owed none: an issue a person or
// another rule blocked gets the notice alone.
func (h *Handler) prepareBudgetRecovery(ctx context.Context, e service.RunBudgetExhausted) *service.BudgetRecoveryRun {
	issue, err := h.Queries.GetIssue(ctx, e.IssueID)
	if err != nil || issue.Status != issuestatus.Blocked || issue.AssigneeType.String != "agent" || !issue.AssigneeID.Valid {
		return nil
	}
	if parked, err := h.Queries.HasRunBudgetNotice(ctx, db.HasRunBudgetNoticeParams{
		IssueID: issue.ID, WindowID: e.Since.UTC().Format(time.RFC3339Nano), Kind: service.RunBudgetKindBlocked,
	}); err != nil || !parked {
		return nil
	}
	if id, ok := h.blockedAgentHandler(ctx, issue); !ok || id != uuidToString(issue.AssigneeID) {
		return nil
	}
	run, err := h.TaskService.PrepareBudgetRecoveryRun(ctx, issue)
	if err != nil {
		slog.Warn("run budget: recovery run not prepared", "err", err, "issue_id", uuidToString(issue.ID))
		return nil
	}
	return run
}

// blockedAgentHandler returns the agent that handles the workflow's blocked
// step. The assignee of a blocked step with no agent handler is whoever held
// the issue before, not a handler the workflow chose for this state.
func (h *Handler) blockedAgentHandler(ctx context.Context, issue db.Issue) (string, bool) {
	def, _, err := issueworkflow.ForIssue(ctx, h.Queries, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
	if err != nil || def == nil {
		return "", false
	}
	step, ok := def.Step(issuestatus.Blocked)
	if !ok || step.Handler.Type != "agent" {
		return "", false
	}
	return step.Handler.ID, true
}

// runBudgetBlockTries bounds the block's re-reads after losing a revision race.
const runBudgetBlockTries = 3

// runBudgetBeforeRecovery runs after the block commits and before the recovery
// run is claimed, which happens in the notice transaction. Tests use it to land
// a budget write in that window.
var runBudgetBeforeRecovery = func() {}

// runBudgetBeforeBlock runs between the callback's reads and its block write.
// Tests use it to land a budget write in that window.
var runBudgetBeforeBlock = func() {}

// errRefusalLifted marks a block that found, under the budget lock, that the
// refusal no longer holds: a top-up or a progress grant landed first.
var errRefusalLifted = fmt.Errorf("run budget: the refusal no longer applies: %w", pgx.ErrNoRows)

// recordBudgetBlock logs the exhaustion block with the cap a raise would set.
// It runs after the recovery run is enqueued, so the target counts that run.
func (h *Handler) recordBudgetBlock(ctx context.Context, issue db.Issue, e service.RunBudgetExhausted, refused string) {
	body, _ := json.Marshal(map[string]any{
		"reason": issueworkflow.BlockedReasonRunBudget, "max_runs": e.Max, "runs": e.Runs, "refused_run": refused,
		"raise_target": runBudgetRaiseTarget(ctx, h.Queries, issue.ID, e).target,
	})
	if _, err := h.Queries.CreateActivity(ctx, db.CreateActivityParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ActorType: pgtype.Text{String: "system", Valid: true},
		Action: "run_budget_exhausted", Details: body,
	}); err != nil {
		slog.Warn("run budget: activity failed", "err", err, "issue_id", uuidToString(issue.ID))
	}
}

// onReviewRunSkipped records a reviewer run the review-head guard refused
// (CHE-4649) on the issue's activity, which is where the skip is read. The
// skipped run is no task row, so nothing else would show it.
func (h *Handler) onReviewRunSkipped(ctx context.Context, e service.ReviewRunSkipped) {
	issue, err := h.Queries.GetIssue(ctx, e.IssueID)
	if err != nil {
		slog.Warn("review run skipped: load issue failed", "err", err, "issue_id", uuidToString(e.IssueID))
		return
	}
	body, _ := json.Marshal(map[string]any{
		"reason": string(ReasonReviewHeadAnswered), "agent_id": uuidToString(e.Target.AgentID),
		"head_sha": e.Head, "trigger": e.Target.Trigger,
	})
	if _, err := h.Queries.CreateActivity(ctx, db.CreateActivityParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ActorType: pgtype.Text{String: "system", Valid: true},
		Action: "run_skipped", Details: body,
	}); err != nil {
		slog.Warn("review run skipped: activity failed", "err", err, "issue_id", uuidToString(issue.ID))
	}
}

// exhaustedNoticeText is the exhaustion comment: what was refused, whether the
// issue was blocked by it, and the raise a person approves in the web app.
func exhaustedNoticeText(refused string, raise, leaves int, e service.RunBudgetExhausted, blocked bool) string {
	lead := "Run budget used up"
	if blocked {
		lead = "Blocked"
	}
	return runBudgetNoticeMessage(lead, refused, int(e.Runs), e.Max, raise, leaves)
}

// runBudgetBlockedMessage is the system comment for a budget block.
func runBudgetBlockedMessage(refused string, runs, max, raise, leaves int) string {
	return runBudgetNoticeMessage("Blocked", refused, runs, max, raise, leaves)
}

// runBudgetNoticeMessage words a budget notice. A raise is approved in the web
// app, never by a command an agent host could run. Without a valid raise target
// (the platform maximum leaves no valid raise) it names the maximum and the
// recovery a person has instead.
func runBudgetNoticeMessage(lead, refused string, runs, max, raise, leaves int) string {
	if raise == 0 {
		return fmt.Sprintf(
			"%s: %s was refused because the issue used %d of %d runs. The platform maximum of %d runs is reached, so no raise is possible. A person moves the issue's status to open a new run window.",
			lead, refused, runs, max, issueworkflow.MaxRunsPerIssueLimit)
	}
	return fmt.Sprintf(
		"%s: %s was refused because the issue used %d of %d runs. To allow more runs, a person approves a raise to %d runs in the web app, which leaves %d runs. Until then, a person moves the issue's status in the web app.",
		lead, refused, runs, max, raise, leaves)
}

// runBudgetRefusedRun names the run the budget refused in one phrase: the
// agent that would have received it, what asked, and the status the issue was
// in.
func (h *Handler) runBudgetRefusedRun(ctx context.Context, e service.RunBudgetExhausted) string {
	who := "an agent"
	if e.Target.AgentID.Valid {
		if agent, err := h.Queries.GetAgent(ctx, e.Target.AgentID); err == nil {
			who = "agent " + agent.Name
		}
	}
	trigger := e.Target.Trigger
	if trigger == "" {
		trigger = "derived"
	}
	return fmt.Sprintf("a run for %s (trigger: %s) while the issue was %s", who, trigger, e.Status)
}

// budgetRaise is a suggested raise: the cap it sets and the runs it leaves.
type budgetRaise struct {
	target int
	leaves int
}

// runBudgetRaiseTarget is the cap that leaves 10 runs after the balance is paid
// off, and the runs it leaves. It reads only through q, so it can run
// inside the notice's transaction without a second pool connection. It prices the raise
// from the ledger as q reads it now, not from the balance the refusal saw. The
// refusal's own balance is the fallback when the ledger cannot be read. The
// target is 0 when the platform maximum leaves no valid raise.
func runBudgetRaiseTarget(ctx context.Context, q *db.Queries, issueID pgtype.UUID, e service.RunBudgetExhausted) budgetRaise {
	max, balance, err := service.CurrentRunBudget(ctx, q, issueID)
	if err != nil {
		max, balance = e.Max, e.Balance
	}
	target, leaves := service.SuggestedRaiseForBalance(max, balance)
	if target == 0 {
		return budgetRaise{}
	}
	return budgetRaise{target: target, leaves: leaves}
}
