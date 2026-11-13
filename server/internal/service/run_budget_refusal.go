package service

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A run-budget refusal is the record of one refused run (CHE-2798 P2r). state
// tracks the resume: open → resume_pending → run_queued | status_restored, with
// still_blocked retryable and superseded terminal. hook_phase tracks the
// exhaustion hook: intent → blocked → done, or retired. The database checks
// both columns; the lookup below treats run_queued, status_restored and
// superseded as terminal.
var (
	ErrRunBudgetRefusalNotFound = errors.New("no open run-budget refusal")
	ErrRunBudgetRefusalMismatch = errors.New("run-budget refusal does not match the issue and window")
)

// FindOpenRunBudgetRefusal returns the id of the issue's non-terminal refusal
// for one window.
func FindOpenRunBudgetRefusal(ctx context.Context, q *db.Queries, issueID pgtype.UUID, windowID string) (pgtype.UUID, error) {
	row, err := q.GetOpenRunBudgetRefusal(ctx, db.GetOpenRunBudgetRefusalParams{IssueID: issueID, WindowID: windowID})
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, ErrRunBudgetRefusalNotFound
	}
	return row.ID, err
}

// RunBudgetRefusalBinds is the approval check: the refusal named in a budget
// action belongs to the action's issue and window.
func RunBudgetRefusalBinds(ctx context.Context, q *db.Queries, refusalID, issueID pgtype.UUID, windowID string) error {
	row, err := q.GetRunBudgetRefusal(ctx, refusalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRunBudgetRefusalMismatch
	}
	if err != nil {
		return err
	}
	if row.IssueID != issueID || row.WindowID != windowID {
		return ErrRunBudgetRefusalMismatch
	}
	return nil
}
