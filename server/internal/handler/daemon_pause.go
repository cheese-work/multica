package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	obsmetrics "github.com/multica-ai/multica/server/internal/metrics"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Machine pause (CHE-2430). A daemon_pause row stops new task claims for every
// runtime on that daemon; running tasks finish.

// A pause lifts itself so a forgotten one cannot hold a machine for ever
// (Cheese, 29/10/2026). Maintenance is planned work, so it gets the longer window.
const (
	maintenancePauseTTL = 2 * time.Hour
	otherPauseTTL       = 30 * time.Minute
)

func pauseTTL(reason string) time.Duration {
	if reason == "maintenance" {
		return maintenancePauseTTL
	}
	return otherPauseTTL
}

func daemonPauseKey(workspaceID pgtype.UUID, daemonID string) string {
	return uuidToString(workspaceID) + "|" + daemonID
}

// pausedDaemonSet returns the keys of the paused daemons among the runtimes'
// (workspace, daemon) pairs. Runtimes with no daemon_id (cloud) are never paused.
func (h *Handler) pausedDaemonSet(ctx context.Context, runtimes []db.AgentRuntime) (map[string]struct{}, error) {
	var keys []string
	for _, rt := range runtimes {
		if rt.DaemonID.Valid {
			keys = append(keys, daemonPauseKey(rt.WorkspaceID, rt.DaemonID.String))
		}
	}
	paused := map[string]struct{}{}
	if len(keys) == 0 {
		return paused, nil
	}
	rows, err := h.Queries.ListPausedDaemons(ctx, db.ListPausedDaemonsParams{
		Keys:            keys,
		MaintenanceSecs: int32(maintenancePauseTTL.Seconds()),
		OtherSecs:       int32(otherPauseTTL.Seconds()),
	})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		paused[daemonPauseKey(row.WorkspaceID, row.DaemonID)] = struct{}{}
	}
	return paused, nil
}

func runtimeOnPausedDaemon(paused map[string]struct{}, rt db.AgentRuntime) bool {
	if !rt.DaemonID.Valid {
		return false
	}
	_, ok := paused[daemonPauseKey(rt.WorkspaceID, rt.DaemonID.String)]
	return ok
}

var validPauseReasons = map[string]bool{"maintenance": true, "incident": true, "suspended": true}

func pauseToResponse(p db.DaemonPause) RuntimePauseResponse {
	return RuntimePauseResponse{
		Reason:    p.Reason,
		PausedBy:  uuidToPtr(p.PausedBy),
		PausedAt:  timestampToString(p.PausedAt),
		ExpiresAt: timestampToString(pgtype.Timestamptz{Time: p.PausedAt.Time.Add(pauseTTL(p.Reason)), Valid: p.PausedAt.Valid}),
	}
}

// requireDaemonPauseAccess resolves the runtime, then requires the runtime
// (daemon) owner or the workspace owner. A workspace admin who is neither may
// not pause: stopping a machine is the lender's call. The pause applies to the
// runtime's whole machine.
func (h *Handler) requireDaemonPauseAccess(w http.ResponseWriter, r *http.Request) (db.AgentRuntime, db.Member, bool) {
	rt, member, ok := h.requireRuntimeReadAccess(w, r, obsmetrics.RuntimeLookupSourceRuntimeAPI, chi.URLParam(r, "runtimeId"))
	if !ok {
		return rt, member, false
	}
	if !canPauseDaemon(member, rt) {
		writeError(w, http.StatusForbidden, "only the machine owner and the workspace owner can pause or resume a machine")
		return rt, member, false
	}
	if !rt.DaemonID.Valid {
		writeError(w, http.StatusBadRequest, "runtime is not hosted by a machine daemon")
		return rt, member, false
	}
	return rt, member, true
}

func canPauseDaemon(member db.Member, rt db.AgentRuntime) bool {
	return member.Role == "owner" || (rt.OwnerID.Valid && uuidToString(rt.OwnerID) == uuidToString(member.UserID))
}

