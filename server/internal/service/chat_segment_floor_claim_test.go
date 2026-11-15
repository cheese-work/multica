package service

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A question sent before a compact must sit beside its answer after the claim
// of a retry child (input owned by the root task) or of a requeued original
// send, and the claim input must still be the question exactly once (CHE-2819).
func TestClaimMovesChatInputAcrossTheSegmentFloor(t *testing.T) {
	for _, tc := range []struct {
		name  string
		child bool
	}{{"retry child", true}, {"requeued original", false}} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newResolveOriginatorPool(t)
			ctx := context.Background()
			q := db.New(pool)
			workspaceID, userID, agentID, _ := seedAttributionFixture(t, pool)

			var sessionID string
			if err := pool.QueryRow(ctx, `
				INSERT INTO chat_session (workspace_id, agent_id, creator_id)
				VALUES ($1, $2, $3) RETURNING id`, workspaceID, agentID, userID).Scan(&sessionID); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				pool.Exec(context.Background(), `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID)
				pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, sessionID)
				pool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, sessionID)
			})

			// The root turn failed; its question predates the compact.
			var rootID, childID string
			if err := pool.QueryRow(ctx, `
				INSERT INTO agent_task_queue (agent_id, runtime_id, chat_session_id, status, priority)
				VALUES ($1, (SELECT runtime_id FROM agent WHERE id = $1), $2, 'failed', 2) RETURNING id::text`, agentID, sessionID).Scan(&rootID); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE agent_task_queue SET chat_input_task_id = id WHERE id = $1`, rootID); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `
				INSERT INTO chat_message (chat_session_id, role, content, task_id, created_at)
				VALUES ($1, 'user', 'the question', $2, now() - interval '1 minute')`, sessionID, rootID); err != nil {
				t.Fatal(err)
			}

			divider, err := q.CreateChatCompactionDivider(ctx, db.CreateChatCompactionDividerParams{
				ID: util.MustParseUUID("01a19bb0-02d1-7c64-83b3-f0f81b385843"), ChatSessionID: util.MustParseUUID(sessionID), Content: "summary",
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := q.CompactChatSessionInPlace(ctx, db.CompactChatSessionInPlaceParams{ID: util.MustParseUUID(sessionID), DividerID: divider.ID}); err != nil {
				t.Fatal(err)
			}

			inputOwner := rootID
			if tc.child {
				if err := pool.QueryRow(ctx, `
					INSERT INTO agent_task_queue (agent_id, runtime_id, chat_session_id, status, priority, parent_task_id, chat_input_task_id)
					VALUES ($1, (SELECT runtime_id FROM agent WHERE id = $1), $2, 'queued', 2, $3, $3) RETURNING id::text`, agentID, sessionID, rootID).Scan(&childID); err != nil {
					t.Fatal(err)
				}
			} else if _, err := pool.Exec(ctx, `UPDATE agent_task_queue SET status = 'queued' WHERE id = $1`, rootID); err != nil {
				t.Fatal(err)
			}

			svc := NewTaskService(q, pool, nil, events.New())
			task, err := svc.ClaimTask(ctx, util.MustParseUUID(agentID))
			if err != nil || task == nil {
				t.Fatalf("ClaimTask = %v, %v; want the queued chat task", task, err)
			}
			batch, err := q.ListChatInputMessages(ctx, util.MustParseUUID(inputOwner))
			if err != nil || len(batch) != 1 || batch[0].Content != "the question" {
				t.Fatalf("claim input = %+v (%v), want the question once", batch, err)
			}
			if !batch[0].CreatedAt.Time.After(divider.CreatedAt.Time) {
				t.Errorf("question at %v is not after the divider %v", batch[0].CreatedAt.Time, divider.CreatedAt.Time)
			}
		})
	}
}
