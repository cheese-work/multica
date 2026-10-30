package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Project workflows on the issue write paths (MUL-7420).
//
// Two rules apply to an issue in a project:
//   - when the project uses a named workflow, its status must be one the
//     workflow lists; a move into such a project keeps an equivalent status
//     instead of failing. Default accepts every status;
//   - entering a column the project hands off, through an ordinary status
//     change of an issue that stays in its project, assigns the issue to that
//     handler and starts its run with the step's instructions. A write that
//     sets the assignee itself keeps its explicit choice. A project move
//     enters the column it lands on only when the caller asks for it.
//
// Handoffs reuse assignment wholesale: the resolved handler goes through the
// same permission gate as a hand-picked assignee, and the run starts through
// the same WillEnqueueRun predicate, with the step's brief as its handoff note.

// workflowHandoff is a handoff an issue write triggers.
type workflowHandoff struct {
	def     issueworkflow.Definition
	step    issueworkflow.Step
	project db.Project
}

// applyIssueWorkflow enforces prev's (possibly new) project workflow on one
// prospective update and may rewrite params: the status of an issue moving
// into the project, and the assignee when the write enters a handoff step.
// enterOnMove makes a project move enter the step the issue lands on.
// caller is who writes: a run on this issue may only move the issue from the
// step it works on, and an agent only along its step's exits. It returns the
// handoff that write triggers, or nil.
func (h *Handler) applyIssueWorkflow(ctx context.Context, q *db.Queries, prev db.Issue, params *db.UpdateIssueParams, statusExplicit, assigneeTouched, enterOnMove bool, caller workflowCaller) (*workflowHandoff, error) {
	run := caller.run
	// A review superseded by a newer head has no say on the status, in any
	// workflow (CHE-2307).
	if oldHead, stale := staleVerdictRun(ctx, q, run); stale && statusExplicit && params.Status.Valid && params.Status.String != prev.Status {
		h.recordReviewActivity(ctx, prev.ID, "agent", uuidToString(run.AgentID), "review_verdict_stale", map[string]string{
			"message": "verdict for " + oldHead + " is stale", "task_id": uuidToString(run.ID), "head_sha": oldHead,
		})
		return nil, &issueworkflow.StaleVerdictError{
			IssueIdentifier: h.getIssuePrefix(ctx, prev.WorkspaceID) + "-" + strconv.Itoa(int(prev.Number)),
			ReviewedSHA:     oldHead,
		}
	}
	if err := h.checkAgentMove(ctx, q, prev, params, statusExplicit, caller); err != nil {
		return nil, err
	}
	def, project, err := issueworkflow.ForIssue(ctx, q, prev.WorkspaceID, prev.WorkflowID, params.ProjectID)
	if err != nil || def == nil {
		return nil, err
	}
	projectMoving := params.ProjectID != prev.ProjectID
	if projectMoving && !statusExplicit && !def.IsDefault && !def.Has(prev.Status) {
		target := issueworkflow.MoveTarget(*def, prev.Status, func(key string) string {
			return issuestatus.Category(ctx, q, prev.WorkspaceID, key)
		})
		params.Status = pgtype.Text{String: target, Valid: true}
	}
	if params.Status.Valid {
		if err := issueworkflow.CheckStatus(def, params.Status.String); err != nil {
			return nil, err
		}
	}
	// Someone else moved the issue while the run worked: the run's decision was
	// made for a step the issue has left, and applying it would undo that move.
	// Default without handoffs keeps the pre-workflow behavior, so workspaces
	// that never adopt workflows see no change.
	if run != nil && run.WorkflowStep.Valid && run.WorkflowStep.String != prev.Status &&
		(!def.IsDefault || def.HandsOffAnywhere()) &&
		statusExplicit && params.Status.Valid && params.Status.String != prev.Status {
		return nil, h.stepMovedError(ctx, prev, run.WorkflowStep.String)
	}
	if assigneeTouched || prev.TriageState.Valid {
		return nil, nil
	}
	entering := ""
	switch {
	case projectMoving && enterOnMove:
		entering = prev.Status
		if params.Status.Valid {
			entering = params.Status.String
		}
	// A project move is administrative unless the caller opted in, and
	// re-selecting the current status is a no-op: neither hands the issue off.
	case !projectMoving && statusExplicit && params.Status.Valid && params.Status.String != prev.Status:
		entering = params.Status.String
	default:
		return nil, nil
	}
	step, ok := def.Step(entering)
	if !ok || !step.HandsOff() {
		return nil, nil
	}
	humans, err := h.workflowHumans(ctx, step, project, prev.WorkspaceID, prev.RequestedByID)
	if err != nil {
		return nil, err
	}
	assigneeType, assigneeID, ok := issueworkflow.ResolveHandler(step, project, prev.CreatorType, prev.CreatorID, humans)
	if !ok {
		return nil, nil
	}
	params.AssigneeType = pgtype.Text{String: assigneeType, Valid: true}
	params.AssigneeID = assigneeID
	return &workflowHandoff{def: *def, step: step, project: project}, nil
}

