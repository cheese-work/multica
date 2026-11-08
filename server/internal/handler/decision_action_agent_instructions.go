package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	agentInstructionsAction = "agent.instructions.set"
	agentInstructionsField  = "agent.instructions"
	// agentInstructionsTokenLimit matches the limit of the agent update endpoint.
	agentInstructionsTokenLimit = 4000
)

func init() {
	registerConfigChange(agentInstructionsAction, configChange{
		Fields:     []string{agentInstructionsField},
		Lock:       lockAgentInstructionsTarget,
		Read:       readAgentInstructions,
		Apply:      applyAgentInstructions,
		Publish:    publishAgentInstructionsSet,
		QueueField: agentInstructionsField,
	})
}

// loadAgentInstructionsTarget resolves params.agent_id in the workspace; a
// missing and a foreign agent get the same answer.
func loadAgentInstructionsTarget(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, params json.RawMessage) (db.Agent, error) {
	id, ok := targetParamID(params, "agent_id")
	if !ok {
		return db.Agent{}, errAgentTargetOutside
	}
	agent, err := queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: id, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Agent{}, errAgentTargetOutside
	}
	return agent, err
}

var errAgentTargetOutside = decisionIntentRefusal{http.StatusForbidden, "target_workspace: the target agent is not in this workspace"}

func lockAgentInstructionsTarget(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, params json.RawMessage) error {
	agent, err := loadAgentInstructionsTarget(ctx, queries, workspaceID, params)
	if err != nil {
		return err
	}
	_, err = queries.LockDecisionAgentsNowait(ctx, db.LockDecisionAgentsNowaitParams{WorkspaceID: workspaceID, Ids: []pgtype.UUID{agent.ID}})
	return err
}

func readAgentInstructions(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, params json.RawMessage) (configChangeRead, error) {
	agent, err := loadAgentInstructionsTarget(ctx, queries, workspaceID, params)
	if err != nil {
		return configChangeRead{}, err
	}
	if agent.ArchivedAt.Valid {
		return configChangeRead{}, decisionIntentRefusal{http.StatusConflict, "agent_archived: the target agent is archived"}
	}
	var wanted decisionaction.AgentInstructionsSetParams
	if err := json.Unmarshal(params, &wanted); err != nil || wanted.Instructions == nil {
		return configChangeRead{}, decisionIntentRefusal{http.StatusBadRequest, "invalid_params: params.instructions is required"}
	}
	live, after := agent.Instructions, *wanted.Instructions
	if err := checkAgentInstructionsSize(live, after); err != nil {
		return configChangeRead{}, err
	}
	read := textConfigRead(agentInstructionsField, uuidToString(agent.ID), live, after)
	read.HumanBlocksIntact = protectedBlocksUnchanged(live, after)
	return read, nil
}

// checkAgentInstructionsSize applies the agent update endpoint's limit: a value
// over it is still accepted when it shrinks an already oversized value.
func checkAgentInstructionsSize(live, after string) error {
	tokens := util.EstimateInstructionTokens(after)
	if tokens <= agentInstructionsTokenLimit || after == live {
		return nil
	}
	if liveTokens := util.EstimateInstructionTokens(live); liveTokens > agentInstructionsTokenLimit && tokens <= liveTokens {
		return nil
	}
	return decisionIntentRefusal{http.StatusUnprocessableEntity, "instructions_too_long: instructions exceed the limit of 4000 tokens"}
}

func applyAgentInstructions(ctx context.Context, execution decisionActionExecution, read configChangeRead) error {
	id, err := parseUUIDStrict(read.TargetID)
	if err != nil {
		return err
	}
	after, _ := read.After.(string)
	_, err = execution.Queries.UpdateAgent(ctx, db.UpdateAgentParams{ID: id, Instructions: pgtype.Text{String: after, Valid: true}})
	return err
}

func publishAgentInstructionsSet(ctx context.Context, handler *Handler, action db.DecisionAction) error {
	id, ok := targetParamID(action.Params, "agent_id")
	if !ok {
		return nil
	}
	agent, err := handler.Queries.GetAgent(ctx, id)
	if err != nil {
		return err
	}
	return handler.publishAgentEnvUpdated(ctx, agent, action.ApprovedByUserID)
}
