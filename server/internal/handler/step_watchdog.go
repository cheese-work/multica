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
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// The step watchdog (CHE-6116) finds an issue that waited in a handler step
// with no run, and says why no run came. The block and shadow actions are here;
// re-dispatch is a later slice; the notice to requested_by is in step_watchdog_notice.go.

// stepWatchdogMinAge is how long an issue may sit in its status occurrence
// before the watchdog looks at it.
const stepWatchdogMinAge = 24 * time.Hour

// stepCandidate is an issue that has waited in a handler step with no run.
type stepCandidate struct {
	Issue         db.Issue
	OccurrenceKey string
	InStepSince   time.Time
}

// stepWatchdogCase names the ledger action the candidate calls for.
type stepWatchdogCase string

const (
	stepCaseBlockBudget stepWatchdogCase = "block_budget"
	stepCaseRedispatch  stepWatchdogCase = "redispatch"
	stepCaseNotifyOnly  stepWatchdogCase = "notify_only"
)

// stepClass is the typed decision for one candidate. Refusal is the admission
// sentinel behind it, nil when the run would start.
type stepClass struct {
	Case    stepWatchdogCase
	Refusal error
}

// stepWatchdogCandidates lists up to limit issues, oldest first. A member
// assignee, a finished or backlog status, an active run, a Cheese stop and a
// need_decision issue with a decision request are filtered in the query; a step
// without a handler is filtered here, because only the workflow knows it. The
// query is paged by keyset, and keep (nil keeps all) drops what the caller will
// not act on, so older issues that are skipped or already handled cannot fill the
// batch and hide a candidate behind them.
// ponytail: a tick scans every kept-out candidate before it reaches the limit,
// so its cost grows with the stalled issues the watchdog skips; keep a cursor
// across ticks if that is ever measured.
func (h *Handler) stepWatchdogCandidates(ctx context.Context, limit int32, keep func(stepCandidate) (bool, error)) ([]stepCandidate, error) {
	var out []stepCandidate
	afterAt, afterID := pgtype.Timestamptz{Valid: true}, pgtype.UUID{Valid: true} // year 1, nil UUID: before every row
	olderThan := pgtype.Timestamptz{Time: time.Now().Add(-stepWatchdogMinAge), Valid: true}
	for int32(len(out)) < limit {
		rows, err := h.Queries.ListStepWatchdogCandidates(ctx, db.ListStepWatchdogCandidatesParams{
			OlderThan: olderThan, AfterAt: afterAt, AfterID: afterID, RowLimit: limit,
		})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if int32(len(out)) >= limit {
				break
			}
			cand, ok, err := h.stepCandidateFor(ctx, row)
			if err != nil {
				return nil, err
			}
			if ok && keep != nil {
				ok, err = keep(cand)
				if err != nil {
					return nil, err
				}
			}
			if ok {
				out = append(out, cand)
			}
		}
		if int32(len(rows)) < limit {
			break
		}
		last := rows[len(rows)-1]
		afterAt, afterID = last.InStepSince, last.ID
	}
	return out, nil
}

