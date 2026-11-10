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

// ErrAdmissionDeferred is returned when a reviewer run is held back because the
// PR head is not admissible yet (CHE-4650): an admission check is failing,
// pending or missing, or the PR has a merge conflict. The deferral is on
// record for 24 h; handlers render it as 409 admission_deferred and background
// callers drop the trigger.
var ErrAdmissionDeferred = errors.New("the PR head is not admissible, so the review is deferred")

// reviewerDependentChecks wait on a reviewer, so a reviewer run can never wait
// on them. They are dropped from the admission list even when it names them.
var reviewerDependentChecks = map[string]bool{"security-gate": true}

// admissionEnabled is the one switch for deferral (MULTICA_REVIEW_ADMISSION,
// "true" or "1", read once at start). Default off: a reviewer run starts as
// before. Turn it on only once CHE-4651 (start the deferred run) is deployed;
// alone, a deferred review would wait for the 24 h expiry.
var admissionEnabled = admissionEnabledFromEnv(os.Getenv)

func admissionEnabledFromEnv(getenv func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv("MULTICA_REVIEW_ADMISSION"))) {
	case "true", "1":
		return true
	}
	return false
}

// admissionChecks is the explicit list of checks that must be green on a PR
// head before a reviewer run starts (MULTICA_REVIEW_ADMISSION_CHECKS, comma
// separated, read once at start). Empty: only a merge conflict defers.
var admissionChecks = admissionChecksFromEnv(os.Getenv)

func admissionChecksFromEnv(getenv func(string) string) []string {
	var names []string
	for _, n := range strings.Split(getenv("MULTICA_REVIEW_ADMISSION_CHECKS"), ",") {
		if n = strings.TrimSpace(n); n != "" && !reviewerDependentChecks[n] {
			names = append(names, n)
		}
	}
	return names
}

// admissionReviewers is the explicit set of reviewer agents the guard judges
// (MULTICA_REVIEW_ADMISSION_REVIEWERS, comma-separated agent IDs, read once at
// start). Empty: no agent is judged.
var admissionReviewers = admissionReviewersFromEnv(os.Getenv)

func admissionReviewersFromEnv(getenv func(string) string) map[string]bool {
	set := map[string]bool{}
	for _, id := range strings.Split(getenv("MULTICA_REVIEW_ADMISSION_REVIEWERS"), ",") {
		if id = strings.ToLower(strings.TrimSpace(id)); id != "" {
			set[id] = true
		}
	}
	return set
}

// admissionDeferredError is ErrAdmissionDeferred for one (issue, agent): the
// record that must be written down. errors.Is(err, ErrAdmissionDeferred) holds.
type admissionDeferredError struct {
	issueID, agentID pgtype.UUID
	actor, comment   pgtype.UUID
	trigger, head    string
	reasons          []string
	taint            []string
}

func (*admissionDeferredError) Error() string        { return ErrAdmissionDeferred.Error() }
func (*admissionDeferredError) Is(target error) bool { return target == ErrAdmissionDeferred }

func markAdmissionDeferred(ctx context.Context, q *db.Queries, d *admissionDeferredError) error {
	taint := d.taint
	if taint == nil {
		taint = []string{}
	}
	if err := q.MarkReviewAdmissionDeferred(ctx, db.MarkReviewAdmissionDeferredParams{
		IssueID: d.issueID, AgentID: d.agentID, Trigger: d.trigger, HeadSha: d.head, Reasons: d.reasons,
		ActorUserID: d.actor, TriggerCommentID: d.comment, TaintIds: taint,
	}); err != nil {
		return fmt.Errorf("review admission: record deferral: %w", err)
	}
	return nil
}

// admissionReasons names why a PR head is not admissible: a merge conflict, and
// each admission check that is failing, pending or missing. Empty: admissible.
// The worst row wins when a check name repeats.
func admissionReasons(required []string, runs []db.ListAdmissionCheckRunsRow, mergeState string) []string {
	var reasons []string
	if mergeState == "DIRTY" {
		reasons = append(reasons, "merge conflict")
	}
	for _, name := range required {
		state := "missing"
		for _, r := range runs {
			if r.Name != name {
				continue
			}
			switch {
			case r.Status != "completed":
				if state != "failing" {
					state = "pending"
				}
			case r.Conclusion.String == "success" || r.Conclusion.String == "neutral" || r.Conclusion.String == "skipped":
				if state == "missing" {
					state = ""
				}
			default:
				state = "failing"
			}
		}
		if state != "" {
			reasons = append(reasons, name+": "+state)
		}
	}
	return reasons
}