// checkAgentMove refuses an agent's status write the issue's current step has
// no agent exit for. An agent decides its step's exits, whichever issue its run
// belongs to, so the step comes from the workflow the issue is in now: a write
// that also clears or changes the project must not drop that policy first.
func (h *Handler) checkAgentMove(ctx context.Context, q *db.Queries, prev db.Issue, params *db.UpdateIssueParams, statusExplicit bool, caller workflowCaller) error {
	if !caller.agent || !statusExplicit || !params.Status.Valid || params.Status.String == prev.Status {
		return nil
	}
	def, _, err := issueworkflow.ForIssue(ctx, q, prev.WorkspaceID, prev.WorkflowID, prev.ProjectID)
	if err != nil || def == nil {
		return err
	}
	return issueworkflow.CheckAgentMove(*def, prev.Status, params.Status.String)
}

// createHandoffError is a handoff handler the caller may not use, found while
// a create resolved it under the workflow locks.
type createHandoffError struct {
	status  int
	message string
}

func (e *createHandoffError) Error() string { return e.message }

// createHandoffResolver resolves a step's handler for a new issue and applies
// the same permission gate a hand-picked assignee passes. IssueService.Create
// calls it under the catalog and project locks.
func (h *Handler) createHandoffResolver(r *http.Request, workspaceID, creatorType string, creatorID, requestedByID pgtype.UUID) service.CreateHandoffResolver {
	return func(ctx context.Context, step issueworkflow.Step, project db.Project) (pgtype.Text, pgtype.UUID, error) {
		humans, err := h.workflowHumans(ctx, step, project, project.WorkspaceID, requestedByID)
		if err != nil {
			return pgtype.Text{}, pgtype.UUID{}, err
		}
		assigneeType, assigneeID, ok := issueworkflow.ResolveHandler(step, project, creatorType, creatorID, humans)
		if !ok {
			return pgtype.Text{}, pgtype.UUID{}, nil
		}
		assignee := pgtype.Text{String: assigneeType, Valid: true}
		if status, msg := h.validateAssigneePair(ctx, r, workspaceID, assignee, assigneeID); status != 0 {
			return pgtype.Text{}, pgtype.UUID{}, &createHandoffError{status: status, message: msg}
		}
		return assignee, assigneeID, nil
	}
}

// handoffRunReasonTargetDiffers records that a handoff run started although the
// caller asked for no run: the step's handler is not the caller.
const handoffRunReasonTargetDiffers = "handoff_target_differs"

// suppressRunForWrite is the write's suppress_run after the handoff rule:
// --no-start says the caller's own work is already underway, which holds only
// when the caller is the agent the handoff goes to. A handoff to anyone else
// keeps its run, or nobody starts it. The reason is set when the flag was
// overridden.
func (h *Handler) suppressRunForWrite(ctx context.Context, actorType, actorID string, prev db.Issue, params db.UpdateIssueParams, handoff *workflowHandoff, suppressRun bool) (suppress bool, reason string) {
	if !suppressRun || handoff == nil {
		return suppressRun, ""
	}
	if actorType == "agent" && h.handedToAgent(ctx, prev, params, true) == actorID {
		return true, ""
	}
	return false, handoffRunReasonTargetDiffers
}

