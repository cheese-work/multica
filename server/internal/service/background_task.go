package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
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
	// ErrBackgroundChainChanged is returned by EnqueueBackgroundTask when the
	// member's or workspace's chain no longer names the agent in the slot the
	// caller resolved. The caller resolves again.
	ErrBackgroundChainChanged = errors.New("background task: the agent chain changed")
)

// Setting keys under which the chains are stored (member_setting key;
// workspace.settings key).
const (
	MemberBackgroundAgentsKey    = "background_agents"
	WorkspaceBackgroundAgentsKey = "background_agents"
)

// LockMemberBackgroundAgents takes the transaction-scoped lock that serializes
// a member's personal-chain write with every enqueue for that member (CHE-4114).
func LockMemberBackgroundAgents(ctx context.Context, tx db.DBTX, ws, user pgtype.UUID) error {
	key := "member_setting:" + util.UUIDToString(ws) + ":" + util.UUIDToString(user) + ":" + MemberBackgroundAgentsKey
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key)
	return err
}

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

type backgroundTaskContext struct {
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
	payload := backgroundTaskContext{
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
	task, err := s.insertUnderCurrentChain(ctx, in, db.CreateBackgroundTaskParams{
		ID: dbid.NewV7(), AgentID: in.AgentID, RuntimeID: agent.RuntimeID, Priority: priorityToInt("low"),
		Context: contextJSON, Purpose: pgtype.Text{String: bgagents.Purpose(in.Job), Valid: true},
		OriginatorUserID: in.MemberID, AccountableUserID: attr.AccountableUserID,
		OriginatorSource: attrSource, TriggerEvidenceKind: attrEvidenceKind, TriggerEvidenceRefID: attrEvidenceRef,
	})
	if err != nil {
		return db.AgentTaskQueue{}, err
	}
	s.RecordHumanDelegation(ctx, attr)
	s.NotifyTaskEnqueued(ctx, task)
	return task, nil
}

// insertUnderCurrentChain inserts the task under the member's lock, after
// re-reading the chains: a personal write that committed since the caller
// resolved the chain is seen here, and one that has not yet committed waits for
// this insert, then cancels the row (CHE-4114). Without a member there is no
// personal chain to serialize with; only the workspace chain is checked.
func (s *TaskService) insertUnderCurrentChain(ctx context.Context, in BackgroundTaskInput, p db.CreateBackgroundTaskParams) (db.AgentTaskQueue, error) {
	if s.TxStarter == nil {
		return db.AgentTaskQueue{}, errors.New("background task enqueue requires transaction support")
	}
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return db.AgentTaskQueue{}, fmt.Errorf("begin background enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if in.MemberID.Valid {
		if err := LockMemberBackgroundAgents(ctx, tx, in.WorkspaceID, in.MemberID); err != nil {
			return db.AgentTaskQueue{}, fmt.Errorf("lock member background agents: %w", err)
		}
	}
	q := s.Queries.WithTx(tx)
	if err := s.checkChainStillNames(ctx, q, in); err != nil {
		return db.AgentTaskQueue{}, err
	}
	task, err := q.CreateBackgroundTask(ctx, p)
	if err != nil {
		return db.AgentTaskQueue{}, fmt.Errorf("create background task: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return db.AgentTaskQueue{}, fmt.Errorf("commit background enqueue: %w", err)
	}
	return task, nil
}

// checkChainStillNames mirrors bgagents.Resolve on the stored values: the
// source's chain must still hold the agent in the slot, and a workspace-sourced
// task needs the member to have no personal chain for the job.
func (s *TaskService) checkChainStillNames(ctx context.Context, q *db.Queries, in BackgroundTaskInput) error {
	chain := func(raw []byte, scope bgagents.Scope) bgagents.Chain {
		cfg, err := bgagents.Parse(raw, scope)
		if err != nil {
			return bgagents.Chain{} // an invalid stored value counts as empty, as in Resolve
		}
		return cfg[in.Job]
	}
	var personal bgagents.Chain
	if in.MemberID.Valid {
		row, err := q.GetMemberSetting(ctx, db.GetMemberSettingParams{WorkspaceID: in.WorkspaceID, UserID: in.MemberID, Key: MemberBackgroundAgentsKey})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read member background agents: %w", err)
		}
		personal = chain(row.Value, bgagents.UserScope)
	}
	current := personal
	if in.Source == bgagents.WorkspaceSource {
		ws, err := q.GetWorkspace(ctx, in.WorkspaceID)
		if err != nil {
			return fmt.Errorf("read workspace background agents: %w", err)
		}
		var settings map[string]json.RawMessage
		_ = json.Unmarshal(ws.Settings, &settings)
		current = chain(settings[WorkspaceBackgroundAgentsKey], bgagents.WorkspaceScope)
		if personal.AgentID != "" {
			return ErrBackgroundChainChanged
		}
	}
	slotAgent := current.AgentID
	if in.Slot == bgagents.Fallback {
		slotAgent = current.FallbackAgentID
	}
	if slotAgent == "" || slotAgent != util.UUIDToString(in.AgentID) {
		return ErrBackgroundChainChanged
	}
	return nil
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

// CancelBackgroundTasksForMember cancels, in the caller's transaction, the
// member's non-terminal tasks of the job from either chain source. Callers
// broadcast the returned rows after commit (BroadcastCancelledTasks).
func CancelBackgroundTasksForMember(ctx context.Context, q *db.Queries, workspaceID, memberID pgtype.UUID, job string) ([]db.AgentTaskQueue, error) {
	cancelled, err := q.CancelBackgroundTasksByPurposeAndMember(ctx, db.CancelBackgroundTasksByPurposeAndMemberParams{
		WorkspaceID: workspaceID, Purpose: pgtype.Text{String: bgagents.Purpose(bgagents.Job(job)), Valid: true},
		MemberID: util.UUIDToString(memberID),
	})
	if err != nil {
		return nil, err
	}
	return cancelled, SettleTerminalTaskState(ctx, q, cancelled...)
}