// PauseDaemon stops new task claims on the runtime's machine. Re-pausing
// replaces the reason and actor.
func (h *Handler) PauseDaemon(w http.ResponseWriter, r *http.Request) {
	rt, member, ok := h.requireDaemonPauseAccess(w, r)
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Reason == "" {
		req.Reason = "maintenance"
	}
	if !validPauseReasons[req.Reason] {
		writeError(w, http.StatusBadRequest, "reason must be maintenance, incident or suspended")
		return
	}
	pause, err := h.Queries.UpsertDaemonPause(r.Context(), db.UpsertDaemonPauseParams{
		WorkspaceID: rt.WorkspaceID,
		DaemonID:    rt.DaemonID.String,
		Reason:      req.Reason,
		PausedBy:    member.UserID,
	})
	if err != nil {
		slog.Error("pause daemon failed", "error", err, "runtime_id", uuidToString(rt.ID))
		writeError(w, http.StatusInternalServerError, "failed to pause machine")
		return
	}
	h.PublishRuntimeRefresh(uuidToString(rt.WorkspaceID), "member", uuidToString(member.UserID), "pause")
	writeJSON(w, http.StatusOK, pauseToResponse(pause))
}

// ResumeDaemon lets the machine claim again. Idempotent.
func (h *Handler) ResumeDaemon(w http.ResponseWriter, r *http.Request) {
	rt, member, ok := h.requireDaemonPauseAccess(w, r)
	if !ok {
		return
	}
	if _, err := h.Queries.DeleteDaemonPause(r.Context(), db.DeleteDaemonPauseParams{
		WorkspaceID: rt.WorkspaceID,
		DaemonID:    rt.DaemonID.String,
	}); err != nil {
		slog.Error("resume daemon failed", "error", err, "runtime_id", uuidToString(rt.ID))
		writeError(w, http.StatusInternalServerError, "failed to resume machine")
		return
	}
	h.PublishRuntimeRefresh(uuidToString(rt.WorkspaceID), "member", uuidToString(member.UserID), "resume")
	w.WriteHeader(http.StatusNoContent)
}

// attachDaemonPauseState fills Pause and DaemonRunningTasks on a runtime list.
func (h *Handler) attachDaemonPauseState(ctx context.Context, workspaceID pgtype.UUID, resp []AgentRuntimeResponse) error {
	pauses, err := h.Queries.ListDaemonPausesByWorkspace(ctx, db.ListDaemonPausesByWorkspaceParams{
		WorkspaceID:     workspaceID,
		MaintenanceSecs: int32(maintenancePauseTTL.Seconds()),
		OtherSecs:       int32(otherPauseTTL.Seconds()),
	})
	if err != nil {
		return err
	}
	counts, err := h.Queries.CountRunningTasksByDaemon(ctx, workspaceID)
	if err != nil {
		return err
	}
	pauseByDaemon := make(map[string]RuntimePauseResponse, len(pauses))
	for _, p := range pauses {
		pauseByDaemon[p.DaemonID] = pauseToResponse(p)
	}
	runningByDaemon := make(map[string]int, len(counts))
	for _, c := range counts {
		runningByDaemon[c.DaemonID] = int(c.RunningTasks)
	}
	for i := range resp {
		if resp[i].DaemonID == nil {
			continue
		}
		daemonID := *resp[i].DaemonID
		if p, ok := pauseByDaemon[daemonID]; ok {
			resp[i].Pause = &p
		}
		running := runningByDaemon[daemonID]
		resp[i].DaemonRunningTasks = &running
	}
	return nil
}

// RuntimePauseResponse is the pause state of a machine (daemon).
type RuntimePauseResponse struct {
	Reason   string  `json:"reason"`
	PausedBy *string `json:"paused_by"`
	PausedAt string  `json:"paused_at"`
	// ExpiresAt is when the pause lifts on its own (see pauseTTL).
	ExpiresAt string `json:"expires_at"`
}
