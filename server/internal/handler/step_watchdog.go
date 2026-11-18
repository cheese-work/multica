package handler

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The step watchdog (CHE-6116) finds an issue that waited in a handler step
// with no run, and says why no run came. This file only reads; the actions
// (block, re-dispatch, notice) are later slices.

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
// query is paged by keyset so older issues in a step with no handler cannot fill
// the batch and hide a candidate behind them.
func (h *Handler) stepWatchdogCandidates(ctx context.Context, limit int32) ([]stepCandidate, error) {
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
