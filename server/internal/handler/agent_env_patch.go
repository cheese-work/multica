package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// PatchAgentEnvRequest is the wire shape for `PATCH /api/agents/{id}/env`.
// It is a key-scoped mutation: keys it does not name are never touched, so a
// concurrent writer's unrelated additions survive.
type PatchAgentEnvRequest struct {
	Set   map[string]string `json:"set"`
	Unset []string          `json:"unset"`
	// IfRevision, when present, makes the patch conditional: it applies only
	// if the stored env still hashes to this revision (see envRevision, as
	// returned by GET/PUT/PATCH). A mismatch is 412 and nothing is written.
	IfRevision *string `json:"if_revision"`
}

// PatchAgentEnv sets and/or unsets individual env keys.
//
// Contract, and its limits:
//   - Read, condition check, merge, write and audit row happen in one
//     transaction under a row lock on the agent (SELECT ... FOR UPDATE), so
//     every other env writer that takes the same lock — PATCH and PUT — is
//     serialized against it. A PUT is still a wholesale replace by contract:
//     a PUT serialized after this patch removes whatever its body omits.
//   - The "****" sentinel is rejected, not interpreted: PATCH has no
//     "preserve" meaning because it never touches unnamed keys.
//   - The response carries every key masked as "****" plus the new revision;
//     values are never echoed. Audit rows list keys only, as for PUT.
//   - Same authorization as PUT/GET: human owner/admin or agent owner;
//     agent actors are rejected.
//   - An audit-write failure rolls the whole patch back (fail-closed).
func (h *Handler) PatchAgentEnv(w http.ResponseWriter, r *http.Request) {
	agent, member, ok := h.authorizeAgentEnv(w, r)
	if !ok {
		return
	}

	var req PatchAgentEnvRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Set) == 0 && len(req.Unset) == 0 {
		writeError(w, http.StatusBadRequest, "specify at least one key in set or unset")
		return
	}
	for k, v := range req.Set {
		if k == "" {
			writeError(w, http.StatusBadRequest, "env keys must not be empty")
			return
		}
		if v == envSentinel {
			writeError(w, http.StatusBadRequest, "set values must not be the masked marker "+envSentinel)
			return
		}
	}
	for _, k := range req.Unset {
		if k == "" {
			writeError(w, http.StatusBadRequest, "env keys must not be empty")
			return
		}
		if _, dup := req.Set[k]; dup {
			writeError(w, http.StatusBadRequest, "a key may not appear in both set and unset")
			return
		}
	}

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		slog.Error("agent_env patch: begin tx failed",
			append(logger.RequestAttrs(r), "error", err, "agent_id", uuidToString(agent.ID))...)
		writeError(w, http.StatusInternalServerError, "failed to update env")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)

	locked, err := qtx.GetAgentForUpdate(r.Context(), agent.ID)
	if err != nil {
		slog.Warn("agent_env patch: lock agent row failed",
			append(logger.RequestAttrs(r), "error", err, "agent_id", uuidToString(agent.ID))...)
		writeError(w, http.StatusInternalServerError, "failed to update env")
		return
	}
	updated, next, err := applyAgentEnvPatch(r.Context(), qtx, locked, req, verifiedApprover{UserID: member.UserID})
	if errors.Is(err, errEnvRevisionMismatch) {
		writeError(w, http.StatusPreconditionFailed, "env changed since if_revision; re-read and retry")
		return
	}
	if err != nil {
		slog.Error("agent_env patch failed; rolling back",
			append(logger.RequestAttrs(r), "error", err, "agent_id", uuidToString(agent.ID))...)
		writeError(w, http.StatusInternalServerError, "failed to update env")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		slog.Error("agent_env patch: tx commit failed",
			append(logger.RequestAttrs(r), "error", err, "agent_id", uuidToString(agent.ID))...)
		writeError(w, http.StatusInternalServerError, "failed to update env")
		return
	}

	if !h.broadcastAgentEnvUpdated(w, r, updated, member) {
		return
	}

	masked := make(map[string]string, len(next))
	for k := range next {
		masked[k] = envSentinel
	}
	writeJSON(w, http.StatusOK, AgentEnvResponse{
		AgentID:   uuidToString(updated.ID),
		CustomEnv: masked,
		Revision:  envRevision(next),
	})
}

// verifiedApprover is the human behind an env change. DecisionActionID is set
// only when the change runs from an approved decision card.
type verifiedApprover struct {
	UserID           pgtype.UUID
	DecisionActionID pgtype.UUID
}

var errEnvRevisionMismatch = errors.New("env changed since if_revision")

// applyAgentEnvPatch merges req into the locked agent's env and writes the
// audit row in the caller's transaction. It takes no locks and publishes
// nothing: the caller owns both, so an approved action and the PATCH
// endpoint share one write path. An audit failure fails the whole patch.
func applyAgentEnvPatch(ctx context.Context, queries *db.Queries, locked db.Agent, req PatchAgentEnvRequest, approver verifiedApprover) (db.Agent, map[string]string, error) {
	existing := unmarshalCustomEnv(locked)
	if req.IfRevision != nil && *req.IfRevision != envRevision(existing) {
		return db.Agent{}, nil, errEnvRevisionMismatch
	}
	next := make(map[string]string, len(existing)+len(req.Set))
	for k, v := range existing {
		next[k] = v
	}
	for k, v := range req.Set {
		next[k] = v
	}
	for _, k := range req.Unset {
		delete(next, k)
	}
	_, audit := mergeAgentEnv(existing, next)

	envBytes, err := json.Marshal(next)
	if err != nil {
		return db.Agent{}, nil, err
	}
	updated, err := queries.UpdateAgentCustomEnv(ctx, db.UpdateAgentCustomEnvParams{ID: locked.ID, CustomEnv: envBytes})
	if err != nil {
		return db.Agent{}, nil, err
	}
	detail := map[string]any{
		"agent_id":     uuidToString(locked.ID),
		"agent_name":   locked.Name,
		"operation":    "patch",
		"conditional":  req.IfRevision != nil,
		"added_keys":   audit.added,
		"removed_keys": audit.removed,
		"changed_keys": audit.changed,
	}
	if approver.DecisionActionID.Valid {
		detail["decision_action_id"] = uuidToString(approver.DecisionActionID)
	}
	details, _ := json.Marshal(detail)
	if _, err := queries.CreateActivity(ctx, db.CreateActivityParams{
		ID:          dbid.NewV7(),
		WorkspaceID: locked.WorkspaceID,
		IssueID:     pgtype.UUID{},
		ActorType:   pgtype.Text{String: "member", Valid: true},
		ActorID:     approver.UserID,
		Action:      agentEnvActivityUpdated,
		Details:     details,
	}); err != nil {
		return db.Agent{}, nil, err
	}
	return updated, next, nil
}
