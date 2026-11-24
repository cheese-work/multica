package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// The review-round cap (CHE-6192) stops a review loop at its third round,
// whichever way the reviewer hands the work back. A status move into
// agent_review is counted in reviewRoundsCapped; a reviewer's wakeup to the
// author (agent_review step 4b) leaves the issue in agent_review, so it is
// counted here. Either way the issue goes to need_decision and one decision
// card carries the round count.

const (
	activityHandoffWakeup = "review_handoff_wakeup"
	// activityRoundsAutoSplit marks a plan issue's round limit that took the
	// Split path without a decision card (CHE-9483). The daily digest lists
	// these rows; a veto reopens the card for the capped round.
	activityRoundsAutoSplit = "review_rounds_auto_split"
	// reviewRoundsFingerprint prefixes the card's fingerprint; the cap's own
	// activity id follows, so each cap opens one card.
	reviewRoundsFingerprint = serverDecisionFingerprint + "review_rounds:"
)

// roundsCap is a cap a write recorded: how many rounds ran out and the
// activity that marks it.
type roundsCap struct {
	rounds     int
	activityID pgtype.UUID
}

// errReviewRoundsCapped refuses the wakeup that would have been the issue's
// third CHANGES round.
var errReviewRoundsCapped = errors.New("this was the third review round without a PASS: the platform did not wake the author, moved the issue to need_decision and opened one decision card; stop here")

// recordRoundsCap stores the cap marker, which restarts the count.
func recordRoundsCap(ctx context.Context, q *db.Queries, issue db.Issue, agentID pgtype.UUID) (pgtype.UUID, error) {
	row, err := q.CreateActivity(ctx, db.CreateActivityParams{
		ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ActorType: strToText("agent"),
		ActorID: agentID, Action: activityRoundsCapped, Details: []byte(`{}`),
	})
	return row.ID, err
}

// openCappedCard opens the card for a write that capped the issue, after its
// commit.
func (h *Handler) openCappedCard(ctx context.Context, issue db.Issue, wf *workflowWrite) {
	if wf == nil || wf.capped == nil {
		return
	}
	h.ensureReviewRoundsCard(ctx, issue, *wf.capped)
}

// isPlanIssueTitle reports whether the title is a plan issue's: the
// conventional-commit type "plan", with or without a scope.
func isPlanIssueTitle(title string) bool {
	t := strings.ToLower(strings.TrimSpace(title))
	if !strings.HasPrefix(t, "plan") {
		return false
	}
	rest := t[len("plan"):]
	return strings.HasPrefix(rest, "(") || strings.HasPrefix(rest, ":")
}

// autoSplitPlanIssue takes the Split path for a plan issue at the round limit
// with no decision request: it records the decision as an activity row and
// posts one system comment for the need_decision handler. The issue is already
// in need_decision, so the handler's step runs the split and moves it on.
func (h *Handler) autoSplitPlanIssue(ctx context.Context, issue db.Issue, c roundsCap) error {
	return h.withPgxTx(ctx, func(tx pgx.Tx) error {
		q := h.Queries.WithTx(tx)
		details, _ := json.Marshal(map[string]any{"rounds": c.rounds, "cap_activity_id": uuidToString(c.activityID)})
		if _, err := q.CreateActivity(ctx, db.CreateActivityParams{
			ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ActorType: strToText("system"),
			Action: activityRoundsAutoSplit, Details: details,
		}); err != nil {
			return err
		}
		_, err := q.CreateComment(ctx, db.CreateCommentParams{
			ID: dbid.NewV7(), IssueID: issue.ID, WorkspaceID: issue.WorkspaceID,
			AuthorType: "system", AuthorID: pgtype.UUID{Valid: true}, Type: "system",
			Content: fmt.Sprintf("Review round limit: this plan issue has had %d review rounds without a PASS. Decision recorded: auto-Split at round limit (Cheese, 24 Nov 2026, CHE-9483). No decision request was opened. Run the Split path now: one comment with the round count, the candidate hash and the open findings; one backlog issue for the removed work, read back; then move this issue to in_progress. The daily report lists this auto-Split; a veto reopens the decision card.", c.rounds),
		})
		return err
	})
}

const reviewRoundsCardAttempts = 3

// errNoCardRecipient means the issue has no owner or asking agent to ask.
var errNoCardRecipient = errors.New("review rounds: nobody to ask")

