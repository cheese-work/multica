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
	"github.com/multica-ai/multica/server/pkg/dbid"
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
		facts.PRs = append(facts.PRs, issueworkflow.PRFact{State: p.State, ChecksRollup: p.ChecksRollupState, HeadFresh: p.HeadFresh, HeadSHA: p.HeadSha, PRHead: p.PrHeadSha, Review: p.Review})
	}
	if facts.IssueStatuses, err = h.guardedIssueStatuses(ctx, q, issue.WorkspaceID, edges); err != nil {
		return facts, err
	}
	facts.Webhooks, err = h.guardedWebhooks(ctx, issue, def.ID, edges, entered)
	return facts, err
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

// onRunBudgetExhausted blocks an issue whose workflow run budget is spent, so
// a person sees why nothing more runs, and writes the run-budget notice (one
// comment and inbox item per budget window). It leaves an issue that is already
// blocked (something else parked it) and one whose workflow has no blocked
// status where they are, with the notice alone, and skips both when the issue is
// no longer in the status the refused run was for: the hook runs after the
// refusal, and a finish written in between (CHE-2226) must stay. An issue in a
// review status is blocked only by a refusal of its reviewer.
func (h *Handler) onRunBudgetExhausted(ctx context.Context, e service.RunBudgetExhausted) {
	var issue db.Issue
	var refused string
	moved := false
	var claim pgtype.UUID
	// Another writer (the 80% warning's comment, a person) can bump the issue's
	// revision between the read and the block. The block then loses, so it
	// reads the issue again and retries while the revision keeps moving.
	for try := 0; try < runBudgetBlockTries; try++ {
		var ok bool
		if issue, ok = h.exhaustedIssue(ctx, e); !ok {
			return
		}
		// One callback per window owns the block, the recovery run and the
		// notice, so no other callback words a notice the owner's recovery
		// debit then outdates. The owner is the first whose refusal still
		// holds; it gives the claim back when it is done, so a later refusal in
		// the same window (the issue moved back to work) is handled again.
		if !claim.Valid {
			if claim = h.claimExhaustionHandling(ctx, issue, e); !claim.Valid {
				return
			}
			defer h.releaseExhaustionHandling(ctx, claim)
		}
		refused = h.runBudgetRefusedRun(ctx, e)
		var lifted bool
		if moved, lifted = h.blockExhaustedIssue(ctx, issue, e, refused); lifted {
			return
		}
		if moved || issue.Status == issuestatus.Blocked {
			break
		}
		if latest, err := h.Queries.GetIssue(ctx, e.IssueID); err != nil || latest.Revision == issue.Revision {
			break
		}
	}
	// The recovery run debits the ledger, so it comes before anything that
	// states the balance. The command is priced from the ledger as it stands
	// then: in the notice, under the budget lock its transaction takes.
	ref := issueBudgetRef(h.getIssuePrefix(ctx, issue.WorkspaceID), issue)
	if moved {
		runBudgetBeforeRecovery()
		h.enqueueBudgetRecovery(ctx, e)
		h.recordBudgetBlock(ctx, issue, e, ref, refused)
	}
	h.writeRunBudgetNoticeFrom(ctx, service.RunBudgetEvent{IssueID: e.IssueID, Kind: service.RunBudgetKindExhausted, Runs: e.Runs, Max: e.Max, Since: e.Since},
		func(ctx context.Context, q *db.Queries) string {
			raise := runBudgetRaiseCommand(ctx, q, issue.ID, ref, e)
			return exhaustedNoticeText(refused, raise.command, raise.leaves, e, moved)
		})
}

