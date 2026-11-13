package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// The exhaustion hook is three idempotent transactions over one
// run_budget_refusal row (CHE-2798 P2r):
//
//	R records the refusal (hook_phase intent).
//	B blocks the issue if the refusal is still eligible (phase blocked).
//	N writes the notice and the recovery run if still eligible (phase done).
//
// A B or N that finds the refusal ineligible retires the hook with a reason and
// moves or dispatches nothing. The in-process callback and the restart sweep
// both run advanceRefusalHook, so a crash between transactions is finished by
// the sweep from the row alone.

// runBudgetHookAt runs after each committed transaction of the hook
// ("after_intent", "after_block"). A test returns true to stop the hook there,
// as a crash would, or lands a write in the gap and returns false.
var runBudgetHookAt = func(stage string) bool { return false }

// forIssueWorkflow reads the workflow of an issue; a test replaces it to fail the lookup.
var forIssueWorkflow = issueworkflow.ForIssue

// errHookAdvanced means another advancer already took this step: nothing to do.
var errHookAdvanced = errors.New("run budget: the hook already advanced")

// hookRef names one generation of a refusal's hook; a revival starts the next.
type hookRef struct {
	id         pgtype.UUID
	generation int32
}

// hookIneligible means the refusal no longer applies; the hook is retired.
type hookIneligible struct{ reason string }

func (e *hookIneligible) Error() string { return "run budget: refusal not eligible: " + e.reason }

// refusalSince is the start of the window the refusal was recorded for.
func refusalSince(row db.RunBudgetRefusal) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, row.WindowID)
}

// exhaustedIssueWith reads the issue a refusal is about and reports whether the
// refusal still applies: not finished, its window still current and still
// spent, and still in the status that was refused unless it is already blocked.
func exhaustedIssueWith(ctx context.Context, q *db.Queries, e service.RunBudgetExhausted) (db.Issue, bool) {
	issue, err := q.GetIssue(ctx, e.IssueID)
	if err != nil || service.IsFinishedStatus(ctx, q, issue.WorkspaceID, issue.Status) {
		return issue, false
	}
	if refuses, err := service.RunBudgetStillRefuses(ctx, q, e.IssueID, e.Since); err != nil || !refuses {
		return issue, false
	}
	if issue.Status == issuestatus.Blocked {
		return issue, true
	}
	if issue.Status != e.Status {
		return issue, false
	}
	// A refused run for anyone but the reviewer must not block the review: the
	// reviewer's own first run on a head is still admitted.
	if service.IsReviewStatus(issue.Status) && issue.AssigneeID != e.Target.AgentID {
		return issue, false
	}
	return issue, true
}

