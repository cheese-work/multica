package handler

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Resume (CHE-2798 P2r). A budget approval links the refusal it answers
// (linkRunBudgetRefusal, inside the approval's transaction). The worker then
// resumes it once, in one transaction: the status is restored when the hook had
// blocked the issue, the run is admitted, and the refusal ends run_queued or
// status_restored. Anything the worker refuses rolls all of that back; the
// outcome is then written on the refusal row by a second statement.

// runBudgetResumeAt runs inside the worker's transaction after its work and
// before the commit. A test returns true to stop there, as a crash would.
var runBudgetResumeAt = func(stage string) bool { return false }

var (
	errResumeDone    = errors.New("run budget: the refusal is not waiting for a resume")
	errResumeCrashed = errors.New("run budget: resume stopped before commit")
)

// resumeSuperseded ends the refusal: it no longer applies.
type resumeSuperseded struct{ reason string }

func (e *resumeSuperseded) Error() string { return "run budget: resume superseded: " + e.reason }

// resumeRefused means admission refused the run for a reason that is not a
// duplicate: the budget is still spent, or another gate said no.
type resumeRefused struct{ cause error }

func (e *resumeRefused) Error() string { return "run budget: resume refused: " + e.cause.Error() }
func (e *resumeRefused) Unwrap() error { return e.cause }