// claimExhaustionHandling claims the window's exhaustion handling for this
// callback; the claim is invalid when another callback holds it. A claim older
// than runBudgetHandlingLease belongs to an owner that exited or could not
// release it, and is taken over. A failed claim counts as lost: the holder, or
// a later refusal, handles the window.
func (h *Handler) claimExhaustionHandling(ctx context.Context, issue db.Issue, e service.RunBudgetExhausted) pgtype.UUID {
	window := e.Since.UTC().Format(time.RFC3339Nano)
	id, err := h.Queries.ClaimRunBudgetNotice(ctx, db.ClaimRunBudgetNoticeParams{
		ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, WindowID: window, Kind: runBudgetKindHandling,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		id, err = h.Queries.TakeOverRunBudgetNotice(ctx, db.TakeOverRunBudgetNoticeParams{
			ID: dbid.NewV7(), IssueID: issue.ID, WindowID: window, Kind: runBudgetKindHandling,
			LeaseSeconds: runBudgetHandlingLease.Seconds(),
		})
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Warn("run budget: claim exhaustion handling failed", "err", err, "issue_id", uuidToString(issue.ID))
	}
	return id
}

func (h *Handler) releaseExhaustionHandling(ctx context.Context, claim pgtype.UUID) {
	if err := h.Queries.ReleaseRunBudgetNotice(context.WithoutCancel(ctx), claim); err != nil {
		slog.Warn("run budget: release exhaustion handling failed", "err", err)
	}
}

// runBudgetKindHandling keys the claim of a window's exhaustion handling. It
// shares the claim table with the notices but writes no notice.
const runBudgetKindHandling = "handling"

// runBudgetHandlingLease is how long a handling claim holds before another
// callback may take it over. The handling is a few queries and one
// transaction, so a claim this old has no live owner.
const runBudgetHandlingLease = 2 * time.Minute

// enqueueBudgetRecovery gives the blocked step's handler its one recovery run
// for the window. Only an agent handler gets one; a second claim in the window
// is refused.
func (h *Handler) enqueueBudgetRecovery(ctx context.Context, e service.RunBudgetExhausted) {
	issue, err := h.Queries.GetIssue(ctx, e.IssueID)
	if err != nil || issue.Status != issuestatus.Blocked || issue.AssigneeType.String != "agent" || !issue.AssigneeID.Valid {
		return
	}
	if id, ok := h.blockedAgentHandler(ctx, issue); !ok || id != uuidToString(issue.AssigneeID) {
		return
	}
	if _, err := h.TaskService.EnqueueBudgetRecoveryRun(ctx, issue, e.Since); err != nil && !errors.Is(err, service.ErrRecoveryClaimed) && !errors.Is(err, service.ErrRecoveryWindowLifted) {
		slog.Warn("run budget: recovery run not enqueued", "err", err, "issue_id", uuidToString(issue.ID))
	}
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

// exhaustedIssue reads the issue a refusal is about and reports whether the
// refusal still applies: not finished, its window still current and still
// spent, and still in the status that was refused unless it is already blocked.
func (h *Handler) exhaustedIssue(ctx context.Context, e service.RunBudgetExhausted) (db.Issue, bool) {
	issue, err := h.Queries.GetIssue(ctx, e.IssueID)
	if err != nil || service.IsFinishedStatus(ctx, h.Queries, issue.WorkspaceID, issue.Status) {
		return issue, false
	}
	// Read after the issue, so a top-up that commits after this point moves the
	// revision the block is conditional on, and the retry reads it again.
	if refuses, err := service.RunBudgetStillRefuses(ctx, h.Queries, e.IssueID, e.Since); err != nil || !refuses {
		return issue, false
	}
	if issue.Status == issuestatus.Blocked {
		return issue, true
	}
	if issue.Status != e.Status {
		return issue, false
	}
	// A refused run for anyone but the reviewer must not block the review: the
	// reviewer's own first run on a head is still admitted.
	if service.IsReviewStatus(issue.Status) && issue.AssigneeID != e.Target.AgentID {
		return issue, false
	}
	return issue, true
}

// runBudgetBeforeRecovery runs after the block commits and before the recovery
// run is claimed. Tests use it to land a budget write in that window.
var runBudgetBeforeRecovery = func() {}

// runBudgetBeforeBlock runs between the callback's reads and its block write.
// Tests use it to land a budget write in that window.
var runBudgetBeforeBlock = func() {}

// errRefusalLifted marks a block that found, under the budget lock, that the
// refusal no longer holds: a top-up or a progress grant landed first.
var errRefusalLifted = fmt.Errorf("run budget: the refusal no longer applies: %w", pgx.ErrNoRows)

// blockExhaustedIssue moves the issue to blocked and records why. It reports
// whether this call moved the issue, and whether the budget was restored
// before the move so the refusal no longer applies.
//
// The check runs in the status write's transaction under the budget lock that
// every grant, top-up and admission takes. A balance-restoring write either
// commits before the check and the block is dropped, or waits for the block's
// commit and is judged on the blocked issue. A write that changes no issue
// field (a head grant) cannot be caught by the expected revision alone.
func (h *Handler) blockExhaustedIssue(ctx context.Context, issue db.Issue, e service.RunBudgetExhausted, refused string) (moved, lifted bool) {
	if issue.Status == issuestatus.Blocked {
		return false, false
	}
	runBudgetBeforeBlock()
	def, _, err := issueworkflow.ForIssue(ctx, h.Queries, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
	if err != nil || def == nil || !def.Has(issuestatus.Blocked) {
		slog.Warn("run budget: cannot block issue", "err", err, "issue_id", uuidToString(issue.ID))
		return false, false
	}
	details := map[string]any{
		"reason": issueworkflow.BlockedReasonRunBudget, "max_runs": e.Max, "runs": e.Runs,
		"refused_run": refused,
	}
	stillRefused := func(ctx context.Context, q *db.Queries, _ *db.UpdateIssueParams) error {
		if _, err := q.LockIssueRunBudget(ctx, issue.ID); err != nil {
			return err
		}
		if still, err := service.RunBudgetStillRefuses(ctx, q, issue.ID, e.Since); err != nil || !still {
			if err == nil {
				err = errRefusalLifted
			}
			return err
		}
		return nil
	}
	moved, _, err = h.tryMoveIssueOnEdge(withIssueMoveGuard(ctx, stillRefused), issue, issuestatus.Blocked, "run_budget", details, false)
	if errors.Is(err, errRefusalLifted) {
		return false, true
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Warn("run budget: block failed", "err", err, "issue_id", uuidToString(issue.ID))
	}
	if !moved {
		return false, false
	}
	reason, _ := json.Marshal(issueworkflow.BlockedReasonRunBudget)
	if _, err := h.Queries.SetIssueMetadataKey(ctx, db.SetIssueMetadataKeyParams{Key: "blocked_reason", Value: reason, ID: issue.ID, WorkspaceID: issue.WorkspaceID}); err != nil {
		slog.Warn("run budget: set blocked_reason failed", "err", err, "issue_id", uuidToString(issue.ID))
	}
	return true, false
}

// recordBudgetBlock logs the exhaustion block with the command that lifts it.
// It runs after the recovery run is enqueued, so the command counts that run.
func (h *Handler) recordBudgetBlock(ctx context.Context, issue db.Issue, e service.RunBudgetExhausted, ref, refused string) {
	details := map[string]any{
		"reason": issueworkflow.BlockedReasonRunBudget, "max_runs": e.Max, "runs": e.Runs, "refused_run": refused, "raise_command": "",
	}
	if !e.TokensSpent {
		details["raise_command"] = runBudgetRaiseCommand(ctx, h.Queries, issue.ID, ref, e).command
	}
	body, _ := json.Marshal(details)
	if _, err := h.Queries.CreateActivity(ctx, db.CreateActivityParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ActorType: pgtype.Text{String: "system", Valid: true},
		Action: "run_budget_exhausted", Details: body,
	}); err != nil {
		slog.Warn("run budget: activity failed", "err", err, "issue_id", uuidToString(issue.ID))
	}
}

// exhaustedNoticeText is the exhaustion comment: what was refused, whether the
// issue was blocked by it, and the command that lifts the cap.
func exhaustedNoticeText(refused, raise string, leaves int, e service.RunBudgetExhausted, blocked bool) string {
	lead := "Run budget used up"
	if blocked {
		lead = "Blocked"
	}
	if e.TokensSpent {
		return fmt.Sprintf(
			"%s: %s was refused because the issue's token allowance is spent (balance %d). No command raises it. A person moves the issue's status to open a new window with a new allowance.",
			lead, refused, e.Tokens)
	}
	return runBudgetNoticeMessage(lead, refused, int(e.Runs), e.Max, raise, leaves)
}

// runBudgetBlockedMessage is the system comment for a budget block.
func runBudgetBlockedMessage(refused string, runs, max int, raise string, leaves int) string {
	return runBudgetNoticeMessage("Blocked", refused, runs, max, raise, leaves)
}

// runBudgetNoticeMessage words a budget notice. Without a raise command (the
// platform maximum leaves no valid raise) it names the maximum and the
// recovery a person has instead.
func runBudgetNoticeMessage(lead, refused string, runs, max int, raise string, leaves int) string {
	if raise == "" {
		return fmt.Sprintf(
			"%s: %s was refused because the issue used %d of %d runs. The platform maximum of %d runs is reached, so no raise is possible. A person moves the issue's status to open a new run window.",
			lead, refused, runs, max, issueworkflow.MaxRunsPerIssueLimit)
	}
	return fmt.Sprintf(
		"%s: %s was refused because the issue used %d of %d runs. To allow more runs, a person runs `%s`, which leaves %d runs.",
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

// issueBudgetRef names an issue in a budget command: its identifier, else its id.
// It takes the prefix so a caller inside a transaction needs no second pool
// connection to look it up.
func issueBudgetRef(prefix string, issue db.Issue) string {
	if prefix != "" {
		return fmt.Sprintf("%s-%d", prefix, issue.Number)
	}
	return uuidToString(issue.ID)
}

// budgetRaise is a suggested raise: the command and the runs it leaves.
type budgetRaise struct {
	command string
	leaves  int
}

// runBudgetRaiseCommand is the command that lifts this issue's cap to leave
// 10 runs after the balance is paid off, and the runs it leaves, naming the
// issue by ref (see issueBudgetRef). It reads only through q, so it can run
// inside the notice's transaction without a second pool connection. It prices the raise
// from the ledger as q reads it now, not from the balance the refusal saw. The
// refusal's own balance is the fallback when the ledger cannot be read. The
// command is empty when the platform maximum leaves no valid raise.
func runBudgetRaiseCommand(ctx context.Context, q *db.Queries, issueID pgtype.UUID, ref string, e service.RunBudgetExhausted) budgetRaise {
	max, balance, err := service.CurrentRunBudget(ctx, q, issueID)
	if err != nil {
		max, balance = e.Max, e.Balance
	}
	target, leaves := service.SuggestedRaiseForBalance(max, balance)
	if target == 0 {
		return budgetRaise{}
	}
	return budgetRaise{command: fmt.Sprintf("multica issue budget raise %s --runs %d", ref, target), leaves: leaves}
}
