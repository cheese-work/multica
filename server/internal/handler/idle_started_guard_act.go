package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

const (
	idleGuardAttempts = 3

	idleGuardMoved    = "moved"
	idleGuardRefused  = "refused"
	idleGuardNotified = "notified"
	idleGuardResumed  = "resumed"
	idleGuardGaveUp   = "gave_up"

	idleGuardKindHumanWait      = "human_wait"
	idleGuardKindOpenRequest    = "open_request"
	idleGuardKindNoContinuation = "no_continuation"

	idleGuardNeedDecision = "need_decision"
)

// idleGuardReceipt is the issue.metadata.idle_guard episode record. It is the
// once-per-episode mark: the main scan skips an issue that carries one, and the
// episode pass removes it when the issue leaves the episode's status. MovedAt is
// empty while the move has not happened; From is where an unmoved episode sits.
type idleGuardReceipt struct {
	State     string `json:"state"`
	Kind      string `json:"kind"`
	From      string `json:"from"`
	To        string `json:"to"`
	IdleSince string `json:"idle_since"`
	MovedAt   string `json:"moved_at"`
	Attempts  int    `json:"attempts"`
	AlertID   string `json:"alert_id,omitempty"`
	CommentID string `json:"comment_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

// issueMoveGuard runs inside a status write's transaction, after the issue row
// is locked and before the write. A non-nil error stops the write.
type issueMoveGuard func(ctx context.Context, q *db.Queries, params *db.UpdateIssueParams) error

type issueMoveGuardKey struct{}

// withIssueMoveGuard arms guard for the next status write on ctx; it runs once.
func withIssueMoveGuard(ctx context.Context, guard issueMoveGuard) context.Context {
	var used atomic.Bool
	once := issueMoveGuard(func(ctx context.Context, q *db.Queries, params *db.UpdateIssueParams) error {
		if !used.CompareAndSwap(false, true) {
			return nil
		}
		return guard(ctx, q, params)
	})
	return context.WithValue(ctx, issueMoveGuardKey{}, once)
}

func issueMoveGuardFrom(ctx context.Context) issueMoveGuard {
	guard, _ := ctx.Value(issueMoveGuardKey{}).(issueMoveGuard)
	return guard
}

// idleGuardBeforeMove runs between reading the issue and locking it for the
// move. Tests use it to land a competing write in that window.
var idleGuardBeforeMove = func() {}

// isStaleMove reports a move that lost to a concurrent writer or to new
// activity. It is not a refusal and leaves no receipt: the next pass decides
// again from the issue as it is then.
func isStaleMove(err error) bool {
	return errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errWorkflowChanged)
}

// actOnIdleFinding moves one idle issue. The move re-checks idleness under the
// issue lock, in the same transaction as the status write; a write that landed
// since the issue was read leaves it where it is. A refused move leaves a
// bounded, visible record instead of vanishing.
func (handler *Handler) actOnIdleFinding(ctx context.Context, settings idleGuardSettings, finding idleGuardFinding) error {
	issue, err := handler.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: finding.WorkspaceID, ID: finding.IssueID})
	if err != nil {
		return err
	}
	prior, _ := readIdleGuardReceipt(issue)
	idleGuardBeforeMove()
	receipt := idleGuardReceipt{State: idleGuardMoved, From: issue.Status, IdleSince: finding.IdleSince.UTC().Format(time.RFC3339)}
	if prior.State == idleGuardRefused {
		receipt.Attempts = prior.Attempts
	}
	var unavailable error
	if receipt.To, receipt.Kind, unavailable = handler.idleGuardTarget(ctx, issue); unavailable != nil {
		if !errors.Is(unavailable, errIdleGuardNoTarget) {
			return unavailable
		}
		return handler.refuseIdleMove(ctx, issue, receipt, unavailable)
	}
	var comment db.Comment
	guard := func(ctx context.Context, q *db.Queries, params *db.UpdateIssueParams) error {
		locked, err := q.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID})
		if err != nil {
			return err
		}
		// The move was built from the row read above. Any write since then
		// (a field edit, a comment, a task) makes its dates, parent, stage and
		// assignee stale; the revision the write expects stays the one it was
		// built from.
		if locked.Revision != params.ExpectedRevision.Int64 {
			return fmt.Errorf("idle guard: issue changed since it was read: %w", pgx.ErrNoRows)
		}
		still, err := q.ListIdleStartedGuardIssues(ctx, db.ListIdleStartedGuardIssuesParams{
			WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, Statuses: settings.statuses(), AfterSeconds: settings.threshold().Seconds(),
			Episode: pgtype.Bool{Bool: true, Valid: true},
		})
		if err != nil {
			return err
		}
		if len(still) == 0 {
			return fmt.Errorf("idle guard: issue is no longer idle: %w", pgx.ErrNoRows)
		}
		receipt.MovedAt = time.Now().UTC().Format(time.RFC3339)
		// The explanation commits with the move, so it survives a notice that
		// cannot be admitted.
		created, err := q.CreateComment(ctx, db.CreateCommentParams{
			ID: dbid.NewV7(), IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, AuthorType: "system", AuthorID: pgtype.UUID{Valid: true},
			Content: fmt.Sprintf("Idle started-issue guard: no run, wakeup or sub-issue was left to continue this issue and it had been idle since %s. Moved from %s to %s (%s).",
				receipt.IdleSince, receipt.From, receipt.To, receipt.Kind),
			Type: "system",
		})
		if err != nil {
			return err
		}
		comment = created.Comment()
		receipt.CommentID = uuidToString(comment.ID)
		written, err := writeIdleGuardReceipt(ctx, q, issue, receipt)
		if err != nil {
			return err
		}
		if receipt.To == issuestatus.Blocked {
			reason, _ := json.Marshal("idle-no-continuation")
			if written, err = q.SetIssueMetadataKey(ctx, db.SetIssueMetadataKeyParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID, Key: "blocked_reason", Value: reason}); err != nil {
				return err
			}
		}
		params.ExpectedRevision = pgtype.Int8{Int64: written.Revision, Valid: true}
		return nil
	}
	extra := map[string]any{"kind": receipt.Kind, "idle_since": receipt.IdleSince}
	moved, _, err := handler.tryMoveIssueOnEdge(withIssueMoveGuard(ctx, guard), issue, receipt.To, "idle_started_guard", extra, false)
	switch {
	case err != nil && isStaleMove(err):
		return nil
	case err != nil:
		return handler.refuseIdleMove(ctx, issue, receipt, err)
	case !moved:
		return nil
	}
	slog.Info("idle_started_guard moved", "issue_id", uuidToString(issue.ID), "from", receipt.From, "to", receipt.To, "kind", receipt.Kind)
	handler.publishIdleGuardComment(ctx, issue.WorkspaceID, issue.ID, comment)
	return handler.noticeIdleGuardLeader(ctx, settings, issue.WorkspaceID, issue.ID)
}

var errIdleGuardNoTarget = errors.New("idle guard: the issue's workflow lists neither need_decision nor blocked")

// idleGuardTarget picks where an idle issue goes. A person is the next move when
// an open decision request, a member-filtered wakeup, a mention of a member in
// the newest comment, Readiness gated-human or a person-handled workflow step
// says so; otherwise nothing is left to continue it and it is blocked. A target
// the issue's workflow or catalog lacks falls back to blocked.
func (handler *Handler) idleGuardTarget(ctx context.Context, issue db.Issue) (to, kind string, err error) {
	kind = idleGuardKindNoContinuation
	requests, err := handler.Queries.ListDecisionRequestsByIssue(ctx, db.ListDecisionRequestsByIssueParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID})
	if err != nil {
		return "", "", err
	}
	for _, request := range requests {
		if request.Status == "open" {
			kind = idleGuardKindOpenRequest
		}
	}
	if kind == idleGuardKindNoContinuation {
		waiting, err := handler.idleGuardHumanWait(ctx, issue)
		if err != nil {
			return "", "", err
		}
		if waiting {
			kind = idleGuardKindHumanWait
		}
	}
	for _, candidate := range []string{idleGuardNeedDecision, issuestatus.Blocked} {
		if candidate == idleGuardNeedDecision && kind == idleGuardKindNoContinuation {
			continue
		}
		available, err := handler.idleGuardStatusAvailable(ctx, issue, candidate)
		if err != nil {
			return "", "", err
		}
		if available {
			return candidate, kind, nil
		}
	}
	return "", "", errIdleGuardNoTarget
}

func (handler *Handler) idleGuardHumanWait(ctx context.Context, issue db.Issue) (bool, error) {
	waiting, err := handler.Queries.IdleStartedGuardHumanWait(ctx, issue.ID)
	if err != nil || waiting {
		return waiting, err
	}
	if waiting, err = decisionWaitEligible(ctx, handler.Queries, issue); err != nil || waiting {
		return waiting, err
	}
	def, _, err := issueworkflow.ForIssue(ctx, handler.Queries, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
	if err != nil || def == nil {
		return false, err
	}
	step, ok := def.Step(issue.Status)
	if !ok {
		return false, nil
	}
	switch step.Handler.Type {
	case issueworkflow.HandlerMember, issueworkflow.HandlerProjectLead, issueworkflow.HandlerCreator, issueworkflow.HandlerHumanByRole:
		return true, nil
	}
	return false, nil
}

// idleGuardStatusAvailable reports whether the issue can sit in status: its
// workflow lists it, or, with no workflow, the workspace catalog knows it.
func (handler *Handler) idleGuardStatusAvailable(ctx context.Context, issue db.Issue, status string) (bool, error) {
	def, _, err := issueworkflow.ForIssue(ctx, handler.Queries, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
	if err != nil {
		return false, err
	}
	if def != nil {
		return def.Has(status), nil
	}
	if _, err := issuestatus.CategoryWithError(ctx, handler.Queries, issue.WorkspaceID, status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// refuseIdleMove records a move that did not happen, counts the attempt and,
// on the last one, gives up with a comment and a leader alert.
func (handler *Handler) refuseIdleMove(ctx context.Context, issue db.Issue, receipt idleGuardReceipt, cause error) error {
	if receipt.To == "" {
		receipt.To = issuestatus.Blocked
	}
	receipt.State, receipt.MovedAt, receipt.Attempts, receipt.Error = idleGuardRefused, "", receipt.Attempts+1, truncateIdleGuardError(cause)
	slog.Warn("idle_started_guard move refused", "issue_id", uuidToString(issue.ID), "attempt", receipt.Attempts, "err", cause)
	if receipt.Attempts < idleGuardAttempts {
		_, err := writeIdleGuardReceipt(ctx, handler.Queries, issue, receipt)
		return err
	}
	receipt.State = idleGuardGaveUp
	if _, err := writeIdleGuardReceipt(ctx, handler.Queries, issue, receipt); err != nil {
		return err
	}
	note := fmt.Sprintf("Idle started-issue guard: gave up after %d attempts to move this issue from %s to %s (idle since %s). Last error: %s",
		receipt.Attempts, receipt.From, receipt.To, receipt.IdleSince, receipt.Error)
	handler.postIdleGuardComment(ctx, issue, note)
	if task, err := handler.alertIdleGuardLeader(ctx, issue, note); err != nil {
		slog.Warn("idle_started_guard could not alert the leader after giving up", "issue_id", uuidToString(issue.ID), "err", err)
	} else if task.ID.Valid {
		handler.TaskService.PublishTaskEnqueued(ctx, task)
	}
	return nil
}
