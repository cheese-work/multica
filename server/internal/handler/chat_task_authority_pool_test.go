package handler

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The compaction decision runs under the DM lock inside a transaction. Every
// lookup, including the creator's right to invoke a shared compaction agent,
// must use that transaction's connection: a second pool connection deadlocks
// concurrent compactions once the pool is exhausted by lock holders.
func TestChatTaskAuthorityDecisionStaysOnTheTransactionConnection(t *testing.T) {
	a := newAuthorityFixture(t)
	// A shared agent owned by someone else, so the check reads its invocation
	// targets and the creator's membership rather than short-circuiting on owner.
	otherOwner := dbfx.User(t, "pool-owner", fmt.Sprintf("pool-owner-%d@example.test", time.Now().UnixNano()))
	dbfx.Exec(t, `UPDATE agent SET owner_id = $1 WHERE id = $2`, otherOwner, a.compactAgent)

	cfg := testPool.Config().Copy()
	cfg.MaxConns = 1
	cfg.MinConns = 0
	small, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer small.Close()
	hub := realtime.NewHub()
	go hub.Run()
	h := New(db.New(small), small, hub, events.New(), service.NewEmailService(), nil, nil, analytics.NoopClient{}, Config{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := small.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())

	session := db.ChatSession{
		ID:          parseUUID(a.session),
		WorkspaceID: parseUUID(testWorkspaceID),
		CreatorID:   parseUUID(testUserID),
		AgentID:     parseUUID(a.dmAgent),
	}
	task := db.AgentTaskQueue{
		ID:            parseUUID(a.task(a.compactAgent, "running", taskPurposeCompaction)),
		AgentID:       parseUUID(a.compactAgent),
		ChatSessionID: session.ID,
		Status:        "running",
		Purpose:       pgtype.Text{String: taskPurposeCompaction, Valid: true},
	}
	refusal := h.decideChatTask(ctx, h.Queries.WithTx(tx), chatOpCompact, task, session, testWorkspaceID)
	if refusal != nil {
		t.Fatalf("decision on the only connection = %+v, want granted", refusal)
	}
}
