package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// Interactive DM gate (CHE-5918, CHE-3073 v11 slice 1a). One server check for
// every interactive action on a `dm:` source (bind, send a turn, find-or-create):
// the workspace flag is on, and the actor is both agent.owner_id and the owner
// of the agent's current runtime. It does not apply to `thread:` sources.
// Callers re-run it on every action, never cache the answer.

// Stable refusal codes.
const (
	codeInteractiveDMFlagOff              = "flag_off"
	codeInteractiveDMNotAgentOwner        = "not_agent_owner"
	codeInteractiveDMRuntimeOwnerless     = "runtime_ownerless"
	codeInteractiveDMRuntimeOwnerMismatch = "runtime_owner_mismatch"
	codeInteractiveDMAgentNotFound        = "agent_not_found"
)

const interactiveDMGateEvent = "interactive_dm_gate"

type interactiveDMRefusal struct {
	Status  int
	Code    string
	Message string
}

func refuseInteractiveDM(status int, code, message string) *interactiveDMRefusal {
	return &interactiveDMRefusal{Status: status, Code: code, Message: message}
}

// decideInteractiveDM is the pure rule. runtime is the agent's current runtime.
func decideInteractiveDM(flagOn bool, actor pgtype.UUID, agent db.Agent, runtime db.AgentRuntime) *interactiveDMRefusal {
	switch {
	case !flagOn:
		return refuseInteractiveDM(http.StatusForbidden, codeInteractiveDMFlagOff, "Interactive DM is off for this workspace.")
	case !actor.Valid || !agent.OwnerID.Valid || agent.OwnerID != actor:
		return refuseInteractiveDM(http.StatusForbidden, codeInteractiveDMNotAgentOwner, "Only the agent's owner can use interactive DM with it.")
	case !runtime.OwnerID.Valid:
		return refuseInteractiveDM(http.StatusForbidden, codeInteractiveDMRuntimeOwnerless, "This agent's runtime has no owner.")
	case runtime.OwnerID != actor:
		return refuseInteractiveDM(http.StatusForbidden, codeInteractiveDMRuntimeOwnerMismatch, "Only the owner of the agent's runtime can use interactive DM with it.")
	}
	return nil
}

// checkInteractiveDM loads the agent and its current runtime and decides.
// A nil refusal with a nil error means allowed. q may be a transaction's
// queries so a caller holding a lock re-reads under it. The flag is judged
// for workspaceID whatever evaluation context ctx carries.
func (h *Handler) checkInteractiveDM(ctx context.Context, q *db.Queries, workspaceID, actorUserID string, agentID pgtype.UUID) (*interactiveDMRefusal, error) {
	ec := featureflag.EvalContextFrom(ctx)
	ec.WorkspaceID = workspaceID
	if ec.UserID == "" {
		ec.UserID = actorUserID
	}
	flagOn := featureflags.InteractiveDMEnabled(featureflag.WithEvalContext(ctx, ec), h.FeatureFlags)

	wsID, err := util.ParseUUID(workspaceID)
	if err != nil {
		return refuseInteractiveDM(http.StatusNotFound, codeInteractiveDMAgentNotFound, "Agent not found."), nil
	}
	actor, _ := util.ParseUUID(actorUserID)

	agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agentID, WorkspaceID: wsID})
	if errors.Is(err, pgx.ErrNoRows) {
		return refuseInteractiveDM(http.StatusNotFound, codeInteractiveDMAgentNotFound, "Agent not found."), nil
	}
	if err != nil {
		return nil, err
	}
	// The runtime is read even when the flag is off, so the log below is complete.
	runtime, err := q.GetAgentRuntimeForWorkspace(ctx, db.GetAgentRuntimeForWorkspaceParams{ID: agent.RuntimeID, WorkspaceID: wsID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	// A missing runtime row has a zero (ownerless) owner and is refused as such.

	refusal := decideInteractiveDM(flagOn, actor, agent, runtime)
	attrs := []any{"workspace_id", workspaceID, "agent_id", uuidToString(agentID), "actor_id", actorUserID}
	if refusal != nil {
		slog.WarnContext(ctx, interactiveDMGateEvent, append(attrs, "decision", "refused", "code", refusal.Code)...)
		return refusal, nil
	}
	slog.DebugContext(ctx, interactiveDMGateEvent, append(attrs, "decision", "granted")...)
	return nil, nil
}

// requireInteractiveDM is the HTTP form: it writes the refusal (403 with the
// reason code) or a 500 and returns false.
func (h *Handler) requireInteractiveDM(ctx context.Context, w http.ResponseWriter, q *db.Queries, workspaceID, actorUserID string, agentID pgtype.UUID) bool {
	refusal, err := h.checkInteractiveDM(ctx, q, workspaceID, actorUserID, agentID)
	if err != nil {
		slog.ErrorContext(ctx, interactiveDMGateEvent, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to check interactive DM access")
		return false
	}
	if refusal != nil {
		writeErrorCode(w, refusal.Status, refusal.Code, refusal.Message)
		return false
	}
	return true
}
