package decisiontx

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var ErrIntentClosed = errors.New("decision intent window is closed")

func ValidateIntent(ctx context.Context, tx pgx.Tx, intent db.DecisionIntent) error {
	queries := db.New(tx)
	reason, err := queries.DecisionIntentRefusal(ctx, db.DecisionIntentRefusalParams{WorkspaceID: intent.WorkspaceID, ID: intent.ID})
	if err != nil || reason == "" {
		return err
	}
	if reason == "comment_deleted" {
		err = queries.CloseDecisionIntent(ctx, db.CloseDecisionIntentParams{WorkspaceID: intent.WorkspaceID, ID: intent.ID, CloseReason: pgtype.Text{String: reason, Valid: true}})
	} else {
		err = queries.SealDecisionIntent(ctx, db.SealDecisionIntentParams{WorkspaceID: intent.WorkspaceID, ID: intent.ID})
	}
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return fmt.Errorf("%w: %s", ErrIntentClosed, reason)
}
