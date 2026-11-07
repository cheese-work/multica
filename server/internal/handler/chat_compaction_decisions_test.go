package handler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-4447: a compaction must carry every recorded decision of the DM into the
// next session, whatever the agent's own summary kept.

type recordedDecision struct{ key, words, answerID string }

// recordDecision stores one answered, standing decision of sessionID. A card
// answer has no chat source; an agent record quotes a DM message.
func recordDecision(t *testing.T, agentID, sessionID, title, words string, card bool) recordedDecision {
	t.Helper()
	issueID := dbfx.Issue(t, title)
	var key string
	dbfx.QueryRow(t, `SELECT workspace.issue_prefix || '-' || issue.number::text FROM issue JOIN workspace ON workspace.id = issue.workspace_id WHERE issue.id = $1`, issueID).Scan(&key)
	requestID := dbfx.Insert(t, "decision_request", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": issueID, "asking_agent_id": agentID, "human_user_id": testUserID, "chat_session_id": sessionID, "escalation_class": "reversible", "origin": "asked", "status": "answered"})
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM decision_request WHERE id = $1`, requestID) })
	var answerID string
	if card {
		answerID = dbfx.Insert(t, "decision_answer", testutil.Cols{"decision_request_id": requestID, "answered_by_user_id": testUserID, "answer_source": "card", "chat_message_ids": "{}", "content_snapshot": words, "reply_text": words, "effective_at": time.Now()})
	} else {
		messageID := dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "user", "content": words})
		taskID := dbfx.Task(t, agentID, testutil.Cols{"status": "completed", "chat_session_id": sessionID, "runtime_id": handlerTestRuntimeID(t)})
		answerID = dbfx.Insert(t, "decision_answer", testutil.Cols{"decision_request_id": requestID, "answered_by_task_id": taskID, "source_chat_session_id": sessionID, "chat_message_ids": "{" + messageID + "}", "content_snapshot": words, "effective_at": time.Now()})
	}
	return recordedDecision{key: key, words: words, answerID: answerID}
}

func requireDecisions(t *testing.T, where, text string, want ...recordedDecision) {
	t.Helper()
	if !strings.Contains(text, "Decisions in this DM") {
		t.Fatalf("%s lacks the decisions block:\n%s", where, text)
	}
	for _, decision := range want {
		for _, part := range []string{decision.key, decision.words, decision.answerID, "ICT"} {
			if !strings.Contains(text, part) {
				t.Fatalf("%s lacks %q of decision %s:\n%s", where, part, decision.key, text)
			}
		}
	}
}

func TestCompactionCarriesEveryRecordedDecision(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID := createHandlerTestAgent(t, "compaction-decisions", []byte("[]"))
	sessionID := dbfx.ChatSession(t, agentID)
	first := recordDecision(t, agentID, sessionID, "decision one", "Ship the reversible default.", false)
	second := recordDecision(t, agentID, sessionID, "decision two", "Use option B for the retry cap.", false)
	third := recordDecision(t, agentID, sessionID, "decision three", "Approve the card as asked.", true)
	// Not this DM's: another DM's record, and a withdrawn one.
	otherSession := dbfx.ChatSession(t, agentID)
	other := recordDecision(t, agentID, otherSession, "other dm decision", "Other DM words must not leak.", false)
	withdrawn := recordDecision(t, agentID, sessionID, "withdrawn decision", "Withdrawn words must not appear.", false)
	dbfx.Exec(t, `UPDATE decision_answer SET disclaimed_at = now() WHERE id = $1`, withdrawn.answerID)

	// The agent's summary keeps none of them.
	session, summary, fail := testHandler.compactChatInPlace(ctx, parseUUID(sessionID), pgtype.UUID{}, "Goal: refactor. Nothing else.")
	if fail != nil {
		t.Fatalf("compact: %s", fail.message)
	}
	requireDecisions(t, "folded summary row", summary.Content, first, second, third)
	seed, err := testHandler.compactionSeedBlock(ctx, session, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireDecisions(t, "next session prompt", seed, first, second, third)
	for _, leaked := range []string{other.words, withdrawn.words} {
		if strings.Contains(seed, leaked) || strings.Contains(summary.Content, leaked) {
			t.Fatalf("block contains %q, which is not a standing decision of this DM", leaked)
		}
	}
	if strings.Index(seed, third.words) > strings.Index(seed, first.words) {
		t.Fatalf("decisions are not newest first:\n%s", seed)
	}

	// A later compaction keeps the older decisions and adds the new one.
	fourth := recordDecision(t, agentID, sessionID, "decision four", "Keep the DM block under 8 KB.", true)
	session, summary, fail = testHandler.compactChatInPlace(ctx, session.ID, pgtype.UUID{}, "Second summary, also silent.")
	if fail != nil {
		t.Fatalf("second compact: %s", fail.message)
	}
	seed, err = testHandler.compactionSeedBlock(ctx, session, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireDecisions(t, "second prompt", seed, first, second, third, fourth)
	requireDecisions(t, "second folded row", summary.Content, first, second, third, fourth)
}
