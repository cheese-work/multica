package handler

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
)

// POST /api/issues/timeline (CHE-4312 S1a/S1b). Takes the issues that match
// the table-query scope and filters, selects those whose recorded status
// history, markers or presence overlap the window, and returns one keyset page
// of their clipped status segments.

type issueActualTimelineRequest struct {
	Scope   issueTableScope          `json:"scope"`
	Filters issueTableFiltersRequest `json:"filters"`
	Window  *struct {
		From *time.Time `json:"from"`
		To   *time.Time `json:"to"`
	} `json:"window"`
	Page issueTablePageRequest `json:"page"`
}

const (
	timelineDefaultWindow = 30 * 24 * time.Hour
	timelineDefaultLimit  = 200
	timelineMaxLimit      = 500
)

// timelinePageCursor is the opaque position after the last returned issue. It
// carries as_of and the window so every page of a chain evaluates the same
// instant, and the scope+filters fingerprint so a foreign cursor is refused.
type timelinePageCursor struct {
	Version     int       `json:"v"`
	Fingerprint string    `json:"query"`
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
	AsOf        time.Time `json:"as_of"`
	SortAt      time.Time `json:"sort_at"`
	IssueID     string    `json:"issue_id"`
}

func decodeTimelineCursor(raw *string) (*timelinePageCursor, bool) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, true
	}
	decoded, err := base64.RawURLEncoding.DecodeString(*raw)
	var c timelinePageCursor
	if err != nil || json.Unmarshal(decoded, &c) != nil || c.Version != 1 || c.IssueID == "" || c.SortAt.IsZero() || c.AsOf.IsZero() || c.From.IsZero() || c.To.IsZero() {
		return nil, false
	}
	if _, err := util.ParseUUID(c.IssueID); err != nil {
		return nil, false
	}
	return &c, true
}

func encodeTimelineCursor(c timelinePageCursor) string {
	encoded, _ := json.Marshal(c) // plain struct of strings and times: cannot fail
	return base64.RawURLEncoding.EncodeToString(encoded)
}

