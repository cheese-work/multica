package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/askguard"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// The guard and the mention notification agree for every language and link
// shape: the member is notified exactly when the guard sees a mention.
func TestAskGuardMatrixMentionMatchesNotification(t *testing.T) {
	texts := map[string]string{
		"english":    "Please decide whether to waive or rewrite it: %s",
		"vietnamese": "Vui lòng quyết định bỏ qua hay viết lại: %s",
		"chinese":    "请决定是豁免还是重写：%s",
	}
	shapes := map[string]string{"plain text": "%s", "inline code": "`%s`", "fenced code": "```\n%s\n```"}
	for lang, text := range texts {
		for shape, wrap := range shapes {
			t.Run(lang+"/"+shape, func(t *testing.T) {
				fx := workspaceFixture(t)
				recipientID := fx.User(t, "Guard recipient", "guard-"+uuid.NewString()+"@multica.ai")
				fx.Member(t, testWorkspaceID, recipientID, "member")
				issueID := fx.Issue(t, "Guard agreement")
				fx.Cleanup(t, `DELETE FROM inbox_item WHERE issue_id = $1`, issueID)
				fx.Cleanup(t, `DELETE FROM issue_subscriber WHERE issue_id = $1`, issueID)

				link := "[@Recipient](mention://member/" + recipientID + ")"
				content := strings.Replace(text, "%s", fmt.Sprintf(wrap, link), 1)
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
				if !notified || !guarded {
					t.Fatalf("notified=%v guarded=%v, want both for %q", notified, guarded, content)
				}
			})
		}
	}
}
