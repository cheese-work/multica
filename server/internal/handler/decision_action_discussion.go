package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// maxDiscussionMessage caps one "Discuss more" message.
const maxDiscussionMessage = 4000

var (
	errEmptyDiscussion = decisionIntentRefusal{http.StatusBadRequest, "message_required: write the message to send"}
	errDiscussionLong  = decisionIntentRefusal{http.StatusBadRequest, "message_too_long: keep it to 4000 bytes"}
)

// discussionSummary is the handoff the proposing agent reads: the outcome, the
// message, and the fact that the ask is still open.
func discussionSummary(message string) string {
	return "Decision outcome: discussion. The person replied to your proposal and the ask is still open; nothing was approved or run. Answer them in the thread and wait for their decision.\nMessage: " + message
}

// DiscussDecisionRequest sends the person's message to the proposing agent and
// leaves the ask open (B v9 S5, CHE-5415). The message is only a message: typing
// "approve" here approves nothing, and no action runs. Each call wakes the agent
// once; a message sent while its run is still queued merges into that run.
func (handler *Handler) DiscussDecisionRequest(writer http.ResponseWriter, request *http.Request) {
	caller, ok := handler.authorizeDecisionActionCaller(writer, request)
	if !ok {
		return
	}
	requestID := caller.actionID // the {id} of this route names the request
	var body struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 2*maxDiscussionMessage)).Decode(&body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid request body")
		return
	}
	message := strings.TrimSpace(body.Message)
	switch {
	case message == "":
		writeDecisionError(writer, errEmptyDiscussion, "discussion")
		return
	case len(message) > maxDiscussionMessage:
		writeDecisionError(writer, errDiscussionLong, "discussion")
		return
	}
	outcome, comment, queued, err := handler.discussDecision(request.Context(), caller, requestID, message)
	if err != nil {
		writeDecisionError(writer, err, "discussion")
		return
	}
	handler.publish(protocol.EventCommentCreated, uuidToString(caller.workspaceID), "member", uuidToString(caller.userID), map[string]any{"comment": commentToResponse(comment, nil, nil)})
	if queued.ID.Valid {
		handler.TaskService.PublishTaskEnqueued(context.WithoutCancel(request.Context()), queued)
	}
	writeJSON(writer, http.StatusOK, map[string]string{"outcome": "discussion", "wake": outcome, "comment_id": uuidToString(comment.ID)})
}

func (handler *Handler) discussDecision(ctx context.Context, caller decisionActionCaller, requestID pgtype.UUID, message string) (string, db.Comment, db.AgentTaskQueue, error) {
	var noComment db.Comment
	var noTask db.AgentTaskQueue
	transaction, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return "", noComment, noTask, err
	}
	defer transaction.Rollback(ctx)
	queries := handler.Queries.WithTx(transaction)
	located, err := queries.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: caller.workspaceID, ID: requestID})
	if errors.Is(err, pgx.ErrNoRows) || err == nil && located.HumanUserID != caller.userID {
		return "", noComment, noTask, errNotTheHuman
	} else if err != nil {
		return "", noComment, noTask, err
	}
	if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: caller.workspaceID, ID: located.IssueID}); err != nil {
		return "", noComment, noTask, err
	}
	current, err := queries.LockDecisionRequest(ctx, db.LockDecisionRequestParams{WorkspaceID: caller.workspaceID, ID: requestID})
	if err != nil {
		return "", noComment, noTask, err
	}
	if current.Status != "open" {
		return "", noComment, noTask, errNotPending
	}
	issue, err := queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: caller.workspaceID, ID: current.IssueID})
	if err != nil {
		return "", noComment, noTask, err
	}
	created, err := queries.CreateComment(ctx, db.CreateCommentParams{
		ID: dbid.NewV7(), WorkspaceID: caller.workspaceID, IssueID: current.IssueID, ParentID: current.IssueCommentID,
		AuthorType: "member", AuthorID: caller.userID, Type: "comment", Content: message,
	})
	if err != nil {
		return "", noComment, noTask, err
	}
	comment := created.Comment()
	snapshot := decisionWakeSnapshot{issue: issue, request: current}
	if err := handler.loadDecisionRecipient(ctx, queries, &snapshot, current.AskingAgentID); err != nil {
		return "", noComment, noTask, err
	}
	wake := "skipped_ineligible"
	if mayInvoke, err := handler.personMayInvoke(ctx, queries, current.AskingAgentID, caller.userID, caller.workspaceID); err != nil {
		return "", noComment, noTask, err
	} else if mayInvoke && snapshot.recipient.RuntimeID.Valid && !snapshot.recipient.ArchivedAt.Valid {
		wake, queued, err := handler.dispatchDecisionInputs(ctx, queries, snapshot, comment, []pgtype.UUID{comment.ID}, discussionSummary(message))
		if err != nil {
			return "", noComment, noTask, err
		}
		if wake != "" {
			if err := transaction.Commit(ctx); err != nil {
				return "", noComment, noTask, err
			}
			return wake, comment, queued, nil
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		return "", noComment, noTask, err
	}
	return wake, comment, noTask, nil
}