// recordRefusalIntent is tx R: it records the refusal of one window and ends
// every earlier refusal of the issue. A callback that finds the window's row
// still serving a live or finished hook gets that row's id back; a row that no
// longer applies (its hook was retired, or a move superseded it) starts again
// for the new occurrence. ok is false when the refusal no longer applies and
// nothing was recorded.
func (h *Handler) recordRefusalIntent(ctx context.Context, e service.RunBudgetExhausted) (id pgtype.UUID, generation int32, ok bool) {
	window := e.Since.UTC().Format(time.RFC3339Nano)
	err := h.withPgxTx(ctx, func(tx pgx.Tx) error {
		q := h.Queries.WithTx(tx)
		// The issue lock comes before anything that reads or updates its runs.
		if _, err := q.LockIssueRunBudget(ctx, e.IssueID); err != nil {
			return err
		}
		taint, err := service.CtxTaint(ctx, q)
		if err != nil {
			return err
		}
		existing, err := q.GetRunBudgetRefusalByWindow(ctx, db.GetRunBudgetRefusalByWindowParams{IssueID: e.IssueID, WindowID: window})
		found := err == nil
		if !found && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if found {
			if reuse, err := refusalServes(ctx, q, existing); err != nil {
				return err
			} else if reuse {
				if err := q.MergeRunBudgetRefusalTaint(ctx, db.MergeRunBudgetRefusalTaintParams{ID: existing.ID, Taint: taint}); err != nil {
					return err
				}
				// Past the notice the recovery run exists and no longer reads the row.
				if existing.HookPhase == "done" && len(taint) > 0 {
					if err := q.TaintBudgetRecoveryRuns(ctx, db.TaintBudgetRecoveryRunsParams{IssueID: existing.IssueID, Taint: taint}); err != nil {
						return err
					}
				}
				// A resume that already ended on a run: the join reaches that exact run.
				if existing.TaskID.Valid && len(taint) > 0 {
					if err := q.UnionTaskTaint(ctx, db.UnionTaskTaintParams{ID: existing.TaskID, Taint: taint}); err != nil {
						return err
					}
				}
				id, generation, ok = existing.ID, existing.HookGeneration, true
				return nil
			}
		}
		issue, eligible := exhaustedIssueWith(ctx, q, e)
		if !eligible {
			return nil
		}
		in := db.InsertRunBudgetRefusalParams{
			TaintIds: taint,
			ID:       dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, WindowID: window,
			Status:  pgtype.Text{String: e.Status, Valid: true},
			AgentID: e.Target.AgentID, Trigger: e.Target.Trigger, State: "open",
			ActorUserID: e.Target.ActorUserID, TriggerCommentID: e.Target.TriggerCommentID,
		}
		newest, err := q.NewestIssueStatusChange(ctx, issue.ID)
		switch {
		case err == nil:
			in.OccurrenceActivityID = newest.ID
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		// Refused while blocked: the status and target the block hid are the
		// original refusal's, which the block's history row names. A block that
		// names none (made before refusals were recorded) has no status to give
		// back: the row ends unknown_origin and only the notice goes out.
		source := db.SetRunBudgetRefusalSourceParams{SourceWakeupID: e.Target.SourceWakeupID, SourceTaskID: e.Target.SourceTaskID}
		if issue.Status == issuestatus.Blocked {
			if prev, ok := h.blockingRefusal(ctx, q, newest); ok {
				in.Status, in.AgentID, in.Trigger, in.Blocked = prev.Status, prev.AgentID, prev.Trigger, true
				source = db.SetRunBudgetRefusalSourceParams{SourceWakeupID: prev.SourceWakeupID, SourceTaskID: prev.SourceTaskID}
				in.ActorUserID, in.TriggerCommentID = prev.ActorUserID, prev.TriggerCommentID
			} else {
				in.Status, in.Blocked = pgtype.Text{}, true
				in.State, in.StateReason = "superseded", pgtype.Text{String: stateReasonUnknownOrigin, Valid: true}
			}
		}
		if found {
			row, err := q.ReviveRunBudgetRefusal(ctx, db.ReviveRunBudgetRefusalParams{
				ID: existing.ID, Status: in.Status, OccurrenceActivityID: in.OccurrenceActivityID, Blocked: in.Blocked,
				AgentID: in.AgentID, Trigger: in.Trigger, StateReason: in.StateReason, State: in.State, TaintIds: in.TaintIds,
				ActorUserID: in.ActorUserID, TriggerCommentID: in.TriggerCommentID,
			})
			if err != nil {
				return fmt.Errorf("revive refusal: %w", err)
			}
			id, generation, ok = row.ID, row.HookGeneration, true
		} else {
			row, err := q.InsertRunBudgetRefusal(ctx, in)
			if err != nil {
				return fmt.Errorf("insert refusal: %w", err)
			}
			id, generation, ok = row.ID, row.HookGeneration, true
		}
		source.ID = id
		if err := q.SetRunBudgetRefusalSource(ctx, source); err != nil {
			return err
		}
		return q.SupersedeRunBudgetRefusalsExcept(ctx, db.SupersedeRunBudgetRefusalsExceptParams{IssueID: issue.ID, ID: id})
	})
	if err != nil {
		slog.Warn("run budget: record refusal failed", "err", err, "issue_id", uuidToString(e.IssueID))
		return pgtype.UUID{}, 0, false
	}
	return id, generation, ok
}

// stateReasonUnknownOrigin marks a refusal found on a block that left no refusal row.
const stateReasonUnknownOrigin = "unknown_origin"

// refusalServes reports whether the window's existing row answers a new refusal
// of the same window: its hook is still to run, or it ran and the issue is
// still where it left it. Any other row is started again, so the new refusal
// gets its own block.
func refusalServes(ctx context.Context, q *db.Queries, row db.RunBudgetRefusal) (bool, error) {
	switch {
	case row.HookPhase == "intent" || row.HookPhase == "blocked":
		return true, nil
	case row.State != "open" && row.State != "superseded":
		return true, nil
	case row.HookPhase == "retired":
		return false, nil
	}
	reason, err := refusalIneligible(ctx, q, row)
	return reason == "", err
}

// blockingRefusal is the refusal whose block is the issue's newest status
// occurrence, if the budget made that block.
func (h *Handler) blockingRefusal(ctx context.Context, q *db.Queries, newest db.NewestIssueStatusChangeRow) (db.RunBudgetRefusal, bool) {
	if newest.CauseKind != service.StatusCauseRunBudget || newest.RefusalID == "" {
		return db.RunBudgetRefusal{}, false
	}
	prev, err := q.GetRunBudgetRefusal(ctx, parseUUID(newest.RefusalID))
	return prev, err == nil && prev.Blocked
}

// refusalIneligible reports why the refusal no longer applies, or "". The
// issue must be where the hook last left it, in the status occurrence the
// refusal was recorded in: a status that went away and came back is another
// occurrence. Read through q, under the issue lock.
func refusalIneligible(ctx context.Context, q *db.Queries, row db.RunBudgetRefusal) (string, error) {
	if row.State != "open" && row.StateReason.String != stateReasonUnknownOrigin {
		return row.State, nil
	}
	_, reason, err := refusalMoved(ctx, q, row)
	if reason != "" || err != nil {
		return reason, err
	}
	since, err := refusalSince(row)
	if err != nil {
		return "", err
	}
	// Last: a status move also changes the window, and the move is the better reason.
	if still, err := service.RunBudgetStillRefuses(ctx, q, row.IssueID, since); err != nil {
		return "", err
	} else if !still {
		return "budget_restored", nil
	}
	return "", nil
}

// refusalMoved reports whether the issue left the status occurrence the
// refusal was recorded in ("status_moved", or "assignee_changed" for a review).
// It returns the issue it read.
func refusalMoved(ctx context.Context, q *db.Queries, row db.RunBudgetRefusal) (db.Issue, string, error) {
	issue, err := q.GetIssue(ctx, row.IssueID)
	if err != nil {
		return issue, "", err
	}
	want := row.Status.String
	if row.Blocked {
		want = issuestatus.Blocked
	}
	if issue.Status != want {
		return issue, "status_moved", nil
	}
	if service.IsReviewStatus(want) && issue.AssigneeID != row.AgentID {
		return issue, "assignee_changed", nil
	}
	var occurrence pgtype.UUID
	switch newest, err := q.NewestIssueStatusChange(ctx, row.IssueID); {
	case err == nil:
		occurrence = newest.ID
	case !errors.Is(err, pgx.ErrNoRows):
		return issue, "", err
	}
	if occurrence != row.OccurrenceActivityID {
		return issue, "status_moved", nil
	}
	return issue, "", nil
}

// lockHookStep locks the refusal and checks that its hook is in phase and still
// eligible. The caller holds the issue's budget lock. errHookAdvanced means
// another advancer took the step; *hookIneligible means the hook must retire.
func lockHookStep(ctx context.Context, q *db.Queries, id pgtype.UUID, generation int32, phase string) (taint []string, err error) {
	row, err := q.LockRunBudgetRefusal(ctx, id)
	if err != nil {
		return nil, err
	}
	if row.HookGeneration != generation || row.HookPhase != phase {
		return nil, errHookAdvanced
	}
	reason, err := refusalIneligible(ctx, q, row)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		return nil, &hookIneligible{reason: reason}
	}
	return row.TaintIds, nil
}

