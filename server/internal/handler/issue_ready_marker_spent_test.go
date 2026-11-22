package handler

import (
	"context"
	"testing"
)

// The author's ready marker (metadata ready_for_review) covers one handoff
// (CHE-5741 slice A). A DB trigger spends it on any issue.status change.
func TestReadyMarkerIsSpentByStatusChange(t *testing.T) {
	if testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	hasMarker := func(id string) bool {
		var v bool
		if err := testPool.QueryRow(ctx, `SELECT metadata ? 'ready_for_review' FROM issue WHERE id=$1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	exec := func(sql string, args ...any) {
		if _, err := testPool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("status change spends the marker", func(t *testing.T) {
		id := dbfx.Issue(t, "marker spent", map[string]any{"status": "in_progress", "metadata": `{"ready_for_review": true, "keep": 1}`})
		if !hasMarker(id) {
			t.Fatal("fixture: marker not set")
		}
		exec(`UPDATE issue SET status='in_review' WHERE id=$1`, id)
		if hasMarker(id) {
			t.Fatal("status change left ready_for_review set")
		}
		var keep string
		if err := testPool.QueryRow(ctx, `SELECT metadata->>'keep' FROM issue WHERE id=$1`, id).Scan(&keep); err != nil || keep != "1" {
			t.Fatalf("other metadata keys must survive: keep=%q err=%v", keep, err)
		}
	})

	t.Run("marker set after the move survives", func(t *testing.T) {
		id := dbfx.Issue(t, "marker after move", map[string]any{"status": "in_progress"})
		exec(`UPDATE issue SET status='in_review' WHERE id=$1`, id)
		exec(`UPDATE issue SET metadata=metadata||'{"ready_for_review": true}'::jsonb WHERE id=$1`, id)
		if !hasMarker(id) {
			t.Fatal("marker set after the move was cleared")
		}
		exec(`UPDATE issue SET title='renamed' WHERE id=$1`, id)
		if !hasMarker(id) {
			t.Fatal("a non-status write spent the marker")
		}
		exec(`UPDATE issue SET status='in_review' WHERE id=$1`, id) // same status: no change
		if !hasMarker(id) {
			t.Fatal("a status write that changes nothing spent the marker")
		}
	})
}
