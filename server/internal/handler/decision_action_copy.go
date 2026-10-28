package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/agentconfig"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/pkg/agent"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/jev"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

var errCopySourceNotVisible = decisionIntentRefusal{http.StatusForbidden, "source_not_visible: you cannot view the agent being copied"}

// executeAgentCopyAction is the agent.copy core: the same fields and checks as
// CreateAgent for a copy, owned by the human who approved. Secrets (custom_env)
// and MCP config never travel with a copy.
func executeAgentCopyAction(ctx context.Context, execution decisionActionExecution) (json.RawMessage, error) {
	var params decisionaction.AgentCopyParams
	if err := json.Unmarshal(execution.Action.Params, &params); err != nil {
		return nil, err
	}
	source := execution.Agent
	if source.ArchivedAt.Valid {
		return nil, actionFailure{Code: "agent_archived", Message: "the source agent is archived"}
	}
	member, err := execution.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID: execution.Approver.UserID, WorkspaceID: source.WorkspaceID,
	})
	if err != nil {
		return nil, err
	}
	targets, err := execution.Queries.ListAgentInvocationTargets(ctx, source.ID)
	if err != nil {
		return nil, err
	}
	skills, err := execution.Queries.ListAgentSkills(ctx, source.ID)
	if err != nil {
		return nil, err
	}
	// The approver may only copy what the normal view rule shows them, and
	// only the settings they were shown at request time.
	if !memberAllowedToViewAgent(source, targets, uuidToString(execution.Approver.UserID), member.Role) {
		return nil, errCopySourceNotVisible
	}
	if digest, err := digestAgentCopySettings(source, targets, skills); err != nil {
		return nil, err
	} else if drifted, err := copySettingsDrifted(execution.Action, digest); err != nil {
		return nil, err
	} else if drifted {
		return nil, errPreconditionDrift
	}
	runtime := execution.Runtime
	if !runtime.ID.Valid || runtime.WorkspaceID != source.WorkspaceID || !canUseRuntimeForAgent(member, runtime) {
		return nil, actionFailure{Code: "runtime_unavailable", Message: "the target runtime is missing or private to another member"}
	}
	instructions := source.Instructions
	if params.Instructions != nil {
		instructions = *params.Instructions
	}
	if tokens := jev.EstimateInstructionTokens(instructions); tokens > 4000 {
		return nil, actionFailure{Code: "instructions_too_long", Message: fmt.Sprintf("agent instructions exceed limit of 4000 tokens (estimated: %d tokens)", tokens)}
	}
	if utf8.RuneCountInString(params.Description) > maxAgentDescriptionLength {
		return nil, actionFailure{Code: "description_too_long", Message: fmt.Sprintf("description must be %d characters or fewer", maxAgentDescriptionLength)}
	}
	starters := make([]AgentConversationStarter, len(params.ConversationStarters))
	for i, starter := range params.ConversationStarters {
		starters[i] = AgentConversationStarter(starter)
	}
	starters, err = normaliseAgentConversationStarters(starters)
	if err != nil {
		return nil, actionFailure{Code: "invalid_conversation_starters", Message: err.Error()}
	}
	if message := rejectedRuntimeOption(runtime.Provider, params); message != "" {
		return nil, actionFailure{Code: "invalid_runtime_option", Message: message}
	}
	avatar, err := copiedAvatar(source, params.AvatarURL)
	if err != nil {
		return nil, err
	}
	permission := resolvedPermission{mode: source.PermissionMode}
	for _, target := range targets {
		permission.targets = append(permission.targets, targetSpec{targetType: target.TargetType, targetID: target.TargetID})
	}
	startersJSON, err := json.Marshal(starters)
	if err != nil {
		return nil, err
	}
	customArgs, err := json.Marshal(params.CustomArgs)
	if err != nil {
		return nil, err
	}
	maxConcurrent := params.MaxConcurrentTasks
	if maxConcurrent == 0 {
		maxConcurrent = agentconfig.DefaultMaxConcurrentTasks
	}
	created, err := execution.Queries.CreateAgent(ctx, db.CreateAgentParams{
		WorkspaceID:          source.WorkspaceID,
		Name:                 params.Name,
		Description:          params.Description,
		Instructions:         instructions,
		AvatarUrl:            avatar,
		RuntimeMode:          runtime.RuntimeMode,
		RuntimeConfig:        []byte("{}"),
		RuntimeID:            runtime.ID,
		Visibility:           permission.legacyVisibility(),
		PermissionMode:       permission.mode,
		MaxConcurrentTasks:   maxConcurrent,
		OwnerID:              execution.Approver.UserID,
		CustomEnv:            []byte("{}"),
		CustomArgs:           customArgs,
		Model:                pgtype.Text{String: params.Model, Valid: params.Model != ""},
		ThinkingLevel:        pgtype.Text{String: params.ThinkingLevel, Valid: params.ThinkingLevel != ""},
		ServiceTier:          pgtype.Text{String: params.ServiceTier, Valid: params.ServiceTier != ""},
		ConversationStarters: startersJSON,
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "agent_workspace_name_unique" {
		return nil, actionFailure{Code: "name_taken", Message: fmt.Sprintf("an agent named %q already exists in this workspace", params.Name)}
	}
	if err != nil {
		return nil, err
	}
	if err := replaceInvocationTargetsWithQueries(ctx, execution.Queries, created.ID, execution.Approver.UserID, permission.targets); err != nil {
		return nil, err
	}
	for _, skill := range skills {
		if err := execution.Queries.AddAgentSkill(ctx, db.AddAgentSkillParams{AgentID: created.ID, SkillID: skill.ID}); err != nil {
			return nil, err
		}
	}
	return json.Marshal(map[string]string{"agent_id": uuidToString(created.ID), "name": created.Name})
}

// rejectedRuntimeOption mirrors CreateAgent's provider checks that need no
// handler state. The ACP catalog check stays with the interactive create.
func rejectedRuntimeOption(provider string, params decisionaction.AgentCopyParams) string {
	switch {
	case !agent.IsKnownThinkingValue(provider, params.ThinkingLevel):
		return thinkingLevelRejection(provider, params.ThinkingLevel)
	case agent.ThinkingLevelRejectedWithoutModel(provider) && params.ThinkingLevel != "" && params.Model == "":
		return thinkingNeedsExplicitModelRejection(provider)
	case !agent.IsKnownServiceTier(provider, params.ServiceTier):
		return fmt.Sprintf("service_tier %q is not a recognised value for runtime %q", params.ServiceTier, provider)
	}
	return ""
}

// copiedAvatar keeps the source's own image reference. A copy approval never
// publishes a new storage object, so any other URL is refused.
func copiedAvatar(source db.Agent, requested *string) (pgtype.Text, error) {
	if requested == nil || *requested == "" {
		return pgtype.Text{String: randomAgentEmojiAvatar(), Valid: true}, nil
	}
	if !source.AvatarUrl.Valid || source.AvatarUrl.String != *requested {
		return pgtype.Text{}, actionFailure{Code: "avatar_not_copied", Message: "a copy may only reuse the source agent's avatar"}
	}
	return source.AvatarUrl, nil
}

// copyPreconditionDrifted reports whether the source's instructions changed
// since the ask: the approver saw a hash of exactly the text being copied.
func copyPreconditionDrifted(action db.DecisionAction, source db.Agent) (bool, error) {
	var wanted struct {
		InstructionsSHA256 string `json:"instructions_sha256"`
	}
	if err := json.Unmarshal(action.Preconditions, &wanted); err != nil {
		return false, err
	}
	digest := sha256.Sum256([]byte(source.Instructions))
	return wanted.InstructionsSHA256 != hex.EncodeToString(digest[:]), nil
}

// copySettingsDrifted reports whether the source's permission mode, invocation
// targets or skills differ from the digest the request snapshotted.
func copySettingsDrifted(action db.DecisionAction, digest string) (bool, error) {
	var wanted struct {
		SettingsSHA256 string `json:"settings_sha256"`
	}
	if err := json.Unmarshal(action.Preconditions, &wanted); err != nil {
		return false, err
	}
	return wanted.SettingsSHA256 != digest, nil
}

// publishAgentCopyCreated announces the agent a copy action created. It is
// idempotent: a repeat only re-sends the event.
func (handler *Handler) publishAgentCopyCreated(ctx context.Context, action db.DecisionAction) error {
	var result struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.Unmarshal(action.Result, &result); err != nil {
		return err
	}
	id, err := parseUUIDString(result.AgentID)
	if err != nil {
		return err
	}
	created, err := handler.Queries.GetAgent(ctx, id)
	if err != nil {
		return err
	}
	if runtime, err := handler.Queries.GetAgentRuntime(ctx, created.RuntimeID); err == nil && runtime.Status == "online" {
		handler.TaskService.ReconcileAgentStatus(ctx, created.ID)
		created, _ = handler.Queries.GetAgent(ctx, created.ID)
	}
	resp := handler.agentToResponse(created)
	if err := handler.attachAgentSkills(ctx, &resp, created.ID); err != nil {
		return err
	}
	if err := handler.enrichAgentResponseWithTargets(ctx, &resp, created.ID); err != nil {
		return err
	}
	handler.publish(protocol.EventAgentCreated, uuidToString(action.WorkspaceID), "member", uuidToString(action.ApprovedByUserID), map[string]any{"agent": broadcastAgentResponse(resp)})
	return nil
}
