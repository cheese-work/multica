package handler

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/attribution"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
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
	// errResumeRelinked: a newer approval linked the refusal after the worker read it.
	errResumeRelinked = errors.New("run budget: the refusal was linked by a newer approval")
	// errResumeSurvivorHeld: a pending run holds the slot but is not the refused
	// work and cannot take the stored comment now. The refusal stays resume_pending
	// and the sweep retries it once that run has started or finished.
	errResumeSurvivorHeld = errors.New("run budget: a pending run for another comment holds the slot")
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
// answers, under the issue lock it already holds. A refusal still waiting (open,
// still_blocked or resume_pending) takes the latest approval's action and
// window; a superseded or resumed refusal stays as it is; a refusal of another
// issue and an approval without a refusal link nothing.
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
// and checks the refusal is still waiting and eligible. It returns the row as
// the lock read it: a taint that joined after the worker's first read is on it
// (CHE-2860), so the run is never admitted with the older copy.
func lockResume(ctx context.Context, q *db.Queries, id, issueID, actionID pgtype.UUID) (db.RunBudgetRefusal, error) {
	if _, err := q.LockIssueRunBudget(ctx, issueID); err != nil {
		return db.RunBudgetRefusal{}, err
	}
	row, err := q.LockRunBudgetRefusal(ctx, id)
	if err != nil {
		return db.RunBudgetRefusal{}, err
	}
	if row.State != "resume_pending" {
		return db.RunBudgetRefusal{}, errResumeDone
	}
	if row.ActionID != actionID {
		return db.RunBudgetRefusal{}, errResumeRelinked
	}
	reason, err := resumeIneligible(ctx, q, row)
	if err != nil {
		return db.RunBudgetRefusal{}, err
	}
	if reason != "" {
		return db.RunBudgetRefusal{}, &resumeSuperseded{reason: reason}
	}
	return row, nil
}

// isRunRefusal reports whether err is an admission refusal, as opposed to a failure.
func isRunRefusal(err error) bool {
	return errors.Is(err, service.ErrRunBudgetExhausted) || errors.Is(err, service.ErrStageQueueFull) ||
		errors.Is(err, service.ErrAdmissionDeferred) || errors.Is(err, service.ErrIssueInTriage) ||
		errors.Is(err, service.ErrRecoveryCaused) || errors.Is(err, errSquadLeaderNotAdmitted)
}

// finishResume is the worker's terminal write, in the worker's transaction.
// task is the exact run the resume admitted or found already holding the slot;
// invalid when the restore started none. fresh is the row the locks read: its
// taint is unioned into that run, so a run that survived the refusal is never
// cleaner than the refusal (the trigger keeps it monotonic).
func finishResume(ctx context.Context, q *db.Queries, fresh db.RunBudgetRefusal, task pgtype.UUID, direct bool) error {
	state := "run_queued"
	if !task.Valid && !direct {
		state = "status_restored"
	}
	if task.Valid && len(fresh.TaintIds) > 0 {
		if err := q.UnionTaskTaint(ctx, db.UnionTaskTaintParams{ID: task, Taint: fresh.TaintIds}); err != nil {
			return err
		}
	}
	if err := q.FinishRunBudgetRefusalResume(ctx, db.FinishRunBudgetRefusalResumeParams{ID: fresh.ID, State: state, TaskID: task}); err != nil {
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
		case errors.Is(err, errResumeRelinked):
			continue
		case errors.Is(err, errResumeSurvivorHeld):
			slog.Info("run budget: resume waits for the pending run", "refusal_id", uuidToString(id))
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
	runBudgetResumeAt("before_resume")
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
	// A blocked refusal always ends in the restore, whatever its source: the
	// restored status owes its own run, never the source's.
	if row.Blocked {
		return h.resumeBlocked(ctx, row, issue)
	}
	if row.SourceWakeupID.Valid || row.SourceTaskID.Valid {
		return h.resumeSource(ctx, row, issue)
	}
	return h.resumeDirect(ctx, row, issue)
}

// resumeBlocked gives the status the hook hid back and admits the run that
// status owes, in the transaction of the move.
func (h *Handler) resumeBlocked(ctx context.Context, row db.RunBudgetRefusal, issue db.Issue) (func(context.Context), error) {
	to := row.Status.String
	overlay := h.prepareEdgeRunOverlay(ctx, issue, to, true)
	// fresh is the refusal as the guard's lock read it, inside the move.
	fresh := row
	guard := func(ctx context.Context, q *db.Queries, _ *db.UpdateIssueParams) error {
		locked, err := lockResume(ctx, q, row.ID, issue.ID, row.ActionID)
		if err == nil {
			fresh = locked
		}
		return err
	}
	extra := map[string]any{"reason": "budget_approved", "action_id": uuidToString(row.ActionID)}
	var edge edgeMove
	// The run the move admits carries the taint the refusal recorded; finishResume
	// adds what joined after this read.
	moveCtx := withIssueMoveGuard(service.WithTaint(ctx, row.TaintIds), guard)
	err := h.withPgxTx(ctx, func(tx pgx.Tx) error {
		q := h.Queries.WithTx(tx)
		var err error
		edge, err = h.moveIssueOnEdgeTx(moveCtx, q, issue, to, "run_budget", extra, true, overlay)
		if errors.Is(err, service.ErrAgentArchived) {
			return &resumeSuperseded{reason: "agent_archived"}
		}
		if isPermanentRestoreError(err) {
			return &resumeSuperseded{reason: "restore_refused"}
		}
		if err != nil {
			return err
		}
		if errors.Is(edge.RunRefused, service.ErrAgentArchived) {
			return &resumeSuperseded{reason: "agent_archived"}
		}
		if edge.RunRefused != nil && !errors.Is(edge.RunRefused, service.ErrDuplicatePendingTask) {
			return &resumeRefused{cause: edge.RunRefused}
		}
		return finishResume(ctx, q, fresh, edge.TaskID, false)
	})
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) { edge.AfterCommit(ctx) }, nil
}

