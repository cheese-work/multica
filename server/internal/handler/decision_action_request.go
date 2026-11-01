package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// decisionActionWire is the optional `action` of POST /api/decisions.
type decisionActionWire struct {
	Type             string                      `json:"type"`
	Version          int                         `json:"version"`
	Params           json.RawMessage             `json:"params"`
	SecretSlots      []decisionaction.SecretSlot `json:"secret_slots"`
	ExpiresInSeconds int64                       `json:"expires_in_seconds"`
}

// decisionActionDraft is a registry-validated action waiting for its request
// row. It is part of the retry fingerprint, so a different action is a
// different request.
type decisionActionDraft struct {
	Type        string                      `json:"type"`
	Version     int                         `json:"version"`
	Params      json.RawMessage             `json:"params"`
	SecretSlots []decisionaction.SecretSlot `json:"secret_slots"`
	TTL         time.Duration               `json:"ttl"`
	Definition  *decisionaction.Definition  `json:"-"`
}

// decisionActionResponse is the non-secret card view of a stored action.
type decisionActionResponse struct {
	ID            string          `json:"id"`
	ActionType    string          `json:"action_type"`
	ActionVersion int32           `json:"action_version"`
	Params        json.RawMessage `json:"params"`
	Preconditions json.RawMessage `json:"preconditions"`
	SecretSlots   json.RawMessage `json:"secret_slots"`
	PayloadSHA256 string          `json:"payload_sha256"`
	ExpiresAt     string          `json:"expires_at"`
	State         string          `json:"state"`
}

func newDecisionActionResponse(action db.DecisionAction) *decisionActionResponse {
	return &decisionActionResponse{
		ID: uuidToString(action.ID), ActionType: action.ActionType, ActionVersion: action.ActionVersion,
		Params: action.Params, Preconditions: action.Preconditions, SecretSlots: action.SecretSlots,
		PayloadSHA256: action.PayloadSha256, ExpiresAt: timestampToString(action.ExpiresAt), State: action.State,
	}
}

// draftDecisionAction validates the wire action against the allowlist.
func draftDecisionAction(wire *decisionActionWire) (*decisionActionDraft, error) {
	normalized, err := decisionaction.Normalize(decisionaction.Request{Type: wire.Type, Version: wire.Version, Params: wire.Params, SecretSlots: wire.SecretSlots})
	if err != nil {
		return nil, actionRefusal(http.StatusBadRequest, err)
	}
	ttl, err := normalized.Definition.TTL(time.Duration(wire.ExpiresInSeconds) * time.Second)
	if err != nil {
		return nil, actionRefusal(http.StatusBadRequest, err)
	}
	return &decisionActionDraft{Type: wire.Type, Version: wire.Version, Params: normalized.Params, SecretSlots: normalized.SecretSlots, TTL: ttl, Definition: normalized.Definition}, nil
}

func actionRefusal(status int, err error) decisionIntentRefusal {
	var coded *decisionaction.Error
	if errors.As(err, &coded) {
		return decisionIntentRefusal{status, coded.Code + ": " + coded.Message}
	}
	return decisionIntentRefusal{status, err.Error()}
}

// actionRequestClass is max(agent class, registry minimum): an agent cannot
// lower it. "reversible" is the lowest class, so only it is raised.
func actionRequestClass(agentClass, minClass string) string {
	if agentClass == "reversible" {
		return minClass
	}
	return agentClass
}

// snapshotDecisionAction reads the non-secret state the human approves
// against and re-checks at approval.
func snapshotDecisionAction(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, draft *decisionActionDraft) (map[string]any, error) {
	targetOutside := decisionIntentRefusal{http.StatusForbidden, "target_workspace: the target agent is not in this workspace"}
	loadAgent := func(id string) (db.Agent, error) {
		agentID, err := parseUUIDStrict(id)
		if err != nil {
			return db.Agent{}, targetOutside
		}
		agent, err := queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agentID, WorkspaceID: workspaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Agent{}, targetOutside
		}
		return agent, err
	}
	switch draft.Type {
	case "agent.env.patch":
		var params struct {
			AgentID string `json:"agent_id"`
		}
		if err := json.Unmarshal(draft.Params, &params); err != nil {
			return nil, err
		}
		agent, err := loadAgent(params.AgentID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"agent_id": params.AgentID, "custom_env_version": agent.CustomEnvVersion}, nil
	case "agent.copy":
		var params struct {
			SourceAgentID string  `json:"source_agent_id"`
			Instructions  *string `json:"instructions"`
		}
		if err := json.Unmarshal(draft.Params, &params); err != nil {
			return nil, err
		}
		source, err := loadAgent(params.SourceAgentID)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256([]byte(source.Instructions))
		settings, err := agentCopySettingsDigest(ctx, queries, source)
		if err != nil {
			return nil, err
		}
		return map[string]any{"source_agent_id": params.SourceAgentID, "instructions_sha256": hex.EncodeToString(digest[:]), "settings_sha256": settings}, nil
	}
	return nil, errors.New("no snapshot for action " + draft.Type)
}

// checkIssueTarget requires params.issue_id of an issue-target action to be
// the issue its request is on, so an approval cannot reach another issue.
func checkIssueTarget(params json.RawMessage, requestIssueID pgtype.UUID, status int) error {
	var target struct {
		IssueID string `json:"issue_id"`
	}
	if json.Unmarshal(params, &target) != nil {
		return decisionIntentRefusal{status, "target_issue_mismatch: params.issue_id must be the issue the request is on"}
	}
	id, err := parseUUIDString(target.IssueID)
	if err != nil || id != requestIssueID {
		return decisionIntentRefusal{status, "target_issue_mismatch: params.issue_id must be the issue the request is on"}
	}
	return nil
}

