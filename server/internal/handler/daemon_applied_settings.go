package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"reflect"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type appliedSettingsRequest struct {
	RuntimeID            string   `json:"runtime_id"`
	DispatchedAt         string   `json:"dispatched_at"`
	AppliedModel         string   `json:"applied_model"`
	AppliedThinkingLevel string   `json:"applied_thinking_level"`
	AppliedServiceTier   string   `json:"applied_service_tier"`
	AppliedContextArgs   []string `json:"applied_context_args"`
}

// ReportAppliedSettings records the launch selection the daemon passed to the
// CLI for one claim generation (CHE-2248). It is the daemon's own report, not
// the model or effort the provider served. The claim generation is
// (task, runtime, dispatched_at), the same contract StartTask uses; there is no
// legacy form of this request.
func (h *Handler) ReportAppliedSettings(w http.ResponseWriter, r *http.Request) {
	task, workspaceID, ok := h.requireDaemonTaskAccessWithWorkspace(w, r, chi.URLParam(r, "taskId"))
	if !ok {
		return
	}
	var req appliedSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	runtimeID, ok := parseUUIDOrBadRequest(w, req.RuntimeID, "runtime_id")
	if !ok {
		return
	}
	generation, err := time.Parse(time.RFC3339Nano, req.DispatchedAt)
	if err != nil || generation.Nanosecond()%1000 != 0 {
		writeError(w, http.StatusBadRequest, "dispatched_at must be a PostgreSQL claim timestamp")
		return
	}
	runtime, ok := h.requireDaemonRuntimeAccess(w, r, req.RuntimeID)
	if !ok {
		return
	}
	if workspaceID != uuidToString(runtime.WorkspaceID) || task.RuntimeID != runtimeID {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}

	contextArgs, err := json.Marshal(nonNilStrings(req.AppliedContextArgs))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid applied_context_args")
		return
	}
	dispatchedAt := pgtype.Timestamptz{Time: generation, Valid: true}
	rows, err := h.Queries.SetTaskAppliedSettings(r.Context(), db.SetTaskAppliedSettingsParams{
		ID: task.ID, RuntimeID: runtimeID, DispatchedAt: dispatchedAt,
		AppliedModel:         pgtype.Text{String: req.AppliedModel, Valid: true},
		AppliedThinkingLevel: pgtype.Text{String: req.AppliedThinkingLevel, Valid: true},
		AppliedServiceTier:   pgtype.Text{String: req.AppliedServiceTier, Valid: true},
		AppliedContextArgs:   contextArgs,
	})
	if err != nil {
		slog.Warn("applied settings: write failed", "task_id", uuidToString(task.ID), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to record applied settings")
		return
	}
	if rows == 1 || h.appliedReportIsRetry(r, task, runtimeID, generation, req) {
		w.WriteHeader(http.StatusOK)
		return
	}
	writeError(w, http.StatusConflict, "applied settings were already reported or the claim is stale")
}

// appliedReportIsRetry reports whether an unwritten report repeats what this
// claim generation already stores (a retry after a lost response) while the task is still live. Anything
// else, including an older claim on the same runtime, is a conflict.
func (h *Handler) appliedReportIsRetry(r *http.Request, task db.AgentTaskQueue, runtimeID pgtype.UUID, generation time.Time, req appliedSettingsRequest) bool {
	current, err := h.Queries.GetAgentTask(r.Context(), task.ID)
	if err != nil || current.RuntimeID != runtimeID || !current.DispatchedAt.Valid || !current.DispatchedAt.Time.Equal(generation) || !current.AppliedReportedAt.Valid || !appliedReportableStatus(current.Status) {
		return false
	}
	var stored []string
	if err := json.Unmarshal(current.AppliedContextArgs, &stored); err != nil {
		return false
	}
	return current.AppliedModel.String == req.AppliedModel &&
		current.AppliedThinkingLevel.String == req.AppliedThinkingLevel &&
		current.AppliedServiceTier.String == req.AppliedServiceTier &&
		reflect.DeepEqual(nonNilStrings(stored), nonNilStrings(req.AppliedContextArgs))
}

// appliedReportableStatus mirrors the status filter of SetTaskAppliedSettings.
func appliedReportableStatus(status string) bool {
	return status == "dispatched" || status == "waiting_local_directory" || status == "running"
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