// resumeSource runs again the native dispatch a refusal stopped: a wakeup tick,
// a system wakeup or a retry (CHE-2860). The dispatch, not the worker, builds
// the run, so its evidence, attribution and receipts are the ones it would have
// had; it binds the refusal to the run in its own transaction (bindRefusedRun),
// together with the receipts or retry it consumes. A refusal that blocked the
// issue never comes here (resumeOnce).
func (h *Handler) resumeSource(ctx context.Context, row db.RunBudgetRefusal, issue db.Issue) (func(context.Context), error) {
	if err := h.kickSource(ctx, row); isRunRefusal(err) {
		return nil, &resumeRefused{cause: err}
	} else if err != nil {
		return nil, err
	}
	bound, err := h.Queries.GetRunBudgetRefusal(ctx, row.ID)
	switch {
	case err != nil:
		return nil, err
	case bound.State != "resume_pending":
		// run_queued: the dispatch delivered the run and bound the refusal to it.
		return nil, nil
	}
	// Nothing was delivered. The budget still refusing is a later approval's to
	// answer; otherwise the source no longer has the work (its receipts were
	// consumed, its rule ended, a run already holds the slot).
	since, err := refusalSince(bound)
	if err != nil {
		return nil, err
	}
	if still, err := service.RunBudgetStillRefuses(ctx, h.Queries, bound.IssueID, since); err != nil {
		return nil, err
	} else if still {
		return nil, &resumeRefused{cause: service.ErrRunBudgetExhausted}
	}
	return nil, &resumeSuperseded{reason: "source_not_delivered"}
}

