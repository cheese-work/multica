package handler

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Banner audience (CHE-2674, L4 of CHE-2429). The daemon owner hears about a
// pause from the runtime list. Everyone else hears only when it costs them: a
// task of theirs is queued for a machine that is paused or has no online runtime.

// AffectedMachineResponse is one off machine that holds work the caller cares about.
type AffectedMachineResponse struct {
	MachineOwnerID string `json:"machine_owner_id"`
	DaemonID       string `json:"daemon_id"`
	DeviceName     string `json:"device_name"`
	// State is "paused" or "offline". A paused machine reports "paused" even when
	// it is also offline.
	State  string  `json:"state"`
	Reason *string `json:"reason"`
	// EventID names this pause or offline period. A client that dismissed the
	// banner shows it again when the id changes.
	EventID      string  `json:"event_id"`
	ExpiresAt    *string `json:"expires_at"`
	WaitingTasks int     `json:"waiting_tasks"`
}

// ListAffectedMachines returns the off machines that hold queued tasks the
// caller is affected by. The workspace owner counts every queued task of the
// workspace; any other member counts tasks of the projects they lead.
func (h *Handler) ListAffectedMachines(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	member, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found")
	if !ok {
		return
	}
	rows, err := h.Queries.ListAffectedMachines(r.Context(), db.ListAffectedMachinesParams{
		WorkspaceID:     parseUUID(workspaceID),
		AllProjects:     member.Role == "owner",
		UserID:          parseUUID(userID),
		MaintenanceSecs: int32(maintenancePauseTTL.Seconds()),
		OtherSecs:       int32(otherPauseTTL.Seconds()),
	})
	if err != nil {
		slog.Error("list affected machines failed", "error", err, "workspace_id", workspaceID)
		writeError(w, http.StatusInternalServerError, "failed to list affected machines")
		return
	}
	machines := make([]AffectedMachineResponse, len(rows))
	for i, row := range rows {
		machines[i] = affectedMachineToResponse(row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"machines": machines})
}

func affectedMachineToResponse(row db.ListAffectedMachinesRow) AffectedMachineResponse {
	machine := AffectedMachineResponse{
		MachineOwnerID: uuidToString(row.MachineOwnerID),
		DaemonID:       row.DaemonID,
		DeviceName:     row.DeviceName,
		WaitingTasks:   int(row.WaitingTasks),
	}
	key := machineKeyOf(row.MachineOwnerID, row.DaemonID)
	if row.PauseReason.Valid {
		pause := pauseToResponse(db.DaemonPause{Reason: row.PauseReason.String, PausedAt: row.PausedAt})
		machine.State = "paused"
		machine.Reason = &row.PauseReason.String
		machine.ExpiresAt = &pause.ExpiresAt
		machine.EventID = eventID("paused", key, row.PausedAt)
		return machine
	}
	machine.State = "offline"
	machine.EventID = eventID("offline", key, row.LastSeenAt)
	return machine
}

func eventID(state, machineKey string, since pgtype.Timestamptz) string {
	var micros int64
	if since.Valid {
		micros = since.Time.Truncate(time.Microsecond).UnixMicro()
	}
	return fmt.Sprintf("%s:%s:%d", state, machineKey, micros)
}