type issueActualTimelineEntry struct {
	sortAt       time.Time
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

// Hard bounds on one request. Issues load in keyset batches, so memory holds
// one batch plus the selected result, never the whole workspace. The cap counts
// selected issues, so a window with few matches answers in a large workspace.
// Scope, filters and paging (S1b) are the way to stay under the caps. Variables
// so tests can lower them.
const timelineMaxBody = 64 << 10

var (
	timelineMaxIssues = 5000   // selected issues per response
	timelineMaxRows   = 200000 // status rows per batch
	timelineBatchSize = 1000   // candidate issues per batch

	// Request-wide work budget across all batches, so a window that selects
	// nothing still cannot scan the whole workspace on every request.
	timelineMaxScanned   = 100000  // candidate issues scanned per request
	timelineMaxTotalRows = 1000000 // status rows read per request
)

var errTimelineTooLarge = errors.New("timeline result set too large")

func (h *Handler) ListIssueActualTimeline(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if _, ok := h.workspaceMember(w, r, workspaceID); !ok {
		return
	}
	var req issueActualTimelineRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, timelineMaxBody))
	dec.DisallowUnknownFields() // an ignored scope or filter would return the wrong issues
	if err := dec.Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	limit := req.Page.Limit
	if limit == 0 {
		limit = timelineDefaultLimit
	}
	if limit < 1 || limit > timelineMaxLimit {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("page.limit must be between 1 and %d", timelineMaxLimit))
		return
	}
	switch req.Scope.Kind {
	case "":
		req.Scope.Kind = "workspace"
	case "workspace", "project", "my":
	default: // assignee, creator, team: no timeline view uses them
		writeError(w, http.StatusBadRequest, "invalid scope.kind")
		return
	}
	cursor, ok := decodeTimelineCursor(req.Page.Cursor)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid cursor")
		return
	}

	asOf := time.Now().UTC()
	var from, to time.Time
	switch {
	case req.Window != nil && (req.Window.From == nil || req.Window.To == nil):
		writeError(w, http.StatusBadRequest, "window.from and window.to are both required")
		return
	case req.Window != nil:
		from, to = *req.Window.From, *req.Window.To
	case cursor != nil: // a default window follows its chain, not the clock
		from, to = cursor.From, cursor.To
	default:
		from, to = asOf.Add(-timelineDefaultWindow), asOf
	}
	if !from.Before(to) {
		writeError(w, http.StatusBadRequest, "window.from must be before window.to")
		return
	}
	from, to = from.UTC(), to.UTC()

	compiled, ok := h.compileIssueTableQuery(w, r, issueTableQuerySpec{Scope: req.Scope, Filters: req.Filters})
	if !ok {
		return
	}
	// The status filter must read the as-of status, which is only known after
	// the history rows load. The SQL leaves it out; the fingerprint keeps it.
	sqlQuery := compiled
	var statusFilter map[string]bool
	if len(req.Filters.Statuses) > 0 {
		statusFilter = map[string]bool{}
		for _, status := range req.Filters.Statuses {
			statusFilter[status] = true
		}
		noStatus := req.Filters
		noStatus.Statuses = nil
		if sqlQuery, ok = h.compileIssueTableQuery(w, r, issueTableQuerySpec{Scope: req.Scope, Filters: noStatus}); !ok {
			return
		}
	}
	if req.Scope.Kind == "project" {
		var exists bool
		projectID, _ := util.ParseUUID(req.Scope.ProjectID) // validated by compileIssueTableQuery
		if err := h.DB.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM project WHERE id = $1 AND workspace_id = $2)`, projectID, compiled.workspaceID).Scan(&exists); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to load project")
			return
		}
		if !exists {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
	}
	if cursor != nil {
		if cursor.Fingerprint != compiled.fingerprint || !cursor.From.Equal(from) || !cursor.To.Equal(to) {
			writeError(w, http.StatusBadRequest, "cursor does not belong to this timeline query")
			return
		}
		asOf = cursor.AsOf.UTC()
	}

	categories, err := h.loadTimelineCategories(r, compiled.workspaceID)
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

	selectedAll := make([]issueActualTimelineEntry, 0)
	var scan *timelineScan
	scanned, totalRows := 0, 0
	for {
		batch, order, next, err := h.loadTimelineBatch(r, sqlQuery, from, to, asOf, scan)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to load issues")
			return
		}
		if len(order) == 0 {
			break
		}
		scan = next
		if scanned += len(order); scanned > timelineMaxScanned {
			writeError(w, http.StatusUnprocessableEntity, "too many issues scanned for this window; narrow the window")
			return
		}
		rowsRead, err := h.attachTimelineRows(r, order, batch, asOf, timelineMaxTotalRows-totalRows)
		totalRows += rowsRead
		if errors.Is(err, errTimelineTooLarge) {
			writeError(w, http.StatusUnprocessableEntity, "too much status history for this window; narrow the window")
			return
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to load status history")
			return
		}
		for _, uuid := range order {
			id := util.UUIDToString(uuid)
			issue := batch[id]
			if statusFilter != nil && !statusFilter[issue.Status] { // issue.Status is the as-of status here
				continue
			}
			tl, selected := service.BuildActualTimeline(issue.TimelineIssue, categoryOf, from, to, asOf)
			if !selected {
				continue
			}
			sortAt := timelineSortAt(issue.TimelineIssue, tl)
			if cursor != nil && !timelineAfter(sortAt, id, cursor) {
				continue
			}
			if len(selectedAll) >= timelineMaxIssues {
				writeError(w, http.StatusUnprocessableEntity, "too many issues for this window; narrow the window")
				return
			}
			selectedAll = append(selectedAll, issueActualTimelineEntry{
				sortAt:  sortAt,
				IssueID: id, Status: issue.Status, History: tl.History, StartKnown: tl.StartKnown,
				ActualStart: tl.ActualStart, ActualEnd: tl.ActualEnd,
				PresenceFrom: tl.PresenceFrom, PresenceTo: tl.PresenceTo,
				StartDate: dateString(issue.StartDate), DueDate: dateString(issue.DueDate),
				Segments: nonNilSegments(tl.Segments),
			})
		}
		if len(order) < timelineBatchSize {
			break
		}
	}

	sort.Slice(selectedAll, func(i, j int) bool {
		a, b := selectedAll[i], selectedAll[j]
		if !a.sortAt.Equal(b.sortAt) {
			return a.sortAt.Before(b.sortAt)
		}
		return a.IssueID < b.IssueID
	})
	var nextCursor *string
	if len(selectedAll) > limit {
		selectedAll = selectedAll[:limit]
		last := selectedAll[limit-1]
		c := encodeTimelineCursor(timelinePageCursor{
			Version: 1, Fingerprint: compiled.fingerprint, From: from, To: to, AsOf: asOf,
			SortAt: last.sortAt, IssueID: last.IssueID,
		})
		nextCursor = &c
	}
	pageIDs := make([]string, len(selectedAll))
	for i, entry := range selectedAll {
		pageIDs[i] = entry.IssueID
	}
	wakeupEvents, ok := h.loadTimelineWakeupEvents(w, r, workspaceID, pageIDs, from, to, asOf)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"as_of":         asOf,
		"window":        map[string]time.Time{"from": from, "to": to},
		"issues":        selectedAll,
		"wakeup_events": wakeupEvents,
		"next_cursor":   nextCursor,
	})
}

// timelineSortAt is COALESCE(actual_start, first row time, start_date,
// due_date, created_at). Rows are already cut at as_of.
func timelineSortAt(issue service.TimelineIssue, tl service.ActualTimeline) time.Time {
	if tl.ActualStart != nil {
		return *tl.ActualStart
	}
	if len(issue.Rows) > 0 {
		first := issue.Rows[0].At
		for _, row := range issue.Rows[1:] {
			if row.At.Before(first) {
				first = row.At
			}
		}
		return first
	}
	for _, d := range []*time.Time{issue.StartDate, issue.DueDate} {
		if d != nil {
			return *d
		}
	}
	return issue.CreatedAt
}

func timelineAfter(sortAt time.Time, id string, c *timelinePageCursor) bool {
	if !sortAt.Equal(c.SortAt) {
		return sortAt.After(c.SortAt)
	}
	return id > c.IssueID
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

// timelineCandidates is the SQL prefilter on top of the compiled scope+filters
// WHERE. An issue created at or after the window end has no history, presence
// or active interval in it; only its start/due markers can still select it
// (rule 3). An issue created after as_of does not exist for this page chain.
// Placeholders: %[1]s from, %[2]s to, %[3]s as_of.
const timelineCandidates = `i.created_at <= %[3]s AND (i.created_at < %[2]s
	OR (i.start_date::timestamp >= %[1]s AT TIME ZONE 'UTC' AND i.start_date::timestamp < %[2]s AT TIME ZONE 'UTC')
	OR (i.due_date::timestamp >= %[1]s AT TIME ZONE 'UTC' AND i.due_date::timestamp < %[2]s AT TIME ZONE 'UTC'))`

// timelineScan is the keyset position after the last loaded issue.
type timelineScan struct {
	createdAt time.Time
	id        pgtype.UUID
}

func (h *Handler) loadTimelineBatch(r *http.Request, q issueTableSQL, from, to, asOf time.Time, after *timelineScan) (map[string]*timelineIssueRecord, []pgtype.UUID, *timelineScan, error) {
	var afterAt *time.Time
	var afterID pgtype.UUID
	if after != nil {
		afterAt, afterID = &after.createdAt, after.id
	}
	args := append(append([]any(nil), q.args...), from, to, asOf, afterAt, afterID, timelineBatchSize)
	n := len(q.args)
	ph := func(i int) string { return fmt.Sprintf("$%d", n+i) }
	rows, err := h.DB.Query(r.Context(), `SELECT i.id, i.status, i.created_at, i.start_date, i.due_date FROM issue i
		WHERE `+q.where+` AND `+fmt.Sprintf(timelineCandidates, ph(1), ph(2), ph(3))+`
		AND (`+ph(4)+`::timestamptz IS NULL OR (i.created_at, i.id) > (`+ph(4)+`::timestamptz, `+ph(5)+`::uuid))
		ORDER BY i.created_at, i.id LIMIT `+ph(6), args...)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	issues := map[string]*timelineIssueRecord{}
	var order []pgtype.UUID
	var last *timelineScan
	for rows.Next() {
		var (
			id         pgtype.UUID
			status     string
			createdAt  time.Time
			start, due pgtype.Date
		)
		if err := rows.Scan(&id, &status, &createdAt, &start, &due); err != nil {
			return nil, nil, nil, err
		}
		issues[util.UUIDToString(id)] = &timelineIssueRecord{service.TimelineIssue{
			Status: status, CreatedAt: createdAt, StartDate: dateUTC(start), DueDate: dateUTC(due),
		}}
		order = append(order, id)
		last = &timelineScan{createdAt: createdAt, id: id}
	}
	return issues, order, last, rows.Err()
}

func dateUTC(d pgtype.Date) *time.Time {
	if !d.Valid {
		return nil
	}
	t := time.Date(d.Time.Year(), d.Time.Month(), d.Time.Day(), 0, 0, 0, 0, time.UTC)
	return &t
}

// attachTimelineRows reads one batch's status rows and returns how many it read.
// It fails with errTimelineTooLarge past the per-batch cap or the remaining
// request budget. Rows after asOf are dropped and the issue status reverts to
// the status before the first of them, so a change made while paging cannot
// move an issue.
func (h *Handler) attachTimelineRows(r *http.Request, ids []pgtype.UUID, issues map[string]*timelineIssueRecord, asOf time.Time, budget int) (int, error) {
	limit := min(timelineMaxRows, budget)
	rows, err := h.DB.Query(r.Context(), `
		SELECT issue_id, id, created_at, COALESCE(details->>'from', ''), COALESCE(details->>'to', '')
		FROM activity_log
		WHERE action = 'status_changed' AND issue_id = ANY($1)
		ORDER BY issue_id, created_at, id LIMIT $2`, ids, limit+1)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	reverted := map[string]bool{}
	for rows.Next() {
		if n++; n > limit {
			return n, errTimelineTooLarge
		}
		var (
			issueID, id pgtype.UUID
			at          time.Time
			from, to    string
		)
		if err := rows.Scan(&issueID, &id, &at, &from, &to); err != nil {
			return n, err
		}
		if to == "" {
			continue
		}
		key := util.UUIDToString(issueID)
		issue, ok := issues[key]
		if !ok {
			continue
		}
		if at.After(asOf) {
			if !reverted[key] { // rows are ordered, so the first later row names the as-of status
				reverted[key] = true
				issue.Status = from
			}
			continue
		}
		issue.Rows = append(issue.Rows, service.TimelineRow{ID: util.UUIDToString(id), At: at, From: from, To: to})
	}
	return n, rows.Err()
}
