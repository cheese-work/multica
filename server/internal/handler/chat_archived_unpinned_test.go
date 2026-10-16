package handler

// chat_archived_unpinned_test.go — Addendum A2 (CHE-1801): an archived DM is
// never pinned. Archiving and continuing clear pinned_at, pinning an archived
// DM is refused, unpinning one stays allowed, and migration 627 unpins the
// archived rows that already carry a pin.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func setChatSessionArchived(t *testing.T, sessionID string, archived bool) {
	t.Helper()
	body := map[string]any{"archived": archived}
	req := withChatTestWorkspaceCtx(t, withURLParam(newRequest(http.MethodPatch, "/api/chat/sessions/"+sessionID+"/archive", body), "sessionId", sessionID))
	testutil.Call(t, testHandler.SetChatSessionArchived, req).Want(http.StatusOK)
}

func setChatSessionPinned(t *testing.T, sessionID string, pinned bool) *testutil.Response {
	t.Helper()
	req := withChatTestWorkspaceCtx(t, withURLParam(newRequest(http.MethodPatch, "/api/chat/sessions/"+sessionID+"/pin", map[string]any{"pinned": pinned}), "sessionId", sessionID))
	return testutil.Call(t, testHandler.SetChatSessionPinned, req)
}

func chatSessionPinned(t *testing.T, sessionID string) bool {
	t.Helper()
	var pinned bool
	dbfx.QueryRow(t, `SELECT pinned_at IS NOT NULL FROM chat_session WHERE id = $1`, sessionID).Scan(&pinned)
	return pinned
}

// chatSessionFor inserts a user-created DM; the pin route only serves those.
func chatSessionFor(t *testing.T, agentID string, over testutil.Cols) string {
	t.Helper()
	cols := testutil.Cols{"explicitly_created_at": testutil.Raw("now()")}
	for k, v := range over {
		cols[k] = v
	}
	return dbfx.ChatSession(t, agentID, cols)
}

func newPinnedChatSession(t *testing.T, name string) string {
	t.Helper()
	agentID := createHandlerTestAgent(t, name, []byte("[]"))
	return chatSessionFor(t, agentID, testutil.Cols{"pinned_at": testutil.Raw("now()")})
}

func TestArchivingAChatSessionClearsItsPin(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	sessionID := newPinnedChatSession(t, "archive-unpins")

	setChatSessionArchived(t, sessionID, true)

	if chatSessionPinned(t, sessionID) {
		t.Error("archived DM kept pinned_at; archived means unpinned")
	}
	setChatSessionArchived(t, sessionID, false)
	if chatSessionPinned(t, sessionID) {
		t.Error("unarchive restored the pin; it must leave the DM unpinned")
	}
}

func TestContinuingAPinnedChatSessionMovesThePinToTheNewDM(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	sessionID := newPinnedChatSession(t, "continue-moves-pin")
	dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "user", "content": "goal"})

	response := continueChatRequest(t, sessionID, "Goal: kept.", "")
	if response.Code != http.StatusCreated {
		t.Fatalf("continue: %d %s", response.Code, response.Body.String())
	}
	var result ChatContinueResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	dbfx.Cleanup(t, `DELETE FROM chat_message WHERE chat_session_id = $1`, result.Session.ID)
	dbfx.Cleanup(t, `DELETE FROM chat_session WHERE id = $1`, result.Session.ID)

	if chatSessionPinned(t, sessionID) {
		t.Error("continued (archived) source kept its pin")
	}
	if !chatSessionPinned(t, result.Session.ID) {
		t.Error("continued DM lost the copied pin")
	}
}

func TestPinningAnArchivedChatSessionIsRefused(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "pin-archived", []byte("[]"))
	sessionID := chatSessionFor(t, agentID, testutil.Cols{"status": "archived"})

	setChatSessionPinned(t, sessionID, true).Want(http.StatusConflict)

	if chatSessionPinned(t, sessionID) {
		t.Error("a refused pin request still pinned the archived DM")
	}
}

func TestUnpinningAnArchivedChatSessionSucceeds(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "unpin-archived", []byte("[]"))
	// A row archived before this layer can still carry its old pin.
	sessionID := chatSessionFor(t, agentID, testutil.Cols{"status": "archived", "pinned_at": testutil.Raw("now()")})

	setChatSessionPinned(t, sessionID, false).Want(http.StatusOK)

	if chatSessionPinned(t, sessionID) {
		t.Error("unpin on an archived DM did not clear pinned_at")
	}
}

func TestPinningAnActiveChatSessionStillWorks(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "pin-active", []byte("[]"))
	sessionID := chatSessionFor(t, agentID, nil)

	setChatSessionPinned(t, sessionID, true).Want(http.StatusOK)

	if !chatSessionPinned(t, sessionID) {
		t.Error("pin on an active DM did not set pinned_at")
	}
}

func TestUnpinArchivedChatSessionsMigration(t *testing.T) {
	if testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	matches, err := filepath.Glob("../../migrations/*_unpin_archived_chat_sessions.up.sql")
	if err != nil || len(matches) != 1 {
		t.Fatalf("want exactly one *_unpin_archived_chat_sessions.up.sql migration, got %v (err %v)", matches, err)
	}
	migration, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	agentID := createHandlerTestAgent(t, "migration-unpin", []byte("[]"))
	archivedPinned := chatSessionFor(t, agentID, testutil.Cols{"status": "archived", "pinned_at": testutil.Raw("now()")})
	archivedPlain := chatSessionFor(t, agentID, testutil.Cols{"status": "archived"})
	activePinned := chatSessionFor(t, agentID, testutil.Cols{"pinned_at": testutil.Raw("now()")})

	ctx := context.Background()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// Twice: a replay must be harmless.
	for range 2 {
		if _, err := tx.Exec(ctx, string(migration)); err != nil {
			t.Fatalf("migration: %v", err)
		}
	}
	for id, wantPinned := range map[string]bool{archivedPinned: false, archivedPlain: false, activePinned: true} {
		var pinned bool
		if err := tx.QueryRow(ctx, `SELECT pinned_at IS NOT NULL FROM chat_session WHERE id = $1`, id).Scan(&pinned); err != nil {
			t.Fatal(err)
		}
		if pinned != wantPinned {
			t.Errorf("session %s pinned = %v after migration, want %v", id, pinned, wantPinned)
		}
	}
}
