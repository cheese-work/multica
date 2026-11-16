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

// Delegated governed writes (CHE-2454): a workspace setting naming the agents
// and governed fields for which a human decision can stand in for an owner
// write. It lives in workspace.settings under one key. Only the dedicated
// route below changes it; a generic settings save keeps the stored value.
const (
	delegatedWritesKey      = "delegated_governed_writes"
	delegatedWritesActivity = "delegated_writes_changed"
)

// delegatedWriteFields are the governed fields a workspace may delegate.
var delegatedWriteFields = []string{
	"workflow.steps", "workflow.instructions", "workspace.context",
	"squad.instructions", "agent.instructions", "project.description",
	"workflow.budget", "issue.workflow",
}

// DelegatedWritesConfig defaults to no agents, no fields and a required human
// decision, so an unset workspace delegates nothing.
type DelegatedWritesConfig struct {
	AllowedAgents         []string `json:"allowed_agents"`
	AllowedFields         []string `json:"allowed_fields"`
	RequiresHumanDecision bool     `json:"requires_human_decision"`
}

func defaultDelegatedWrites() DelegatedWritesConfig {
	return DelegatedWritesConfig{AllowedAgents: []string{}, AllowedFields: []string{}, RequiresHumanDecision: true}
}

// delegatedWritesFromSettings reads the stored value. A missing key is the
// default; a malformed one is an error so callers fail closed.
func delegatedWritesFromSettings(settings map[string]any) (DelegatedWritesConfig, error) {
	cfg := defaultDelegatedWrites()
	raw, ok := settings[delegatedWritesKey]
	if !ok {
		return cfg, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return defaultDelegatedWrites(), err
	}
	return cfg, nil
}

func (h *Handler) loadDelegatedWrites(ctx context.Context, ws pgtype.UUID) (DelegatedWritesConfig, error) {
	return loadDelegatedWritesQ(ctx, h.Queries, ws)
}

// loadDelegatedWritesQ reads the setting through queries, so an approval
// transaction can read it under the workspace lock.
func loadDelegatedWritesQ(ctx context.Context, queries *db.Queries, ws pgtype.UUID) (DelegatedWritesConfig, error) {
	row, err := queries.GetWorkspace(ctx, ws)
	if err != nil {
		return defaultDelegatedWrites(), err
	}
	var settings map[string]any
	_ = json.Unmarshal(row.Settings, &settings) // settings that are not an object delegate nothing
	return delegatedWritesFromSettings(settings)
}

// delegatedWriteAllowed reports whether agentID may make a delegated write to
// field, and whether that write needs a human decision. It fails closed: a
// read error or malformed setting denies and requires a decision.
func (h *Handler) delegatedWriteAllowed(ctx context.Context, ws pgtype.UUID, agentID, field string) (requiresDecision, ok bool) {
	return delegatedWriteAllowedQ(ctx, h.Queries, ws, agentID, field)
}

// delegatedWriteAllowedQ is delegatedWriteAllowed over any *db.Queries.
func delegatedWriteAllowedQ(ctx context.Context, queries *db.Queries, ws pgtype.UUID, agentID, field string) (requiresDecision, ok bool) {
	cfg, err := loadDelegatedWritesQ(ctx, queries, ws)
	if err != nil {
		return true, false
	}
	allowed := slices.ContainsFunc(cfg.AllowedAgents, func(a string) bool { return strings.EqualFold(a, agentID) }) &&
		slices.Contains(cfg.AllowedFields, field)
	return cfg.RequiresHumanDecision, allowed
}

// reconcileDelegatedWrites makes a generic settings save keep the stored
// setting: whatever the body says about the key is dropped.
func reconcileDelegatedWrites(stored, incoming map[string]any) {
	delete(incoming, delegatedWritesKey)
	if v, ok := stored[delegatedWritesKey]; ok {
		incoming[delegatedWritesKey] = v
	}
}

