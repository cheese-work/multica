package handler

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

func TestHandoffSampleSelfEchoReplay(test *testing.T) {
	path := os.Getenv("MULTICA_HANDOFF_REPLAY_SAMPLE")
	if path == "" {
		test.Skip("no historical handoff sample supplied")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		test.Fatal(err)
	}
	var sample struct {
		SquadLeaders map[string]string `json:"squad_leaders"`
		Samples      []struct {
			CommentID string `json:"comment_id"`
			AuthorID  string `json:"author_id"`
			Content   string `json:"content"`
		} `json:"samples"`
	}
	if err := json.Unmarshal(data, &sample); err != nil {
		test.Fatal(err)
	}
	if len(sample.Samples) != 20 {
		test.Fatalf("sample_size=%d, want 20", len(sample.Samples))
	}
	ctx := context.Background()
	for _, original := range sample.Samples {
		test.Run(original.CommentID, func(test *testing.T) {
			agents := map[string]string{}
			mappedAgent := func(originalID string) string {
				if agents[originalID] == "" {
					agents[originalID] = createHandlerTestAgent(test, "replayed "+originalID, nil)
				}
				return agents[originalID]
			}
			authorID := mappedAgent(original.AuthorID)
			content := original.Content
			for _, mention := range util.ParseMentions(original.Content) {
				switch mention.Type {
				case "agent":
					content = strings.ReplaceAll(content, "mention://agent/"+mention.ID, "mention://agent/"+mappedAgent(mention.ID))
				case "squad":
					leaderID := sample.SquadLeaders[mention.ID]
					if leaderID == "" {
						test.Fatalf("missing squad leader for %s", mention.ID)
					}
					squadID := dbfx.Squad(test, "replayed squad", mappedAgent(leaderID))
					content = strings.ReplaceAll(content, "mention://squad/"+mention.ID, "mention://squad/"+squadID)
				}
			}
			issueID := dbfx.Issue(test, "handoff replay")
			dbfx.Task(test, authorID, testutil.Cols{"runtime_id": handlerTestRuntimeID(test), "issue_id": issueID, "status": "running"})
			commentID := dbfx.Comment(test, issueID, content, testutil.Cols{"author_type": "agent", "author_id": authorID})
			issue, err := testHandler.Queries.GetIssue(ctx, util.MustParseUUID(issueID))
			if err != nil {
				test.Fatal(err)
			}
			comment, err := testHandler.Queries.GetComment(ctx, util.MustParseUUID(commentID))
			if err != nil {
				test.Fatal(err)
			}
			testHandler.triggerTasksForComment(ctx, issue, comment, nil, "agent", authorID, testUserID, nil, nil)
			if got := countQueuedOrDispatched(test, authorID, issueID); got != 0 {
				test.Fatalf("self_echo_runs=%d, want zero", got)
			}
		})
	}
}
