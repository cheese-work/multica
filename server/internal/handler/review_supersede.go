package handler

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Review SHA binding (CHE-2307). A review run records the PR (pull_request_id),
// the head it reviews (context.head_sha) and its workflow step. A new head
// cancels the review of the old one through the stop gate, and one replacement
// per (issue, PR, step) is released once the gate opens. The replacement is not
// a stored row: it is the cancelled superseded_head run itself, marked by
// replacement_released_at when its successor is enqueued, and the successor
// reads the PR's current head at release time. A push while the old run drains
// therefore changes nothing but the head the replacement will read.
//
// Author runs carry another workflow step and are never listed. Run budget
// refunds are out of scope.

// supersedeStaleReviews cancels the live review runs of a PR whose stamped head
// is no longer the PR's head, then tries to release their replacement. It is the
// ghsnapshot pipeline's reaction to a PR snapshot; a closed or merged PR has no
// head worth reviewing.
func (h *Handler) supersedeStaleReviews(ctx context.Context, prID pgtype.UUID) {
	pr, err := h.Queries.GetGitHubPullRequestByID(ctx, prID)
	if err != nil || pr.HeadSha == "" || (pr.State != "open" && pr.State != "draft") {
		return
	}
	stale, err := h.Queries.ListLiveReviewRunsForPR(ctx, db.ListLiveReviewRunsForPRParams{PullRequestID: prID, HeadSha: pr.HeadSha})
	if err != nil {
		slog.Warn("review supersede: list stale reviews failed", "pr_id", uuidToString(prID), "err", err)
		return
	}
	for _, run := range stale {
		if _, err := h.TaskService.CancelTaskWithReason(ctx, run.ID, "superseded by a newer head "+pr.HeadSha, service.FailureReasonSupersededHead); err != nil {
			slog.Warn("review supersede: cancel failed", "task_id", uuidToString(run.ID), "err", err)
			continue
		}
		h.recordReviewActivity(ctx, run.IssueID, "agent", uuidToString(run.AgentID), "review_superseded", map[string]string{
			"task_id": uuidToString(run.ID), "head_sha": pr.HeadSha,
		})
	}
	issueIDs, err := h.Queries.ListIssueIDsForPullRequest(ctx, prID)
	if err != nil {
		return
	}
	for _, id := range issueIDs {
		h.releaseSupersededReviewReplacement(ctx, id)
	}
}

// releaseSupersededReviewReplacement enqueues the one replacement review for
// each (PR, step) group of superseded runs, once the stop gate is open. It runs
// after a PR snapshot and after every cancel-ack; a closed gate makes it a
// no-op, and the marker makes a replay one.
func (h *Handler) releaseSupersededReviewReplacement(ctx context.Context, issueID pgtype.UUID) {
	if service.StopGate(ctx, h.Queries, issueID) != nil {
		return
	}
	groups, err := h.Queries.ListUnreleasedSupersededReviews(ctx, issueID)
	if err != nil || len(groups) == 0 {
		return
	}
	issue, err := h.Queries.GetIssue(ctx, issueID)
	if err != nil {
		return
	}
	released := map[string]bool{}
	for _, g := range groups {
		key := uuidToString(g.PullRequestID) + "/" + g.WorkflowStep.String
		if released[key] {
			continue
		}
		released[key] = true
		// A closed PR has no head to review: settle the group without a run.
		if h.prAwaitsReview(ctx, g.PullRequestID) && !h.dispatchIssueRun(service.WithReviewPR(ctx, g.PullRequestID), issue, service.IssueRunTrigger{
			IssueID: issue.ID, AgentID: issue.AssigneeID, AssigneeType: issue.AssigneeType.String,
		}, "agent", uuidToString(g.AgentID), "") {
			continue // not enqueued: stays unreleased for the next trigger
		}
		if err := h.Queries.MarkReviewReplacementReleased(ctx, db.MarkReviewReplacementReleasedParams{
			IssueID: issue.ID, PullRequestID: g.PullRequestID, WorkflowStep: g.WorkflowStep,
		}); err != nil {
			slog.Warn("review supersede: mark replacement released failed", "issue_id", uuidToString(issueID), "err", err)
		}
	}
}

// prAwaitsReview reports whether the PR still has a head a review can be for.
func (h *Handler) prAwaitsReview(ctx context.Context, prID pgtype.UUID) bool {
	pr, err := h.Queries.GetGitHubPullRequestByID(ctx, prID)
	return err == nil && (pr.State == "open" || pr.State == "draft")
}

// staleVerdictRun reports whether run is a review that was superseded by a
// newer head, and the head it reviewed.
func staleVerdictRun(run *db.AgentTaskQueue) (oldHead string, stale bool) {
	// stop_unconfirmed_at outlives a rewrite of status/failure_reason (b1 trigger).
	if run == nil || !run.PullRequestID.Valid ||
		(run.FailureReason.String != service.FailureReasonSupersededHead && !run.StopUnconfirmedAt.Valid) {
		return "", false
	}
	var ctxJSON struct {
		HeadSHA string `json:"head_sha"`
	}
	_ = json.Unmarshal(run.Context, &ctxJSON)
	return ctxJSON.HeadSHA, true
}

// recordReviewActivity leaves a review-supersession event where people read the
// issue.
func (h *Handler) recordReviewActivity(ctx context.Context, issueID pgtype.UUID, actorType, actorID, action string, fields map[string]string) {
	issue, err := h.Queries.GetIssue(ctx, issueID)
	if err != nil {
		return
	}
	details, _ := json.Marshal(fields)
	activity, err := h.Queries.CreateActivity(ctx, db.CreateActivityParams{
		ID:          dbid.NewV7(),
		WorkspaceID: issue.WorkspaceID,
		IssueID:     issue.ID,
		ActorType:   strToText(actorType),
		ActorID:     optionalActorUUID(actorID),
		Action:      action,
		Details:     details,
	})
	if err != nil {
		slog.Warn("record review activity failed", "action", action, "err", err, "issue_id", uuidToString(issueID))
		return
	}
	h.publish(protocol.EventActivityCreated, uuidToString(issue.WorkspaceID), actorType, actorID, map[string]any{
		"issue_id": uuidToString(issue.ID),
		"entry": map[string]any{
			"type": "activity", "id": uuidToString(activity.ID), "action": activity.Action,
			"actor_type": actorType, "actor_id": actorID, "details": json.RawMessage(details),
			"created_at": timestampToString(activity.CreatedAt),
		},
	})
}
