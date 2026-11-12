package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/stagequeue"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// stageLimits is N per stage (CHE-3611), read once at start from the env.
var stageLimits = stagequeue.LimitsFromEnv(os.Getenv)

type stageTaintKey struct{}

// withStageTaint makes every task inserted under ctx carry taint, on top of the
// taint of any acting task (createAgentTask).
func withStageTaint(ctx context.Context, taint []string) context.Context {
	return context.WithValue(ctx, stageTaintKey{}, taint)
}

func stageTaint(ctx context.Context) []string {
	taint, _ := ctx.Value(stageTaintKey{}).([]string)
	return taint
}

// stageEntries lists the PRs in one owner's stage as queue entries: those that
// hold a slot, those refused and still waiting, and the candidate being admitted.
func stageEntries(ctx context.Context, q *db.Queries, stage stagequeue.Stage, agentID, candidate pgtype.UUID) ([]stagequeue.Entry, error) {
	type row struct {
		run, pr pgtype.UUID
		gates   int32
		created pgtype.Timestamptz
		active  bool
	}
	var rows []row
	if stage == stagequeue.StageReview {
		rs, err := q.ListReviewStageEntries(ctx, db.ListReviewStageEntriesParams{AgentID: agentID, CandidateID: candidate})
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			rows = append(rows, row{r.RunIssueID, r.PrKey, r.OpenGates, r.CreatedAt, r.Active})
		}
	} else {
		rs, err := q.ListGateStageEntries(ctx, db.ListGateStageEntriesParams{AgentID: agentID, Stage: string(stage), CandidateID: candidate})
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			rows = append(rows, row{r.RunIssueID, r.PrKey, r.OpenGates, r.CreatedAt, r.Active})
		}
	}
	entries := make([]stagequeue.Entry, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, stagequeue.Entry{
			PR: util.UUIDToString(r.pr), OpenGates: int(r.gates), Created: r.created.Time,
			Active: r.active, Ref: util.UUIDToString(r.run),
		})
	}
	return entries, nil
}

// stageOf names the stage the agent's run on this issue belongs to, if any:
// review for a PR in review, device or browser for a lane's gate issue.
func stageOf(issue db.Issue, agentID pgtype.UUID) (stagequeue.Stage, bool) {
	if issue.AssigneeType.String != "agent" || issue.AssigneeID != agentID {
		return "", false
	}
	if IsReviewStatus(issue.Status) {
		return stagequeue.StageReview, true
	}
	return stagequeue.GateStage(issue.Title)
}

// guardStageQueue refuses a stage run beyond the stage's WIP limit (CHE-3611).
// It runs inside the enqueue transaction after the budget guard; the per-owner
// advisory lock makes the check-then-insert atomic. Authors and runs that are
// not review-stage work are never counted. A refused PR is not lost: the
// release job starts it when a slot frees.
func guardStageQueue(ctx context.Context, q *db.Queries, issueID pgtype.UUID, target RunTarget) error {
	if !target.ReviewRun || !target.AgentID.Valid || !issueID.Valid {
		return nil
	}
	issue, err := q.GetIssue(ctx, issueID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("stage queue: load issue: %w", err)
	}
	stage, ok := stageOf(issue, target.AgentID)
	if !ok {
		return nil
	}
	if err := q.LockStageQueue(ctx, db.LockStageQueueParams{Stage: string(stage), AgentID: target.AgentID}); err != nil {
		return fmt.Errorf("stage queue: lock: %w", err)
	}
	entries, err := stageEntries(ctx, q, stage, target.AgentID, issueID)
	if err != nil {
		return fmt.Errorf("stage queue: list: %w", err)
	}
	ref := util.UUIDToString(issueID)
	var pr string
	for _, e := range entries {
		if e.Ref == ref {
			pr = e.PR
			break
		}
	}
	if pr == "" {
		return nil // not a PR in this stage (no linked PR): nothing to cap
	}
	d := stagequeue.Plan(entries, stageLimits.For(stage))
	for _, e := range d.Admitted {
		if e.PR == pr {
			return nil
		}
	}
	// Fail closed: a requester whose taint cannot be read is never shown clean.
	taint, err := ActingTaskTaint(ctx, q)
	if err != nil {
		return fmt.Errorf("stage queue: read requester taint: %w", err)
	}
	refusal := &stageFullError{
		issueID: issueID, agentID: target.AgentID,
		actor: target.ActorUserID, comment: target.TriggerCommentID,
		// Taint carried in the context (e.g. restored from an admission deferral) survives the refusal.
		taint: append(append([]string(nil), taint...), stageTaint(ctx)...),
	}
	if err := markStageWaiting(ctx, q, refusal); err != nil {
		return err
	}
	slog.Info("stage queue: run refused, PR queued",
		"stage", stage, "agent_id", util.UUIDToString(target.AgentID), "issue_id", ref, "queued", len(d.Queued))
	return refusal
}

