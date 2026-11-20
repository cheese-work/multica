package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The intake_simple and qa_proof entry gates (CHE-7668): the facts they read
// and the intake_call activity of each evaluation.

const (
	activityIntakeCall   = "intake_call"
	activityPCWSet       = "post_close_work_set"
	gateCommentPageSize  = 200
	gateCommentMaxPages  = 25
	gateActivityScanSize = 1000
)

func entryGateOn(step issueworkflow.Step, kind string) bool {
	for _, g := range step.Entry {
		if parsed, err := issueworkflow.ParseGuard(g.Guard); err == nil && parsed.Kind == kind && g.Mode != issueworkflow.EntryModeOff {
			return true
		}
	}
	return false
}

// loadGateFacts adds the intake and QA facts that the step's gates ask for.
func (w *workflowWrite) loadGateFacts(ctx context.Context, q *db.Queries, issue db.Issue, step issueworkflow.Step, facts *issueworkflow.Facts) {
	if entryGateOn(step, issueworkflow.GuardIntakeSimple) {
		facts.Intake = loadIntakeFacts(ctx, q, issue)
	}
	if entryGateOn(step, issueworkflow.GuardQAProof) {
		facts.QA = w.loadQAFacts(ctx, q, issue)
	}
}

// commentsSince reads the issue's comments after since, oldest first.
func commentsSince(ctx context.Context, q *db.Queries, issue db.Issue, since time.Time) ([]db.Comment, error) {
	var out []db.Comment
	after := pgtype.Timestamptz{Time: since, Valid: true}
	for range gateCommentMaxPages {
		page, err := q.ListCommentsSinceForIssue(ctx, db.ListCommentsSinceForIssueParams{
			IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, CreatedAt: after, Limit: gateCommentPageSize,
		})
		if err != nil {
			return nil, err
		}
		for _, c := range page {
			if !c.DeletedAt.Valid && c.Type != "system" {
				out = append(out, c)
			}
		}
		if len(page) < gateCommentPageSize {
			return out, nil
		}
		after = page[len(page)-1].CreatedAt
	}
	return nil, fmt.Errorf("the issue has more than %d comments", gateCommentPageSize*gateCommentMaxPages)
}

// loadIntakeFacts reads the intake lines from the description and the comments,
// the newest value of a line winning, and whether a decision request is open.
func loadIntakeFacts(ctx context.Context, q *db.Queries, issue db.Issue) *issueworkflow.IntakeFacts {
	facts := &issueworkflow.IntakeFacts{Lines: issueworkflow.ParseIntakeLines(issue.Description.String)}
	comments, err := commentsSince(ctx, q, issue, time.Time{})
	if err != nil {
		facts.Err = "the comments could not be read: " + err.Error()
		return facts
	}
	for _, c := range comments {
		for name, value := range issueworkflow.ParseIntakeLines(c.Content) {
			facts.Lines[name] = value
		}
	}
	open, err := q.HasOpenDecisionRequestForIssue(ctx, db.HasOpenDecisionRequestForIssueParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID})
	if err != nil {
		facts.Err = "decision requests could not be read: " + err.Error()
		return facts
	}
	facts.OpenDecision = open
	return facts
}

// stepEntered is when the issue entered the step it is in: its newest status
// move onto that status; zero when no such move is stored.
func stepEntered(ctx context.Context, q *db.Queries, issue db.Issue) (time.Time, error) {
	rows, err := q.ListActivitiesForIssue(ctx, db.ListActivitiesForIssueParams{IssueID: issue.ID, Limit: gateActivityScanSize})
	if err != nil {
		return time.Time{}, err
	}
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Action != "status_changed" {
			continue
		}
		var d struct {
			To string `json:"to"`
		}
		if json.Unmarshal(rows[i].Details, &d) == nil && d.To == issue.Status {
			return rows[i].CreatedAt.Time, nil
		}
	}
	return time.Time{}, nil
}

