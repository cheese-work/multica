package ghsnapshot

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// PREventKind is one captured kind of PR event.
type PREventKind string

const (
	PREventConflict     PREventKind = "conflict"
	PREventHeadChanged  PREventKind = "head_changed"
	PREventChecksFailed PREventKind = "checks_failed"
	PREventMerged       PREventKind = "merged"
	PREventClosed       PREventKind = "closed"
	PREventDraft        PREventKind = "draft"
	PREventReady        PREventKind = "ready"
)

// PREventOutcome reports what CapturePREvent did with one event.
type PREventOutcome string

const (
	PREventNotCaptured PREventOutcome = "not_captured"
	PREventAccepted    PREventOutcome = "accepted"
	PREventSuperseded  PREventOutcome = "superseded"
	PREventDropped     PREventOutcome = "dropped"
	PREventDuplicate   PREventOutcome = "duplicate"
)

// PREventInput is one observed PR event, before it fans out to linked issues.
type PREventInput struct {
	PRID       pgtype.UUID
	Kind       PREventKind
	HeadSHA    string
	BaseSHA    string
	ObservedAt time.Time
}

// CapturePREvent stores in once for every issue linked to the PR and returns
// one outcome per linked issue. Pass queries bound to a transaction: the PR row
// lock that serializes capture per PR is held until that transaction ends.
//
// Ordering: an event for a head other than the PR's current head, or observed
// before the latest stored event of that (issue, PR, head), is stored as superseded
// and never delivered. Checks for a non-current head are dropped outright.
func CapturePREvent(ctx context.Context, q *db.Queries, in PREventInput) ([]PREventOutcome, error) {
	pr, err := q.LockGitHubPRForEvent(ctx, in.PRID)
	if err != nil {
		return nil, err
	}
	issueIDs, err := q.ListIssueIDsForPR(ctx, in.PRID)
	if err != nil {
		return nil, err
	}
	if len(issueIDs) == 0 {
		return nil, nil
	}
	enabled, err := q.PREventKindEnabled(ctx, db.PREventKindEnabledParams{ID: pr.WorkspaceID, Kind: string(in.Kind)})
	if err != nil {
		return nil, err
	}
	staleHead := in.HeadSHA != pr.HeadSha
	outcomes := make([]PREventOutcome, 0, len(issueIDs))
	for _, issueID := range issueIDs {
		outcome, err := capturePREventForIssue(ctx, q, pr, issueID, in, enabled, staleHead)
		if err != nil {
			return nil, err
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, nil
}

func capturePREventForIssue(ctx context.Context, q *db.Queries, pr db.LockGitHubPRForEventRow, issueID pgtype.UUID, in PREventInput, enabled, staleHead bool) (PREventOutcome, error) {
	if !enabled {
		return PREventNotCaptured, nil
	}
	if staleHead && in.Kind == PREventChecksFailed {
		return PREventDropped, nil
	}
	if _, err := q.LockIssueForPREvent(ctx, issueID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PREventNotCaptured, nil
		}
		return "", err
	}
	status, outcome := "accepted", PREventAccepted
	superseded := staleHead
	if !superseded {
		latest, err := q.GetLatestIssuePREventObservedAt(ctx, db.GetLatestIssuePREventObservedAtParams{IssueID: issueID, PullRequestID: in.PRID, HeadSha: in.HeadSHA})
		if err != nil {
			return "", err
		}
		superseded = latest.Valid && in.ObservedAt.Before(latest.Time)
	}
	if superseded {
		status, outcome = "superseded", PREventSuperseded
	}
	n, err := q.InsertIssuePREvent(ctx, db.InsertIssuePREventParams{
		WorkspaceID:   pr.WorkspaceID,
		IssueID:       issueID,
		PullRequestID: in.PRID,
		Kind:          string(in.Kind),
		HeadSha:       in.HeadSHA,
		BaseSha:       in.BaseSHA,
		ObservedAt:    tsFromTime(in.ObservedAt),
		DedupeKey:     in.dedupeKey(),
		Status:        status,
	})
	if err != nil {
		return "", err
	}
	if n == 0 {
		return PREventDuplicate, nil
	}
	return outcome, nil
}

// dedupeKey names what makes two observations the same event. A conflict is a
// function of head and base, so a moved base is a new event; a draft or ready
// flip can recur on one head, so it is keyed by the observation time that a
// webhook redelivery repeats.
func (in PREventInput) dedupeKey() string {
	switch in.Kind {
	case PREventConflict:
		return string(in.Kind) + "|" + in.HeadSHA + "|" + in.BaseSHA
	case PREventDraft, PREventReady, PREventClosed:
		return string(in.Kind) + "|" + in.HeadSHA + "|" + strconv.FormatInt(in.ObservedAt.UnixNano(), 10)
	default:
		return string(in.Kind) + "|" + in.HeadSHA
	}
}

// snapshotPREvents returns the events a freshly applied snapshot raises, given
// the verdicts stored before it overwrote them.
func snapshotPREvents(prev db.GetGitHubPRSnapshotStateRow, snap *PRSnapshot, observed time.Time) []PREventInput {
	sameHead := prev.SnapshotHeadSha == snap.HeadSHA
	var events []PREventInput
	if snap.Mergeable == "CONFLICTING" && !(sameHead && prev.ApiMergeable.String == "CONFLICTING") {
		events = append(events, PREventInput{Kind: PREventConflict, HeadSHA: snap.HeadSHA, BaseSHA: snap.BaseSHA, ObservedAt: observed})
	}
	if snap.HasChecks && snap.RollupState == "FAILURE" && !(sameHead && prev.ChecksRollupState.String == "FAILURE") {
		events = append(events, PREventInput{Kind: PREventChecksFailed, HeadSHA: snap.HeadSHA, BaseSHA: snap.BaseSHA, ObservedAt: observed})
	}
	return events
}
