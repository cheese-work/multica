package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/designguard"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// Review-loop guards (CHE-3243). A reviewer's CHANGES verdict is an agent's
// status write along the step's agent_verdict(CHANGES) exit. It must carry
// findings with a P0 or P1 among them, and it is counted: after maxChangesRounds
// recorded rounds the next one is a decision request instead of another round.

const (
	maxChangesRounds = 3

	activityChangesRequested = "review_changes_requested"
	activityPassed           = "review_passed"
	activityRoundsCapped     = "review_rounds_capped"
	statusAgentReview        = "agent_review"
	statusNeedDecision       = "need_decision"
)

// ReviewFinding is one finding of a review verdict.
type ReviewFinding struct {
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
}

// errFindingsRequired refuses a CHANGES verdict without a blocking finding.
var errFindingsRequired = errors.New("a changes-requested verdict needs review_findings with at least one P0 or P1 finding (severity P0-P3); lower findings alone do not send work back")

// blocking reports whether the findings justify sending the work back: every
// severity is valid and one of them is P0 or P1.
func blocking(findings []ReviewFinding) bool {
	found := false
	for _, f := range findings {
		if !slices.Contains([]string{"P0", "P1", "P2", "P3"}, f.Severity) {
			return false
		}
		found = found || f.Severity == "P0" || f.Severity == "P1"
	}
	return found
}

// errVerdictNeedsRun refuses a verdict from an agent without a run on this
// issue: such a write names no reviewed PR or head, so it could not be counted
// or skipped on.
var errVerdictNeedsRun = errors.New("a review verdict must come from the agent's run on this issue (X-Task-ID of a run on this issue); a run of another issue names no reviewed PR or head")

// reviewIndependenceError refuses a PASS verdict whose reviewer is not
// independent of the issue's authors. code is the designguard A-code.
type reviewIndependenceError struct{ code string }

func (e *reviewIndependenceError) Error() string {
	return "the reviewer is not independent of the issue's authors (" + e.code + "); a verdict on a reviewer step needs a reviewer of another lab that did not author this issue"
}

func writeReviewLoopError(w http.ResponseWriter, err error) bool {
	var independence *reviewIndependenceError
	switch {
	case errors.As(err, &independence):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": independence.code})
	case errors.Is(err, errFindingsRequired):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "code": "review_verdict_findings_required"})
	case errors.Is(err, errVerdictNeedsRun):
		writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error(), "code": "review_verdict_run_required"})
	default:
		return false
	}
	return true
}

// explicitAgentMove reports whether the write is an agent's move off a step, the
// only write that can deliver a verdict.
func explicitAgentMove(from string, to pgtype.Text, caller workflowCaller) bool {
	return caller.agent && to.Valid && to.String != from
}

// verdictOf is the verdict a move from a step to a status delivers: "CHANGES",
// "PASS", or "" when no agent_verdict exit leads there. When both exits lead to
// the same status, the findings decide: CHANGES with a P0 or P1, else PASS. The
// guard and the record both resolve through it, with the same findings.
func verdictOf(def *issueworkflow.Definition, from, to string, findings []ReviewFinding) string {
	var pass, changes bool
	for _, e := range def.EdgesOf(from) {
		if g, err := issueworkflow.ParseGuard(e.Guard); err == nil && g.Kind == issueworkflow.GuardAgentVerdict && e.To == to {
			pass = pass || g.Args[0] == "PASS"
			changes = changes || g.Args[0] == "CHANGES"
		}
	}
	if pass && changes {
		if blocking(findings) {
			return "CHANGES"
		}
		return "PASS"
	}
	return def.VerdictOf(from, to)
}

// runReviewHead is the PR lineage and head the verdict's review run was
// dispatched for; both are empty for a run without a PR.
func runReviewHead(run *db.AgentTaskQueue) (prID, head string) {
	if run == nil {
		return "", ""
	}
	var ctxJSON struct {
		HeadSHA string `json:"head_sha"`
	}
	_ = json.Unmarshal(run.Context, &ctxJSON)
	return uuidToString(run.PullRequestID), ctxJSON.HeadSHA
}

