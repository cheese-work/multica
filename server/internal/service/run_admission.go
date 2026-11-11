package service

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// RunInput is one trigger of an (issue, agent) run, with a stable event id.
type RunInput struct {
	WorkspaceID, IssueID, AgentID pgtype.UUID
	EventID, Kind                 string
	Payload                       []byte
}

func (s *TaskService) RecordRunInput(ctx context.Context, in RunInput) (bool, error) { return false, nil }

func (s *TaskService) ReleaseProjectAdmission(ctx context.Context, projectID pgtype.UUID) error {
	return nil
}

func (s *TaskService) ExpireDueReservations(ctx context.Context) (int, error) { return 0, nil }

func (s *TaskService) ReconcileAdmission(ctx context.Context) error { return nil }

func (s *TaskService) ResumeParkedInputs(ctx context.Context, issueID, agentID pgtype.UUID) (int, error) {
	return 0, nil
}

func (s *TaskService) DiscardParkedInputs(ctx context.Context, issueID, agentID pgtype.UUID) (int, error) {
	return 0, nil
}

func (s *TaskService) RunInputsForTask(ctx context.Context, taskID pgtype.UUID) ([]db.RunInput, error) {
	return nil, nil
}
