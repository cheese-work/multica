package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// deferralExpiredNote is the reason a run carries when it starts because the
// 24 h deferral ran out, not because the head became admissible.
const deferralExpiredNote = "deferral expired"

// admissionRecovery is the evidence that a deferral ran out: the issue, agent
// and head it was recorded for, and its deadline. It is a claim made before the
// final enqueue, so guardReviewAdmission checks it against the PR head it reads
// there; evidence for another head is ignored and the run is judged as usual.
type admissionRecovery struct {
	IssueID, AgentID pgtype.UUID
	Head             string
	ExpiresAt        time.Time
}

type admissionRecoveryKey struct{}

func withAdmissionRecovery(ctx context.Context, r admissionRecovery) context.Context {
	return context.WithValue(ctx, admissionRecoveryKey{}, r)
}

// expiredFor reports whether the evidence names this issue, agent and head and
// its deadline has passed.
func (r admissionRecovery) expiredFor(issueID, agentID pgtype.UUID, head string) bool {
	return r.Head != "" && r.IssueID == issueID && r.AgentID == agentID && r.Head == head && !r.ExpiresAt.After(time.Now())
}

func admissionRecoveryOf(ctx context.Context) admissionRecovery {
	r, _ := ctx.Value(admissionRecoveryKey{}).(admissionRecovery)
	return r
}

// releaseAdmissionDeferrals starts the deferred reviewer runs whose PR head is
// admissible now, and the ones whose 24 h ran out (CHE-4651). issueIDs limits
// it to those issues (a GitHub event for their PR); nil takes every record (the
// fallback tick).
//
// The enqueue judges the head again, so nothing here decides admissibility:
// a head that is still not admissible is deferred again by the guard, which
// replaces the record when the head changed. A record whose PR is no longer
// open is dropped. A started run, or one already pending for the same issue,
// agent and head (ErrDuplicatePendingTask), ends the record.
//
// A run starts under the actor, trigger comment and taint its deferral
// recorded, so a released run is never cleaner or more privileged than the one
// that was deferred.
func releaseAdmissionDeferrals(ctx context.Context, q *db.Queries, issueIDs []pgtype.UUID,
	enqueue func(ctx context.Context, issue db.Issue, agentID, actor, comment pgtype.UUID, note string) error) error {
	records, err := q.ListReviewAdmissionDeferrals(ctx, issueIDs)
	if err != nil {
		return fmt.Errorf("admission release: list: %w", err)
	}
	var errs []error
	for _, r := range records {
		if err := releaseAdmissionDeferral(ctx, q, r, enqueue); err != nil {
			errs = append(errs, fmt.Errorf("admission release %s: %w", util.UUIDToString(r.IssueID), err))
		}
	}
	return errors.Join(errs...)
}