// ensureReviewRoundsCard opens the cap's card. The issue is already in
// need_decision, so a failed attempt is retried, and when every attempt fails
// the issue gets a system comment that says the card is missing: need_decision
// never lacks its card silently.
func (h *Handler) ensureReviewRoundsCard(ctx context.Context, issue db.Issue, c roundsCap) {
	if isPlanIssueTitle(issue.Title) {
		if err := h.autoSplitPlanIssue(ctx, issue, c); err != nil {
			slog.Error("review rounds: plan auto-split not recorded", "err", err, "issue_id", uuidToString(issue.ID))
		} else {
			return
		}
	}
	var err error
	for attempt := 1; attempt <= reviewRoundsCardAttempts; attempt++ {
		if err = h.openReviewRoundsCard(ctx, issue, c); err == nil || errors.Is(err, errNoCardRecipient) {
			break
		}
		slog.Warn("review rounds: decision card not opened", "err", err, "attempt", attempt, "issue_id", uuidToString(issue.ID))
	}
	if err == nil {
		return
	}
	slog.Error("review rounds: decision card missing", "err", err, "issue_id", uuidToString(issue.ID))
	if _, cerr := h.Queries.CreateComment(ctx, db.CreateCommentParams{
		ID: dbid.NewV7(), IssueID: issue.ID, WorkspaceID: issue.WorkspaceID,
		AuthorType: "system", AuthorID: pgtype.UUID{Valid: true}, Type: "system",
		Content: fmt.Sprintf("Review round limit: this issue has had %d review rounds without a PASS and is in need_decision, but the platform could not open its decision card. The issue owner decides: split the work or accept the current head.", c.rounds),
	}); cerr != nil {
		slog.Error("review rounds: missing-card notice not posted", "err", cerr, "issue_id", uuidToString(issue.ID))
	}
}

// openReviewRoundsCard opens the cap's decision request to the issue's owner.
func (h *Handler) openReviewRoundsCard(ctx context.Context, issue db.Issue, c roundsCap) error {
	var results []decisionRequestResult
	err := h.withPgxTx(ctx, func(tx pgx.Tx) error {
		q := h.Queries.WithTx(tx)
		recipients, err := runBudgetRecipients(ctx, q, issue)
		if err != nil {
			return err
		}
		owners, err := runBudgetCardRecipients(ctx, q, issue, recipients)
		if err != nil {
			return err
		}
		plan, err := h.planRunBudgetDecisions(ctx, q, issue, owners)
		if err != nil {
			return err
		}
		if len(plan.lanes) == 0 {
			return errNoCardRecipient
		}
		// The comment takes the issue lock, after the decision locks.
		notice, err := q.CreateComment(ctx, db.CreateCommentParams{
			ID: dbid.NewV7(), IssueID: issue.ID, WorkspaceID: issue.WorkspaceID,
			AuthorType: "system", AuthorID: pgtype.UUID{Valid: true}, Type: "system",
			Content: fmt.Sprintf("Review round limit: this issue has had %d review rounds without a PASS. The platform did not start another round and moved the issue to need_decision. One decision request asks: split the work or accept the current head.", c.rounds),
		})
		if err != nil {
			return err
		}
		ask := reviewRoundsAsk(issue, c.rounds)
		ask.SourceCommentID = uuidToString(notice.ID)
		results, err = h.openRunBudgetAsk(ctx, q, issue, ask, reviewRoundsFingerprint+uuidToString(c.activityID), plan)
		return err
	})
	if err != nil {
		return err
	}
	h.publishRunBudgetDecisions(issue, results)
	return nil
}

