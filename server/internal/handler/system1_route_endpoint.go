package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/dispatch"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-4551 (S3 of CHE-4439): POST /api/issues/{id}/route moves an issue's
// status, records why, and writes the System 1 route label in one transaction.
// Status only; an explicit owner is S4.

const routeReasonMaxChars = 500

type routeIssueRequest struct {
	Status           string   `json:"status"`
	Reason           string   `json:"reason"`
	Confidence       *float64 `json:"confidence"`
	ExpectedRevision *int64   `json:"expected_revision"`
	SuppressRun      bool     `json:"suppress_run"`
}

type routeRunOutcome struct {
	Started bool   `json:"started"`
	Refused string `json:"refused,omitempty"`
}

// routeRefusal names the enqueue errors that leave the move and its label
// committed with no run. Any other enqueue error rolls the call back.
// errAgentNotInvocable is why an assigned agent got no run: the caller may not
// invoke it.
var errAgentNotInvocable = errors.New("agent run not admitted")

func routeRefusal(err error) (string, bool) {
	switch {
	case errors.Is(err, service.ErrDuplicatePendingTask):
		return "duplicate_pending_task", true
	case errors.Is(err, service.ErrRunBudgetExhausted):
		return "run_budget_exhausted", true
	case errors.Is(err, service.ErrStageQueueFull):
		return "stage_queue_full", true
	case errors.Is(err, service.ErrIssueInTriage):
		return "issue_in_triage", true
	case errors.Is(err, service.ErrAgentPaused):
		return "agent_paused", true
	case errors.Is(err, service.ErrRecoveryCaused):
		return "recovery_caused", true
	case errors.Is(err, errSquadLeaderNotAdmitted):
		return "squad_leader_not_admitted", true
	case errors.Is(err, errAgentNotInvocable):
		return "invocation_not_allowed", true
	}
	return "", false
}

