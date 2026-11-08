package handler

import (
	"context"
	"fmt"
	"strings"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// stageQueueBriefing is the leader's "## Stage Queue" section (CHE-3613). When a
// stage is full it names the queued PRs and the idle squad members, and tells
// the leader to route an idle member to a queued PR before starting new PR
// work. Empty when no stage has a PR waiting.
func stageQueueBriefing(ctx context.Context, q *db.Queries, squad db.Squad) string {
	stages, err := service.LoadFullStages(ctx, q, squad.WorkspaceID)
	if err != nil || len(stages) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## Stage Queue\n\n")
	sb.WriteString("A stage below is full: its PRs wait for a slot. Route an idle member to a queued PR before you start any new PR work.\n")
	for _, s := range stages {
		fmt.Fprintf(&sb, "\n%s stage — limit %d, slot owner `%s`. Queued PRs, dispatch order:\n", s.Stage, s.Limit, s.AgentID)
		for _, p := range s.Queued {
			note := "No push task is open — dispatch one."
			if p.PushOpen {
				note = "push task open."
			}
			fmt.Fprintf(&sb, "- `%s` — %s\n", p.IssueID, note)
		}
	}
	idle := idleSquadMembers(ctx, q, squad)
	if len(idle) == 0 {
		sb.WriteString("\nIdle members: (none — leave the queue, start no new PR work.)\n")
	} else {
		sb.WriteString("\nIdle members:\n")
		for _, m := range idle {
			fmt.Fprintf(&sb, "- `%s`\n", formatMention(m.Name, "agent", util.UUIDToString(m.ID)))
		}
	}
	sb.WriteString("\nTo dispatch: give an idle member a task on the first queued PR with no push task open. The task: update the branch from `main`, fix the review findings, prepare the evidence. Do not run the stage's gate yourself.\n")
	sb.WriteString("New PR work waits while a queued PR has a push task open.\n")
	return sb.String()
}

// idleSquadMembers are the roster's live agents with no running task.
func idleSquadMembers(ctx context.Context, q *db.Queries, squad db.Squad) []db.Agent {
	members, err := q.ListSquadMembers(ctx, squad.ID)
	if err != nil {
		return nil
	}
	var idle []db.Agent
	for _, m := range members {
		if m.MemberType != "agent" || m.MemberID == squad.LeaderID {
			continue
		}
		a, err := q.GetAgent(ctx, m.MemberID)
		if err != nil || a.ArchivedAt.Valid {
			continue
		}
		if n, err := q.CountRunningTasks(ctx, a.ID); err == nil && n == 0 {
			idle = append(idle, a)
		}
	}
	return idle
}