// advanceRefusalHook runs the steps of the hook the row still owes, in order.
// The event and generation come from the same refusal; a row revived since
// belongs to a newer one.
func (h *Handler) advanceRefusalHook(ctx context.Context, id pgtype.UUID, generation int32, e service.RunBudgetExhausted) {
	row, err := h.Queries.GetRunBudgetRefusal(ctx, id)
	if err != nil {
		slog.Warn("run budget: load refusal failed", "err", err, "refusal_id", uuidToString(id))
		return
	}
	if row.HookGeneration != generation {
		// The row was revived for a newer refusal, which owns it and its event.
		return
	}
	if _, err := h.Queries.GetIssue(ctx, row.IssueID); errors.Is(err, pgx.ErrNoRows) {
		h.settleHookStep(ctx, id, generation, &hookIneligible{reason: "issue_closed"})
		return
	}
	switch row.HookPhase {
	case "intent":
		if runBudgetHookAt("after_intent") || !h.settleHookStep(ctx, id, generation, h.blockStep(ctx, id, generation, e)) || runBudgetHookAt("after_block") {
			return
		}
	case "blocked":
	default:
		return
	}
	h.settleHookStep(ctx, id, generation, h.noticeStep(ctx, id, generation, e))
}

// settleHookStep reports whether the hook goes on after a step. A step that
// found the refusal ineligible retires the hook; a failed step stays for the sweep.
func (h *Handler) settleHookStep(ctx context.Context, id pgtype.UUID, generation int32, err error) bool {
	var ineligible *hookIneligible
	switch {
	case err == nil:
		return true
	case errors.Is(err, errHookAdvanced):
	case errors.As(err, &ineligible):
		if rerr := h.Queries.RetireRunBudgetRefusalHook(ctx, db.RetireRunBudgetRefusalHookParams{ID: id, Generation: generation, Reason: ineligible.reason}); rerr != nil {
			slog.Warn("run budget: retire hook failed", "err", rerr, "refusal_id", uuidToString(id))
		}
	default:
		slog.Warn("run budget: hook step failed", "err", err, "refusal_id", uuidToString(id))
	}
	return false
}

