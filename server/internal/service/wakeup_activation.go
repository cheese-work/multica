package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Activation of scoped definition writes (CHE-1082 L13). Production holds zero
// scoped definitions until this has run: it backfills customized legacy
// platform-rule rows as issue overrides one bounded page per transaction,
// verifies in a second pass that nothing is left and that nothing but
// backfilled overrides exists, and only then marks itself complete. The
// definition-write gate follows that mark, so no operator step and no client
// action opens it. Capture and execution stay on the legacy path throughout;
// the backfill inserts definition rows and nothing else.

const (
	wakeupActivationName  = "legacy_backfill"
	activationBackfill    = "backfill"
	activationVerify      = "verify"
	activationComplete    = "complete"
	activationPage        = 200
	activationPagesPerRun = 20
)

// ErrActivationVerification means the backfill finished but the database holds
// something the activation does not allow; the gate stays closed.
var ErrActivationVerification = errors.New("wakeup activation verification failed")

// ActivationRequest bounds one run. WorkspaceID, when valid, limits the
// backfill and its verification to one workspace; tests use it.
type ActivationRequest struct {
	Page, MaxPages int32
	WorkspaceID    pgtype.UUID
}

// ActivationResult reports this run. Complete is the persisted state, so a run
// that finds activation already done reports it with nothing scanned.
type ActivationResult struct {
	Complete                   bool
	Scanned, Inserted, Skipped int
}

// wakeupActivationKey is the production activation's name; a test points it at
// its own row so a half-run activation never leaks into another test.
var wakeupActivationKey = wakeupActivationName

func (s *IssueWakeupService) activationName() string {
	if s.ActivationKey != "" {
		return s.ActivationKey
	}
	return wakeupActivationKey
}

// wakeupActivationPending reports whether the activation started and has not
// completed. Until it completes, capture and dispatch ignore stored
// definitions and keep to the legacy path: the backfill commits definitions a
// page at a time, and a half-backfilled set must not change what runs.
func wakeupActivationPending(ctx context.Context, q *db.Queries) (bool, error) {
	row, err := q.GetWakeupDefinitionActivation(ctx, wakeupActivationKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil && row.State != activationComplete, err
}

// WakeupDefinitionsActivated reports whether the activation completed. A
// missing row is not activated, and reading never creates it.
func (s *IssueWakeupService) WakeupDefinitionsActivated(ctx context.Context) (bool, error) {
	row, err := s.Tasks.Queries.GetWakeupDefinitionActivation(ctx, s.activationName())
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil && row.State == activationComplete, err
}

// RunWakeupActivation is the scheduler's bounded step: it advances the
// production activation by a few pages and logs the outcome.
func (s *IssueWakeupService) RunWakeupActivation(ctx context.Context) error {
	res, err := s.ActivateWakeupDefinitions(ctx, ActivationRequest{Page: activationPage, MaxPages: activationPagesPerRun})
	if err != nil {
		return err
	}
	if res.Scanned > 0 || res.Inserted > 0 {
		slog.Info("wakeup definition activation", "complete", res.Complete, "scanned", res.Scanned, "inserted", res.Inserted)
	}
	return nil
}

// ActivateWakeupDefinitions advances the activation by at most MaxPages pages.
// Each page commits with its cursor, so an interrupted or concurrent run
// resumes where the last committed page stopped and never repeats one. A
// failure leaves the gate closed and is recorded for the next attempt.
func (s *IssueWakeupService) ActivateWakeupDefinitions(ctx context.Context, req ActivationRequest) (ActivationResult, error) {
	if req.Page < 1 || req.MaxPages < 1 {
		return ActivationResult{}, fmt.Errorf("%w: activation needs a positive page size and page count", ErrWakeupInput)
	}
	var total ActivationResult
	for range req.MaxPages {
		page, err := s.activationStep(ctx, req)
		total.Scanned += page.Scanned
		total.Inserted += page.Inserted
		total.Skipped += page.Skipped
		total.Complete = page.Complete
		if err != nil {
			s.recordActivationError(ctx, err)
			return total, err
		}
		if page.Complete {
			return total, nil
		}
	}
	return total, nil
}

// activationStep is one page in one transaction.
func (s *IssueWakeupService) activationStep(ctx context.Context, req ActivationRequest) (ActivationResult, error) {
	name := s.activationName()
	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return ActivationResult{}, err
	}
	defer tx.Rollback(ctx)
	q := s.Tasks.Queries.WithTx(tx)
	if err := q.EnsureWakeupDefinitionActivation(ctx, name); err != nil {
		return ActivationResult{}, err
	}
	row, err := q.LockWakeupDefinitionActivation(ctx, name)
	if err != nil {
		return ActivationResult{}, err
	}
	if row.State == activationComplete {
		return ActivationResult{Complete: true}, nil
	}
	page, err := backfillLegacyPage(ctx, q, LegacyBackfillRequest{After: row.Cursor, WorkspaceID: req.WorkspaceID, Limit: req.Page})
	if err != nil {
		return ActivationResult{}, err
	}
	out := ActivationResult{Scanned: page.Scanned, Inserted: page.Inserted, Skipped: page.Skipped}
	next := db.AdvanceWakeupDefinitionActivationParams{
		Name: name, State: row.State, Cursor: page.Next, Scanned: row.Scanned + int64(page.Scanned), Inserted: row.Inserted + int64(page.Inserted),
	}
	var failure error
	switch {
	case !page.Done:
	case row.State == activationBackfill || page.Inserted > 0:
		// The first pass is done, or a row was customized while the verifying
		// pass ran: either way another full pass must find nothing to insert.
		next.State, next.Cursor = activationVerify, pgtype.UUID{}
	default:
		if beyond, err := q.CountWakeupDefinitionsBeyondBackfill(ctx, req.WorkspaceID); err != nil {
			return ActivationResult{}, err
		} else if beyond > 0 {
			failure = fmt.Errorf("%w: %d definitions are not backfilled issue overrides", ErrActivationVerification, beyond)
			next.State, next.Cursor = activationVerify, pgtype.UUID{}
			next.LastError = pgtype.Text{String: failure.Error(), Valid: true}
		} else {
			next.State, out.Complete = activationComplete, true
		}
	}
	if err := q.AdvanceWakeupDefinitionActivation(ctx, next); err != nil {
		return ActivationResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ActivationResult{}, err
	}
	return out, failure
}

// recordActivationError keeps why a pass could not finish. It is best effort:
// the gate is closed either way, and a lost note only costs the diagnosis.
func (s *IssueWakeupService) recordActivationError(ctx context.Context, cause error) {
	if errors.Is(cause, ErrActivationVerification) || ctx.Err() != nil {
		return
	}
	_ = s.Tasks.Queries.EnsureWakeupDefinitionActivation(ctx, s.activationName())
	if err := s.Tasks.Queries.NoteWakeupDefinitionActivationError(ctx, db.NoteWakeupDefinitionActivationErrorParams{
		Name: s.activationName(), LastError: pgtype.Text{String: cause.Error(), Valid: true},
	}); err != nil {
		slog.Warn("wakeup activation: could not record failure", "error", err, "cause", cause)
	}
}
