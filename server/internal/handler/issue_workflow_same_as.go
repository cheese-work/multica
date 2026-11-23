package handler

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The same_as handler (CHE-9114) hands an issue to the agent that last handled
// another step on it: the issue's current assignee when it sits in that step,
// else the assignee it had when it last left the step (issue_step_assignee
// under issueworkflow.LeftStepKey). An agent that is archived or gone counts
// as not found. Then the step's own agent id applies, then the named step's
// own handler, so a step never lacks an assignee for want of history.
//
// A step that names agent_review is the exception (CHE-9318): it hands the
// issue to the agent that signed the PASS for the exact head, with no history
// fallback; see passSignerHumans.

// sameAsHumans fills Humans.SameAs for a same_as step. A failed read is an
// error, so the write fails instead of handing the issue to the wrong agent.
func (h *Handler) sameAsHumans(ctx context.Context, q *db.Queries, step issueworkflow.Step, project db.Project, workspaceID, requestedByID pgtype.UUID, subject workflowSubject) (issueworkflow.Humans, error) {
	if bound, err := bindsPassSigner(ctx, q, step, subject); err != nil {
		return issueworkflow.Humans{}, err
	} else if bound {
		return passSignerHumans(ctx, q, workspaceID, subject)
	}
	handled, err := h.lastHandler(ctx, q, workspaceID, subject, step.Handler.Step)
	if err != nil {
		return issueworkflow.Humans{}, err
	}
	if handled != nil {
		return issueworkflow.Humans{SameAsType: handled.Type, SameAsID: handled.ID}, nil
	}
	if step.Handler.ID != "" {
		return issueworkflow.Humans{}, nil // ResolveHandler uses the step's own agent
	}
	def, _, err := issueworkflow.ForIssue(ctx, q, workspaceID, subject.WorkflowID, subject.projectOr(project))
	if err != nil || def == nil {
		return issueworkflow.Humans{}, err
	}
	named, ok := def.Step(step.Handler.Step)
	if !ok || !named.HandsOff() {
		return issueworkflow.Humans{}, nil
	}
	humans, err := h.workflowHumans(ctx, q, named, project, workspaceID, requestedByID, subject)
	if err != nil {
		return issueworkflow.Humans{}, err
	}
	if named.Handler.Type == issueworkflow.HandlerReviewer {
		return h.sameAsReviewer(ctx, q, workspaceID, subject)
	}
	typ, id, ok := issueworkflow.ResolveHandler(named, project, "", pgtype.UUID{}, humans)
	if !ok {
		return issueworkflow.Humans{}, nil
	}
	return issueworkflow.Humans{SameAsType: typ, SameAsID: id}, nil
}

// sameAsReviewer resolves a same_as step whose named step is a reviewer step
// by the same routed pick as that step, so the table's reviewer is not dropped
// for a routed type (CHE-9318). A refusal is returned, not hidden. A subject
// with no issue yet has no authors to judge a reviewer against.
func (h *Handler) sameAsReviewer(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, subject workflowSubject) (issueworkflow.Humans, error) {
	if !subject.ID.Valid {
		return issueworkflow.Humans{}, nil
	}
	guard, err := h.loadReviewerGuard(ctx, q, subject.ID)
	if err != nil {
		return issueworkflow.Humans{}, err
	}
	picked, ok, refusal := h.pickReviewer(ctx, q, workspaceID, subject.Title, guard)
	if !ok {
		return issueworkflow.Humans{}, refusal
	}
	return issueworkflow.Humans{SameAsType: issueworkflow.HandlerAgent, SameAsID: picked}, nil
}

// passSignerRuleSince is when the PASS-signer rule starts (CHE-9318). An issue
// that left agent_review before it keeps the route it had until it closes. Set
// it to the deploy time when the rule ships.
var passSignerRuleSince = time.Date(2026, time.November, 24, 0, 0, 0, 0, time.UTC)

// bindsPassSigner reports whether a step hands the issue to the PASS signer:
// a same_as step that names agent_review, for an issue that is in agent_review
// or did not leave it before passSignerRuleSince. The cohort rests on the last
// exit from agent_review, so later moves never change it; an issue that
// re-enters agent_review after the rule starts leaves it again after the
// cutoff and joins the new rule.
func bindsPassSigner(ctx context.Context, q *db.Queries, step issueworkflow.Step, subject workflowSubject) (bool, error) {
	if step.Handler.Type != issueworkflow.HandlerSameAs || step.Handler.Step != statusAgentReview || !subject.ID.Valid {
		return false, nil
	}
	if subject.Status == statusAgentReview || subject.PassSigner.Valid {
		return true, nil
	}
	left, err := q.GetIssueLastReviewExit(ctx, subject.ID)
	if err != nil {
		return false, err
	}
	return !left.Valid || !left.Time.Before(passSignerRuleSince), nil
}

// runHeadIsLinkedHead reports whether the run reviewed a head that every
// linked pull request stands at.
func runHeadIsLinkedHead(ctx context.Context, q *db.Queries, issueID pgtype.UUID, run *db.AgentTaskQueue) (bool, error) {
	_, head := runReviewHead(run)
	return linkedHeadsAre(ctx, q, issueID, head)
}

// linkedHeadsAre reports whether head is non-empty and is the head of every
// pull request linked to the issue.
func linkedHeadsAre(ctx context.Context, q *db.Queries, issueID pgtype.UUID, head string) (bool, error) {
	prs, err := q.ListIssuePullRequestHeads(ctx, issueID)
	if err != nil {
		return false, err
	}
	for _, pr := range prs {
		if head == "" || pr.HeadSha != head {
			return false, nil
		}
	}
	return true, nil
}

