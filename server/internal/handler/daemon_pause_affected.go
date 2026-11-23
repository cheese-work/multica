package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
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
	// Dismissed is set only while the server-stored dismissal is on
	// (bannerDismissalCapability): true when the caller closed the banner for
	// this offline period. Always false for a paused row.
	Dismissed *bool `json:"dismissed,omitempty"`
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
	rows, err := h.listAffectedRows(r.Context(), h.Queries, workspaceID, userID, member.Role)
	if err != nil {
		slog.Error("list affected machines failed", "error", err, "workspace_id", workspaceID)
		writeError(w, http.StatusInternalServerError, "failed to list affected machines")
		return
	}
	machines := make([]AffectedMachineResponse, len(rows))
	for i, row := range rows {
		machines[i] = affectedMachineToResponse(row)
	}
	if bannerDismissalCapability == "" {
		writeJSON(w, http.StatusOK, map[string]any{"machines": machines})
		return
	}
	stored, err := h.Queries.ListMachineBannerDismissals(r.Context(), db.ListMachineBannerDismissalsParams{
		WorkspaceID: parseUUID(workspaceID),
		UserID:      parseUUID(userID),
	})
	if err != nil {
		slog.Error("list banner dismissals failed", "error", err, "workspace_id", workspaceID)
		writeError(w, http.StatusInternalServerError, "failed to list affected machines")
		return
	}
	storedID := make(map[string]string, len(stored))
	for _, d := range stored {
		storedID[machineKeyOf(d.MachineOwnerID, d.DaemonID)] = d.EventID
	}
	for i, row := range rows {
		// Exact equality: a new offline period has a new id and shows again.
		dismissed := machines[i].State == "offline" && storedID[machineKeyOf(row.MachineOwnerID, row.DaemonID)] == machines[i].EventID
		machines[i].Dismissed = &dismissed
	}
	writeJSON(w, http.StatusOK, map[string]any{"machines": machines, "banner_dismissal": bannerDismissalCapability})
}

