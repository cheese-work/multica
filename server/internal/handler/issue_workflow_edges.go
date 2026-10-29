package handler

import (
	"context"
	"encoding/json"
	"errors"
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
	if def != nil {
		if step, ok := def.Step(issue.Status); ok {
			edges = issueworkflow.FactEdges(step.Edges)
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
	facts, err := h.loadEdgeFacts(ctx, issue, edges, def, w.CreatedAt.Time)
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
// when includeChildren, of its direct children; whether every child is done;
// the status of each issue an issue_status guard names, and which webhook
// guards have a success event since the issue entered the step.
func (h *Handler) loadEdgeFacts(ctx context.Context, issue db.Issue, edges []issueworkflow.Edge, def *issueworkflow.Definition, entered time.Time) (issueworkflow.Facts, error) {
	includeChildren := def.IncludeChildPRs
	var facts issueworkflow.Facts
	children, err := h.Queries.ListChildIssues(ctx, issue.ID)
	if err != nil {
		return facts, err
	}
	ids := []pgtype.UUID{issue.ID}
	facts.ChildrenDone = len(children) > 0
	for _, c := range children {
		if includeChildren {
			ids = append(ids, c.ID)
		}
		if facts.ChildrenDone {
			cat := issuestatus.Category(ctx, h.Queries, c.WorkspaceID, c.Status)
			facts.ChildrenDone = cat == issuestatus.CategoryDone || cat == issuestatus.CategoryClosed
		}
	}
	prs, err := h.Queries.ListEdgePullRequests(ctx, ids)
	if err != nil {
		return facts, err
	}
	for _, p := range prs {
		facts.PRs = append(facts.PRs, issueworkflow.PRFact{State: p.State, ChecksRollup: p.ChecksRollupState, HeadFresh: p.HeadFresh, HeadSHA: p.HeadSha})
	}
	if facts.IssueStatuses, err = h.guardedIssueStatuses(ctx, issue.WorkspaceID, edges); err != nil {
		return facts, err
	}
	facts.Webhooks, err = h.guardedWebhooks(ctx, issue, def.ID, edges, entered)
	return facts, err
}

// guardedIssueStatuses resolves the identifiers named by issue_status guards
// in the workspace; an identifier that names no issue here is left out, so its
// guard never holds.
func (h *Handler) guardedIssueStatuses(ctx context.Context, workspaceID pgtype.UUID, edges []issueworkflow.Edge) (map[string]string, error) {
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
		target, err := h.Queries.GetIssueByNumber(ctx, db.GetIssueByNumberParams{WorkspaceID: workspaceID, Number: parts.number})
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

// onRunBudgetExhausted blocks an issue whose workflow run budget is spent, so
// a person sees why nothing more runs. It leaves an issue that is already
// blocked alone (something else parked it), and one whose workflow has no
// blocked status, where the refusal alone holds the line.
func (h *Handler) onRunBudgetExhausted(ctx context.Context, e service.RunBudgetExhausted) {
	issue, err := h.Queries.GetIssue(ctx, e.IssueID)
	if err != nil || issue.Status == issuestatus.Blocked {
		return
	}
	def, _, err := issueworkflow.ForIssue(ctx, h.Queries, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
	if err != nil || def == nil || !def.Has(issuestatus.Blocked) {
		slog.Warn("run budget: cannot block issue", "err", err, "issue_id", uuidToString(issue.ID))
		return
	}
	details := map[string]any{"reason": issueworkflow.BlockedReasonRunBudget, "max_runs": e.Max, "runs": e.Runs}
	if moved, _ := h.moveIssueOnEdge(ctx, issue, issuestatus.Blocked, "run_budget", details, false); !moved {
		return
	}
	reason, _ := json.Marshal(issueworkflow.BlockedReasonRunBudget)
	if _, err := h.Queries.SetIssueMetadataKey(ctx, db.SetIssueMetadataKeyParams{Key: "blocked_reason", Value: reason, ID: issue.ID, WorkspaceID: issue.WorkspaceID}); err != nil {
		slog.Warn("run budget: set blocked_reason failed", "err", err, "issue_id", uuidToString(issue.ID))
	}
	body, _ := json.Marshal(details)
	if _, err := h.Queries.CreateActivity(ctx, db.CreateActivityParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ActorType: pgtype.Text{String: "system", Valid: true},
		Action: "run_budget_exhausted", Details: body,
	}); err != nil {
		slog.Warn("run budget: activity failed", "err", err, "issue_id", uuidToString(issue.ID))
	}
}