// stepCandidateFor keeps a row only when its workflow step has a handler.
func (h *Handler) stepCandidateFor(ctx context.Context, row db.ListStepWatchdogCandidatesRow) (stepCandidate, bool, error) {
	issue, err := h.Queries.GetIssue(ctx, row.ID)
	if err != nil {
		return stepCandidate{}, false, err
	}
	def, _, err := issueworkflow.ForIssue(ctx, h.Queries, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
	if err != nil {
		// A dangling workflow override has no handler to wait for.
		if errors.Is(err, issueworkflow.ErrOverrideMissing) {
			return stepCandidate{}, false, nil
		}
		return stepCandidate{}, false, err
	}
	if def == nil {
		return stepCandidate{}, false, nil
	}
	if step, ok := def.Step(issue.Status); !ok || !step.HandsOff() {
		return stepCandidate{}, false, nil
	}
	return stepCandidate{Issue: issue, OccurrenceKey: row.OccurrenceKey, InStepSince: row.InStepSince.Time}, true, nil
}

// classifyStep asks admission what a run of the step's handler would meet now.
// A run budget or weekly guard refusal is a budget case; any other refusal gets
// a notice only, since a re-dispatch would be refused again; no refusal means
// the run was simply never started.
func (h *Handler) classifyStep(ctx context.Context, c stepCandidate) (stepClass, error) {
	agentID := squadLeaderOrAgent(ctx, h.Queries, c.Issue.WorkspaceID, c.Issue.AssigneeType.String, c.Issue.AssigneeID)
	if !agentID.Valid {
		return stepClass{Case: stepCaseNotifyOnly, Refusal: errSquadLeaderNotAdmitted}, nil
	}
	refusal := service.RunAdmissionRefusal(ctx, h.Queries, c.Issue, agentID)
	switch {
	case refusal == nil:
		return stepClass{Case: stepCaseRedispatch}, nil
	case errors.Is(refusal, service.ErrRunBudgetExhausted):
		return stepClass{Case: stepCaseBlockBudget, Refusal: refusal}, nil
	case errors.Is(refusal, service.ErrAgentPaused), errors.Is(refusal, service.ErrAgentArchived):
		return stepClass{Case: stepCaseNotifyOnly, Refusal: refusal}, nil
	}
	return stepClass{}, refusal
}

// Workspace setting step_watchdog.mode. Unset or unknown is shadow: record what
// the watchdog would do and do nothing.
const (
	stepWatchdogOff    = "off"
	stepWatchdogShadow = "shadow"
	stepWatchdogOn     = "on"
)

func stepWatchdogModeFromSettings(settings []byte) string {
	var doc struct {
		Watchdog struct {
			Mode string `json:"mode"`
		} `json:"step_watchdog"`
	}
	if json.Unmarshal(settings, &doc) != nil {
		return stepWatchdogShadow
	}
	switch doc.Watchdog.Mode {
	case stepWatchdogOff, stepWatchdogOn:
		return doc.Watchdog.Mode
	}
	return stepWatchdogShadow
}

// errStepWatchdogClaimed means another sweeper holds the claim for this action
// on this status occurrence: nothing was written.
var errStepWatchdogClaimed = errors.New("step watchdog: action already claimed")

// sweepStepWatchdog acts on up to limit candidates. A candidate in an off
// workspace, one this mode has nothing to do for, and one whose action is already
// claimed are not counted, so repeated sweeps move past them. A failure on one
// candidate does not stop the rest.
func (h *Handler) sweepStepWatchdog(ctx context.Context, limit int32) error {
	var failures error
	fail := func(c stepCandidate, err error) {
		slog.Warn("step watchdog: candidate failed", "err", err, "issue_id", uuidToString(c.Issue.ID))
		failures = errors.Join(failures, err)
	}
	modes := map[pgtype.UUID]string{}
	classes := map[pgtype.UUID]stepClass{}
	keep := func(c stepCandidate) (bool, error) {
		mode, ok := modes[c.Issue.WorkspaceID]
		if !ok {
			ws, err := h.Queries.GetWorkspace(ctx, c.Issue.WorkspaceID)
			if err != nil {
				fail(c, err)
				return false, nil
			}
			mode = stepWatchdogModeFromSettings(ws.Settings)
			modes[c.Issue.WorkspaceID] = mode
		}
		if mode == stepWatchdogOff {
			return false, nil
		}
		class, err := h.classifyStep(ctx, c)
		if err != nil {
			fail(c, err)
			return false, nil
		}
		action := "would_act"
		if mode == stepWatchdogOn {
			if class.Case != stepCaseBlockBudget {
				return false, nil // re-dispatch and notice are later slices
			}
			action = string(class.Case)
		}
		claimed, err := h.Queries.StepWatchdogActionClaimed(ctx, db.StepWatchdogActionClaimedParams{
			IssueID: c.Issue.ID, OccurrenceKey: c.OccurrenceKey, Action: action,
		})
		if err != nil {
			fail(c, err)
			return false, nil
		}
		classes[c.Issue.ID] = class
		return !claimed, nil
	}
	cands, err := h.stepWatchdogCandidates(ctx, limit, keep)
	if err != nil {
		return errors.Join(failures, err)
	}
	for _, c := range cands {
		if err := h.actOnStep(ctx, c, classes[c.Issue.ID], modes[c.Issue.WorkspaceID]); err != nil {
			fail(c, err)
		}
	}
	// Owed notices come from the ledger, so they follow the actions above and any an earlier tick left.
	return errors.Join(failures, h.deliverOwedStepNotices(ctx, limit))
}

// actOnStep acts once per (issue, status occurrence, action). Only the budget
// block acts in on mode so far; sweepStepWatchdog keeps the other cases out.
func (h *Handler) actOnStep(ctx context.Context, c stepCandidate, class stepClass, mode string) error {
	switch {
	case mode == stepWatchdogShadow:
		return h.recordWouldAct(ctx, c, class)
	case class.Case == stepCaseBlockBudget:
		return h.blockBudgetRefused(ctx, c, class)
	}
	return nil
}

// stepOccurrenceKey names the issue's current status occurrence: the newest
// status_changed row, or created:<issue id> when it has none. Never empty.
func stepOccurrenceKey(ctx context.Context, q *db.Queries, issueID pgtype.UUID) (string, error) {
	newest, err := q.NewestIssueStatusChange(ctx, issueID)
	switch {
	case err == nil:
		return uuidToString(newest.ID), nil
	case errors.Is(err, pgx.ErrNoRows):
		return "created:" + uuidToString(issueID), nil
	}
	return "", err
}

// claimStepAction locks the issue, checks that it still sits in the occurrence
// the candidate was read in, and claims the action in the caller's transaction.
// A moved issue or a lost claim returns an error that stepWatchdogSkipped reads.
func claimStepAction(ctx context.Context, q *db.Queries, c stepCandidate, action string, details map[string]any) (db.Issue, error) {
	locked, err := q.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{ID: c.Issue.ID, WorkspaceID: c.Issue.WorkspaceID})
	if err != nil {
		return db.Issue{}, err
	}
	key, err := stepOccurrenceKey(ctx, q, c.Issue.ID)
	if err != nil {
		return db.Issue{}, err
	}
	if key != c.OccurrenceKey || locked.Status != c.Issue.Status {
		return db.Issue{}, fmt.Errorf("step watchdog: the issue left the status occurrence: %w", pgx.ErrNoRows)
	}
	body, err := json.Marshal(details)
	if err != nil {
		return db.Issue{}, err
	}
	if _, err := q.ClaimStepWatchdogAction(ctx, db.ClaimStepWatchdogActionParams{
		ID: dbid.NewV7(), WorkspaceID: c.Issue.WorkspaceID, IssueID: c.Issue.ID, OccurrenceKey: key, Action: action, Details: body,
	}); errors.Is(err, pgx.ErrNoRows) {
		return db.Issue{}, errStepWatchdogClaimed
	} else if err != nil {
		return db.Issue{}, err
	}
	return locked, nil
}

