package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ErrAdmissionDeferred is returned by an enqueue path that did not start a
// reviewer run because the PR head is not admissible yet (CHE-4650). The
// deferral record is stored; handlers render it as 409 admission_deferred and
// background callers drop the trigger, which the record's later start replaces.
var ErrAdmissionDeferred = errors.New("the PR head is not admissible yet, so the reviewer run is deferred")

// admissionChecks is the explicit server list of CI checks a reviewer run waits
// for (MULTICA_ADMISSION_CHECKS, comma separated). Only listed checks count: a
// reviewer-dependent check such as security-gate is never listed, so a pending
// or missing one never defers the reviewer that has to turn it green. An empty
// list turns the guard off.
var admissionChecks = splitList(os.Getenv("MULTICA_ADMISSION_CHECKS"))

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// admissionDeferredError is ErrAdmissionDeferred for one (issue, agent): the
// refusal's record, with who asked (actor, trigger comment) and the taint the
// asker carried, so a later start is never cleaner than the refused run.
type admissionDeferredError struct {
	issueID, agentID pgtype.UUID
	actor, comment   pgtype.UUID
	trigger, head    string
	reasons, taint   []string
}

func (e *admissionDeferredError) Error() string {
	return ErrAdmissionDeferred.Error() + ": " + strings.Join(e.reasons, ", ")
}
func (*admissionDeferredError) Is(target error) bool { return target == ErrAdmissionDeferred }

// recordAdmissionDeferral stores the refusal. A failure is returned: a
// deferral that is not on record is a lost run.
func recordAdmissionDeferral(ctx context.Context, q *db.Queries, e *admissionDeferredError) error {
	if err := q.UpsertAdmissionDeferral(ctx, db.UpsertAdmissionDeferralParams{
		IssueID: e.issueID, AgentID: e.agentID, Trigger: e.trigger, HeadSha: e.head, Reasons: e.reasons,
		ActorUserID: e.actor, TriggerCommentID: e.comment, TaintIds: e.taint,
	}); err != nil {
		return fmt.Errorf("admission: record deferral: %w", err)
	}
	return nil
}

// checkReason classifies the runs of one configured check on the head: "" when
// it passed, else failing, pending or missing. Several runs of one name (a
// rerun, a status context beside a check run) resolve to the worst.
func checkReason(runs []db.ListAdmissionCheckRunsRow) string {
	if len(runs) == 0 {
		return "missing"
	}
	reason := ""
	for _, r := range runs {
		switch {
		case r.Status != "completed":
			if reason == "" {
				reason = "pending"
			}
		case r.Conclusion.String != "success" && r.Conclusion.String != "neutral" && r.Conclusion.String != "skipped":
			return "failing"
		}
	}
	return reason
}

// guardAdmission defers a reviewer run while the PR head is not admissible
// (CHE-4650): a configured admission check on the head is failing, pending or
// missing, or the head's merge state is DIRTY. It runs inside the enqueue
// transaction, after the budget guard and before the stage queue, so a
// deferred run never takes a stage slot. Only a reviewer run (the agent is the
// assignee of an issue in a review status) on an issue with exactly one open
// PR is judged. The deferral is stored; whoever starts the run later reads it.
func guardAdmission(ctx context.Context, q *db.Queries, issueID pgtype.UUID, target RunTarget) error {
	if len(admissionChecks) == 0 || !target.ReviewRun || !target.AgentID.Valid || !issueID.Valid {
		return nil
	}
	issue, err := q.GetIssue(ctx, issueID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("admission: load issue: %w", err)
	}
	if issue.AssigneeType.String != "agent" || issue.AssigneeID != target.AgentID || !IsReviewStatus(issue.Status) {
		return nil
	}
	prs, err := q.ListAdmissionPullRequests(ctx, issueID)
	if err != nil {
		return fmt.Errorf("admission: list pull requests: %w", err)
	}
	if len(prs) != 1 {
		return nil // no PR, or no single head to judge
	}
	pr := prs[0]
	runs, err := q.ListAdmissionCheckRuns(ctx, db.ListAdmissionCheckRunsParams{PrID: pr.ID, HeadSha: pr.HeadSha, Names: admissionChecks})
	if err != nil {
		return fmt.Errorf("admission: list check runs: %w", err)
	}
	byName := make(map[string][]db.ListAdmissionCheckRunsRow, len(admissionChecks))
	for _, r := range runs {
		byName[r.Name] = append(byName[r.Name], r)
	}
	var reasons []string
	for _, name := range admissionChecks {
		if r := checkReason(byName[name]); r != "" {
			reasons = append(reasons, "check:"+name+":"+r)
		}
	}
	if pr.MergeState == "DIRTY" {
		reasons = append(reasons, "merge_state:DIRTY")
	}
	if len(reasons) == 0 {
		return nil
	}
	// Fail closed: a requester whose taint cannot be read is never shown clean.
	taint, err := ActingTaskTaint(ctx, q)
	if err != nil {
		return fmt.Errorf("admission: read requester taint: %w", err)
	}
	if taint == nil {
		taint = []string{}
	}
	deferral := &admissionDeferredError{
		issueID: issueID, agentID: target.AgentID, actor: target.ActorUserID, comment: target.TriggerCommentID,
		trigger: target.Trigger, head: pr.HeadSha, reasons: reasons, taint: taint,
	}
	if err := recordAdmissionDeferral(ctx, q, deferral); err != nil {
		return err
	}
	slog.Info("admission: reviewer run deferred",
		"issue_id", util.UUIDToString(issueID), "agent_id", util.UUIDToString(target.AgentID), "head_sha", pr.HeadSha, "reasons", reasons)
	return deferral
}
