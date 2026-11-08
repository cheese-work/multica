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
// work. Only PRs this squad owns are listed (squadOwnsPR, by assignment); empty when it owns
// none, so one squad's queue never redirects another squad's leader.
func stageQueueBriefing(ctx context.Context, q *db.Queries, squad db.Squad) string {
	loaded, err := service.LoadFullStages(ctx, q, squad.WorkspaceID)
	if err != nil {
		return ""
	}
	var stages []service.FullStage
	for _, s := range loaded {
		var mine []service.QueuedPR
		for _, p := range s.Queued {
			if squadOwnsPR(ctx, q, squad, p.IssueID) {
				mine = append(mine, p)
			}
		}
		if len(mine) > 0 {
			s.Queued = mine
			stages = append(stages, s)
		}
	}
	if len(stages) == 0 {
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

// squadOwnsPR is true when the PR's work item, or an ancestor of it, is
// assigned to the squad. Past tasks by squad members do not count: a member
// may have helped another team's PR as a guest.
func squadOwnsPR(ctx context.Context, q *db.Queries, squad db.Squad, prIssue string) bool {
	id := util.MustParseUUID(prIssue)
	for depth := 0; id.Valid && depth < 8; depth++ { // 8: epic -> PR nesting is shallow
		issue, err := q.GetIssue(ctx, id)
		if err != nil {
			return false
		}
		if issue.AssigneeType.String == "squad" && issue.AssigneeID == squad.ID {
			return true
		}
		id = issue.ParentIssueID
	}
	return false
}