func (h *Handler) GetDelegatedWrites(w http.ResponseWriter, r *http.Request) {
	id := workspaceIDFromURL(r, "id")
	member, ok := h.requireWorkspaceMember(w, r, id, "workspace not found")
	if !ok {
		return
	}
	cfg, err := h.loadDelegatedWrites(r.Context(), member.WorkspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load delegated writes")
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

type updateDelegatedWritesRequest struct {
	AllowedAgents         *[]string `json:"allowed_agents"`
	AllowedFields         *[]string `json:"allowed_fields"`
	RequiresHumanDecision *bool     `json:"requires_human_decision"`
}

// UpdateDelegatedWrites replaces the setting. Human owner/admin only: a task
// token or cloud PAT gets 403 even when its owner holds the role.
func (h *Handler) UpdateDelegatedWrites(w http.ResponseWriter, r *http.Request) {
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
	var req updateDelegatedWritesRequest
	if err := dec.Decode(&req); err != nil || req.AllowedAgents == nil || req.AllowedFields == nil {
		writeError(w, http.StatusBadRequest, "allowed_agents and allowed_fields are required lists")
		return
	}
	next, invalid := buildDelegatedWrites(r.Context(), h.Queries, member.WorkspaceID, req)
	if invalid != "" {
		writeError(w, http.StatusBadRequest, invalid)
		return
	}

	if err := h.saveDelegatedWrites(r.Context(), r, member, next); err != nil {
		if failedAudit(w, r, err, "workspace.delegated_writes.update") {
			return
		}
		slog.Warn("update delegated writes failed", append(logger.RequestAttrs(r), "error", err, "workspace_id", id)...)
		writeError(w, http.StatusInternalServerError, "failed to update delegated writes")
		return
	}
	slog.Info("delegated writes updated", append(logger.RequestAttrs(r), "workspace_id", id, "actor_id", requestUserID(r))...)
	if ws, err := h.Queries.GetWorkspace(r.Context(), member.WorkspaceID); err == nil {
		h.publish(protocol.EventWorkspaceUpdated, id, "member", requestUserID(r), map[string]any{"workspace": h.workspaceToResponse(ws)})
	}
	writeJSON(w, http.StatusOK, next)
}

// buildDelegatedWrites validates a request body into the setting it names: the
// fields must be delegable, the agents must be in the workspace, and both lists
// lose their duplicates. A non-empty message is the reason it is refused. The
// route and the workspace.delegated_writes.set action both call it.
func buildDelegatedWrites(ctx context.Context, queries *db.Queries, ws pgtype.UUID, req updateDelegatedWritesRequest) (DelegatedWritesConfig, string) {
	next := defaultDelegatedWrites()
	if req.AllowedAgents == nil || req.AllowedFields == nil {
		return next, "allowed_agents and allowed_fields are required lists"
	}
	if req.RequiresHumanDecision != nil {
		next.RequiresHumanDecision = *req.RequiresHumanDecision
	}
	for _, f := range *req.AllowedFields {
		if !slices.Contains(delegatedWriteFields, f) {
			return next, "unknown allowed field: " + f
		}
		if !slices.Contains(next.AllowedFields, f) {
			next.AllowedFields = append(next.AllowedFields, f)
		}
	}
	for _, a := range *req.AllowedAgents {
		agentUUID, err := util.ParseUUID(a)
		if err != nil {
			return next, "allowed_agents must hold agent ids"
		}
		if _, err := queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agentUUID, WorkspaceID: ws}); err != nil {
			return next, "allowed agent not in this workspace: " + a
		}
		if canon := uuidToString(agentUUID); !slices.Contains(next.AllowedAgents, canon) {
			next.AllowedAgents = append(next.AllowedAgents, canon)
		}
	}
	return next, ""
}

// saveDelegatedWrites writes the key and its audit record in one transaction
// under the workspace row lock, so a change without a record does not happen.
func (h *Handler) saveDelegatedWrites(ctx context.Context, r *http.Request, member db.Member, next DelegatedWritesConfig) error {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := h.Queries.WithTx(tx)
	prev, err := storeDelegatedWrites(ctx, q, member.WorkspaceID, member.UserID, next)
	if err != nil {
		return err
	}
	if err := h.writeAudit(ctx, q, r, auditInput{
		Kind: "workspace.delegated_writes.update", WorkspaceID: member.WorkspaceID, TargetID: member.WorkspaceID,
		Before: delegatedWritesAuditFields(prev), After: delegatedWritesAuditFields(next),
	}); err != nil {
		return auditWriteError{err}
	}
	return tx.Commit(ctx)
}

// storeDelegatedWrites replaces the key under the workspace row lock and
// records the change as a delegated_writes_changed activity by actor. It runs
// in the caller's transaction and returns the setting it replaced. The route
// and the workspace.delegated_writes.set action both write through it.
func storeDelegatedWrites(ctx context.Context, q *db.Queries, ws, actor pgtype.UUID, next DelegatedWritesConfig) (DelegatedWritesConfig, error) {
	storedJSON, err := q.LockWorkspaceSettings(ctx, ws)
	if err != nil {
		return DelegatedWritesConfig{}, err
	}
	var stored map[string]any
	_ = json.Unmarshal(storedJSON, &stored) // stored settings that are not an object start over as one
	if stored == nil {
		stored = map[string]any{}
	}
	prev, err := delegatedWritesFromSettings(stored)
	if err != nil {
		prev = defaultDelegatedWrites()
	}
	stored[delegatedWritesKey] = next
	settings, err := json.Marshal(stored)
	if err != nil {
		return prev, err
	}
	if _, err := q.UpdateWorkspace(ctx, db.UpdateWorkspaceParams{ID: ws, Settings: settings}); err != nil {
		return prev, err
	}
	details, err := json.Marshal(map[string]any{"from": prev, "to": next})
	if err != nil {
		return prev, err
	}
	_, err = q.CreateActivity(ctx, db.CreateActivityParams{
		ID: dbid.NewV7(), WorkspaceID: ws, ActorType: strToText("member"), ActorID: actor,
		Action: delegatedWritesActivity, Details: details,
	})
	return prev, err
}
