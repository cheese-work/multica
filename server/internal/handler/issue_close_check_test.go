package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-9266 (CHE-9122 step 1): a rejected close records why, with the inputs each
// predicate read, and `issue close-check` shows the same predicates read-only.

type closeCheckPredicate struct {
	Name string `json:"name"`
	Held bool   `json:"held"`
}

type closeCheckView struct {
	Source       string                         `json:"source"`
	Outcome      string                         `json:"outcome"`
	FirstFailure string                         `json:"first_failure"`
	Mode         struct{ Value, Source string } `json:"mode"`
	Predicates   []closeCheckPredicate          `json:"predicates"`
}

type skippedRow struct {
	ID      string
	Details struct {
		FirstFailure string                         `json:"first_failure"`
		Mode         struct{ Value, Source string } `json:"mode"`
		Inputs       map[string]json.RawMessage     `json:"inputs"`
		CheckedAt    string                         `json:"checked_at"`
	}
}

func (c autoClose) skippedRows(t *testing.T) []skippedRow {
	t.Helper()
	rows, err := testPool.Query(t.Context(), `SELECT id::text, details FROM activity_log WHERE issue_id = $1 AND action = 'auto_close_skipped' ORDER BY created_at, id`, c.issueID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []skippedRow
	for rows.Next() {
		var r skippedRow
		var raw []byte
		if err := rows.Scan(&r.ID, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &r.Details); err != nil {
			t.Fatalf("details %s: %v", raw, err)
		}
		out = append(out, r)
	}
	return out
}

// closeCheck calls GET /api/issues/{id}/close-check, with ?activity=<row> when row is set.
func (c autoClose) closeCheck(t *testing.T, row string) closeCheckView {
	t.Helper()
	path := "/api/issues/" + c.issueID + "/close-check"
	if row != "" {
		path += "?activity=" + row
	}
	var view closeCheckView
	testutil.Call(t, testHandler.GetIssueCloseCheck, withURLParam(newRequest(http.MethodGet, path, nil), "id", c.issueID)).Want(http.StatusOK).JSON(&view)
	return view
}

// (a) A child with every predicate but post_close_work enters merged. Today the
// handler's run starts and no row says why the close was skipped.
func TestCloseCheck_RejectedCloseRecordsTheFirstFailureWithItsInputs(t *testing.T) {
	for _, mode := range []string{"block", "warn"} {
		t.Run(mode, func(t *testing.T) {
			c := newAutoCloseMerged(t, mode)
			c.seedVerdict(t, "review_passed", "[]", "", "1 hour")
			dbfx.Exec(t, `UPDATE issue SET metadata = '{}'::jsonb WHERE id = $1`, c.issueID)
			c.enterMerged(t)
			c.wantHandlerRun(t)
			rows := c.skippedRows(t)
			if len(rows) != 1 {
				t.Fatalf("auto_close_skipped rows = %d, want 1", len(rows))
			}
			d := rows[0].Details
			if d.FirstFailure != "post_close_work_none" {
				t.Fatalf("first_failure = %q, want post_close_work_none", d.FirstFailure)
			}
			if d.Mode.Value != mode || d.Mode.Source != "workspace" {
				t.Fatalf("mode = %+v, want %s from the workspace", d.Mode, mode)
			}
			if d.CheckedAt == "" {
				t.Fatal("checked_at is empty")
			}
			for _, key := range []string{"issue", "done_edge", "verdict", "prs", "open_children", "open_decision", "wakeups", "parent"} {
				if _, ok := d.Inputs[key]; !ok {
					t.Errorf("inputs lack %q: %v", key, d.Inputs)
				}
			}
		})
	}
}

// A close that succeeds records no skipped row.
func TestCloseCheck_NoRowWhenTheCloseHolds(t *testing.T) {
	c := newAutoCloseMerged(t, "block")
	c.seedVerdict(t, "review_passed", "[]", "", "1 hour")
	c.enterMerged(t)
	c.wantClosed(t)
	if rows := c.skippedRows(t); len(rows) != 0 {
		t.Fatalf("closed issue has %d auto_close_skipped rows, want 0", len(rows))
	}
}

// Mode off is the kill switch: nothing is checked, so nothing is recorded.
func TestCloseCheck_NoRowWhenModeIsOff(t *testing.T) {
	c := newAutoCloseMerged(t, "off")
	c.seedVerdict(t, "review_passed", "[]", "", "1 hour")
	dbfx.Exec(t, `UPDATE issue SET metadata = '{}'::jsonb WHERE id = $1`, c.issueID)
	c.enterMerged(t)
	if rows := c.skippedRows(t); len(rows) != 0 {
		t.Fatalf("mode off recorded %d auto_close_skipped rows, want 0", len(rows))
	}
}

// (b) The issue changes after the row: --activity still returns the recorded
// first failure; the plain form reads the new state.
func TestCloseCheck_ActivityReplaysTheRecordedFailureAfterTheIssueChanges(t *testing.T) {
	c := newAutoCloseMerged(t, "block")
	c.seedVerdict(t, "review_passed", "[]", "", "1 hour")
	dbfx.Exec(t, `UPDATE issue SET metadata = '{}'::jsonb WHERE id = $1`, c.issueID)
	c.enterMerged(t)
	rows := c.skippedRows(t)
	if len(rows) != 1 {
		t.Fatalf("auto_close_skipped rows = %d, want 1", len(rows))
	}
	dbfx.Exec(t, `UPDATE issue SET metadata = '{"post_close_work":"none"}'::jsonb WHERE id = $1`, c.issueID)

	replay := c.closeCheck(t, rows[0].ID)
	if replay.Source != "activity" || replay.FirstFailure != "post_close_work_none" || replay.Outcome != "rejected" {
		t.Fatalf("replay = %+v, want the recorded post_close_work_none rejection", replay)
	}
	now := c.closeCheck(t, "")
	if now.Source != "current" || now.FirstFailure != "" || now.Outcome != "holds" {
		t.Fatalf("current = %+v, want every predicate held", now)
	}
	if now.Mode.Value != "block" || now.Mode.Source != "workspace" {
		t.Fatalf("current mode = %+v, want block from the workspace", now.Mode)
	}
	if len(now.Predicates) == 0 || now.Predicates[len(now.Predicates)-1].Name != "post_close_work_none" {
		t.Fatalf("predicates = %+v, want all ten in order ending at post_close_work_none", now.Predicates)
	}
}

// close-check reads and writes nothing.
func TestCloseCheck_PlainFormWritesNothing(t *testing.T) {
	c := newAutoCloseMerged(t, "block")
	c.seedVerdict(t, "review_passed", "[]", "", "1 hour")
	dbfx.Exec(t, `UPDATE issue SET metadata = '{}'::jsonb WHERE id = $1`, c.issueID)
	before := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1`, c.issueID)
	view := c.closeCheck(t, "")
	if view.FirstFailure != "post_close_work_none" {
		t.Fatalf("first_failure = %q, want post_close_work_none", view.FirstFailure)
	}
	if after := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1`, c.issueID); after != before {
		t.Fatalf("activity rows %d -> %d, close-check must be read-only", before, after)
	}
}

// (c) Replay of every fixture row equals its recorded first failure.
func TestCloseCheck_ReplayOfEveryFixtureRowEqualsItsRecordedFirstFailure(t *testing.T) {
	for name, tc := range autoCloseNegatives {
		t.Run(name, func(t *testing.T) {
			c := newAutoClose(t, "block")
			c.seedVerdict(t, "review_passed", "[]", "", "1 hour")
			tc.setup(t, c)
			c.enter(t)
			rows := c.skippedRows(t)
			if len(rows) != 1 {
				t.Fatalf("auto_close_skipped rows = %d, want 1", len(rows))
			}
			recorded := rows[0].Details.FirstFailure
			if recorded == "" {
				t.Fatal("recorded first_failure is empty")
			}
			replay := c.closeCheck(t, rows[0].ID)
			if replay.FirstFailure != recorded {
				t.Fatalf("replay first_failure = %q, recorded %q", replay.FirstFailure, recorded)
			}
		})
	}
}
