package main

import (
	"encoding/json"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestNotification_MalformedMemberMentionPreservesValidRecipients(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mentionAll bool
		mapPayload bool
	}{
		{name: "explicit member"},
		{name: "explicit member map payload", mapPayload: true},
		{name: "all members", mentionAll: true},
		{name: "all members map payload", mentionAll: true, mapPayload: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := workspaceFixture(t)
			recipientID := fx.User(t, "Mention recipient", "mention-recipient@multica.ai")
			mutedID := fx.User(t, "Muted recipient", "mention-muted@multica.ai")
			fx.Member(t, testWorkspaceID, recipientID, "member")
			fx.Member(t, testWorkspaceID, mutedID, "member")
			fx.Insert(t, "notification_preference", testutil.Cols{
				"workspace_id": testWorkspaceID,
				"user_id":      mutedID,
				"preferences":  `{"mentions":"muted"}`,
			})
			issueID := fx.Issue(t, "Malformed mention")
			fx.Cleanup(t, `DELETE FROM inbox_item WHERE issue_id = $1`, issueID)
			fx.Cleanup(t, `DELETE FROM issue_subscriber WHERE issue_id = $1`, issueID)

			// These IDs match the markdown parser but are not UUIDs. Keep a
			// valid recipient on either side to exercise the whole batch.
			content := "[@Recipient](mention://member/" + recipientID + ") " +
				"[@Bad](mention://member/deadbeef) [@Bad all](mention://member/all) " +
				"[@Recipient again](mention://member/" + recipientID + ") " +
				"[@Muted](mention://member/" + mutedID + ") " +
				"[@Self](mention://member/" + testUserID + ")"
			if tc.mentionAll {
				content = "[@Bad](mention://member/deadbeef) [@All](mention://all/all)"
			}
			commentID := fx.Comment(t, issueID, content)
			comment := handler.CommentResponse{
				ID: commentID, IssueID: issueID, AuthorType: "member",
				AuthorID: testUserID, Content: content, Type: "comment",
			}
			var commentPayload any = comment
			if tc.mapPayload {
				commentPayload = map[string]any{
					"id": commentID, "issue_id": issueID, "author_type": "member",
					"author_id": testUserID, "content": content, "type": "comment",
				}
			}

			queries := db.New(testPool)
			bus := newNotificationBus(t, queries)
			var inboxEvents []events.Event
			bus.Subscribe(protocol.EventInboxNew, func(e events.Event) {
				inboxEvents = append(inboxEvents, e)
			})
			bus.Publish(events.Event{
				Type: protocol.EventCommentCreated, WorkspaceID: testWorkspaceID,
				ActorType: "member", ActorID: testUserID,
				Payload: map[string]any{
					"comment": commentPayload, "issue_title": "Malformed mention", "issue_status": "todo",
				},
			})

			items := inboxItemsForRecipient(t, queries, recipientID)
			if len(items) != 1 {
				t.Fatalf("valid recipient got %d inbox items, want 1", len(items))
			}
			if items[0].Type != "mentioned" || util.UUIDToString(items[0].IssueID) != issueID {
				t.Fatalf("unexpected mention inbox item: %+v", items[0])
			}
			var details map[string]string
			if err := json.Unmarshal(items[0].Details, &details); err != nil || details["comment_id"] != commentID {
				t.Fatalf("mention details = %s, error = %v; want comment_id %s", items[0].Details, err, commentID)
			}
			if count := fx.Count(t, `SELECT count(*) FROM inbox_item WHERE issue_id = $1`, issueID); count != 1 {
				t.Fatalf("issue has %d inbox items, want only the valid, unmuted recipient", count)
			}
			if len(inboxEvents) != 1 {
				t.Fatalf("got %d inbox:new events, want 1", len(inboxEvents))
			}
			item := inboxEvents[0].Payload.(map[string]any)["item"].(map[string]any)
			if item["recipient_id"] != recipientID || item["type"] != "mentioned" || item["issue_status"] != "todo" {
				t.Fatalf("unexpected inbox:new item: %+v", item)
			}
		})
	}
}

func TestNotifyMentionedMembersOnlyMalformedIDs(t *testing.T) {
	// Call the boundary directly so the event bus cannot hide a panic. With
	// no valid recipients, neither the database nor the bus is needed.
	notifyMentionedMembers(nil, nil, events.Event{},
		parseMentions("[@Bad](mention://member/deadbeef) [@Bad all](mention://member/all)"),
		"", "", "", "", nil, nil)
}

func TestNotification_MentionRecordsItsSource(t *testing.T) {
	fx := workspaceFixture(t)
	directID := fx.User(t, "Direct recipient", "mention-source-direct@multica.ai")
	squadOnlyID := fx.User(t, "Squad-only recipient", "mention-source-squad@multica.ai")
	bothID := fx.User(t, "Direct and squad recipient", "mention-source-both@multica.ai")
	for _, id := range []string{directID, squadOnlyID, bothID} {
		fx.Member(t, testWorkspaceID, id, "member")
	}
	squadID := fx.Squad(t, "Mention source squad", fx.Agent(t, "Mention source leader", ""))
	fx.SquadMember(t, squadID, "member", squadOnlyID)
	fx.SquadMember(t, squadID, "member", bothID)

	sourcesFor := func(content string, recipients ...string) map[string]string {
		issueID := fx.Issue(t, "Mention source")
		fx.Cleanup(t, `DELETE FROM inbox_item WHERE issue_id = $1`, issueID)
		fx.Cleanup(t, `DELETE FROM issue_subscriber WHERE issue_id = $1`, issueID)
		commentID := fx.Comment(t, issueID, content)
		queries := db.New(testPool)
		bus := newNotificationBus(t, queries)
		bus.Publish(events.Event{
			Type: protocol.EventCommentCreated, WorkspaceID: testWorkspaceID,
			ActorType: "member", ActorID: testUserID,
			Payload: map[string]any{
				"comment": handler.CommentResponse{
					ID: commentID, IssueID: issueID, AuthorType: "member",
					AuthorID: testUserID, Content: content, Type: "comment",
				},
				"issue_title": "Mention source", "issue_status": "todo",
			},
		})
		sources := map[string]string{}
		for _, id := range recipients {
			for _, item := range inboxItemsForRecipient(t, queries, id) {
				if item.Type != "mentioned" || util.UUIDToString(item.IssueID) != issueID {
					continue
				}
				var details map[string]string
				if err := json.Unmarshal(item.Details, &details); err != nil {
					t.Fatalf("details = %s: %v", item.Details, err)
				}
				if details["comment_id"] != commentID {
					t.Fatalf("details = %s; want comment_id %s kept", item.Details, commentID)
				}
				sources[id] = details["mention_source"]
			}
		}
		return sources
	}

	got := sourcesFor("[@Direct](mention://member/"+directID+") [@Both](mention://member/"+bothID+") [@Squad](mention://squad/"+squadID+")", directID, squadOnlyID, bothID)
	want := map[string]string{directID: "direct", squadOnlyID: "squad", bothID: "direct"}
	if len(got) != len(want) {
		t.Fatalf("direct + squad mention sources = %v, want %v", got, want)
	}
	for id, source := range want {
		if got[id] != source {
			t.Fatalf("direct + squad mention sources = %v, want %v", got, want)
		}
	}

	got = sourcesFor("[@All](mention://all/all) [@Squad](mention://squad/"+squadID+")", directID, squadOnlyID)
	if got[directID] != "all" || got[squadOnlyID] != "all" {
		t.Fatalf("@all + squad mention sources = %v, want all for every member (@all outranks squad)", got)
	}
}
