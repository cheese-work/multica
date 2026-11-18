package handler

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/logger"
	obsmetrics "github.com/multica-ai/multica/server/internal/metrics"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Read side of the Machine card's History, Audit log and Planned tabs
// (CHE-5709, plan D14). All three are keyed by a runtime and answer for its machine.

const (
	machineHistoryDefaultLimit = 20
	machineHistoryMaxLimit     = 100
	plannedDeployWindow        = 24 * time.Hour
)

// machineAuditKinds are the audit kinds the machine tab shows: the machine
// actions and every schedule change.
var machineAuditKinds = []string{
	"runtime.pause", "runtime.resume", "runtime.update",
	"runtime.update_schedule.set", "runtime.update_schedule.clear",
}

// RecordServerDeploy writes one server_deploy row when commit differs from the
// newest row, and reports whether it wrote. A dev build has no commit and
// writes nothing.
func (h *Handler) RecordServerDeploy(ctx context.Context, commit, version string) (bool, error) {
	if commit == "" || commit == "unknown" {
		return false, nil
	}
	n, err := h.Queries.RecordServerDeploy(ctx, db.RecordServerDeployParams{Commit: commit, Version: version})
	return n > 0, err
}

// requireMachineReadAccess loads the runtime and refuses with 404 (never 403, so
// a runtime's existence does not leak) unless the caller may see it: a member
// who owns or may use the runtime, or the workspace owner.
func (h *Handler) requireMachineReadAccess(w http.ResponseWriter, r *http.Request, adminOverride bool) (db.AgentRuntime, db.Member, bool) {
	runtimeUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "runtimeId"), "runtime_id")
	if !ok {
		return db.AgentRuntime{}, db.Member{}, false
	}
	rt, err := h.getAgentRuntime(r.Context(), obsmetrics.RuntimeLookupSourceRuntimeAPI, runtimeUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "runtime not found")
		return rt, db.Member{}, false
	}
	member, ok := h.requireWorkspaceMember(w, r, uuidToString(rt.WorkspaceID), "runtime not found")
	if !ok {
		return rt, member, false
	}
	isAdmin := member.Role == "owner" || member.Role == "admin"
	if !(adminOverride && isAdmin) && !canPauseDaemon(member, rt) && !canUseRuntimeForAgent(member, rt) {
		writeError(w, http.StatusNotFound, "runtime not found")
		return rt, member, false
	}
	if !rt.DaemonID.Valid {
		writeError(w, http.StatusBadRequest, "runtime is not hosted by a machine daemon")
		return rt, member, false
	}
	return rt, member, true
}

// MachineCycleResponse is one update cycle of a machine. Outcome is null while
// the cycle is open; it is stored text, so the client treats an unknown value as `unknown`.
type MachineCycleResponse struct {
	ID            string   `json:"id"`
	State         string   `json:"state"`
	Outcome       *string  `json:"outcome"`
	OutcomeDetail *string  `json:"outcome_detail"`
	OutcomeSource *string  `json:"outcome_source"`
	FromCommit    *string  `json:"from_commit"`
	TargetCommit  *string  `json:"target_commit"`
	Notes         []string `json:"notes"`
	LiveRunsAtEnd *int32   `json:"live_runs_at_end"`
	StartedAt     *string  `json:"started_at"`
	DeadlineAt    *string  `json:"deadline_at"`
	FinishedAt    *string  `json:"finished_at"`
}

type MachineHistoryPage struct {
	Cycles     []MachineCycleResponse `json:"cycles"`
	NextCursor *string                `json:"next_cursor"`
}

// GetMachineHistory answers GET /api/runtimes/{runtimeId}/machine/history: the
// machine's update cycles, newest first. Query: limit, cursor.
func (h *Handler) GetMachineHistory(w http.ResponseWriter, r *http.Request) {
	rt, _, ok := h.requireMachineReadAccess(w, r, false)
	if !ok {
		return
	}
	limit, cursorAt, cursorID, ok := parseMachinePage(w, r, machineHistoryDefaultLimit, machineHistoryMaxLimit)
	if !ok {
		return
	}
	rows, err := h.Queries.ListMachineUpdateCycles(r.Context(), db.ListMachineUpdateCyclesParams{
		MachineOwnerID: machineOwnerID(rt), DaemonID: rt.DaemonID.String,
		CursorAt: cursorAt, CursorID: cursorID, RowLimit: int32(limit + 1),
	})
	if err != nil {
		slog.Error("list machine update cycles failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to load update history")
		return
	}
	page := MachineHistoryPage{Cycles: []MachineCycleResponse{}}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1]
		c := encodeAuditCursor(last.StartedAt.Time, last.ID)
		page.NextCursor = &c
	}
	for _, c := range rows {
		notes := c.Notes
		if notes == nil {
			notes = []string{}
		}
		page.Cycles = append(page.Cycles, MachineCycleResponse{
			ID: uuidToString(c.ID), State: c.State, Outcome: textToPtr(c.Outcome), OutcomeDetail: textToPtr(c.OutcomeDetail),
			OutcomeSource: textToPtr(c.OutcomeSource), FromCommit: textToPtr(c.FromCommit), TargetCommit: textToPtr(c.TargetCommit),
			Notes: notes, LiveRunsAtEnd: int4ToPtr(c.LiveRunsAtEnd), StartedAt: timestampToPtr(c.StartedAt),
			DeadlineAt: timestampToPtr(c.DeadlineAt), FinishedAt: timestampToPtr(c.FinishedAt),
		})
	}
	writeJSON(w, http.StatusOK, page)
}

