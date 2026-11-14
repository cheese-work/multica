-- CHE-4439 S5: the status-only fallback runs captureRouteLabel in a savepoint on
-- the issue write's transaction, so a capture error drops only the label row.

-- name: RouteCaptureSavepoint :exec
SAVEPOINT route_capture;

-- name: RouteCaptureRollbackToSavepoint :exec
ROLLBACK TO SAVEPOINT route_capture;

-- name: RouteCaptureReleaseSavepoint :exec
RELEASE SAVEPOINT route_capture;
