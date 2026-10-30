package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	obsmetrics "github.com/multica-ai/multica/server/internal/metrics"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
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

// A machine is (owner, daemon_id), not the daemon_id alone: registration takes
// a daemon_id the client chose, so another owner can register the same string.
// The owner is the runtime's owner, or its workspace when it has none. One owner's
// machine joined to several workspaces is one machine.

// machineOwnerID is the owner half of a machine's identity.
func machineOwnerID(rt db.AgentRuntime) pgtype.UUID {
	if rt.OwnerID.Valid {
		return rt.OwnerID
	}
	return rt.WorkspaceID
}

// machineKey identifies the machine a runtime runs on. ok is false for a runtime
// with no daemon (cloud), which no pause applies to.
func machineKey(rt db.AgentRuntime) (key string, ok bool) {
	if !rt.DaemonID.Valid {
		return "", false
	}
	return uuidToString(machineOwnerID(rt)) + "/" + rt.DaemonID.String, true
}

func machineKeyOf(ownerID pgtype.UUID, daemonID string) string {
	return uuidToString(ownerID) + "/" + daemonID
}

// machinesOf collects the owner and daemon_id lists for the runtimes' machines.
func machinesOf(runtimes []db.AgentRuntime) (ownerIDs []pgtype.UUID, daemonIDs []string) {
	for _, rt := range runtimes {
		if _, ok := machineKey(rt); ok {
			ownerIDs = append(ownerIDs, machineOwnerID(rt))
			daemonIDs = append(daemonIDs, rt.DaemonID.String)
		}
	}
	return ownerIDs, daemonIDs
}

// livePauses returns the live pause of each given runtime's machine, by machine key.
func (h *Handler) livePauses(ctx context.Context, runtimes []db.AgentRuntime) (map[string]db.DaemonPause, error) {
	pauses := map[string]db.DaemonPause{}
	ownerIDs, daemonIDs := machinesOf(runtimes)
	if len(daemonIDs) == 0 {
		return pauses, nil
	}
	rows, err := h.Queries.ListLiveDaemonPauses(ctx, db.ListLiveDaemonPausesParams{
		MachineOwnerIds: ownerIDs,
		DaemonIds:       daemonIDs,
		MaintenanceSecs: int32(maintenancePauseTTL.Seconds()),
		OtherSecs:       int32(otherPauseTTL.Seconds()),
	})
	if err != nil {
		return nil, err
	}
	asked := make(map[string]struct{}, len(daemonIDs))
	for i := range daemonIDs {
		asked[machineKeyOf(ownerIDs[i], daemonIDs[i])] = struct{}{}
	}
	for _, row := range rows {
		key := machineKeyOf(row.MachineOwnerID, row.DaemonID)
		if _, ok := asked[key]; ok {
			pauses[key] = row
		}
	}
	return pauses, nil
}

// pausedDaemonSet returns the keys of the paused machines among the runtimes'.
func (h *Handler) pausedDaemonSet(ctx context.Context, runtimes []db.AgentRuntime) (map[string]struct{}, error) {
	pauses, err := h.livePauses(ctx, runtimes)
	if err != nil {
		return nil, err
	}
	paused := make(map[string]struct{}, len(pauses))
	for key := range pauses {
		paused[key] = struct{}{}
	}
	return paused, nil
}

func runtimeOnPausedDaemon(paused map[string]struct{}, rt db.AgentRuntime) bool {
	key, ok := machineKey(rt)
	if !ok {
		return false
	}
	_, hit := paused[key]
	return hit
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
// not pause: stopping a machine is the lender's call. The workspace owner needs
// no access to a private runtime to pause its machine; a member who may not even
// see the runtime still gets 404. The pause applies to the runtime's whole machine.
func (h *Handler) requireDaemonPauseAccess(w http.ResponseWriter, r *http.Request) (db.AgentRuntime, db.Member, bool) {
	runtimeUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "runtimeId"), "runtime_id")
	if !ok {
		return db.AgentRuntime{}, db.Member{}, false
	}
	rt, err := h.getAgentRuntime(r.Context(), obsmetrics.RuntimeLookupSourceRuntimeAPI, runtimeUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "runtime not found")
		return db.AgentRuntime{}, db.Member{}, false
	}
	member, ok := h.requireWorkspaceMember(w, r, uuidToString(rt.WorkspaceID), "runtime not found")
	if !ok {
		return db.AgentRuntime{}, db.Member{}, false
	}
	if !canPauseDaemon(member, rt) {
		if !canUseRuntimeForAgent(member, rt) {
			writeError(w, http.StatusNotFound, "runtime not found")
			return rt, member, false
		}
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
		MachineOwnerID: machineOwnerID(rt),
		DaemonID:       rt.DaemonID.String,
		WorkspaceID:    rt.WorkspaceID,
		Reason:         req.Reason,
		PausedBy:       member.UserID,
	})
	if err != nil {
		slog.Error("pause daemon failed", "error", err, "runtime_id", uuidToString(rt.ID))
		writeError(w, http.StatusInternalServerError, "failed to pause machine")
		return
	}
	h.publishMachineRefresh(r.Context(), rt, "member", uuidToString(member.UserID), "pause")
	writeJSON(w, http.StatusOK, pauseToResponse(pause))
}

