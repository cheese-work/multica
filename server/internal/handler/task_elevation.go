package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Elevation state, per-operation authority and the revocation fence
// (CHE-2216 plan 3.8). The claim records a task's elevation once; every
// elevated action then calls authorize against live state, so a stale claim
// cannot extend a grant.
const (
	elevationElevated = "elevated"

	elevationFenceInterval = 10 * time.Second
)

var opInputHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// isLaunchScope reports scopes granted at launch and ended with the run: they
// cannot be authorized per operation (decision 2a).
func isLaunchScope(s string) bool { return s == "docker" || strings.HasPrefix(s, "device:") }

type authorizeElevatedOpRequest struct {
	Epoch     int64  `json:"epoch"`
	Scope     string `json:"scope"`
	InputHash string `json:"input_hash"`
}

// AuthorizeElevatedOp records one operation the caller is about to run with the
// exact input it hashed. One transaction under the task's row lock checks grant
// liveness, scope, state and epoch, so a fence either precedes the check or
// waits for it. Task and issue taint also refuse, read under share locks so a
// writer serializes with them. The issue row is locked first, before any task
// row: the one lock order of every path that touches a run of an issue.
func (h *Handler) AuthorizeElevatedOp(w http.ResponseWriter, r *http.Request) {
	task, ok := h.requireDaemonTaskAccess(w, r, chi.URLParam(r, "taskId"))
	if !ok {
		return
	}
	var req authorizeElevatedOpRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if !validGrantScope(req.Scope) || !opInputHash.MatchString(req.InputHash) {
		writeError(w, http.StatusBadRequest, "scope must be a grant scope and input_hash 64 lowercase hex characters")
		return
	}
	if isLaunchScope(req.Scope) {
		writeError(w, http.StatusForbidden, "launch-time scopes are not authorized per operation")
		return
	}

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to authorize")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	if !task.IssueID.Valid {
		writeError(w, http.StatusForbidden, "task is not elevated")
		return
	}
	issueTaint, err := lockedIssueTaint(r.Context(), qtx, task.IssueID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to authorize")
		return
	}
	te, err := qtx.LockTaskElevation(r.Context(), task.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusForbidden, "task is not elevated")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to authorize")
		return
	}
	taint, err := qtx.LockTaskTaintIDs(r.Context(), task.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to authorize")
		return
	}
	live, err := qtx.CountLiveGrantsByID(r.Context(), te.GrantIds)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to authorize")
		return
	}
	switch {
	case len(taint) > 0:
		writeError(w, http.StatusForbidden, "task is tainted")
	case len(issueTaint) > 0:
		writeError(w, http.StatusForbidden, "issue is tainted")
	case te.State != elevationElevated:
		writeError(w, http.StatusForbidden, "task is fenced")
	case te.Epoch != req.Epoch:
		writeError(w, http.StatusForbidden, "stale epoch")
	case live != int64(len(te.GrantIds)):
		writeError(w, http.StatusForbidden, "grant expired or revoked")
	case !slices.Contains(te.Scopes, req.Scope):
		writeError(w, http.StatusForbidden, "scope not granted")
	default:
		op, err := qtx.CreateElevatedOp(r.Context(), db.CreateElevatedOpParams{
			WorkspaceID: te.WorkspaceID, TaskID: te.TaskID, Epoch: te.Epoch, Scope: req.Scope, InputHash: req.InputHash,
		})
		if err == nil {
			err = tx.Commit(r.Context())
		}
		if err != nil {
			slog.Warn("authorize elevated op failed", "task_id", uuidToString(task.ID), "error", err)
			writeError(w, http.StatusInternalServerError, "failed to authorize")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"op_id": uuidToString(op.OpID), "epoch": op.Epoch, "lease_until": op.LeaseUntil.Time.UTC().Format(time.RFC3339),
		})
	}
}

type completeElevatedOpRequest struct {
	Result string `json:"result"`
}

