package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// maxPreviewTriggerIssues caps a single preview request so a pathological
// selection cannot fan out into thousands of readiness probes.
const maxPreviewTriggerIssues = 500

// issueTriggerWriteProbe builds the probe the write paths feed to
// WillEnqueueRun. The private-agent gate is already enforced at the HTTP
// boundary (validateAssigneePair on assign) and inside enqueueSquadLeaderTask
// (canEnqueueSquadLeader), so a write must NOT re-run or sink it — it passes
// allow-all. The self-loop check needs the request's X-Task-ID header.
func (h *Handler) issueTriggerWriteProbe(r *http.Request, actorType, actorID string, issue db.Issue) service.IssueTriggerProbe {
	return service.IssueTriggerProbe{
		CanAccessAgent: nil, // allow-all; gate lives at the write boundary
		CanResumeBlockedIssue: func(agent db.Agent) bool {
			return actorType == "member" && h.canInvokeAgent(r.Context(), agent, actorType, actorID, actorID, uuidToString(issue.WorkspaceID))
		},
		IsSelfLoop: func() bool {
			return h.isAgentRunningOnIssue(r, actorType, issue)
		},
		SuppressActiveSelfAssignment: func(agentID pgtype.UUID) bool {
			suppress := h.shouldSuppressActiveSelfAssignment(r.Context(), actorType, actorID, issue.ID, agentID)
			if suppress {
				slog.Info("suppressing duplicate self-assignment enqueue",
					"issue_id", uuidToString(issue.ID),
					"agent_id", uuidToString(agentID),
				)
			}
			return suppress
		},
	}
}

// issueTriggerPreviewProbe mirrors the real write-time gates for the read-only
// preview: the private-agent gate (so preview never leaks a private agent's
// readiness to a member who cannot see it — matching validateAssigneePair /
// canEnqueueSquadLeader) and the same self-loop guard.
func (h *Handler) issueTriggerPreviewProbe(r *http.Request, actorType, actorID, workspaceID string, issue db.Issue) service.IssueTriggerProbe {
	originatorUserID := h.invokeOriginatorFromRequest(r, actorType, actorID)
	return service.IssueTriggerProbe{
		CanResumeBlockedIssue: func(agent db.Agent) bool {
			return actorType == "member" && h.canInvokeAgent(r.Context(), agent, actorType, actorID, actorID, workspaceID)
		},
		CanAccessAgent: func(agent db.Agent) bool {
			return h.canInvokeAgent(r.Context(), agent, actorType, actorID, originatorUserID, workspaceID)
		},
		IsSelfLoop: func() bool {
			return h.isAgentRunningOnIssue(r, actorType, issue)
		},
		SuppressActiveSelfAssignment: func(agentID pgtype.UUID) bool {
			return h.shouldSuppressActiveSelfAssignment(r.Context(), actorType, actorID, issue.ID, agentID)
		},
	}
}

// shouldSuppressActiveSelfAssignment prevents a trusted task-scoped agent
// actor from creating another run for the target pair merely to claim issue
// ownership. It intentionally checks the TARGET pair, not whether the actor is
// busy anywhere: cross-issue self handoffs are a supported workflow and must
// still enqueue when the target has no active run. Query errors fail closed
// against the external enqueue side effect while leaving the ownership write
// itself intact. The API still returns success for that ownership write; only
// the server log exposes the failed advisory lookup, because enqueue is the
// optional side effect and suppressing it is safer than risking duplicate work.
func (h *Handler) shouldSuppressActiveSelfAssignment(ctx context.Context, actorType, actorID string, issueID, targetAgentID pgtype.UUID) bool {
	if actorType != "agent" || actorID == "" || actorID != uuidToString(targetAgentID) {
		return false
	}
	active, err := h.Queries.HasActiveTaskForIssueAndAgent(ctx, db.HasActiveTaskForIssueAndAgentParams{IssueID: issueID, AgentID: targetAgentID})
	return active || err != nil
}

