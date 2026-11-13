package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// workspace.routing_agent.set (CHE-4486) replaces the workspace's routing
// agent setting through the config-change core. The value goes through the
// dedicated route's validator and writes its routing_agent_changed activity;
// the owner's passkey approval stands in for the route's human-only gate.

const (
	routingAgentSetAction = "workspace.routing_agent.set"
	routingAgentField     = "workspace.routing_agent"
)

type routingAgentSetParams struct {
	WorkspaceID  string              `json:"workspace_id"`
	RoutingAgent *RoutingAgentConfig `json:"routing_agent"`
	BaseSHA256   string              `json:"base_sha256"`
	AfterSHA256  string              `json:"after_sha256"`
}

func init() {
	decisionaction.Register(&decisionaction.Definition{
		Type: routingAgentSetAction, Version: 1, MinClass: "spend", Target: decisionaction.TargetWorkspace, RequiresPreview: true,
		Template:  decisionaction.Template{Title: "Change the workspace routing agent", Summary: "Changes the workspace routing-agent setting to the proposed value."},
		Normalize: normalizeRoutingAgentSet,
	})
	registerConfigChange(routingAgentSetAction, configChange{
		Fields: []string{routingAgentField},
		Lock: func(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, params json.RawMessage) error {
			_, err := loadWorkspaceTarget(ctx, queries, workspaceID, params, http.StatusConflict, true)
			return err
		},
		Authorize: authorizeRoutingAgentSet,
		Read:      readRoutingAgentSet,
		Apply:     applyRoutingAgentSet,
		Publish: func(ctx context.Context, handler *Handler, action db.DecisionAction) error {
			workspace, err := handler.Queries.GetWorkspace(ctx, action.WorkspaceID)
			if err != nil {
				return err
			}
			handler.publish(protocol.EventWorkspaceUpdated, uuidToString(workspace.ID), "agent", uuidToString(action.RequestedByAgentID), map[string]any{"workspace": handler.workspaceToResponse(workspace)})
			return nil
		},
	})
}

func normalizeRoutingAgentSet(raw json.RawMessage, slots []decisionaction.SecretSlot) (json.RawMessage, []decisionaction.SecretSlot, error) {
	if len(slots) > 0 {
		return nil, nil, &decisionaction.Error{Code: decisionaction.CodeInvalidParams, Message: routingAgentSetAction + " takes no secret slots"}
	}
	var params routingAgentSetParams
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&params); err != nil {
		return nil, nil, &decisionaction.Error{Code: decisionaction.CodeInvalidParams, Message: fmt.Sprintf("params: %v", err)}
	}
	if params.RoutingAgent == nil {
		return nil, nil, &decisionaction.Error{Code: decisionaction.CodeInvalidParams, Message: "routing_agent must be an object"}
	}
	out, err := json.Marshal(params)
	return out, nil, err
}

// routingAgentSetValue validates the requested value with the dedicated
// route's rules, plus: no archived agent may be named.
func routingAgentSetValue(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, raw json.RawMessage) (RoutingAgentConfig, error) {
	var params routingAgentSetParams
	if err := json.Unmarshal(raw, &params); err != nil || params.RoutingAgent == nil {
		return RoutingAgentConfig{}, settingsInvalid("params must hold a routing_agent object")
	}
	next := *params.RoutingAgent
	if msg := validateRoutingAgentQ(ctx, queries, workspaceID, &next); msg != "" {
		return RoutingAgentConfig{}, settingsInvalid("%s", msg)
	}
	named := map[string]string{"agent_id": next.AgentID, "fallback_agent_id": next.FallbackAgentID}
	for key, id := range next.Overrides {
		named["overrides."+key] = id
	}
	for slot, id := range named {
		if id == "" {
			continue
		}
		agent, err := queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: util.MustParseUUID(id), WorkspaceID: workspaceID})
		if err != nil {
			return RoutingAgentConfig{}, err
		}
		if agent.ArchivedAt.Valid {
			return RoutingAgentConfig{}, settingsInvalid("%s is an archived agent: %s", slot, id)
		}
	}
	return next, nil
}

