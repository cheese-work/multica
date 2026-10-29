package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Failure reasons of a run stopped because its PR merged or closed. Like
// FailureReasonSupersededHead they arm the stop gate until the daemon acks.
const (
	FailureReasonPRMerged = "pr_merged"
	FailureReasonPRClosed = "pr_closed"
)

// PRLifecycleEvent is one lifecycle change of a PR that a live run must hear
// about or stop for. Kind is the issue_pr_event kind.
type PRLifecycleEvent struct {
	PRID                pgtype.UUID
	RepoOwner, RepoName string
	Number              int32
	Kind                string
}

// ApplyPRLifecycleEvent runs after the event is stored (CHE-2298 c1), for every
// issue linked to the PR. merged and closed stop the work that only serves the
// PR; draft and ready are notices to a live run and never start one. Safe to
// repeat: a webhook redelivery finds nothing left to stop or deliver.
func (s *TaskService) ApplyPRLifecycleEvent(ctx context.Context, ev PRLifecycleEvent) error {
	issueIDs, err := s.Queries.ListIssueIDsForPR(ctx, ev.PRID)
	if err != nil {
		return fmt.Errorf("list issues of pull request: %w", err)
	}
	for _, issueID := range issueIDs {
		switch ev.Kind {
		case "merged", "closed":
			err = s.stopPROnlyWork(ctx, issueID, ev)
		case "draft", "ready":
			err = s.noticePRLifecycle(ctx, issueID, ev)
		}
		if err != nil {
			return fmt.Errorf("issue %s: %w", util.UUIDToString(issueID), err)
		}
	}
	return nil
}

// stopPROnlyWork cancels the issue's unfinished runs that exist only for this
// PR and supersedes its undelivered events. Runs that serve the issue beyond the
// PR, and the PR's own pr.merged wake, are untouched. Cancel keeps the daemon's
// drain and cleanup path: the run still gets its cancel-ack, which opens the gate.
func (s *TaskService) stopPROnlyWork(ctx context.Context, issueID pgtype.UUID, ev PRLifecycleEvent) error {
	reason := FailureReasonPRClosed
	if ev.Kind == "merged" {
		reason = FailureReasonPRMerged
	}
	if _, err := s.Queries.SupersedeOpenPREventsForPR(ctx, db.SupersedeOpenPREventsForPRParams{IssueID: issueID, PullRequestID: ev.PRID}); err != nil {
		return fmt.Errorf("supersede open pr events: %w", err)
	}
	prefix := strings.ToLower(fmt.Sprintf("github:%s/%s#%d", ev.RepoOwner, ev.RepoName, ev.Number))
	taskIDs, err := s.Queries.ListPROnlyRunIDs(ctx, db.ListPROnlyRunIDsParams{IssueID: issueID, KeyPrefix: prefix})
	if err != nil {
		return fmt.Errorf("list pr-only runs: %w", err)
	}
	for _, taskID := range taskIDs {
		if _, err := s.CancelTaskWithReason(ctx, taskID, "pull request "+ev.Kind, reason); err != nil {
			return fmt.Errorf("cancel run %s: %w", util.UUIDToString(taskID), err)
		}
	}
	return nil
}

// NoticePRSnapshotEvents runs after a snapshot is applied (CHE-2436): it hands
// the conflict and checks-failed events the snapshot stored to live runs of the
// PR's issues. Safe to repeat: an event already taken is no longer accepted.
func (s *TaskService) NoticePRSnapshotEvents(ctx context.Context, prID pgtype.UUID) error {
	issueIDs, err := s.Queries.ListIssueIDsForPR(ctx, prID)
	if err != nil {
		return fmt.Errorf("list issues of pull request: %w", err)
	}
	for _, issueID := range issueIDs {
		if err := s.noticePRLifecycle(ctx, issueID, PRLifecycleEvent{PRID: prID}); err != nil {
			return fmt.Errorf("issue %s: %w", util.UUIDToString(issueID), err)
		}
	}
	return nil
}

// noticePRLifecycle hands the issue's accepted notice events to a live run
// that negotiated typed PR-event delivery. With no such run the event stays
// accepted for the next-turn path; it never starts a run.
func (s *TaskService) noticePRLifecycle(ctx context.Context, issueID pgtype.UUID, ev PRLifecycleEvent) error {
	eventIDs, err := s.Queries.ListAcceptedPRNoticeEvents(ctx, db.ListAcceptedPRNoticeEventsParams{IssueID: issueID, PullRequestID: ev.PRID})
	if err != nil || len(eventIDs) == 0 {
		return err
	}
	taskIDs, err := s.Queries.ListRunningPREventTaskIDs(ctx, issueID)
	if err != nil || len(taskIDs) == 0 {
		return err
	}
	// One live run takes the notice; a second run would repeat it.
	for _, eventID := range eventIDs {
		if _, err := s.Queries.EnqueueTaskPREventDelivery(ctx, db.EnqueueTaskPREventDeliveryParams{TaskID: taskIDs[0], EventID: eventID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	return nil
}
