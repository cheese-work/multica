package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-9045: run telemetry reads an empty failure_reason as "unknown". An
// automatic cancel with no caller reason must carry a named class; an explicit
// reason and a user cancel (empty by design) must not change.
func TestCancelTaskFailureReasonClass(t *testing.T) {
	ctx := context.Background()
	pool := sharedTestPool(t)
	fx := testutil.New(pool, "", "")
	suffix := time.Now().UnixNano()
	fx.UserID = fx.User(t, "cancel reason", fmt.Sprintf("cancel-reason-%d@example.test", suffix))
	fx.WorkspaceID = fx.Workspace(t, "cancel reason", fmt.Sprintf("cancel-reason-%d", suffix))
	fx.Member(t, fx.WorkspaceID, fx.UserID, "owner")
	runtimeID := fx.Runtime(t, "cancel reason")
	agentID := fx.Agent(t, "cancel reason", runtimeID)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())

	cases := []struct {
		name   string
		cancel func(id pgtype.UUID) error
		want   any // nil = NULL
	}{
		{"automatic without reason is named", func(id pgtype.UUID) error {
			_, err := svc.CancelTask(ctx, id)
			return err
		}, FailureReasonCancelled},
		{"explicit reason is kept", func(id pgtype.UUID) error {
			_, err := svc.CancelTaskWithReason(ctx, id, "why", FailureReasonSupersededHead)
			return err
		}, FailureReasonSupersededHead},
		{"user cancel stays unlabeled", func(id pgtype.UUID) error {
			_, err := svc.CancelTaskByUser(ctx, id, TaskCancellationActor{Type: "member", ID: util.MustParseUUID(fx.UserID)})
			return err
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			taskID := fx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "status": "queued"})
			if err := tc.cancel(util.MustParseUUID(taskID)); err != nil {
				t.Fatal(err)
			}
			var status string
			var reason *string
			if err := pool.QueryRow(ctx, `SELECT status, failure_reason FROM agent_task_queue WHERE id=$1`, taskID).Scan(&status, &reason); err != nil {
				t.Fatal(err)
			}
			if status != "cancelled" {
				t.Fatalf("status=%q want cancelled", status)
			}
			switch want := tc.want.(type) {
			case nil:
				if reason != nil {
					t.Fatalf("failure_reason=%q want NULL", *reason)
				}
			case string:
				if reason == nil || *reason != want {
					t.Fatalf("failure_reason=%v want %q", reason, want)
				}
			}
		})
	}
}