func releaseAdmissionDeferral(ctx context.Context, q *db.Queries, r db.ListReviewAdmissionDeferralsRow,
	enqueue func(ctx context.Context, issue db.Issue, agentID, actor, comment pgtype.UUID, note string) error) error {
	drop := func() error {
		return q.DeleteReviewAdmissionDeferral(ctx, db.DeleteReviewAdmissionDeferralParams{IssueID: r.IssueID, AgentID: r.AgentID, HeadSha: r.HeadSha, ExpiresAt: r.ExpiresAt})
	}
	pr, err := q.GetIssueAdmissionPR(ctx, r.IssueID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return drop()
		}
		return fmt.Errorf("load PR: %w", err)
	}
	// The dispatch halt (CHE-2220) holds the record, not drops it: a failed
	// read holds too, because a run that cannot be shown allowed must not start.
	halted, err := q.GitHubDispatchHalted(ctx, db.GitHubDispatchHaltedParams{WorkspaceID: pr.WorkspaceID, RepoOwner: pr.RepoOwner, RepoName: pr.RepoName})
	if err != nil {
		return fmt.Errorf("read dispatch halt: %w", err)
	}
	if halted {
		return nil
	}
	issue, err := q.GetIssue(ctx, r.IssueID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return drop()
		}
		return fmt.Errorf("load issue: %w", err)
	}
	// A record belongs to the run it deferred. A handoff or wakeup record is
	// the issue's own run for its assignee: once the issue left that agent, or
	// closed, the run is stale and must not start. A mention record does not
	// depend on the assignee, only on the issue still being open.
	if issueClosedForRelease(ctx, q, issue) || (r.Trigger != deferralTriggerMention && issue.AssigneeID != r.AgentID) {
		return drop()
	}
	ctx = withAdmissionRecovery(ctx, admissionRecovery{IssueID: r.IssueID, AgentID: r.AgentID, Head: r.HeadSha, ExpiresAt: r.ExpiresAt.Time})
	err = enqueue(withStageTaint(ctx, r.TaintIds), issue, r.AgentID, r.ActorUserID, r.TriggerCommentID, "")
	switch {
	case err == nil || errors.Is(err, ErrDuplicatePendingTask):
		slog.Info("admission release: run started", "issue_id", util.UUIDToString(r.IssueID), "agent_id", util.UUIDToString(r.AgentID), "head", r.HeadSha)
		return drop()
	case errors.Is(err, ErrAdmissionDeferred):
		return nil // still not admissible; the guard kept or replaced the record
	case errors.Is(err, ErrStageQueueFull):
		// Admitted, but the stage is full: its waiting record now carries the
		// request, and ReleaseStageQueues starts it when a slot frees. This
		// record stays: it holds the deadline, so an expired deferral still
		// starts with its reason when CI is red at that point. The next tick
		// ends it once the run has started.
		slog.Info("admission release: admitted, queued behind a full stage", "issue_id", util.UUIDToString(r.IssueID), "agent_id", util.UUIDToString(r.AgentID), "head", r.HeadSha)
		return nil
	case errors.Is(err, ErrReviewHeadAnswered), errors.Is(err, ErrIssueInTriage):
		return drop() // never clears for this record: the agent answered the head, or the issue is in triage
	case errors.Is(err, ErrRunBudgetExhausted):
		// A person can raise the budget, so the record waits for its deadline
		// and the tick does not fail meanwhile.
		slog.Info("admission release: run budget used up", "issue_id", util.UUIDToString(r.IssueID), "agent_id", util.UUIDToString(r.AgentID))
		if r.ExpiresAt.Time.After(time.Now()) {
			return nil
		}
		return drop()
	}
	// Any other refusal is reported. A record past its deadline ends anyway, so
	// one stuck record (an archived agent) cannot fail every tick until the PR closes.
	if !r.ExpiresAt.Time.After(time.Now()) {
		if dropErr := drop(); dropErr != nil {
			return errors.Join(err, dropErr)
		}
	}
	return err
}

// deferralTriggerMention is the trigger a mention run records (see
// enqueueMentionTask); every other trigger is the issue's own run.
const deferralTriggerMention = "mention"

// issueClosedForRelease reports whether the issue is in a done or closed
// status category, where a deferred run no longer starts. A failed catalog read
// reads as open: the release then judges the run as before.
func issueClosedForRelease(ctx context.Context, q *db.Queries, issue db.Issue) bool {
	category, _ := issuestatus.CategoryAndName(ctx, q, issue.WorkspaceID, issue.Status)
	return category == issuestatus.CategoryDone || category == issuestatus.CategoryClosed
}

// ReleaseAdmissionDeferrals is the entry for the GitHub event (issueIDs of the
// PR) and the scheduler's fallback tick (nil).
func (s *TaskService) ReleaseAdmissionDeferrals(ctx context.Context, issueIDs []pgtype.UUID) error {
	return releaseAdmissionDeferrals(ctx, s.Queries, issueIDs, func(ctx context.Context, issue db.Issue, agentID, actor, comment pgtype.UUID, note string) error {
		var err error
		if issue.AssigneeID == agentID {
			_, err = s.enqueueIssueTask(ctx, issue, comment, false, note, actor, pgtype.UUID{}, pgtype.Timestamptz{}, OriginDerived)
		} else {
			_, err = s.enqueueMentionTask(ctx, issue, agentID, comment, false, pgtype.UUID{}, false, note, actor, pgtype.UUID{}, OriginDerived)
		}
		return err
	})
}
