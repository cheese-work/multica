package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
)

// POST /api/issues/timeline (CHE-4312 S1a). Selects the workspace issues whose
// recorded status history, markers or presence overlap the window and returns
// their clipped status segments. Scope, filters and paging are S1b.

type issueActualTimelineRequest struct {
	Window struct {
		From time.Time `json:"from"`
		To   time.Time `json:"to"`
	} `json:"window"`
}

type issueActualTimelineEntry struct {
	IssueID      string                    `json:"issue_id"`
	Status       string                    `json:"status"`
	History      string                    `json:"history"`
	StartKnown   bool                      `json:"start_known"`
	ActualStart  *time.Time                `json:"actual_start"`
	ActualEnd    *time.Time                `json:"actual_end"`
	PresenceFrom *time.Time                `json:"presence_from"`
	PresenceTo   *time.Time                `json:"presence_to"`
	StartDate    *string                   `json:"start_date"`
	DueDate      *string                   `json:"due_date"`
	Segments     []service.TimelineSegment `json:"segments"`
}

// Hard bounds on one request. Scope, filters and paging (S1b) are the way to
// stay under them in a large workspace. Variables so tests can lower them.
var (
	timelineMaxIssues = 5000
	timelineMaxRows   = 100000
)

var errTimelineTooLarge = errors.New("timeline result set too large")

func (h *Handler) ListIssueActualTimeline(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if _, ok := h.workspaceMember(w, r, workspaceID); !ok {
		return
	}
	var req issueActualTimelineRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields() // an ignored scope or filter would return the wrong issues
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	from, to := req.Window.From, req.Window.To
	if from.IsZero() || to.IsZero() || !from.Before(to) {
		writeError(w, http.StatusBadRequest, "window.from must be before window.to")
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}
	asOf := time.Now().UTC()

	categories, err := h.loadTimelineCategories(r, wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load statuses")
		return
	}
	// Built-ins resolve without a catalog row, like issuestatus.Category.
	categoryOf := func(status string) string {
		if c, ok := issuestatus.CategoryForBehavior(status); ok {
			return c
		}
		if c, ok := categories[status]; ok {
			return c
		}
		return service.TimelineCategoryUnknown
	}

	issues, order, err := h.loadTimelineIssues(r, wsUUID, from, to)
	if errors.Is(err, errTimelineTooLarge) {
		writeError(w, http.StatusUnprocessableEntity, "too many issues for this window; narrow the window")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load issues")
		return
	}
	if err := h.attachTimelineRows(r, wsUUID, from, to, issues); errors.Is(err, errTimelineTooLarge) {
		writeError(w, http.StatusUnprocessableEntity, "too much status history for this window; narrow the window")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load status history")
		return
	}

	out := make([]issueActualTimelineEntry, 0)
	for _, id := range order {
		issue := issues[id]
		tl, selected := service.BuildActualTimeline(issue.TimelineIssue, categoryOf, from, to, asOf)
		if !selected {
			continue
		}
		out = append(out, issueActualTimelineEntry{
			IssueID: id, Status: issue.Status, History: tl.History, StartKnown: tl.StartKnown,
			ActualStart: tl.ActualStart, ActualEnd: tl.ActualEnd,
			PresenceFrom: tl.PresenceFrom, PresenceTo: tl.PresenceTo,
			StartDate: dateString(issue.StartDate), DueDate: dateString(issue.DueDate),
			Segments: nonNilSegments(tl.Segments),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"as_of":  asOf,
		"window": map[string]time.Time{"from": from, "to": to},
		"issues": out,
	})
}

func nonNilSegments(s []service.TimelineSegment) []service.TimelineSegment {
	if s == nil {
		return []service.TimelineSegment{}
	}
	return s
}

func dateString(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.DateOnly)
	return &s
}

func (h *Handler) loadTimelineCategories(r *http.Request, ws pgtype.UUID) (map[string]string, error) {
	rows, err := h.DB.Query(r.Context(), `SELECT key, issue_status_category(category) FROM issue_status WHERE workspace_id = $1`, ws)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, category string
		if err := rows.Scan(&key, &category); err != nil {
			return nil, err
		}
		out[key] = category
	}
	return out, rows.Err()
}

type timelineIssueRecord struct {
	service.TimelineIssue
}

// timelineCandidates is the SQL prefilter shared by both loaders. An issue
// created at or after the window end has no history, presence or active
// interval in it; only its start/due markers can still select it (rule 3).
const timelineCandidates = `i.workspace_id = $1 AND (i.created_at < $3
	OR (i.start_date::timestamp >= $2 AT TIME ZONE 'UTC' AND i.start_date::timestamp < $3 AT TIME ZONE 'UTC')
	OR (i.due_date::timestamp >= $2 AT TIME ZONE 'UTC' AND i.due_date::timestamp < $3 AT TIME ZONE 'UTC'))`

func (h *Handler) loadTimelineIssues(r *http.Request, ws pgtype.UUID, from, to time.Time) (map[string]*timelineIssueRecord, []string, error) {
	rows, err := h.DB.Query(r.Context(), `SELECT i.id, i.status, i.created_at, i.start_date, i.due_date FROM issue i WHERE `+timelineCandidates+` ORDER BY i.created_at, i.id LIMIT $4`, ws, from, to, timelineMaxIssues+1)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	issues := map[string]*timelineIssueRecord{}
	var order []string
	for rows.Next() {
		var (
			id         pgtype.UUID
			status     string
			createdAt  time.Time
			start, due pgtype.Date
		)
		if err := rows.Scan(&id, &status, &createdAt, &start, &due); err != nil {
			return nil, nil, err
		}
		key := util.UUIDToString(id)
		issues[key] = &timelineIssueRecord{service.TimelineIssue{
			Status: status, CreatedAt: createdAt, StartDate: dateUTC(start), DueDate: dateUTC(due),
		}}
		order = append(order, key)
		if len(order) > timelineMaxIssues {
			return nil, nil, errTimelineTooLarge
		}
	}
	return issues, order, rows.Err()
}

func dateUTC(d pgtype.Date) *time.Time {
	if !d.Valid {
		return nil
	}
	t := time.Date(d.Time.Year(), d.Time.Month(), d.Time.Day(), 0, 0, 0, 0, time.UTC)
	return &t
}

func (h *Handler) attachTimelineRows(r *http.Request, ws pgtype.UUID, from, to time.Time, issues map[string]*timelineIssueRecord) error {
	rows, err := h.DB.Query(r.Context(), `
		SELECT a.issue_id, a.id, a.created_at, COALESCE(a.details->>'from', ''), COALESCE(a.details->>'to', '')
		FROM activity_log a JOIN issue i ON i.id = a.issue_id
		WHERE a.action = 'status_changed' AND `+timelineCandidates+`
		ORDER BY a.issue_id, a.created_at, a.id LIMIT $4`, ws, from, to, timelineMaxRows+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		if n++; n > timelineMaxRows {
			return errTimelineTooLarge
		}
		var (
			issueID, id pgtype.UUID
			at          time.Time
			from, to    string
		)
		if err := rows.Scan(&issueID, &id, &at, &from, &to); err != nil {
			return err
		}
		if to == "" {
			continue
		}
		if issue, ok := issues[util.UUIDToString(issueID)]; ok {
			issue.Rows = append(issue.Rows, service.TimelineRow{ID: util.UUIDToString(id), At: at, From: from, To: to})
		}
	}
	return rows.Err()
}
