package service

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/dispatch"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-5757 (W02 of CHE-5751): two autopilots rendered one title on 9 Nov 2026.
// dispatchCreateIssue must refuse a title that already exists, open or closed,
// when the template carries {{date}} — and must not touch dateless templates.
func seedCreateIssueAutopilot(t *testing.T, pool *pgxpool.Pool, workspaceID, agentID, creatorID, template string) (ap db.Autopilot, runID string) {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := pool.QueryRow(ctx, `
		INSERT INTO autopilot (workspace_id, title, issue_title_template, assignee_type, assignee_id, status, execution_mode, created_by_type, created_by_id)
		VALUES ($1, 'brief', $4, 'agent', $2, 'active', 'create_issue', 'member', $3) RETURNING id`,
		workspaceID, agentID, creatorID, template).Scan(&id); err != nil {
		t.Fatalf("seed autopilot: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM issue WHERE origin_id = $1`, id)
		pool.Exec(context.Background(), `DELETE FROM autopilot WHERE id = $1`, id)
	})
	if err := pool.QueryRow(ctx, `INSERT INTO autopilot_run (autopilot_id, source, status) VALUES ($1, 'manual', 'running') RETURNING id`, id).Scan(&runID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	ap, err := db.New(pool).GetAutopilot(ctx, util.MustParseUUID(id))
	if err != nil {
		t.Fatal(err)
	}
	return ap, runID
}

func TestDispatchCreateIssueRefusesExistingDatedTitle(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, publisherID, agentID, _ := seedAttributionFixture(t, pool)
	svc := &AutopilotService{Queries: q, TxStarter: pool, Bus: events.New(), TaskSvc: &TaskService{Queries: q, TxStarter: pool, Bus: events.New()}}

	dispatchOne := func(template string) error {
		ap, runID := seedCreateIssueAutopilot(t, pool, workspaceID, agentID, publisherID, template)
		run, err := q.GetAutopilotRun(ctx, util.MustParseUUID(runID))
		if err != nil {
			t.Fatal(err)
		}
		return svc.dispatchCreateIssue(ctx, ap, &run, "UTC", util.MustParseUUID(publisherID))
	}

	if err := dispatchOne("Evening brief {{date}}"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// A second, different autopilot with the same rendered title is refused.
	var skip *errDispatchSkipped
	if err := dispatchOne("Evening brief {{date}}"); !errors.As(err, &skip) || skip.code != dispatch.ReasonAlreadyActive {
		t.Fatalf("open collision = %v, want skipped already_active", err)
	}
	// A closed issue still blocks it.
	if _, err := pool.Exec(ctx, `UPDATE issue SET status = 'done' WHERE workspace_id = $1 AND title LIKE 'Evening brief %'`, workspaceID); err != nil {
		t.Fatal(err)
	}
	if err := dispatchOne("Evening brief {{date}}"); !errors.As(err, &skip) {
		t.Fatalf("closed collision = %v, want skipped", err)
	}
	// A template without {{date}} is not date-scoped: two autopilots may share it.
	for i := 0; i < 2; i++ {
		if err := dispatchOne("Weekly sync"); err != nil {
			t.Fatalf("dateless create %d: %v", i, err)
		}
	}
}
