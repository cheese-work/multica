package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	SweepFailed bool    `json:"sweep_failed"`
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
			RunsRevoked: row.RunsRevoked, SweepFailed: row.SweepFailed, At: timestampToString(row.CreatedAt),
		})
	}
	return out, nil
}

// changeGitHubDispatchHalt activates or lifts the switch. The switch row and
// its audit row commit alone, under the exclusive lock every external admission
// path holds shared: from that commit on every admission sees the halt, and a
// crash or failing sweep cannot take it back. Then, when halting, the sweep
// cancels the runs created before that commit; the cutoff fences a sweep that
// outlives its lease from touching a run started after a restore. A halt is
// always allowed, even while an earlier sweep is pending. The audit row carries
// a sweep_pending lease until its sweep ends. A restore that finds a live lease
// is refused (errGitHubDispatchHaltBusy) instead of waiting, so no change holds
// one connection while needing another; a lease older than ten minutes counts as
// a crashed sweep, and a failed sweep stays unfinished; the restore reruns both
// first and does not proceed without. A failed sweep leaves the halt in force, marks the audit row
// sweep_failed and returns the error with the count revoked so far; repeating
// the halt retries it.
func (h *Handler) changeGitHubDispatchHalt(ctx context.Context, audit db.RecordGitHubDispatchHaltAuditParams) (revoked int, sweepErr error, err error) {
	if !audit.Halted {
		if err := h.finishAbandonedSweeps(ctx, audit.WorkspaceID); err != nil {
			return 0, nil, err
		}
	}
	recorded, err := h.commitGitHubDispatchHalt(ctx, audit)
	if err != nil {
		return 0, nil, err
	}
	if !audit.Halted {
		return 0, nil, nil
	}
	// The change is committed: finish the sweep even if the caller hangs up.
	ctx = context.WithoutCancel(ctx)
	revoked, sweepErr = h.sweepGitHubTriggeredRuns(ctx, h.Queries, audit.WorkspaceID, audit.Scope, recorded.CreatedAt)
	if err := h.Queries.SetGitHubDispatchHaltAuditSweep(ctx, db.SetGitHubDispatchHaltAuditSweepParams{ID: recorded.ID, RunsRevoked: int32(revoked), SweepFailed: sweepErr != nil}); err != nil {
		slog.Error("github dispatch halt: audit sweep result not saved", "audit_id", uuidToString(recorded.ID), "err", err)
	}
	return revoked, sweepErr, nil
}

// finishAbandonedSweeps runs again the sweep of every halt whose sweep failed or whose lease expired.
// Its audit row keeps its flags when the rerun fails, so the restore is refused until a sweep completes.
func (h *Handler) finishAbandonedSweeps(ctx context.Context, workspaceID pgtype.UUID) error {
	abandoned, err := h.Queries.ListAbandonedGitHubDispatchHaltSweeps(ctx, workspaceID)
	if err != nil {
		return err
	}
	for _, sweep := range abandoned {
		revoked, err := h.sweepGitHubTriggeredRuns(ctx, h.Queries, workspaceID, sweep.Scope, sweep.CreatedAt)
		if err != nil {
			return fmt.Errorf("finishing an earlier sweep of %s: %w", sweep.Scope, err)
		}
		if err := h.Queries.SetGitHubDispatchHaltAuditSweep(ctx, db.SetGitHubDispatchHaltAuditSweepParams{ID: sweep.ID, RunsRevoked: int32(revoked)}); err != nil {
			return err
		}
	}
	return nil
}

// errGitHubDispatchHaltBusy means an earlier halt's sweep is running or failed since the restore looked.
var errGitHubDispatchHaltBusy = errors.New("an earlier kill switch change has not finished revoking runs")