// stageFullError is ErrStageQueueFull for one (issue, agent): the pair whose
// refusal must be written down, with who asked (actor, trigger comment) and
// the taint the asker carried. errors.Is(err, ErrStageQueueFull) holds.
type stageFullError struct {
	issueID, agentID pgtype.UUID
	actor, comment   pgtype.UUID
	taint            []string
}

func (*stageFullError) Error() string        { return ErrStageQueueFull.Error() }
func (*stageFullError) Is(target error) bool { return target == ErrStageQueueFull }

// markStageWaiting records a refusal so the release job can start the PR later.
// A failure is returned: a refusal that is not on record is a lost run.
func markStageWaiting(ctx context.Context, q *db.Queries, r *stageFullError) error {
	taint := r.taint
	if taint == nil {
		taint = []string{}
	}
	if err := q.MarkStageWaiting(ctx, db.MarkStageWaitingParams{
		IssueID: r.issueID, AgentID: r.agentID, ActorUserID: r.actor, TriggerCommentID: r.comment, TaintIds: taint,
	}); err != nil {
		return fmt.Errorf("stage queue: record refusal: %w", err)
	}
	return nil
}

// recordStageRefusal runs after a transaction that ended in a stage refusal or
// an admission deferral has rolled back, taking the mark the guard wrote inside
// it. It writes the mark again on the pool. A write failure replaces the
// refusal, so the caller sees an error instead of a run that is neither started
// nor queued.
func (s *TaskService) recordStageRefusal(ctx context.Context, err error) error {
	var full *stageFullError
	var deferred *admissionDeferredError
	switch {
	case errors.As(err, &full):
		if merr := markStageWaiting(ctx, s.Queries, full); merr != nil {
			return merr
		}
	case errors.As(err, &deferred):
		if merr := markAdmissionDeferred(ctx, s.Queries, deferred); merr != nil {
			return merr
		}
	}
	return err
}

// releaseStageQueues starts the admitted PRs that have no active run, for every
// owner with a PR waiting. It is the only way a refused PR advances: a PR holds
// no slot while it waits, so there is nothing to hand over on completion.
//
// A PR starts under the attribution and taint its refusal recorded: enqueue
// gets the actor and trigger comment of the refused run, and ctx carries its
// taint (stageTaint), so a released run is never cleaner or more privileged
// than the one that was refused.
func releaseStageQueues(ctx context.Context, q *db.Queries, enqueue func(ctx context.Context, issue db.Issue, actor, comment pgtype.UUID) error) error {
	owners, err := q.ListWaitingStageOwners(ctx)
	if err != nil {
		return fmt.Errorf("stage queue release: list owners: %w", err)
	}
	var errs []error
	for _, o := range owners {
		stage := stagequeue.Stage(o.Stage)
		entries, err := stageEntries(ctx, q, stage, o.AgentID, pgtype.UUID{})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, e := range stagequeue.Plan(entries, stageLimits.For(stage)).Admitted {
			if e.Active {
				continue
			}
			issue, err := q.GetIssue(ctx, util.MustParseUUID(e.Ref))
			if err != nil {
				errs = append(errs, err)
				continue
			}
			// Fail closed: no readable refusal record means no run.
			w, err := q.GetStageWaiting(ctx, db.GetStageWaitingParams{IssueID: issue.ID, AgentID: o.AgentID})
			if err != nil {
				errs = append(errs, fmt.Errorf("stage queue release %s: read refusal: %w", e.Ref, err))
				continue
			}
			if err := enqueue(withStageTaint(ctx, w.TaintIds), issue, w.ActorUserID, w.TriggerCommentID); err != nil &&
				!errors.Is(err, ErrDuplicatePendingTask) && !errors.Is(err, ErrStageQueueFull) {
				errs = append(errs, fmt.Errorf("stage queue release %s: %w", e.Ref, err))
			}
		}
	}
	return errors.Join(errs...)
}