// startsReview reports whether the run can start a review: a comment, handoff
// or mention (ReviewRun) or a wakeup, but never a leader's callback.
func startsReview(target RunTarget) bool {
	switch target.Trigger {
	case "squad leader callback":
		return false
	case "wakeup", "system wakeup":
		return true
	}
	return target.ReviewRun
}

// guardReviewAdmission defers a reviewer run on a PR head that is not
// admissible (CHE-4650). Only an agent in the configured reviewer set is
// judged, only when the issue has a linked open PR, and never the agent that
// authored that PR (GitHubPullRequestAuthor), in any status and on any path: its
// run is what turns red CI green, even when it reviews other PRs. The author
// comes from the PR binding or task evidence, never from assignee or status.
// An unknown or ambiguous author skips only the exemption; the deferral then
// records "author: unresolved".
//
// The decision names the one PR head the run was judged on. A deferral that ran
// out (CHE-4651) lets the run start without judging CI only for the head the
// recovery evidence names: the evidence is checked against the head read here,
// so a PR that moved after the evidence was captured is judged as usual.
// admissionDecision is what guardReviewAdmission settled for the run: the PR
// head it judged (empty when the run is not judged) and whether it starts
// because that head's deferral expired.
type admissionDecision struct {
	Head    string
	Expired bool
}

func guardReviewAdmission(ctx context.Context, q *db.Queries, issueID pgtype.UUID, target RunTarget) (admissionDecision, error) {
	if !admissionEnabled || !startsReview(target) || !target.AgentID.Valid || !issueID.Valid || !admissionReviewers[util.UUIDToString(target.AgentID)] {
		return admissionDecision{}, nil
	}
	pr, err := admissionPR(ctx, q, issueID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return admissionDecision{}, nil
		}
		return admissionDecision{}, fmt.Errorf("review admission: load PR: %w", err)
	}
	if admissionRecoveryOf(ctx).expiredFor(issueID, target.AgentID, pr.HeadSha) {
		return admissionDecision{Head: pr.HeadSha, Expired: true}, nil
	}
	authorKnown := true
	author, err := GitHubPullRequestAuthor(ctx, q, pr)
	switch {
	case err == nil:
		if author.ID == target.AgentID {
			return admissionDecision{}, nil
		}
	case errors.Is(err, pgx.ErrNoRows):
		authorKnown = false
	default:
		return admissionDecision{}, fmt.Errorf("review admission: resolve PR author: %w", err)
	}
	runs, err := q.ListAdmissionCheckRuns(ctx, db.ListAdmissionCheckRunsParams{PrID: pr.ID, HeadSha: pr.HeadSha})
	if err != nil {
		return admissionDecision{}, fmt.Errorf("review admission: load checks: %w", err)
	}
	reasons := admissionReasons(admissionChecks, runs, pr.ApiMergeStateStatus.String)
	if len(reasons) == 0 {
		return admissionDecision{Head: pr.HeadSha}, nil
	}
	if !authorKnown {
		reasons = append(reasons, "author: unresolved")
	}
	// Fail closed: a requester whose taint cannot be read is never shown clean.
	taint, err := ActingTaskTaint(ctx, q)
	if err != nil {
		return admissionDecision{}, fmt.Errorf("review admission: read requester taint: %w", err)
	}
	deferral := &admissionDeferredError{
		issueID: issueID, agentID: target.AgentID, trigger: target.Trigger, head: pr.HeadSha, reasons: reasons,
		actor: target.ActorUserID, comment: target.TriggerCommentID, taint: append(taint, stageTaint(ctx)...),
	}
	if err := markAdmissionDeferred(ctx, q, deferral); err != nil {
		return admissionDecision{}, err
	}
	slog.Info("review admission: run deferred",
		"issue_id", util.UUIDToString(issueID), "agent_id", util.UUIDToString(target.AgentID), "reasons", reasons)
	return admissionDecision{}, deferral
}

// admissionPR is the PR whose head the review is judged on: the PR a
// replacement review is pinned to, otherwise the issue's newest open PR. A
// pinned PR that is no longer open or has no head is not judged (ErrNoRows),
// as the issue-wide read skips it.
func admissionPR(ctx context.Context, q *db.Queries, issueID pgtype.UUID) (db.GithubPullRequest, error) {
	prID := reviewPRPin(ctx)
	if !prID.Valid {
		return q.GetIssueAdmissionPR(ctx, issueID)
	}
	pr, err := q.GetGitHubPullRequestByID(ctx, prID)
	if err != nil {
		return db.GithubPullRequest{}, err
	}
	if (pr.State != "open" && pr.State != "draft") || pr.HeadSha == "" {
		return db.GithubPullRequest{}, pgx.ErrNoRows
	}
	return pr, nil
}