// workflowHandoffPayload describes a handoff on the issue:updated event, so
// the activity log can record the status change and the handoff as one
// timeline entry.
func workflowHandoffPayload(issue db.Issue, hand *workflowHandoff, note string, runStarted bool, runReason string) map[string]string {
	out := map[string]string{
		"workflow": hand.def.Name,
		"to_type":  issue.AssigneeType.String,
		"to_id":    uuidToString(issue.AssigneeID),
		"run":      strconv.FormatBool(runStarted),
	}
	if runStarted && note != "" {
		out["note"] = note
	}
	if runStarted && runReason != "" {
		out["run_reason"] = runReason
	}
	return out
}

// writeIssueWorkflowError answers a workflow rejection. It reports false when
// err is not one, so the caller can fall through to its generic 500.
func writeIssueWorkflowError(w http.ResponseWriter, err error) bool {
	if errors.Is(err, issueworkflow.ErrOverrideMissing) {
		writeError(w, http.StatusBadRequest, "workflow not found in this workspace")
		return true
	}
	var notIn *issueworkflow.NotInWorkflowError
	if errors.As(err, &notIn) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":            notIn.Error(),
			"code":             "status_not_in_workflow",
			"workflow_name":    notIn.WorkflowName,
			"allowed_statuses": notIn.Allowed,
		})
		return true
	}
	var notAllowed *issueworkflow.MoveNotAllowedError
	if errors.As(err, &notAllowed) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":         notAllowed.Error(),
			"code":          "status_move_not_allowed",
			"from":          notAllowed.From,
			"allowed_moves": notAllowed.Allowed,
		})
		return true
	}
	if errors.Is(err, errHandoffPreviewStale) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": err.Error() + "; review the move again",
			"code":  "handoff_preview_stale",
		})
		return true
	}
	if errors.Is(err, errWorkflowChanged) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": err.Error() + "; retry",
			"code":  "workflow_changed",
		})
		return true
	}
	var moved *issueworkflow.StepMovedError
	if errors.As(err, &moved) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":          moved.Error(),
			"code":           "workflow_step_moved",
			"step":           moved.Step,
			"current_status": moved.Current,
		})
		return true
	}
	var stale *issueworkflow.StaleVerdictError
	if errors.As(err, &stale) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":        stale.Error(),
			"code":         "review_verdict_stale",
			"reviewed_sha": stale.ReviewedSHA,
		})
		return true
	}
	return false
}

// workflowCaller is who makes a status write: run is the agent's run on this
// issue, nil for members and for runs on other issues; agent is true for any
// agent, whatever run it carries.
type workflowCaller struct {
	run   *db.AgentTaskQueue
	agent bool
}

// agentRunOnIssue returns the calling agent's run when it works on this issue,
// nil for members and for runs on other issues. The task id is the
// server-trusted X-Task-ID resolveActor vouched for.
func (h *Handler) agentRunOnIssue(r *http.Request, actorType string, issue db.Issue) *db.AgentTaskQueue {
	if actorType != "agent" {
		return nil
	}
	taskID, err := util.ParseUUID(r.Header.Get("X-Task-ID"))
	if err != nil {
		return nil
	}
	task, err := h.Queries.GetAgentTask(r.Context(), taskID)
	if err != nil || task.IssueID != issue.ID {
		return nil
	}
	return &task
}

