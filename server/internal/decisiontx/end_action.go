package decisiontx

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// EndAction closes the pending privileged action of a request that was just
// cancelled ('expired' when the cancel reason is expiry, else 'cancelled').
// Call it after the transaction's last lock; it waits on the action row. It
// reports whether an action was pending.
func EndAction(ctx context.Context, queries *db.Queries, request db.DecisionRequest, cancelReason string) (bool, error) {
	action, err := queries.LockPendingDecisionAction(ctx, db.LockPendingDecisionActionParams{WorkspaceID: request.WorkspaceID, DecisionRequestID: request.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	state := "cancelled"
	if cancelReason == "expired" {
		state = "expired"
	}
	if _, err := queries.EndDecisionAction(ctx, db.EndDecisionActionParams{ID: action.ID, WorkspaceID: request.WorkspaceID, State: state}); err != nil {
		return false, err
	}
	details, err := json.Marshal(map[string]string{"decision_request_id": util.UUIDToString(request.ID), "decision_action_id": util.UUIDToString(action.ID), "action_type": action.ActionType, "payload_sha256": action.PayloadSha256})
	if err != nil {
		return false, err
	}
	_, err = queries.CreateActivity(ctx, db.CreateActivityParams{
		WorkspaceID: request.WorkspaceID, IssueID: request.IssueID, ActorType: pgtype.Text{String: "system", Valid: true},
		Action: "decision_action_" + state, Details: details,
	})
	return err == nil, err
}
