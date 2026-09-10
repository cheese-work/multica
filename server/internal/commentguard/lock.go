// Package commentguard centralizes the thread-scoped advisory lock every
// comment-reply writer must take before its insert commits, so the
// resolve/reply commit-ordering guard (see LockCommentThread in
// pkg/db/queries/comment.sql) cannot be bypassed by a call site that forgot
// to take it. Any code path that inserts a reply comment (parent_id set)
// should route the lock acquisition through LockThreadForReply instead of
// calling GetThreadRoot/LockCommentThread directly.
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
	if !parentID.Valid {
		return nil
	}
	root, err := q.GetThreadRoot(ctx, db.GetThreadRootParams{
		CommentID:   parentID,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		return fmt.Errorf("resolve thread root for reply lock: %w", err)
	}
	if err := q.LockCommentThread(ctx, root.ID); err != nil {
		return fmt.Errorf("lock comment thread: %w", err)
	}
	return nil
}