// stepWatchdogSkipped reports a lost race, not a failure: another sweeper acted,
// or the issue moved. The next tick decides again from the issue as it is.
func stepWatchdogSkipped(err error) bool {
	return errors.Is(err, errStepWatchdogClaimed) || isStaleMove(err)
}

func stepClassDetails(c stepCandidate, class stepClass) map[string]any {
	d := map[string]any{"case": string(class.Case), "status": c.Issue.Status, "in_step_since": c.InStepSince.UTC().Format(time.RFC3339)}
	if class.Refusal != nil {
		d["refusal"] = class.Refusal.Error()
	}
	return d
}

// recordWouldAct is shadow mode: one ledger row and one activity row per status
// occurrence, and nothing else.
func (h *Handler) recordWouldAct(ctx context.Context, c stepCandidate, class stepClass) error {
	details := stepClassDetails(c, class)
	err := h.withPgxTx(ctx, func(tx pgx.Tx) error {
		q := h.Queries.WithTx(tx)
		if _, err := claimStepAction(ctx, q, c, "would_act", details); err != nil {
			return err
		}
		body, _ := json.Marshal(details)
		_, err := q.CreateActivity(ctx, db.CreateActivityParams{
			WorkspaceID: c.Issue.WorkspaceID, IssueID: c.Issue.ID, ActorType: pgtype.Text{String: "system", Valid: true},
			Action: "step_watchdog_would_act", Details: body,
		})
		return err
	})
	if stepWatchdogSkipped(err) {
		return nil
	}
	return err
}

