package service

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A channel create defers the assigned run inside the create transaction. In a
// paused project the DB fence refuses that insert, which would abort the whole
// create; the create must instead commit with no run. (CHE-10021)
func TestCreateChannelIssueInPausedProjectCommitsWithoutDeferredRun(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, userID, agentID, _ := seedAttributionFixture(t, pool)
	workspaceUUID := util.MustParseUUID(workspaceID)
	bus := events.New()
	taskService := &TaskService{Queries: q, TxStarter: pool, Bus: bus}
	issueService := NewIssueService(q, pool, bus, nil, taskService)

	for _, creatorType := range []string{"member", "agent"} {
		t.Run(creatorType, func(t *testing.T) {
			var projectID pgtype.UUID
			if err := pool.QueryRow(ctx, `INSERT INTO project (workspace_id, title, status) VALUES ($1, 'paused intake', 'paused') RETURNING id`, workspaceUUID).Scan(&projectID); err != nil {
				t.Fatalf("seed project: %v", err)
			}
			creatorID := util.MustParseUUID(userID)
			if creatorType == "agent" {
				creatorID = util.MustParseUUID(agentID)
			}
			result, err := issueService.Create(ctx, IssueCreateParams{
				WorkspaceID:  workspaceUUID,
				ProjectID:    projectID,
				Title:        "Channel issue in paused project " + creatorType,
				Status:       "todo",
				Priority:     "medium",
				AssigneeType: pgtype.Text{String: "agent", Valid: true},
				AssigneeID:   util.MustParseUUID(agentID),
				CreatorType:  creatorType,
				CreatorID:    creatorID,
			}, IssueCreateOpts{AssignedAgentRunFireAt: time.Now().Add(time.Minute)})
			if err != nil {
				t.Fatalf("Create in a paused project: %v", err)
			}
			if result.AssignedTaskID.Valid {
				t.Fatal("create returned a deferred task in a paused project")
			}
			var tasks int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, result.Issue.ID).Scan(&tasks); err != nil || tasks != 0 {
				t.Fatalf("tasks on the new issue = %d (err %v), want 0", tasks, err)
			}
		})
	}
}

func TestRequireProjectIntakeAllowsOnlyPausedAmongInactive(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, _, _, _ := seedAttributionFixture(t, pool)
	workspaceUUID := util.MustParseUUID(workspaceID)
	for status, wantErr := range map[string]bool{"planned": false, "in_progress": false, "paused": false, "cancelled": true, "completed": true} {
		var projectID pgtype.UUID
		if err := pool.QueryRow(ctx, `INSERT INTO project (workspace_id, title, status) VALUES ($1, 'intake', $2) RETURNING id`, workspaceUUID, status).Scan(&projectID); err != nil {
			t.Fatalf("seed project: %v", err)
		}
		err := RequireProjectIntake(ctx, q, workspaceUUID, projectID)
		if (err != nil) != wantErr || wantErr && !IsProjectInactive(err) {
			t.Errorf("%s: RequireProjectIntake = %v, want refusal %v", status, err, wantErr)
		}
		if got := RequireActiveProject(ctx, q, workspaceUUID, projectID); (got != nil) != ProjectInactive(status) {
			t.Errorf("%s: RequireActiveProject = %v changed", status, got)
		}
	}
	if err := RequireProjectIntake(ctx, q, workspaceUUID, pgtype.UUID{}); err != nil {
		t.Errorf("no project: %v", err)
	}
}
