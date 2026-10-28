package service

import (
	"context"
	"errors"
	"fmt"

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