// budgetBlockReason is the typed blocked_reason for a budget refusal.
func budgetBlockReason(refusal error) (reason, label string) {
	if errors.Is(refusal, service.ErrWeeklyRunGuard) {
		return issueworkflow.BlockedReasonWeeklyRunGuard, "weekly run guard"
	}
	return issueworkflow.BlockedReasonRunBudget, "run budget"
}

// blockBudgetRefused blocks an issue whose handler run a budget refuses. The
// claim, the explanation and the status write commit together, so a second
// sweeper on the same occurrence finds the claim or a moved issue and writes
// nothing. The watchdog never writes a done category status.
func (h *Handler) blockBudgetRefused(ctx context.Context, c stepCandidate, class stepClass) error {
	issue, err := h.Queries.GetIssue(ctx, c.Issue.ID)
	if err != nil {
		return err
	}
	if issue.Status == issuestatus.Blocked || issue.Status != c.Issue.Status {
		return nil
	}
	if ok, err := statusAvailableToIssue(ctx, h.Queries, issue, issuestatus.Blocked); err != nil || !ok {
		return err // no blocked status to move to: leave the issue where it is
	}
	reason, label := budgetBlockReason(class.Refusal)
	var comment db.Comment
	guard := func(ctx context.Context, q *db.Queries, params *db.UpdateIssueParams) error {
		locked, err := claimStepAction(ctx, q, c, string(stepCaseBlockBudget), stepClassDetails(c, class))
		if err != nil {
			return err
		}
		if locked.Revision != params.ExpectedRevision.Int64 {
			return fmt.Errorf("step watchdog: issue changed since it was read: %w", pgx.ErrNoRows)
		}
		runs, err := q.CountIssueRunsSince(ctx, db.CountIssueRunsSinceParams{
			IssueID: issue.ID, Since: pgtype.Timestamptz{Time: time.Now().Add(-service.WeeklyRunGuardWindow), Valid: true},
		})
		if err != nil {
			return err
		}
		created, err := q.CreateComment(ctx, db.CreateCommentParams{
			ID: dbid.NewV7(), IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, AuthorType: "system", AuthorID: pgtype.UUID{Valid: true},
			Content: fmt.Sprintf("Step watchdog: this issue sat in %s since %s with no run, and the %s refuses its handler's run (%d runs in the last 7 days). Blocked (reason %s).",
				issue.Status, c.InStepSince.UTC().Format(time.RFC3339), label, runs, reason),
			Type: "system",
		})
		if err != nil {
			return err
		}
		comment = created.Comment()
		if _, err := setIssueMetadataValue(ctx, q, issue.WorkspaceID, issue.ID, "blocked_reason", reason); err != nil {
			return err
		}
		// The comment and the metadata write bumped the revision the status write expects.
		current, err := q.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID})
		if err != nil {
			return err
		}
		params.ExpectedRevision = pgtype.Int8{Int64: current.Revision, Valid: true}
		return nil
	}
	moved, _, err := h.tryMoveIssueOnEdge(withIssueMoveGuard(ctx, guard), issue, issuestatus.Blocked, "step_watchdog_budget", stepClassDetails(c, class), false)
	switch {
	case err != nil && stepWatchdogSkipped(err):
		return nil
	case err != nil:
		return err
	case moved:
		h.publishIdleGuardComment(ctx, issue.WorkspaceID, issue.ID, comment)
	}
	return nil
}
