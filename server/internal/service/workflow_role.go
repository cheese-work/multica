package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ActivityWorkflowRole is the insert-only activity row that records the role a
// task worked in (CHE-7658 step 11): task, agent, the handler type of the
// step it was queued for, the workflow and its version, and the status. The
// activity log is never rewritten, so a later workflow edit or rebind cannot
// change who counted as an author.
const ActivityWorkflowRole = "workflow_role"

// LabUnknown is the lab of an author whose provider and model are not in the
// lab registry. It never matches a reviewer's lab.
const LabUnknown = "unknown"

// RecordWorkflowRole writes one workflow_role row for a task of the issue at
// the given step. The role is the handler type the issue's workflow gives that
// step; "none" when the issue follows no workflow or the step is not in it.
// A write failure is returned: callers run it in the transaction of the write
// it records, so the task or the advance is not made without its evidence. A
// swallowed error would also leave that transaction aborted, and a run whose
// latest row still says reviewer would pass the author query while it works as
// an author (RGS-1, fail closed).
func RecordWorkflowRole(ctx context.Context, q *db.Queries, issue db.Issue, task db.AgentTaskQueue, step string) error {
	if !issue.ID.Valid || !task.ID.Valid || !task.AgentID.Valid {
		return nil
	}
	role, workflowID, version := "none", "", ""
	def, _, err := issueworkflow.ForIssue(ctx, q, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
	if err != nil {
		return fmt.Errorf("workflow role: load workflow: %w", err)
	}
	if def != nil {
		if def.ID.Valid {
			workflowID = util.UUIDToString(def.ID)
		}
		if !def.Version.IsZero() {
			version = def.Version.UTC().Format(time.RFC3339Nano)
		}
		if s, ok := def.Step(step); ok && s.Handler.Type != "" {
			role = s.Handler.Type
		}
	}
	details, _ := json.Marshal(map[string]string{
		"task_id": util.UUIDToString(task.ID), "agent_id": util.UUIDToString(task.AgentID),
		"role": role, "workflow_id": workflowID, "workflow_version": version, "status": step,
	})
	if _, err := q.CreateActivity(ctx, db.CreateActivityParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID,
		ActorType: pgtype.Text{String: "agent", Valid: true}, ActorID: task.AgentID,
		Action: ActivityWorkflowRole, Details: details,
	}); err != nil {
		return fmt.Errorf("workflow role: write: %w", err)
	}
	return nil
}

// recordEnqueuedRole writes the role row of a task just queued for an issue,
// at the step the task row says it was queued for.
func recordEnqueuedRole(ctx context.Context, q *db.Queries, task db.AgentTaskQueue) error {
	if !task.IssueID.Valid {
		return nil
	}
	issue, err := q.GetIssue(ctx, task.IssueID)
	if err != nil {
		return fmt.Errorf("workflow role: load issue: %w", err)
	}
	return RecordWorkflowRole(ctx, q, issue, task, task.WorkflowStep.String)
}

// CopyWorkflowRoleToRetry gives a retry the role its parent worked in.
func CopyWorkflowRoleToRetry(ctx context.Context, q *db.Queries, parent, child db.AgentTaskQueue) error {
	if !parent.IssueID.Valid {
		return nil
	}
	if err := q.CopyWorkflowRoleToRetry(ctx, db.CopyWorkflowRoleToRetryParams{
		IssueID: parent.IssueID, ParentTaskID: util.UUIDToString(parent.ID), ChildTaskID: util.UUIDToString(child.ID),
	}); err != nil {
		return fmt.Errorf("workflow role: copy for retry: %w", err)
	}
	return nil
}

// IssueAuthor is an agent task on an issue that counts as authorship.
type IssueAuthor struct {
	AgentID  string
	TaskID   string
	Provider string
	Model    string
	// Lab is the caller's labOf(Provider, Model), or LabUnknown.
	Lab string
}

// IssueAuthors lists the agent tasks of an issue that count as authorship:
// every agent task except those whose workflow_role rows say reviewer. A task
// with no row counts (fail closed), so an issue from before this evidence
// existed has only authors. labOf maps an exact provider and model to a lab, or
// "" when the registry does not list it; that becomes LabUnknown.
func IssueAuthors(ctx context.Context, q *db.Queries, issueID pgtype.UUID, labOf func(provider, model string) string) ([]IssueAuthor, error) {
	rows, err := q.ListIssueAuthorTasks(ctx, issueID)
	if err != nil {
		return nil, fmt.Errorf("list issue author tasks: %w", err)
	}
	out := make([]IssueAuthor, 0, len(rows))
	for _, r := range rows {
		lab := ""
		if labOf != nil {
			lab = labOf(r.Provider, r.Model)
		}
		if lab == "" {
			lab = LabUnknown
		}
		out = append(out, IssueAuthor{
			AgentID: util.UUIDToString(r.AgentID), TaskID: util.UUIDToString(r.TaskID),
			Provider: r.Provider, Model: r.Model, Lab: lab,
		})
	}
	return out, nil
}