// handedToAgent names the agent a write hands the issue to: its new agent
// assignee, the leader of its new squad assignee, or a workflow step's
// handler even when that handler already owned the issue. Empty when the
// write leaves the issue with whoever had it.
func (h *Handler) handedToAgent(ctx context.Context, prev db.Issue, params db.UpdateIssueParams, handoff bool) string {
	if !params.AssigneeID.Valid {
		return ""
	}
	if !handoff && params.AssigneeType == prev.AssigneeType && params.AssigneeID == prev.AssigneeID {
		return ""
	}
	switch params.AssigneeType.String {
	case "agent":
		return uuidToString(params.AssigneeID)
	case "squad":
		squad, err := h.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: params.AssigneeID, WorkspaceID: prev.WorkspaceID})
		if err == nil && squad.LeaderID.Valid {
			return uuidToString(squad.LeaderID)
		}
	}
	return ""
}

// handedToAgentIfRun is handedToAgent, kept only when the write will start the
// agent's run. Naming the agent makes the write's issue events skip its event
// wakeups, which is right only when the run delivers the change instead: a
// backlog assignment, a suppressed run or an agent that cannot run would
// otherwise reach the agent through neither. It decides before the write, on
// the issue as the write will leave it.
func (h *Handler) handedToAgentIfRun(r *http.Request, actorType, actorID string, prev db.Issue, params db.UpdateIssueParams, assigneeSupplied, handoff, suppressRun bool) string {
	agent := h.handedToAgent(r.Context(), prev, params, handoff)
	if agent == "" || suppressRun {
		return ""
	}
	next := prev
	next.AssigneeType, next.AssigneeID = params.AssigneeType, params.AssigneeID
	if params.Status.Valid {
		next.Status = params.Status.String
	}
	assigneeChanged := (assigneeSupplied || handoff) &&
		(prev.AssigneeType.String != next.AssigneeType.String || uuidToString(prev.AssigneeID) != uuidToString(next.AssigneeID))
	_, willRun := h.IssueService.WillEnqueueRun(r.Context(),
		service.IssueTriggerInput{
			Issue:              next,
			PrevStatus:         prev.Status,
			AssigneeChanged:    assigneeChanged || handoff,
			StatusChanged:      prev.Status != next.Status,
			EnteredHandoffStep: handoff,
		},
		h.issueTriggerWriteProbe(r, actorType, actorID, next),
	)
	if !willRun {
		return ""
	}
	return agent
}

// advanceRunStep moves a run's workflow step along with a status change the
// run made itself, so its next change is judged from where it put the issue.
func (h *Handler) advanceRunStep(ctx context.Context, run *db.AgentTaskQueue, prev, issue db.Issue) {
	if run == nil || !run.WorkflowStep.Valid || prev.Status == issue.Status {
		return
	}
	if err := h.Queries.SetTaskWorkflowStep(ctx, db.SetTaskWorkflowStepParams{
		ID:           run.ID,
		WorkflowStep: pgtype.Text{String: issue.Status, Valid: true},
	}); err != nil {
		slog.Warn("workflow: advance run step failed", "task_id", uuidToString(run.ID), "error", err)
	}
}

// stepMovedError explains a refused status change: where the run's step was,
// where the issue is now, and who moved it.
func (h *Handler) stepMovedError(ctx context.Context, issue db.Issue, step string) error {
	resolver := issuestatus.NewResolver(issue.WorkspaceID)
	out := &issueworkflow.StepMovedError{
		IssueIdentifier: h.getIssuePrefix(ctx, issue.WorkspaceID) + "-" + strconv.Itoa(int(issue.Number)),
		Step:            step,
		StepName:        resolver.Name(ctx, h.Queries, step),
		Current:         issue.Status,
		CurrentName:     resolver.Name(ctx, h.Queries, issue.Status),
	}
	if last, err := h.Queries.GetLastIssueStatusChange(ctx, issue.ID); err == nil && last.ActorID.Valid {
		out.MovedBy = h.actorDisplayName(ctx, issue.WorkspaceID, last.ActorType.String, last.ActorID)
	}
	return out
}

