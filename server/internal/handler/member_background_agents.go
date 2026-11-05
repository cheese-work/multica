package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/bgagents"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A member's own background-agent chains live in member_setting under this
// key (CHE-4111). Every read is keyed by the caller's identity: no route takes
// a user id, so one member cannot read another's row. Writes are in
// member_background_agents_write.go.
const memberBackgroundAgentsKey = "background_agents"

// GetMyBackgroundAgents returns the caller's own value, or {} when unset.
// ?job= validates against the user-scoped catalog first: routing is
// workspace-only and answers 400.
func (h *Handler) GetMyBackgroundAgents(w http.ResponseWriter, r *http.Request) {
	member, ok := h.requireWorkspaceMember(w, r, workspaceIDFromURL(r, "id"), "workspace not found")
	if !ok {
		return
	}
	if job := r.URL.Query().Get("job"); job != "" {
		if bad := userScopedJob(bgagents.Job(job)); bad != nil {
			writeErrorCode(w, http.StatusBadRequest, bad.Code, bad.Msg)
			return
		}
	}
	raw, err := h.memberBackgroundAgents(r.Context(), member.WorkspaceID, member.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load background agents")
		return
	}
	out := map[string]any{}
	if raw != nil {
		_ = json.Unmarshal(raw, &out) // stored value is always an object; see memberBackgroundAgents
	}
	if job := r.URL.Query().Get("job"); job != "" {
		v, found := out[job]
		out = map[string]any{}
		if found {
			out[job] = v
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func userScopedJob(job bgagents.Job) *bgagents.Error {
	spec, ok := bgagents.SpecFor(job)
	switch {
	case !ok:
		return &bgagents.Error{Code: bgagents.CodeUnknownJob, Msg: "unknown background job " + string(job)}
	case !spec.UserScoped:
		return &bgagents.Error{Code: bgagents.CodeRoutingUserScope, Msg: "job " + string(job) + " is workspace-only"}
	}
	return nil
}

// memberBackgroundAgents returns the caller's stored value, nil when unset.
func (h *Handler) memberBackgroundAgents(ctx context.Context, ws, user pgtype.UUID) ([]byte, error) {
	return h.memberBackgroundAgentsWith(ctx, h.Queries, ws, user)
}

func (h *Handler) memberBackgroundAgentsWith(ctx context.Context, q *db.Queries, ws, user pgtype.UUID) ([]byte, error) {
	row, err := q.GetMemberSetting(ctx, db.GetMemberSettingParams{WorkspaceID: ws, UserID: user, Key: memberBackgroundAgentsKey})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return row.Value, err
}

// ResolveBackgroundAgents picks the chain for job on behalf of user: user chain
// first, then workspace, else none (see bgagents.Resolve). Eligibility is
// checked now, not at write time. A user-chain agent must be live in this
// workspace and pass canInvokeAgent for that user; a private agent the user
// owns passes. A workspace-chain agent must pass ENG-8. A failed lookup skips
// the slot.
func (h *Handler) ResolveBackgroundAgents(ctx context.Context, ws, user pgtype.UUID, job bgagents.Job) (bgagents.Resolution, error) {
	wsRow, err := h.Queries.GetWorkspace(ctx, ws)
	if err != nil {
		return bgagents.Resolution{}, err
	}
	var settings map[string]json.RawMessage
	_ = json.Unmarshal(wsRow.Settings, &settings)
	workspaceCfg := parseStoredBackgroundAgents(settings[backgroundAgentsKey], bgagents.WorkspaceScope, ws)
	userRaw, err := h.memberBackgroundAgents(ctx, ws, user)
	if err != nil {
		return bgagents.Resolution{}, err
	}
	userCfg := parseStoredBackgroundAgents(userRaw, bgagents.UserScope, ws)

	wsID, userID := uuidToString(ws), uuidToString(user)
	eligible := func(src bgagents.Source, agentID string) bool {
		id, err := parseUUIDStrict(agentID)
		if err != nil {
			return false
		}
		agent, err := h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: id, WorkspaceID: ws})
		if err != nil {
			return false
		}
		if src == bgagents.WorkspaceSource {
			return bgagents.WorkspaceEligible(agent)
		}
		return !agent.ArchivedAt.Valid && agent.RuntimeID.Valid && h.canInvokeAgent(ctx, agent, "member", userID, "", wsID)
	}
	return bgagents.Resolve(job, userCfg, workspaceCfg, eligible), nil
}

// parseStoredBackgroundAgents treats an unset or invalid stored value as empty:
// a bad row must not break every task, and the write paths validate already.
func parseStoredBackgroundAgents(raw []byte, scope bgagents.Scope, ws pgtype.UUID) bgagents.Config {
	if len(raw) == 0 {
		return nil
	}
	cfg, err := bgagents.Parse(raw, scope)
	if err != nil {
		slog.Warn("ignoring invalid stored background agents", "scope", scope, "workspace_id", uuidToString(ws), "error", err)
		return nil
	}
	return cfg
}
