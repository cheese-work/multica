package ciwatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ErrWorkspaceGone means the workspace was deleted before the CI rows could be
// written. Nothing is stored; the delivery fails and the retry stops at the
// webhook's own workspace lookup.
var ErrWorkspaceGone = errors.New("ciwatch: workspace no longer exists")

// WorkspaceLocker takes a KEY SHARE lock on the workspace row until the
// transaction ends, and returns pgx.ErrNoRows when the workspace is gone.
type WorkspaceLocker interface {
	LockWorkspaceForCIWrite(ctx context.Context, id pgtype.UUID) (pgtype.UUID, error)
}

// Writers are the writes of one run, valid inside one InTx call.
type Writers struct {
	Workspace    WorkspaceLocker
	Events       EventWriter
	Observations ObservationWriter
}

// InTx runs fn in one database transaction: every write fn makes commits, or
// none does.
type InTx func(ctx context.Context, fn func(Writers) error) error

// Pipeline is the event path for one completed workflow run: collect evidence,
// then store the failure event and the System 1 observation together.
type Pipeline struct {
	Collector *Collector
	Tx        InTx
	Observer  Observer
}

// Process handles one delivery. The event and the observation commit in one
// transaction, so a failure leaves neither behind and the retry makes its own
// first decision from fresh state; a stored event and its observation can
// never come from two different collections. A redelivery of a committed run
// recollects and then writes nothing (both inserts are idempotent).
func (p Pipeline) Process(ctx context.Context, in Input, h History) (Result, error) {
	res, err := p.Collector.Handle(ctx, in)
	if err != nil {
		return res, err
	}
	obs, err := p.Observer.Build(ctx, in.Run, res, h)
	if err != nil {
		return res, err
	}
	if obs == nil && res.Kind != KindEvidence {
		return res, nil
	}
	var ws pgtype.UUID
	if err := ws.Scan(in.Run.WorkspaceID); err != nil {
		return res, fmt.Errorf("ciwatch: workspace id: %w", err)
	}
	if err := p.Tx(ctx, func(w Writers) error {
		// Neither table has a foreign key to workspace. The KEY SHARE lock holds
		// the row until commit, so a delete that wins the race makes this fail
		// before any insert, and one that comes later waits for the commit.
		if _, err := w.Workspace.LockWorkspaceForCIWrite(ctx, ws); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrWorkspaceGone
			}
			return err
		}
		if res.Kind == KindEvidence {
			if _, err := Store(ctx, w.Events, res.Event); err != nil {
				return err
			}
		}
		if obs == nil {
			return nil
		}
		_, err := w.Observations.CreateSystem1CIObservation(ctx, *obs)
		return err
	}); err != nil {
		return res, err
	}
	// The label needs the committed row.
	return res, p.Observer.applyLabel(ctx, in.Run, res)
}
