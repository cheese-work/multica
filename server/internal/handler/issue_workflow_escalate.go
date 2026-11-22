package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Escalate (CHE-7682): an issue that follows Quick returns to the full
// workflow its newest quick_entry row names. The target is a server fact; the
// request carries a reason and nothing else.

const (
	activityWorkflowEscalated = "workflow_escalated"
	intakeCallEscalated       = "escalated"
	// escalateReasonRollback is the reason that needs no Estimate: the issue
	// goes back where it was, so there is no new size to state.
	escalateReasonRollback = "rollback"
)

// EscalateIssueWorkflowRequest is the whole request body. Unknown fields are
// refused, so a caller cannot name a target.
type EscalateIssueWorkflowRequest struct {
	Reason string `json:"reason"`
}

// EscalateIssueWorkflowResponse reports where the switch stands: "applied"
// when the issue follows the full workflow, "pending" when a live task keeps
// it on Quick until the last one ends.
type EscalateIssueWorkflowResponse struct {
	State      string `json:"state"`
	WorkflowID string `json:"workflow_id"`
}

// escalateRefusal is a refusal to escalate, ready to write.
type escalateRefusal struct {
	status int
	code   string
	msg    string
}

func (e *escalateRefusal) Error() string { return e.msg }

// EscalateIssueWorkflow moves a Quick issue to its full workflow. It records
// the reason, the intake call and the pending switch in one transaction, then
// applies the switch: at once when no task of the issue can still run, else
// after the last one ends (CHE-7669).
func (h *Handler) EscalateIssueWorkflow(w http.ResponseWriter, r *http.Request) {
	loaded, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	if !h.requireProjectWorkflowsV1(w, r) {
		return
	}
	var req EscalateIssueWorkflowRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" {
		writeErrorCode(w, http.StatusBadRequest, "reason_required", "reason is required")
		return
	}
	workspaceID := uuidToString(loaded.WorkspaceID)
	member, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found")
	if !ok {
		return
	}
	actorType, actorID := h.resolveActor(r, uuidToString(member.UserID), workspaceID)

	ctx := r.Context()
	target, err := h.recordEscalation(ctx, loaded.ID, req.Reason, actorType, actorID)
	var refusal *escalateRefusal
	switch {
	case errors.As(err, &refusal):
		writeErrorCode(w, refusal.status, refusal.code, refusal.msg)
		return
	case err != nil:
		slog.Warn("escalate issue workflow", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to escalate")
		return
	}

	h.ApplyDeferredWorkflowSwitch(ctx, loaded.ID)

	pending, err := h.Queries.HasIssueWorkflowSwitchPending(ctx, loaded.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to escalate")
		return
	}
	if pending {
		writeJSON(w, http.StatusAccepted, EscalateIssueWorkflowResponse{State: "pending", WorkflowID: uuidToString(target)})
		return
	}
	now, err := h.Queries.GetIssue(ctx, loaded.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to escalate")
		return
	}
	if now.WorkflowID != target {
		// The full workflow refused the move; the apply blocked the issue and
		// commented the cause.
		writeErrorCode(w, http.StatusConflict, "switch_refused", "the full workflow refused the move; the issue is blocked with the reason")
		return
	}
	writeJSON(w, http.StatusOK, EscalateIssueWorkflowResponse{State: "applied", WorkflowID: uuidToString(target)})
}

// recordEscalation checks that the issue may escalate and, in one transaction,
// writes the pending switch and its two activities. It returns the target.
func (h *Handler) recordEscalation(ctx context.Context, issueID pgtype.UUID, reason, actorType, actorID string) (pgtype.UUID, error) {
	lk, err := h.lockDeferredSwitchIssue(ctx, issueID)
	if err != nil {
		return pgtype.UUID{}, err
	}
	defer lk.close(ctx)
	qtx, issue := lk.qtx, lk.issue

	workspace, err := qtx.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return pgtype.UUID{}, err
	}
	def, _, err := issueworkflow.ForIssue(ctx, qtx, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
	if err != nil {
		return pgtype.UUID{}, err
	}
	if !issueworkflow.IsQuick(def, issueworkflow.DecodeQuickFirst(workspace.Settings)) {
		return pgtype.UUID{}, &escalateRefusal{http.StatusConflict, "not_quick", "the issue does not follow the Quick workflow"}
	}
	target, err := fullWorkflowOfNewestQuickEntry(ctx, qtx, issue)
	if err != nil {
		return pgtype.UUID{}, err
	}
	needsEstimate := (issue.Status == "todo" || issue.Status == "investigating") && !strings.EqualFold(reason, escalateReasonRollback)
	facts := loadIntakeFacts(ctx, qtx, issue)
	estimate := facts.Estimate()
	if needsEstimate && estimate == "" {
		if facts.Err != "" {
			return pgtype.UUID{}, errors.New(facts.Err)
		}
		return pgtype.UUID{}, &escalateRefusal{http.StatusUnprocessableEntity, "estimate_required",
			"an Estimate line is required to escalate from " + issue.Status + " (reason \"" + escalateReasonRollback + "\" needs none)"}
	}

	if err := qtx.UpsertIssueWorkflowSwitchPending(ctx, db.UpsertIssueWorkflowSwitchPendingParams{
		IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, WorkflowID: target, Reason: reason,
	}); err != nil {
		return pgtype.UUID{}, err
	}
	if err := recordEntryActivity(ctx, qtx, issue, actorType, actorID, activityWorkflowEscalated,
		map[string]any{"reason": reason, "workflow_id": uuidToString(target), "from_workflow_id": uuidToString(issue.WorkflowID)}); err != nil {
		return pgtype.UUID{}, err
	}
	if err := recordEntryActivity(ctx, qtx, issue, actorType, actorID, activityIntakeCall,
		map[string]any{"call": intakeCallEscalated, "estimate": estimate, "step": issue.Status}); err != nil {
		return pgtype.UUID{}, err
	}
	if err := lk.tx.Commit(ctx); err != nil {
		return pgtype.UUID{}, err
	}
	return target, nil
}

// fullWorkflowOfNewestQuickEntry is the workflow the newest quick_entry row
// names, as long as it still exists in the workspace.
func fullWorkflowOfNewestQuickEntry(ctx context.Context, q *db.Queries, issue db.Issue) (pgtype.UUID, error) {
	refuse := func(msg string) error {
		return &escalateRefusal{http.StatusConflict, "no_full_workflow", msg}
	}
	row, err := q.GetLatestIssueActivityByAction(ctx, db.GetLatestIssueActivityByActionParams{IssueID: issue.ID, Action: issueworkflow.ActivityQuickEntry})
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, refuse("the issue has no record of entering Quick, so there is no full workflow to return to")
	}
	if err != nil {
		return pgtype.UUID{}, err
	}
	var details struct {
		FullWorkflowID string `json:"full_workflow_id"`
	}
	if err := json.Unmarshal(row.Details, &details); err != nil {
		return pgtype.UUID{}, refuse("the issue's Quick entry record is unreadable")
	}
	target, err := util.ParseUUID(details.FullWorkflowID)
	if details.FullWorkflowID == "" || err != nil {
		return pgtype.UUID{}, refuse("the issue followed no workflow before Quick")
	}
	if _, err := q.GetIssueWorkflow(ctx, db.GetIssueWorkflowParams{ID: target, WorkspaceID: issue.WorkspaceID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pgtype.UUID{}, refuse("the workflow the issue followed before Quick no longer exists")
		}
		return pgtype.UUID{}, err
	}
	return target, nil
}