func readRoutingAgentSet(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, raw json.RawMessage) (configChangeRead, error) {
	workspace, err := loadWorkspaceTarget(ctx, queries, workspaceID, raw, http.StatusBadRequest, false)
	if err != nil {
		return configChangeRead{}, err
	}
	stored, err := decodeStoredSettings(workspace.Settings)
	if err != nil {
		return configChangeRead{}, err
	}
	next, err := routingAgentSetValue(ctx, queries, workspaceID, raw)
	if err != nil {
		return configChangeRead{}, err
	}
	nextJSON, err := json.Marshal(next)
	if err != nil {
		return configChangeRead{}, err
	}
	read := configChangeRead{Field: routingAgentField, TargetID: uuidToString(workspaceID), HumanBlocksIntact: true, After: next}
	if live := stored[routingAgentKey]; len(live) > 0 {
		read.Before = live
	}
	if read.LiveSHA256, err = decisionaction.SettingsDigest(read.TargetID, map[string]json.RawMessage{routingAgentKey: stored[routingAgentKey]}); err != nil {
		return read, err
	}
	read.AfterSHA256, err = decisionaction.SettingsDigest(read.TargetID, map[string]json.RawMessage{routingAgentKey: nextJSON})
	return read, err
}

// authorizeRoutingAgentSet: the proposer must be a live agent of the workspace
// and may not newly name itself in any slot (agent, fallback, override target).
func authorizeRoutingAgentSet(ctx context.Context, queries *db.Queries, workspaceID, proposer pgtype.UUID, _ json.RawMessage, read configChangeRead) error {
	agent, err := queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: proposer, WorkspaceID: workspaceID})
	if err != nil || agent.ArchivedAt.Valid {
		return decisionIntentRefusal{http.StatusConflict, "proposer_revoked: the proposing agent is no longer a live agent of this workspace"}
	}
	after, ok := read.After.(RoutingAgentConfig)
	if !ok {
		return decisionIntentRefusal{http.StatusInternalServerError, "invalid_read: routing agent value missing"}
	}
	live, err := loadRoutingAgent(ctx, queries, workspaceID)
	if err != nil {
		return err
	}
	self := uuidToString(proposer)
	newlyNames := func(slot, liveID, afterID string) error {
		if strings.EqualFold(afterID, self) && !strings.EqualFold(liveID, self) {
			return decisionIntentRefusal{http.StatusForbidden, "self_route_refused: the proposing agent may not newly name itself as " + slot}
		}
		return nil
	}
	if err := newlyNames("agent_id", live.AgentID, after.AgentID); err != nil {
		return err
	}
	if err := newlyNames("fallback_agent_id", live.FallbackAgentID, after.FallbackAgentID); err != nil {
		return err
	}
	for key, id := range after.Overrides {
		if err := newlyNames("an override target ("+key+")", live.Overrides[key], id); err != nil {
			return err
		}
	}
	return nil
}

// applyRoutingAgentSet is saveRoutingAgent's write and audit in the approval
// transaction. The workspace row is locked; stored keys keep their exact bytes.
func applyRoutingAgentSet(ctx context.Context, execution decisionActionExecution, read configChangeRead) error {
	next, ok := read.After.(RoutingAgentConfig)
	if !ok {
		return fmt.Errorf("routing agent value missing from the read")
	}
	workspace, err := loadWorkspaceTarget(ctx, execution.Queries, execution.Action.WorkspaceID, execution.Action.Params, http.StatusConflict, false)
	if err != nil {
		return err
	}
	stored, err := decodeStoredSettings(workspace.Settings)
	if err != nil {
		return err
	}
	prev := routingAgentFromSettings(map[string]any{routingAgentKey: stored[routingAgentKey]})
	if stored[routingAgentKey], err = json.Marshal(next); err != nil {
		return err
	}
	settings, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	if _, err := execution.Queries.UpdateWorkspace(ctx, db.UpdateWorkspaceParams{ID: workspace.ID, Settings: settings}); err != nil {
		return err
	}
	details, err := json.Marshal(map[string]any{"from": prev, "to": next})
	if err != nil {
		return err
	}
	_, err = execution.Queries.CreateActivity(ctx, db.CreateActivityParams{
		ID: dbid.NewV7(), WorkspaceID: workspace.ID, ActorType: strToText("member"), ActorID: execution.Approver.UserID,
		Action: routingAgentActivity, Details: details,
	})
	return err
}
