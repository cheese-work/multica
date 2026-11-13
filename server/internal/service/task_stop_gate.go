package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// FailureReasonSupersededHead marks a run cancelled because a newer head
// replaced it. Only such a cancel arms the stop gate.
const FailureReasonSupersededHead = "superseded_head"

// StopNotConfirmedError: a superseded run at the issue's current step has no
// daemon cancel-ack yet, so its runner may still be active.
type StopNotConfirmedError struct{ TaskID pgtype.UUID }

func (e *StopNotConfirmedError) Error() string {
	return fmt.Sprintf("stop not confirmed for run %s", util.UUIDToString(e.TaskID))
}

// StopGate is the one stop-gate check. It returns nil (open) or a
// *StopNotConfirmedError (closed). The gate opens only when the cancelled run's
// cancel_acked_at is set: no member, agent or timer releases it, and a runtime
// being offline is not proof the runner stopped.
func StopGate(ctx context.Context, q *db.Queries, issueID pgtype.UUID) error {
	taskID, err := q.GetUnackedSupersededRun(ctx, issueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		// A gate that cannot be read stays closed: a run that cannot be shown
		// to be safe must not start.
		return fmt.Errorf("stop gate: %w", err)
	}
	return &StopNotConfirmedError{TaskID: taskID}
}

// StopReleaseDaemonReconcile is the reason recorded when the stop gate opened
// on the claiming daemon's live task set instead of a per-task cancel-ack.
const StopReleaseDaemonReconcile = "daemon_reconcile"

// daemonReconcileGrace keeps a run out of reach of a live-set report for this
// long after dispatch. The report is built before the claim response reaches
// the daemon registers the task, so a younger run being absent proves nothing.
const daemonReconcileGrace = 2 * time.Minute

// ReleaseStopsAttestedByDaemon opens the gate for gate-arming cancelled, unacked
// runs of runtimeID that the daemon's live task set no longer holds, and
// returns the issues whose gate it opened. Only an online runtime attests; the
// caller passes a set only from a daemon that reports one (a daemon that does
// not report it must not call this: an empty set means idle, not unknown).
// Only runs dispatched since the daemon process booted (daemonUptime ago) are
// attested: a restarted daemon starts with an empty set while the runner of a
// run it did not start can outlive it (independent process groups). The boot
// time is derived from the uptime on the server clock; transit delay can only
// move it later, which keeps more runs unattested.
// An id that does not parse is dropped, which can only keep the gate closed.
func ReleaseStopsAttestedByDaemon(ctx context.Context, q *db.Queries, runtimeID pgtype.UUID, liveTaskIDs []string, daemonUptime time.Duration) ([]pgtype.UUID, error) {
	live := make([]pgtype.UUID, 0, len(liveTaskIDs))
	for _, id := range liveTaskIDs {
		if u, err := util.ParseUUID(id); err == nil {
			live = append(live, u)
		}
	}
	now := time.Now()
	rows, err := q.ReleaseStopsAttestedByDaemon(ctx, db.ReleaseStopsAttestedByDaemonParams{
		RuntimeID:        runtimeID,
		DispatchedBefore: pgtype.Timestamptz{Time: now.Add(-daemonReconcileGrace), Valid: true},
		DispatchedAfter:  pgtype.Timestamptz{Time: now.Add(-daemonUptime), Valid: true},
		LiveTaskIds:      live,
	})
	if err != nil {
		return nil, fmt.Errorf("daemon stop reconcile: %w", err)
	}
	issues := make([]pgtype.UUID, 0, len(rows))
	for _, r := range rows {
		if r.IssueID.Valid {
			issues = append(issues, r.IssueID)
		}
	}
	return issues, nil
}
