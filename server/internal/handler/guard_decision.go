package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/guard"
	obsmetrics "github.com/multica-ai/multica/server/internal/metrics"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Tool-guard decisions (CHE-5207). The daemon uploads a run's decision log when
// the run ends; the Guard settings page and `multica guard log|stats` read the
// same rows, so both show the same numbers.

const (
	guardDecisionMaxUpload = 500 // one run's log; the daemon sends the first 500
	guardDecisionMaxLine   = 500
	guardStatsMaxDays      = 90
	guardListMaxLimit      = 500
)

var guardActions = map[string]bool{
	guard.KindPass: true, guard.KindFixed: true, guard.KindRefused: true,
	guard.KindPluginError: true, guard.KindShadowHit: true,
}

// GuardDecisionUpload is one line of the guard's JSON decision log.
type GuardDecisionUpload struct {
	Seq        int32  `json:"seq"`
	Time       string `json:"time"`
	Plugin     string `json:"plugin"`
	Action     string `json:"action"`
	Tool       string `json:"tool"`
	Subcommand string `json:"subcommand"`
	Line       string `json:"line"`
	Issue      string `json:"issue"`
	Note       string `json:"note"`
}

func clipGuardText(s string) string {
	if r := []rune(s); len(r) > guardDecisionMaxLine {
		return string(r[:guardDecisionMaxLine])
	}
	return s
}

