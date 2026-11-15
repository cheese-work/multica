package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
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

func writeReviewLoopError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, errFindingsRequired):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "code": "review_verdict_findings_required"})
	case errors.Is(err, errVerdictNeedsRun):
		writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error(), "code": "review_verdict_run_required"})
	default:
		return false
	}
	return true
}

// verdictOf is the verdict an agent's write delivers: an explicit move along
// the step's agent_verdict exit ("CHANGES", "PASS"), "" for any other write.
func verdictOf(def *issueworkflow.Definition, from string, to pgtype.Text, caller workflowCaller) string {
	if !caller.agent || !to.Valid || to.String == from {
		return ""
	}
	return def.VerdictOf(from, to.String)
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
	verdict := verdictOf(def, prev.Status, params.Status, caller)
	if verdict == "" {
		return nil
	}
	if caller.run == nil {
		return errVerdictNeedsRun
	}
	if verdict != "CHANGES" {
		return nil
	}
	if !blocking(caller.findings) {
		return errFindingsRequired
	}
	prID, _ := runReviewHead(caller.run)
	earlier, err := q.CountChangesVerdictsForPR(ctx, db.CountChangesVerdictsForPRParams{IssueID: prev.ID, PullRequestID: prID})
	if err != nil {
		return err
	}
	if earlier+1 >= maxChangesRounds && def.Has(statusNeedDecision) {
		params.Status = pgtype.Text{String: statusNeedDecision, Valid: true}
	}
	return nil
}

// recordVerdict stores an accepted verdict with its findings, PR and head, in
// the transaction that moves the issue: a CHANGES round is counted exactly when
// the move commits, and the head of the last verdict, PASS or CHANGES, is what
// a later review is skipped on.
func (w *workflowWrite) recordVerdict(ctx context.Context, q *db.Queries, moved db.Issue) error {
	if w == nil || !w.statusExplicit || moved.Status == w.prev.Status {
		return nil
	}
	prev := w.prev
	def, _, err := issueworkflow.ForIssue(ctx, q, prev.WorkspaceID, prev.WorkflowID, w.pre.ProjectID)
	if err != nil || def == nil {
		return err
	}
	action := map[string]string{"CHANGES": activityChangesRequested, "PASS": activityPassed}[verdictOf(def, prev.Status, w.pre.Status, w.caller)]
	if action == "" {
		return nil
	}
	prID, head := runReviewHead(w.caller.run)
	findings, _ := json.Marshal(w.caller.findings)
	details, _ := json.Marshal(map[string]string{"findings": string(findings), "pull_request_id": prID, "head_sha": head})
	_, err = q.CreateActivity(ctx, db.CreateActivityParams{
		ID:          dbid.NewV7(),
		WorkspaceID: prev.WorkspaceID,
		IssueID:     prev.ID,
		ActorType:   strToText("agent"),
		ActorID:     w.caller.run.AgentID,
		Action:      action,
		Details:     details,
	})
	return err
}
