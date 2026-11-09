package service

import (
	"sort"
	"time"
)

// Actual-timeline core (CHE-4312 "Timeline contract", slice S1a). Pure: the
// handler loads rows, this decides history, actual start/end, selection and
// clipping.

const (
	TimelineHistoryComplete = "complete"
	TimelineHistoryPartial  = "partial"
	TimelineHistoryNone     = "none"

	TimelineCategoryUnstarted = "unstarted"
	TimelineCategoryStarted   = "started"
	TimelineCategoryDone      = "done"
	TimelineCategoryClosed    = "closed"
	TimelineCategoryUnknown   = "unknown"
)

// TimelineRow is one status_changed row.
type TimelineRow struct {
	ID       string
	At       time.Time
	From, To string
}

// TimelineIssue is the input for one issue. Dates are calendar days at 00:00 UTC.
type TimelineIssue struct {
	Status    string
	CreatedAt time.Time
	StartDate *time.Time
	DueDate   *time.Time
	Rows      []TimelineRow
}

// TimelineSegment is one status stretch clipped to the window. A nil From
// marks the "before records" stretch of a partial history.
type TimelineSegment struct {
	RowID        string     `json:"row_id,omitempty"`
	Status       string     `json:"status"`
	Category     string     `json:"category"`
	From         *time.Time `json:"from"`
	To           *time.Time `json:"to"`
	ClippedStart bool       `json:"clipped_start"`
	ClippedEnd   bool       `json:"clipped_end"`
}

// ActualTimeline is the result for a selected issue.
type ActualTimeline struct {
	History      string
	StartKnown   bool
	ActualStart  *time.Time
	ActualEnd    *time.Time
	PresenceFrom *time.Time
	PresenceTo   *time.Time
	Segments     []TimelineSegment
}

// BuildActualTimeline reports whether the issue is selected for the window
// [from, to) as of asOf. categoryOf maps a status key to a timeline category.
func BuildActualTimeline(issue TimelineIssue, categoryOf func(string) string, from, to, asOf time.Time) (ActualTimeline, bool) {
	rows := append([]TimelineRow(nil), issue.Rows...)
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].At.Equal(rows[j].At) {
			return rows[i].At.Before(rows[j].At)
		}
		return rows[i].ID < rows[j].ID
	})

	tl := ActualTimeline{History: TimelineHistoryNone}
	if len(rows) > 0 {
		tl.History = TimelineHistoryPartial
		if categoryOf(rows[0].From) == TimelineCategoryUnstarted {
			tl.History = TimelineHistoryComplete
		}
	}
	tl.StartKnown = tl.History == TimelineHistoryComplete

	if tl.StartKnown {
		for _, r := range rows {
			if categoryOf(r.To) == TimelineCategoryStarted {
				at := r.At
				tl.ActualStart = &at
				break
			}
		}
	}

	current := categoryOf(issue.Status)
	if current == TimelineCategoryDone || current == TimelineCategoryClosed {
		for i := len(rows) - 1; i >= 0; i-- {
			if c := categoryOf(rows[i].To); c == TimelineCategoryDone || c == TimelineCategoryClosed {
				at := rows[i].At
				tl.ActualEnd = &at
				break
			}
		}
	}

	if tl.History != TimelineHistoryComplete {
		tl.PresenceFrom, tl.PresenceTo = presenceInterval(issue, tl, rows, current, asOf)
	}

	if !selected(issue, tl, rows, from, to, asOf) {
		return ActualTimeline{}, false
	}
	tl.Segments = clipSegments(rows, categoryOf, from, to, asOf)
	return tl, true
}

func presenceInterval(issue TimelineIssue, tl ActualTimeline, rows []TimelineRow, current string, asOf time.Time) (*time.Time, *time.Time) {
	var end time.Time
	switch {
	case tl.ActualEnd != nil:
		end = *tl.ActualEnd
	case current == TimelineCategoryStarted:
		end = asOf
	case tl.History == TimelineHistoryPartial:
		end = rows[len(rows)-1].At
	default:
		return nil, nil
	}
	start := issue.CreatedAt
	return &start, &end
}

func inWindow(t, from, to time.Time) bool { return !t.Before(from) && t.Before(to) }

func overlaps(start, end, from, to time.Time) bool { return start.Before(to) && !end.Before(from) }

func selected(issue TimelineIssue, tl ActualTimeline, rows []TimelineRow, from, to, asOf time.Time) bool {
	if tl.ActualStart != nil {
		end := asOf
		if tl.ActualEnd != nil {
			end = *tl.ActualEnd
		}
		if overlaps(*tl.ActualStart, end, from, to) {
			return true
		}
	}
	for _, r := range rows {
		if inWindow(r.At, from, to) {
			return true
		}
	}
	for _, d := range []*time.Time{issue.StartDate, issue.DueDate} {
		if d != nil && inWindow(*d, from, to) {
			return true
		}
	}
	return tl.PresenceFrom != nil && overlaps(*tl.PresenceFrom, *tl.PresenceTo, from, to)
}

// clipSegments turns rows into status stretches. The stretch opened by the last
// row before from (the seed) is kept; stretches wholly outside the window drop.
func clipSegments(rows []TimelineRow, categoryOf func(string) string, from, to, asOf time.Time) []TimelineSegment {
	var out []TimelineSegment
	add := func(rowID, status string, start *time.Time, end time.Time) {
		if start != nil && !start.Before(to) {
			return
		}
		if end.Before(from) {
			return
		}
		seg := TimelineSegment{RowID: rowID, Status: status, Category: categoryOf(status)}
		if start != nil {
			s := *start
			if s.Before(from) {
				s, seg.ClippedStart = from, true
			}
			seg.From = &s
		}
		e := end
		if e.After(to) {
			e, seg.ClippedEnd = to, true
		}
		seg.To = &e
		out = append(out, seg)
	}
	if len(rows) == 0 {
		return out
	}
	if categoryOf(rows[0].From) != TimelineCategoryUnstarted {
		add("", rows[0].From, nil, rows[0].At)
	}
	for i, r := range rows {
		end := asOf
		if i+1 < len(rows) {
			end = rows[i+1].At
		}
		at := r.At
		add(r.ID, r.To, &at, end)
	}
	return out
}
