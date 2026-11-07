package service

import (
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// triggeredDetails reads the details of the single wakeup_triggered row a real
// firing wrote for issue.
func triggeredDetails(t *testing.T, f principalFixture, issue pgtype.UUID) map[string]json.RawMessage {
	t.Helper()
	if n := f.Count(t, "SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='wakeup_triggered'", issue); n != 1 {
		t.Fatalf("wakeup_triggered rows = %d, want 1", n)
	}
	var raw []byte
	f.QueryRow(t, "SELECT details FROM activity_log WHERE issue_id=$1 AND action='wakeup_triggered'", issue).Scan(&raw)
	var details map[string]json.RawMessage
	if err := json.Unmarshal(raw, &details); err != nil {
		t.Fatalf("details are not an object: %v: %s", err, raw)
	}
	return details
}

func decodeField(t *testing.T, details map[string]json.RawMessage, key string, into any) {
	t.Helper()
	raw, ok := details[key]
	if !ok {
		keys := make([]string, 0, len(details))
		for k := range details {
			keys = append(keys, k)
		}
		t.Fatalf("details lack %q; has %v", key, keys)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("details.%s: %v: %s", key, err, raw)
	}
}

// CHE-4312 E2 (assumption): an other_issue firing keeps the watched issue and
// its status in details.observed, and the rule snapshot in details.wakeup, so
// the timeline can draw the arrow from observed data alone.
func TestWakeupTriggeredRowKeepsOtherIssueObservation(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	other := f.Issue(t, "Waiting for review", testutil.Cols{"status": "in_progress"})
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Instruction: "Continue after review",
		Condition: condition(t, map[string]any{"type": "other_issue", "issue_id": other, "state": "in_review"})})
	wakeSetStatus(t, f, parseTestUUID(t, other), "in_review")
	wakeTick(t, f, s, w.ID)
	if n := wakeRuns(t, f, w.ID); n != 1 {
		t.Fatalf("met condition started %d runs, want 1", n)
	}

	details := triggeredDetails(t, f, issue)
	var observed struct {
		IssueID string `json:"issue_id"`
		Status  string `json:"status"`
	}
	decodeField(t, details, "observed", &observed)
	if observed.IssueID != other || observed.Status != "in_review" {
		t.Fatalf("observed = %+v, want issue %s in_review", observed, other)
	}
	var snapshot struct {
		ID        string          `json:"id"`
		Condition json.RawMessage `json:"condition"`
	}
	decodeField(t, details, "wakeup", &snapshot)
	var cond WakeupCondition
	if err := json.Unmarshal(snapshot.Condition, &cond); err != nil || cond.Type != "other_issue" {
		t.Fatalf("snapshot condition = %s (%v)", snapshot.Condition, err)
	}
	var taskID string
	decodeField(t, details, "task_id", &taskID)
	if snapshot.ID != util.UUIDToString(w.ID) || taskID == "" {
		t.Fatalf("snapshot id %q or task id %q missing", snapshot.ID, taskID)
	}
}

// CHE-4312 E4 (assumption): a children_done stage firing keeps counts in
// details.observed and the stage in the rule snapshot, with no child IDs.
func TestWakeupTriggeredRowKeepsChildrenDoneObservation(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	f.Issue(t, "Stage 1 child", testutil.Cols{"parent_issue_id": issue, "status": "done", "stage": 1})
	f.Issue(t, "Stage 2 child", testutil.Cols{"parent_issue_id": issue, "status": "done", "stage": 2})
	f.Issue(t, "Stage 3 child", testutil.Cols{"parent_issue_id": issue, "status": "todo", "stage": 3})
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Instruction: "Start stage 3",
		Condition: condition(t, map[string]any{"type": "children_done", "stage": 2})})
	wakeTick(t, f, s, w.ID)
	if n := wakeRuns(t, f, w.ID); n != 1 {
		t.Fatalf("met condition started %d runs, want 1", n)
	}

	details := triggeredDetails(t, f, issue)
	var observed map[string]json.RawMessage
	decodeField(t, details, "observed", &observed)
	var finished, total int
	if err := json.Unmarshal(observed["finished"], &finished); err != nil {
		t.Fatalf("observed.finished: %v: %v", err, observed)
	}
	if err := json.Unmarshal(observed["total"], &total); err != nil {
		t.Fatalf("observed.total: %v: %v", err, observed)
	}
	if finished != 2 || total != 2 {
		t.Fatalf("observed = %v, want 2 of 2 children through stage 2", observed)
	}
	for key := range observed {
		if key == "issue_id" || key == "children" || key == "child_ids" {
			t.Fatalf("observed names a child (%q); no single child is the cause: %v", key, observed)
		}
	}
	var snapshot struct {
		Condition WakeupCondition `json:"condition"`
	}
	decodeField(t, details, "wakeup", &snapshot)
	if snapshot.Condition.Type != "children_done" || snapshot.Condition.Stage == nil || *snapshot.Condition.Stage != 2 {
		t.Fatalf("snapshot condition = %+v, want children_done stage 2", snapshot.Condition)
	}
}