func (h *Handler) commitGitHubDispatchHalt(ctx context.Context, audit db.RecordGitHubDispatchHaltAuditParams) (db.RecordGitHubDispatchHaltAuditRow, error) {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return db.RecordGitHubDispatchHaltAuditRow{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	q := h.Queries.WithTx(tx)
	if err := q.LockGitHubDispatchHaltExclusive(ctx, audit.WorkspaceID); err != nil {
		return db.RecordGitHubDispatchHaltAuditRow{}, err
	}
	if !audit.Halted {
		busy, err := q.HasUnfinishedGitHubDispatchHaltSweep(ctx, audit.WorkspaceID)
		if err != nil {
			return db.RecordGitHubDispatchHaltAuditRow{}, err
		}
		if busy {
			return db.RecordGitHubDispatchHaltAuditRow{}, errGitHubDispatchHaltBusy
		}
	}
	if audit.Halted {
		err = q.HaltGitHubDispatch(ctx, db.HaltGitHubDispatchParams{WorkspaceID: audit.WorkspaceID, Scope: audit.Scope, HaltedBy: audit.ActorID})
	} else {
		_, err = q.ResumeGitHubDispatch(ctx, db.ResumeGitHubDispatchParams{WorkspaceID: audit.WorkspaceID, Scope: audit.Scope})
	}
	if err != nil {
		return db.RecordGitHubDispatchHaltAuditRow{}, err
	}
	recorded, err := q.RecordGitHubDispatchHaltAudit(ctx, audit)
	if err != nil {
		return db.RecordGitHubDispatchHaltAuditRow{}, err
	}
	return recorded, tx.Commit(ctx)
}

// sweepGitHubTriggeredRuns revokes the live GitHub-triggered runs in a scope. A
// run's token goes first: the sweep selects live runs only, so a run cancelled
// with its token still valid would never be found by a retry.
func (h *Handler) sweepGitHubTriggeredRuns(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, scope string, before pgtype.Timestamptz) (int, error) {
	ids, err := q.ListGitHubTriggeredActiveTasks(ctx, db.ListGitHubTriggeredActiveTasksParams{WorkspaceID: workspaceID, Scope: scope, Before: before})
	if err != nil {
		return 0, fmt.Errorf("listing live GitHub-triggered runs: %w", err)
	}
	revoked := 0
	var cancelErrs []error
	for _, id := range ids {
		if err := q.DeleteTaskTokensByTask(ctx, id); err != nil {
			return revoked, errors.Join(append(cancelErrs, fmt.Errorf("revoking a run token: %w", err))...)
		}
		if _, err := h.TaskService.CancelTaskWithReason(ctx, id, "cancelled by GitHub dispatch kill switch", "github_dispatch_halted"); err != nil {
			slog.Warn("github dispatch halt: cancel run failed", "task_id", uuidToString(id), "err", err)
			cancelErrs = append(cancelErrs, fmt.Errorf("cancelling run %s: %w", uuidToString(id), err))
			continue
		}
		revoked++
	}
	return revoked, errors.Join(cancelErrs...)
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
	revoked, sweepErr, err := h.changeGitHubDispatchHalt(ctx, db.RecordGitHubDispatchHaltAuditParams{
		WorkspaceID: workspaceID, Scope: scope, Halted: *req.Halted, ActorID: userID, ActorSource: actorSource,
	})
	if errors.Is(err, errGitHubDispatchHaltBusy) {
		writeError(w, http.StatusConflict, err.Error()+"; retry shortly")
		return
	}
	if err != nil {
		slog.Error("github dispatch halt: change failed", "workspace_id", uuidToString(workspaceID), "scope", scope, "err", err)
		writeError(w, http.StatusInternalServerError, "failed to change the kill switch")
		return
	}
	if sweepErr != nil {
		slog.Error("github dispatch halt: sweep incomplete", "workspace_id", uuidToString(workspaceID), "scope", scope, "err", sweepErr)
		writeError(w, http.StatusInternalServerError, "kill switch is on, but revoking live GitHub-triggered runs failed")
		return
	}
	slog.Warn("github dispatch halt changed", "workspace_id", uuidToString(workspaceID), "scope", scope, "halted", *req.Halted, "actor", requestUserID(r), "runs_revoked", revoked)
	halts, err := h.listGitHubDispatchHalts(r, uuidToString(workspaceID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read dispatch halts")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"halts": halts, "runs_revoked": revoked})
}