// guardChangesVerdict judges an agent's move off a step. For a CHANGES verdict
// it refuses findings without a P0/P1, and when this verdict is the
// maxChangesRounds-th round of the run's PR it rewrites the target to
// need_decision. Any other move passes untouched. It runs again under the
// issue row lock, so concurrent verdicts count one after the other.
func (h *Handler) guardChangesVerdict(ctx context.Context, q *db.Queries, prev db.Issue, params *db.UpdateIssueParams, def *issueworkflow.Definition, caller workflowCaller) error {
	if capped, err := h.reviewRoundsCapped(ctx, q, prev, params, def, caller); err != nil || capped {
		return err
	}
	if !explicitAgentMove(prev.Status, params.Status, caller) {
		return nil
	}
	verdict := verdictOf(def, prev.Status, params.Status.String, caller.findings)
	if verdict == "" {
		return nil
	}
	if caller.run == nil {
		return errVerdictNeedsRun
	}
	if verdict == "PASS" {
		return h.guardReviewerIndependence(ctx, q, prev, def, caller)
	}
	if verdict != "CHANGES" {
		return nil
	}
	if !blocking(caller.findings) {
		return errFindingsRequired
	}
	prID, _ := runReviewHead(caller.run)
	earlier, err := q.CountChangesRoundsForPR(ctx, db.CountChangesRoundsForPRParams{IssueID: prev.ID, PullRequestID: prID})
	if err != nil {
		return err
	}
	if earlier+1 >= maxChangesRounds && def.Has(statusNeedDecision) {
		params.Status = pgtype.Text{String: statusNeedDecision, Valid: true}
	}
	return nil
}

