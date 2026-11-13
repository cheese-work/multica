package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/dbid"
)

// CHE-4340 (S1a of CHE-4312): the actual-timeline core. Every fixture asserts
// endpoint membership: the issue is, or is not, in the POST /api/issues/timeline
// response for a window.

type tlRow struct {
	ago      time.Duration // age of the status_changed row
	from, to string
	id       string // optional fixed row ID, for tie-order tests
}

type tlIssue struct {
	status  string
	created time.Duration // age of created_at
	rows    []tlRow
	start   *time.Duration // start_date, as an age
	due     *time.Duration // due_date, as an age
}

func ago(d time.Duration) *time.Duration { return &d }

const day = 24 * time.Hour

func seedTLIssue(t *testing.T, spec tlIssue) string {
	t.Helper()
	ctx := context.Background()
	issueID := createIssueForTimeline(t, fmt.Sprintf("actual timeline fixture %d", time.Now().UnixNano()))
	now := time.Now()
	if _, err := testPool.Exec(ctx, `UPDATE issue SET status=$2, created_at=$3 WHERE id=$1`, issueID, spec.status, now.Add(-spec.created)); err != nil {
		t.Fatal(err)
	}
	if spec.start != nil {
		if _, err := testPool.Exec(ctx, `UPDATE issue SET start_date=$2 WHERE id=$1`, issueID, now.Add(-*spec.start)); err != nil {
			t.Fatal(err)
		}
	}
	if spec.due != nil {
		if _, err := testPool.Exec(ctx, `UPDATE issue SET due_date=$2 WHERE id=$1`, issueID, now.Add(-*spec.due)); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range spec.rows {
		id := row.id
		if id == "" {
			id = dbid.NewV7().String()
		}
		details, _ := json.Marshal(map[string]any{"from": row.from, "to": row.to})
		if _, err := testPool.Exec(ctx, `INSERT INTO activity_log (id, workspace_id, issue_id, actor_type, action, details, created_at) VALUES ($1,$2,$3,'system','status_changed',$4,$5)`,
			id, testWorkspaceID, issueID, details, now.Add(-row.ago)); err != nil {
			t.Fatal(err)
		}
	}
	return issueID
}

type tlSegment struct {
	Status       string     `json:"status"`
	Category     string     `json:"category"`
	From         *time.Time `json:"from"`
	To           *time.Time `json:"to"`
	ClippedStart bool       `json:"clipped_start"`
	ClippedEnd   bool       `json:"clipped_end"`
}

type tlResult struct {
	IssueID     string      `json:"issue_id"`
	History     string      `json:"history"`
	StartKnown  bool        `json:"start_known"`
	ActualStart *time.Time  `json:"actual_start"`
	ActualEnd   *time.Time  `json:"actual_end"`
	StartDate   *string     `json:"start_date"`
	DueDate     *string     `json:"due_date"`
	Segments    []tlSegment `json:"segments"`
}

func fetchActualTimeline(t *testing.T, from, to time.Duration) map[string]tlResult {
	t.Helper()
	now := time.Now()
	body := map[string]any{"window": map[string]any{
		"from": now.Add(-from).UTC().Format(time.RFC3339Nano),
		"to":   now.Add(-to).UTC().Format(time.RFC3339Nano),
	}}
	w := httptest.NewRecorder()
	testHandler.ListIssueActualTimeline(w, newRequest(http.MethodPost, "/api/issues/timeline", body))
	if w.Code != http.StatusOK {
		t.Fatalf("timeline: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Issues []tlResult `json:"issues"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	out := map[string]tlResult{}
	for _, issue := range resp.Issues {
		out[issue.IssueID] = issue
	}
	return out
}

func wantIn(t *testing.T, got map[string]tlResult, id string) tlResult {
	t.Helper()
	r, ok := got[id]
	if !ok {
		t.Fatalf("issue %s missing from timeline response", id)
	}
	return r
}

func wantAbsent(t *testing.T, got map[string]tlResult, id string) {
	t.Helper()
	if _, ok := got[id]; ok {
		t.Fatalf("issue %s present in timeline response, want absent", id)
	}
}

func near(t *testing.T, label string, got *time.Time, wantAgo time.Duration) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s = nil, want ~%s ago", label, wantAgo)
	}
	if d := time.Since(*got) - wantAgo; d > time.Minute || d < -time.Minute {
		t.Fatalf("%s = %s, want ~%s ago", label, got, wantAgo)
	}
}

// T1: old active issue, started 60 days ago, no transition in the window.
func TestTimelineT1OldActiveIssue(t *testing.T) {
	id := seedTLIssue(t, tlIssue{status: "in_progress", created: 70 * day, rows: []tlRow{
		{ago: 60 * day, from: "todo", to: "in_progress"},
	}})
	got := fetchActualTimeline(t, 30*day, 0)
	r := wantIn(t, got, id)
	near(t, "actual_start", r.ActualStart, 60*day)
	if r.ActualEnd != nil || r.History != "complete" || !r.StartKnown {
		t.Fatalf("T1 = %+v", r)
	}
	if len(r.Segments) != 1 || !r.Segments[0].ClippedStart || r.Segments[0].Status != "in_progress" {
		t.Fatalf("T1 segments = %+v", r.Segments)
	}
}

// T2: a completed interval that crosses the window start gets a seed segment.
func TestTimelineT2CompletedIntervalCrossesFrom(t *testing.T) {
	id := seedTLIssue(t, tlIssue{status: "done", created: 70 * day, rows: []tlRow{
		{ago: 40 * day, from: "todo", to: "in_progress"},
		{ago: 20 * day, from: "in_progress", to: "done"},
	}})
	r := wantIn(t, fetchActualTimeline(t, 30*day, 0), id)
	near(t, "actual_start", r.ActualStart, 40*day)
	near(t, "actual_end", r.ActualEnd, 20*day)
	if len(r.Segments) != 2 || !r.Segments[0].ClippedStart || r.Segments[0].Status != "in_progress" || r.Segments[1].Status != "done" {
		t.Fatalf("T2 segments = %+v", r.Segments)
	}
}

// T3: a reopened issue keeps one bar; later segments follow the done segment.
func TestTimelineT3ReopenAfterDone(t *testing.T) {
	id := seedTLIssue(t, tlIssue{status: "in_progress", created: 70 * day, rows: []tlRow{
		{ago: 40 * day, from: "todo", to: "in_progress"},
		{ago: 20 * day, from: "in_progress", to: "done"},
		{ago: 10 * day, from: "done", to: "in_progress"},
	}})
	r := wantIn(t, fetchActualTimeline(t, 30*day, 0), id)
	near(t, "actual_start", r.ActualStart, 40*day)
	if r.ActualEnd != nil {
		t.Fatalf("T3 actual_end = %v, want nil after reopen", r.ActualEnd)
	}
	if n := len(r.Segments); n != 3 || r.Segments[2].Status != "in_progress" || r.Segments[1].Status != "done" {
		t.Fatalf("T3 segments = %+v", r.Segments)
	}
}

// T4: markers only. No started row, due_date in the window: no bar.
func TestTimelineT4MarkersOnly(t *testing.T) {
	id := seedTLIssue(t, tlIssue{status: "todo", created: 70 * day, due: ago(5 * day)})
	r := wantIn(t, fetchActualTimeline(t, 30*day, 0), id)
	if r.ActualStart != nil || len(r.Segments) != 0 || r.DueDate == nil || r.History != "none" {
		t.Fatalf("T4 = %+v", r)
	}
}

// T5: equal created_at on two rows orders by id.
func TestTimelineT5EqualTimestampOrdersByID(t *testing.T) {
	base := dbid.NewV7().String()
	low, high := "00"+base[2:], "ff"+base[2:]
	id := seedTLIssue(t, tlIssue{status: "done", created: 20 * day, rows: []tlRow{
		// inserted high first: only ORDER BY created_at, id can put low first
		{ago: 5 * day, from: "in_progress", to: "done", id: high},
		{ago: 5 * day, from: "todo", to: "in_progress", id: low},
	}})
	r := wantIn(t, fetchActualTimeline(t, 30*day, 0), id)
	if len(r.Segments) != 2 || r.Segments[0].Status != "in_progress" || r.Segments[1].Status != "done" {
		t.Fatalf("T5 segments = %+v", r.Segments)
	}
	if r.ActualEnd == nil {
		t.Fatalf("T5 actual_end nil, want set")
	}
}

// T6: standalone active issue with no history and no dates is returned (rule 4).
func TestTimelineT6StandaloneActiveNoHistory(t *testing.T) {
	id := seedTLIssue(t, tlIssue{status: "in_progress", created: 90 * day})
	r := wantIn(t, fetchActualTimeline(t, 30*day, 0), id)
	if r.History != "none" || len(r.Segments) != 0 || r.ActualStart != nil {
		t.Fatalf("T6 = %+v", r)
	}
}

// T7: old partial-history issue, still in_review, no row in the window (rule 4).
func TestTimelineT7PartialHistory(t *testing.T) {
	id := seedTLIssue(t, tlIssue{status: "in_review", created: 70 * day, rows: []tlRow{
		{ago: 50 * day, from: "in_progress", to: "in_review"},
	}})
	r := wantIn(t, fetchActualTimeline(t, 30*day, 0), id)
	if r.History != "partial" || r.ActualStart != nil || r.StartKnown {
		t.Fatalf("T7 = %+v", r)
	}
	if len(r.Segments) != 1 || r.Segments[0].Status != "in_review" || !r.Segments[0].ClippedStart {
		t.Fatalf("T7 segments = %+v", r.Segments)
	}
}

// T9, T10: no history and not started now: absent in any window.
func TestTimelineT9T10NoHistoryNotStartedAbsent(t *testing.T) {
	backlog := seedTLIssue(t, tlIssue{status: "backlog", created: 90 * day})
	done := seedTLIssue(t, tlIssue{status: "done", created: 90 * day})
	for _, w := range [][2]time.Duration{{30 * day, 0}, {100 * day, 0}, {60 * day, 30 * day}} {
		got := fetchActualTimeline(t, w[0], w[1])
		wantAbsent(t, got, backlog)
		wantAbsent(t, got, done)
	}
}

// T11: partial history, now done, ended 40 days ago.
func TestTimelineT11PartialDoneAbsentFromDefaultWindow(t *testing.T) {
	id := seedTLIssue(t, tlIssue{status: "done", created: 100 * day, rows: []tlRow{
		{ago: 40 * day, from: "in_review", to: "done"},
	}})
	wantAbsent(t, fetchActualTimeline(t, 30*day, 0), id)
	wantIn(t, fetchActualTimeline(t, 60*day, 30*day), id)
}

// T12: complete history, one backlog->todo row 45 days ago, still todo.
func TestTimelineT12CompleteTodoOnlyInOwnWindow(t *testing.T) {
	id := seedTLIssue(t, tlIssue{status: "todo", created: 60 * day, rows: []tlRow{
		{ago: 45 * day, from: "backlog", to: "todo"},
	}})
	wantAbsent(t, fetchActualTimeline(t, 30*day, 0), id)
	wantIn(t, fetchActualTimeline(t, 60*day, 30*day), id)
}

func TestTimelineRejectsBadRequests(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	for name, body := range map[string]map[string]any{
		"from after to": {"window": map[string]any{"from": now, "to": "2020-01-01T00:00:00Z"}},
		"unknown field": {"window": map[string]any{"from": "2020-01-01T00:00:00Z", "to": now}, "scope": "x"},
	} {
		w := httptest.NewRecorder()
		testHandler.ListIssueActualTimeline(w, newRequest(http.MethodPost, "/api/issues/timeline", body))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: got %d, want 400", name, w.Code)
		}
	}
}

