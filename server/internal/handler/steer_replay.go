package handler

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-5542: a member steer the turn never read is replayed as a follow-up run
// when the turn completes. When queueing that run fails for a transient reason,
// the receipt keeps the retry state (failure_reason, replay_attempts,
// replay_next_at), so a restart loses nothing and no request handler sleeps.
const (
	// ReplayQueuedReason marks a receipt whose follow-up run is being retried.
	ReplayQueuedReason = "replay_queued"
	// ReplayExhaustedReason marks a receipt whose retries all failed.
	ReplayExhaustedReason = "replay_exhausted"

	replayBaseDelay  = 5 * time.Second // 5, 10, 20, 40, 80 s
	replayMaxRetries = 5
	replayJitter     = 0.2 // +-20%
	// replayLease hides a claimed receipt from other instances while its retry
	// runs; a crash mid-retry is retried once the lease lapses.
	replayLease     = time.Minute
	replayBatchSize = 50
)

// replayRetryAt is when retry number `failures` (1-based: the count of failed
// attempts so far) is due, or ok=false once the retries ran out. jitter is in
// [-1, 1] and scales the +-20% spread, so tests inject it.
func replayRetryAt(now time.Time, failures int, jitter float64) (at time.Time, ok bool) {
	if failures < 1 || failures > replayMaxRetries {
		return time.Time{}, false
	}
	delay := replayBaseDelay << (failures - 1)
	return now.Add(time.Duration(float64(delay) * (1 + replayJitter*jitter))), true
}

// transientReplayReason: a failure that can clear by itself. Anything else
// that blocks the enqueue is a policy refusal (budget exhausted, agent paused,
// attribution blocked, ...), which a retry cannot change.
func transientReplayReason(reason DispatchReasonCode) bool {
	switch reason {
	case ReasonInternalError, ReasonStageQueueFull, ReasonAdmissionDeferred, ReasonAlreadyActive:
		return true
	}
	return false
}

// replayOutcome is what replaying one comment for the completed task's agent
// came to. routed=false means no trigger resolves to that agent.
type replayOutcome struct {
	routed  bool
	handled bool
	reason  DispatchReasonCode
}

// replayComment routes comment c to task's agent only (never a fan-out) and
// enqueues the follow-up. handled means a run now covers the comment. unread
// marks a steer the turn never read; its follow-up is deduplicated per
// (agent, comment) so two paths replaying it queue one run.
func (h *Handler) replayComment(ctx context.Context, issue db.Issue, task *db.AgentTaskQueue, c db.Comment, planned, unread bool) replayOutcome {
	agentID := uuidToString(task.AgentID)
	var parentComment *db.Comment
	if c.ParentID.Valid {
		// Scope to the issue's workspace; a comment's parent is always in the
		// same workspace, so this only fails closed against a stray foreign
		// UUID rather than changing behavior (MUL-4252).
		if parent, err := h.Queries.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{
			ID:          c.ParentID,
			WorkspaceID: issue.WorkspaceID,
		}); err == nil {
			parentComment = &parent
		}
	}
	// Compute what this comment would trigger, then keep ONLY the agent
	// that just completed — never the full fan-out (that would re-wake
	// unrelated `@other-agent` targets).
	//
	// The comment is routed under its OWN author_type. A member is its own
	// originator. For an agent author, the originator is the human at the
	// top of that agent's trigger chain (resolved from the comment's source
	// task); canInvokeAgent judges an agent→agent (A2A) mention by that
	// originator, not the immediate agent principal (MUL-3963).
	actorType := c.AuthorType
	actorID := uuidToString(c.AuthorID)
	originatorUserID := actorID
	if actorType != "member" {
		originatorUserID = uuidToString(h.TaskService.ResolveOriginatorFromTriggerComment(ctx, issue.WorkspaceID, c.ID))
	}
	triggers, _ := h.computeCommentAgentTriggers(ctx, issue, c.Content, parentComment, actorType, actorID, commentTriggerComputeOptions{
		ExcludeTriggerCommentID: c.ID,
		AuthoringTaskID:         c.SourceTaskID,
		OriginatorUserID:        originatorUserID,
	})
	// Agent replies discovered only by timestamp must not start a new
	// conversation. Replay explicit mentions, or a worker reply that the
	// creation path already accepted and recorded in this run's input plan.
	// Recomputed routing still checks current permissions and the self guard.
	if actorType != "member" {
		triggers = keepReplayableAgentTriggers(triggers, planned || unread)
	}
	scoped := make([]commentAgentTrigger, 0, 1)
	for _, trigger := range triggers {
		if uuidToString(trigger.Agent.ID) == agentID {
			scoped = append(scoped, trigger)
		}
	}
	if len(scoped) == 0 {
		return replayOutcome{}
	}
	if unread {
		if covered, err := h.Queries.CommentHasFollowUpRun(ctx, db.CommentHasFollowUpRunParams{
			IssueID: task.IssueID, AgentID: task.AgentID, TaskID: task.ID,
			Since: task.CreatedAt, CommentID: c.ID,
		}); err == nil && covered {
			return replayOutcome{routed: true, handled: true, reason: ReasonCoalesced}
		}
	}
	// The first qualifying comment enqueues the follow-up task; later ones
	// find it AlreadyPending and merge in, so all undelivered comments end
	// up covered by a single bounded run.
	//
	// A BLOCKED replay must not be discarded (#5914, Elon round 8): the slot
	// can be held by a task that will never see this comment (it predates that
	// task and is not in its planned ids), so dropping the failure here is
	// exactly how a promised follow-up is lost. Hand the obligation to that
	// blocker instead, keeping it alive until some run provably covers it.
	res := h.enqueueCommentAgentTriggers(ctx, issue, c.ID, scoped)[agentID]
	out := replayOutcome{routed: true, handled: res.status != DispatchBlocked, reason: res.reason}
	if res.status == DispatchBlocked {
		headSha := h.TaskService.ResolveIssueReviewSHAParam(ctx, task.IssueID)
		if h.propagateUncoveredCommentObligation(ctx, issue, scoped[0], c.ID, headSha) {
			out.handled = true
			slog.Info("reconcile comments on completion: replay blocked, obligation handed to the active task",
				"issue_id", uuidToString(task.IssueID), "agent_id", agentID, "comment_id", uuidToString(c.ID))
		} else {
			// The slot is held by a DIFFERENT-head queued task: it can neither
			// cover this comment nor accept it (merging would let an old-head
			// run consume a new-head request — TEN-356). A member steer keeps
			// its receipt and retries (settleReplayedSteer); anything else is
			// surfaced loudly rather than let it disappear quietly.
			slog.Error("reconcile comments on completion: replay blocked and obligation could not be handed off",
				"issue_id", uuidToString(task.IssueID), "agent_id", agentID, "comment_id", uuidToString(c.ID),
				"reason", res.reason)
		}
	}
	return out
}

