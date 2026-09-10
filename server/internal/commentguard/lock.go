// Package commentguard centralizes the thread-scoped advisory lock every
// comment-reply writer must take before its insert commits, so the
// resolve/reply commit-ordering guard (see LockCommentThread in
// pkg/db/queries/comment.sql) cannot be bypassed by a call site that forgot
// to take it. Any code path that inserts a reply comment (parent_id set)
// should route the lock acquisition through LockThreadForReply (or
// LockThreadForReplyAndClearResolution, when the call site also needs to
// reopen a resolved thread in the same transaction) instead of calling
// GetThreadRoot/LockCommentThread directly.
package commentguard

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// LockThreadForReply takes the thread-scoped advisory lock
// (LockCommentThread) for the thread a reply is about to land in, keyed by
// the thread ROOT so it contends with both CreateComment's other reply
// writers and ResolveComment's concurrent-feedback guard, which lock by the
// same root.
//
// q must be a *db.Queries bound to the caller's transaction (q.WithTx(tx)):
// the lock is an xact-scoped pg_advisory_xact_lock, so acquiring it outside
// the transaction that performs the insert would release it before the
// insert's commit and defeat the guarantee entirely.
//
// parentID is the comment being replied to. An invalid (zero) parentID means
// a top-level comment — those can never be "the reply that lands after a
// guard checked" for a thread that already existed, so this is a deliberate
// no-op rather than an error.
//
// A present parentID that fails to resolve to a thread root is NOT silently
// skipped: it fails closed and returns the error, because Round 2 introduced
// this lock specifically to close a concurrency window, and silently
// continuing without the lock on a lookup error would reopen exactly that
// window for a code path that never notices it happened.
func LockThreadForReply(ctx context.Context, q *db.Queries, workspaceID, parentID pgtype.UUID) error {
	_, _, err := LockThreadForReplyAndClearResolution(ctx, q, workspaceID, parentID)
	return err
}

// LockThreadForReplyAndLoadRoot does everything LockThreadForReply does, and
// also returns the resolved thread root comment so callers that need it for
// thread-level side effects (e.g. publishing comment:unresolved with the
// thread's WorkspaceID) don't have to duplicate the GetThreadRoot lookup.
//
// The returned root is nil for a top-level comment (invalid parentID),
// matching LockThreadForReply's no-op semantics for that case.
//
// Deprecated in favor of LockThreadForReplyAndClearResolution for any caller
// that also needs the reopen-on-reply behavior — this variant does NOT clear
// a resolved thread. Kept only as a thin wrapper so its doc comment remains a
// stable link target; new call sites should use
// LockThreadForReplyAndClearResolution directly.
func LockThreadForReplyAndLoadRoot(ctx context.Context, q *db.Queries, workspaceID, parentID pgtype.UUID) (*db.Comment, error) {
	root, err := loadThreadRootAndLock(ctx, q, workspaceID, parentID)
	return root, err
}

// LockThreadForReplyAndClearResolution takes the thread-scoped advisory lock
// exactly as LockThreadForReply does, and — still inside that same lock, still
// before the caller's transaction commits — clears any resolution currently
// held by ANY comment in the thread (the root or a reply; a thread has at
// most one resolved comment at a time, enforced by ClearOtherThreadResolutions'
// single-resolution invariant, so ClearThreadResolutionForReply finds at most
// one row to clear).
//
// This replaces the Round 4 design where the reopen happened via
// TaskService.AutoUnresolveThreadOnReply AFTER the reply's transaction
// committed, through an unlocked *db.Queries handle. That post-commit write
// was itself racy: a ResolveComment call could acquire the lock and commit a
// fresh, legitimate resolution in the gap between the reply's commit and the
// stale post-commit unresolve call, which would then incorrectly clear that
// later resolution — violating commit ordering. Moving the clear inside the
// SAME transaction that holds the lock and inserts the reply makes that
// window structurally impossible: nothing else can observe or mutate the
// thread's resolution state while this transaction holds the advisory lock,
// and the clear commits atomically with the reply itself.
//
// It also fixes the root-only limitation of the old design: a resolved REPLY
// (not the thread root) is now reopened too, matching the actual
// single-resolution invariant instead of assuming the root is always the
// resolved comment.
//
// Returns the thread root (nil for a top-level comment, matching
// LockThreadForReply's no-op semantics) and every comment row that was
// cleared (empty when nothing in the thread was resolved). Callers publish
// comment:unresolved for EACH cleared row AFTER their transaction commits —
// event publishing after commit is fine and is the existing pattern for
// comment:created too; only the database write had to move inside the tx.
//
// ClearThreadResolutionForReply is a :many query. Under today's
// single-resolution invariant (enforced by ClearOtherThreadResolutions) a
// thread has at most one resolved comment at a time, so in practice this
// slice holds at most one row — but the invariant is enforced by a separate
// code path, not by this function's SQL, so this helper (and every caller)
// handles the general N-row case defensively rather than assuming len(rows)
// <= 1 and silently discarding anything beyond the first row.
func LockThreadForReplyAndClearResolution(ctx context.Context, q *db.Queries, workspaceID, parentID pgtype.UUID) (root *db.Comment, cleared []db.Comment, err error) {
	root, err = loadThreadRootAndLock(ctx, q, workspaceID, parentID)
	if err != nil || root == nil {
		return root, nil, err
	}
	rows, err := q.ClearThreadResolutionForReply(ctx, db.ClearThreadResolutionForReplyParams{
		ThreadRootID: root.ID,
		IssueID:      root.IssueID,
		WorkspaceID:  workspaceID,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("clear thread resolution for reply: %w", err)
	}
	return root, rows, nil
}

// loadThreadRootAndLock resolves parentID's thread root and takes the
// thread-scoped advisory lock on it. Returns (nil, nil) for a top-level
// comment (invalid parentID) — the shared no-op case.
func loadThreadRootAndLock(ctx context.Context, q *db.Queries, workspaceID, parentID pgtype.UUID) (*db.Comment, error) {
	if !parentID.Valid {
		return nil, nil
	}
	root, err := q.GetThreadRoot(ctx, db.GetThreadRootParams{
		CommentID:   parentID,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve thread root for reply lock: %w", err)
	}
	if err := q.LockCommentThread(ctx, root.ID); err != nil {
		return nil, fmt.Errorf("lock comment thread: %w", err)
	}
	return &root, nil
}