// isIssueTargetAction reports whether the stored action targets its request's issue.
func isIssueTargetAction(action db.DecisionAction) bool {
	definition, found := decisionaction.Lookup(action.ActionType, int(action.ActionVersion))
	return found && definition.Target == decisionaction.TargetIssue
}

// actionPreconditions records what the approver must still find true: the
// issue itself for an issue target, otherwise the agent's snapshot.
func actionPreconditions(ctx context.Context, queries *db.Queries, issue db.Issue, draft *decisionActionDraft) (map[string]any, error) {
	if draft.Definition.Target != decisionaction.TargetIssue {
		return snapshotDecisionAction(ctx, queries, issue.WorkspaceID, draft)
	}
	if err := checkIssueTarget(draft.Params, issue.ID, http.StatusBadRequest); err != nil {
		return nil, err
	}
	return map[string]any{"issue_id": uuidToString(issue.ID)}, nil
}

// createDecisionAction stores the action in the request's transaction, so a
// request never exists without the action it asks Cheese to approve.
func createDecisionAction(ctx context.Context, queries *db.Queries, issue db.Issue, request db.DecisionRequest, taskID pgtype.UUID, draft *decisionActionDraft) (db.DecisionAction, error) {
	empty := db.DecisionAction{}
	preconditions, err := actionPreconditions(ctx, queries, issue, draft)
	if err != nil {
		return empty, err
	}
	preconditionsJSON, err := json.Marshal(preconditions)
	if err != nil {
		return empty, err
	}
	slots := draft.SecretSlots
	if slots == nil {
		slots = []decisionaction.SecretSlot{}
	}
	slotsJSON, err := json.Marshal(slots)
	if err != nil {
		return empty, err
	}
	// Postgres stores microseconds; hash the instant it will read back.
	expiresAt := time.Now().Add(draft.TTL).UTC().Truncate(time.Microsecond)
	hash, err := decisionaction.Payload{
		ActionType: draft.Type, ActionVersion: draft.Version,
		WorkspaceID: uuidToString(issue.WorkspaceID), DecisionRequestID: uuidToString(request.ID),
		Params: draft.Params, Preconditions: preconditionsJSON, SecretSlots: slots, ExpiresAt: expiresAt,
	}.Hash()
	if err != nil {
		return empty, err
	}
	action, err := queries.CreateDecisionAction(ctx, db.CreateDecisionActionParams{
		WorkspaceID: issue.WorkspaceID, DecisionRequestID: request.ID,
		ActionType: draft.Type, ActionVersion: int32(draft.Version),
		Params: draft.Params, Preconditions: preconditionsJSON, SecretSlots: slotsJSON,
		PayloadSha256: hash, ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
		RequestedByAgentID: request.AskingAgentID, RequestedByTaskID: taskID,
	})
	if err != nil {
		return empty, err
	}
	details, err := json.Marshal(map[string]any{"decision_request_id": uuidToString(request.ID), "action_type": draft.Type, "action_version": draft.Version, "payload_sha256": hash})
	if err != nil {
		return empty, err
	}
	_, err = queries.CreateActivity(ctx, db.CreateActivityParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ActorType: pgtype.Text{String: "agent", Valid: true},
		ActorID: request.AskingAgentID, Action: "decision_action_requested", Details: details,
	})
	return action, err
}

// replayedDecisionAction returns the action a retried request already stored.
func (handler *Handler) replayedDecisionAction(ctx context.Context, issue db.Issue, request db.DecisionRequest) (*decisionActionResponse, error) {
	action, err := handler.Queries.GetDecisionActionByRequest(ctx, db.GetDecisionActionByRequestParams{DecisionRequestID: request.ID, WorkspaceID: issue.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return newDecisionActionResponse(action), nil
}

// agentCopySettingsDigest hashes every source setting an agent.copy carries
// over besides the instructions: permission mode, invocation targets and skill
// assignments. The request snapshots it and the copy re-checks it, so the
// signed hash covers what the copy executes.
func agentCopySettingsDigest(ctx context.Context, queries *db.Queries, source db.Agent) (string, error) {
	targets, err := queries.ListAgentInvocationTargets(ctx, source.ID)
	if err != nil {
		return "", err
	}
	skills, err := queries.ListAgentSkills(ctx, source.ID)
	if err != nil {
		return "", err
	}
	return digestAgentCopySettings(source, targets, skills)
}

func digestAgentCopySettings(source db.Agent, targets []db.AgentInvocationTarget, skills []db.Skill) (string, error) {
	settings := struct {
		PermissionMode string   `json:"permission_mode"`
		Targets        []string `json:"targets"`
		Skills         []string `json:"skills"`
	}{PermissionMode: source.PermissionMode, Targets: []string{}, Skills: []string{}}
	for _, target := range targets {
		settings.Targets = append(settings.Targets, target.TargetType+":"+uuidToString(target.TargetID))
	}
	for _, skill := range skills {
		settings.Skills = append(settings.Skills, uuidToString(skill.ID))
	}
	sort.Strings(settings.Targets)
	sort.Strings(settings.Skills)
	encoded, err := json.Marshal(settings)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
