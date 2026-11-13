package handler

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5421: an unmentioned member reply in a thread wakes the agent that wrote
// the newest agent comment in that thread, not the author of the thread root.
func TestUnmentionedMemberReplyWakesLastAgentInThread_CHE5421(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setAgentCommentWakePolicyForTest(t, false)
	ctx := context.Background()
	fx := newSquadCommentTriggerFixture(t)
	issueID := uuidToString(fx.Issue.ID)

	rootID := dbfx.Comment(t, issueID, "root by agent A", testutil.Cols{"author_type": "agent", "author_id": fx.LeaderID})
	dbfx.Comment(t, issueID, "later by agent B", testutil.Cols{
		"author_type": "agent", "author_id": fx.OtherID, "parent_id": rootID,
	})
	root, err := testHandler.Queries.GetComment(ctx, parseUUID(rootID))
	if err != nil {
		t.Fatal(err)
	}

	triggers, _ := testHandler.computeCommentAgentTriggers(ctx, fx.Issue, "A", &root, "member", testUserID, commentTriggerComputeOptions{OriginatorUserID: testUserID})
	if len(triggers) != 1 || uuidToString(triggers[0].Agent.ID) != fx.OtherID {
		t.Fatalf("triggers = %+v, want exactly agent B (%s)", triggers, fx.OtherID)
	}
}

// With no agent comment other than the root author's, today's rule still holds.
func TestUnmentionedMemberReplyRootAgentOnly_CHE5421(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setAgentCommentWakePolicyForTest(t, false)
	ctx := context.Background()
	fx := newSquadCommentTriggerFixture(t)
	issueID := uuidToString(fx.Issue.ID)

	rootID := dbfx.Comment(t, issueID, "root by agent A", testutil.Cols{"author_type": "agent", "author_id": fx.LeaderID})
	dbfx.Comment(t, issueID, "member reply", testutil.Cols{"author_type": "member", "author_id": testUserID, "parent_id": rootID})
	root, err := testHandler.Queries.GetComment(ctx, parseUUID(rootID))
	if err != nil {
		t.Fatal(err)
	}
	triggers, _ := testHandler.computeCommentAgentTriggers(ctx, fx.Issue, "follow-up", &root, "member", testUserID, commentTriggerComputeOptions{OriginatorUserID: testUserID})
	if len(triggers) != 1 || uuidToString(triggers[0].Agent.ID) != fx.LeaderID {
		t.Fatalf("triggers = %+v, want root author", triggers)
	}
}