// kickSource runs the source's dispatch once with the hook suppressed. A
// dispatch that refuses returns that refusal.
func (h *Handler) kickSource(ctx context.Context, row db.RunBudgetRefusal) error {
	if row.SourceWakeupID.Valid {
		return (&service.IssueWakeupService{Tasks: h.TaskService}).DispatchRule(ctx, row.WorkspaceID, row.SourceWakeupID)
	}
	parent, err := h.Queries.GetAgentTask(ctx, row.SourceTaskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = h.TaskService.MaybeRetryFailedTask(ctx, parent)
	return err
}

// isPermanentRestoreError reports whether the move that restores the hidden
// status was refused for a reason a retry cannot change: the workflow or a gate
// says no. A lost race and an infrastructure failure are not permanent.
func isPermanentRestoreError(err error) bool {
	var notInWorkflow *issueworkflow.NotInWorkflowError
	var notAllowed *issueworkflow.MoveNotAllowedError
	var stepMoved *issueworkflow.StepMovedError
	var staleVerdict *issueworkflow.StaleVerdictError
	var entryGate *EntryGateError
	var notConfirmed *service.StopNotConfirmedError
	return errors.Is(err, issueworkflow.ErrOverrideMissing) || errors.Is(err, errFindingsRequired) ||
		errors.Is(err, errVerdictNeedsRun) || errors.Is(err, errIssueStatusArchivedRace) ||
		errors.As(err, &notInWorkflow) || errors.As(err, &notAllowed) || errors.As(err, &stepMoved) ||
		errors.As(err, &staleVerdict) || errors.As(err, &entryGate) || errors.As(err, &notConfirmed)
}

// resumeDirect admits the run the budget refused, for an issue the hook never
// blocked. The run is the one the refusal stored: its agent and trigger.
func (h *Handler) resumeDirect(ctx context.Context, row db.RunBudgetRefusal, issue db.Issue) (func(context.Context), error) {
	var overlay *service.PreparedRuntimeMCPOverlay
	if issue.AssigneeType.String == "agent" {
		overlay = h.TaskService.PrepareIssueRunOverlay(ctx, issue, pgtype.UUID{})
	}
	prep := h.prepareBind(ctx, row, issue)
	var after func()
	err := h.withPgxTx(ctx, func(tx pgx.Tx) error {
		after = nil
		q := h.Queries.WithTx(tx)
		fresh, err := lockResume(ctx, q, row.ID, issue.ID, row.ActionID)
		if err != nil {
			return err
		}
		// Read under the lock: the run is admitted for the issue as it is now.
		locked, err := q.GetIssue(ctx, issue.ID)
		if err != nil {
			return err
		}
		// The run carries the taint the locked refusal holds now.
		var taskID pgtype.UUID
		taskID, after, err = h.enqueueRefusedRun(service.WithTaint(ctx, fresh.TaintIds), q, fresh, locked, overlay)
		switch {
		case err == nil:
		case errors.Is(err, service.ErrDuplicatePendingTask):
			// The pending run is the refused work only if it takes the stored comment.
			if after, err = h.bindSurvivor(ctx, q, fresh, taskID, prep); err != nil {
				return err
			}
		case isRunRefusal(err):
			return &resumeRefused{cause: err}
		default:
			return err
		}
		return finishResume(ctx, q, fresh, taskID, true)
	})
	if err != nil {
		return nil, err
	}
	if after == nil {
		return nil, nil
	}
	return func(context.Context) { after() }, nil
}

// bindPrep is what folding the stored comment into a surviving run needs. It is
// read before the transaction: the overlay is a network call that must not run
// under the issue lock.
type bindPrep struct {
	agent  db.Agent
	attr   attribution.Result
	params db.MergeCommentIntoQueuedIssueTaskParams
}

// prepareBind reads the re-attribution the stored comment would stamp on a
// surviving run. Nil when the refusal stored no comment or the agent or the
// attribution cannot be read; bindSurvivor then holds the refusal if it needs it.
func (h *Handler) prepareBind(ctx context.Context, row db.RunBudgetRefusal, issue db.Issue) *bindPrep {
	if !row.TriggerCommentID.Valid {
		return nil
	}
	agentID := row.AgentID
	switch {
	case agentID.Valid:
	case issue.AssigneeType.String == "agent":
		agentID = issue.AssigneeID
	case issue.AssigneeType.String == "squad":
		squad, err := h.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: issue.AssigneeID, WorkspaceID: issue.WorkspaceID})
		if err != nil {
			return nil
		}
		agentID = squad.LeaderID
	default:
		return nil
	}
	agent, err := h.Queries.GetAgent(ctx, agentID)
	if err != nil {
		return nil
	}
	attr, err := h.TaskService.AttributionForMergedComment(ctx, issue.WorkspaceID, row.TriggerCommentID, row.Trigger == "mention", agent)
	if err != nil {
		return nil
	}
	mcp, apps := h.TaskService.BuildRuntimeMCPOverlayForMerge(ctx, attr.UserID, agent)
	return &bindPrep{agent: agent, attr: attr, params: db.MergeCommentIntoQueuedIssueTaskParams{
		NewTriggerCommentID:     row.TriggerCommentID,
		NewTriggerSummary:       h.TaskService.BuildCommentTriggerSummary(ctx, issue.WorkspaceID, row.TriggerCommentID),
		NewOriginatorUserID:     attr.UserID,
		NewAccountableUserID:    attr.AccountableUserID,
		NewOriginatorSource:     pgtype.Text{String: attr.Source.String(), Valid: true},
		NewDelegatedFromTaskID:  attr.DelegatedFromTaskID,
		NewRuleVersionID:        attr.RuleVersionID,
		NewTriggerEvidenceKind:  pgtype.Text{String: string(attr.EvidenceKind), Valid: attr.EvidenceKind != ""},
		NewTriggerEvidenceRefID: attr.EvidenceRefID,
		NewRuntimeMcpOverlay:    mcp,
		NewRuntimeConnectedApps: apps,
		IssueID:                 issue.ID,
		AgentID:                 agent.ID,
		HeadSha:                 h.TaskService.ResolveIssueReviewSHAParam(ctx, issue.ID),
	}}
}

