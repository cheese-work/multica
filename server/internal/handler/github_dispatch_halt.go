package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// githubHaltScopeRe is '*', a GitHub owner, or owner/repo, lower-cased.
var githubHaltScopeRe = regexp.MustCompile(`^(\*|[a-z0-9._-]+(/[a-z0-9._-]+)?)$`)

type githubDispatchHaltAuditResponse struct {
	Scope       string  `json:"scope"`
	Halted      bool    `json:"halted"`
	ActorID     *string `json:"actor_id"`
	ActorSource string  `json:"actor_source"`
	RunsRevoked int32   `json:"runs_revoked"`
	At          string  `json:"at"`
}

type githubDispatchHaltResponse struct {
	Scope    string  `json:"scope"`
	HaltedBy *string `json:"halted_by"`
	HaltedAt string  `json:"halted_at"`
}

func (h *Handler) listGitHubDispatchHalts(r *http.Request, workspaceID string) ([]githubDispatchHaltResponse, error) {
	rows, err := h.Queries.ListGitHubDispatchHalts(r.Context(), parseUUID(workspaceID))
	if err != nil {
		return nil, err
	}
	out := make([]githubDispatchHaltResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, githubDispatchHaltResponse{Scope: row.Scope, HaltedBy: uuidToPtr(row.HaltedBy), HaltedAt: timestampToString(row.HaltedAt)})
	}
	return out, nil
}

func (h *Handler) listGitHubDispatchHaltAudit(r *http.Request, workspaceID string) ([]githubDispatchHaltAuditResponse, error) {
	rows, err := h.Queries.ListGitHubDispatchHaltAudit(r.Context(), parseUUID(workspaceID))
	if err != nil {
		return nil, err
	}
	out := make([]githubDispatchHaltAuditResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, githubDispatchHaltAuditResponse{
			Scope: row.Scope, Halted: row.Halted, ActorID: uuidToPtr(row.ActorID), ActorSource: row.ActorSource,
			RunsRevoked: row.RunsRevoked, At: timestampToString(row.CreatedAt),
		})
	}
	return out, nil
}

func (h *Handler) changeGitHubDispatchHalt(ctx context.Context, audit db.RecordGitHubDispatchHaltAuditParams) (pgtype.UUID, error) {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return pgtype.UUID{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	q := h.Queries.WithTx(tx)
	if err := q.LockGitHubDispatchHaltExclusive(ctx, audit.WorkspaceID); err != nil {
		return pgtype.UUID{}, err
	}
	if audit.Halted {
		err = q.HaltGitHubDispatch(ctx, db.HaltGitHubDispatchParams{WorkspaceID: audit.WorkspaceID, Scope: audit.Scope, HaltedBy: audit.ActorID})
	} else {
		_, err = q.ResumeGitHubDispatch(ctx, db.ResumeGitHubDispatchParams{WorkspaceID: audit.WorkspaceID, Scope: audit.Scope})
	}
	if err != nil {
		return pgtype.UUID{}, err
	}
	id, err := q.RecordGitHubDispatchHaltAudit(ctx, audit)
	if err != nil {
		return pgtype.UUID{}, err
	}
	return id, tx.Commit(ctx)
}

// GetGitHubDispatchHalt (GET /api/github/dispatch-halt) reads the kill switch
// without touching it.
func (h *Handler) GetGitHubDispatchHalt(w http.ResponseWriter, r *http.Request) {
	halts, err := h.listGitHubDispatchHalts(r, h.resolveWorkspaceID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read dispatch halts")
		return
	}
	audit, err := h.listGitHubDispatchHaltAudit(r, h.resolveWorkspaceID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read dispatch halt audit")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"halts": halts, "audit": audit})
}

// SetGitHubDispatchHalt (PUT /api/github/dispatch-halt) turns the kill switch
// on or off for a scope. Turning it on also cancels the live runs GitHub
// started in that scope and revokes their tokens. A machine credential may
// only turn it on: an agent must not be able to lift a halt.
func (h *Handler) SetGitHubDispatchHalt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scope  string `json:"scope"`
		Halted *bool  `json:"halted"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Halted == nil {
		writeError(w, http.StatusBadRequest, "scope and halted are required")
		return
	}
	scope := strings.ToLower(strings.TrimSpace(req.Scope))
	if !githubHaltScopeRe.MatchString(scope) {
		writeError(w, http.StatusBadRequest, "scope must be *, an owner, or owner/repo")
		return
	}
	if !*req.Halted && isMachineCredentialActor(r) {
		writeError(w, http.StatusForbidden, "only a human can turn the kill switch off")
		return
	}
	ctx := r.Context()
	workspaceID := parseUUID(h.resolveWorkspaceID(r))
	userID := parseUUID(requestUserID(r))
	actorSource := "member"
	if isMachineCredentialActor(r) {
		actorSource = r.Header.Get("X-Actor-Source")
	}
	// The switch row and its audit row commit together, under the lock every
	// intake dispatch holds shared, so no dispatch straddles the change.
	auditID, err := h.changeGitHubDispatchHalt(ctx, db.RecordGitHubDispatchHaltAuditParams{
		WorkspaceID: workspaceID, Scope: scope, Halted: *req.Halted, ActorID: userID, ActorSource: actorSource,
	})
	if err != nil {
		slog.Error("github dispatch halt: change failed", "workspace_id", uuidToString(workspaceID), "scope", scope, "err", err)
		writeError(w, http.StatusInternalServerError, "failed to change the kill switch")
		return
	}
	revoked := 0
	// The change is committed: finish the sweep and the audit count even if the caller hangs up.
	ctx = context.WithoutCancel(ctx)
	defer func() {
		if err := h.Queries.SetGitHubDispatchHaltAuditRevoked(ctx, db.SetGitHubDispatchHaltAuditRevokedParams{ID: auditID, RunsRevoked: int32(revoked)}); err != nil {
			slog.Error("github dispatch halt: audit revoked count not saved", "audit_id", uuidToString(auditID), "err", err)
		}
	}()
	if *req.Halted {
		ids, err := h.Queries.ListGitHubTriggeredActiveTasks(ctx, db.ListGitHubTriggeredActiveTasksParams{WorkspaceID: workspaceID, Scope: scope})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "kill switch is on, but listing live GitHub-triggered runs failed")
			return
		}
		for _, id := range ids {
			if _, err := h.TaskService.CancelTaskWithReason(ctx, id, "cancelled by GitHub dispatch kill switch", "github_dispatch_halted"); err != nil {
				slog.Warn("github dispatch halt: cancel run failed", "task_id", uuidToString(id), "err", err)
			}
			if err := h.Queries.DeleteTaskTokensByTask(ctx, id); err != nil {
				writeError(w, http.StatusInternalServerError, "kill switch is on, but revoking a run token failed")
				return
			}
			revoked++
		}
	}
	slog.Warn("github dispatch halt changed", "workspace_id", uuidToString(workspaceID), "scope", scope, "halted", *req.Halted, "actor", requestUserID(r), "runs_revoked", revoked)
	halts, err := h.listGitHubDispatchHalts(r, uuidToString(workspaceID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read dispatch halts")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"halts": halts, "runs_revoked": revoked})
}
