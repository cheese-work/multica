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

func TestHandoffSampleSelfEchoReplay(t *testing.T) {
	path := os.Getenv("MULTICA_HANDOFF_REPLAY_SAMPLE")
	if path == "" {
		t.Skip("no historical handoff sample supplied")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
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
		t.Fatal(err)
	}
	if len(sample.Samples) != 20 {
		t.Fatalf("sample_size=%d, want 20", len(sample.Samples))
	}
	ctx := context.Background()
	for _, original := range sample.Samples {
		t.Run(original.CommentID, func(t *testing.T) {
			agents := map[string]string{}
			mappedAgent := func(originalID string) string {
				if agents[originalID] == "" {
					agents[originalID] = createHandlerTestAgent(t, "replayed "+originalID, nil)
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
						t.Fatalf("missing squad leader for %s", mention.ID)
					}
					squadID := dbfx.Squad(t, "replayed squad", mappedAgent(leaderID))
					content = strings.ReplaceAll(content, "mention://squad/"+mention.ID, "mention://squad/"+squadID)
				}
			}
			issueID := dbfx.Issue(t, "handoff replay")
			dbfx.Task(t, authorID, testutil.Cols{"runtime_id": handlerTestRuntimeID(t), "issue_id": issueID, "status": "running"})
			commentID := dbfx.Comment(t, issueID, content, testutil.Cols{"author_type": "agent", "author_id": authorID})
			issue, err := testHandler.Queries.GetIssue(ctx, util.MustParseUUID(issueID))
			if err != nil {
				t.Fatal(err)
			}
			comment, err := testHandler.Queries.GetComment(ctx, util.MustParseUUID(commentID))
			if err != nil {
				t.Fatal(err)
			}
			testHandler.triggerTasksForComment(ctx, issue, comment, nil, "agent", authorID, testUserID, nil, nil)
			if got := countQueuedOrDispatched(t, authorID, issueID); got != 0 {
				t.Fatalf("self_echo_runs=%d, want zero", got)
			}
		})
	}
}
