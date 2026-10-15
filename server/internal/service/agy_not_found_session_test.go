package service

import (
	"context"
	"testing"

	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A daemon without the CHE-1715 classifier stores the AGY replayed 404 as
// agent_error.unknown; the resume queries must still skip that session.
const agyNotFoundRow = "agent executor error: generating and executing: Error 404, Message: Requested entity was not found., Status: NOT_FOUND, Details: []"

func TestAGYNotFoundLegacySessionSelection(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	workspaceID, userID, agentID, issueID := seedAttributionFixture(t, pool)
	fixture := dbfx.New(pool, workspaceID, userID)
	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	fixture.Task(t, agentID, dbfx.Cols{
		"runtime_id": runtimeID, "issue_id": issueID, "status": "completed", "session_id": "older-distinct-session",
		"completed_at": dbfx.Raw("now() - interval '2 minutes'"),
	})
	fixture.Task(t, agentID, dbfx.Cols{
		"runtime_id": runtimeID, "issue_id": issueID, "status": "completed", "session_id": "agy-poisoned",
		"completed_at": dbfx.Raw("now() - interval '1 minute'"),
	})
	fixture.Task(t, agentID, dbfx.Cols{
		"runtime_id": runtimeID, "issue_id": issueID, "status": "failed", "session_id": "agy-poisoned",
		"failure_reason": "agent_error.unknown", "completed_at": dbfx.Raw("now()"), "error": agyNotFoundRow,
	})
	prior, err := db.New(pool).GetLastTaskSession(ctx, db.GetLastTaskSessionParams{
		AgentID: util.MustParseUUID(agentID), IssueID: util.MustParseUUID(issueID),
	})
	if err != nil || prior.SessionID.String != "older-distinct-session" {
		t.Errorf("GetLastTaskSession = (%q, %v), want the older healthy session", prior.SessionID.String, err)
	}
}

func TestAGYNotFoundLegacyChatSessionSelection(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	workspaceID, userID, agentID, _ := seedAttributionFixture(t, pool)
	fixture := dbfx.New(pool, workspaceID, userID)
	chatID := fixture.Insert(t, "chat_session", dbfx.Cols{
		"workspace_id": workspaceID, "creator_id": userID, "agent_id": agentID, "title": "agy chat",
	})
	fixture.Task(t, agentID, dbfx.Cols{
		"chat_session_id": chatID, "status": "completed", "session_id": "older-distinct-chat-session",
		"completed_at": dbfx.Raw("now() - interval '2 minutes'"),
	})
	fixture.Task(t, agentID, dbfx.Cols{
		"chat_session_id": chatID, "status": "completed", "session_id": "agy-chat",
		"completed_at": dbfx.Raw("now() - interval '1 minute'"),
	})
	fixture.Task(t, agentID, dbfx.Cols{
		"chat_session_id": chatID, "status": "failed", "session_id": "agy-chat",
		"failure_reason": "agent_error.unknown", "completed_at": dbfx.Raw("now()"), "error": agyNotFoundRow,
	})
	prior, err := db.New(pool).GetLastChatTaskSession(ctx, db.GetLastChatTaskSessionParams{ChatSessionID: util.MustParseUUID(chatID)})
	if err != nil || prior.SessionID.String != "older-distinct-chat-session" {
		t.Errorf("GetLastChatTaskSession = (%q, %v), want the older healthy session", prior.SessionID.String, err)
	}
}
