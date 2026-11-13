package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/attribution"
	"github.com/multica-ai/multica/server/internal/bgagents"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// BackgroundTaskContextType marks an output-only background task in its context.
const BackgroundTaskContextType = "background"

// BackgroundInputMaxRunes is the server ceiling on a task's input. A job's
// caller applies its own tighter budget (dm_title: 2,000 characters).
const BackgroundInputMaxRunes = 16_000

var (
	ErrBackgroundJobNotOutputOnly = errors.New("background task: job is not an output_only job")
	// ErrInvalidBackgroundOutput is returned by a BackgroundWriter that rejected
	// the output before writing anything.
	ErrInvalidBackgroundOutput = errors.New("invalid background task output")
)

// BackgroundWriter validates a finished output-only task's result and writes
// it, inside the completion transaction.
type BackgroundWriter func(ctx context.Context, q *db.Queries, task db.AgentTaskQueue, result []byte) error

// BackgroundTaskInput is one output-only job run (CHE-4099).
type BackgroundTaskInput struct {
	Job         bgagents.Job
	WorkspaceID pgtype.UUID
	AgentID     pgtype.UUID
	MemberID    pgtype.UUID // the DM creator; zero when the task is not member-attributable
	Source      bgagents.Source
	Slot        bgagents.SlotName
	Input       string
}

// BackgroundTaskContext is the JSON stored in the task context; the claim
// handler reads it back.
type BackgroundTaskContext struct {
	Type         string `json:"type"`
	Job          string `json:"job"`
	WorkspaceID  string `json:"workspace_id"`
	ChainSource  string `json:"chain_source"`
	MemberID     string `json:"member_id,omitempty"`
	Slot         string `json:"slot"`
	ClaimSeconds int    `json:"claim_deadline_seconds"`
	RunSeconds   int    `json:"run_deadline_seconds"`
	Input        string `json:"input"`
}

// EnqueueBackgroundTask queues an output-only job on the chain agent's runtime:
// no issue, no chat. Only the catalog's output_only jobs qualify. The claim is
// restricted to that runtime by ClaimAgentTask.
func (s *TaskService) EnqueueBackgroundTask(ctx context.Context, in BackgroundTaskInput) (db.AgentTaskQueue, error) {
	spec, ok := bgagents.SpecFor(in.Job)
	if !ok || spec.Runner != bgagents.OutputOnly {
		return db.AgentTaskQueue{}, fmt.Errorf("%w: %q", ErrBackgroundJobNotOutputOnly, in.Job)
	}
	if in.Source != bgagents.UserSource && in.Source != bgagents.WorkspaceSource {
		return db.AgentTaskQueue{}, fmt.Errorf("background task: chain source must be user or workspace, got %q", in.Source)
	}
	agent, err := s.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: in.AgentID, WorkspaceID: in.WorkspaceID})
	if err != nil {
		return db.AgentTaskQueue{}, fmt.Errorf("load agent: %w", err)
	}
	if agent.ArchivedAt.Valid {
		return db.AgentTaskQueue{}, errors.New("agent is archived")
	}
	if !agent.RuntimeID.Valid {
		return db.AgentTaskQueue{}, errors.New("agent has no runtime")
	}
	input := in.Input
	if r := []rune(input); len(r) > BackgroundInputMaxRunes {
		input = string(r[:BackgroundInputMaxRunes])
	}
	payload := BackgroundTaskContext{
		Type: BackgroundTaskContextType, Job: string(in.Job), WorkspaceID: util.UUIDToString(in.WorkspaceID),
		ChainSource: string(in.Source), Slot: string(in.Slot),
		ClaimSeconds: int(spec.ClaimDeadline.Seconds()), RunSeconds: int(spec.RunDeadline.Seconds()),
		Input: input,
	}
	if in.MemberID.Valid {
		payload.MemberID = util.UUIDToString(in.MemberID)
	}
	contextJSON, err := json.Marshal(payload)
	if err != nil {
		return db.AgentTaskQueue{}, fmt.Errorf("marshal background context: %w", err)
	}
	// The member who owns the DM is the direct_human originator; a task with no
	// member degrades to owner_fallback like quick-create (MUL-4302 §3.5).
	attr, err := s.applyAttributionFallback(ctx, attribution.DirectHumanRun(in.MemberID, "", pgtype.UUID{}), agent)
	if err != nil {
		return db.AgentTaskQueue{}, err
	}
	attrSource, _, attrEvidenceKind, attrEvidenceRef := attributionCreateParams(attr)
	task, err := s.Queries.CreateBackgroundTask(ctx, db.CreateBackgroundTaskParams{
		ID: dbid.NewV7(), AgentID: in.AgentID, RuntimeID: agent.RuntimeID, Priority: priorityToInt("low"),
		Context: contextJSON, Purpose: pgtype.Text{String: bgagents.Purpose(in.Job), Valid: true},
		OriginatorUserID: in.MemberID, AccountableUserID: attr.AccountableUserID,
		OriginatorSource: attrSource, TriggerEvidenceKind: attrEvidenceKind, TriggerEvidenceRefID: attrEvidenceRef,
	})
	if err != nil {
		return db.AgentTaskQueue{}, fmt.Errorf("create background task: %w", err)
	}
	s.RecordHumanDelegation(ctx, attr)
	s.NotifyTaskEnqueued(ctx, task)
	return task, nil
}

// writeBackgroundOutput runs inside the completion transaction. A purpose with
// no registered writer is a no-op. A writer that returns ErrInvalidBackgroundOutput
// (it must do so before writing) leaves the task completed with nothing written.
func (s *TaskService) writeBackgroundOutput(ctx context.Context, q *db.Queries, task db.AgentTaskQueue, result []byte) error {
	write := s.BackgroundWriters[task.Purpose.String]
	if !task.Purpose.Valid || write == nil {
		return nil
	}
	err := write(ctx, q, task, result)
	if errors.Is(err, ErrInvalidBackgroundOutput) {
		slog.Warn("background task output rejected", "task_id", util.UUIDToString(task.ID), "purpose", task.Purpose.String, "error", err)
		return nil
	}
	return err
}

// CancelBackgroundTasksForJob cancels, in the caller's transaction, the job's
// non-terminal tasks whose chain came from the workspace setting. Callers
// broadcast the returned rows after commit (BroadcastCancelledTasks).
func CancelBackgroundTasksForJob(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, job string) ([]db.AgentTaskQueue, error) {
	cancelled, err := q.CancelBackgroundTasksByPurposeAndSource(ctx, db.CancelBackgroundTasksByPurposeAndSourceParams{
		WorkspaceID: workspaceID, Purpose: pgtype.Text{String: bgagents.Purpose(bgagents.Job(job)), Valid: true},
	})
	if err != nil {
		return nil, err
	}
	return cancelled, SettleTerminalTaskState(ctx, q, cancelled...)
}
