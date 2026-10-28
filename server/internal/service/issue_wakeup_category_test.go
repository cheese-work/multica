package service

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// children_done reads a child's category from the catalog on every evaluation,
// so a category change on a custom status applies to the very next read
// (CHE-2274).
func TestChildrenDoneFollowsStatusCategoryChange(t *testing.T) {
	f, _, parent, _ := wakeFixture(t)
	ctx := context.Background()
	workspace := parseTestUUID(t, f.WorkspaceID)
	entry, err := f.q.CreateIssueStatusEntry(ctx, db.CreateIssueStatusEntryParams{
		WorkspaceID: workspace, Key: "children_done_cat", Name: "children_done_cat", Category: "done", Color: "#123456",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.Cleanup(t, "DELETE FROM issue_status WHERE id=$1", entry.ID)
	f.Issue(t, "child on custom status", testutil.Cols{"status": entry.Key, "parent_issue_id": parent})

	childrenClosed := func() bool {
		t.Helper()
		tx, err := f.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		children, err := loadSubIssues(ctx, tx, f.q.WithTx(tx), parent, workspace)
		if err != nil {
			t.Fatal(err)
		}
		if len(children) != 1 {
			t.Fatalf("children = %d, want 1", len(children))
		}
		met, _, _ := childrenDone(WakeupCondition{Type: "children_done"}, children)
		return met
	}
	setCategory := func(category string) {
		t.Helper()
		if _, err := f.q.UpdateIssueStatusEntry(ctx, db.UpdateIssueStatusEntryParams{
			ID: entry.ID, WorkspaceID: workspace, Category: pgtype.Text{String: category, Valid: true},
		}); err != nil {
			t.Fatal(err)
		}
	}

	if !childrenClosed() {
		t.Fatal("child on a done-category status must count as closed")
	}
	setCategory("started")
	if childrenClosed() {
		t.Fatal("child still counted as closed right after its status moved to started")
	}
	setCategory("done")
	if !childrenClosed() {
		t.Fatal("child not counted as closed right after its status moved back to done")
	}
}
