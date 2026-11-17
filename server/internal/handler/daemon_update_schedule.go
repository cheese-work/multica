package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Per-machine update schedule (CHE-5465, plan decisions D2, D3, D9). One row per
// machine (owner, daemon_id), the same key and the same permission as machine pause.

const (
	defaultUpdateTimezone   = "Asia/Ho_Chi_Minh"
	defaultUpdateWindowMins = 30
	minUpdateWindowMins     = 10
	// maxUpdateWindowMins is the hard cap (D3); the table has the same CHECK.
	maxUpdateWindowMins  = 30
	updateScheduleNext   = 3
	updateScheduleDenied = "only the machine owner and the workspace owner can change a machine's update schedule"
)

// UpdateScheduleResponse is the schedule as API and runtime list show it.
// NextRuns is filled by the schedule endpoints only.
type UpdateScheduleResponse struct {
	Enabled       bool     `json:"enabled"`
	Cron          string   `json:"cron"`
	Timezone      string   `json:"timezone"`
	WindowMinutes int      `json:"window_minutes"`
	NextRuns      []string `json:"next_runs,omitempty"`
	UpdatedAt     *string  `json:"updated_at"`
	UpdatedBy     *string  `json:"updated_by"`
}

func updateScheduleToResponse(s db.DaemonUpdateSchedule) UpdateScheduleResponse {
	return UpdateScheduleResponse{
		Enabled:       s.Enabled,
		Cron:          s.Cron,
		Timezone:      s.Timezone,
		WindowMinutes: int(s.WindowMinutes),
		UpdatedAt:     timestampToPtr(s.UpdatedAt),
		UpdatedBy:     uuidToPtr(s.UpdatedBy),
	}
}

// defaultUpdateSchedule is what a machine with no saved schedule reads as.
func defaultUpdateSchedule() UpdateScheduleResponse {
	return UpdateScheduleResponse{Timezone: defaultUpdateTimezone, WindowMinutes: defaultUpdateWindowMins}
}

// withNextRuns fills the next fire times in the schedule's own time zone.
func withNextRuns(resp UpdateScheduleResponse) UpdateScheduleResponse {
	resp.NextRuns = []string{}
	if resp.Cron == "" {
		return resp
	}
	loc, err := time.LoadLocation(resp.Timezone)
	if err != nil {
		return resp
	}
	times, err := service.NextOccurrencesAfterUTC(resp.Cron, resp.Timezone, time.Now().UTC(), updateScheduleNext)
	if err != nil {
		return resp
	}
	for _, at := range times {
		resp.NextRuns = append(resp.NextRuns, at.In(loc).Format(time.RFC3339))
	}
	return resp
}

// launchedByDesktop reports a daemon the Electron app manages. It refuses
// self-update, so a schedule for it could never run (D9).
func launchedByDesktop(rt db.AgentRuntime) bool {
	var md struct {
		LaunchedBy string `json:"launched_by"`
	}
	if err := json.Unmarshal(rt.Metadata, &md); err != nil {
		return false
	}
	return md.LaunchedBy == "desktop"
}

// validateUpdateSchedule checks a request and returns the values to store, or a
// message for a 400.
func validateUpdateSchedule(enabled bool, cronExpr string, timezone *string, window *int) (db.UpsertDaemonUpdateScheduleParams, string) {
	p := db.UpsertDaemonUpdateScheduleParams{Enabled: enabled, Cron: strings.TrimSpace(cronExpr), Timezone: defaultUpdateTimezone, WindowMinutes: defaultUpdateWindowMins}
	if timezone != nil && *timezone != "" {
		p.Timezone = *timezone
	}
	if window != nil {
		if *window < minUpdateWindowMins || *window > maxUpdateWindowMins {
			return p, "window_minutes must be between 10 and 30"
		}
		p.WindowMinutes = int32(*window)
	}
	if err := service.ValidateTimezone(p.Timezone); err != nil {
		return p, "unknown time zone " + p.Timezone
	}
	if p.Cron == "" {
		return p, "cron is required"
	}
	// The time zone is its own field, and a descriptor such as @daily is not 5 fields.
	if len(strings.Fields(p.Cron)) != 5 || strings.ContainsAny(p.Cron, "@=") {
		return p, "cron must have exactly 5 fields: minute hour day-of-month month day-of-week"
	}
	if _, err := service.NextOccurrencesAfterUTC(p.Cron, p.Timezone, time.Now().UTC(), 1); err != nil {
		return p, "invalid cron: " + err.Error()
	}
	return p, ""
}

func (h *Handler) loadUpdateSchedule(w http.ResponseWriter, r *http.Request, rt db.AgentRuntime) (*db.DaemonUpdateSchedule, bool) {
	s, err := h.Queries.GetDaemonUpdateSchedule(r.Context(), db.GetDaemonUpdateScheduleParams{MachineOwnerID: machineOwnerID(rt), DaemonID: rt.DaemonID.String})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, true
	}
	if err != nil {
		slog.Error("get update schedule failed", "error", err, "runtime_id", uuidToString(rt.ID))
		writeError(w, http.StatusInternalServerError, "failed to load update schedule")
		return nil, false
	}
	return &s, true
}

// GetUpdateSchedule returns the machine's schedule, or the defaults (disabled,
// no cron) when none is saved.
func (h *Handler) GetUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	rt, _, ok := h.requireMachineOwnerAccess(w, r, updateScheduleDenied)
	if !ok {
		return
	}
	s, ok := h.loadUpdateSchedule(w, r, rt)
	if !ok {
		return
	}
	resp := defaultUpdateSchedule()
	if s != nil {
		resp = updateScheduleToResponse(*s)
	}
	writeJSON(w, http.StatusOK, withNextRuns(resp))
}

