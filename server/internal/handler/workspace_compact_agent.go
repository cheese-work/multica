package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// settings.compact_agent_id names the agent that runs chat compaction in place
// of the DM agent (CHE-2894 step 5). Choosing it hands the whole DM transcript
// to that agent, so only a human owner or admin changes it (SEC-3), and a
// change cancels the compaction tasks queued under the old value (SEC-1 rule 4).
const (
	compactAgentIDKey = "compact_agent_id"

	// compactAgentChangedEvent is the structured log event of a committed change.
	// No settings audit table exists; the event carries ids only, never text.
	compactAgentChangedEvent = "workspace.compact_agent_changed"

	compactAgentForbiddenCode  = "compact_agent_forbidden"
	compactAgentInvalidCode    = "invalid_compact_agent"
	compactAgentIneligibleCode = "compact_agent_ineligible"
)

// compactAgentError is a refusal of a compact_agent_id write. The copy never
// names the agent: an ineligible or private agent must not be confirmable.
type compactAgentError struct {
	status int
	code   string
	msg    string
}

func (e compactAgentError) Error() string { return e.msg }

var (
	errCompactAgentForbidden  = compactAgentError{http.StatusForbidden, compactAgentForbiddenCode, "only a workspace owner or admin can change the compaction agent"}
	errCompactAgentInvalid    = compactAgentError{http.StatusBadRequest, compactAgentInvalidCode, "compact_agent_id must be an agent id or null"}
	errCompactAgentIneligible = compactAgentError{http.StatusBadRequest, compactAgentIneligibleCode, "that agent cannot be the compaction agent"}
)

// compactAgentWrite is what one request says about compact_agent_id besides
// the nested settings blob.
type compactAgentWrite struct {
	// field is the dedicated body field; nil when the request did not send it.
	// A literal null resets.
	field json.RawMessage
	// allowed is true for a human owner or admin. A task token, a cloud node
	// credential and an agent actor are never allowed, whatever role the user
	// behind them holds.
	allowed bool
}

// compactAgentChange is a committed effective change of the setting.
type compactAgentChange struct {
	changed        bool
	oldID, newID   string
	cancelledTasks []db.AgentTaskQueue
}

// parseCompactAgentValue canonicalizes a sent value: nil resets, a string must
// be an agent id.
func parseCompactAgentValue(v any) (string, error) {
	if v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", errCompactAgentInvalid
	}
	id, err := util.ParseUUID(s)
	if err != nil {
		return "", errCompactAgentInvalid
	}
	return util.UUIDToString(id), nil
}

// reconcileCompactAgent rewrites the key of an incoming settings object and
// returns the value the write leaves. A request that does not mention the key
// clears it, like any other settings replacement; the dedicated field wins over
// the nested one. clientSettings is false when incoming is a clone of the
// stored blob, which keeps the stored value.
func reconcileCompactAgent(incoming map[string]any, field json.RawMessage, clientSettings bool, stored string) (string, error) {
	var sent any
	switch {
	case field != nil:
		if err := json.Unmarshal(field, &sent); err != nil {
			return "", errCompactAgentInvalid
		}
	case clientSettings:
		sent = incoming[compactAgentIDKey]
	default:
		return stored, nil
	}
	want, err := parseCompactAgentValue(sent)
	if err != nil {
		return "", err
	}
	if want == "" {
		delete(incoming, compactAgentIDKey)
	} else {
		incoming[compactAgentIDKey] = want
	}
	return want, nil
}

// compactAgentEligible is ENG-8: an agent of this workspace that is not
// archived, not private and runs on a workspace-shared runtime.
func compactAgentEligible(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, agentID string) (bool, error) {
	id, err := util.ParseUUID(agentID)
	if err != nil {
		return false, nil
	}
	agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: id, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if agent.ArchivedAt.Valid || agent.PermissionMode == "private" || !agent.RuntimeID.Valid {
		return false, nil
	}
	runtime, err := q.GetAgentRuntimeForWorkspace(ctx, db.GetAgentRuntimeForWorkspaceParams{ID: agent.RuntimeID, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return runtime.Visibility == "public", nil
}

// applyCompactAgentChange runs inside the saveWorkspace transaction, after the
// workspace row is written: it checks eligibility of a new value and cancels
// every live compaction task. The compare, the check, the write and the cancel
// share the workspace row lock, so no task survives a change.
func applyCompactAgentChange(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, oldID, newID string, allowed bool) (compactAgentChange, error) {
	change := compactAgentChange{oldID: oldID, newID: newID}
	if oldID == newID {
		return change, nil
	}
	if !allowed {
		return change, errCompactAgentForbidden
	}
	if newID != "" {
		ok, err := compactAgentEligible(ctx, q, workspaceID, newID)
		if err != nil {
			return change, err
		}
		if !ok {
			return change, errCompactAgentIneligible
		}
	}
	change.changed = true
	return change, nil
}

// cancelCompactionTasks cancels the workspace's live compaction tasks. Lock
// order is chat_session, then agent_task_queue, as every chat writer takes it.
func cancelCompactionTasks(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID) ([]db.AgentTaskQueue, error) {
	if _, err := q.LockChatSessionsOfActiveCompactionTasks(ctx, workspaceID); err != nil {
		return nil, err
	}
	cancelled, err := q.CancelCompactionTasksByWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	return cancelled, service.SettleTerminalTaskState(ctx, q, cancelled...)
}

// publishCompactAgentChange runs after the commit. It logs the change and tells
// clients about the cancelled tasks; neither carries transcript text.
func (h *Handler) publishCompactAgentChange(ctx context.Context, workspaceID, actorType, actorID string, change compactAgentChange) {
	if !change.changed {
		return
	}
	slog.Info(compactAgentChangedEvent,
		"workspace_id", workspaceID, "actor_type", actorType, "actor_id", actorID,
		"old_value", change.oldID, "new_value", change.newID, "cancelled_tasks", len(change.cancelledTasks))
	h.TaskService.BroadcastCancelledTasks(ctx, workspaceID, change.cancelledTasks)
}