// dispatchIssueRun executes the enqueue side effect for a decision produced by
// WillEnqueueRun. handoffNote is a legacy API input retained for installed
// clients and travels only with a run that actually starts. The squad path
// still flows through enqueueSquadLeaderTask so the leader access gate and
// pending dedup stay in one place. It reports whether an executable run now
// carries the write: a run it started, or an unstarted one that held the slot.
func (h *Handler) dispatchIssueRun(ctx context.Context, issue db.Issue, trigger service.IssueRunTrigger, actorType, actorID, handoffNote string) (started bool) {
	// Checked here as well as at the queue door so the activity names the actor
	// whose status move or assignment was refused; the door has no actor.
	var notConfirmed *service.StopNotConfirmedError
	if err := service.StopGate(ctx, h.Queries, issue.ID); errors.As(err, &notConfirmed) {
		h.recordStopNotConfirmed(ctx, issue, actorType, actorID, notConfirmed)
		return false
	}
	switch trigger.AssigneeType {
	case "agent":
		// The member who performed this assign/promote is the accountable human
		// for the run (MUL-4302 §4). An agent actor is not a human, so only a
		// member actor is threaded; otherwise attribution falls back to the chain.
		_, err := h.TaskService.EnqueueTaskForIssueWithHandoff(ctx, issue, handoffNote, memberActorUserID(actorType, actorID))
		if errors.Is(err, service.ErrDuplicatePendingTask) {
			// A sibling run holds the slot. It delivers this write only if it
			// is queued and is the assignment-level run for the step the issue
			// is in now; see HasQueuedRunCarryingIssueWrite.
			return h.queuedRunCarriesWrite(ctx, issue, trigger.AgentID)
		}
		// Anything else is a real enqueue failure that was previously
		// discarded silently (CHE-486).
		if err != nil {
			slog.Error("dispatch issue run failed", "issue_id", uuidToString(issue.ID), "agent_id", uuidToString(trigger.AgentID), "error", err)
			return false
		}
		return true
	case "squad":
		// Delivery only when the leader's run was queued. Jev can route the work
		// to a specialist instead: the specialist's run carries the write, not
		// the leader's, so the leader's receipts stay pending.
		return h.enqueueSquadLeaderTaskOutcome(ctx, issue, pgtype.UUID{}, actorType, actorID, handoffNote) == squadEnqueueLeader
	}
	return false
}

// queuedRunCarriesWrite reports whether a queued run of the agent can stand in
// for the run a write lost to the pending-task unique index.
func (h *Handler) queuedRunCarriesWrite(ctx context.Context, issue db.Issue, agentID pgtype.UUID) bool {
	stepHandsOff := false
	if def, _, err := issueworkflow.ForIssue(ctx, h.Queries, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID); err == nil && def != nil {
		step, ok := def.Step(issue.Status)
		stepHandsOff = ok && step.HandsOff()
	}
	carries, err := h.Queries.HasQueuedRunCarryingIssueWrite(ctx, db.HasQueuedRunCarryingIssueWriteParams{
		IssueID:      issue.ID,
		AgentID:      agentID,
		HeadSha:      h.TaskService.ResolveIssueReviewSHAParam(ctx, issue.ID),
		StepHandsOff: stepHandsOff,
		Step:         issue.Status,
	})
	return err == nil && carries
}

// memberActorUserID returns the acting member's user id as a pgtype.UUID when the
// actor is a member, and an invalid UUID otherwise (an agent actor id is not a
// human and must never become an accountable human). Used to thread the
// assign/promote actor into the attribution resolver (MUL-4302 §4).
func memberActorUserID(actorType, actorID string) pgtype.UUID {
	if actorType != "member" {
		return pgtype.UUID{}
	}
	uid, err := util.ParseUUID(actorID)
	if err != nil {
		return pgtype.UUID{}
	}
	return uid
}

