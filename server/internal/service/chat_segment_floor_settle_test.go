package service

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// newFloorDM seeds a DM and returns the ids the segment-floor path tests need.
func newFloorDM(t *testing.T, pool *pgxpool.Pool) (sessionID, agentID, runtimeID string) {
	t.Helper()
	ctx := context.Background()
	workspaceID, userID, agentID, _ := seedAttributionFixture(t, pool)
	if err := pool.QueryRow(ctx, `
		INSERT INTO chat_session (workspace_id, agent_id, creator_id)
		VALUES ($1, $2, $3) RETURNING id::text`, workspaceID, agentID, userID).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID)
		pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, sessionID)
		pool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, sessionID)
	})
	return sessionID, agentID, runtimeID
}

// seedDirectTurn inserts an original-send task (it owns its input) and its question.
func seedDirectTurn(t *testing.T, pool *pgxpool.Pool, sessionID, agentID, runtimeID, status, question, askedAgo string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, chat_session_id, status, priority)
		VALUES ($1, $2, $3, $4, 2) RETURNING id::text`, agentID, runtimeID, sessionID, status).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_task_queue SET chat_input_task_id = id WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO chat_message (chat_session_id, role, content, task_id, created_at)
		VALUES ($1, 'user', $2, $3, now() - $4::interval)`, sessionID, question, id, askedAgo); err != nil {
		t.Fatal(err)
	}
	return id
}

// Plan v5 test (i), original send at reply settle: B is queued behind the
// compacting run A. When A's reply settles, B's question (older than the
// divider) must land after the reply, and still be B's claim input once.
func TestReplySettleMovesQueuedQuestionAcrossTheSegmentFloor(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	sessionID, agentID, runtimeID := newFloorDM(t, pool)
	sid := util.MustParseUUID(sessionID)

	taskA := seedDirectTurn(t, pool, sessionID, agentID, runtimeID, "running", "question A", "2 minutes")
	taskB := seedDirectTurn(t, pool, sessionID, agentID, runtimeID, "queued", "question B", "1 minute")

	divider, err := q.CreateChatCompactionDivider(ctx, db.CreateChatCompactionDividerParams{
		ID: util.MustParseUUID("01a19bb0-02d1-7c64-83b3-f0f81b385850"), ChatSessionID: sid, Content: "summary",
		TaskID: util.MustParseUUID(taskA),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.CompactChatSessionInPlace(ctx, db.CompactChatSessionInPlaceParams{ID: sid, DividerID: divider.ID}); err != nil {
		t.Fatal(err)
	}
	// The settling task leaves the visible-head status set before the reply is written.
	if _, err := pool.Exec(ctx, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, taskA); err != nil {
		t.Fatal(err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	reply, err := createAssistantChatMessage(ctx, db.New(tx), db.CreateChatMessageParams{
		ChatSessionID: sid, Role: "assistant", Content: "the compacting run's answer", TaskID: util.MustParseUUID(taskA),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	rows, err := pool.Query(ctx, `SELECT content FROM chat_message WHERE chat_session_id = $1 ORDER BY created_at, id`, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var order []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		order = append(order, c)
	}
	want := []string{"question A", "summary", "the compacting run's answer", "question B"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v (reply at %v, divider at %v)", order, want, reply.CreatedAt.Time, divider.CreatedAt.Time)
		}
	}
	batch, err := q.ListChatInputMessages(ctx, util.MustParseUUID(taskB))
	if err != nil || len(batch) != 1 || batch[0].Content != "question B" {
		t.Fatalf("claim input of B = %+v (%v), want the question once", batch, err)
	}
}

// Plan v5 test (i)/(s), stale-dispatch reclaim: a dispatched task blocks the
// compact of the DM (the guard counts it as another active task), so reclaim
// cannot cross a floor. Reclaim redelivers the same task and its question once.
func TestStaleDispatchReclaimKeepsTheEpochAndDeliversInputOnce(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	sessionID, agentID, runtimeID := newFloorDM(t, pool)
	sid := util.MustParseUUID(sessionID)

	dispatched := seedDirectTurn(t, pool, sessionID, agentID, runtimeID, "dispatched", "question D", "2 minutes")
	compacter := seedDirectTurn(t, pool, sessionID, agentID, runtimeID, "running", "question C", "1 minute")
	if _, err := pool.Exec(ctx, `UPDATE agent_task_queue SET dispatched_at = now() - interval '1 hour' WHERE id = $1`, dispatched); err != nil {
		t.Fatal(err)
	}
	blocked := func(when string) {
		t.Helper()
		n, err := q.CountOtherActiveChatTasks(ctx, db.CountOtherActiveChatTasksParams{ChatSessionID: sid, ID: util.MustParseUUID(compacter)})
		if err != nil || n != 1 {
			t.Fatalf("%s: compact guard counts %d other active tasks (%v), want 1", when, n, err)
		}
	}
	blocked("before reclaim")

	reclaimed, err := q.ReclaimStaleDispatchedTaskForRuntime(ctx, db.ReclaimStaleDispatchedTaskForRuntimeParams{
		RuntimeID: util.MustParseUUID(runtimeID), ClaimRecoverySecs: 1, PrepareLeaseSecs: 60, RuntimeStaleSecs: 1e9,
	})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if util.UUIDToString(reclaimed.ID) != dispatched {
		t.Fatalf("reclaimed %s, want the dispatched task %s", util.UUIDToString(reclaimed.ID), dispatched)
	}
	blocked("after reclaim")

	var floorSet bool
	if err := pool.QueryRow(ctx, `SELECT segment_floor_at IS NOT NULL FROM chat_session WHERE id = $1`, sessionID).Scan(&floorSet); err != nil || floorSet {
		t.Fatalf("floor set = %v (%v), want no floor", floorSet, err)
	}
	batch, err := q.ListChatInputMessages(ctx, reclaimed.ID)
	if err != nil || len(batch) != 1 || batch[0].Content != "question D" {
		t.Fatalf("claim input = %+v (%v), want the question once", batch, err)
	}
}