func postTimeline(from, to time.Duration) (int, []tlResult) {
	now := time.Now()
	body := map[string]any{"window": map[string]any{
		"from": now.Add(-from).UTC().Format(time.RFC3339Nano),
		"to":   now.Add(-to).UTC().Format(time.RFC3339Nano),
	}}
	w := httptest.NewRecorder()
	testHandler.ListIssueActualTimeline(w, newRequest(http.MethodPost, "/api/issues/timeline", body))
	var resp struct {
		Issues []tlResult `json:"issues"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w.Code, resp.Issues
}

func restoreTimelineLimits(t *testing.T) {
	oldIssues, oldRows, oldBatch := timelineMaxIssues, timelineMaxRows, timelineBatchSize
	oldScanned, oldTotal := timelineMaxScanned, timelineMaxTotalRows
	t.Cleanup(func() {
		timelineMaxIssues, timelineMaxRows, timelineBatchSize = oldIssues, oldRows, oldBatch
		timelineMaxScanned, timelineMaxTotalRows = oldScanned, oldTotal
	})
}

// The result cap counts issues the window selects, not every older issue the
// workspace holds: a large workspace with few matches must still answer.
func TestTimelineCapCountsSelectedNotLoaded(t *testing.T) {
	restoreTimelineLimits(t)
	seedTLIssue(t, tlIssue{status: "todo", created: 300 * day, rows: []tlRow{{ago: 175 * day, from: "backlog", to: "todo"}}})
	for i := 0; i < 3; i++ { // older issues the window does not select
		seedTLIssue(t, tlIssue{status: "backlog", created: 300 * day})
	}
	code, selected := postTimeline(200*day, 150*day)
	if code != http.StatusOK || len(selected) == 0 {
		t.Fatalf("baseline: %d, %d selected", code, len(selected))
	}
	timelineMaxIssues = len(selected)
	if code, _ := postTimeline(200*day, 150*day); code != http.StatusOK {
		t.Fatalf("cap equal to selected count: got %d, want 200", code)
	}
	timelineMaxIssues = len(selected) - 1
	if code, _ := postTimeline(200*day, 150*day); code != http.StatusUnprocessableEntity {
		t.Fatalf("selected above cap: got %d, want 422", code)
	}
}

// Issues load in keyset batches; a small batch must return the same result.
func TestTimelineBatchesMatchSingleLoad(t *testing.T) {
	restoreTimelineLimits(t)
	for i := 0; i < 5; i++ {
		seedTLIssue(t, tlIssue{status: "todo", created: 400 * day, rows: []tlRow{{ago: 375 * day, from: "backlog", to: "todo"}}})
	}
	_, whole := postTimeline(420*day, 350*day)
	timelineBatchSize = 2
	_, batched := postTimeline(420*day, 350*day)
	if len(whole) < 5 || len(batched) != len(whole) {
		t.Fatalf("whole %d, batched %d", len(whole), len(batched))
	}
	for i := range whole {
		if whole[i].IssueID != batched[i].IssueID {
			t.Fatalf("order differs at %d", i)
		}
	}
}

func TestTimelineRowCap(t *testing.T) {
	restoreTimelineLimits(t)
	seedTLIssue(t, tlIssue{status: "todo", created: 9 * day, rows: []tlRow{{ago: 8 * day, from: "backlog", to: "todo"}}})
	timelineMaxRows = 0
	if code, _ := postTimeline(30*day, 0); code != http.StatusUnprocessableEntity {
		t.Fatalf("row cap: got %d, want 422", code)
	}
}

func TestTimelineRejectsOversizedBody(t *testing.T) {
	pad := strings.Repeat("x", 1<<20)
	body := map[string]any{"window": map[string]any{"from": "2020-01-01T00:00:00Z", "to": "2020-02-01T00:00:00Z"}, "pad": pad}
	w := httptest.NewRecorder()
	testHandler.ListIssueActualTimeline(w, newRequest(http.MethodPost, "/api/issues/timeline", body))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("1 MiB body: got %d, want 413", w.Code)
	}
}

// The per-batch caps do not bound a request that spans many batches: the
// cumulative candidate scan and status-row reads have their own budget.
func TestTimelineRequestScanBudget(t *testing.T) {
	restoreTimelineLimits(t)
	for i := 0; i < 5; i++ { // older issues the window never selects
		seedTLIssue(t, tlIssue{status: "backlog", created: 300 * day})
	}
	timelineBatchSize = 2
	if code, _ := postTimeline(20*day, 10*day); code != http.StatusOK {
		t.Fatalf("baseline: got %d, want 200", code)
	}
	timelineMaxScanned = 3
	if code, _ := postTimeline(20*day, 10*day); code != http.StatusUnprocessableEntity {
		t.Fatalf("scan over budget: got %d, want 422", code)
	}
}

func TestTimelineRequestRowBudget(t *testing.T) {
	restoreTimelineLimits(t)
	for i := 0; i < 4; i++ {
		seedTLIssue(t, tlIssue{status: "todo", created: 9 * day, rows: []tlRow{{ago: 8 * day, from: "backlog", to: "todo"}}})
	}
	timelineBatchSize = 1
	timelineMaxRows = 1 // one row per batch is fine; the sum is not
	if code, _ := postTimeline(30*day, 0); code != http.StatusOK {
		t.Fatalf("baseline: got %d, want 200", code)
	}
	timelineMaxTotalRows = 1
	if code, _ := postTimeline(30*day, 0); code != http.StatusUnprocessableEntity {
		t.Fatalf("rows over budget: got %d, want 422", code)
	}
}
