package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-6172: a weekly run guard refusal is no silent stop. The first refusal of
// an issue keeps the refused trigger, opens one spend card for the owner and
// writes one system comment. Approving the card adds runGuardAllowRuns runs to
// the guard for 7 days and runs the kept trigger once; rejecting runs nothing.

const (
	runGuardAllowType = "issue.run_guard.allow"
	// weeklyCardFingerprint is the request fingerprint of one refusal's card.
	weeklyCardFingerprint = serverDecisionFingerprint + "run_guard_allow:"
)

// openWeeklyRunGuardCard records the refusal and opens its card, comment and
// inbox ping in one transaction. An issue with a card still open gets nothing
// more; a refusal whose card was answered is superseded by the new one.
func (h *Handler) openWeeklyRunGuardCard(ctx context.Context, issue db.Issue, e service.WeeklyRunGuardTripped) error {
	// CHE-6299: a finished issue gets no card. The decision lifecycle cancels
	// it as issue_closed within seconds, a cancelled card does not count as
	// declined, and the next refusal then opens another one. The refusal
	// stands (no run starts) and the incident row is written by the caller.
	if service.IsFinishedStatus(ctx, h.Queries, issue.WorkspaceID, issue.Status) {
		return nil
	}
	// CHE-6241: a refusal whose trigger is a system comment (the reject or
	// approve note, the guard's own notice) is the platform echoing itself, not
	// a person or agent asking for a run. It opens no card, so a declined card
	// stays declined until a new member or agent trigger is refused.
	if e.Target.TriggerCommentID.Valid {
		trigger, err := h.Queries.GetComment(ctx, e.Target.TriggerCommentID)
		if err == nil && trigger.AuthorType == "system" {
			return nil
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	var results []decisionRequestResult
	var comment db.Comment
	var revision int64
	err := h.withPgxTx(ctx, func(tx pgx.Tx) error {
		results, comment, revision = nil, db.Comment{}, 0
		q := h.Queries.WithTx(tx)
		recipients, err := runBudgetRecipients(ctx, q, issue)
		if err != nil {
			return err
		}
		owners, err := runBudgetCardRecipients(ctx, q, issue, recipients)
		if err != nil {
			return err
		}
		// Decision locks first, then the issue the comment locks.
		plan, err := h.planRunBudgetDecisions(ctx, q, issue, owners)
		if err != nil || len(plan.lanes) == 0 {
			return err
		}
		open, err := q.LockOpenWeeklyRunGuardRefusal(ctx, issue.ID)
		switch {
		case err == nil:
			standing, err := q.CountOpenWeeklyRunGuardCards(ctx, db.CountOpenWeeklyRunGuardCardsParams{IssueID: issue.ID, RequestFingerprint: weeklyCardFingerprint + uuidToString(open.ID)})
			if err != nil || standing > 0 {
				return err
			}
			// CHE-6241: a declined card stays declined for its trigger. The same
			// trigger refused again inside the guard window opens no card; a new
			// trigger from a member or an agent does.
			if open.CreatedAt.Time.After(time.Now().Add(-7*24*time.Hour)) && sameWeeklyRefusedTrigger(open, e) {
				declined, err := q.HasRejectedWeeklyRunGuardCard(ctx, db.HasRejectedWeeklyRunGuardCardParams{IssueID: issue.ID, RequestFingerprint: weeklyCardFingerprint + uuidToString(open.ID)})
				if err != nil || declined {
					return err
				}
			}
			if err := q.SetWeeklyRunGuardRefusalState(ctx, db.SetWeeklyRunGuardRefusalStateParams{ID: open.ID, State: "superseded"}); err != nil {
				return err
			}
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		refusal, err := q.InsertWeeklyRunGuardRefusal(ctx, db.InsertWeeklyRunGuardRefusalParams{
			ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, AgentID: e.Target.AgentID, Trigger: e.Target.Trigger,
			ActorUserID: e.Target.ActorUserID, TriggerCommentID: e.Target.TriggerCommentID,
			SourceWakeupID: e.Target.SourceWakeupID, SourceTaskID: e.Target.SourceTaskID, Runs: e.Runs,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // a concurrent refusal opened the card
		}
		if err != nil {
			return err
		}
		agentName := "the assignee"
		if agent, err := q.GetAgent(ctx, e.Target.AgentID); err == nil {
			agentName = agent.Name
		}
		trigger := e.Target.Trigger
		if trigger == "" {
			trigger = "run"
		}
		ask := fmt.Sprintf("**Weekly run guard**\nThe platform refused a run on [Issue](mention://issue/%s): it started %d runs in 7 days.\nRefused trigger: %s, for %s.\nApprove with your passkey to allow %d more runs for 7 days and run that trigger once, or decline to keep it stopped.",
			uuidToString(issue.ID), e.Runs, trigger, agentName, decisionaction.RunGuardAllowRuns)
		raw, err := json.Marshal(decisionaction.RunGuardAllowParams{IssueID: uuidToString(issue.ID), RefusalID: uuidToString(refusal.ID), Runs: decisionaction.RunGuardAllowRuns})
		if err != nil {
			return err
		}
		draft, err := draftDecisionAction(&decisionActionWire{Type: runGuardAllowType, Version: 1, Params: raw})
		if err != nil {
			return err
		}
		input := decisionRequestInput{
			Ask: ask, EscalationClass: actionRequestClass("reversible", draft.Definition.MinClass), Action: draft,
			Fingerprint: weeklyCardFingerprint + uuidToString(refusal.ID),
		}
		var approveURL string
		workspace, err := q.GetWorkspace(ctx, issue.WorkspaceID)
		if err != nil {
			return err
		}
		// One card per issue: the first owner gets it, and any owner may answer it.
		for _, lane := range plan.lanes[:1] {
			input.HumanUserID = lane.human
			result, err := h.openRunBudgetDecision(ctx, q, issue, plan, lane, input)
			if err != nil {
				return err
			}
			results = append(results, result)
			if approveURL == "" {
				approveURL = "/" + workspace.Slug + "/issues/" + uuidToString(issue.ID) + "?approve=" + uuidToString(result.Action.ID)
			}
		}
		content := fmt.Sprintf("Weekly run guard: this issue started %d runs in 7 days, so the platform refused a run (%s, for %s). Nothing runs on it until a workspace owner approves.\n\n[Approve %d more runs](%s)",
			e.Runs, trigger, agentName, decisionaction.RunGuardAllowRuns, approveURL)
		created, err := q.CreateComment(ctx, db.CreateCommentParams{
			ID: dbid.NewV7(), IssueID: issue.ID, WorkspaceID: issue.WorkspaceID,
			AuthorType: "system", AuthorID: pgtype.UUID{Valid: true}, Content: content, Type: "system",
		})
		if err != nil {
			return err
		}
		comment, revision = created.Comment(), created.IssueRevision
		return nil
	})
	if err != nil {
		return err
	}
	if comment.ID.Valid {
		h.publish(protocol.EventCommentCreated, uuidToString(issue.WorkspaceID), "system", "", map[string]any{
			"comment": commentToResponse(comment, nil, nil), "issue_title": issue.Title,
			"issue_assignee_type": textToPtr(issue.AssigneeType), "issue_assignee_id": uuidToPtr(issue.AssigneeID),
			"issue_status": issue.Status, "issue_revision": revision,
		})
	}
	h.publishRunBudgetDecisions(issue, results)
	return nil
}

// executeRunGuardAllowAction marks the refusal approved for the post-commit
// resume and grants the allowance in the same transaction. A refusal that no
// longer waits grants nothing.
func executeRunGuardAllowAction(ctx context.Context, execution decisionActionExecution) (json.RawMessage, error) {
	var params decisionaction.RunGuardAllowParams
	if err := json.Unmarshal(execution.Action.Params, &params); err != nil {
		return nil, err
	}
	issueID, err := parseUUIDString(params.IssueID)
	if err != nil {
		return nil, err
	}
	refusalID, err := parseUUIDString(params.RefusalID)
	if err != nil {
		return nil, err
	}
	q := execution.Queries
	refusal, err := q.GetWeeklyRunGuardRefusal(ctx, refusalID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (refusal.IssueID != issueID || refusal.WorkspaceID != execution.Action.WorkspaceID)) {
		return nil, actionFailure{Code: "refusal_mismatch", Message: "the refusal does not belong to this issue"}
	}
	if err != nil {
		return nil, err
	}
	// The refusal moves from open to approved once; only that move grants runs.
	moved, err := q.ApproveWeeklyRunGuardRefusal(ctx, db.ApproveWeeklyRunGuardRefusalParams{ID: refusalID, IssueID: issueID, ActionID: execution.Action.ID})
	if err != nil {
		return nil, err
	}
	if moved == 0 {
		return nil, actionFailure{Code: "refusal_closed", Message: "this refusal was already approved or is no longer open"}
	}
	// CHE-6295: the allowance counts from the current run count. An issue
	// already over its limit would otherwise stay refused after the approval.
	runs := int64(params.Runs)
	limit, err := service.WeeklyRunGuardLimit(ctx, q, issueID)
	if err != nil {
		return nil, err
	}
	if limit > 0 {
		used, err := q.CountIssueRunsSince(ctx, db.CountIssueRunsSinceParams{
			IssueID: issueID, Since: pgtype.Timestamptz{Time: time.Now().Add(-service.WeeklyRunGuardWindow), Valid: true},
		})
		if err != nil {
			return nil, err
		}
		runs += max(used-limit, 0)
	}
	if err := q.CreateWeeklyRunGuardAllowance(ctx, db.CreateWeeklyRunGuardAllowanceParams{
		ID: dbid.NewV7(), WorkspaceID: refusal.WorkspaceID, IssueID: issueID, Runs: int32(runs), ActionID: execution.Action.ID,
	}); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"issue_id": params.IssueID, "runs": runs})
}

// resumeWeeklyRefusalsOfAction runs the trigger of every refusal the action approved.
func (h *Handler) resumeWeeklyRefusalsOfAction(ctx context.Context, actionID pgtype.UUID) error {
	rows, err := h.Queries.ListApprovedWeeklyRunGuardRefusalsByAction(ctx, actionID)
	if err != nil {
		return err
	}
	for _, row := range rows {
		h.resumeWeeklyRefusal(ctx, row.ID)
	}
	return nil
}

// sweepWeeklyRunGuardResumes resumes approved refusals a crash left behind.
func (h *Handler) sweepWeeklyRunGuardResumes(ctx context.Context, before time.Time) {
	rows, err := h.Queries.ListWeeklyRunGuardRefusalsToResume(ctx, pgtype.Timestamptz{Time: before, Valid: true})
	if err != nil {
		slog.Warn("weekly run guard: list pending resumes failed", "err", err)
		return
	}
	for _, row := range rows {
		h.resumeWeeklyRefusal(ctx, row.ID)
	}
}

// resumeWeeklyRefusal runs the kept trigger once. The refusal row is locked and
// ends 'resumed' in the transaction that queues the run, so a repeat finds
// nothing to resume. A run another gate or the guard refuses ends 'refused'; a
// failure leaves the row approved for the sweep.
func (h *Handler) resumeWeeklyRefusal(ctx context.Context, id pgtype.UUID) {
	// A refused resume ends the refusal; it must not open another card.
	ctx = service.WithoutRunBudgetHook(ctx)
	state, err := h.resumeWeeklyOnce(ctx, id)
	if err != nil {
		slog.Warn("weekly run guard: resume failed", "err", err, "refusal_id", uuidToString(id))
		return
	}
	if state != "" {
		if err := h.Queries.SetWeeklyRunGuardRefusalState(ctx, db.SetWeeklyRunGuardRefusalStateParams{ID: id, State: state}); err != nil {
			slog.Warn("weekly run guard: record resume outcome failed", "err", err, "refusal_id", uuidToString(id))
		}
	}
	// CHE-6295: an approved trigger that is refused again is never dropped
	// silently. The suppressed hook opens no card (that would loop), so say it
	// in a system comment.
	if state == "refused" {
		h.postWeeklyResumeRefusedComment(ctx, id)
	}
}

// postWeeklyResumeRefusedComment tells the issue that the approved trigger was
// refused again. A system-authored comment opens no card and runs nothing
// (CHE-6241), so it cannot loop.
func (h *Handler) postWeeklyResumeRefusedComment(ctx context.Context, id pgtype.UUID) {
	ref, err := h.Queries.GetWeeklyRunGuardRefusal(ctx, id)
	if err != nil {
		slog.Warn("weekly run guard: read refused resume failed", "err", err, "refusal_id", uuidToString(id))
		return
	}
	issue, err := h.Queries.GetIssue(ctx, ref.IssueID)
	if err != nil {
		slog.Warn("weekly run guard: read issue of refused resume failed", "err", err, "refusal_id", uuidToString(id))
		return
	}
	created, err := h.Queries.CreateComment(ctx, db.CreateCommentParams{
		ID: dbid.NewV7(), IssueID: issue.ID, WorkspaceID: issue.WorkspaceID,
		AuthorType: "system", AuthorID: pgtype.UUID{Valid: true}, Type: "system",
		Content: "Weekly run guard: the approved run was refused again by another run gate (a competing run, the loop breaker or the run budget). Nothing was started. Run `multica issue budget get` to see what blocks the issue, then re-trigger it.",
	})
	if err != nil {
		slog.Warn("weekly run guard: post refused resume comment failed", "err", err, "refusal_id", uuidToString(id))
		return
	}
	h.publish(protocol.EventCommentCreated, uuidToString(issue.WorkspaceID), "system", "", map[string]any{
		"comment": commentToResponse(created.Comment(), nil, nil), "issue_title": issue.Title,
		"issue_assignee_type": textToPtr(issue.AssigneeType), "issue_assignee_id": uuidToPtr(issue.AssigneeID),
		"issue_status": issue.Status, "issue_revision": created.IssueRevision,
	})
}

// resumeWeeklyOnce returns the state to record after the transaction, or "" when
// the transaction already recorded it (or the refusal is not waiting).
func (h *Handler) resumeWeeklyOnce(ctx context.Context, id pgtype.UUID) (string, error) {
	ref, err := h.Queries.GetWeeklyRunGuardRefusal(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && ref.State != "approved") {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	row := db.RunBudgetRefusal{
		ID: ref.ID, WorkspaceID: ref.WorkspaceID, IssueID: ref.IssueID, AgentID: ref.AgentID, Trigger: ref.Trigger,
		ActorUserID: ref.ActorUserID, TriggerCommentID: ref.TriggerCommentID,
		SourceWakeupID: ref.SourceWakeupID, SourceTaskID: ref.SourceTaskID,
	}
	issue, err := h.Queries.GetIssue(ctx, ref.IssueID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && service.IsFinishedStatus(ctx, h.Queries, issue.WorkspaceID, issue.Status)) {
		return "superseded", nil
	}
	if err != nil {
		return "", err
	}
	// A wakeup or a retry runs its own dispatch, which builds the run.
	if ref.SourceWakeupID.Valid || ref.SourceTaskID.Valid {
		switch err := h.kickSource(ctx, row); {
		case isRunRefusal(err):
			return "refused", nil
		case err != nil:
			return "", err
		}
		return "resumed", nil
	}
	var overlay *service.PreparedRuntimeMCPOverlay
	if issue.AssigneeType.String == "agent" {
		overlay = h.TaskService.PrepareIssueRunOverlay(ctx, issue, pgtype.UUID{})
	}
	var after func()
	err = h.withPgxTx(ctx, func(tx pgx.Tx) error {
		after = nil
		q := h.Queries.WithTx(tx)
		if _, err := q.LockApprovedWeeklyRunGuardRefusal(ctx, id); errors.Is(err, pgx.ErrNoRows) {
			return weeklyResumeEnded("") // a concurrent resume took it
		} else if err != nil {
			return err
		}
		locked, err := q.GetIssue(ctx, issue.ID)
		if err != nil {
			return err
		}
		var superseded *resumeSuperseded
		_, run, err := h.enqueueRefusedRun(ctx, q, row, locked, overlay)
		switch {
		case err == nil:
			after = run
		case errors.Is(err, service.ErrDuplicatePendingTask):
			return weeklyResumeEnded("resumed") // a pending run already holds the slot
		case errors.As(err, &superseded):
			return weeklyResumeEnded("superseded")
		case isRunRefusal(err):
			return weeklyResumeEnded("refused")
		default:
			return err
		}
		return q.SetWeeklyRunGuardRefusalState(ctx, db.SetWeeklyRunGuardRefusalStateParams{ID: id, State: "resumed"})
	})
	var ended weeklyResumeEnded
	if errors.As(err, &ended) {
		return string(ended), nil // rolled back: the refused attempt wrote nothing
	}
	if err != nil {
		return "", err
	}
	if after != nil {
		after()
	}
	return "", nil
}

// weeklyResumeEnded ends a resume without a run: the state to record after the
// transaction rolled back, or "" when there is nothing to record.
type weeklyResumeEnded string

func (e weeklyResumeEnded) Error() string { return "weekly run guard: resume ended: " + string(e) }

// addWeeklyRunGuard adds the weekly run guard's count and limit to a budget
// read and, while it refuses, names it in Blocking with its open card.
func (h *Handler) addWeeklyRunGuard(ctx context.Context, issue db.Issue, view *runBudgetView) error {
	limit, err := service.WeeklyRunGuardLimit(ctx, h.Queries, issue.ID)
	if err != nil || limit <= 0 {
		return err
	}
	runs, err := h.Queries.CountIssueRunsSince(ctx, db.CountIssueRunsSinceParams{
		IssueID: issue.ID, Since: pgtype.Timestamptz{Time: time.Now().Add(-service.WeeklyRunGuardWindow), Valid: true},
	})
	if err != nil {
		return err
	}
	view.WeeklyRuns, view.WeeklyLimit = runs, limit
	if runs < limit {
		return nil
	}
	workspace, err := h.Queries.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return err
	}
	// Only block mode refuses a run; warn and off leave the issue unblocked.
	if service.WeeklyRunGuardModeFromSettings(workspace.Settings) != service.WeeklyRunGuardBlock {
		return nil
	}
	view.Blocking = append(view.Blocking, "weekly_run_guard")
	if view.OpenCard != nil {
		return nil
	}
	card, err := h.Queries.FindOpenWeeklyRunGuardCard(ctx, db.FindOpenWeeklyRunGuardCardParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	view.OpenCard = &runBudgetCard{
		RequestID: uuidToString(card.RequestID), ActionID: uuidToString(card.ActionID),
		URL: "/" + workspace.Slug + "/issues/" + uuidToString(issue.ID) + "?approve=" + uuidToString(card.ActionID),
	}
	view.NextCommand = "Wait for the approval card " + view.OpenCard.URL + ". Do not open another."
	return nil
}

// sameWeeklyRefusedTrigger reports whether the event refuses the very trigger
// the open refusal recorded: the same agent, kind, comment, wakeup and task.
func sameWeeklyRefusedTrigger(open db.WeeklyRunGuardRefusal, e service.WeeklyRunGuardTripped) bool {
	return open.AgentID == e.Target.AgentID && open.Trigger == e.Target.Trigger &&
		open.TriggerCommentID == e.Target.TriggerCommentID &&
		open.SourceWakeupID == e.Target.SourceWakeupID && open.SourceTaskID == e.Target.SourceTaskID
}
