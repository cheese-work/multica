package issuecreatedvia

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Querier is the slice of *db.Queries the loader reads.
type Querier interface {
	GetAgentTask(ctx context.Context, id pgtype.UUID) (db.AgentTaskQueue, error)
	GetComment(ctx context.Context, id pgtype.UUID) (db.Comment, error)
	GetIssueInWorkspace(ctx context.Context, arg db.GetIssueInWorkspaceParams) (db.Issue, error)
}

// Request is what the create path knows.
type Request struct {
	WorkspaceID  pgtype.UUID
	OriginType   string
	CreatorAgent bool
	// AgentID and TaskID come from the server-validated X-Agent-ID / X-Task-ID.
	AgentID pgtype.UUID
	TaskID  pgtype.UUID
	// Channel is the request channel of a member creator.
	Channel  string
	ParentID pgtype.UUID
}

// maxRepeatHops bounds the retry/rerun chain walk.
const maxRepeatHops = 3

// ForCreate loads the origin rows and classifies. It never fails: a failed task
// or comment lookup gives Agent for an agent creator and is logged.
func ForCreate(ctx context.Context, q Querier, r Request) string {
	in := Input{OriginType: r.OriginType, CreatorAgent: r.CreatorAgent, Channel: r.Channel}
	if r.CreatorAgent && r.OriginType != OriginQuickCreate && r.OriginType != OriginAutopilot && r.TaskID.Valid {
		in.Task = loadTask(ctx, q, r)
		if in.Task != nil && r.ParentID.Valid {
			in.ParentMadeBySameTask = parentMadeBy(ctx, q, r)
		}
	}
	return Classify(in)
}

func loadTask(ctx context.Context, q Querier, r Request) *TaskFacts {
	task, err := q.GetAgentTask(ctx, r.TaskID)
	if err != nil {
		slog.Warn("created_via: origin task lookup failed", "task_id", r.TaskID.Bytes, "error", err)
		return nil
	}
	if task.AgentID != r.AgentID {
		return nil
	}
	// A retry or rerun with no chat or comment evidence of its own takes the
	// value of the task it repeats.
	for hop := 0; hop < maxRepeatHops && !hasOwnEvidence(task); hop++ {
		next := task.RetryOfTaskID
		if !next.Valid {
			next = task.RerunOfTaskID
		}
		if !next.Valid {
			break
		}
		prev, err := q.GetAgentTask(ctx, next)
		if err != nil {
			slog.Warn("created_via: repeated task lookup failed", "task_id", next.Bytes, "error", err)
			break
		}
		task = prev
	}
	f := &TaskFacts{
		HasChatSession:   task.ChatSessionID.Valid,
		EvidenceKind:     task.TriggerEvidenceKind.String,
		EvidenceKindNull: !task.TriggerEvidenceKind.Valid,
	}
	f.Autopilot = task.AutopilotRunID.Valid || f.EvidenceKind == "autopilot_run" || f.EvidenceKind == "rule_version"
	if task.TriggerCommentID.Valid && !f.HasChatSession && !f.Autopilot &&
		(f.EvidenceKind == "comment" || f.EvidenceKindNull) {
		c, err := q.GetComment(ctx, task.TriggerCommentID)
		if err != nil {
			slog.Warn("created_via: trigger comment lookup failed", "comment_id", task.TriggerCommentID.Bytes, "error", err)
		} else {
			f.TriggerCommentByMember = c.AuthorType == "member"
		}
	}
	return f
}

func hasOwnEvidence(t db.AgentTaskQueue) bool {
	return t.ChatSessionID.Valid || t.AutopilotRunID.Valid || t.TriggerEvidenceKind.String == "comment"
}

func parentMadeBy(ctx context.Context, q Querier, r Request) bool {
	p, err := q.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: r.ParentID, WorkspaceID: r.WorkspaceID})
	if err != nil {
		return false
	}
	return p.OriginType.String == OriginAgentCreate && p.OriginID == r.TaskID
}