// ReportGuardDecisions stores one run's decision log. (task, seq) is unique, so
// a retried upload adds nothing.
func (h *Handler) ReportGuardDecisions(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	task, workspaceID, ok := h.requireDaemonTaskAccessWithWorkspace(w, r, taskID)
	if !ok {
		return
	}
	var req struct {
		Decisions []GuardDecisionUpload `json:"decisions"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Decisions) > guardDecisionMaxUpload {
		writeError(w, http.StatusBadRequest, "too many decisions in one upload")
		return
	}
	times := make([]time.Time, len(req.Decisions))
	for i, d := range req.Decisions {
		at, err := time.Parse(time.RFC3339, d.Time)
		if err != nil || d.Seq < 0 || d.Plugin == "" || !guardActions[d.Action] {
			writeError(w, http.StatusBadRequest, "invalid decision at index "+strconv.Itoa(i))
			return
		}
		times[i] = at
	}
	var runtime string
	if len(req.Decisions) > 0 {
		if rt, err := h.getAgentRuntime(r.Context(), obsmetrics.RuntimeLookupSourceTask, task.RuntimeID); err == nil {
			runtime = normalizeProvider(rt.Provider)
		} else {
			slog.Warn("load runtime provider for guard decisions failed", "task_id", taskID, "error", err)
		}
	}
	for i, d := range req.Decisions {
		if err := h.Queries.InsertGuardDecision(r.Context(), db.InsertGuardDecisionParams{
			WorkspaceID: parseUUID(workspaceID),
			TaskID:      task.ID,
			Seq:         d.Seq,
			DecidedAt:   pgtype.Timestamptz{Time: times[i], Valid: true},
			Plugin:      clipGuardText(d.Plugin),
			Action:      d.Action,
			Tool:        clipGuardText(d.Tool),
			Subcommand:  clipGuardText(d.Subcommand),
			Line:        clipGuardText(d.Line),
			IssueKey:    clipGuardText(d.Issue),
			Note:        clipGuardText(d.Note),
			Runtime:     runtime,
		}); err != nil {
			slog.Warn("store guard decision failed", "task_id", taskID, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to store guard decisions")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// GuardDecisionResponse is one stored decision.
type GuardDecisionResponse struct {
	ID         string `json:"id"`
	TaskID     string `json:"task_id"`
	Time       string `json:"time"`
	Plugin     string `json:"plugin"`
	Action     string `json:"action"`
	Tool       string `json:"tool"`
	Subcommand string `json:"subcommand"`
	Line       string `json:"line"`
	Issue      string `json:"issue"`
	Note       string `json:"note"`
	Runtime    string `json:"runtime"`
}

// ListGuardDecisions returns the newest decisions, optionally for one plugin or
// action. ?since takes a day count ("7"), a duration ("24h", "7d") or an RFC
// 3339 time; the default is 7 days.
func (h *Handler) ListGuardDecisions(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if _, ok := h.workspaceMember(w, r, workspaceID); !ok {
		return
	}
	q := r.URL.Query()
	since, ok := parseGuardSince(q.Get("since"), time.Now())
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid since")
		return
	}
	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > guardListMaxLimit {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = n
	}
	params := db.ListGuardDecisionsParams{
		WorkspaceID: parseUUID(workspaceID),
		Since:       pgtype.Timestamptz{Time: since, Valid: true},
		RowLimit:    int32(limit),
	}
	if p := q.Get("plugin"); p != "" {
		params.Plugin = pgtype.Text{String: p, Valid: true}
	}
	if a := q.Get("action"); a != "" {
		if !guardActions[a] {
			writeError(w, http.StatusBadRequest, "invalid action")
			return
		}
		params.Action = pgtype.Text{String: a, Valid: true}
	}
	rows, err := h.Queries.ListGuardDecisions(r.Context(), params)
	if err != nil {
		slog.Error("list guard decisions failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list guard decisions")
		return
	}
	out := make([]GuardDecisionResponse, len(rows))
	for i, d := range rows {
		out[i] = GuardDecisionResponse{
			ID: uuidToString(d.ID), TaskID: uuidToString(d.TaskID), Time: d.DecidedAt.Time.UTC().Format(time.RFC3339),
			Plugin: d.Plugin, Action: d.Action, Tool: d.Tool, Subcommand: d.Subcommand, Line: d.Line,
			Issue: d.IssueKey, Note: d.Note, Runtime: d.Runtime,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"decisions": out})
}

func parseGuardSince(v string, now time.Time) (time.Time, bool) {
	if v == "" {
		return now.AddDate(0, 0, -7), true
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, true
	}
	if n, ok := strings.CutSuffix(v, "d"); ok {
		d, err := strconv.Atoi(n)
		return now.AddDate(0, 0, -d), err == nil && d > 0 && d <= guardStatsMaxDays
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 && d <= guardStatsMaxDays*24*time.Hour {
		return now.Add(-d), true
	}
	if d, err := strconv.Atoi(v); err == nil && d > 0 && d <= guardStatsMaxDays {
		return now.AddDate(0, 0, -d), true
	}
	return time.Time{}, false
}

// GuardDayCounts is one day of one plugin's hits.
type GuardDayCounts struct {
	Day     string `json:"day"`
	Fixed   int64  `json:"fixed"`
	Refused int64  `json:"refused"`
	Errors  int64  `json:"errors"`
}

// GuardPluginStats is one plugin's counts over the window. Passes and shadow
// hits are kept apart from the three outcomes the page shows.
type GuardPluginStats struct {
	Plugin     string           `json:"plugin"`
	Fixed      int64            `json:"fixed"`
	Refused    int64            `json:"refused"`
	Errors     int64            `json:"errors"`
	ShadowHits int64            `json:"shadow_hits"`
	Passes     int64            `json:"passes"`
	Daily      []GuardDayCounts `json:"daily"`
}

// GetGuardStats returns per-plugin counts over the last ?days (default 7) days
// of the viewer's time zone, with one dense row per day.
func (h *Handler) GetGuardStats(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if _, ok := h.workspaceMember(w, r, workspaceID); !ok {
		return
	}
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > guardStatsMaxDays {
			writeError(w, http.StatusBadRequest, "invalid days")
			return
		}
		days = n
	}
	tz := h.resolveViewingTZ(r)
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc, tz = time.UTC, "UTC"
	}
	today := time.Now().In(loc)
	first := time.Date(today.Year(), today.Month(), today.Day()-(days-1), 0, 0, 0, 0, loc)
	rows, err := h.Queries.GuardDecisionDaily(r.Context(), db.GuardDecisionDailyParams{
		WorkspaceID: parseUUID(workspaceID),
		Tz:          tz,
		Since:       pgtype.Timestamptz{Time: first, Valid: true},
	})
	if err != nil {
		slog.Error("guard stats failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load guard stats")
		return
	}
	byPlugin := map[string]*GuardPluginStats{}
	index := map[string]map[string]int{}
	dayList := make([]string, days)
	for i := range dayList {
		dayList[i] = first.AddDate(0, 0, i).Format("2006-01-02")
	}
	for _, row := range rows {
		p := byPlugin[row.Plugin]
		if p == nil {
			p = &GuardPluginStats{Plugin: row.Plugin, Daily: make([]GuardDayCounts, days)}
			index[row.Plugin] = map[string]int{}
			for i, d := range dayList {
				p.Daily[i].Day = d
				index[row.Plugin][d] = i
			}
			byPlugin[row.Plugin] = p
		}
		i, ok := index[row.Plugin][row.Day.Time.Format("2006-01-02")]
		if !ok {
			continue
		}
		switch row.Action {
		case guard.KindFixed:
			p.Fixed += row.Hits
			p.Daily[i].Fixed += row.Hits
		case guard.KindRefused:
			p.Refused += row.Hits
			p.Daily[i].Refused += row.Hits
		case guard.KindPluginError:
			p.Errors += row.Hits
			p.Daily[i].Errors += row.Hits
		case guard.KindShadowHit:
			p.ShadowHits += row.Hits
		case guard.KindPass:
			p.Passes += row.Hits
		}
	}
	plugins := make([]GuardPluginStats, 0, len(byPlugin))
	for _, p := range byPlugin {
		plugins = append(plugins, *p)
	}
	sort.Slice(plugins, func(i, j int) bool { return plugins[i].Plugin < plugins[j].Plugin })
	writeJSON(w, http.StatusOK, map[string]any{"days": days, "tz": tz, "since": dayList[0], "plugins": plugins})
}
