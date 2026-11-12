package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// MaxPrelockRestarts bounds how often a delete's issue pre-lock starts over
// because a reference created while it ran named another issue.
const MaxPrelockRestarts = 3

// ErrPrelockUnstable means the set of issues a delete must lock kept changing
// through every restart. The delete fails closed instead of locking an issue on
// top of the locks it holds.
var ErrPrelockUnstable = errors.New("delete pre-lock: the set of issues to lock kept changing")

// MergeSortedUUIDs returns the union of two id sets in ascending byte order.
func MergeSortedUUIDs(a, b []pgtype.UUID) []pgtype.UUID {
	out := append(append([]pgtype.UUID(nil), a...), b...)
	slices.SortFunc(out, func(x, y pgtype.UUID) int { return bytes.Compare(x.Bytes[:], y.Bytes[:]) })
	return slices.CompactFunc(out, func(x, y pgtype.UUID) bool { return x.Bytes == y.Bytes })
}

// prelockPlan describes one delete: the issues its cascades will lock, and the
// rows a late reference to the removed rows would have to point at.
type prelockPlan struct {
	list   func(ctx context.Context) ([]pgtype.UUID, error)
	freeze func(ctx context.Context) error
}

// lockIssuesBeforeDelete takes the issue locks a legacy-FK delete needs before
// the delete touches any run row. AuthorizeElevatedOp holds an issue and then
// waits on the run row; a delete that holds run rows and then asks for an issue
// aborts one side (40P01).
//
// Under READ COMMITTED each statement has its own snapshot, so a list read
// before the locks can miss a reference created in between. After locking, the
// plan freezes the referenced rows (FOR UPDATE conflicts with the key-share
// lock of a foreign key check, so no new reference can land) and lists again.
// A new issue in the second list is never locked on top of what is held: the
// savepoint rolls back, which releases every lock taken since it, and the
// attempt starts over with the full set in ascending order.
//
// The caller must hold no run row when it calls this, and no issue lock it takes
// itself after a savepoint it still needs: locks taken before the savepoint stay.
func lockIssuesBeforeDelete(ctx context.Context, q *db.Queries, plan prelockPlan) error {
	var known []pgtype.UUID
	for attempt := 0; attempt <= MaxPrelockRestarts; attempt++ {
		if err := q.PrelockSavepoint(ctx); err != nil {
			return fmt.Errorf("prelock savepoint: %w", err)
		}
		first, err := plan.list(ctx)
		if err != nil {
			return fmt.Errorf("list issues: %w", err)
		}
		ids := MergeSortedUUIDs(known, first)
		if err := q.LockIssuesByID(ctx, ids); err != nil {
			return fmt.Errorf("lock issues: %w", err)
		}
		if err := plan.freeze(ctx); err != nil {
			return fmt.Errorf("freeze referenced rows: %w", err)
		}
		second, err := plan.list(ctx)
		if err != nil {
			return fmt.Errorf("list issues after freeze: %w", err)
		}
		merged := MergeSortedUUIDs(ids, second)
		if len(merged) == len(ids) {
			if err := q.PrelockReleaseSavepoint(ctx); err != nil {
				return fmt.Errorf("prelock release: %w", err)
			}
			return nil
		}
		known = merged
		if err := q.PrelockRollbackToSavepoint(ctx); err != nil {
			return fmt.Errorf("prelock rollback: %w", err)
		}
		if err := q.PrelockReleaseSavepoint(ctx); err != nil {
			return fmt.Errorf("prelock release: %w", err)
		}
	}
	return ErrPrelockUnstable
}

// LockIssuesBeforeIssueDelete locks the deleted issues and the issues their
// comment and run cascades reach.
func LockIssuesBeforeIssueDelete(ctx context.Context, q *db.Queries, issueIDs []pgtype.UUID) error {
	return lockIssuesBeforeDelete(ctx, q, prelockPlan{
		list: func(ctx context.Context) ([]pgtype.UUID, error) { return q.ListIssuesOfIssueDelete(ctx, issueIDs) },
		freeze: func(ctx context.Context) error {
			if err := q.FreezeCommentsOfIssues(ctx, issueIDs); err != nil {
				return err
			}
			return q.FreezeRunsOfIssues(ctx, issueIDs)
		},
	})
}

// LockIssuesBeforeCommentDelete locks the issues of the comment, its ancestors
// and the runs that name them.
func LockIssuesBeforeCommentDelete(ctx context.Context, q *db.Queries, commentID, workspaceID pgtype.UUID) error {
	return lockIssuesBeforeDelete(ctx, q, prelockPlan{
		list: func(ctx context.Context) ([]pgtype.UUID, error) {
			return q.ListIssuesOfCommentDelete(ctx, db.ListIssuesOfCommentDeleteParams{ID: commentID, WorkspaceID: workspaceID})
		},
		freeze: func(ctx context.Context) error {
			return q.FreezeCommentChain(ctx, db.FreezeCommentChainParams{ID: commentID, WorkspaceID: workspaceID})
		},
	})
}

// LockIssuesBeforeSystemAgentDelete locks the issues of the children of the
// runs of a session-owned system agent. Any other agent locks nothing.
func LockIssuesBeforeSystemAgentDelete(ctx context.Context, q *db.Queries, agentID pgtype.UUID) error {
	return lockIssuesBeforeDelete(ctx, q, prelockPlan{
		list: func(ctx context.Context) ([]pgtype.UUID, error) {
			return q.ListIssuesOfAgentBuilderTaskChildren(ctx, agentID)
		},
		freeze: func(ctx context.Context) error {
			if err := q.FreezeAgentBuilderAgent(ctx, agentID); err != nil {
				return err
			}
			return q.FreezeRunsOfAgentBuilderAgent(ctx, agentID)
		},
	})
}

// LockIssuesBeforeRuntimeSystemAgentsDelete is the same for every system agent
// of a runtime.
func LockIssuesBeforeRuntimeSystemAgentsDelete(ctx context.Context, q *db.Queries, runtimeID pgtype.UUID) error {
	return lockIssuesBeforeDelete(ctx, q, prelockPlan{
		list: func(ctx context.Context) ([]pgtype.UUID, error) {
			return q.ListIssuesOfSystemAgentTaskChildrenByRuntime(ctx, runtimeID)
		},
		freeze: func(ctx context.Context) error {
			if err := q.FreezeSystemAgentsOfRuntime(ctx, runtimeID); err != nil {
				return err
			}
			return q.FreezeRunsOfSystemAgentsOfRuntime(ctx, runtimeID)
		},
	})
}