// blockStep is tx B. The eligibility check runs in the status write's
// transaction, so a move that commits first retires the hook and nothing is
// written; the phase advance is written by the block's own history row.
func (h *Handler) blockStep(ctx context.Context, id pgtype.UUID, generation int32, e service.RunBudgetExhausted) error {
	// Another writer (the 80% warning's comment, a person) can bump the issue's
	// revision between the read and the block. The block then loses, so it
	// reads the issue again and retries while the revision keeps moving.
	for try := 0; try < runBudgetBlockTries; try++ {
		issue, err := h.Queries.GetIssue(ctx, e.IssueID)
		if err != nil {
			return err
		}
		runBudgetBeforeBlock()
		def, _, err := forIssueWorkflow(ctx, h.Queries, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
		if err != nil {
			// Not "no Blocked step": the hook stays at intent for the sweep.
			return fmt.Errorf("read the issue's workflow: %w", err)
		}
		if issue.Status == issuestatus.Blocked || def == nil || !def.Has(issuestatus.Blocked) {
			// Something else parked the issue, or the workflow has no blocked
			// status: the notice goes out alone.
			return h.advanceWithoutBlock(ctx, id, generation, issue.ID)
		}
		guard := func(ctx context.Context, q *db.Queries, _ *db.UpdateIssueParams) error {
			if _, err := q.LockIssueRunBudget(ctx, issue.ID); err != nil {
				return err
			}
			if _, err := lockHookStep(ctx, q, id, generation, "intent"); err != nil {
				return err
			}
			// Committed with the move: the budget parked this issue in this window.
			_, err := q.ClaimRunBudgetNotice(ctx, db.ClaimRunBudgetNoticeParams{
				ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID,
				WindowID: e.Since.UTC().Format(time.RFC3339Nano), Kind: service.RunBudgetKindBlocked,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		details := map[string]any{
			"reason": issueworkflow.BlockedReasonRunBudget, "max_runs": e.Max, "runs": e.Runs,
			"refused_run": h.runBudgetRefusedRun(ctx, e), "refusal_id": uuidToString(id),
		}
		_, _, err = h.tryMoveIssueOnEdge(withIssueMoveGuard(ctx, guard), issue, issuestatus.Blocked, "run_budget", details, false)
		var ineligible *hookIneligible
		switch {
		case err == nil:
			h.setBlockedReason(ctx, issue)
			return nil
		case errors.Is(err, errHookAdvanced), errors.As(err, &ineligible):
			return err
		case errors.Is(err, pgx.ErrNoRows):
			continue
		default:
			return err
		}
	}
	return fmt.Errorf("run budget: block lost the revision race %d times", runBudgetBlockTries)
}

func (h *Handler) setBlockedReason(ctx context.Context, issue db.Issue) {
	if _, err := setIssueMetadataValue(ctx, h.Queries, issue.WorkspaceID, issue.ID, "blocked_reason", issueworkflow.BlockedReasonRunBudget); err != nil {
		slog.Warn("run budget: set blocked_reason failed", "err", err, "issue_id", uuidToString(issue.ID))
	}
}

// advanceWithoutBlock is tx B for a refusal that moves nothing: the phase
// advances if the refusal is still eligible.
func (h *Handler) advanceWithoutBlock(ctx context.Context, id pgtype.UUID, generation int32, issueID pgtype.UUID) error {
	return h.withPgxTx(ctx, func(tx pgx.Tx) error {
		q := h.Queries.WithTx(tx)
		if _, err := q.LockIssueRunBudget(ctx, issueID); err != nil {
			return err
		}
		if _, err := lockHookStep(ctx, q, id, generation, "intent"); err != nil {
			return err
		}
		return q.SetRunBudgetRefusalHook(ctx, db.SetRunBudgetRefusalHookParams{ID: id, HookPhase: "blocked"})
	})
}

// noticeStep is tx N. The recovery run debits the ledger, so it commits with
// the notice that states the balance, in the notice's transaction: whichever
// callback writes the notice also gives the run. The target is priced under the
// budget lock that transaction takes.
func (h *Handler) noticeStep(ctx context.Context, id pgtype.UUID, generation int32, e service.RunBudgetExhausted) error {
	issue, err := h.Queries.GetIssue(ctx, e.IssueID)
	if err != nil {
		return err
	}
	row, err := h.Queries.GetRunBudgetRefusal(ctx, id)
	if err != nil {
		return err
	}
	// This refusal moved the issue if its block is the newest status occurrence;
	// a refusal that found the issue already blocked only words a notice.
	moved := false
	if newest, err := h.Queries.NewestIssueStatusChange(ctx, e.IssueID); err == nil {
		moved = row.Blocked && newest.RefusalID == uuidToString(id)
	}
	refused := h.runBudgetRefusedRun(ctx, e)
	if moved {
		runBudgetBeforeRecovery()
	}
	err = h.writeRunBudgetNoticeFrom(ctx, service.RunBudgetEvent{IssueID: e.IssueID, Kind: service.RunBudgetKindExhausted, Runs: e.Runs, Max: e.Max, Since: e.Since},
		hookRef{id: id, generation: generation}, h.prepareBudgetRecovery(ctx, e),
		func(ctx context.Context, q *db.Queries) string {
			raise := runBudgetRaiseTarget(ctx, q, issue.ID, e)
			return exhaustedNoticeText(refused, raise.target, raise.leaves, e, moved)
		})
	if err == nil && moved {
		h.recordBudgetBlock(ctx, issue, e, refused)
	}
	return err
}

// sweepRunBudgetHooks finishes the hooks a crashed callback left behind, from
// the row and the ledger alone.
func (h *Handler) sweepRunBudgetHooks(ctx context.Context, before time.Time) {
	ids, err := h.Queries.ListStaleRunBudgetHooks(ctx, pgtype.Timestamptz{Time: before, Valid: true})
	if err != nil {
		slog.Warn("run budget: list stale hooks failed", "err", err)
		return
	}
	for _, id := range ids {
		row, err := h.Queries.GetRunBudgetRefusal(ctx, id)
		if err != nil {
			continue
		}
		since, err := refusalSince(row)
		if err != nil {
			slog.Warn("run budget: refusal window unreadable", "err", err, "refusal_id", uuidToString(id))
			continue
		}
		e := service.RunBudgetExhausted{
			IssueID: row.IssueID, Since: since, Status: row.Status.String,
			Target: service.RunTarget{AgentID: row.AgentID, Trigger: row.Trigger},
		}
		if max, _, err := service.CurrentRunBudget(ctx, h.Queries, row.IssueID); err == nil {
			e.Max = max
		}
		e.Runs, _ = h.Queries.CountIssueRunsSinceExcept(ctx, db.CountIssueRunsSinceExceptParams{
			IssueID: row.IssueID, Since: pgtype.Timestamptz{Time: since, Valid: true},
		})
		h.advanceRefusalHook(ctx, id, row.HookGeneration, e)
	}
}

// runBudgetHookStale is how long a hook may sit unfinished before the sweep
// takes it over from a callback that may still be running.
const runBudgetHookStale = time.Minute

// RunRunBudgetHookSweeper finishes crashed exhaustion hooks every minute until
// ctx ends.
func (h *Handler) RunRunBudgetHookSweeper(ctx context.Context) {
	ticker := time.NewTicker(runBudgetHookStale)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.sweepRunBudgetHooks(ctx, time.Now().Add(-runBudgetHookStale))
			h.sweepRunBudgetResumes(ctx, time.Now().Add(-runBudgetHookStale))
		}
	}
}
