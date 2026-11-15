package handler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// CHE-2819 L1b-2: a compacted DM starts its next provider session from the
// divider summary plus the current segment, built from stored rows at claim.

type carryOverDM struct {
	agentID, sessionID, runtimeID, daemonID string
}

// newCarryOverDM builds a DM with an old segment: question, answer, a follow-up
// question after that answer. Rows are spaced in the past so a later compact
// divider and later writes sort after them.
func newCarryOverDM(t *testing.T) carryOverDM {
	t.Helper()
	ctx := context.Background()
	agentID, sessionID, runtimeID, daemonID := setupDirectChatSession(t, ctx, "carry-over "+t.Name())
	for i, m := range []struct{ role, content string }{
		{"user", "OLD-QUESTION-FIRST"},
		{"assistant", "OLD-ANSWER-FIRST"},
		{"user", "OLD-QUESTION-SECOND"},
		{"assistant", "OLD-ANSWER-LAST"},
		{"user", "OLD-USER-AFTER-ANSWER"},
	} {
		dbfx.Exec(t, `INSERT INTO chat_message (chat_session_id, role, content, created_at) VALUES ($1, $2, $3, now() - $4::interval)`,
			sessionID, m.role, m.content, (time.Duration(10-i) * time.Minute).String())
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, sessionID)
		testPool.Exec(ctx, `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID)
	})
	return carryOverDM{agentID, sessionID, runtimeID, daemonID}
}

// compact writes the divider and floor the way the in-place compact does.
func (dm carryOverDM) compact(t *testing.T, summary string) {
	t.Helper()
	ctx := context.Background()
	sid := parseUUID(dm.sessionID)
	divider, err := testHandler.Queries.CreateChatCompactionDivider(ctx, db.CreateChatCompactionDividerParams{
		ID: dbid.NewV7(), ChatSessionID: sid, Content: summary,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testHandler.Queries.CompactChatSessionInPlace(ctx, db.CompactChatSessionInPlaceParams{ID: sid, DividerID: divider.ID}); err != nil {
		t.Fatal(err)
	}
}

func (dm carryOverDM) reply(t *testing.T, content string) {
	t.Helper()
	dbfx.Exec(t, `INSERT INTO chat_message (chat_session_id, role, content, created_at) VALUES ($1, 'assistant', $2, clock_timestamp())`, dm.sessionID, content)
}

// claim sends one question and returns the prompt the claim builds for it.
func (dm carryOverDM) claim(t *testing.T, question string) AgentTaskResponse {
	t.Helper()
	sendDirectChat(t, context.Background(), dm.agentID, dm.sessionID, question)
	return claimedChatTask(t, dm.runtimeID, dm.daemonID)
}

func wantInClaim(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("claim message missing %q:\n%s", want, got)
		}
	}
}

func wantNotIn(t *testing.T, got string, unwanted ...string) {
	t.Helper()
	for _, bad := range unwanted {
		if strings.Contains(got, bad) {
			t.Errorf("claim message must not contain %q:\n%s", bad, got)
		}
	}
}

// (b)+(d): summary, the compacting run's reply after the divider and the new
// question; nothing of the old segment, and no resumed session.
func TestCarryOver_ClaimCarriesSummaryAndCurrentSegmentOnly(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dm := newCarryOverDM(t)
	dm.compact(t, "SUMMARY-TEXT")
	dm.reply(t, "REPLY-AFTER-DIVIDER")
	got := dm.claim(t, "NEXT-QUESTION")
	if got.PriorSessionID != "" {
		t.Fatalf("PriorSessionID = %q, want empty", got.PriorSessionID)
	}
	wantInClaim(t, got.ChatMessage, "SUMMARY-TEXT", "REPLY-AFTER-DIVIDER", "NEXT-QUESTION")
	wantNotIn(t, got.ChatMessage, "OLD-QUESTION", "OLD-ANSWER", "OLD-USER-AFTER-ANSWER")
	if n := strings.Count(got.ChatMessage, "NEXT-QUESTION"); n != 1 {
		t.Errorf("the new question appears %d times, want once", n)
	}
}

// (e)+(f): no answer in the new segment (human compact, or the compacting run
// ended without replying): the last old answer and the questions after it.
func TestCarryOver_FallsBackToTheOldSegmentTail(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dm := newCarryOverDM(t)
	dm.compact(t, "SUMMARY-TEXT")
	got := dm.claim(t, "NEXT-QUESTION")
	wantInClaim(t, got.ChatMessage, "SUMMARY-TEXT", "OLD-ANSWER-LAST", "OLD-USER-AFTER-ANSWER", "NEXT-QUESTION")
	wantNotIn(t, got.ChatMessage, "OLD-QUESTION-FIRST", "OLD-ANSWER-FIRST", "OLD-QUESTION-SECOND")
}

// (i): a question queued before the compact reaches its own claim once: the
// claim input carries it and the carry-over block must not repeat it.
func TestCarryOver_QueuedQuestionIsDeliveredOnce(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dm := newCarryOverDM(t)
	sendDirectChat(t, context.Background(), dm.agentID, dm.sessionID, "QUEUED-BEFORE-COMPACT")
	dm.compact(t, "SUMMARY-TEXT")
	got := claimedChatTask(t, dm.runtimeID, dm.daemonID)
	wantInClaim(t, got.ChatMessage, "SUMMARY-TEXT", "QUEUED-BEFORE-COMPACT")
	if n := strings.Count(got.ChatMessage, "QUEUED-BEFORE-COMPACT"); n != 1 {
		t.Errorf("queued question appears %d times, want once:\n%s", n, got.ChatMessage)
	}
}

// The fallback stops at the previous divider: after two compacts with no answer
// between them, nothing from before the first divider is carried.
func TestCarryOver_FallbackStopsAtThePreviousDivider(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	for name, seg1 := range map[string][]string{
		"no answer in the previous segment": {"user", "SEG1-USER"},
		"answer in the previous segment":    {"assistant", "SEG1-ANSWER"},
	} {
		t.Run(name, func(t *testing.T) {
			dm := newCarryOverDM(t)
			dm.compact(t, "SUMMARY-ONE")
			dbfx.Exec(t, `INSERT INTO chat_message (chat_session_id, role, content, created_at) VALUES ($1, $2, $3, clock_timestamp())`, dm.sessionID, seg1[0], seg1[1])
			dm.compact(t, "SUMMARY-TWO")
			got := dm.claim(t, "NEXT-QUESTION")
			wantInClaim(t, got.ChatMessage, "SUMMARY-TWO", seg1[1], "NEXT-QUESTION")
			wantNotIn(t, got.ChatMessage, "SUMMARY-ONE", "OLD-ANSWER-LAST", "OLD-USER-AFTER-ANSWER", "OLD-QUESTION")
		})
	}
}

// A second queued question is not part of this claim at all.
func TestCarryOver_LaterQueuedQuestionStaysOut(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dm := newCarryOverDM(t)
	sendDirectChat(t, context.Background(), dm.agentID, dm.sessionID, "FIRST-QUEUED")
	sendDirectChat(t, context.Background(), dm.agentID, dm.sessionID, "SECOND-QUEUED")
	dm.compact(t, "SUMMARY-TEXT")
	got := claimedChatTask(t, dm.runtimeID, dm.daemonID)
	wantInClaim(t, got.ChatMessage, "FIRST-QUEUED")
	wantNotIn(t, got.ChatMessage, "SECOND-QUEUED")
}

// (j): compaction wins over the fork block: a compacted fork carries the
// summary and the current segment, no fork history and no attachment index.
func TestCarryOver_CompactedForkSkipsTheForkBlock(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	dm := newCarryOverDM(t)
	var sourceID string
	if err := testPool.QueryRow(ctx, `INSERT INTO chat_session (workspace_id, agent_id, creator_id, title) VALUES ($1, $2, $3, 'fork source') RETURNING id::text`,
		testWorkspaceID, dm.agentID, testUserID).Scan(&sourceID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM chat_session WHERE id = $1`, sourceID) })
	dbfx.Exec(t, `UPDATE chat_session SET forked_from_session_id = $2 WHERE id = $1`, dm.sessionID, sourceID)
	dm.compact(t, "SUMMARY-TEXT")
	got := dm.claim(t, "NEXT-QUESTION")
	wantInClaim(t, got.ChatMessage, "SUMMARY-TEXT", "NEXT-QUESTION")
	wantNotIn(t, got.ChatMessage, "fork_history", "fork of another DM", "Copied attachments")
}

