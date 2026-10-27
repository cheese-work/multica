// Package dmnotify keeps one coalesced inbox record per (recipient, DM) for the
// replies an agent writes into that DM (CHE-1793).
package dmnotify

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/inbox"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Decision messages are announced by the decision ping, which already records
// one item linked to the DM; notifying here would count them twice.
var skippedKinds = map[string]bool{"decision_request": true, "decision_reference": true, protocol.ChatMessageKindDecisionCancelNotice: true}

// Notified is the record a reply created or updated.
type Notified struct {
	Item    db.InboxItem
	Created bool
}

// Reply records one final assistant reply of a run. Call it with the
// transaction-bound queries that wrote the message, so the record commits or
// rolls back together with the reply. It returns nil when nothing is owed: the
// DM is archived, bound to a channel (the channel already delivers the reply),
// or the recipient switched both dm channels off.
func Reply(ctx context.Context, q *db.Queries, sessionID pgtype.UUID, message db.ChatMessage) (*Notified, error) {
	if message.Role != "assistant" || skippedKinds[message.MessageKind] {
		return nil, nil
	}
	session, err := q.GetChatSession(ctx, sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if session.Status != "active" {
		return nil, nil
	}
	bound, err := q.ChatSessionHasCurrentChannelBinding(ctx, session.ID)
	if err != nil || bound {
		return nil, err
	}
	switches, err := q.GetDMChannelSwitches(ctx, db.GetDMChannelSwitchesParams{WorkspaceID: session.WorkspaceID, UserID: session.CreatorID})
	if err != nil || (!switches.InApp && !switches.Push) {
		return nil, err
	}
	allowed, err := inbox.AllowsDelivery(ctx, q, session.WorkspaceID, session.CreatorID, pgtype.UUID{}, false)
	if err != nil || !allowed {
		return nil, err
	}
	item, err := q.UpsertDMReplyInboxItem(ctx, db.UpsertDMReplyInboxItemParams{
		ID: dbid.NewV7(), WorkspaceID: session.WorkspaceID, RecipientID: session.CreatorID,
		Title: session.Title, AgentID: session.AgentID, ChatSessionID: session.ID, ChatMessageID: message.ID,
		DedupeKey: pgtype.Text{String: "dm:" + util.UUIDToString(session.ID), Valid: true},
	})
	if err != nil {
		return nil, err
	}
	return &Notified{Item: item, Created: replyCount(item) == 1}, nil
}

func replyCount(item db.InboxItem) int {
	var details struct {
		ReplyCount int `json:"reply_count"`
	}
	_ = json.Unmarshal(item.Details, &details)
	return details.ReplyCount
}

// Publish announces a committed record to its recipient: inbox:new when the
// reply opened it, inbox:updated when it joined an unread one.
func Publish(bus *events.Bus, n *Notified) {
	if bus == nil || n == nil {
		return
	}
	eventType := protocol.EventInboxUpdated
	if n.Created {
		eventType = protocol.EventInboxNew
	}
	bus.Publish(events.Event{
		Type:        eventType,
		WorkspaceID: util.UUIDToString(n.Item.WorkspaceID),
		ActorType:   "agent",
		ActorID:     util.UUIDToString(n.Item.ActorID),
		Payload:     map[string]any{"item": Payload(n.Item)},
	})
}

// Payload is the realtime shape of an inbox item.
func Payload(item db.InboxItem) map[string]any {
	updatedAt := item.UpdatedAt
	if !updatedAt.Valid {
		updatedAt = item.CreatedAt
	}
	return map[string]any{
		"id":              util.UUIDToString(item.ID),
		"workspace_id":    util.UUIDToString(item.WorkspaceID),
		"recipient_type":  item.RecipientType,
		"recipient_id":    util.UUIDToString(item.RecipientID),
		"type":            item.Type,
		"severity":        item.Severity,
		"issue_id":        util.UUIDToPtr(item.IssueID),
		"title":           item.Title,
		"body":            util.TextToPtr(item.Body),
		"read":            item.Read,
		"archived":        item.Archived,
		"created_at":      util.TimestampToString(item.CreatedAt),
		"updated_at":      util.TimestampToString(updatedAt),
		"actor_type":      util.TextToPtr(item.ActorType),
		"actor_id":        util.UUIDToPtr(item.ActorID),
		"details":         json.RawMessage(item.Details),
		"chat_session_id": util.UUIDToPtr(item.ChatSessionID),
	}
}
