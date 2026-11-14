package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// RunInputResponse is one stored trigger of an issue's agent.
type RunInputResponse struct {
	EventID            string          `json:"event_id"`
	Seq                int64           `json:"seq"`
	AgentID            string          `json:"agent_id"`
	Kind               string          `json:"kind"`
	State              string          `json:"state"`
	TaskID             *string         `json:"task_id"`
	RedeliveryOfTaskID *string         `json:"redelivery_of_task_id"`
	ParkReason         *string         `json:"park_reason"`
	FailureClass       *string         `json:"failure_class"`
	Payload            json.RawMessage `json:"payload"`
	CreatedAt          string          `json:"created_at"`
	// ThreadKey is the comment thread the input belongs to; null while unknown.
	ThreadKey *string `json:"thread_key"`
}

func runInputResponse(in db.RunInput) RunInputResponse {
	payload := json.RawMessage(in.Payload)
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	return RunInputResponse{
		EventID: in.EventID, Seq: in.Seq.Int64, AgentID: uuidToString(in.AgentID), Kind: in.Kind, State: in.State,
		TaskID: uuidToPtr(in.TaskID), RedeliveryOfTaskID: uuidToPtr(in.RedeliveryOfTaskID),
		ParkReason: textToPtr(in.ParkReason), FailureClass: textToPtr(in.FailureClass),
		Payload: payload, CreatedAt: timestampToString(in.CreatedAt), ThreadKey: uuidToPtr(in.ThreadKey),
	}
}

// ListIssueRunInputs lists owned, claimed and parked inputs of an issue.
func (h *Handler) ListIssueRunInputs(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	rows, err := h.Queries.ListIssueRunInputs(r.Context(), issue.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list inputs")
		return
	}
	out := make([]RunInputResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, runInputResponse(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"inputs": out})
}

type runInputsActionRequest struct {
	AgentID string `json:"agent_id"`
	// ThreadKey and InputIDs are the explicit recovery of inputs whose thread is
	// unknown: a comment id of the issue, or "none" for no thread, and the exact
	// input seqs to resume there.
	ThreadKey string  `json:"thread_key"`
	InputIDs  []int64 `json:"input_ids"`
}

// ResumeIssueRunInputs and DiscardIssueRunInputs act on the parked batch of one
// agent. A member who may run that agent decides; the default agent is the
// issue's assignee.
func (h *Handler) ResumeIssueRunInputs(w http.ResponseWriter, r *http.Request) {
	h.actOnParkedInputs(w, r, false)
}

func (h *Handler) DiscardIssueRunInputs(w http.ResponseWriter, r *http.Request) {
	h.actOnParkedInputs(w, r, true)
}

func (h *Handler) actOnParkedInputs(w http.ResponseWriter, r *http.Request, discard bool) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	var req runInputsActionRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	var agentID pgtype.UUID
	if req.AgentID != "" {
		if agentID, ok = parseUUIDOrBadRequest(w, req.AgentID, "agent_id"); !ok {
			return
		}
	} else if issue.AssigneeType.String == "agent" && issue.AssigneeID.Valid {
		agentID = issue.AssigneeID
	} else {
		writeError(w, http.StatusBadRequest, "agent_id required: the issue has no agent assignee")
		return
	}
	workspaceID := uuidToString(issue.WorkspaceID)
	agent, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{ID: agentID, WorkspaceID: issue.WorkspaceID})
	if err != nil {
		writeError(w, http.StatusNotFound, "agent not found")
		return
	}
	actorType, actorID := h.resolveActor(r, userID, workspaceID)
	if actorType != "member" || !h.canInvokeAgent(r.Context(), agent, actorType, actorID, h.invokeOriginatorFromRequest(r, actorType, actorID), workspaceID) {
		h.writeDispatchBlocked(w, http.StatusForbidden, ReasonInvocationNotAllowed)
		return
	}
	if discard {
		n, derr := h.TaskService.DiscardParkedInputs(r.Context(), issue.ID, agent.ID)
		if derr != nil {
			writeError(w, http.StatusInternalServerError, "failed to update inputs")
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"count": n})
		return
	}
	var res service.ResumeResult
	if req.ThreadKey != "" || len(req.InputIDs) > 0 {
		threadKey := pgtype.UUID{Valid: true}
		if req.ThreadKey != "none" {
			var tok bool
			if threadKey, tok = parseUUIDOrBadRequest(w, req.ThreadKey, "thread_key"); !tok {
				return
			}
		}
		res, err = h.TaskService.ResumeUnknownThreadInputs(r.Context(), issue.ID, agent.ID, threadKey, req.InputIDs)
		if errors.Is(err, service.ErrResumeInvalid) {
			writeError(w, http.StatusBadRequest, "invalid thread_key or input_ids")
			return
		}
		if errors.Is(err, service.ErrResumeConflict) {
			writeError(w, http.StatusConflict, "inputs changed or the thread's run is busy; nothing was changed")
			return
		}
	} else {
		res, err = h.TaskService.ResumeParkedInputs(r.Context(), issue.ID, agent.ID)
	}
	if writeProjectAdmissionError(w, err) {
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update inputs")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type setWIPLimitRequest struct {
	Limit *int32 `json:"limit"`
}

// SetProjectWIPLimit sets or clears (null) the project's WIP limit.
func (h *Handler) SetProjectWIPLimit(w http.ResponseWriter, r *http.Request) {
	wsUUID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace id")
	if !ok {
		return
	}
	projectID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "project id")
	if !ok {
		return
	}
	if _, ok := requireUserID(w, r); !ok {
		return
	}
	var req setWIPLimitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.Limit != nil && *req.Limit < 1) {
		writeError(w, http.StatusBadRequest, "limit must be a positive integer or null")
		return
	}
	if _, err := h.Queries.GetProjectInWorkspace(r.Context(), db.GetProjectInWorkspaceParams{ID: projectID, WorkspaceID: wsUUID}); err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	if _, err := h.TaskService.SetProjectWIPLimit(r.Context(), wsUUID, projectID, req.Limit); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to set WIP limit")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"wip_limit": req.Limit})
}