// workflowHandoffBrief renders the brief a handoff's agent or squad will
// read, for the timeline and the handoff confirmation. Members run nothing,
// so they get none. The run itself reads the brief its claim renders.
func (h *Handler) workflowHandoffBrief(ctx context.Context, issue db.Issue, hand *workflowHandoff) string {
	if hand == nil || !issue.AssigneeType.Valid {
		return ""
	}
	if t := issue.AssigneeType.String; t != "agent" && t != "squad" {
		return ""
	}
	in := h.workflowBriefInput(ctx, issue, hand.def, hand.project)
	in.StatusKey = hand.step.StatusKey
	return issueworkflow.Brief(in)
}

// workflowBriefInput describes an issue at its current status under its
// project's workflow, for the assignee's brief.
func (h *Handler) workflowBriefInput(ctx context.Context, issue db.Issue, def issueworkflow.Definition, project db.Project) issueworkflow.BriefInput {
	resolver := issuestatus.NewResolver(issue.WorkspaceID)
	return issueworkflow.BriefInput{
		Workflow:        def,
		ProjectName:     project.Title,
		StatusKey:       issue.Status,
		IssueIdentifier: h.getIssuePrefix(ctx, issue.WorkspaceID) + "-" + strconv.Itoa(int(issue.Number)),
		StatusName: func(key string) string {
			return resolver.Name(ctx, h.Queries, key)
		},
		HandlerName: func(step issueworkflow.Step) string {
			if !step.HandsOff() {
				return ""
			}
			return h.stepHandlerName(ctx, issue, project, step)
		},
	}
}

// actorDisplayName names an agent, squad or member for a brief. It falls back
// to the actor type so a lookup failure never blocks a handoff.
func (h *Handler) actorDisplayName(ctx context.Context, wsUUID pgtype.UUID, actorType string, id pgtype.UUID) string {
	switch actorType {
	case "agent":
		if agent, err := h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: id, WorkspaceID: wsUUID}); err == nil {
			return agent.Name
		}
	case "squad":
		if squad, err := h.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: id, WorkspaceID: wsUUID}); err == nil {
			return squad.Name
		}
	case "member":
		if user, err := h.Queries.GetUser(ctx, id); err == nil {
			return user.Name
		}
	}
	return actorType
}

// stopPreviousAssigneeRuns cancels the active runs of whoever a handoff took
// the issue from — an agent's runs, or every run made on a squad's behalf —
// when the caller asked for it. A handoff back to the same handler keeps its
// run, and the agent making the handoff keeps its own: that run is the one
// making this write.
//
// It cancels the runs the write found under the issue lock, not the ones it
// finds now: a run the previous assignee got after the commit is work the
// person never saw. Each cancellation is also conditional on the issue not
// belonging to the previous assignee again, so a request that handed the issue
// back in between keeps its run.
func (h *Handler) stopPreviousAssigneeRuns(ctx context.Context, w *workflowWrite, issue db.Issue, actorType, actorID string) {
	if w == nil || len(w.stopRuns) == 0 {
		return
	}
	actor := service.TaskCancellationActor{Type: actorType}
	if id, err := parseUUIDString(actorID); err == nil {
		actor.ID = id
		actor.Name = h.actorDisplayName(ctx, issue.WorkspaceID, actorType, id)
	}
	for _, task := range w.stopRuns {
		if actorType == "agent" && uuidToString(task.AgentID) == actorID {
			continue
		}
		if _, err := h.TaskService.CancelTaskWithResult(ctx, task.ID, service.CancelTaskOptions{
			ClientSupportsDraftRestore: true,
			CancelledBy:                actor,
			UserInitiated:              true,
			UnlessAssignee:             w.stopFrom,
		}); err != nil {
			slog.Warn("workflow: cancel previous run failed",
				"issue_id", uuidToString(issue.ID), "task_id", uuidToString(task.ID), "error", err)
		}
	}
}

