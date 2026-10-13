package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type decisionMentionPlanDB struct {
	db.DBTX
	test *testing.T
}

func (database decisionMentionPlanDB) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	if strings.Contains(query, "ListDecisionLaneMentions") {
		rows, err := database.DBTX.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				return nil, err
			}
			database.test.Log(line)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return database.DBTX.Query(ctx, query, args...)
}

func TestDecisionLaneMentionCaps(test *testing.T) {
	fixture := newIntentFixture(test)
	openLaneIntent(test, fixture, fixture.mention())
	now := time.Now()
	sessionID := ""
	for index := 0; index < 12; index++ {
		created := dbfx.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": fixture.agentID, "creator_id": fixture.humanID, "title": fmt.Sprintf("lane-%d", index), "updated_at": now.Add(-time.Duration(index) * time.Hour)})
		if index == 0 {
			sessionID = created
		}
	}
	latestIssue := ""
	for index := 0; index < 22; index++ {
		issueID := dbfx.Issue(test, fmt.Sprintf("mentioned-%d", index))
		if index == 0 {
			latestIssue = issueID
		}
		link := "[issue](mention://issue/" + issueID + ")"
		for _, role := range []string{"user", "assistant"} {
			dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": role, "content": strings.Repeat("界", 80) + link + strings.Repeat("界", 80), "created_at": now.Add(-time.Duration(index+1) * time.Minute)})
		}
	}
	for _, kind := range []string{"continuation_summary", "continuation_notice"} {
		issueID := dbfx.Issue(test, "excluded "+kind)
		dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "assistant", "message_kind": kind, "content": "[copied](mention://issue/" + issueID + ")", "created_at": now})
	}
	dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "user", "content": "[@Owner](mention://member/" + fixture.humanID + ") " + strings.Repeat("context ", 20) + "[issue[nested]](mention://issue/" + latestIssue + ") fresh\n [docs](/issue/id) [web](https://example.com/private) mentioned-0", "created_at": now})
	dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "user", "content": "[invalid](mention://issue/------------------------------------) [missing](mention://issue/00000000-0000-4000-8000-000000000001)", "created_at": now})
	foreignIssue := dbfx.Issue(test, "foreign issue", testutil.Cols{"workspace_id": dbfx.Workspace(test, "foreign", "foreign-"+fixture.agentID)})
	dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "user", "content": "[foreign](mention://issue/" + foreignIssue + ")", "created_at": now})
	originalQueries := testHandler.Queries
	testHandler.Queries = db.New(decisionMentionPlanDB{DBTX: testPool, test: test})
	test.Cleanup(func() { testHandler.Queries = originalQueries })
	var lanes []struct {
		ID       string `json:"id"`
		Mentions []struct {
			ID           string `json:"issue_id"`
			Key          string `json:"issue_key"`
			Title        string `json:"issue_title"`
			Snippet      string `json:"snippet"`
			MentionedAt  string `json:"mentioned_at"`
			RelativeTime string `json:"relative_time"`
		} `json:"issue_mentions"`
	}
	testutil.Call(test, testHandler.ListChatSessions, laneRequest(fixture, "")).Want(http.StatusOK).JSON(&lanes)
	if len(lanes) != 10 || lanes[0].ID != sessionID || len(lanes[0].Mentions) != 20 || lanes[0].Mentions[0].ID != latestIssue {
		test.Fatalf("wrong caps or order: %+v", lanes)
	}
	if !strings.Contains(lanes[0].Mentions[0].Snippet, "fresh docs web") || strings.Contains(lanes[0].Mentions[0].Snippet, "Owner") {
		test.Fatalf("snippet crossed another mention: %+v", lanes[0].Mentions[0])
	}
	for _, mention := range lanes[0].Mentions {
		if !utf8.ValidString(mention.Snippet) || utf8.RuneCountInString(mention.Snippet) > 100 || strings.Contains(mention.Snippet, "mention://") || strings.Contains(mention.Snippet, "](") || strings.Contains(mention.Snippet, "/issue/") || strings.Contains(mention.Snippet, "https://") || strings.Contains(mention.Snippet, mention.Title) || strings.Contains(mention.Snippet, mention.Key) || mention.Key == "" || mention.Title == "" || mention.MentionedAt == "" || mention.RelativeTime == "" {
			test.Fatalf("incomplete or unbounded mention: %+v", mention)
		}
	}
	if lanes[1].Mentions == nil || len(lanes[1].Mentions) != 0 {
		test.Fatalf("empty lane must have a stable empty array: %+v", lanes[1])
	}
}