func reviewRoundsAsk(issue db.Issue, rounds int) decisiontx.Ask {
	return decisiontx.Ask{
		ContextELI10:  fmt.Sprintf("%q has had %d review rounds without a PASS. The platform stopped the loop and moved the issue to Need Decision.", issue.Title, rounds),
		AgentDid:      fmt.Sprintf("The reviewer sent the work back %d times (%d review rounds). The platform refused a further round instead of letting the loop run.", rounds, rounds),
		Decision:      "Do you split the work into smaller issues, or accept the current head?",
		StakesIfWrong: "If you accept a head the reviewer rejected, a known defect can ship. If you split work that was nearly done, you add issues and runs for little gain.",
		Options: []decisiontx.AskOption{
			{Label: "Split the work", WhatHappens: "The issue is split into smaller issues, each with its own review rounds. The open findings move to the issue they belong to.", Pros: []string{"Each part gets a fresh round count", "The reviewer's findings stay open"}, Cons: []string{"More issues to track"}, CostOrTime: "A few new issues and their runs"},
			{Label: "Accept the current head", WhatHappens: "You accept the PR as it stands. The open findings are recorded as accepted risk and the issue goes on.", Pros: []string{"No more review runs"}, Cons: []string{"The findings stay in the code"}, CostOrTime: "Free"},
		},
		Recommendation:   "Read the last verdict. If the findings repeat, split the work; if they are small, accept the head.",
		DefaultIfNoReply: "No action. The issue stays in Need Decision and nothing runs on it.",
		SourceIssueID:    uuidToString(issue.ID),
	}
}

// reviewHandoffWakeup judges a new wakeup an agent makes: whether it is a
// reviewer's CHANGES handoff, a run at agent_review waking another agent on
// the issue that sits in agent_review. It returns the run, and false for any
// other wakeup.
func (h *Handler) reviewHandoffWakeup(ctx context.Context, issue db.Issue, source pgtype.UUID, in service.WakeupInput) (db.AgentTaskQueue, bool) {
	if !source.Valid || issue.Status != statusAgentReview {
		return db.AgentTaskQueue{}, false
	}
	run, err := h.Queries.GetAgentTask(ctx, source)
	if err != nil || run.IssueID != issue.ID || run.WorkflowStep.String != statusAgentReview || in.AgentID == uuidToString(run.AgentID) {
		return db.AgentTaskQueue{}, false
	}
	def, _, err := issueworkflow.ForIssue(ctx, h.Queries, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
	if err != nil || def == nil || !def.Has(statusAgentReview) || !def.Has(statusNeedDecision) {
		return db.AgentTaskQueue{}, false
	}
	return run, true
}

// capReviewHandoff counts a reviewer's wakeup as a CHANGES round. A round
// before the third returns false and the wakeup goes on. The third is refused:
// the issue moves to need_decision, the card opens, and the caller answers 409.
func (h *Handler) capReviewHandoff(ctx context.Context, issue db.Issue, run db.AgentTaskQueue) (bool, error) {
	prID, _ := runReviewHead(&run)
	prior, err := h.Queries.CountChangesRoundsForPR(ctx, db.CountChangesRoundsForPRParams{IssueID: issue.ID, PullRequestID: prID})
	if err != nil || prior+1 < maxChangesRounds {
		return false, err
	}
	var capID pgtype.UUID
	moved, _, err := h.tryMoveIssueOnEdgeIf(ctx, issue, statusNeedDecision, "review_rounds", nil, false, func(ctx context.Context, q *db.Queries) (bool, error) {
		var err error
		capID, err = recordRoundsCap(ctx, q, issue, run.AgentID)
		return err == nil, err
	})
	if err != nil || !moved {
		return false, err
	}
	if after, err := h.Queries.GetIssue(ctx, issue.ID); err == nil {
		issue = after
	}
	h.ensureReviewRoundsCard(ctx, issue, roundsCap{rounds: int(prior) + 1, activityID: capID})
	return true, nil
}

// recordReviewHandoff counts the wakeup the reviewer made.
func (h *Handler) recordReviewHandoff(ctx context.Context, issue db.Issue, run db.AgentTaskQueue, wakeup db.IssueWakeup) {
	prID, head := runReviewHead(&run)
	details, _ := json.Marshal(map[string]string{"wakeup_id": uuidToString(wakeup.ID), "agent_id": uuidToString(wakeup.AgentID), "pull_request_id": prID, "head_sha": head})
	if _, err := h.Queries.CreateActivity(ctx, db.CreateActivityParams{
		ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ActorType: strToText("agent"),
		ActorID: run.AgentID, Action: activityHandoffWakeup, Details: details,
	}); err != nil {
		slog.Warn("review rounds: handoff not recorded", "err", err, "issue_id", uuidToString(issue.ID))
	}
}

func writeReviewRoundsCapped(w http.ResponseWriter) {
	writeJSON(w, http.StatusConflict, map[string]any{"error": errReviewRoundsCapped.Error(), "code": "review_rounds_capped"})
}
