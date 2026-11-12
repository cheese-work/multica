package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Routing agent (CHE-3627): a workspace setting naming the agent that handles
// routing-only workflow steps, a fallback for when it is unusable, and
// per-status overrides. It lives in workspace.settings under one key; only the
// dedicated route changes it, and every change is audited. The stored shape and
// its readers live in the service package.
const (
	routingAgentKey      = service.RoutingAgentSettingsKey
	routingAgentActivity = "routing_agent_changed"
)

var defaultRoutingStatuses = service.DefaultRoutingStatuses

type RoutingAgentConfig = service.RoutingAgentConfig

func routingAgentFromSettings(settings map[string]any) RoutingAgentConfig {
	return service.RoutingAgentFromSettings(settings)
}

func loadRoutingAgent(ctx context.Context, q *db.Queries, ws pgtype.UUID) (RoutingAgentConfig, error) {
	row, err := q.GetWorkspace(ctx, ws)
	if err != nil {
		return RoutingAgentConfig{}, err
	}
	var settings map[string]any
	_ = json.Unmarshal(row.Settings, &settings)
	return routingAgentFromSettings(settings), nil
}

// routingAgentFor returns the agent that should handle a step entering status,
// or an invalid id when the step keeps its own handler. The configured agent
// is skipped when archived, then the fallback is tried. A read error routes
// nothing: the step's own handler is always a safe owner.
func routingAgentFor(ctx context.Context, q *db.Queries, ws pgtype.UUID, project db.Project, status string) pgtype.UUID {
	cfg, err := loadRoutingAgent(ctx, q, ws)
	if err != nil || cfg.AgentID == "" {
		return pgtype.UUID{}
	}
	primary := cfg.AgentFor(service.WorkflowScope(ctx, q, project), status)
	if primary == "" {
		return pgtype.UUID{}
	}
	for _, candidate := range []string{primary, cfg.FallbackAgentID} {
		id, err := util.ParseUUID(candidate)
		if err != nil {
			continue
		}
		agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: id, WorkspaceID: ws})
		if err == nil && !agent.ArchivedAt.Valid {
			return id
		}
	}
	return pgtype.UUID{}
}

// reconcileRoutingAgent makes a generic settings save keep the stored value.
func reconcileRoutingAgent(stored, incoming map[string]any) {
	delete(incoming, routingAgentKey)
	if v, ok := stored[routingAgentKey]; ok {
		incoming[routingAgentKey] = v
	}
}

