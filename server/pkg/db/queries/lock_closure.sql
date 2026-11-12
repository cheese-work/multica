-- Savepoint bracket of service.LockIssueClosure. ROLLBACK TO SAVEPOINT releases
-- every row lock taken after the savepoint, which is what lets the closure
-- restart with a larger, sorted issue set instead of asking for a newly found
-- issue while it holds run or comment rows.

-- name: SavepointLockClosure :exec
SAVEPOINT lock_closure;

-- name: RollbackToLockClosure :exec
ROLLBACK TO SAVEPOINT lock_closure;

-- name: ReleaseLockClosure :exec
RELEASE SAVEPOINT lock_closure;