// settleReplayedSteer updates the receipt of a replayed unread steer. failures
// is how many replays failed before this one. A member's covered steer keeps
// its receipt as "queued" with no retry due; a transient failure schedules the
// next retry; a refusal shows its reason. An agent's steer keeps the old
// policy (CHE-2151): covered drops the receipt, otherwise it is left alone.
func (h *Handler) settleReplayedSteer(ctx context.Context, workspaceID pgtype.UUID, taskID pgtype.UUID, c db.Comment, out replayOutcome, failures int, now time.Time) {
	if out.handled && c.AuthorType != "member" {
		if n, err := h.Queries.DeleteUndeliveredTaskSupplementForRun(ctx, db.DeleteUndeliveredTaskSupplementForRunParams{
			CommentID: c.ID, TaskID: taskID,
		}); err == nil && n > 0 {
			h.publishCommentSupplementUpdate(ctx, workspaceID, c.ID)
		}
		return
	}
	if c.AuthorType != "member" {
		return
	}
	// A covered steer stays "queued" with no retry due: the receipt reads
	// "will read it in the next run" until that run exists and reads it.
	reason, attempts, next := ReplayQueuedReason, int32(failures), pgtype.Timestamptz{}
	if !out.handled {
		reason = string(out.reason)
	}
	if !out.handled && transientReplayReason(out.reason) {
		reason = ReplayExhaustedReason
		if at, ok := replayRetryAt(now, failures+1, rand.Float64()*2-1); ok {
			reason, attempts, next = ReplayQueuedReason, int32(failures+1), pgtype.Timestamptz{Time: at, Valid: true}
		}
	}
	if n, err := h.Queries.SetSupplementReplay(ctx, db.SetSupplementReplayParams{
		CommentID: c.ID, TaskID: taskID, FailureReason: pgtype.Text{String: reason, Valid: true},
		ReplayAttempts: attempts, NextAt: next,
	}); err != nil {
		slog.Warn("steer replay: record state failed", "comment_id", uuidToString(c.ID), "error", err)
	} else if n > 0 {
		h.publishCommentSupplementUpdate(ctx, workspaceID, c.ID)
	}
}

// SweepSteerReplays retries every due steer replay once. Each receipt is leased
// first, so it is retried by one instance at a time.
func (h *Handler) SweepSteerReplays(ctx context.Context, now time.Time) {
	rows, err := h.Queries.ClaimDueSupplementReplays(ctx, db.ClaimDueSupplementReplaysParams{
		LeaseUntil: pgtype.Timestamptz{Time: now.Add(replayLease), Valid: true},
		Now:        pgtype.Timestamptz{Time: now, Valid: true},
		BatchSize:  replayBatchSize,
	})
	if err != nil {
		slog.Warn("steer replay sweep: claim failed", "error", err)
		return
	}
	for _, row := range rows {
		h.retrySteerReplay(ctx, row, now)
	}
}

func (h *Handler) retrySteerReplay(ctx context.Context, row db.TaskSupplement, now time.Time) {
	task, taskErr := h.Queries.GetAgentTask(ctx, row.TaskID)
	issue, issueErr := h.Queries.GetIssue(ctx, row.IssueID)
	c, commentErr := h.Queries.GetComment(ctx, row.CommentID)
	if taskErr != nil || issueErr != nil || commentErr != nil {
		slog.Warn("steer replay: load failed; lease will lapse and retry",
			"comment_id", uuidToString(row.CommentID), "task", taskErr, "issue", issueErr, "comment", commentErr)
		return
	}
	out := replayOutcome{handled: c.DeletedAt.Valid}
	if !out.handled {
		out = h.replayComment(ctx, issue, &task, c, false, true)
		if !out.routed {
			// The agent can no longer be addressed (archived, permissions).
			out.reason = ReasonTargetUnavailable
		}
	}
	h.settleReplayedSteer(ctx, issue.WorkspaceID, task.ID, c, out, int(row.ReplayAttempts), now)
}

// RunSteerReplaySweeper retries due steer replays until ctx ends.
func (h *Handler) RunSteerReplaySweeper(ctx context.Context) {
	ticker := time.NewTicker(replayBaseDelay)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.SweepSteerReplays(ctx, time.Now())
		}
	}
}