// ReleaseStageQueues is the scheduler's tick.
func (s *TaskService) ReleaseStageQueues(ctx context.Context) error {
	return releaseStageQueues(ctx, s.Queries, func(ctx context.Context, issue db.Issue, actor, comment pgtype.UUID) error {
		ctx, err := s.stageRecoveryContext(ctx, issue)
		if err != nil {
			return err
		}
		return s.enqueueStageRelease(ctx, issue, actor, comment)
	})
}

// stageRecoveryContext carries an admission deferral that expired while the
// run waited (CHE-4651), only for the head it was recorded for: a newer head is
// judged. The lookup is advisory; enqueueStageRelease validates it again.
func (s *TaskService) stageRecoveryContext(ctx context.Context, issue db.Issue) (context.Context, error) {
	pr, err := s.Queries.GetIssueAdmissionPR(ctx, issue.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ctx, nil
		}
		return ctx, fmt.Errorf("read admission PR: %w", err)
	}
	expires, err := s.Queries.GetReviewAdmissionDeferralExpiry(ctx, db.GetReviewAdmissionDeferralExpiryParams{IssueID: issue.ID, AgentID: issue.AssigneeID, HeadSha: pr.HeadSha})
	switch {
	case err == nil:
		return withAdmissionRecovery(ctx, admissionRecovery{IssueID: issue.ID, AgentID: issue.AssigneeID, Head: pr.HeadSha, ExpiresAt: expires.Time}), nil
	case !errors.Is(err, pgx.ErrNoRows):
		return ctx, fmt.Errorf("read admission deferral: %w", err)
	}
	return ctx, nil
}

func (s *TaskService) enqueueStageRelease(ctx context.Context, issue db.Issue, actor, comment pgtype.UUID) error {
	_, err := s.enqueueIssueTask(ctx, issue, comment, false, "", actor, pgtype.UUID{}, pgtype.Timestamptz{}, OriginDerived)
	return err
}

// StageQueueSnapshot is where one issue's stage run stands: the stage, the
// owner's limit, and the owner's admitted and queued PRs in dispatch order.
// State is "admitted", "queued" or "none" (the issue is not stage work).
type StageQueueSnapshot struct {
	Stage    string   `json:"stage,omitempty"`
	AgentID  string   `json:"agent_id,omitempty"`
	Limit    int      `json:"limit,omitempty"`
	State    string   `json:"state"`
	Position int      `json:"position"` // 0-based place in the queue; 0 when admitted
	Admitted []string `json:"admitted"` // run issue ids that hold or may take a slot
	Queued   []string `json:"queued"`   // run issue ids waiting, next first
}

// LoadStageQueueSnapshot reads the stage queue the issue's assignee owns. It
// writes nothing: a PR is marked queued only when a run is refused.
func LoadStageQueueSnapshot(ctx context.Context, q *db.Queries, issue db.Issue) (StageQueueSnapshot, error) {
	snap := StageQueueSnapshot{State: "none", Admitted: []string{}, Queued: []string{}}
	stage, ok := stageOf(issue, issue.AssigneeID)
	if !ok {
		return snap, nil
	}
	entries, err := stageEntries(ctx, q, stage, issue.AssigneeID, issue.ID)
	if err != nil {
		return snap, err
	}
	limit := stageLimits.For(stage)
	d := stagequeue.Plan(entries, limit)
	snap.Stage, snap.AgentID, snap.Limit = string(stage), util.UUIDToString(issue.AssigneeID), limit
	self := util.UUIDToString(issue.ID)
	for _, e := range d.Admitted {
		snap.Admitted = append(snap.Admitted, e.Ref)
		if e.Ref == self {
			snap.State = "admitted"
		}
	}
	for i, e := range d.Queued {
		snap.Queued = append(snap.Queued, e.Ref)
		if e.Ref == self {
			snap.State, snap.Position = "queued", i
		}
	}
	return snap, nil
}
