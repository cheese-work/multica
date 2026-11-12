package service

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// LockClosurePasses caps how often LockIssueClosure widens its issue set.
const LockClosurePasses = 5

// LockIssueClosure takes, inside the caller's transaction, every issue a delete
// reaches, before the delete holds any run, comment or agent row it can wait on.
//
// A delete nulls trigger_comment_id and parent_task_id on rows that name what it
// removes, and that update locks the issue of each such row. An authorizer holds
// an issue and then waits on a run row, so a delete that holds a row and then
// asks for a newly found issue can be aborted with 40P01. The closure never does
// that:
//
//  1. discover reads the issues without a lock, and the closure locks them all
//     with LockIssuesByID, ascending, under a savepoint.
//  2. fence takes the rows whose FOR KEY SHARE a new reference needs (the comment
//     or parent run it would name). Once they are held, nothing new can name
//     them, so the next read is complete.
//  3. discover reads again. If it names an issue that is not held, the closure
//     rolls back to the savepoint, which releases the fence rows and every lock
//     taken since, widens the set and starts again from step 1: the larger set
//     is locked in one ascending pass while no fence row is held.
//
// The closure stops after LockClosurePasses passes and fails closed. On success
// the savepoint is released and the issues and fence rows stay locked until the
// transaction ends. discover must be repeatable and fence may be nil. After an
// error the caller rolls the transaction back.
func LockIssueClosure(
	ctx context.Context,
	q *db.Queries,
	discover func(context.Context) ([]pgtype.UUID, error),
	fence func(context.Context) error,
) error {
	if err := q.SavepointLockClosure(ctx); err != nil {
		return fmt.Errorf("open lock closure: %w", err)
	}
	need, err := discover(ctx)
	if err != nil {
		return fmt.Errorf("discover issues: %w", err)
	}
	held := make(map[pgtype.UUID]struct{}, len(need))
	var heldIDs []pgtype.UUID
	for pass := 1; pass <= LockClosurePasses; pass++ {
		for _, id := range need {
			if _, ok := held[id]; !ok {
				held[id] = struct{}{}
				heldIDs = append(heldIDs, id)
			}
		}
		if len(heldIDs) > 0 {
			if err := q.LockIssuesByID(ctx, heldIDs); err != nil {
				return fmt.Errorf("lock issues: %w", err)
			}
		}
		if fence != nil {
			if err := fence(ctx); err != nil {
				return err
			}
		}
		current, err := discover(ctx)
		if err != nil {
			return fmt.Errorf("rediscover issues: %w", err)
		}
		need = need[:0]
		for _, id := range current {
			if _, ok := held[id]; !ok {
				need = append(need, id)
			}
		}
		if len(need) == 0 {
			if err := q.ReleaseLockClosure(ctx); err != nil {
				return fmt.Errorf("close lock closure: %w", err)
			}
			return nil
		}
		if pass == LockClosurePasses {
			break
		}
		if err := q.RollbackToLockClosure(ctx); err != nil {
			return fmt.Errorf("restart lock closure: %w", err)
		}
	}
	return fmt.Errorf("issues kept changing after %d lock passes; refusing to proceed with an issue unlocked", LockClosurePasses)
}