// guardReviewerIndependence refuses a PASS from a step with a reviewer handler
// unless the reviewer's run passes designguard A1 to A5 against every author
// of the issue (CHE-7658 step 14). Authors come from the workflow_role
// evidence of the issue's tasks, never from the current assignee, so a CHANGES
// round or a reassignment cannot drop the first author. It runs before the
// verdict row, so a refusal writes none.
func (h *Handler) guardReviewerIndependence(ctx context.Context, q *db.Queries, prev db.Issue, def *issueworkflow.Definition, caller workflowCaller) error {
	if step, ok := def.Step(prev.Status); !ok || step.Handler.Type != issueworkflow.HandlerReviewer {
		return nil
	}
	authors, err := service.IssueAuthors(ctx, q, prev.ID, voteLab)
	if err != nil {
		return err
	}
	tasks := make([]designguard.Task, 0, len(authors)+1)
	view := designguard.View{ProvenanceComplete: true}
	for _, a := range authors {
		view.Authors = append(view.Authors, designguard.Author{Kind: "agent", AgentID: a.AgentID, TaskID: a.TaskID})
		tasks = append(tasks, designguard.Task{ID: a.TaskID, AgentID: a.AgentID, Provider: a.Provider, Model: a.Model})
	}
	if len(authors) == 0 {
		// No agent task authored the issue: a person did.
		view.Authors = []designguard.Author{{Kind: "user"}}
	}
	run := caller.run
	reviewer := designguard.Reviewer{AgentID: uuidToString(run.AgentID), TaskID: uuidToString(run.ID)}
	provider := ""
	if run.RuntimeID.Valid {
		rt, err := q.GetAgentRuntime(ctx, run.RuntimeID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		provider = rt.Provider
	}
	tasks = append(tasks, designguard.Task{ID: reviewer.TaskID, AgentID: reviewer.AgentID, Provider: provider, Model: run.AppliedModel.String})
	code, err := designguard.CheckIndependence(ctx, view, reviewer, "", designguard.Deps{
		Tasks: func(context.Context, []string) ([]designguard.Task, error) { return tasks, nil },
		Lab:   voteLab,
	})
	if err != nil {
		return err
	}
	if code != "" {
		return &reviewIndependenceError{code: code}
	}
	return nil
}

// reviewRoundsCapped sends an agent's entry into agent_review to need_decision
// once the issue has had maxChangesRounds reviews without a PASS (CHE-6192). It
// counts entries, not CHANGES verdicts, so a reviewer who leaves through Blocked
// or a wakeup cannot keep the loop going unseen.
func (h *Handler) reviewRoundsCapped(ctx context.Context, q *db.Queries, prev db.Issue, params *db.UpdateIssueParams, def *issueworkflow.Definition, caller workflowCaller) (bool, error) {
	if !caller.agent || !params.Status.Valid || params.Status.String != statusAgentReview || prev.Status == statusAgentReview ||
		!def.Has(statusAgentReview) || !def.Has(statusNeedDecision) {
		return false, nil
	}
	rounds, err := q.CountReviewEntriesSinceLastVerdict(ctx, prev.ID)
	if err != nil || rounds < maxChangesRounds {
		return false, err
	}
	params.Status = pgtype.Text{String: statusNeedDecision, Valid: true}
	return true, nil
}

// recordRoundsCapped stores the cap, which restarts the count. The decision
// card opens after the commit (openCappedCard): its locks come before the issue
// row lock this write already holds.
func (w *workflowWrite) recordRoundsCapped(ctx context.Context, q *db.Queries, moved db.Issue) error {
	rounds, err := q.CountReviewEntriesSinceLastVerdict(ctx, moved.ID)
	if err != nil {
		return err
	}
	id, err := recordRoundsCap(ctx, q, moved, w.caller.run.AgentID)
	w.capped = &roundsCap{rounds: int(rounds), activityID: id}
	return err
}

// recordVerdict stores an accepted verdict with its findings, PR and head, in
// the transaction that moves the issue: a CHANGES round is counted exactly when
// the move commits, and the head of the last verdict, PASS or CHANGES, is what
// a later review is skipped on.
func (w *workflowWrite) recordVerdict(ctx context.Context, q *db.Queries, moved db.Issue) error {
	if w == nil || !w.statusExplicit || moved.Status == w.prev.Status {
		return nil
	}
	if w.caller.run != nil && w.pre.Status.String == statusAgentReview && moved.Status == statusNeedDecision {
		return w.recordRoundsCapped(ctx, q, moved)
	}
	prev := w.prev
	def, _, err := issueworkflow.ForIssue(ctx, q, prev.WorkspaceID, prev.WorkflowID, w.pre.ProjectID)
	if err != nil || def == nil {
		return err
	}
	if !explicitAgentMove(prev.Status, w.pre.Status, w.caller) {
		return nil
	}
	action := map[string]string{"CHANGES": activityChangesRequested, "PASS": activityPassed}[verdictOf(def, prev.Status, w.pre.Status.String, w.caller.findings)]
	if action == "" || (action == activityPassed && w.handoff != nil && w.handoff.signerMissing != "") {
		// A PASS the workflow could not route to its signer stands for
		// nothing: recording it would make this agent the latest signer while
		// the issue sits in blocked (CHE-9318).
		return nil
	}
	prID, head := runReviewHead(w.caller.run)
	findings, _ := json.Marshal(w.caller.findings)
	details, _ := json.Marshal(map[string]string{"findings": string(findings), "pull_request_id": prID, "head_sha": head})
	if _, err = q.CreateActivity(ctx, db.CreateActivityParams{
		ID:          dbid.NewV7(),
		WorkspaceID: prev.WorkspaceID,
		IssueID:     prev.ID,
		ActorType:   strToText("agent"),
		ActorID:     w.caller.run.AgentID,
		Action:      action,
		Details:     details,
	}); err != nil || action != activityChangesRequested || moved.Status != statusNeedDecision || w.pre.Status.String == statusNeedDecision {
		return err
	}
	// The guard rewrote this CHANGES verdict to need_decision: it was the
	// maxChangesRounds-th round of the PR. Record the cap so the count restarts
	// and the card opens after the commit.
	rounds, err := q.CountChangesRoundsForPR(ctx, db.CountChangesRoundsForPRParams{IssueID: prev.ID, PullRequestID: prID})
	if err != nil {
		return err
	}
	id, err := recordRoundsCap(ctx, q, moved, w.caller.run.AgentID)
	w.capped = &roundsCap{rounds: int(rounds), activityID: id}
	return err
}