// bindSurvivor decides whether the pending run that held the slot is the refused
// work, in the resume transaction. It is when the refusal stored no comment, when
// the run already answers the stored comment, or when this transaction adds that
// comment to it: folded into a queued run with the refusal's attribution, or
// registered as planned input of a started one. Otherwise the run answers
// something else and ending the refusal on it would lose the refused comment:
// errResumeSurvivorHeld, and the refusal stays resume_pending.
func (h *Handler) bindSurvivor(ctx context.Context, q *db.Queries, row db.RunBudgetRefusal, survivor pgtype.UUID, prep *bindPrep) (func(), error) {
	if !row.TriggerCommentID.Valid {
		return nil, nil
	}
	task, err := q.GetAgentTask(ctx, survivor)
	if err != nil {
		return nil, err
	}
	if task.TriggerCommentID == row.TriggerCommentID || slices.Contains(task.CoalescedCommentIds, row.TriggerCommentID) {
		return nil, nil
	}
	if prep == nil || prep.agent.ID != task.AgentID {
		return nil, errResumeSurvivorHeld
	}
	var merged db.AgentTaskQueue
	switch task.Status {
	case "queued":
		params := prep.params
		params.TargetTaskID = survivor
		params.SquadID = task.SquadID
		folded, err := q.MergeCommentIntoQueuedIssueTask(ctx, params)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errResumeSurvivorHeld
		}
		if err != nil {
			return nil, err
		}
		merged = task
		merged.CoalescedCommentIds = folded.CoalescedCommentIds
	case "dispatched", "running", "waiting_local_directory":
		// Only a run of the same thread takes planned input.
		planned, err := q.RegisterPlannedCommentForActiveTask(ctx, db.RegisterPlannedCommentForActiveTaskParams{
			CommentID:    row.TriggerCommentID,
			IssueID:      task.IssueID,
			AgentID:      task.AgentID,
			TargetTaskID: survivor,
			HeadSha:      prep.params.HeadSha,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errResumeSurvivorHeld
		}
		if err != nil {
			return nil, err
		}
		merged = task
		merged.CoalescedCommentIds = planned.CoalescedCommentIds
	default:
		return nil, errResumeSurvivorHeld
	}
	return func() {
		h.TaskService.RecordHumanDelegation(ctx, prep.attr)
		h.TaskService.BroadcastTaskCoalesced(ctx, merged)
	}, nil
}

