package handler

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestCreateRetryTask_KeepsPurpose locks CHE-2894 ENG-3: a retry clone keeps
// what its parent was for, in both the queued and the deferred (fire_at) form.
func TestCreateRetryTask_KeepsPurpose(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	fx := newSquadCommentTriggerFixture(t)
	issueID := uuidToString(fx.Issue.ID)

	var runtimeID string
	if err := testPool.QueryRow(ctx, `SELECT runtime_id FROM agent WHERE id = $1`, fx.LeaderID).Scan(&runtimeID); err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	})

	for _, tc := range []struct {
		name    string
		purpose *string
		fireAt  bool
	}{
		{name: "queued retry keeps purpose", purpose: ptr("compaction")},
		{name: "deferred retry keeps purpose", purpose: ptr("compaction"), fireAt: true},
		{name: "ordinary parent stays NULL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var parentID string
			if err := testPool.QueryRow(ctx, `
				INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, attempt, max_attempts, purpose)
				VALUES ($1, $2, $3, 'failed', 1, 3, $4)
				RETURNING id
			`, fx.LeaderID, runtimeID, issueID, tc.purpose).Scan(&parentID); err != nil {
				t.Fatalf("seed parent task: %v", err)
			}
			t.Cleanup(func() {
				testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1 OR parent_task_id = $1`, parentID)
			})

			params := db.CreateRetryTaskParams{ID: util.MustParseUUID(parentID)}
			if tc.fireAt {
				params.FireAt = pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
			}
			child, err := testHandler.Queries.CreateRetryTask(ctx, params)
			if err != nil {
				t.Fatalf("CreateRetryTask: %v", err)
			}
			var got *string
			if tc.purpose != nil {
				if !child.Purpose.Valid {
					t.Fatalf("child.Purpose is NULL, want %q", *tc.purpose)
				}
				got = &child.Purpose.String
			} else if child.Purpose.Valid {
				t.Fatalf("child.Purpose = %q, want NULL", child.Purpose.String)
			}
			if tc.purpose != nil && *got != *tc.purpose {
				t.Fatalf("child.Purpose = %q, want %q", *got, *tc.purpose)
			}
			if tc.fireAt && child.Status != "deferred" {
				t.Fatalf("child.Status = %q, want deferred", child.Status)
			}
		})
	}
}

// TestCreateManualQuickCreateRetryTask_KeepsPurpose: the other task-copy query
// (a human retry of an issue-less quick-create) carries purpose too.
func TestCreateManualQuickCreateRetryTask_KeepsPurpose(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	fx := newSquadCommentTriggerFixture(t)

	var runtimeID string
	if err := testPool.QueryRow(ctx, `SELECT runtime_id FROM agent WHERE id = $1`, fx.LeaderID).Scan(&runtimeID); err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	var parentID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, status, context, purpose)
		VALUES ($1, $2, 'failed', '{"type":"quick_create"}'::jsonb, 'compaction')
		RETURNING id
	`, fx.LeaderID, runtimeID).Scan(&parentID); err != nil {
		t.Fatalf("seed parent task: %v", err)
	}
	var actorID string
	if err := testPool.QueryRow(ctx, `SELECT id FROM "user" LIMIT 1`).Scan(&actorID); err != nil {
		t.Fatalf("load user: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1 OR rerun_of_task_id = $1`, parentID)
	})

	child, err := testHandler.Queries.CreateManualQuickCreateRetryTask(ctx, db.CreateManualQuickCreateRetryTaskParams{
		ActorUserID:  util.MustParseUUID(actorID),
		NewTaskID:    util.MustParseUUID(uuid.NewString()),
		SourceTaskID: util.MustParseUUID(parentID),
	})
	if err != nil {
		t.Fatalf("CreateManualQuickCreateRetryTask: %v", err)
	}
	if !child.Purpose.Valid || child.Purpose.String != "compaction" {
		t.Fatalf("child.Purpose = %+v, want compaction", child.Purpose)
	}
}
