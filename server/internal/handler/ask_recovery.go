package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const askRecoveryBatch = 20

// askRecoveryFault lets a test stop a delivery just before or after its commit.
var askRecoveryFault func(point string) error

func askRecoveryFaultAt(point string) error {
	if askRecoveryFault == nil {
		return nil
	}
	return askRecoveryFault(point)
}

// Why an event cannot be sent; the repair slice reads these.
const (
	askRecoveryNoSource      = "no_source"
	askRecoveryNotSingle     = "member_not_single"
	askRecoveryNoAccess      = "recipient_no_access"
	askRecoveryNoAgent       = "agent_unavailable"
	askRecoveryLaneNotUsable = "lane_unavailable"
)

// deliverAskRecoveries sends one recovery notice per open block event whose
// task has ended. One failing event does not hold back the others.
func (handler *Handler) deliverAskRecoveries(ctx context.Context) error {
	events, err := handler.Queries.ListAskRecoveryEvents(ctx, askRecoveryBatch)
	if err != nil {
		return err
	}
	var failures []error
	for _, event := range events {
		if err := handler.deliverAskRecovery(ctx, event); err != nil {
			slog.Warn("ask recovery delivery failed", "event_id", uuidToString(event.ID), "error", err)
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// askRecoveryContent quotes the stored source verbatim. It names no option,
// recommendation or approval, so nothing in it can answer a decision.
func askRecoveryContent(identifier, issueID, source string) string {
	return fmt.Sprintf("This is automatic recovery, not a decision request. A run asked about [%s](mention://issue/%s) and ended before the ask reached you.\n\nOriginal ask:\n\n%s\n\nReply in the comment thread under this notice on the issue to answer; a reply in this chat does not count. %s", identifier, issueID, source, util.OpenIssueFooter(identifier, issueID))
}

// deliverAskRecovery writes the notice, the issue comment, the DM message and
// the inbox item, and marks the event delivered, in one transaction. An event
// that cannot be sent safely records why and waits for repair instead.
func (handler *Handler) deliverAskRecovery(ctx context.Context, listed db.AskGuardEvent) error {
	transaction, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer transaction.Rollback(ctx)
	queries := handler.Queries.WithTx(transaction)
	if _, err := queries.LockWorkspaceForChatSessionCreate(ctx, listed.WorkspaceID); err != nil {
		return err
	}
	event, err := queries.LockAskGuardEvent(ctx, db.LockAskGuardEventParams{ID: listed.ID, WorkspaceID: listed.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if event.ClearedAt.Valid || event.RepairReason.Valid {
		return nil
	}
	repair := func(reason string) error {
		if err := queries.SetAskGuardEventRepair(ctx, db.SetAskGuardEventRepairParams{ID: event.ID, WorkspaceID: event.WorkspaceID, RepairReason: pgtype.Text{String: reason, Valid: true}}); err != nil {
			return err
		}
		return transaction.Commit(ctx)
	}
	// The run already asked this member on this issue: the ask reached them, so a
	// notice would repeat it. An event that opened after the request (a refused
	// edit) was not served when the request was created, so close it here.
	if event.MemberID.Valid {
		asked, err := queries.TaskAskedMemberOnIssue(ctx, db.TaskAskedMemberOnIssueParams{WorkspaceID: event.WorkspaceID, IssueID: event.IssueID, HumanUserID: event.MemberID, TaskID: event.TaskID})
		if err != nil {
			return err
		}
		if asked {
			if _, err := queries.ServeAskGuardEvent(ctx, db.ServeAskGuardEventParams{ID: event.ID, WorkspaceID: event.WorkspaceID}); err != nil {
				return err
			}
			return transaction.Commit(ctx)
		}
	}
	source, err := queries.GetAskGuardEventSource(ctx, db.GetAskGuardEventSourceParams{ID: event.ID, WorkspaceID: event.WorkspaceID})
	if err != nil {
		return err
	}
	if source == "" {
		return repair(askRecoveryNoSource)
	}
	if !event.MemberID.Valid {
		return repair(askRecoveryNotSingle)
	}
	// ponytail: access is active workspace membership; the repo has no per-issue ACL.
	if _, err := queries.LockDecisionMember(ctx, db.LockDecisionMemberParams{WorkspaceID: event.WorkspaceID, UserID: event.MemberID}); errors.Is(err, pgx.ErrNoRows) {
		return repair(askRecoveryNoAccess)
	} else if err != nil {
		return err
	}
	issue, err := queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: event.WorkspaceID, ID: event.IssueID})
	if errors.Is(err, pgx.ErrNoRows) {
		return repair(askRecoveryNoAccess)
	}
	if err != nil {
		return err
	}
	task, err := queries.GetAgentTask(ctx, event.TaskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return repair(askRecoveryNoAgent)
	}
	if err != nil {
		return err
	}
	agent, err := queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{WorkspaceID: event.WorkspaceID, ID: task.AgentID})
	if errors.Is(err, pgx.ErrNoRows) {
		return repair(askRecoveryNoAgent)
	}
	if err != nil {
		return err
	}
	workspace, err := queries.GetWorkspace(ctx, event.WorkspaceID)
	if err != nil {
		return err
	}
	identifier := fmt.Sprintf("%s-%d", workspace.IssuePrefix, issue.Number)
	content := askRecoveryContent(identifier, uuidToString(issue.ID), source)
	sent, err := handler.writeMemberNotice(ctx, queries, memberNotice{
		Issue: issue, Agent: agent, MemberID: event.MemberID, Content: content,
		Kind: protocol.ChatMessageKindAskRecovery, AuthorType: "agent", AuthorID: agent.ID,
		Link: func(commentID, messageID, sessionID pgtype.UUID) (string, pgtype.UUID, error) {
			notice, err := queries.InsertAskRecoveryNotice(ctx, db.InsertAskRecoveryNoticeParams{ID: dbid.NewV7(), WorkspaceID: event.WorkspaceID, IssueID: issue.ID, EventID: event.ID, MemberID: event.MemberID, ChatSessionID: sessionID, ChatMessageID: messageID, CommentID: commentID})
			return "ask_recovery_notice_id", notice.ID, err
		},
	})
	var refusal decisionIntentRefusal
	if errors.As(err, &refusal) {
		return repair(askRecoveryLaneNotUsable)
	}
	if err != nil {
		return err
	}
	if rows, err := queries.MarkAskGuardEventDelivered(ctx, db.MarkAskGuardEventDeliveredParams{ID: event.ID, WorkspaceID: event.WorkspaceID}); err != nil || rows != 1 {
		return errors.Join(err, errors.New("ask recovery event changed; retry"))
	}
	if err := askRecoveryFaultAt("before_commit"); err != nil {
		return err
	}
	if err := transaction.Commit(ctx); err != nil {
		return err
	}
	handler.publishMemberNotice(event.WorkspaceID, "agent", agent.ID, event.MemberID, sent)
	return askRecoveryFaultAt("after_commit")
}

// memberNotice is one notice to one member: an issue comment, a message in the
// member's decision DM for the issue's topic, and a mandatory inbox item.
type memberNotice struct {
	Issue      db.Issue
	Agent      db.Agent // owns the DM lane when the topic has no DM yet
	MemberID   pgtype.UUID
	Content    string
	Kind       string // chat message kind
	AuthorType string // comment author; the inbox actor is the same
	AuthorID   pgtype.UUID
	// Link writes the caller's own record of this notice and returns the key and
	// id the inbox item carries in its details.
	Link func(commentID, messageID, sessionID pgtype.UUID) (key string, id pgtype.UUID, err error)
}

type sentMemberNotice struct {
	Session db.ChatSession
	Message db.ChatMessage
	Item    db.InboxItem
}

// writeMemberNotice writes the notice in the caller's transaction. A lane the
// member cannot use returns a decisionIntentRefusal and writes nothing.
func (handler *Handler) writeMemberNotice(ctx context.Context, queries *db.Queries, n memberNotice) (sentMemberNotice, error) {
	issue := n.Issue
	topic, err := resolveDecisionTopic(ctx, queries, issue)
	if err != nil {
		return sentMemberNotice{}, err
	}
	session, found, err := topicLane(ctx, queries, issue, topic, n.MemberID, n.Agent.ID)
	if err != nil {
		return sentMemberNotice{}, err
	}
	if !found {
		steward, err := handler.decisionSteward(ctx, queries, topic, n.Agent, n.MemberID)
		if err != nil {
			return sentMemberNotice{}, err
		}
		lane := db.ChatSession{AgentID: steward.ID, CreatorID: n.MemberID, Title: "Decisions: " + topic.Title, DecisionTopicID: topic.ID}
		if session, err = createDecisionDM(ctx, queries, issue, lane); err != nil {
			return sentMemberNotice{}, err
		}
	}
	commentRow, err := queries.CreateComment(ctx, db.CreateCommentParams{ID: dbid.NewV7(), IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, AuthorType: n.AuthorType, AuthorID: n.AuthorID, Content: n.Content, Type: "comment"})
	if err != nil {
		return sentMemberNotice{}, err
	}
	comment := commentRow.Comment()
	message, err := queries.CreateChatMessage(ctx, db.CreateChatMessageParams{ID: dbid.NewV7(), ChatSessionID: session.ID, Role: "assistant", Content: n.Content, MessageKind: pgtype.Text{String: n.Kind, Valid: true}, SentVia: pgtype.Text{String: "system", Valid: true}})
	if err != nil {
		return sentMemberNotice{}, err
	}
	if err := queries.TouchChatSession(ctx, session.ID); err != nil {
		return sentMemberNotice{}, err
	}
	key, id, err := n.Link(comment.ID, message.ID, session.ID)
	if err != nil {
		return sentMemberNotice{}, err
	}
	details, err := json.Marshal(map[string]pgtype.UUID{"comment_id": comment.ID, key: id})
	if err != nil {
		return sentMemberNotice{}, err
	}
	// The ping is mandatory: it skips the delivery preferences that
	// inbox.CreateIfEligible applies, and a missing recipient fails the delivery.
	// The item opens the issue comment, not the DM, so push says nothing about a decision.
	item, err := queries.CreateInboxItem(ctx, db.CreateInboxItemParams{ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, RecipientType: "member", RecipientID: n.MemberID, Type: "mentioned", Severity: "info", IssueID: issue.ID, Title: issue.Title, Body: pgtype.Text{String: n.Content, Valid: true}, ActorType: pgtype.Text{String: n.AuthorType, Valid: true}, ActorID: n.AuthorID, Details: details})
	if err != nil {
		return sentMemberNotice{}, err
	}
	return sentMemberNotice{Session: session, Message: message, Item: item}, nil
}

// publishMemberNotice announces a committed notice.
func (handler *Handler) publishMemberNotice(workspace pgtype.UUID, actorType string, actorID, member pgtype.UUID, sent sentMemberNotice) {
	workspaceID, actor := uuidToString(workspace), uuidToString(actorID)
	handler.publishChatFor(protocol.EventChatMessage, workspaceID, actorType, actor, uuidToString(member), uuidToString(sent.Session.ID), protocol.ChatMessagePayload{ChatSessionID: uuidToString(sent.Session.ID), MessageID: uuidToString(sent.Message.ID), Role: sent.Message.Role, Content: sent.Message.Content, CreatedAt: timestampToString(sent.Message.CreatedAt), MessageKind: sent.Message.MessageKind})
	handler.publish(protocol.EventInboxNew, workspaceID, actorType, actor, map[string]any{"item": inboxToResponse(sent.Item)})
}

// askRecoveryNoticeAnswer finds the notice that a reply under parent is for. A
// reply that would answer it is refused once its member lost access; the reply
// itself answers only when its author is the addressed member.
func (handler *Handler) askRecoveryNoticeAnswer(writer http.ResponseWriter, request *http.Request, parent *db.Comment, userID string) (notice db.AskRecoveryNotice, answers, ok bool) {
	if parent == nil {
		return notice, false, true
	}
	notice, err := handler.Queries.GetAskRecoveryNoticeByComment(request.Context(), db.GetAskRecoveryNoticeByCommentParams{CommentID: parent.ID, WorkspaceID: parent.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return notice, false, true
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "failed to load ask recovery notice")
		return notice, false, false
	}
	if uuidToString(notice.MemberID) != userID {
		return notice, false, true
	}
	if !handler.isActiveWorkspaceMember(request.Context(), userID, uuidToString(notice.WorkspaceID)) {
		writeError(writer, http.StatusForbidden, "workspace membership is required")
		return notice, false, false
	}
	return notice, true, true
}