// IssueTriggerPreviewRequest asks "if I apply this assignee and/or status to
// these issues (or create one), which runs will start". All fields are
// optional; a nil prospective field means "leave unchanged".
type IssueTriggerPreviewRequest struct {
	// IssueIDs are existing issues to evaluate (single assign, single status,
	// or a batch). Empty with IsCreate=true evaluates a candidate new issue.
	IssueIDs []string `json:"issue_ids"`
	// IsCreate previews a not-yet-persisted issue from AssigneeType/ID/Status.
	IsCreate     bool    `json:"is_create"`
	AssigneeType *string `json:"assignee_type"`
	AssigneeID   *string `json:"assignee_id"`
	Status       *string `json:"status"`
	// ProjectID places a create preview in a project, so its workflow's
	// starting status and starting-step handoff apply. (MUL-7420)
	ProjectID *string `json:"project_id,omitempty"`
}

// IssueTriggerPreviewItem is one issue that WILL start a run under the
// prospective write. AgentID is the runnable agent (squad leader for squads).
type IssueTriggerPreviewItem struct {
	IssueID string `json:"issue_id"`
	AgentID string `json:"agent_id"`
	Source  string `json:"source"`
}

// IssueTriggerPreviewResponse lists every issue that will enqueue plus a total
// the UI can show directly ("将启动 N 个"). Issues that will NOT start a run are
// simply absent, so total_count == len(triggers).
type IssueTriggerPreviewResponse struct {
	Triggers   []IssueTriggerPreviewItem `json:"triggers"`
	TotalCount int                       `json:"total_count"`
}

// PreviewIssueTrigger dry-runs WillEnqueueRun for a prospective issue write and
// returns the runs that would start, without any side effect. It is the single
// authority the four entry points (create / single assign / single status /
// batch) consult so the frontend never re-implements the enqueue rule
// (MUL-3375). Mirrors PreviewCommentTriggers.
func (h *Handler) PreviewIssueTrigger(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := h.resolveWorkspaceID(r)
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace is required")
		return
	}

	var req IssueTriggerPreviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.IssueIDs) > maxPreviewTriggerIssues {
		writeError(w, http.StatusBadRequest, "too many issue_ids")
		return
	}

	// Resolve the prospective assignee once — a malformed id is a deterministic
	// 400, never a silent miscount.
	var (
		newAssigneeType pgtype.Text
		newAssigneeID   pgtype.UUID
		hasNewAssignee  bool
	)
	if req.AssigneeType != nil && *req.AssigneeType != "" && req.AssigneeID != nil && *req.AssigneeID != "" {
		id, parseOK := parseUUIDOrBadRequest(w, *req.AssigneeID, "assignee_id")
		if !parseOK {
			return
		}
		newAssigneeType = pgtype.Text{String: *req.AssigneeType, Valid: true}
		newAssigneeID = id
		hasNewAssignee = true
	}

	actorType, actorID := h.resolveActor(r, userID, workspaceID)
	resp := IssueTriggerPreviewResponse{Triggers: make([]IssueTriggerPreviewItem, 0)}

	appendTrigger := func(issue db.Issue, in service.IssueTriggerInput) {
		probe := h.issueTriggerPreviewProbe(r, actorType, actorID, workspaceID, issue)
		if trigger, ok := h.IssueService.WillEnqueueRun(r.Context(), in, probe); ok {
			resp.Triggers = append(resp.Triggers, IssueTriggerPreviewItem{
				IssueID: uuidToString(trigger.IssueID),
				AgentID: uuidToString(trigger.AgentID),
				Source:  string(trigger.Source),
			})
		}
	}

	if req.IsCreate {
		wsUUID, err := util.ParseUUID(workspaceID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid workspace")
			return
		}
		status := "todo"
		if req.Status != nil && *req.Status != "" {
			status = *req.Status
		}
		candidate := db.Issue{
			WorkspaceID:  wsUUID,
			Status:       status,
			AssigneeType: newAssigneeType,
			AssigneeID:   newAssigneeID,
		}
		// Mirror CreateIssue's workflow handling: the starting column's
		// handoff when no assignee was picked. (MUL-7420)
		if req.ProjectID != nil && *req.ProjectID != "" {
			if projectUUID, err := util.ParseUUID(*req.ProjectID); err == nil {
				if def, project, err := issueworkflow.ForProject(r.Context(), h.Queries, wsUUID, projectUUID); err == nil && def != nil {
					if step, ok := def.Step(candidate.Status); ok && step.HandsOff() && !hasNewAssignee {
						humans, err := h.workflowHumans(r.Context(), step, project, wsUUID, previewRequester(actorType, actorID))
						if err != nil {
							writeError(w, http.StatusInternalServerError, "failed to resolve workflow handler")
							return
						}
						if t, id, ok := issueworkflow.ResolveHandler(step, project, actorType, parseUUID(actorID), humans); ok {
							candidate.AssigneeType = pgtype.Text{String: t, Valid: true}
							candidate.AssigneeID = id
						}
					}
				}
			}
		}
		appendTrigger(candidate, service.IssueTriggerInput{Issue: candidate, IsCreate: true})
		resp.TotalCount = len(resp.Triggers)
		writeJSON(w, http.StatusOK, resp)
		return
	}

	for _, rawID := range req.IssueIDs {
		issueUUID, err := util.ParseUUID(rawID)
		if err != nil {
			continue // malformed id contributes no trigger; deterministic
		}
		loaded, err := h.Queries.GetIssueInWorkspace(r.Context(), db.GetIssueInWorkspaceParams{
			ID:          issueUUID,
			WorkspaceID: parseUUID(workspaceID),
		})
		if err != nil {
			continue // cross-workspace / unknown id contributes no trigger
		}

		post := loaded
		in := service.IssueTriggerInput{PrevStatus: loaded.Status}
		if hasNewAssignee {
			post.AssigneeType = newAssigneeType
			post.AssigneeID = newAssigneeID
			in.AssigneeChanged = loaded.AssigneeType.String != newAssigneeType.String ||
				uuidToString(loaded.AssigneeID) != uuidToString(newAssigneeID)
		}
		if req.Status != nil && *req.Status != "" {
			post.Status = *req.Status
			in.StatusChanged = loaded.Status != *req.Status
			// A status that enters a workflow handoff step reassigns the
			// issue, exactly as UpdateIssue will. (MUL-7420)
			if !hasNewAssignee {
				params := db.UpdateIssueParams{
					ProjectID:    loaded.ProjectID,
					Status:       pgtype.Text{String: *req.Status, Valid: true},
					AssigneeType: loaded.AssigneeType,
					AssigneeID:   loaded.AssigneeID,
				}
				if hand, err := h.applyIssueWorkflow(r.Context(), h.Queries, loaded, &params, true, false, false, workflowCaller{}); err == nil && hand != nil {
					post.AssigneeType = params.AssigneeType
					post.AssigneeID = params.AssigneeID
					in.AssigneeChanged = true
					in.EnteredHandoffStep = true
				}
			}
		}
		in.Issue = post
		appendTrigger(post, in)
	}

	resp.TotalCount = len(resp.Triggers)
	writeJSON(w, http.StatusOK, resp)
}

