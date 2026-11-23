package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/agentrolescope"
	"github.com/multica-ai/multica/server/internal/capabilityrouting"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// agent_role_scope (CHE-9192) is a governed setting: it changes only through the
// workspace.settings.patch card, so a generic settings save keeps the stored
// value, as for capability_routing.
const (
	agentRoleScopeKey      = agentrolescope.SettingsKey
	agentRoleScopeOverride = "agent_role_scope_override"
)

func reconcileAgentRoleScope(stored, incoming map[string]any) {
	delete(incoming, agentRoleScopeKey)
	if v, ok := stored[agentRoleScopeKey]; ok {
		incoming[agentRoleScopeKey] = v
	}
}

func (h *Handler) loadAgentRoleScope(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID) (agentrolescope.Config, map[string]any, error) {
	row, err := q.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return agentrolescope.Config{}, nil, err
	}
	var settings map[string]any
	_ = json.Unmarshal(row.Settings, &settings)
	return agentrolescope.FromSettings(settings), settings, nil
}

// refuseAgentOutOfScope returns a non-zero status when an agent actor assigns an
// issue to an agent whose agent_role_scope excludes the issue's work class. The
// refusal names the capability_routing target for the issue's type. A person is
// never refused; an out-of-scope assignment by a person leaves an audit note on
// the issue. issueID is invalid on create: no note can be written yet. A read
// error fails open, because the setting only narrows who an agent may pick.
func (h *Handler) refuseAgentOutOfScope(ctx context.Context, actorType, actorID, workspaceID string, issueID pgtype.UUID, current db.Issue, title string, assigneeType pgtype.Text, assigneeID pgtype.UUID) (int, string) {
	if !assigneeType.Valid || assigneeType.String != "agent" || !assigneeID.Valid {
		return 0, ""
	}
	if current.AssigneeType.String == "agent" && current.AssigneeID == assigneeID {
		return 0, ""
	}
	wsUUID, err := util.ParseUUID(workspaceID)
	if err != nil {
		return 0, ""
	}
	cfg, settings, err := h.loadAgentRoleScope(ctx, h.Queries, wsUUID)
	if err != nil {
		return 0, ""
	}
	target := util.UUIDToString(assigneeID)
	class := cfg.ClassOf(title)
	scope, scoped := cfg.Scope(target)
	if !scoped || cfg.Allows(target, class) {
		return 0, ""
	}
	if actorType != "agent" {
		h.noteAgentRoleScopeOverride(ctx, wsUUID, issueID, actorType, actorID, target, class)
		return 0, ""
	}
	return http.StatusForbidden, h.agentOutOfScopeMessage(ctx, wsUUID, settings, title, target, class, scope)
}

func (h *Handler) noteAgentRoleScopeOverride(ctx context.Context, ws, issueID pgtype.UUID, actorType, actorID, target, class string) {
	slog.Info("agent_role_scope: person assigned out-of-scope work", "workspace_id", uuidToString(ws), "issue_id", uuidToString(issueID), "actor_id", actorID, "assignee_id", target, "class", class)
	if !issueID.Valid {
		return
	}
	details, err := json.Marshal(map[string]any{"assignee_id": target, "class": class})
	if err != nil {
		return
	}
	actor, _ := util.ParseUUID(actorID)
	if _, err := h.Queries.CreateActivity(ctx, db.CreateActivityParams{
		ID: dbid.NewV7(), WorkspaceID: ws, IssueID: issueID, ActorType: strToText(actorType), ActorID: actor,
		Action: agentRoleScopeOverride, Details: details,
	}); err != nil {
		slog.Warn("agent_role_scope: audit note failed", "issue_id", uuidToString(issueID), "error", err)
	}
}

// agentOutOfScopeMessage names the agent, its scope and the capability_routing
// target for the issue's type: the author for execution work, the reviewers for a
// code review.
func (h *Handler) agentOutOfScopeMessage(ctx context.Context, ws pgtype.UUID, settings map[string]any, title, targetID, class string, scope []string) string {
	name := func(id string) string {
		agentID, err := util.ParseUUID(id)
		if err == nil {
			if agent, err := h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agentID, WorkspaceID: ws}); err == nil {
				return agent.Name
			}
		}
		return id
	}
	msg := fmt.Sprintf("agent_role_scope: %s takes %s work only; this issue is %s work.", name(targetID), strings.Join(scope, ", "), class)
	typ, ok := issueworkflow.TitleType(title)
	entry, routed := capabilityrouting.FromSettings(settings)[typ]
	if !ok || !routed {
		return msg + " Assign the author or reviewer your workspace routes for this type (capability_routing)."
	}
	if class == agentrolescope.CodeReview {
		names := make([]string, 0, len(entry.Reviewers))
		for _, id := range entry.Reviewers {
			names = append(names, name(id))
		}
		return msg + fmt.Sprintf(" Request review from %s (capability_routing[%s].reviewers).", strings.Join(names, " or "), typ)
	}
	return msg + fmt.Sprintf(" Assign %s (capability_routing[%s].author).", name(entry.Author), typ)
}

// checkAgentRoleScopeAgents refuses a patch whose agent_role_scope names an id
// that is not an agent of the workspace.
func checkAgentRoleScopeAgents(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, patch map[string]json.RawMessage) error {
	raw := patch[agentRoleScopeKey]
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	cfg, err := agentrolescope.Parse(raw)
	if err != nil {
		return settingsInvalid("%v", err)
	}
	for _, id := range cfg.AgentIDs() {
		agentID, err := util.ParseUUID(id)
		if err == nil {
			_, err = q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agentID, WorkspaceID: workspaceID})
		}
		if err != nil {
			return settingsInvalid("%s", fmt.Sprintf("%s names %s, which is not an agent of this workspace", agentRoleScopeKey, id))
		}
	}
	return nil
}

const agentRoleScopeCode = "agent_role_scope"

// AgentOutOfScopeError refuses a review request that an agent aims at an agent
// whose agent_role_scope excludes code review.
type AgentOutOfScopeError struct{ Message string }

func (e *AgentOutOfScopeError) Error() string { return e.Message }

// checkReviewerScope holds an explicit reviewer choice by an agent to the
// reviewer's agent_role_scope. A person is never refused; a reviewer the
// capability_routing table picked is the setting owner's own choice and is not
// judged. A squad assignee is not judged.
func (h *Handler) checkReviewerScope(ctx context.Context, q *db.Queries, prev db.Issue, title string, assignee issueworkflow.StepAssignee, callerIsAgent bool) error {
	if !callerIsAgent || assignee.Type != issueworkflow.HandlerAgent {
		return nil
	}
	cfg, settings, err := h.loadAgentRoleScope(ctx, q, prev.WorkspaceID)
	if err != nil {
		return nil
	}
	target := util.UUIDToString(assignee.ID)
	class := cfg.ReviewClassOf(title)
	scope, scoped := cfg.Scope(target)
	if !scoped || cfg.Allows(target, class) {
		return nil
	}
	return &AgentOutOfScopeError{Message: h.agentOutOfScopeMessage(ctx, prev.WorkspaceID, settings, title, target, class, scope)}
}
