package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// Suspension write (CHE-2591 L5 design §4.4.7). Every refusal of an episode's
// resume ends in this one write, so the kind, the clock, the block, the comment
// and the restore rule cannot drift apart.

// suspendKindRunBudget and suspendKindDispatchRefused are the suspend_reason
// values the episode itself writes; status_moved is written by the status hook.
const (
	suspendKindRunBudget       = "run_budget"
	suspendKindDispatchRefused = "dispatch_refused"
)

// dispatchRefusedSuspendAfter is how long a waiting episode keeps retrying a
// refused dispatch before it suspends (§4.4.4).
const dispatchRefusedSuspendAfter = 15 * time.Minute

func suspendBlockedReason(kind string) string {
	if kind == suspendKindRunBudget {
		return issueworkflow.BlockedReasonRunBudget
	}
	return issueworkflow.BlockedReasonDecisionDispatchRefused
}

// suspendDecisionEpisode suspends the locked, non-closed episode for kind. The
// caller's transaction holds the lifecycle locks and has rolled back to the
// savepoint, so nothing of the refused attempt remains. conflict is the task
// that blocks the dispatch (dispatch_refused only). The returned move is for the
// caller to finish after commit.
func (handler *Handler) suspendDecisionEpisode(ctx context.Context, queries *db.Queries, snapshot decisionWakeSnapshot, kind string, conflict db.AgentTaskQueue) (edgeMove, error) {
	wait := snapshot.wait
	// Reloaded: the rollback and earlier comments moved the revision the block expects.
	issue, err := queries.GetIssue(ctx, wait.IssueID)
	if err != nil {
		return edgeMove{}, err
	}
	def, _, err := issueworkflow.ForIssue(ctx, queries, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
	if err != nil {
		return edgeMove{}, err
	}
	wasBlocked := issue.Status == issuestatus.Blocked
	blocks := !wasBlocked && (def == nil || def.Has(issuestatus.Blocked))

	// Capture rule (§4.4.6): read before the block write clears the flag.
	status, assigneeType, assigneeID := captureRestoreTarget(wait, issue, blocks)
	restoreOnResume := wait.RestoreOnResume || blocks

	var move edgeMove
	if blocks {
		if move, err = handler.moveIssueOnEdgeTx(ctx, queries, issue, issuestatus.Blocked, "run_budget", nil, false, nil); err != nil {
			return edgeMove{}, err
		}
	}
	// A person's or agent's own block (flag false) keeps its reason.
	if blocks || (wasBlocked && wait.RestoreOnResume) {
		value, err := json.Marshal(suspendBlockedReason(kind))
		if err != nil {
			return edgeMove{}, err
		}
		// An unchanged value is a no-op that returns no row.
		_, setErr := queries.SetIssueMetadataKey(ctx, db.SetIssueMetadataKeyParams{Key: "blocked_reason", Value: value, ID: issue.ID, WorkspaceID: issue.WorkspaceID})
		if setErr != nil && !errors.Is(setErr, pgx.ErrNoRows) {
			return edgeMove{}, setErr
		}
	}
	if !wasBlocked || wait.SuspendReason.String != kind {
		content, err := handler.suspensionComment(ctx, queries, snapshot, kind, conflict)
		if err != nil {
			return edgeMove{}, err
		}
		if _, err := queries.CreateComment(ctx, db.CreateCommentParams{
			ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID,
			AuthorType: "system", AuthorID: pgtype.UUID{Valid: true}, Type: "comment", Content: content,
		}); err != nil {
			return edgeMove{}, err
		}
	}
	// Last: the block write above ran the status hook, which this overwrites.
	refused := pgtype.UUID{}
	if kind == suspendKindDispatchRefused {
		refused = conflict.ID
	}
	if _, err := queries.SuspendDecisionWait(ctx, db.SuspendDecisionWaitParams{
		SuspendReason: pgtype.Text{String: kind, Valid: true}, RefusedTaskID: refused,
		RestoreStatus: status, RestoreAssigneeType: assigneeType, RestoreAssigneeID: assigneeID, RestoreOnResume: restoreOnResume,
		ID: wait.ID, WorkspaceID: wait.WorkspaceID,
	}); err != nil {
		return edgeMove{}, err
	}
	return move, nil
}

// suspensionComment names the kind, the conflicting task, the bound answers, R
// and the open requests, and says how the episode resumes.
func (handler *Handler) suspensionComment(ctx context.Context, queries *db.Queries, snapshot decisionWakeSnapshot, kind string, conflict db.AgentTaskQueue) (string, error) {
	var content strings.Builder
	switch kind {
	case suspendKindRunBudget:
		content.WriteString("Decision wait suspended (`run_budget`): the issue used its workflow's run budget, so the answers cannot start a run.\n")
	default:
		fmt.Fprintf(&content, "Decision wait suspended (`dispatch_refused`): task %s of %s is active with squad `%s` (leader task: %t), which differs from the squad the answers resume in.\n",
			uuidToString(conflict.ID), snapshot.recipient.Name, uuidToString(conflict.SquadID), conflict.IsLeaderTask)
	}
	human, err := queries.GetUser(ctx, snapshot.wait.HumanUserID)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&content, "Resuming agent: %s. Human in charge: %s.\n", snapshot.recipient.Name, human.Name)
	for _, request := range snapshot.requests {
		if request.Status == "open" {
			fmt.Fprintf(&content, "- Request %s is still open.\n", uuidToString(request.ID))
		}
	}
	for _, answer := range snapshot.answers {
		if !episodeLiveAnswer(answer) {
			continue
		}
		quoted, err := queries.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{WorkspaceID: snapshot.wait.WorkspaceID, ID: answer.IssueCommentID})
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&content, "- Answer to request %s, not delivered yet:\n> %s\n", uuidToString(answer.DecisionRequestID), strings.ReplaceAll(quoted.Content, "\n", "\n> "))
	}
	content.WriteString("The episode resumes when a person moves the issue: moving the issue resumes it")
	if kind == suspendKindDispatchRefused {
		fmt.Fprintf(&content, "; it also resumes when task %s ends", uuidToString(conflict.ID))
	}
	content.WriteString(".")
	return content.String(), nil
}