// recordStopNotConfirmed leaves the reason a run did not start where people
// read the issue: the gate is closed until the old run's daemon acks.
func (h *Handler) recordStopNotConfirmed(ctx context.Context, issue db.Issue, actorType, actorID string, e *service.StopNotConfirmedError) {
	details, _ := json.Marshal(map[string]string{"message": e.Error(), "task_id": uuidToString(e.TaskID)})
	activity, err := h.Queries.CreateActivity(ctx, db.CreateActivityParams{
		ID:          dbid.NewV7(),
		WorkspaceID: issue.WorkspaceID,
		IssueID:     issue.ID,
		ActorType:   strToText(actorType),
		ActorID:     optionalActorUUID(actorID),
		Action:      "stop_not_confirmed",
		Details:     details,
	})
	if err != nil {
		slog.Warn("record stop-not-confirmed activity failed", "err", err, "issue_id", uuidToString(issue.ID))
		return
	}
	h.publish(protocol.EventActivityCreated, uuidToString(issue.WorkspaceID), actorType, actorID, map[string]any{
		"issue_id": uuidToString(issue.ID),
		"entry": map[string]any{
			"type": "activity", "id": uuidToString(activity.ID), "action": activity.Action,
			"actor_type": actorType, "actor_id": actorID, "details": json.RawMessage(details),
			"created_at": timestampToString(activity.CreatedAt),
		},
	})
}
