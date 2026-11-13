package main

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/askguard"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// The ask guard (S2) and mention notifications must agree on what counts as a
// mention: the guard fires exactly when the member would be notified.
func TestAskGuardMentionMatchesNotification(t *testing.T) {
	for name, wrap := range map[string]string{
		"plain text":  "%s",
		"inline code": "`%s`",
		"fenced code": "```\n%s\n```",
		"no mention":  "nobody",
		"not a uuid":  "[@Bad](mention://member/deadbeef)",
	} {
		t.Run(name, func(t *testing.T) {
			fx := workspaceFixture(t)
			recipientID := fx.User(t, "Guard recipient", "guard-"+uuid.NewString()+"@multica.ai")
			fx.Member(t, testWorkspaceID, recipientID, "member")
			issueID := fx.Issue(t, "Guard agreement")
			fx.Cleanup(t, `DELETE FROM inbox_item WHERE issue_id = $1`, issueID)
			fx.Cleanup(t, `DELETE FROM issue_subscriber WHERE issue_id = $1`, issueID)

			link := "[@Recipient](mention://member/" + recipientID + ")"
			content := "please " + fmt.Sprintf(wrap, link)
			if name == "no mention" || name == "not a uuid" {
				content = wrap
			}
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
					"issue_title": "Guard agreement", "issue_status": "todo",
				},
			})

			notified := len(inboxItemsForRecipient(t, queries, recipientID)) > 0
			guarded := len(askguard.MentionedMembers(content)) > 0
			if notified != guarded {
				t.Fatalf("notified=%v guarded=%v for %q", notified, guarded, content)
			}
			if name != "no mention" && name != "not a uuid" && !guarded {
				t.Fatal("a member link did not count as a mention")
			}
		})
	}
}