// assigneeActiveRuns lists the active runs of an issue's assignee on it: the
// agent's own runs, or every run made on a squad's behalf. Members run
// nothing.
func (h *Handler) assigneeActiveRuns(ctx context.Context, q *db.Queries, issue db.Issue) []db.AgentTaskQueue {
	if !issue.AssigneeID.Valid {
		return nil
	}
	assigneeType := issue.AssigneeType.String
	if assigneeType != "agent" && assigneeType != "squad" {
		return nil
	}
	tasks, err := q.ListActiveTasksByIssue(ctx, issue.ID)
	if err != nil {
		slog.Warn("workflow: list active tasks failed", "issue_id", uuidToString(issue.ID), "error", err)
		return nil
	}
	var out []db.AgentTaskQueue
	for _, task := range tasks {
		if (assigneeType == "agent" && task.AgentID == issue.AssigneeID) ||
			(assigneeType == "squad" && task.SquadID == issue.AssigneeID) {
			out = append(out, task)
		}
	}
	return out
}

// claimProjectWorkflow renders the issue's project workflow for a task claim,
// or nil when there is nothing to say beyond the runtime's own status rules:
// no project, or Default with no handoffs and no run budget. The issue's assignee gets the
// step's instructions and commands; any other agent working on the issue
// gets the observer brief, so it does not move a step it does not own.
func (h *Handler) claimProjectWorkflow(ctx context.Context, issue db.Issue, task *db.AgentTaskQueue) *TaskProjectWorkflowData {
	def, project, err := issueworkflow.ForIssue(ctx, h.Queries, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
	if err != nil {
		slog.Warn("task claim: failed to load project workflow for brief injection",
			"issue_id", uuidToString(issue.ID), "error", err)
		return nil
	}
	if def == nil {
		return nil
	}
	if def.IsDefault && !def.HandsOffAnywhere() {
		// Nothing to say about the workflow, but a budgeted Default still
		// refuses runs at admission, so the run is told its budget.
		if line := h.runBudgetLine(ctx, issue, def); line != "" {
			return &TaskProjectWorkflowData{Brief: line}
		}
		return nil
	}
	in := h.workflowBriefInput(ctx, issue, *def, project)
	if !taskRunsForAssignee(issue, task) {
		in.Observer = true
		if issue.AssigneeType.Valid && issue.AssigneeID.Valid {
			in.AssigneeName = h.actorDisplayName(ctx, issue.WorkspaceID, issue.AssigneeType.String, issue.AssigneeID)
		}
	}
	return &TaskProjectWorkflowData{Brief: issueworkflow.Brief(in) + h.runBudgetLine(ctx, issue, def)}
}

// runBudgetLine tells the run how much of the issue's run budget is left
// (CHE-2297 L1b): runs left, tokens used, and runs that reported no usage.
// Tokens are input plus output; the server cannot see tokens during a run, so
// the figure lags by the runs still in flight. It is empty for a workflow with
// no budget start, matching guardRunBudget.
func (h *Handler) runBudgetLine(ctx context.Context, issue db.Issue, def *issueworkflow.Definition) string {
	if def.RunBudgetSince.IsZero() {
		return ""
	}
	since, err := service.RunBudgetStart(ctx, h.Queries, issue.ID, def)
	if err != nil {
		slog.Warn("task claim: run budget window failed", "issue_id", uuidToString(issue.ID), "error", err)
		return ""
	}
	runs, err := h.Queries.CountIssueRunsSince(ctx, db.CountIssueRunsSinceParams{IssueID: issue.ID, Since: pgtype.Timestamptz{Time: since, Valid: true}})
	if err != nil {
		slog.Warn("task claim: run budget count failed", "issue_id", uuidToString(issue.ID), "error", err)
		return ""
	}
	usage, err := h.Queries.GetIssueUsageSummary(ctx, issue.ID)
	if err != nil {
		slog.Warn("task claim: run budget usage failed", "issue_id", uuidToString(issue.ID), "error", err)
		return ""
	}
	limit := def.EffectiveMaxRuns()
	unit := "runs"
	if usage.UnreportedTaskCount == 1 {
		unit = "run"
	}
	return fmt.Sprintf("Run budget: %d runs left of %d; %d tokens used (%d %s unreported).\n",
		max(0, limit-int(runs)), limit, usage.TotalInputTokens+usage.TotalOutputTokens, usage.UnreportedTaskCount, unit)
}

// taskRunsForAssignee reports whether a run works on the issue as its
// assignee: the assigned agent's own run, or a run made for the assigned
// squad.
func taskRunsForAssignee(issue db.Issue, task *db.AgentTaskQueue) bool {
	if task == nil || !issue.AssigneeID.Valid {
		return false
	}
	switch issue.AssigneeType.String {
	case "agent":
		return task.AgentID == issue.AssigneeID
	case "squad":
		return task.SquadID == issue.AssigneeID
	}
	return false
}

// workflowHumans gathers the people a "human by role" step can resolve to:
// the issue's requester and the workspace owner. A requester or member project
// lead who has left the workspace is absent: the chain moves on, since the
// stored ids outlive the member row. Other handlers need none of this, so they
// skip the lookups. A workspace with no owner is not an error; a failed lookup
// is, so the write fails instead of leaving the issue unassigned.
func (h *Handler) workflowHumans(ctx context.Context, step issueworkflow.Step, project db.Project, workspaceID, requestedByID pgtype.UUID) (issueworkflow.Humans, error) {
	if step.Handler.Type != issueworkflow.HandlerHumanByRole {
		return issueworkflow.Humans{}, nil
	}
	var humans issueworkflow.Humans
	if requestedByID.Valid {
		member, err := h.isWorkspaceMember(ctx, uuidToString(requestedByID), uuidToString(workspaceID))
		if err != nil {
			return issueworkflow.Humans{}, fmt.Errorf("look up requester membership: %w", err)
		}
		if member {
			humans.Requester = requestedByID
			return humans, nil
		}
	}
	if project.LeadType.String == "member" && project.LeadID.Valid {
		member, err := h.isWorkspaceMember(ctx, uuidToString(project.LeadID), uuidToString(workspaceID))
		if err != nil {
			return issueworkflow.Humans{}, fmt.Errorf("look up project lead membership: %w", err)
		}
		if member {
			return humans, nil
		}
		humans.LeadNotMember = true
	}
	owner, err := h.Queries.GetWorkspaceOwnerUserID(ctx, workspaceID)
	switch {
	case err == nil:
		humans.WorkspaceOwner = owner
	case !errors.Is(err, pgx.ErrNoRows):
		return issueworkflow.Humans{}, fmt.Errorf("look up workspace owner: %w", err)
	}
	return humans, nil
}

// stepHandlerName describes who a step hands the issue to, for briefs.
func (h *Handler) stepHandlerName(ctx context.Context, issue db.Issue, project db.Project, step issueworkflow.Step) string {
	// A brief only names the handler; on a lookup failure it names the fallbacks
	// that need no lookup. The write that matters resolves it again and fails.
	humans, err := h.workflowHumans(ctx, step, project, issue.WorkspaceID, issue.RequestedByID)
	if err != nil {
		slog.Warn("workflow brief: resolve human handler", "issue_id", uuidToString(issue.ID), "error", err)
	}
	t, id, ok := issueworkflow.ResolveHandler(step, project, issue.CreatorType, issue.CreatorID, humans)
	if !ok {
		switch step.Handler.Type {
		case issueworkflow.HandlerProjectLead:
			return "the project lead (none set)"
		case issueworkflow.HandlerCreator:
			return "the issue creator"
		case issueworkflow.HandlerHumanByRole:
			return "a person (none found)"
		}
		return ""
	}
	name := h.actorDisplayName(ctx, issue.WorkspaceID, t, id)
	switch step.Handler.Type {
	case issueworkflow.HandlerProjectLead:
		return name + " (project lead)"
	case issueworkflow.HandlerCreator:
		return name + " (issue creator)"
	case issueworkflow.HandlerHumanByRole:
		return name + " (requester, project lead or workspace owner)"
	}
	return name
}