// PutUpdateSchedule replaces the machine's schedule.
func (h *Handler) PutUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	rt, member, ok := h.requireMachineOwnerAccess(w, r, updateScheduleDenied)
	if !ok {
		return
	}
	var req struct {
		Enabled       bool    `json:"enabled"`
		Cron          string  `json:"cron"`
		Timezone      *string `json:"timezone"`
		WindowMinutes *int    `json:"window_minutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if launchedByDesktop(rt) {
		writeError(w, http.StatusBadRequest, "this daemon is managed by the Desktop app and cannot update itself on a schedule")
		return
	}
	params, msg := validateUpdateSchedule(req.Enabled, req.Cron, req.Timezone, req.WindowMinutes)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	params.MachineOwnerID = machineOwnerID(rt)
	params.DaemonID = rt.DaemonID.String
	params.UpdatedBy = member.UserID

	before, ok := h.loadUpdateSchedule(w, r, rt)
	if !ok {
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		slog.Error("update schedule tx begin failed", "error", err, "runtime_id", uuidToString(rt.ID))
		writeError(w, http.StatusInternalServerError, "failed to save update schedule")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	saved, err := qtx.UpsertDaemonUpdateSchedule(r.Context(), params)
	if err == nil {
		if err = h.writeAudit(r.Context(), qtx, r, auditInput{
			Kind: "runtime.update_schedule.set", WorkspaceID: rt.WorkspaceID, TargetID: rt.ID,
			Before: updateScheduleAuditFields(before), After: updateScheduleAuditFields(&saved),
		}); err != nil {
			writeAuditFailure(w, r, err, "runtime.update_schedule.set")
			return
		}
		err = tx.Commit(r.Context())
	}
	if err != nil {
		slog.Error("save update schedule failed", "error", err, "runtime_id", uuidToString(rt.ID))
		writeError(w, http.StatusInternalServerError, "failed to save update schedule")
		return
	}
	h.publishMachineRefresh(r.Context(), rt, "member", uuidToString(member.UserID), "update_schedule")
	writeJSON(w, http.StatusOK, withNextRuns(updateScheduleToResponse(saved)))
}

// DeleteUpdateSchedule removes the machine's schedule. Idempotent.
func (h *Handler) DeleteUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	rt, member, ok := h.requireMachineOwnerAccess(w, r, updateScheduleDenied)
	if !ok {
		return
	}
	before, ok := h.loadUpdateSchedule(w, r, rt)
	if !ok {
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		slog.Error("update schedule tx begin failed", "error", err, "runtime_id", uuidToString(rt.ID))
		writeError(w, http.StatusInternalServerError, "failed to clear update schedule")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	_, err = qtx.DeleteDaemonUpdateSchedule(r.Context(), db.DeleteDaemonUpdateScheduleParams{MachineOwnerID: machineOwnerID(rt), DaemonID: rt.DaemonID.String})
	if err == nil {
		if err = h.writeAudit(r.Context(), qtx, r, auditInput{
			Kind: "runtime.update_schedule.clear", WorkspaceID: rt.WorkspaceID, TargetID: rt.ID,
			Before: updateScheduleAuditFields(before), After: updateScheduleAuditFields(nil),
		}); err != nil {
			writeAuditFailure(w, r, err, "runtime.update_schedule.clear")
			return
		}
		err = tx.Commit(r.Context())
	}
	if err != nil {
		slog.Error("clear update schedule failed", "error", err, "runtime_id", uuidToString(rt.ID))
		writeError(w, http.StatusInternalServerError, "failed to clear update schedule")
		return
	}
	h.publishMachineRefresh(r.Context(), rt, "member", uuidToString(member.UserID), "update_schedule")
	w.WriteHeader(http.StatusNoContent)
}

// updateScheduleAuditFields snapshots a schedule; nil means none is saved.
func updateScheduleAuditFields(s *db.DaemonUpdateSchedule) map[string]any {
	if s == nil {
		return map[string]any{"scheduled": false}
	}
	return map[string]any{"scheduled": true, "enabled": s.Enabled, "cron": s.Cron, "timezone": s.Timezone, "window_minutes": s.WindowMinutes}
}

// attachUpdateSchedules fills UpdateSchedule on a runtime list. LastUpdateOutcome
// stays null until the update cycle (later children) records one.
func (h *Handler) attachUpdateSchedules(r *http.Request, runtimes []db.AgentRuntime, resp []AgentRuntimeResponse) error {
	ownerIDs, daemonIDs := machinesOf(runtimes)
	if len(daemonIDs) == 0 {
		return nil
	}
	rows, err := h.Queries.ListDaemonUpdateSchedules(r.Context(), db.ListDaemonUpdateSchedulesParams{MachineOwnerIds: ownerIDs, DaemonIds: daemonIDs})
	if err != nil {
		return err
	}
	byMachine := make(map[string]db.DaemonUpdateSchedule, len(rows))
	for _, row := range rows {
		byMachine[machineKeyOf(row.MachineOwnerID, row.DaemonID)] = row
	}
	for i, rt := range runtimes {
		key, ok := machineKey(rt)
		if !ok {
			continue
		}
		if row, ok := byMachine[key]; ok {
			s := updateScheduleToResponse(row)
			resp[i].UpdateSchedule = &s
		}
	}
	return nil
}
