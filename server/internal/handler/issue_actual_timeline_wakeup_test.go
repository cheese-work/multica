package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// CHE-4346 (S2b of CHE-4312): wakeup events in the timeline response. The
// source is the durable wakeup_triggered activity row and its rule snapshot.
// S2a (CHE-4341) proved the real writers store `observed`; these fixtures
// insert rows of that shape, because a row has to outlive its rule (E2, E3).

type tlWakeupEvent struct {
	ID            string    `json:"id"`
	At            time.Time `json:"at"`
	TargetIssueID string    `json:"target_issue_id"`
	RuleID        string    `json:"rule_id"`
	RuleKind      string    `json:"rule_kind"`
	ConditionType string    `json:"condition_type"`
	SourceIssueID *string   `json:"source_issue_id"`
	Class         string    `json:"class"`
	Stage         *int      `json:"stage"`
	RunID         *string   `json:"run_id"`
}

func newWakeupTarget(t *testing.T) string {
	t.Helper()
	return seedTLIssue(t, tlIssue{status: "in_progress", created: 5 * day})
}

// insertWakeupRow writes one wakeup_triggered row. snapshot is details.wakeup;
// extra is merged into details (task_id, observed, outcome).
func insertWakeupRow(t *testing.T, issue string, age time.Duration, snapshot, extra map[string]any) string {
	t.Helper()
	details := map[string]any{}
	for k, v := range extra {
		details[k] = v
	}
	if snapshot != nil {
		details["wakeup"] = snapshot
	}
	raw, _ := json.Marshal(details)
	id := dbid.NewV7().String()
	if _, err := testPool.Exec(context.Background(), `INSERT INTO activity_log (id, workspace_id, issue_id, actor_type, action, details, created_at) VALUES ($1,$2,$3,'system','wakeup_triggered',$4,$5)`,
		id, testWorkspaceID, issue, raw, time.Now().Add(-age)); err != nil {
		t.Fatal(err)
	}
	return id
}

func snapshotOf(agent, kind string, condition map[string]any) map[string]any {
	s := map[string]any{"id": dbid.NewV7().String(), "kind": kind, "mode": "once", "agent_id": agent}
	if condition != nil {
		s["condition"] = condition
	}
	return s
}