// (l): a writer that bypasses the task path (preset command, notice, channel)
// still lands above the floor, so the claim and the UI agree it is current.
func TestCarryOver_IndependentWriterLandsInTheCurrentSegment(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dm := newCarryOverDM(t)
	dm.compact(t, "SUMMARY-TEXT")
	if _, err := testHandler.Queries.CreateChatMessage(context.Background(), db.CreateChatMessageParams{
		ChatSessionID: parseUUID(dm.sessionID), Role: "assistant", Content: "INDEPENDENT-WRITE",
	}); err != nil {
		t.Fatal(err)
	}
	wantInClaim(t, dm.claim(t, "NEXT-QUESTION").ChatMessage, "INDEPENDENT-WRITE")
}

// (m): the completion that waited behind a compact writes its answer after the
// commit; the answer is above the floor and the next claim carries it.
func TestCarryOver_AnswerCompletedBehindACompactIsCarried(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	dm := newCarryOverDM(t)
	taskID := sendDirectChat(t, ctx, dm.agentID, dm.sessionID, "RUNNING-QUESTION")
	markTaskRunning(t, ctx, taskID)

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	qtx := db.New(tx)
	if _, err := qtx.LockChatSessionForEnqueue(ctx, parseUUID(dm.sessionID)); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := testHandler.TaskService.CompleteTaskWithRunContext(ctx, parseUUID(taskID),
			[]byte(`{"output":"ANSWER-BEHIND-COMPACT"}`), "", "", "", false, "", "", (*service.RunContext)(nil))
		done <- err
	}()
	// Wait until the completion is blocked on the DM lock.
	deadline := time.Now().Add(5 * time.Second)
	for dbfx.Count(t, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query ILIKE '%chat_session%'`) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("completion never blocked on the DM lock")
		}
		time.Sleep(20 * time.Millisecond)
	}

	divider, err := qtx.CreateChatCompactionDivider(ctx, db.CreateChatCompactionDividerParams{
		ID: dbid.NewV7(), ChatSessionID: parseUUID(dm.sessionID), Content: "SUMMARY-TEXT",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := qtx.CompactChatSessionInPlace(ctx, db.CompactChatSessionInPlaceParams{ID: parseUUID(dm.sessionID), DividerID: divider.ID}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("complete: %v", err)
	}

	if n := dbfx.Count(t, `
		SELECT count(*) FROM chat_message m JOIN chat_session cs ON cs.id = m.chat_session_id
		WHERE m.chat_session_id = $1 AND m.content = 'ANSWER-BEHIND-COMPACT' AND (m.created_at, m.id) > (cs.segment_floor_at, cs.segment_floor_id)`, dm.sessionID); n != 1 {
		t.Fatalf("answer above the floor: %d rows, want 1", n)
	}
	wantInClaim(t, dm.claim(t, "NEXT-QUESTION").ChatMessage, "ANSWER-BEHIND-COMPACT")
}

// A decision withdrawn after the compaction must not reach the claim as a
// standing record: the carry-over reconciles against the live records.
func TestCarryOver_ClaimDropsDecisionsWithdrawnAfterCompaction(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dm := newCarryOverDM(t)
	kept := recordDecision(t, dm.agentID, dm.sessionID, "still standing", "Keep the cache layer.", false)
	revoked := recordDecision(t, dm.agentID, dm.sessionID, "later withdrawn", "Approve the schema drop.", false)
	if _, summary, fail := testHandler.compactChatInPlace(context.Background(), parseUUID(dm.sessionID), pgtype.UUID{}, "Nothing kept."); fail != nil {
		t.Fatalf("compact: %s", fail.message)
	} else {
		requireDecisions(t, "row at compaction", summary.Content, kept, revoked)
	}
	dbfx.Exec(t, `UPDATE decision_answer SET disclaimed_at = now() WHERE id = $1`, revoked.answerID)
	got := dm.claim(t, "NEXT-QUESTION").ChatMessage
	// The fixture's source message sits in the old-segment tail; only the
	// summary block holds the server records.
	summary, _, _ := strings.Cut(got, carryOverSummaryClose)
	requireDecisions(t, "claim summary", summary, kept)
	wantNotIn(t, summary, revoked.words)
	wantNotIn(t, got, revoked.answerID)
}
