package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func seedChatMessage(t *testing.T, sessionID, role, taskID string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(ctx(), `
		INSERT INTO chat_message (chat_session_id, role, content, task_id)
		VALUES ($1, $2, 'x', NULLIF($3, '')::uuid) RETURNING id`, sessionID, role, taskID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// segmentFloor returns the DM's floor position; set is false when never compacted.
func segmentFloor(t *testing.T, sessionID string) (at time.Time, id string, set bool) {
	t.Helper()
	var floorAt *time.Time
	var floorID *string
	if err := testPool.QueryRow(ctx(), `SELECT segment_floor_at, segment_floor_id::text FROM chat_session WHERE id = $1`, sessionID).Scan(&floorAt, &floorID); err != nil {
		t.Fatal(err)
	}
	if floorAt == nil || floorID == nil {
		return time.Time{}, "", false
	}
	return *floorAt, *floorID, true
}

// (a)+(g): the floor is the divider's position, strictly after every earlier
// message, and a second compact moves it strictly forward.
func TestChatCompactSetsSegmentFloorAtTheDivider(t *testing.T) {
	t.Skip("CHE-4873 quarantine (CHE-4909): seeds two active tasks in one chat session; idx_agent_task_queue_one_active_per_chat rejects it")
	fx := newCompactFixture(t)
	seedChatMessage(t, fx.sessionID, "user", "")
	seedChatMessage(t, fx.sessionID, "assistant", "")
	var priorMax time.Time
	if err := testPool.QueryRow(ctx(), `SELECT max(created_at) FROM chat_message WHERE chat_session_id = $1`, fx.sessionID).Scan(&priorMax); err != nil {
		t.Fatal(err)
	}

	if status, body := postCompact(t, fx.token, "First summary."); status != http.StatusOK {
		t.Fatalf("compact = %d %s, want 200", status, body)
	}
	firstAt, firstID, set := segmentFloor(t, fx.sessionID)
	if !set {
		t.Fatal("segment floor not set after compact")
	}
	var dividerID string
	var dividerAt time.Time
	if err := testPool.QueryRow(ctx(), `SELECT id::text, created_at FROM chat_message WHERE chat_session_id = $1 AND message_kind = 'compaction_summary'`, fx.sessionID).Scan(&dividerID, &dividerAt); err != nil {
		t.Fatal(err)
	}
	if firstID != dividerID || !firstAt.Equal(dividerAt) {
		t.Errorf("floor = (%v, %s), want the divider (%v, %s)", firstAt, firstID, dividerAt, dividerID)
	}
	if !dividerAt.After(priorMax) {
		t.Errorf("divider %v not strictly after earlier messages %v", dividerAt, priorMax)
	}

	// A second compact from a new running task: the floor moves forward, no tie.
	token2 := chatTaskToken(t, fx.sessionID, "running")
	if _, err := testPool.Exec(ctx(), `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fx.taskID); err != nil {
		t.Fatal(err)
	}
	if status, body := postCompact(t, token2, "Second summary."); status != http.StatusOK {
		t.Fatalf("second compact = %d %s, want 200", status, body)
	}
	secondAt, secondID, _ := segmentFloor(t, fx.sessionID)
	if !secondAt.After(firstAt) || secondID == firstID {
		t.Errorf("second floor (%v, %s) not after first (%v, %s)", secondAt, secondID, firstAt, firstID)
	}
}

// (d)+(m), Rule W: a message whose INSERT statement runs after a compact
// committed lands above the floor, even when its transaction began earlier.
func TestChatCreateMessageLandsAboveTheFloorAfterACompact(t *testing.T) {
	fx := newCompactFixture(t)
	tx, err := testPool.Begin(ctx())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx())
	var txStart time.Time
	if err := tx.QueryRow(ctx(), `SELECT now()`).Scan(&txStart); err != nil { // pins the transaction start
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)

	if status, body := postCompact(t, fx.token, "Summary."); status != http.StatusOK {
		t.Fatalf("compact = %d %s, want 200", status, body)
	}
	floorAt, floorID, set := segmentFloor(t, fx.sessionID)
	if !set {
		t.Fatal("segment floor not set after compact")
	}

	reply, err := db.New(tx).CreateChatMessage(ctx(), db.CreateChatMessageParams{
		ChatSessionID: parseTestUUID(t, fx.sessionID),
		Role:          "assistant", Content: "the compacting run's answer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx()); err != nil {
		t.Fatal(err)
	}
	if !reply.CreatedAt.Time.After(floorAt) {
		t.Errorf("reply created_at %v is not after the floor %v (tx started %v, divider %s)", reply.CreatedAt.Time, floorAt, txStart, floorID)
	}
}

// Step 4: ReanchorChatInputAcrossFloor moves a batch that sits at or before the
// floor to just after the latest row, keeps its order, and leaves a batch above
// the floor (and a never-compacted DM) alone.
func TestReanchorChatInputAcrossFloor(t *testing.T) {
	fx := newCompactFixture(t)
	sid := parseTestUUID(t, fx.sessionID)
	tid := parseTestUUID(t, fx.taskID)
	q := db.New(testPool)
	order := func() []string {
		rows, err := testPool.Query(ctx(), `SELECT id::text FROM chat_message WHERE chat_session_id = $1 ORDER BY created_at, id`, fx.sessionID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		return ids
	}
	u1 := seedChatMessage(t, fx.sessionID, "user", fx.taskID)
	u2 := seedChatMessage(t, fx.sessionID, "user", fx.taskID)
	reanchor := func() {
		if err := q.ReanchorChatInputAcrossFloor(ctx(), db.ReanchorChatInputAcrossFloorParams{
			ChatSessionID: sid, InputTaskID: tid, DispatchedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		}); err != nil {
			t.Fatal(err)
		}
	}

	before := order()
	reanchor() // no floor yet: no-op
	if got := order(); len(got) != 2 || got[0] != before[0] || got[1] != before[1] {
		t.Fatalf("never-compacted DM reordered: %v -> %v", before, got)
	}

	if status, body := postCompact(t, fx.token, "Summary."); status != http.StatusOK {
		t.Fatalf("compact = %d %s, want 200", status, body)
	}
	_, dividerID, _ := segmentFloor(t, fx.sessionID)
	reanchor()
	got := order()
	if len(got) != 3 || got[0] != dividerID || got[1] != u1 || got[2] != u2 {
		t.Fatalf("order after reanchor = %v, want [divider %s, %s, %s]", got, dividerID, u1, u2)
	}
	var marked int
	if err := testPool.QueryRow(ctx(), `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND reanchored_at IS NOT NULL`, fx.sessionID).Scan(&marked); err != nil {
		t.Fatal(err)
	}
	if marked != 2 {
		t.Errorf("reanchored_at set on %d rows, want exactly the 2 moved input rows", marked)
	}
	floorAt, _, _ := segmentFloor(t, fx.sessionID)
	var minBatch time.Time
	if err := testPool.QueryRow(ctx(), `SELECT min(created_at) FROM chat_message WHERE id = ANY($1::uuid[])`, []string{u1, u2}).Scan(&minBatch); err != nil {
		t.Fatal(err)
	}
	if !minBatch.After(floorAt) {
		t.Errorf("batch %v not above floor %v", minBatch, floorAt)
	}

	reanchor() // already above the floor: idempotent
	if again := order(); again[1] != u1 || again[2] != u2 {
		t.Errorf("second reanchor changed order: %v", again)
	}
}

// A batch older than a later decision ask card must not be restamped past it:
// the answer binding trusts "after the ping" as proof the human replied to it.
func TestReanchorChatInputAcrossFloorSkipsBatchOlderThanAskCard(t *testing.T) {
	fx := newCompactFixture(t)
	sid := parseTestUUID(t, fx.sessionID)
	tid := parseTestUUID(t, fx.taskID)
	u1 := seedChatMessage(t, fx.sessionID, "user", fx.taskID)
	if status, body := postCompact(t, fx.token, "Summary."); status != http.StatusOK {
		t.Fatalf("compact = %d %s, want 200", status, body)
	}
	var cardID string
	if err := testPool.QueryRow(ctx(), `
		INSERT INTO chat_message (chat_session_id, role, content, decision_request_ids)
		VALUES ($1, 'assistant', 'ask', ARRAY[gen_random_uuid()]) RETURNING id::text`, fx.sessionID).Scan(&cardID); err != nil {
		t.Fatal(err)
	}
	var before time.Time
	if err := testPool.QueryRow(ctx(), `SELECT created_at FROM chat_message WHERE id = $1`, u1).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := db.New(testPool).ReanchorChatInputAcrossFloor(ctx(), db.ReanchorChatInputAcrossFloorParams{
		ChatSessionID: sid, InputTaskID: tid, DispatchedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	var after, cardAt time.Time
	if err := testPool.QueryRow(ctx(), `SELECT created_at FROM chat_message WHERE id = $1`, u1).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx(), `SELECT created_at FROM chat_message WHERE id = $1`, cardID).Scan(&cardAt); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(before) || after.After(cardAt) {
		t.Errorf("batch restamped past ask card: before=%v after=%v card=%v", before, after, cardAt)
	}
}

// The answer binding accepts a source DM other than the ask's DM when the same
// human owns both, so a later ask card in a sibling DM also pins old input.
func TestReanchorChatInputAcrossFloorSkipsBatchOlderThanAskCardInSiblingDM(t *testing.T) {
	fx := newCompactFixture(t)
	sid := parseTestUUID(t, fx.sessionID)
	tid := parseTestUUID(t, fx.taskID)
	u1 := seedChatMessage(t, fx.sessionID, "user", fx.taskID)
	if status, body := postCompact(t, fx.token, "Summary."); status != http.StatusOK {
		t.Fatalf("compact = %d %s, want 200", status, body)
	}
	var siblingID, cardID string
	if err := testPool.QueryRow(ctx(), `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, session_id, runtime_id, work_dir)
		SELECT workspace_id, agent_id, creator_id, 'sibling', 'S_sib', runtime_id, '/tmp/compact-work'
		FROM chat_session WHERE id = $1 RETURNING id::text`, fx.sessionID).Scan(&siblingID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx(), `DELETE FROM chat_message WHERE chat_session_id = $1`, siblingID)
		testPool.Exec(ctx(), `DELETE FROM chat_session WHERE id = $1`, siblingID)
	})
	if err := testPool.QueryRow(ctx(), `
		INSERT INTO chat_message (chat_session_id, role, content, decision_request_ids)
		VALUES ($1, 'assistant', 'ask', ARRAY[gen_random_uuid()]) RETURNING id::text`, siblingID).Scan(&cardID); err != nil {
		t.Fatal(err)
	}
	var before, after time.Time
	if err := testPool.QueryRow(ctx(), `SELECT created_at FROM chat_message WHERE id = $1`, u1).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := db.New(testPool).ReanchorChatInputAcrossFloor(ctx(), db.ReanchorChatInputAcrossFloorParams{
		ChatSessionID: sid, InputTaskID: tid, DispatchedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx(), `SELECT created_at FROM chat_message WHERE id = $1`, u1).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(before) {
		t.Errorf("batch restamped past a sibling DM ask card: before=%v after=%v", before, after)
	}
}
