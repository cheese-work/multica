package commentguard

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestLockThreadForReplyAndClearResolution_ReturnsAllClearedRows is the fix
// #2 regression: LockThreadForReplyAndClearResolution used to take only
// rows[0] from ClearThreadResolutionForReply's :many result and silently
// discard the rest, so a caller that published one comment:unresolved event
// per returned row (see TaskService.PublishThreadUnresolvedOnReply and every
// call site) would under-report any row beyond the first.
//
// ClearThreadResolutionForReply's own doc comment argues that today's
// single-resolution invariant (enforced by ClearOtherThreadResolutions,
// which only ever runs inside ResolveComment) makes more than one resolved
// comment in the same thread unreachable through the normal application
// write paths. This test does not rely on that invariant holding forever —
// it seeds the violation directly at the database layer (two comments in the
// same thread both carrying resolved_at, bypassing ResolveComment/
// ClearOtherThreadResolutions entirely) so the fix is verified defensively:
// LockThreadForReplyAndClearResolution must clear and return BOTH rows, not
// silently drop the second one, regardless of whether the invariant that
// makes this state rare in practice is airtight.
func TestLockThreadForReplyAndClearResolution_ReturnsAllClearedRows(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	issueID := testDBFixture.Issue(t, "commentguard multi-row clear fixture")
	root := testDBFixture.Comment(t, issueID, "root")
	replyA := testDBFixture.Comment(t, issueID, "reply a", testutil.Cols{"parent_id": root})
	replyB := testDBFixture.Comment(t, issueID, "reply b", testutil.Cols{"parent_id": root})

	// Directly mark BOTH replyA and replyB as resolved — bypassing
	// ResolveComment/ClearOtherThreadResolutions, which is the only code path
	// that would normally enforce "at most one resolved comment per thread".
	// This reproduces the state ClearThreadResolutionForReply's WHERE clause
	// (resolved_at IS NOT NULL, scoped to the thread's descendants) would
	// find and clear if the invariant were ever violated by a bug elsewhere,
	// a migration, or manual data repair.
	for _, id := range []string{replyA, replyB} {
		if _, err := testPool.Exec(ctx, `
			UPDATE comment SET resolved_at = now(), resolved_by_type = 'member', resolved_by_id = $2
			WHERE id = $1
		`, id, testUserID); err != nil {
			t.Fatalf("seed resolved_at on %s: %v", id, err)
		}
	}

	rootUUID, err := util.ParseUUID(root)
	if err != nil {
		t.Fatalf("parse root id: %v", err)
	}
	workspaceUUID, err := util.ParseUUID(testWorkspaceID)
	if err != nil {
		t.Fatalf("parse workspace id: %v", err)
	}

	// A third reply is about to land under root — call the helper exactly as
	// a real reply-insert call site does, with parentID pointing at root
	// (any comment in the thread would resolve to the same thread root).
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx)
	qtx := testQueries.WithTx(tx)

	gotRoot, cleared, err := LockThreadForReplyAndClearResolution(ctx, qtx, workspaceUUID, rootUUID)
	if err != nil {
		t.Fatalf("LockThreadForReplyAndClearResolution: %v", err)
	}
	if gotRoot == nil {
		t.Fatalf("expected a non-nil thread root")
	}
	if util.UUIDToString(gotRoot.ID) != root {
		t.Fatalf("thread root = %s, want %s", util.UUIDToString(gotRoot.ID), root)
	}

	if len(cleared) != 2 {
		t.Fatalf("cleared rows = %d, want 2 (both replyA and replyB) — got IDs: %v", len(cleared), clearedIDs(cleared))
	}
	gotIDs := map[string]bool{}
	for _, c := range cleared {
		gotIDs[util.UUIDToString(c.ID)] = true
		if c.ResolvedAt.Valid {
			t.Fatalf("cleared row %s still reports resolved_at set in the returned struct", util.UUIDToString(c.ID))
		}
	}
	if !gotIDs[replyA] {
		t.Fatalf("cleared rows did not include replyA (%s): got %v", replyA, clearedIDs(cleared))
	}
	if !gotIDs[replyB] {
		t.Fatalf("cleared rows did not include replyB (%s) — this is exactly the row the old rows[0]-only code silently dropped: got %v", replyB, clearedIDs(cleared))
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	// Confirm both are actually cleared in the database too, not just in the
	// in-memory returned rows.
	for _, id := range []string{replyA, replyB} {
		var resolvedAt *string
		if err := testPool.QueryRow(ctx, `SELECT resolved_at::text FROM comment WHERE id = $1`, id).Scan(&resolvedAt); err != nil {
			t.Fatalf("query resolved_at for %s: %v", id, err)
		}
		if resolvedAt != nil {
			t.Fatalf("comment %s must be cleared (resolved_at NULL) after commit, got %v", id, *resolvedAt)
		}
	}
}

func clearedIDs(rows []db.Comment) []string {
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = util.UUIDToString(r.ID)
	}
	return ids
}