// listAffectedRows reads the affected machines of one caller: the workspace
// owner counts every queued task, anyone else the tasks of projects they lead.
func (h *Handler) listAffectedRows(ctx context.Context, q *db.Queries, workspaceID, userID, role string) ([]db.ListAffectedMachinesRow, error) {
	return q.ListAffectedMachines(ctx, db.ListAffectedMachinesParams{
		WorkspaceID:     parseUUID(workspaceID),
		AllProjects:     role == "owner",
		UserID:          parseUUID(userID),
		MaintenanceSecs: int32(maintenancePauseTTL.Seconds()),
		OtherSecs:       int32(otherPauseTTL.Seconds()),
	})
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
		pause := pauseToResponse(db.DaemonPause{Reason: row.PauseReason.String, PausedAt: row.PausedAt, ExpiresAt: row.PauseExpiresAt})
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

// bannerDismissalCapability is the switch for server-stored banner dismissals
// (CHE-9190). While it is empty the feature is off: the GET carries no
// dismissal fields and both POST routes answer 409. Stage 2 sets "epoch-v1"
// next to the epoch eventID producer (CHE-9254).
var bannerDismissalCapability = ""

// maxBannerDismissalIDs caps one dismiss or reopen request.
const maxBannerDismissalIDs = 100

// Test hooks (nil in production): they let a test hold one writer at a fixed
// point. before-lock runs before the machine lock is taken; after-validate runs
// after the id was last checked and before the write.
var (
	bannerDismissBeforeLockHook    func()
	bannerDismissAfterValidateHook func()
)

// DismissAffectedMachines stores the caller's dismissal for each offline
// machine event id that is still the machine's current offline id.
func (h *Handler) DismissAffectedMachines(w http.ResponseWriter, r *http.Request) {
	h.writeBannerDismissals(w, r, true)
}

// ReopenAffectedMachines deletes the caller's dismissal of each event id that
// is still the machine's current offline id.
func (h *Handler) ReopenAffectedMachines(w http.ResponseWriter, r *http.Request) {
	h.writeBannerDismissals(w, r, false)
}

func (h *Handler) writeBannerDismissals(w http.ResponseWriter, r *http.Request, dismiss bool) {
	workspaceID := h.resolveWorkspaceID(r)
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	member, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found")
	if !ok {
		return
	}
	if bannerDismissalCapability == "" {
		writeError(w, http.StatusConflict, "banner_dismissal_unavailable")
		return
	}
	var req struct {
		EventIDs []string `json:"event_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.EventIDs) > maxBannerDismissalIDs {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d event_ids", maxBannerDismissalIDs))
		return
	}
	rows, err := h.listAffectedRows(r.Context(), h.Queries, workspaceID, userID, member.Role)
	if err != nil {
		slog.Error("banner dismissal list failed", "error", err, "workspace_id", workspaceID)
		writeError(w, http.StatusInternalServerError, "failed to update banner dismissal")
		return
	}
	// Only the caller's own offline rows can be acted on. Unknown, stale,
	// paused and foreign ids are ignored.
	current := map[string]db.ListAffectedMachinesRow{}
	for _, row := range rows {
		if m := affectedMachineToResponse(row); m.State == "offline" {
			current[m.EventID] = row
		}
	}
	done, ignored := []string{}, []string{}
	seen := map[string]bool{}
	for _, id := range req.EventIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		row, ok := current[id]
		if !ok {
			ignored = append(ignored, id)
			continue
		}
		applied, err := h.applyBannerDismissal(r.Context(), workspaceID, userID, member.Role, row, id, dismiss)
		if err != nil {
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				writeError(w, http.StatusNotFound, "workspace not found")
			case isRetryableLockFailure(err):
				writeError(w, http.StatusConflict, "the machine is being changed, try again")
			default:
				slog.Error("banner dismissal write failed", "error", err, "workspace_id", workspaceID)
				writeError(w, http.StatusInternalServerError, "failed to update banner dismissal")
			}
			return
		}
		if applied {
			done = append(done, id)
		} else {
			ignored = append(ignored, id)
		}
	}
	key := "dismissed"
	if !dismiss {
		key = "reopened"
	}
	writeJSON(w, http.StatusOK, map[string]any{key: done, "ignored": ignored})
}

// applyBannerDismissal writes one machine's dismissal in one transaction. The
// transaction takes the member's machine lock, then re-reads the caller's
// affected rows (read committed: each statement sees every earlier commit) and
// acts only if id is still the machine's current offline id. A write validated
// against an older period either runs before a newer write takes the lock and is
// overwritten, or runs after it and finds the id stale. It reports false when
// the id is no longer current.
func (h *Handler) applyBannerDismissal(ctx context.Context, workspaceID, userID, role string, row db.ListAffectedMachinesRow, id string, dismiss bool) (bool, error) {
	if bannerDismissBeforeLockHook != nil {
		bannerDismissBeforeLockHook()
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = %d", pauseLockTimeout.Milliseconds())); err != nil {
		return false, err
	}
	qtx := h.Queries.WithTx(tx)
	ws, user := parseUUID(workspaceID), parseUUID(userID)
	// The workspace row FOR SHARE makes a concurrent workspace delete either
	// finish first (404, nothing written) or wait for this write.
	if _, err := qtx.LockWorkspaceForMachinePause(ctx, ws); err != nil {
		return false, err
	}
	if err := qtx.LockMachineBannerDismissal(ctx, db.LockMachineBannerDismissalParams{
		WorkspaceID: ws, UserID: user, MachineOwnerID: row.MachineOwnerID, DaemonID: row.DaemonID,
	}); err != nil {
		return false, err
	}
	rows, err := h.listAffectedRows(ctx, qtx, workspaceID, userID, role)
	if err != nil {
		return false, err
	}
	stillCurrent := false
	for _, r := range rows {
		if r.MachineOwnerID == row.MachineOwnerID && r.DaemonID == row.DaemonID {
			m := affectedMachineToResponse(r)
			stillCurrent = m.State == "offline" && m.EventID == id
			break
		}
	}
	if !stillCurrent {
		return false, nil
	}
	if bannerDismissAfterValidateHook != nil {
		bannerDismissAfterValidateHook()
	}
	if dismiss {
		err = qtx.UpsertMachineBannerDismissal(ctx, db.UpsertMachineBannerDismissalParams{
			WorkspaceID: ws, UserID: user, MachineOwnerID: row.MachineOwnerID, DaemonID: row.DaemonID, EventID: id,
		})
	} else {
		err = qtx.DeleteMachineBannerDismissal(ctx, db.DeleteMachineBannerDismissalParams{
			WorkspaceID: ws, UserID: user, MachineOwnerID: row.MachineOwnerID, DaemonID: row.DaemonID, EventID: id,
		})
	}
	if err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