// CompleteElevatedOp reports how an authorized operation ended. An operation
// whose lease lapsed (unknown) may still be resolved by a late honest report.
func (h *Handler) CompleteElevatedOp(w http.ResponseWriter, r *http.Request) {
	task, ok := h.requireDaemonTaskAccess(w, r, chi.URLParam(r, "taskId"))
	if !ok {
		return
	}
	opID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "opId"), "op_id")
	if !ok {
		return
	}
	var req completeElevatedOpRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if !slices.Contains([]string{"completed", "failed", "aborted"}, req.Result) {
		writeError(w, http.StatusBadRequest, "result must be completed, failed or aborted")
		return
	}
	op, err := h.Queries.CompleteElevatedOp(r.Context(), db.CompleteElevatedOpParams{OpID: opID, TaskID: task.ID, State: req.Result})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "no open operation")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to record the result")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"op_id": uuidToString(op.OpID), "state": op.State})
}

func decodeStrict(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

// RunElevationFenceSweeper runs the expiry and revocation fence on a ticker.
func (h *Handler) RunElevationFenceSweeper(ctx context.Context) {
	ticker := time.NewTicker(elevationFenceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := h.fenceRound(ctx); err != nil {
				slog.Warn("elevation fence round failed", "error", err)
			}
		}
	}
}

// fenceRound is one fence pass in one transaction: fence tasks whose grant
// lapsed, mark operations whose lease lapsed unknown, and finish fences that
// have drained. An unknown operation never lets a fence finish.
func (h *Handler) fenceRound(ctx context.Context) error {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := h.Queries.WithTx(tx)
	fenced, err := q.FenceLapsedTaskElevations(ctx)
	if err != nil {
		return err
	}
	tainted, err := q.FenceTaintedTaskElevations(ctx, pgtype.UUID{})
	if err != nil {
		return err
	}
	fenced = append(fenced, tainted...)
	unknown, err := q.MarkLapsedElevatedOpsUnknown(ctx)
	if err != nil {
		return err
	}
	done, err := q.FinishDrainedTaskFences(ctx, pgtype.UUID{})
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	for _, id := range fenced {
		slog.Info("elevation fence started", "task_id", uuidToString(id))
	}
	for _, op := range unknown {
		slog.Warn("elevated operation lease lapsed with no report; fence cannot finish", "op_id", uuidToString(op.OpID), "task_id", uuidToString(op.TaskID))
	}
	for _, id := range done {
		slog.Info("elevation fence finished", "task_id", uuidToString(id))
	}
	return nil
}

// GetTaskElevation tells the daemon where the task's fence stands. The daemon
// polls it: a state other than elevated, or a newer epoch, starts its local fence.
func (h *Handler) GetTaskElevation(w http.ResponseWriter, r *http.Request) {
	task, ok := h.requireDaemonTaskAccess(w, r, chi.URLParam(r, "taskId"))
	if !ok {
		return
	}
	te, err := h.Queries.GetTaskElevation(r.Context(), task.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "task is not elevated")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read elevation")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": te.State, "epoch": te.Epoch})
}

// EndTaskLaunchScopes records that the processes holding the task's docker or
// device: scopes exited, which lets a fence finish. Idempotent; refused while
// the task is still elevated.
func (h *Handler) EndTaskLaunchScopes(w http.ResponseWriter, r *http.Request) {
	task, ok := h.requireDaemonTaskAccess(w, r, chi.URLParam(r, "taskId"))
	if !ok {
		return
	}
	_, err := h.Queries.EndTaskLaunchScopes(r.Context(), task.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, "task is not fenced")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to record launch-scope end")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ended": true})
}

// Downgrade handshake (plan 3.4 and 3.8, slice B4c). The read proxy asks to
// deliver withheld bytes; the server fences the task at once, and only when
// every epoch-N operation has ended and the launch scopes are gone does it taint
// the task and the issue and tell the proxy it may deliver. Taint comes first:
// the bytes leave the proxy only after the deliver call returned 200.