// recordPostCloseWorkSetter stores who set post_close_work and to what, so that
// qa_proof can tell the exiting agent's own declaration from a value that
// another actor or an earlier run left behind. A failed log leaves the setter
// unrecorded and the gate then refuses: it fails closed.
func (h *Handler) recordPostCloseWorkSetter(ctx context.Context, issue db.Issue, actorType, actorID string, value json.RawMessage) {
	var v any
	if err := json.Unmarshal(value, &v); err != nil {
		return
	}
	if err := recordEntryActivity(ctx, h.Queries, issue, actorType, actorID, activityPCWSet, map[string]any{"value": v}); err != nil {
		slog.Warn("post_close_work setter not recorded", "issue_id", uuidToString(issue.ID), "err", err)
	}
}

// postCloseWorkSetBy reports whether the newest write of post_close_work was
// made by actor and left the value the issue holds now.
func postCloseWorkSetBy(ctx context.Context, q *db.Queries, issue db.Issue, actorType, actorID string) (bool, error) {
	rows, err := q.ListActivitiesForIssue(ctx, db.ListActivitiesForIssueParams{IssueID: issue.ID, Limit: gateActivityScanSize})
	if err != nil {
		return false, err
	}
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Action != activityPCWSet {
			continue
		}
		var d struct {
			Value any `json:"value"`
		}
		if json.Unmarshal(rows[i].Details, &d) != nil {
			return false, nil
		}
		current := metadataString(issue.Metadata, postCloseWorkKey)
		same := rows[i].ActorType.String == actorType && uuidToString(rows[i].ActorID) == actorID
		return same && d.Value == current, nil
	}
	return false, nil
}

// loadQAFacts reads the writer's comments since the issue entered the step it
// is leaving, the post_close_work metadata and whether that agent set it.
func (w *workflowWrite) loadQAFacts(ctx context.Context, q *db.Queries, issue db.Issue) *issueworkflow.QAFacts {
	facts := &issueworkflow.QAFacts{PostCloseWork: metadataString(issue.Metadata, postCloseWorkKey)}
	entered, err := stepEntered(ctx, q, issue)
	if err != nil {
		facts.Err = "the step entry could not be read: " + err.Error()
		return facts
	}
	if facts.PostCloseWork != "" {
		facts.PostCloseWorkByAgent, err = postCloseWorkSetBy(ctx, q, issue, w.entry.actorType, w.entry.actorID)
		if err != nil {
			facts.Err = "the post_close_work setter could not be read: " + err.Error()
			return facts
		}
	}
	comments, err := commentsSince(ctx, q, issue, entered)
	if err != nil {
		facts.Err = "the comments could not be read: " + err.Error()
		return facts
	}
	for _, c := range comments {
		if c.AuthorType == w.entry.actorType && uuidToString(c.AuthorID) == w.entry.actorID {
			facts.Bodies = append(facts.Bodies, c.Content)
		}
	}
	return facts
}

// intakeCall is one evaluation of intake_simple, ready to be written.
type intakeCall struct {
	issue   db.Issue
	details map[string]any
}

func (w *workflowWrite) newIntakeCall(issue db.Issue, stepKey string, failed []issueworkflow.EntryFailure, facts *issueworkflow.IntakeFacts) intakeCall {
	call, estimate := issueworkflow.IntakeCall(failed, facts)
	return intakeCall{issue: issue, details: map[string]any{"call": call, "estimate": estimate, "step": stepKey}}
}

func (w *workflowWrite) writeIntakeCall(ctx context.Context, q *db.Queries, c intakeCall) error {
	return recordEntryActivity(ctx, q, c.issue, w.entry.actorType, w.entry.actorID, activityIntakeCall, c.details)
}

// flushRefusedIntakeCall writes the call of a refused write. The refusal rolls
// the write transaction back, so the row is written after it, on its own: the
// rejected call is the row the A/B readout needs most. A failed log never
// changes the response.
func (h *Handler) flushRefusedIntakeCall(ctx context.Context, w *workflowWrite) {
	if w == nil || w.refusedIntake == nil {
		return
	}
	c := *w.refusedIntake
	w.refusedIntake = nil
	if err := w.writeIntakeCall(ctx, h.Queries, c); err != nil {
		slog.Warn("intake_call activity not recorded", "issue_id", uuidToString(c.issue.ID), "err", err)
	}
}