// linkRunBudgetRefusal is the Link step: the approval claims the refusal it
// answers, under the issue lock it already holds. A refusal that is superseded,
// already linked or already resumed stays as it is; a refusal of another issue
// and an approval without a refusal link nothing.
func linkRunBudgetRefusal(ctx context.Context, q *db.Queries, issueID pgtype.UUID, refusalID string, actionID pgtype.UUID, window string) error {
	id, err := util.ParseUUID(refusalID)
	if err != nil {
		return nil
	}
	_, err = q.LinkRunBudgetRefusal(ctx, db.LinkRunBudgetRefusalParams{ActionID: actionID, ResumeWindowID: window, ID: id, IssueID: issueID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

// resumeIneligible reports why a linked refusal no longer applies, or "". The
// issue must still be unfinished, in the window the approval paid for, and in
// the status occurrence the refusal was recorded in.
func resumeIneligible(ctx context.Context, q *db.Queries, row db.RunBudgetRefusal) (string, error) {
	issue, moved, err := refusalMoved(ctx, q, row)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "issue_closed", nil
	case err != nil:
		return "", err
	case service.IsFinishedStatus(ctx, q, issue.WorkspaceID, issue.Status):
		return "issue_closed", nil
	case moved != "":
		return moved, nil
	}
	window, err := service.LockRunBudgetWindow(ctx, q, row.IssueID)
	if err != nil {
		return "", err
	}
	if window != row.ResumeWindowID.String {
		return "window_changed", nil
	}
	return "", nil
}

// lockResume takes the locks in lock order E23 (the issue, then the refusal)
// and checks the refusal is still waiting and eligible.
func lockResume(ctx context.Context, q *db.Queries, id, issueID pgtype.UUID) error {
	if _, err := q.LockIssueRunBudget(ctx, issueID); err != nil {
		return err
	}
	row, err := q.LockRunBudgetRefusal(ctx, id)
	if err != nil {
		return err
	}
	if row.State != "resume_pending" {
		return errResumeDone
	}
	reason, err := resumeIneligible(ctx, q, row)
	if err != nil {
		return err
	}
	if reason != "" {
		return &resumeSuperseded{reason: reason}
	}
	return nil
}

// isRunRefusal reports whether err is an admission refusal, as opposed to a failure.
func isRunRefusal(err error) bool {
	return errors.Is(err, service.ErrRunBudgetExhausted) || errors.Is(err, service.ErrStageQueueFull) ||
		errors.Is(err, service.ErrAdmissionDeferred) || errors.Is(err, service.ErrIssueInTriage) ||
		errors.Is(err, service.ErrRecoveryCaused) || errors.Is(err, errSquadLeaderNotAdmitted)
}

// finishResume is the worker's terminal write, in the worker's transaction.
func finishResume(ctx context.Context, q *db.Queries, row db.RunBudgetRefusal, direct bool) error {
	task, err := q.NewestRunBudgetTaskSince(ctx, row.IssueID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	state := "run_queued"
	if !task.Valid && !direct {
		state = "status_restored"
	}
	if err := q.FinishRunBudgetRefusalResume(ctx, db.FinishRunBudgetRefusalResumeParams{ID: row.ID, State: state, TaskID: task}); err != nil {
		return err
	}
	if runBudgetResumeAt("before_commit") {
		return errResumeCrashed
	}
	return nil
}

// resumeRefusal resumes one linked refusal. It is safe to call twice: the
// second call finds the refusal no longer waiting.
func (h *Handler) resumeRefusal(ctx context.Context, id pgtype.UUID) {
	// A refused attempt rolls back, so the exhaustion it would report never happened.
	ctx = service.WithoutRunBudgetHook(ctx)
	for try := 0; try < runBudgetBlockTries; try++ {
		row, err := h.Queries.GetRunBudgetRefusal(ctx, id)
		if err != nil || row.State != "resume_pending" {
			return
		}
		after, err := h.resumeOnce(ctx, row)
		var superseded *resumeSuperseded
		var refused *resumeRefused
		switch {
		case err == nil:
			if after != nil {
				after(ctx)
			}
			return
		case errors.Is(err, errResumeDone):
			return
		case errors.As(err, &superseded):
			h.failResume(ctx, row, "superseded", superseded.reason)
			return
		case errors.As(err, &refused):
			h.refuseResume(ctx, row, refused.cause)
			return
		case errors.Is(err, pgx.ErrNoRows):
			// The issue's revision moved under the write: read it again.
			continue
		default:
			// The row stays resume_pending; the sweep retries it.
			slog.Warn("run budget: resume failed", "err", err, "refusal_id", uuidToString(id))
			return
		}
	}
	slog.Warn("run budget: resume lost the revision race", "refusal_id", uuidToString(id))
}

// resumeOnce is the worker's transaction. It returns what runs after the commit.
func (h *Handler) resumeOnce(ctx context.Context, row db.RunBudgetRefusal) (func(context.Context), error) {
	// The same check the transaction repeats under the locks: a refusal that
	// already moved on needs no overlay and no workflow decision.
	if reason, err := resumeIneligible(ctx, h.Queries, row); err != nil {
		return nil, err
	} else if reason != "" {
		return nil, &resumeSuperseded{reason: reason}
	}
	issue, err := h.Queries.GetIssue(ctx, row.IssueID)
	if err != nil {
		return nil, err
	}
	if row.Blocked {
		return h.resumeBlocked(ctx, row, issue)
	}
	return h.resumeDirect(ctx, row, issue)
}

// resumeBlocked gives the status the hook hid back and admits the run that
// status owes, in the transaction of the move.
func (h *Handler) resumeBlocked(ctx context.Context, row db.RunBudgetRefusal, issue db.Issue) (func(context.Context), error) {
	to := row.Status.String
	overlay := h.prepareEdgeRunOverlay(ctx, issue, to, true)
	guard := func(ctx context.Context, q *db.Queries, _ *db.UpdateIssueParams) error {
		return lockResume(ctx, q, row.ID, issue.ID)
	}
	extra := map[string]any{"reason": "budget_approved", "action_id": uuidToString(row.ActionID)}
	var edge edgeMove
	err := h.withPgxTx(ctx, func(tx pgx.Tx) error {
		q := h.Queries.WithTx(tx)
		var err error
		edge, err = h.moveIssueOnEdgeTx(withIssueMoveGuard(ctx, guard), q, issue, to, "run_budget", extra, true, overlay)
		if err != nil {
			return err
		}
		if edge.RunRefused != nil && !errors.Is(edge.RunRefused, service.ErrDuplicatePendingTask) {
			return &resumeRefused{cause: edge.RunRefused}
		}
		return finishResume(ctx, q, row, false)
	})
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) { edge.AfterCommit(ctx) }, nil
}

// resumeDirect admits the run the budget refused, for an issue the hook never blocked.
func (h *Handler) resumeDirect(ctx context.Context, row db.RunBudgetRefusal, issue db.Issue) (func(context.Context), error) {
	var overlay *service.PreparedRuntimeMCPOverlay
	if issue.AssigneeType.String == "agent" {
		overlay = h.TaskService.PrepareIssueRunOverlay(ctx, issue, pgtype.UUID{})
	}
	var after func()
	err := h.withPgxTx(ctx, func(tx pgx.Tx) error {
		q := h.Queries.WithTx(tx)
		if err := lockResume(ctx, q, row.ID, issue.ID); err != nil {
			return err
		}
		// Read under the lock: the run is admitted for the issue as it is now.
		locked, err := q.GetIssue(ctx, issue.ID)
		if err != nil {
			return err
		}
		th := h.onQueries(q)
		switch locked.AssigneeType.String {
		case "agent":
			after, err = th.TaskService.EnqueueIssueRunInTx(ctx, q, locked, pgtype.UUID{}, overlay)
		case "squad":
			after, err = th.enqueueSquadLeaderRunInTx(ctx, q, locked)
		default:
			return &resumeSuperseded{reason: "assignee_changed"}
		}
		switch {
		case err == nil, errors.Is(err, service.ErrDuplicatePendingTask):
		case isRunRefusal(err):
			return &resumeRefused{cause: err}
		default:
			return err
		}
		return finishResume(ctx, q, row, true)
	})
	if err != nil {
		return nil, err
	}
	if after == nil {
		return nil, nil
	}
	return func(context.Context) { after() }, nil
}

// refuseResume records why a rolled-back resume was refused: still_blocked
// while the budget refuses (a later approval links it again), else the restore
// itself was refused and the refusal ends.
func (h *Handler) refuseResume(ctx context.Context, row db.RunBudgetRefusal, cause error) {
	if !errors.Is(cause, service.ErrRunBudgetExhausted) {
		h.failResume(ctx, row, "superseded", "restore_refused")
		return
	}
	kind, err := service.RunBudgetRefusalKind(ctx, h.Queries, row.IssueID)
	if err != nil {
		slog.Warn("run budget: read refusal kind failed", "err", err, "refusal_id", uuidToString(row.ID))
		return
	}
	h.failResume(ctx, row, "still_blocked", kind)
}

func (h *Handler) failResume(ctx context.Context, row db.RunBudgetRefusal, state, reason string) {
	if err := h.Queries.FailRunBudgetRefusalResume(ctx, db.FailRunBudgetRefusalResumeParams{ID: row.ID, State: state, Reason: reason}); err != nil {
		slog.Warn("run budget: record resume outcome failed", "err", err, "refusal_id", uuidToString(row.ID))
	}
}

// notifyRunBudgetResume starts the resume of the refusal an approved action
// linked, once the approval committed. No linked refusal: nothing to do.
func (h *Handler) notifyRunBudgetResume(ctx context.Context, actionID pgtype.UUID) {
	row, err := h.Queries.GetRunBudgetRefusalByAction(ctx, actionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	if err != nil {
		slog.Warn("run budget: find linked refusal failed", "err", err, "action_id", uuidToString(actionID))
		return
	}
	h.resumeRefusal(ctx, row.ID)
}

// sweepRunBudgetResumes resumes the linked refusals a crashed or missed notify left behind.
func (h *Handler) sweepRunBudgetResumes(ctx context.Context, before time.Time) {
	ids, err := h.Queries.ListResumePendingRunBudgetRefusals(ctx, pgtype.Timestamptz{Time: before, Valid: true})
	if err != nil {
		slog.Warn("run budget: list pending resumes failed", "err", err)
		return
	}
	for _, id := range ids {
		h.resumeRefusal(ctx, id)
	}
}
