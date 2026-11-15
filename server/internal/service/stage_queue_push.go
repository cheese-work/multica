package service

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/stagequeue"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// QueuedPR is a PR that waits for a slot (CHE-3613). IssueID is the PR's work
// item. PushOpen is true while an agent other than the stage owner has an
// active task on it: the "update from main, fix findings, prepare evidence" task.
type QueuedPR struct {
	IssueID  string
	PushOpen bool
}

// FullStage is one slot owner's stage with at least one waiting PR.
type FullStage struct {
	Stage   string
	AgentID string // slot owner
	Limit   int
	Queued  []QueuedPR // dispatch order: fewest open gates, then oldest
}

// LoadFullStages lists the workspace's stages that refused a run and still
// have a PR waiting, so the squad leader can route an idle agent to a queued
// PR instead of starting new PR work.
func LoadFullStages(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID) ([]FullStage, error) {
	owners, err := q.ListWaitingStageOwners(ctx)
	if err != nil {
		return nil, fmt.Errorf("full stages: list owners: %w", err)
	}
	var out []FullStage
	for _, o := range owners {
		owner, err := q.GetAgent(ctx, o.AgentID)
		if err != nil {
			return nil, fmt.Errorf("full stages: load owner: %w", err)
		}
		if owner.WorkspaceID != workspaceID {
			continue
		}
		stage := stagequeue.Stage(o.Stage)
		entries, err := stageEntries(ctx, q, stage, o.AgentID, pgtype.UUID{})
		if err != nil {
			return nil, fmt.Errorf("full stages: list entries: %w", err)
		}
		limit := stageLimits.For(stage)
		fs := FullStage{Stage: o.Stage, AgentID: util.UUIDToString(o.AgentID), Limit: limit}
		for _, e := range stagequeue.Plan(entries, limit).Queued {
			open, err := pushTaskOpen(ctx, q, e.PR, o.AgentID)
			if err != nil {
				return nil, err
			}
			fs.Queued = append(fs.Queued, QueuedPR{IssueID: e.PR, PushOpen: open})
		}
		if len(fs.Queued) > 0 {
			out = append(out, fs)
		}
	}
	return out, nil
}

func pushTaskOpen(ctx context.Context, q *db.Queries, prIssue string, owner pgtype.UUID) (bool, error) {
	tasks, err := q.ListActiveTasksByIssue(ctx, util.MustParseUUID(prIssue))
	if err != nil {
		return false, fmt.Errorf("full stages: active tasks on %s: %w", prIssue, err)
	}
	for _, t := range tasks {
		if t.AgentID != owner {
			return true, nil
		}
	}
	return false, nil
}