// passSignerError says a same_as step that names agent_review found no PASS
// for the issue's exact head to take the signer from. The write recovers
// through blocked, not through another reviewer.
type passSignerError struct{ Reason string }

func (e *passSignerError) Error() string { return "no PASS signer to hand the issue to: " + e.Reason }

// passSignerHumans fills Humans.SameAs with the agent that signed the PASS for
// the issue's exact head (CHE-9318): the agent signing it in this very write,
// else (when the write is no PASS) the agent of the newest verdict row when that is a PASS with no P0 or
// P1 finding, nothing voided it, and it names the head of every linked PR.
// The author never merges, since the signer reviewed it. Anything else is a
// passSignerError, never the last agent on record.
func passSignerHumans(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, subject workflowSubject) (issueworkflow.Humans, error) {
	signer := subject.PassSigner
	if subject.PassRunOffHead {
		return issueworkflow.Humans{}, &passSignerError{Reason: "the PASS run did not review the exact head of the linked pull request"}
	}
	if !signer.Valid {
		var err error
		if signer, err = recordedPassSigner(ctx, q, subject.ID); err != nil {
			return issueworkflow.Humans{}, err
		}
	}
	if !signer.Valid {
		return issueworkflow.Humans{}, &passSignerError{Reason: "no PASS signed for the exact head of the linked pull request"}
	}
	agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: signer, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && agent.ArchivedAt.Valid) {
		return issueworkflow.Humans{}, &passSignerError{Reason: "the agent that signed the PASS is archived or gone"}
	}
	if err != nil {
		return issueworkflow.Humans{}, err
	}
	return issueworkflow.Humans{SameAsType: issueworkflow.HandlerAgent, SameAsID: signer}, nil
}

// recordedPassSigner is the actor of the issue's newest verdict row when it is
// a standing PASS for the exact head; invalid when none stands.
func recordedPassSigner(ctx context.Context, q *db.Queries, issueID pgtype.UUID) (pgtype.UUID, error) {
	verdict, err := q.GetLatestIssueVerdict(ctx, issueID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (verdict.Action != activityPassed || !verdict.ActorID.Valid)) {
		return pgtype.UUID{}, nil
	}
	if err != nil {
		return pgtype.UUID{}, err
	}
	var details struct {
		Findings string `json:"findings"`
		HeadSHA  string `json:"head_sha"`
	}
	var findings []ReviewFinding
	if json.Unmarshal(verdict.Details, &details) != nil || json.Unmarshal([]byte(details.Findings), &findings) != nil {
		return pgtype.UUID{}, nil
	}
	if slices.ContainsFunc(findings, func(f ReviewFinding) bool { return f.Severity == "P0" || f.Severity == "P1" }) {
		return pgtype.UUID{}, nil
	}
	voided, err := q.CountIssueVerdictBreaksSince(ctx, db.CountIssueVerdictBreaksSinceParams{IssueID: issueID, Since: verdict.CreatedAt})
	if err != nil || voided > 0 {
		return pgtype.UUID{}, err
	}
	if ok, err := linkedHeadsAre(ctx, q, issueID, details.HeadSHA); err != nil || !ok {
		return pgtype.UUID{}, err
	}
	return verdict.ActorID, nil
}

// lastHandler is the live agent that last handled a status on the issue; nil
// when none is on record or it can no longer take work.
func (h *Handler) lastHandler(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, subject workflowSubject, status string) (*issueworkflow.StepAssignee, error) {
	var found *issueworkflow.StepAssignee
	switch {
	case !subject.ID.Valid:
		return nil, nil
	case subject.Status == status && subject.AssigneeID.Valid:
		found = &issueworkflow.StepAssignee{Type: subject.AssigneeType, ID: subject.AssigneeID}
	default:
		var err error
		if found, err = issueworkflow.StickyStepAssignee(ctx, q, workspaceID, subject.ID, issueworkflow.LeftStepKey(status)); err != nil {
			return nil, err
		}
	}
	if found == nil || found.Type != issueworkflow.HandlerAgent {
		return nil, nil
	}
	agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: found.ID, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && agent.ArchivedAt.Valid) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return found, nil
}

// recordLeft keeps who an issue had when it leaves a status, in the
// transaction that moves it, for a same_as step to read later. Only an agent
// is kept, the only assignee a same_as step can resolve to, and only when a
// step of the workflow follows the status.
func (w *workflowWrite) recordLeft(ctx context.Context, q *db.Queries, moved db.Issue) error {
	if w == nil || moved.Status == w.prev.Status || !w.prev.AssigneeID.Valid || w.prev.AssigneeType.String != issueworkflow.HandlerAgent {
		return nil
	}
	def, _, err := issueworkflow.ForIssue(ctx, q, moved.WorkspaceID, moved.WorkflowID, moved.ProjectID)
	if err != nil || def == nil {
		return err
	}
	if !slices.ContainsFunc(def.Steps, func(s issueworkflow.Step) bool {
		return s.Handler.Type == issueworkflow.HandlerSameAs && s.Handler.Step == w.prev.Status
	}) {
		return nil
	}
	var setBy pgtype.UUID
	if id, err := util.ParseUUID(w.entry.actorID); err == nil {
		setBy = id
	}
	left := issueworkflow.StepAssignee{Type: w.prev.AssigneeType.String, ID: w.prev.AssigneeID}
	return issueworkflow.SetStepAssignee(ctx, q, moved.WorkspaceID, moved.ID, issueworkflow.LeftStepKey(w.prev.Status), left, w.entry.actorType, setBy)
}
