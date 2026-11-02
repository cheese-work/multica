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
)

// Review-loop guards (CHE-3243). A reviewer's CHANGES verdict is an agent's
// status write along the step's agent_verdict(CHANGES) exit. It must carry
// findings with a P0 or P1 among them, and it is counted: after maxChangesRounds
// recorded rounds the next one is a decision request instead of another round.

const (
	maxChangesRounds = 3

	activityChangesRequested = "review_changes_requested"
	activityRoundsExhausted  = "review_rounds_exhausted"
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

func writeReviewLoopError(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, errFindingsRequired) {
		return false
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "code": "review_verdict_findings_required"})
	return true
}

// guardChangesVerdict judges an agent's move off a step. For a CHANGES verdict
// it refuses findings without a P0/P1, and past the round limit it rewrites the
// target to need_decision. Any other move passes untouched.
func (h *Handler) guardChangesVerdict(ctx context.Context, q *db.Queries, prev db.Issue, params *db.UpdateIssueParams, def *issueworkflow.Definition, caller workflowCaller) error {
	if !caller.agent || !params.Status.Valid || params.Status.String == prev.Status || !def.IsChangesMove(prev.Status, params.Status.String) {
		return nil
	}
	if !blocking(caller.findings) {
		return errFindingsRequired
	}
	rounds, err := q.CountIssueActivityByAction(ctx, db.CountIssueActivityByActionParams{IssueID: prev.ID, Action: activityChangesRequested})
	if err != nil {
		return err
	}
	if rounds >= maxChangesRounds && def.Has(statusNeedDecision) {
		params.Status = pgtype.Text{String: statusNeedDecision, Valid: true}
	}
	return nil
}

// recordChangesVerdict leaves the verdict where the next write counts it: the
// round with its findings, or the refusal of one past the limit.
func (h *Handler) recordChangesVerdict(ctx context.Context, caller workflowCaller, prev, issue db.Issue, requested *string) {
	if !caller.agent || requested == nil || *requested == prev.Status || issue.Status == prev.Status {
		return
	}
	def, _, err := issueworkflow.ForIssue(ctx, h.Queries, prev.WorkspaceID, prev.WorkflowID, prev.ProjectID)
	if err != nil || def == nil || !def.IsChangesMove(prev.Status, *requested) {
		return
	}
	action := activityChangesRequested
	if issue.Status != *requested {
		action = activityRoundsExhausted
	}
	findings, _ := json.Marshal(caller.findings)
	agentID := ""
	if caller.run != nil {
		agentID = uuidToString(caller.run.AgentID)
	}
	h.recordReviewActivity(ctx, issue.ID, "agent", agentID, action, map[string]string{"findings": string(findings)})
}
