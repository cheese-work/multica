package service

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ActivityWorkflowRole is the insert-only activity row that records which role
// a task worked in (CHE-7658 step 11).
const ActivityWorkflowRole = "workflow_role"

// LabUnknown is the lab of an author whose provider and model are not in the
// lab registry.
const LabUnknown = "unknown"

// IssueAuthor is an agent task on an issue that counts as authorship.
type IssueAuthor struct {
	AgentID  string
	TaskID   string
	Provider string
	Model    string
	Lab      string
}

// IssueAuthors is not implemented yet.
func IssueAuthors(ctx context.Context, q *db.Queries, issueID pgtype.UUID, labOf func(provider, model string) string) ([]IssueAuthor, error) {
	return nil, nil
}