func (h *Handler) RouteIssue(w http.ResponseWriter, r *http.Request) {
	r = h.withWakeupActor(r)
	prev, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	var req routeIssueRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if n := utf8.RuneCountInString(req.Reason); req.Reason == "" || n > routeReasonMaxChars {
		writeError(w, http.StatusBadRequest, "reason must be 1 to 500 characters")
		return
	}
	if req.Confidence == nil || !(*req.Confidence >= 0 && *req.Confidence <= 1) {
		writeError(w, http.StatusBadRequest, "confidence is required and must be between 0 and 1")
		return
	}
	if req.Status == "" {
		writeError(w, http.StatusBadRequest, "status is required")
		return
	}
	statusKey, _, ok := h.resolveIssueStatusKeyKind(w, r, prev.WorkspaceID, req.Status)
	if !ok {
		return
	}
	params := db.UpdateIssueParams{
		SourceTaskID:  h.wakeupSourceTaskID(r),
		ID:            prev.ID,
		Status:        pgtype.Text{String: statusKey, Valid: true},
		AssigneeType:  prev.AssigneeType,
		AssigneeID:    prev.AssigneeID,
		StartDate:     prev.StartDate,
		DueDate:       prev.DueDate,
		ParentIssueID: prev.ParentIssueID,
		ProjectID:     prev.ProjectID,
		Stage:         prev.Stage,
	}
	if req.ExpectedRevision != nil {
		if *req.ExpectedRevision < 1 {
			writeError(w, http.StatusBadRequest, "expected_revision must be a positive integer")
			return
		}
		if prev.Revision != *req.ExpectedRevision {
			writeRevisionConflict(w, "issue", prev.ID, *req.ExpectedRevision, prev.Revision)
			return
		}
		params.ExpectedRevision = pgtype.Int8{Int64: *req.ExpectedRevision, Valid: true}
	}
	if prev.TriageState.Valid {
		writeIssueInTriage(w, "status")
		return
	}

	workspaceID := uuidToString(prev.WorkspaceID)
	actorType, actorID := h.resolveActor(r, requestUserID(r), workspaceID)
	callerRun := h.agentRunOnIssue(r, actorType, prev)
	caller := workflowCaller{run: callerRun, agent: actorType == "agent"}
	handoff, workflowWrite, err := h.decideIssueWorkflow(r.Context(), prev, &params, true, false, false, caller)
	if err != nil {
		if writeIssueWorkflowError(w, err) {
			return
		}
		slog.Error("route issue: resolve project workflow", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to resolve project workflow")
		return
	}
	// A project move can remap the status; the write below is what happened.
	statusKey = params.Status.String
	if statusKey == prev.Status {
		writeErrorCode(w, http.StatusConflict, "status_unchanged", "the issue is already in "+statusKey)
		return
	}
	if workflowWrite != nil {
		workflowWrite.entry = entryWrite{actorType: actorType, actorID: actorID, human: actorType == "member" && !isMachineCredentialActor(r)}
	}
	if handoff != nil {
		if status, msg := h.validateAssigneePair(r.Context(), r, workspaceID, params.AssigneeType, params.AssigneeID); status != 0 {
			writeError(w, status, msg)
			return
		}
	}

	suppress := req.SuppressRun
	writeCtx, handed := withWakeupHandedTo(h.actingTaskContext(r), h.handedToAgentIfRun(r, actorType, actorID, prev, params, false, handoff != nil, suppress))
	writeCtx = withBudgetActor(writeCtx, r)
	if handoff != nil {
		if writeCtx, ok = h.withIssueWriteTaint(w, r, writeCtx); !ok {
			return
		}
	}
	release, ok := h.beginRunBudgetMove(w, r, actorType, prev, statusKey)
	if !ok {
		return
	}
	defer release()

	trigger := statusTriggerFromRequest(r, actorType, actorID)
	trigger.Version = service.StatusTriggerVersion
	rec := &statusRecord{workspaceID: prev.WorkspaceID, trigger: trigger}
	if handoff != nil {
		rec.handoffWorkflow = handoff.def.Name
	}
	hook := &routeCapture{Via: "route", DecisionSource: "human", Caller: caller, Confidence: pgtype.Float4{Float32: float32(*req.Confidence), Valid: true}}
	if callerRun != nil {
		hook.RunStep = callerRun.WorkflowStep.String
	}
	if actorType == "agent" {
		hook.DecisionSource = "agent"
	}
	writeCtx = withRouteCapture(withStatusRecord(writeCtx, rec), hook)
	if actorType == "member" {
		writeCtx = withMemberRouteOverride(writeCtx)
	}

	next := prev
	next.AssigneeType, next.AssigneeID = params.AssigneeType, params.AssigneeID
	var overlay *service.PreparedRuntimeMCPOverlay
	if !suppress && handoff != nil && params.AssigneeType.String == "agent" {
		next.Status = statusKey
		overlay = h.TaskService.PrepareIssueRunOverlay(writeCtx, next, memberActorUserID(actorType, actorID))
	}

	var (
		updated          db.Issue
		cancelledWakeups []db.AgentTaskQueue
		outcome          routeRunOutcome
		observationID    pgtype.UUID
		eventID          pgtype.UUID
		runAfter         func()
		cancelAfter      func()
		duplicateRun     bool
		runTrigger       service.IssueRunTrigger
	)
	err = h.runWithIssueStatusGuard(writeCtx, prev.WorkspaceID, statusKey, true, func(q *db.Queries) error {
		th := h.onQueries(q)
		var err error
		if updated, cancelledWakeups, err = h.writeIssueLocked(writeCtx, q, prev.WorkspaceID, params, workflowWrite); err != nil {
			return err
		}
		// The record spends its pre-assigned id; the status_changed row carries it.
		if rec.activity == nil {
			return errors.New("route: the status write recorded no change")
		}
		eventID = rec.activity.ID
		if actorType != "member" {
			if err := service.GrantRunBudgetStep(writeCtx, q, updated, prev.Status, dispatch.BudgetRecoveryCaller(writeCtx)); err != nil {
				return err
			}
		}
		assigneeChanged := prev.AssigneeType.String != updated.AssigneeType.String || uuidToString(prev.AssigneeID) != uuidToString(updated.AssigneeID)
		var willRun bool
		runTrigger, willRun = th.IssueService.WillEnqueueRun(writeCtx, service.IssueTriggerInput{
			Issue: updated, PrevStatus: prev.Status, AssigneeChanged: assigneeChanged || handoff != nil, StatusChanged: true, EnteredHandoffStep: handoff != nil,
		}, h.issueTriggerWriteProbe(r, actorType, actorID, updated))
		if willRun && !suppress {
			var notConfirmed *service.StopNotConfirmedError
			if err := service.StopGate(writeCtx, q, updated.ID); errors.As(err, &notConfirmed) {
				return err
			}
			if cancelAfter, err = th.TaskService.CancelStaleStepRunsInTx(writeCtx, q, updated, runTrigger.AgentID); err != nil {
				return err
			}
			if runTrigger.AssigneeType == "agent" && !th.canInvokeAssignedAgent(writeCtx, r, updated, actorType, actorID) {
				err = errAgentNotInvocable
			} else if runTrigger.AssigneeType == "agent" {
				_, runAfter, err = th.TaskService.EnqueueIssueRunInTx(writeCtx, q, updated, pgtype.UUID{}, memberActorUserID(actorType, actorID), overlay)
			} else {
				_, runAfter, err = th.enqueueSquadLeaderRunAsInTx(writeCtx, q, updated, pgtype.UUID{}, actorType, actorID)
			}
			duplicateRun = errors.Is(err, service.ErrDuplicatePendingTask)
			if refused, isRefusal := routeRefusal(err); isRefusal {
				outcome.Refused = refused
			} else if err != nil {
				return err
			}
		}
		details, err := json.Marshal(map[string]any{
			"from_status": prev.Status, "to_status": updated.Status,
			"reason": req.Reason, "confidence": *req.Confidence,
			"event_id": uuidToString(eventID),
		})
		if err != nil {
			return err
		}
		if _, err := q.CreateActivity(writeCtx, db.CreateActivityParams{
			ID: dbid.NewV7(), WorkspaceID: updated.WorkspaceID, IssueID: updated.ID,
			ActorType: strToText(actorType), ActorID: parseUUID(actorID), Action: "routed", Details: details,
		}); err != nil {
			return err
		}
		observationID, err = q.GetLatestSystem1RouteObservation(writeCtx, updated.ID)
		return err
	})
	if err != nil {
		var notConfirmed *service.StopNotConfirmedError
		switch {
		case writeAgentIssueStatusError(w, err) || writeIssueStatusRaceError(w, err) || writeIssueWorkflowError(w, err) || writeBudgetRebindingError(w, err):
		case errors.As(err, &notConfirmed):
			writeErrorCode(w, http.StatusConflict, "stop_not_confirmed", notConfirmed.Error())
		case errors.Is(err, pgx.ErrNoRows) && req.ExpectedRevision != nil:
			h.writeRouteRevisionConflict(w, r, prev, *req.ExpectedRevision)
		default:
			slog.Warn("route issue failed", append(logger.RequestAttrs(r), "error", err, "issue_id", uuidToString(prev.ID))...)
			writeError(w, http.StatusInternalServerError, "failed to route issue")
		}
		return
	}

	h.advanceRunStep(r.Context(), callerRun, prev, updated)
	h.broadcastCancelledWakeups(r.Context(), prev.WorkspaceID, cancelledWakeups)
	var handoffDetails map[string]string
	if handoff != nil {
		handoffDetails = workflowHandoffPayload(updated, handoff, h.workflowHandoffBrief(r.Context(), updated, handoff), runAfter != nil, "")
		h.finishRecordedHandoff(r.Context(), rec, handoffDetails)
	}
	payload := map[string]any{
		"issue":            h.issueResponseWithCategory(r.Context(), updated),
		"assignee_changed": prev.AssigneeType.String != updated.AssigneeType.String || uuidToString(prev.AssigneeID) != uuidToString(updated.AssigneeID),
		"status_changed":   true,
		"prev_status":      prev.Status,
		"creator_type":     prev.CreatorType,
		"creator_id":       uuidToString(prev.CreatorID),
	}
	if handoffDetails != nil {
		payload["workflow_handoff"] = handoffDetails
	}
	if rec.activity != nil {
		payload["activity_id"] = uuidToString(rec.activity.ID)
	}
	h.publish(protocol.EventIssueUpdated, workspaceID, actorType, actorID, payload)
	h.publishRecordedStatusActivity(rec, actorType, actorID)
	if cancelAfter != nil {
		cancelAfter()
	}
	switch {
	case runAfter != nil:
		runAfter()
		outcome.Started = true
	case duplicateRun:
		outcome.Started = h.queuedRunCarriesWrite(r.Context(), updated, runTrigger.AgentID)
	}
	if outcome.Started {
		h.consumeHandedWakeups(r.Context(), updated.ID, handed)
	}
	if actorType == "member" {
		h.openRunBudgetWindowForMove(r.Context(), updated)
	}
	release()
	if updated.ParentIssueID.Valid || prev.ParentIssueID.Valid {
		h.processChildEvents(r.Context(), updated.ParentIssueID, prev.ParentIssueID)
	}

	resp := issueToResponse(updated, h.getIssuePrefix(r.Context(), updated.WorkspaceID))
	h.fillStatusCategory(r.Context(), updated.WorkspaceID, &resp)
	h.newReadFilter(r).stubDescription(h, updated, &resp)
	writeJSON(w, http.StatusOK, map[string]any{
		"issue":          resp,
		"observation_id": util.UUIDToString(observationID),
		"event_id":       util.UUIDToString(eventID),
		"run":            outcome,
	})
}

func (h *Handler) writeRouteRevisionConflict(w http.ResponseWriter, r *http.Request, prev db.Issue, expected int64) {
	current, err := h.Queries.GetIssueInWorkspace(context.WithoutCancel(r.Context()), db.GetIssueInWorkspaceParams{ID: prev.ID, WorkspaceID: prev.WorkspaceID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to route issue")
		return
	}
	writeRevisionConflict(w, "issue", current.ID, expected, current.Revision)
}

// canInvokeAssignedAgent is the invoke gate for a run the move starts on the
// issue's agent assignee. A handoff validated the pair before the write; a move
// that starts a run without one (promotion out of backlog or blocked) did not,
// so the gate is judged here for every run, on the caller and its originator.
func (h *Handler) canInvokeAssignedAgent(ctx context.Context, r *http.Request, issue db.Issue, actorType, actorID string) bool {
	agent, err := h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: issue.AssigneeID, WorkspaceID: issue.WorkspaceID})
	if err != nil {
		return false
	}
	return h.canInvokeAgent(ctx, agent, actorType, actorID, h.invokeOriginatorFromRequest(r, actorType, actorID), uuidToString(issue.WorkspaceID))
}