func fireExtra(extra map[string]any) map[string]any {
	out := map[string]any{"task_id": dbid.NewV7().String(), "events": []string{"wakeup.manual"}, "count": 1}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func fetchWakeupEvents(t *testing.T, mutate ...func(*http.Request)) []tlWakeupEvent {
	t.Helper()
	req := newRequest(http.MethodPost, "/api/issues/timeline", map[string]any{"window": map[string]any{
		"from": time.Now().Add(-30 * day).UTC().Format(time.RFC3339Nano),
		"to":   time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
	}})
	for _, m := range mutate {
		m(req)
	}
	w := httptest.NewRecorder()
	testHandler.ListIssueActualTimeline(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("timeline: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		WakeupEvents []tlWakeupEvent `json:"wakeup_events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.WakeupEvents
}

func eventsFor(events []tlWakeupEvent, target string) []tlWakeupEvent {
	var out []tlWakeupEvent
	for _, e := range events {
		if e.TargetIssueID == target {
			out = append(out, e)
		}
	}
	return out
}

func oneEvent(t *testing.T, events []tlWakeupEvent, target string) tlWakeupEvent {
	t.Helper()
	got := eventsFor(events, target)
	if len(got) != 1 {
		t.Fatalf("events for %s = %+v, want exactly 1", target, got)
	}
	return got[0]
}

func TestWakeupE1TimerRuleFiresTwiceGivesTwoSelfEvents(t *testing.T) {
	target := newWakeupTarget(t)
	agent := dbfx.Agent(t, "e1 agent", "", testutil.Cols{"permission_mode": "private"})
	snap := snapshotOf(agent, "every", nil)
	insertWakeupRow(t, target, 3*day, snap, fireExtra(nil))
	insertWakeupRow(t, target, 2*day, snap, fireExtra(nil))
	got := eventsFor(fetchWakeupEvents(t), target)
	if len(got) != 2 {
		t.Fatalf("events = %+v, want 2", got)
	}
	for _, e := range got {
		if e.Class != "self" || e.RuleKind != "every" || e.SourceIssueID != nil || e.RuleID != snap["id"] || e.RunID == nil {
			t.Fatalf("event = %+v, want self/every with rule id, run id and no source", e)
		}
	}
	if !got[0].At.Before(got[1].At) {
		t.Fatalf("events not ordered by time: %+v", got)
	}
}

func TestWakeupE2EditedRuleKeepsOldSource(t *testing.T) {
	target := newWakeupTarget(t)
	oldSource := dbfx.Issue(t, "e2 old source")
	newSource := dbfx.Issue(t, "e2 new source")
	agent := dbfx.Agent(t, "e2 agent", "")
	rule := dbfx.Insert(t, "issue_wakeup", testutil.Cols{"id": testutil.Raw("gen_random_uuid()"), "workspace_id": testWorkspaceID, "issue_id": target, "agent_id": agent, "created_by": testUserID, "instruction": "e2", "kind": "event", "mode": "once",
		"condition": fmt.Sprintf(`{"type":"other_issue","issue_id":%q,"state":"done"}`, oldSource)})
	snap := snapshotOf(agent, "event", map[string]any{"type": "other_issue", "issue_id": oldSource, "state": "done"})
	snap["id"] = rule
	insertWakeupRow(t, target, day, snap, fireExtra(map[string]any{"observed": map[string]any{"issue_id": oldSource, "status": "done"}}))
	dbfx.Exec(t, `UPDATE issue_wakeup SET condition=$2 WHERE id=$1`, rule, fmt.Sprintf(`{"type":"other_issue","issue_id":%q,"state":"done"}`, newSource))
	e := oneEvent(t, fetchWakeupEvents(t), target)
	if e.Class != "cross" || e.ConditionType != "other_issue" || e.SourceIssueID == nil || *e.SourceIssueID != oldSource {
		t.Fatalf("event = %+v, want cross from the old source %s", e, oldSource)
	}
}

func TestWakeupE3DeletedRuleStillReturned(t *testing.T) {
	target := newWakeupTarget(t)
	agent := dbfx.Agent(t, "e3 agent", "")
	rule := dbfx.Insert(t, "issue_wakeup", testutil.Cols{"id": testutil.Raw("gen_random_uuid()"), "workspace_id": testWorkspaceID, "issue_id": target, "agent_id": agent, "created_by": testUserID, "instruction": "e3", "kind": "at", "mode": "once", "next_fire_at": time.Now().Add(time.Hour)})
	snap := snapshotOf(agent, "at", nil)
	snap["id"] = rule
	insertWakeupRow(t, target, day, snap, fireExtra(nil))
	dbfx.Exec(t, `DELETE FROM issue_wakeup WHERE id=$1`, rule)
	e := oneEvent(t, fetchWakeupEvents(t), target)
	if e.Class != "self" || e.RuleID != rule {
		t.Fatalf("event = %+v, want self from the snapshot of deleted rule %s", e, rule)
	}
}

func TestWakeupE4ChildrenDoneIsAggregateWithStageAndNoChildSource(t *testing.T) {
	target := newWakeupTarget(t)
	agent := dbfx.Agent(t, "e4 agent", "")
	insertWakeupRow(t, target, day, snapshotOf(agent, "event", map[string]any{"type": "children_done", "stage": 2}),
		fireExtra(map[string]any{"observed": map[string]any{"total": 2, "closed": 2, "stage": 2, "next_stage": 3}}))
	e := oneEvent(t, fetchWakeupEvents(t), target)
	if e.Class != "aggregate_children" || e.Stage == nil || *e.Stage != 2 || e.SourceIssueID != nil {
		t.Fatalf("event = %+v, want aggregate_children stage 2 with no source", e)
	}
}

func TestWakeupE5PullRequestConditionIsExternal(t *testing.T) {
	target := newWakeupTarget(t)
	agent := dbfx.Agent(t, "e5 agent", "")
	insertWakeupRow(t, target, day, snapshotOf(agent, "event", map[string]any{"type": "pull_request", "event": "merged"}),
		fireExtra(map[string]any{"observed": map[string]any{"pull_requests": []any{}}}))
	insertWakeupRow(t, target, 2*day, snapshotOf(agent, "event", map[string]any{"type": "github_run"}), fireExtra(nil))
	got := eventsFor(fetchWakeupEvents(t), target)
	if len(got) != 2 || got[0].Class != "external" || got[1].Class != "external" || got[0].ConditionType != "github_run" || got[1].ConditionType != "pull_request" {
		t.Fatalf("events = %+v, want two external events", got)
	}
}

func TestWakeupE6RowWithoutObservedIsUnknownSource(t *testing.T) {
	target := newWakeupTarget(t)
	agent := dbfx.Agent(t, "e6 agent", "")
	other := dbfx.Issue(t, "e6 other")
	insertWakeupRow(t, target, 2*day, snapshotOf(agent, "event", map[string]any{"type": "other_issue", "issue_id": other, "state": "done"}), fireExtra(nil))
	insertWakeupRow(t, target, day, nil, fireExtra(nil)) // no snapshot at all
	got := eventsFor(fetchWakeupEvents(t), target)
	if len(got) != 2 {
		t.Fatalf("events = %+v, want 2", got)
	}
	for _, e := range got {
		if e.Class != "unknown_source" || e.SourceIssueID != nil {
			t.Fatalf("event = %+v, want unknown_source without a source", e)
		}
	}
}

func TestWakeupE7RowsWithoutARunAreExcluded(t *testing.T) {
	target := newWakeupTarget(t)
	agent := dbfx.Agent(t, "e7 agent", "")
	snap := snapshotOf(agent, "event", nil)
	insertWakeupRow(t, target, day, snap, map[string]any{"outcome": "target_unauthorized"})
	insertWakeupRow(t, target, day, snap, map[string]any{"outcome": "filtered_out", "filtered": 1})
	insertWakeupRow(t, target, day, snap, map[string]any{"outcome": "acknowledged", "events": []string{"comment.created"}})
	if got := eventsFor(fetchWakeupEvents(t), target); len(got) != 0 {
		t.Fatalf("events = %+v, want none: no row carries a task_id", got)
	}
}

func TestWakeupE8EventRuleOnCommentIsSelf(t *testing.T) {
	target := newWakeupTarget(t)
	agent := dbfx.Agent(t, "e8 agent", "")
	insertWakeupRow(t, target, day, snapshotOf(agent, "event", nil), fireExtra(map[string]any{"events": []string{"comment.created"}, "comment_id": dbid.NewV7().String()}))
	e := oneEvent(t, fetchWakeupEvents(t), target)
	if e.Class != "self" || e.RuleKind != "event" || e.SourceIssueID != nil {
		t.Fatalf("event = %+v, want self", e)
	}
}

func TestWakeupEventsStayInsideTheWindowAndPage(t *testing.T) {
	target := newWakeupTarget(t)
	elsewhere := seedTLIssue(t, tlIssue{status: "done", created: 90 * day}) // not selected
	agent := dbfx.Agent(t, "window agent", "")
	snap := snapshotOf(agent, "event", nil)
	insertWakeupRow(t, target, 40*day, snap, fireExtra(nil)) // before the window
	insertWakeupRow(t, elsewhere, day, snap, fireExtra(nil)) // target not in the page
	if got := fetchWakeupEvents(t); len(eventsFor(got, target)) != 0 || len(eventsFor(got, elsewhere)) != 0 {
		t.Fatalf("events = %+v, want none for an old event or an unselected target", got)
	}
}

// N5: a wakeup whose rule agent the viewer cannot see is omitted.
func TestWakeupN5PrivateAgentEventOmitted(t *testing.T) {
	target := newWakeupTarget(t)
	privateAgent := dbfx.Agent(t, "n5 private agent", "")
	insertWakeupRow(t, target, day, snapshotOf(privateAgent, "event", nil), fireExtra(nil))
	viewer := dbfx.User(t, "n5 viewer", fmt.Sprintf("n5-%d@multica.test", time.Now().UnixNano()))
	dbfx.Member(t, testWorkspaceID, viewer, "member")
	asViewer := func(r *http.Request) { r.Header.Set("X-User-ID", viewer) }
	if got := eventsFor(fetchWakeupEvents(t, asViewer), target); len(got) != 0 {
		t.Fatalf("private agent wakeup leaked to a plain member: %+v", got)
	}
	if got := eventsFor(fetchWakeupEvents(t), target); len(got) != 1 {
		t.Fatalf("owner sees %+v, want the event", got)
	}
}

// bulkWakeupRows inserts n run-starting rows for one agent, oldest first, all
// older than the rows insertWakeupRow adds afterwards.
func bulkWakeupRows(t *testing.T, issue, agent string, n int) {
	t.Helper()
	snap, _ := json.Marshal(snapshotOf(agent, "event", nil))
	if _, err := testPool.Exec(context.Background(), `
INSERT INTO activity_log (id, workspace_id, issue_id, actor_type, action, details, created_at)
SELECT gen_random_uuid(), $1, $2, 'system', 'wakeup_triggered',
       jsonb_build_object('task_id', gen_random_uuid()::text, 'wakeup', $3::jsonb),
       now() - interval '20 days' + g * interval '1 second'
FROM generate_series(1, $4) g`, testWorkspaceID, issue, snap, n); err != nil {
		t.Fatal(err)
	}
}

// The row cap counts events the viewer can see: rows hidden by N5 never use
// it up, and visible events after them are not silently dropped.
func TestWakeupCapCountsVisibleEventsOnly(t *testing.T) {
	target := newWakeupTarget(t)
	privateAgent := dbfx.Agent(t, "cap private agent", "")
	viewer := dbfx.User(t, "cap viewer", fmt.Sprintf("cap-%d@multica.test", time.Now().UnixNano()))
	dbfx.Member(t, testWorkspaceID, viewer, "member")
	ownAgent := dbfx.Agent(t, "cap own agent", "", testutil.Cols{"owner_id": viewer}) // the viewer sees its own agent
	bulkWakeupRows(t, target, privateAgent, timelineMaxWakeupEvents+1)
	insertWakeupRow(t, target, day, snapshotOf(ownAgent, "event", nil), fireExtra(nil))
	got := fetchWakeupEvents(t, func(r *http.Request) { r.Header.Set("X-User-ID", viewer) })
	if e := oneEvent(t, got, target); e.Class != "self" {
		t.Fatalf("event = %+v, want the one visible event", e)
	}
}

// More visible events than the cap is a 422, never a silent cut.
func TestWakeupOverCapIs422(t *testing.T) {
	target := newWakeupTarget(t)
	agent := dbfx.Agent(t, "over cap agent", "")
	bulkWakeupRows(t, target, agent, timelineMaxWakeupEvents+1)
	req := newRequest(http.MethodPost, "/api/issues/timeline", map[string]any{"window": map[string]any{
		"from": time.Now().Add(-30 * day).UTC().Format(time.RFC3339Nano),
		"to":   time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
	}})
	w := httptest.NewRecorder()
	testHandler.ListIssueActualTimeline(w, req)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("timeline: %d, want 422", w.Code)
	}
}

// N6 (timeline half): an other_issue source outside the workspace is omitted,
// with no placeholder and no ID.
// Small batches force the keyset cursor across rows that are hidden by agent
// access, dropped for a foreign source, or visible. Nothing may be lost,
// repeated or reordered at a batch boundary.
func TestWakeupBatchBoundariesKeepVisibleEventsInOrder(t *testing.T) {
	old := wakeupBatchSize
	wakeupBatchSize = 2
	t.Cleanup(func() { wakeupBatchSize = old })
	target := newWakeupTarget(t)
	privateAgent := dbfx.Agent(t, "batch private agent", "")
	viewer := dbfx.User(t, "batch viewer", fmt.Sprintf("batch-%d@multica.test", time.Now().UnixNano()))
	dbfx.Member(t, testWorkspaceID, viewer, "member")
	ownAgent := dbfx.Agent(t, "batch own agent", "", testutil.Cols{"owner_id": viewer})
	foreign := dbfx.Issue(t, "batch foreign source", testutil.Cols{"workspace_id": dbfx.Workspace(t, "batch foreign", fmt.Sprintf("batch-%d", time.Now().UnixNano()))})
	var want []string
	for i := 0; i < 14; i++ {
		age := day + time.Duration(20-i)*time.Minute
		switch i % 3 {
		case 0: // visible agent, source outside the workspace: dropped
			insertWakeupRow(t, target, age, snapshotOf(ownAgent, "event", map[string]any{"type": "other_issue", "issue_id": foreign, "state": "done"}),
				fireExtra(map[string]any{"observed": map[string]any{"issue_id": foreign, "status": "done"}}))
		case 1: // agent the viewer cannot see: hidden
			insertWakeupRow(t, target, age, snapshotOf(privateAgent, "event", nil), fireExtra(nil))
		default: // visible
			want = append(want, insertWakeupRow(t, target, age, snapshotOf(ownAgent, "event", nil), fireExtra(nil)))
		}
	}
	got := eventsFor(fetchWakeupEvents(t, func(r *http.Request) { r.Header.Set("X-User-ID", viewer) }), target)
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(got), len(want), got)
	}
	for i, e := range got {
		if e.ID != want[i] {
			t.Fatalf("event %d = %s, want %s", i, e.ID, want[i])
		}
	}
}

func TestWakeupN6ForeignSourceOmitted(t *testing.T) {
	target := newWakeupTarget(t)
	agent := dbfx.Agent(t, "n6 agent", "")
	foreign := dbfx.Issue(t, "n6 foreign source", testutil.Cols{"workspace_id": dbfx.Workspace(t, "n6 foreign", fmt.Sprintf("n6-%d", time.Now().UnixNano()))})
	insertWakeupRow(t, target, day, snapshotOf(agent, "event", map[string]any{"type": "other_issue", "issue_id": foreign, "state": "done"}),
		fireExtra(map[string]any{"observed": map[string]any{"issue_id": foreign, "status": "done"}}))
	if got := eventsFor(fetchWakeupEvents(t), target); len(got) != 0 {
		t.Fatalf("foreign-workspace source leaked: %+v", got)
	}
}