// parseMachinePage reads limit and cursor. The cursor is the audit reader's
// (time, id) cursor.
func parseMachinePage(w http.ResponseWriter, r *http.Request, def, max int) (limit int, at pgtype.Timestamptz, id pgtype.UUID, ok bool) {
	limit = def
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return 0, at, id, false
		}
		limit = min(n, max)
	}
	if s := r.URL.Query().Get("cursor"); s != "" {
		t, cid, err := decodeAuditCursor(s)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return 0, at, id, false
		}
		at, id = pgtype.Timestamptz{Time: t, Valid: true}, cid
	}
	return limit, at, id, true
}

// GetMachineAudit answers GET /api/runtimes/{runtimeId}/machine/audit: the
// workspace audit events about this machine's runtimes, newest first. Same
// access rules as the workspace audit reader (human owner or admin); a caller
// without access gets 404. Query: limit, cursor.
func (h *Handler) GetMachineAudit(w http.ResponseWriter, r *http.Request) {
	if isMachineCredentialActor(r) {
		writeError(w, http.StatusForbidden, "this endpoint is only available to human actors")
		return
	}
	// An owner or admin reads the audit of any runtime in the workspace, like the
	// workspace audit reader; everyone else gets 404 from the access check.
	rt, member, ok := h.requireMachineReadAccess(w, r, true)
	if !ok {
		return
	}
	if member.Role != "owner" && member.Role != "admin" {
		writeError(w, http.StatusNotFound, "runtime not found")
		return
	}
	limit, cursorAt, cursorID, ok := parseMachinePage(w, r, auditReadDefaultLimit, auditReadMaxLimit)
	if !ok {
		return
	}
	ids, err := h.Queries.ListMachineRuntimeIDsInWorkspace(r.Context(), db.ListMachineRuntimeIDsInWorkspaceParams{
		WorkspaceID: rt.WorkspaceID, MachineOwnerID: machineOwnerID(rt), DaemonID: rt.DaemonID,
	})
	if err == nil {
		var found []db.ListMachineAuditEventsRow
		found, err = h.Queries.ListMachineAuditEvents(r.Context(), db.ListMachineAuditEventsParams{
			WorkspaceID: rt.WorkspaceID, RuntimeIds: ids, Kinds: machineAuditKinds,
			CursorAt: cursorAt, CursorID: cursorID, RowLimit: int32(limit + 1),
		})
		if err == nil {
			rows := make([]db.ListWorkspaceAuditEventsRow, len(found)) // the same columns, a separate sqlc row type
			for i, row := range found {
				rows[i] = db.ListWorkspaceAuditEventsRow(row)
			}
			err = h.writeAuditPage(w, r, rt.WorkspaceID, rows, limit)
			if err == nil {
				return
			}
		}
	}
	slog.Error("machine audit failed", append(logger.RequestAttrs(r), "error", err)...)
	writeError(w, http.StatusInternalServerError, "failed to load audit events")
}

// MachineServerDeploy is one server start on a new commit.
type MachineServerDeploy struct {
	Commit    string  `json:"commit"`
	Version   string  `json:"version"`
	StartedAt *string `json:"started_at"`
}

type MachinePlanned struct {
	Timezone      string                `json:"timezone"`
	NextRuns      []string              `json:"next_runs"`
	ServerDeploys []MachineServerDeploy `json:"server_deploys"`
}

// GetMachinePlanned answers GET /api/runtimes/{runtimeId}/machine/planned: the
// next 3 cron times of an enabled schedule, and the server deploys of the last 24 h.
func (h *Handler) GetMachinePlanned(w http.ResponseWriter, r *http.Request) {
	rt, _, ok := h.requireMachineReadAccess(w, r, false)
	if !ok {
		return
	}
	s, ok := h.loadUpdateSchedule(w, r, rt)
	if !ok {
		return
	}
	resp := MachinePlanned{Timezone: defaultUpdateTimezone, NextRuns: []string{}, ServerDeploys: []MachineServerDeploy{}}
	if s != nil {
		resp.Timezone = s.Timezone
		if s.Enabled {
			resp.NextRuns = withNextRuns(updateScheduleToResponse(*s)).NextRuns
		}
	}
	deploys, err := h.Queries.ListServerDeploysSince(r.Context(), pgtype.Timestamptz{Time: time.Now().Add(-plannedDeployWindow), Valid: true})
	if err != nil {
		slog.Error("list server deploys failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to load planned times")
		return
	}
	for _, d := range deploys {
		resp.ServerDeploys = append(resp.ServerDeploys, MachineServerDeploy{Commit: d.Commit, Version: d.Version, StartedAt: timestampToPtr(d.StartedAt)})
	}
	writeJSON(w, http.StatusOK, resp)
}