type downgradeRequest struct {
	Epoch int64 `json:"epoch"`
}

func downgradeTaintID(taskID pgtype.UUID) string { return "downgrade:" + uuidToString(taskID) }

// StartTaskDowngrade fences the task for a downgrade. A task already fencing
// or final answers with its state: the proxy may repeat the call. An epoch the
// run never held is refused.
func (h *Handler) StartTaskDowngrade(w http.ResponseWriter, r *http.Request) {
	task, ok := h.requireDaemonTaskAccess(w, r, chi.URLParam(r, "taskId"))
	if !ok {
		return
	}
	var req downgradeRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if !task.IssueID.Valid {
		writeError(w, http.StatusForbidden, "task is not elevated")
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start the downgrade")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	if err := qtx.LockIssueForTaint(r.Context(), task.IssueID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start the downgrade")
		return
	}
	te, err := qtx.LockTaskElevation(r.Context(), task.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusForbidden, "task is not elevated")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start the downgrade")
		return
	}
	if req.Epoch < 1 || req.Epoch > te.Epoch {
		writeError(w, http.StatusForbidden, "stale epoch")
		return
	}
	if te.State == elevationElevated {
		if req.Epoch != te.Epoch {
			writeError(w, http.StatusForbidden, "stale epoch")
			return
		}
		if te, err = qtx.FenceTaskElevation(r.Context(), task.ID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to start the downgrade")
			return
		}
		slog.Info("elevation fence started", "task_id", uuidToString(task.ID), "reason", "downgrade")
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start the downgrade")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": te.State, "epoch": te.Epoch})
}

// DeliverTaskDowngrade confirms that the proxy may deliver. It finishes the
// task's own fence when nothing is left to drain, then taints the task, and
// through it the issue, in the same transaction. Any operation still
// authorized or unknown, or a launch scope still live, answers 409: no taint,
// no bytes. Repeating it after success changes nothing.
func (h *Handler) DeliverTaskDowngrade(w http.ResponseWriter, r *http.Request) {
	task, ok := h.requireDaemonTaskAccess(w, r, chi.URLParam(r, "taskId"))
	if !ok {
		return
	}
	var req struct{}
	if !decodeStrict(w, r, &req) {
		return
	}
	if !task.IssueID.Valid {
		writeError(w, http.StatusForbidden, "task is not elevated")
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to deliver the downgrade")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	// The issue row first, as a taint writer takes it: issue_taint_note locks it
	// for update, so a share lock taken first would deadlock against it.
	if err := qtx.LockIssueForTaintClear(r.Context(), task.IssueID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to deliver the downgrade")
		return
	}
	te, err := qtx.LockTaskElevation(r.Context(), task.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusForbidden, "task is not elevated")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to deliver the downgrade")
		return
	}
	switch te.State {
	case elevationElevated:
		writeError(w, http.StatusConflict, "downgrade not started")
		return
	case "fencing":
		done, err := qtx.FinishDrainedTaskFences(r.Context(), task.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to deliver the downgrade")
			return
		}
		if len(done) == 0 {
			writeError(w, http.StatusConflict, "fence not complete")
			return
		}
		te.State = "restricted_final"
	}
	if err := qtx.TaintTaskForDowngrade(r.Context(), db.TaintTaskForDowngradeParams{TaskID: task.ID, TaintID: downgradeTaintID(task.ID)}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to deliver the downgrade")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to deliver the downgrade")
		return
	}
	slog.Info("downgrade delivery confirmed", "task_id", uuidToString(task.ID), "taint_id", downgradeTaintID(task.ID))
	writeJSON(w, http.StatusOK, map[string]any{"state": te.State, "epoch": te.Epoch, "taint_id": downgradeTaintID(task.ID)})
}
