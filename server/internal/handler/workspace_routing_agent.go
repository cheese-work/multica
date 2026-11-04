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
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Routing agent (CHE-3627): a workspace setting naming the agent that handles
// routing-only workflow steps, a fallback for when it is unusable, and
// per-status overrides. It lives in workspace.settings under one key; only the
// dedicated route changes it, and every change is audited.
const (
	routingAgentKey      = "routing_agent"
	routingAgentActivity = "routing_agent_changed"
)

// defaultRoutingStatuses are the steps that only route work to the next owner.
var defaultRoutingStatuses = []string{"todo", "blocked", "merged", "need_decision"}

// RoutingAgentConfig is the stored setting. An empty AgentID means routing is
// off: every step keeps its own handler.
type RoutingAgentConfig struct {
	AgentID         string `json:"agent_id"`
	FallbackAgentID string `json:"fallback_agent_id"`
	// Statuses lists the routing steps; empty means defaultRoutingStatuses.
	Statuses []string `json:"statuses"`
	// Overrides sends one routing status to another agent than AgentID. Its
	// keys are routing steps too.
	Overrides map[string]string `json:"overrides"`
}

func routingAgentFromSettings(settings map[string]any) RoutingAgentConfig {
	var cfg RoutingAgentConfig
	if raw, ok := settings[routingAgentKey]; ok {
		if b, err := json.Marshal(raw); err == nil {
			_ = json.Unmarshal(b, &cfg) // a malformed value routes nothing
		}
	}
	if cfg.Statuses == nil {
		cfg.Statuses = slices.Clone(defaultRoutingStatuses)
	}
	if cfg.Overrides == nil {
		cfg.Overrides = map[string]string{}
	}
	return cfg
}

func (h *Handler) loadRoutingAgent(ctx context.Context, ws pgtype.UUID) (RoutingAgentConfig, error) {
	row, err := h.Queries.GetWorkspace(ctx, ws)
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
func (h *Handler) routingAgentFor(ctx context.Context, ws pgtype.UUID, status string) pgtype.UUID {
	cfg, err := h.loadRoutingAgent(ctx, ws)
	if err != nil || cfg.AgentID == "" {
		return pgtype.UUID{}
	}
	override, overridden := cfg.Overrides[status]
	if !overridden && !slices.Contains(cfg.Statuses, status) {
		return pgtype.UUID{}
	}
	primary := cfg.AgentID
	if overridden {
		primary = override
	}
	for _, candidate := range []string{primary, cfg.FallbackAgentID} {
		id, err := util.ParseUUID(candidate)
		if err != nil {
			continue
		}
		agent, err := h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: id, WorkspaceID: ws})
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
	cfg, err := h.loadRoutingAgent(r.Context(), member.WorkspaceID)
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
	canon := func(field, raw string) (string, string) {
		id, err := util.ParseUUID(raw)
		if err != nil {
			return "", field + " must be an agent id"
		}
		if _, err := h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: id, WorkspaceID: ws}); err != nil {
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
	if next.Statuses == nil || len(next.Statuses) == 0 {
		next.Statuses = slices.Clone(defaultRoutingStatuses)
	}
	overrides := make(map[string]string, len(next.Overrides))
	for status, raw := range next.Overrides {
		if strings.TrimSpace(status) == "" {
			return "overrides must be keyed by status key"
		}
		if overrides[status], msg = canon("overrides."+status, raw); msg != "" {
			return msg
		}
	}
	next.Overrides = overrides
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