func (h *Handler) GetRoutingAgent(w http.ResponseWriter, r *http.Request) {
	member, ok := h.requireWorkspaceMember(w, r, workspaceIDFromURL(r, "id"), "workspace not found")
	if !ok {
		return
	}
	cfg, err := loadRoutingAgent(r.Context(), h.Queries, member.WorkspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load routing agent")
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// UpdateRoutingAgent replaces the setting. Human owner/admin only: a task
// token or cloud PAT gets 403 even when its owner holds the role.
func (h *Handler) UpdateRoutingAgent(w http.ResponseWriter, r *http.Request) {
	id := workspaceIDFromURL(r, "id")
	actorType, _ := h.resolveActor(r, requestUserID(r), id)
	if isGovernedFieldMachineActor(r, actorType) {
		writeError(w, http.StatusForbidden, "this endpoint is only available to human actors")
		return
	}
	member, ok := h.requireWorkspaceRole(w, r, id, "workspace not found", "owner", "admin")
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var next RoutingAgentConfig
	if err := dec.Decode(&next); err != nil {
		writeError(w, http.StatusBadRequest, "invalid routing agent body: "+err.Error())
		return
	}
	if msg := h.validateRoutingAgent(r.Context(), member.WorkspaceID, &next); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if err := h.saveRoutingAgent(r.Context(), member, next); err != nil {
		slog.Warn("update routing agent failed", append(logger.RequestAttrs(r), "error", err, "workspace_id", id)...)
		writeError(w, http.StatusInternalServerError, "failed to update routing agent")
		return
	}
	slog.Info("routing agent updated", append(logger.RequestAttrs(r), "workspace_id", id, "actor_id", requestUserID(r))...)
	if ws, err := h.Queries.GetWorkspace(r.Context(), member.WorkspaceID); err == nil {
		h.publish(protocol.EventWorkspaceUpdated, id, "member", requestUserID(r), map[string]any{"workspace": h.workspaceToResponse(ws)})
	}
	writeJSON(w, http.StatusOK, next)
}

// validateRoutingAgent canonicalizes next in place and returns the first
// problem, or "".
func (h *Handler) validateRoutingAgent(ctx context.Context, ws pgtype.UUID, next *RoutingAgentConfig) string {
	return validateRoutingAgentQ(ctx, h.Queries, ws, next)
}

// validateRoutingAgentQ is validateRoutingAgent over any query handle, so the
// approval transaction can run the same rules.
func validateRoutingAgentQ(ctx context.Context, q *db.Queries, ws pgtype.UUID, next *RoutingAgentConfig) string {
	canon := func(field, raw string) (string, string) {
		id, err := util.ParseUUID(raw)
		if err != nil {
			return "", field + " must be an agent id"
		}
		if _, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: id, WorkspaceID: ws}); err != nil {
			return "", field + " not in this workspace: " + raw
		}
		return uuidToString(id), ""
	}
	var msg string
	if next.AgentID != "" {
		if next.AgentID, msg = canon("agent_id", next.AgentID); msg != "" {
			return msg
		}
	}
	if next.FallbackAgentID != "" {
		if next.AgentID == "" {
			return "fallback_agent_id needs agent_id"
		}
		if next.FallbackAgentID, msg = canon("fallback_agent_id", next.FallbackAgentID); msg != "" {
			return msg
		}
		if next.FallbackAgentID == next.AgentID {
			return "fallback_agent_id must differ from agent_id"
		}
	}
	statuses := make([]string, 0, len(next.Statuses))
	for _, s := range next.Statuses {
		s = strings.TrimSpace(s)
		if s == "" {
			return "statuses must be status keys"
		}
		if !slices.Contains(statuses, s) {
			statuses = append(statuses, s)
		}
	}
	next.Statuses = statuses
	if len(next.Statuses) == 0 {
		next.Statuses = slices.Clone(defaultRoutingStatuses)
	}
	overrides := make(map[string]string, len(next.Overrides))
	for key, raw := range next.Overrides {
		if msg := checkOverrideKey(ctx, q, ws, key); msg != "" {
			return msg
		}
		if overrides[key], msg = canon("overrides."+key, raw); msg != "" {
			return msg
		}
	}
	next.Overrides = overrides
	return ""
}

// checkOverrideKey accepts "<status>" or "<workflow id>:<status>" for a
// workflow of this workspace.
func checkOverrideKey(ctx context.Context, q *db.Queries, ws pgtype.UUID, key string) string {
	const bad = "overrides must be keyed by <status> or <workflow id>:<status>"
	scope, status, scoped := strings.Cut(key, ":")
	if !scoped {
		status = key
	}
	if strings.TrimSpace(status) == "" || status != strings.TrimSpace(status) {
		return bad
	}
	if !scoped {
		return ""
	}
	id, err := util.ParseUUID(scope)
	if err != nil {
		return bad
	}
	if _, err := q.GetIssueWorkflow(ctx, db.GetIssueWorkflowParams{ID: id, WorkspaceID: ws}); err != nil {
		return "overrides." + key + ": workflow not in this workspace"
	}
	return ""
}

// saveRoutingAgent writes the key and its audit record in one transaction
// under the workspace row lock, so a change without a record does not happen.
func (h *Handler) saveRoutingAgent(ctx context.Context, member db.Member, next RoutingAgentConfig) error {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := h.Queries.WithTx(tx)
	storedJSON, err := q.LockWorkspaceSettings(ctx, member.WorkspaceID)
	if err != nil {
		return err
	}
	var stored map[string]any
	_ = json.Unmarshal(storedJSON, &stored) // stored settings that are not an object start over as one
	if stored == nil {
		stored = map[string]any{}
	}
	prev := routingAgentFromSettings(stored)
	stored[routingAgentKey] = next
	settings, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	if _, err := q.UpdateWorkspace(ctx, db.UpdateWorkspaceParams{ID: member.WorkspaceID, Settings: settings}); err != nil {
		return err
	}
	details, err := json.Marshal(map[string]any{"from": prev, "to": next})
	if err != nil {
		return err
	}
	if _, err := q.CreateActivity(ctx, db.CreateActivityParams{
		ID: dbid.NewV7(), WorkspaceID: member.WorkspaceID, ActorType: strToText("member"), ActorID: member.UserID,
		Action: routingAgentActivity, Details: details,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
