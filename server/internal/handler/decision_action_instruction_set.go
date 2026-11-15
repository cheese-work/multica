package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// squad.instructions.set and workspace.context.set are the owner-only config
// changes that replace one whole text field. The params name the new text and
// the digests the agent built it on; the server recomputes both from the live
// row (decisionaction.TextDigest), so the agent's digests are only compared.

// squadInstructionTokenLimit is the limit the ordinary squad edit enforces.
const squadInstructionTokenLimit = 3000

// textSetParams re-reads the stored params; the request already normalised them,
// so a failure here means the stored row is not one this action wrote.
func textSetParams(raw json.RawMessage, idKey, contentKey string) (decisionaction.TextSet, error) {
	set, err := decisionaction.ParseTextSet(raw, idKey, contentKey)
	if err != nil {
		return set, decisionIntentRefusal{http.StatusBadRequest, decisionaction.CodeInvalidParams + ": " + err.Error()}
	}
	return set, nil
}

// textSetRead builds the read of a whole-text change from the live and new text.
func textSetRead(field, targetID, live, after string) configChangeRead {
	return configChangeRead{
		Field: field, TargetID: targetID,
		Before: live, After: after,
		LiveSHA256: decisionaction.TextDigest(live), AfterSHA256: decisionaction.TextDigest(after),
		HumanBlocksIntact: protectedBlocksUnchanged(live, after),
	}
}

func init() {
	registerConfigChange("squad.instructions.set", configChange{
		Fields: []string{"squad.instructions"},
		Lock: func(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, params json.RawMessage) error {
			_, err := loadSquadTarget(ctx, queries, workspaceID, params, http.StatusConflict, true)
			return err
		},
		Read: func(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, params json.RawMessage) (configChangeRead, error) {
			set, err := textSetParams(params, "squad_id", "instructions")
			if err != nil {
				return configChangeRead{}, err
			}
			squad, err := loadSquadTarget(ctx, queries, workspaceID, params, http.StatusBadRequest, false)
			if err != nil {
				return configChangeRead{}, err
			}
			if err := squadInstructionsWithinLimit(squad.Instructions, set.Content); err != nil {
				return configChangeRead{}, err
			}
			return textSetRead("squad.instructions", uuidToString(squad.ID), squad.Instructions, set.Content), nil
		},
		Apply: func(ctx context.Context, execution decisionActionExecution, read configChangeRead) error {
			set, err := textSetParams(execution.Action.Params, "squad_id", "instructions")
			if err != nil {
				return err
			}
			id, _ := util.ParseUUID(set.ID)
			_, err = execution.Queries.UpdateSquad(ctx, db.UpdateSquadParams{ID: id, Instructions: pgtype.Text{String: set.Content, Valid: true}})
			return err
		},
		Publish: func(ctx context.Context, handler *Handler, action db.DecisionAction) error {
			set, err := textSetParams(action.Params, "squad_id", "instructions")
			if err != nil {
				return err
			}
			id, _ := util.ParseUUID(set.ID)
			squad, err := handler.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: id, WorkspaceID: action.WorkspaceID})
			if err != nil {
				return err
			}
			resp, err := handler.squadToResponseWithPreview(ctx, squad)
			if err != nil {
				return err
			}
			handler.publish(protocol.EventSquadUpdated, uuidToString(action.WorkspaceID), "member", uuidToString(action.ApprovedByUserID), map[string]any{"squad": resp})
			return nil
		},
	})
}

// squadInstructionsWithinLimit is the ordinary edit's rule: text over the token
// limit is refused unless it leaves an already-oversized value no larger.
func squadInstructionsWithinLimit(live, after string) error {
	newTokens := util.EstimateInstructionTokens(after)
	if newTokens <= squadInstructionTokenLimit || after == live {
		return nil
	}
	if liveTokens := util.EstimateInstructionTokens(live); liveTokens <= squadInstructionTokenLimit || newTokens > liveTokens {
		return decisionIntentRefusal{http.StatusUnprocessableEntity, fmt.Sprintf("squad.instructions exceed limit of %d tokens (estimated: %d tokens)", squadInstructionTokenLimit, newTokens)}
	}
	return nil
}