// enqueueRefusedRun queues the run the refusal stored. An assignment-level or
// squad-leader run needs the assignee it was refused for; any other trigger (a
// mention, a wakeup) runs its own agent whoever the assignee is. A refusal
// without a valid agent falls back to the assignee.
func (h *Handler) enqueueRefusedRun(ctx context.Context, q *db.Queries, row db.RunBudgetRefusal, issue db.Issue, overlay *service.PreparedRuntimeMCPOverlay) (pgtype.UUID, func(), error) {
	th := h.onQueries(q)
	// A wakeup or a retry keeps its evidence in the wakeup rule or the failed
	// task (resumeSource runs it). A refusal recorded before the source was kept
	// has none to run, and re-admitting it here would make it a mention.
	switch row.Trigger {
	case "wakeup", "system wakeup", "retry":
		return pgtype.UUID{}, nil, &resumeSuperseded{reason: "source_unknown"}
	}
	agentID, viaSquad := row.AgentID, row.Trigger == "squad leader callback"
	switch {
	case !agentID.Valid && issue.AssigneeType.String == "agent":
		agentID = issue.AssigneeID
	case !agentID.Valid && issue.AssigneeType.String == "squad":
		viaSquad = true
		squad, err := q.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: issue.AssigneeID, WorkspaceID: issue.WorkspaceID})
		if err != nil {
			return pgtype.UUID{}, nil, &resumeSuperseded{reason: "assignee_changed"}
		}
		agentID = squad.LeaderID
	case !agentID.Valid:
		return pgtype.UUID{}, nil, &resumeSuperseded{reason: "assignee_changed"}
	}
	if viaSquad {
		squad, err := q.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: issue.AssigneeID, WorkspaceID: issue.WorkspaceID})
		if issue.AssigneeType.String != "squad" || err != nil || squad.LeaderID != agentID {
			return pgtype.UUID{}, nil, &resumeSuperseded{reason: "assignee_changed"}
		}
	}
	if agent, err := q.GetAgent(ctx, agentID); errors.Is(err, pgx.ErrNoRows) || (err == nil && agent.ArchivedAt.Valid) {
		return pgtype.UUID{}, nil, &resumeSuperseded{reason: "agent_archived"}
	} else if err != nil {
		return pgtype.UUID{}, nil, err
	}
	switch {
	case viaSquad:
		actorType, actorID := "system", ""
		if row.ActorUserID.Valid {
			actorType, actorID = "member", uuidToString(row.ActorUserID)
		}
		return th.enqueueSquadLeaderRunAsInTx(ctx, q, issue, row.TriggerCommentID, actorType, actorID)
	case row.Trigger == "assignment or status handoff" || row.Trigger == "comment":
		if issue.AssigneeType.String != "agent" || issue.AssigneeID != agentID {
			return pgtype.UUID{}, nil, &resumeSuperseded{reason: "assignee_changed"}
		}
		return th.TaskService.EnqueueIssueRunInTx(ctx, q, issue, row.TriggerCommentID, row.ActorUserID, overlay)
	}
	survivor, err := q.PendingTaskForIssueAndAgent(ctx, db.PendingTaskForIssueAndAgentParams{
		IssueID: issue.ID,
		AgentID: agentID,
		HeadSha: th.TaskService.ResolveIssueReviewSHAParam(ctx, issue.ID),
	})
	if err == nil {
		return survivor, nil, service.ErrDuplicatePendingTask
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, nil, err
	}
	params, err := th.TaskService.PrepareTaskForMention(ctx, q, issue, agentID, row.TriggerCommentID, row.ActorUserID, service.OriginDerived)
	if err != nil {
		return pgtype.UUID{}, nil, err
	}
	task, err := th.TaskService.EnqueuePreparedTaskForMentionInTx(ctx, q, issue, params, service.OriginDerived)
	if err != nil {
		return pgtype.UUID{}, nil, err
	}
	return task.ID, func() { h.TaskService.PublishTaskEnqueued(ctx, task) }, nil
}

// refuseResume records why a rolled-back resume was refused: still_blocked
// while the budget refuses (a later approval links it again), else the restore
// itself was refused and the refusal ends.
func (h *Handler) refuseResume(ctx context.Context, row db.RunBudgetRefusal, cause error) {
	if !errors.Is(cause, service.ErrRunBudgetExhausted) {
		h.failResume(ctx, row, "superseded", "restore_refused")
		return
	}
	h.failResume(ctx, row, "still_blocked", "runs")
}

// failResume records the outcome of the approval the worker resumed. A newer
// approval that linked the refusal meanwhile keeps its link: the statement is
// conditioned on the action the worker read.
func (h *Handler) failResume(ctx context.Context, row db.RunBudgetRefusal, state, reason string) {
	runBudgetResumeAt("before_outcome")
	if err := h.Queries.FailRunBudgetRefusalResume(ctx, db.FailRunBudgetRefusalResumeParams{ID: row.ID, ActionID: row.ActionID, State: state, Reason: reason}); err != nil {
		slog.Warn("run budget: record resume outcome failed", "err", err, "refusal_id", uuidToString(row.ID))
	}
}

// notifyRunBudgetResume starts the resume of every refusal an approved action
// linked, once the approval committed. None linked: nothing to do.
func (h *Handler) notifyRunBudgetResume(ctx context.Context, actionID pgtype.UUID) error {
	ids, err := h.Queries.ListRunBudgetRefusalIDsByAction(ctx, actionID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		h.resumeRefusal(ctx, id)
	}
	return nil
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