// ResumeDaemon lets the machine claim again. Idempotent.
func (h *Handler) ResumeDaemon(w http.ResponseWriter, r *http.Request) {
	rt, member, ok := h.requireDaemonPauseAccess(w, r)
	if !ok {
		return
	}
	if _, err := h.Queries.DeleteDaemonPause(r.Context(), db.DeleteDaemonPauseParams{MachineOwnerID: machineOwnerID(rt), DaemonID: rt.DaemonID.String}); err != nil {
		slog.Error("resume daemon failed", "error", err, "runtime_id", uuidToString(rt.ID))
		writeError(w, http.StatusInternalServerError, "failed to resume machine")
		return
	}
	h.publishMachineRefresh(r.Context(), rt, "member", uuidToString(member.UserID), "resume")
	w.WriteHeader(http.StatusNoContent)
}

// attachDaemonPauseState fills Pause and DaemonRunningTasks on a runtime list.
// A machine's pause and drain count span the workspaces it serves for its owner;
// paused_by is shown only for a pause set in the caller's own workspace.
func (h *Handler) attachDaemonPauseState(ctx context.Context, workspaceID pgtype.UUID, runtimes []db.AgentRuntime, resp []AgentRuntimeResponse) error {
	ownerIDs, daemonIDs := machinesOf(runtimes)
	if len(daemonIDs) == 0 {
		return nil
	}
	pauses, err := h.livePauses(ctx, runtimes)
	if err != nil {
		return err
	}
	counts, err := h.Queries.CountRunningTasksByMachine(ctx, db.CountRunningTasksByMachineParams{
		MachineOwnerIds: ownerIDs,
		DaemonIds:       daemonIDs,
	})
	if err != nil {
		return err
	}
	runningByMachine := make(map[string]int, len(counts))
	for _, c := range counts {
		runningByMachine[machineKeyOf(c.MachineOwnerID, c.DaemonID)] = int(c.RunningTasks)
	}
	for i, rt := range runtimes {
		key, ok := machineKey(rt)
		if !ok {
			continue
		}
		if p, ok := pauses[key]; ok {
			pr := pauseToResponse(p)
			if p.WorkspaceID != workspaceID {
				pr.PausedBy = nil
			}
			resp[i].Pause = &pr
		}
		running := runningByMachine[key]
		resp[i].DaemonRunningTasks = &running
	}
	return nil
}

// publishMachineRefresh tells every workspace the machine serves for its owner
// to refetch its runtimes, so the Machine card and banner there follow a pause,
// resume or drain change made through another workspace. The payload names no task.
func (h *Handler) publishMachineRefresh(ctx context.Context, rt db.AgentRuntime, actorType, actorID, action string) {
	PublishMachineRefresh(ctx, h.Queries, h.Bus, machineOwnerID(rt), rt.DaemonID.String, rt.WorkspaceID, "", actorType, actorID, action)
}

// PublishMachineRefresh sends a runtime refresh to each workspace the machine
// serves, except skipWorkspaceID (which already hears the triggering event).
// The addressed workspace is always included when it serves the machine.
func PublishMachineRefresh(ctx context.Context, q *db.Queries, bus *events.Bus, ownerID pgtype.UUID, daemonID string, fallbackWorkspaceID pgtype.UUID, skipWorkspaceID, actorType, actorID, action string) {
	workspaceIDs, err := q.ListMachineWorkspaceIDs(ctx, db.ListMachineWorkspaceIDsParams{OwnerID: ownerID, DaemonID: strToText(daemonID)})
	if err != nil {
		slog.Warn("list machine workspaces failed", "error", err)
		workspaceIDs = nil
	}
	if len(workspaceIDs) == 0 && fallbackWorkspaceID.Valid {
		workspaceIDs = []pgtype.UUID{fallbackWorkspaceID}
	}
	for _, id := range workspaceIDs {
		wsID := uuidToString(id)
		if wsID == skipWorkspaceID {
			continue
		}
		bus.Publish(events.Event{
			Type:        protocol.EventDaemonRegister,
			WorkspaceID: wsID,
			ActorType:   actorType,
			ActorID:     actorID,
			Payload:     map[string]any{"action": action},
		})
	}
}

// RefreshMachineOfTask runs when a task starts or ends. While its machine is
// paused, the drain count the Machine card shows changes, in every workspace the
// machine serves; only the task's own workspace hears the task event, so the
// others get a runtime refresh. A machine that is not paused needs none.
func RefreshMachineOfTask(ctx context.Context, q *db.Queries, bus *events.Bus, taskID string, eventWorkspaceID string) {
	taskUUID, err := util.ParseUUID(taskID)
	if err != nil {
		return
	}
	task, err := q.GetAgentTask(ctx, taskUUID)
	if err != nil || !task.RuntimeID.Valid {
		return
	}
	rt, err := q.GetAgentRuntime(ctx, task.RuntimeID)
	if err != nil {
		return
	}
	key, ok := machineKey(rt)
	if !ok {
		return
	}
	pauses, err := (&Handler{Queries: q}).livePauses(ctx, []db.AgentRuntime{rt})
	if err != nil {
		slog.Warn("machine pause lookup failed", "error", err)
		return
	}
	if _, paused := pauses[key]; !paused {
		return
	}
	PublishMachineRefresh(ctx, q, bus, machineOwnerID(rt), rt.DaemonID.String, rt.WorkspaceID, eventWorkspaceID, "system", "", "drain")
}

// RuntimePauseResponse is the pause state of a machine (daemon).
type RuntimePauseResponse struct {
	Reason   string  `json:"reason"`
	PausedBy *string `json:"paused_by"`
	PausedAt string  `json:"paused_at"`
	// ExpiresAt is when the pause lifts on its own (see pauseTTL).
	ExpiresAt string `json:"expires_at"`
}
