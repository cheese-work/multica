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
	// workspace's chain changed between the resolver's read and the insert. The
	// caller enqueues again.
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
	AgentID     pgtype.UUID // ignored when Resolve is set
	MemberID    pgtype.UUID // the DM creator; zero when the task is not member-attributable
	Source      bgagents.Source
	Slot        bgagents.SlotName
	// FallbackAgentID is the agent a provider failure of this run moves to; zero
	// when this run holds the last slot.
	FallbackAgentID pgtype.UUID
	Input           string
	// Resolve, when set, picks the chain slot instead of AgentID/Source/Slot/
	// FallbackAgentID. EnqueueBackgroundTask calls it after taking the member's
	// lock, so a personal write is either fully visible to it or waits for the
	// insert and then cancels the row (CHE-4114).
	Resolve BackgroundResolver
}

// BackgroundTarget is the chain slot a job run goes to.
type BackgroundTarget struct {
	AgentID         pgtype.UUID
	Source          bgagents.Source
	Slot            bgagents.SlotName
	FallbackAgentID pgtype.UUID
}

// BackgroundResolver resolves the chain for one enqueue, under the member lock.
type BackgroundResolver func(ctx context.Context) (BackgroundTarget, error)

// BackgroundTaskContext is the JSON stored in the task context; the claim
// handler reads it back.
type BackgroundTaskContext struct {
	Type            string `json:"type"`
	Job             string `json:"job"`
	WorkspaceID     string `json:"workspace_id"`
	ChainSource     string `json:"chain_source"`
	MemberID        string `json:"member_id,omitempty"`
	Slot            string `json:"slot"`
	FallbackAgentID string `json:"fallback_agent_id,omitempty"`
	ClaimSeconds    int    `json:"claim_deadline_seconds"`
	RunSeconds      int    `json:"run_deadline_seconds"`
	Input           string `json:"input"`
}

// EnqueueBackgroundTask queues an output-only job on the chain agent's runtime:
// no issue, no chat. Only the catalog's output_only jobs qualify. The claim is
// restricted to that runtime by ClaimAgentTask. The chain is resolved and the
// row inserted under the member's lock (CHE-4114): a personal write that
// committed before the lock is seen by the resolver, and one that has not yet
// committed waits for this insert, then cancels the row. Without a member there
// is no personal chain to serialize with.
func (s *TaskService) EnqueueBackgroundTask(ctx context.Context, in BackgroundTaskInput) (db.AgentTaskQueue, error) {
	spec, ok := bgagents.SpecFor(in.Job)
	if !ok || spec.Runner != bgagents.OutputOnly {
		return db.AgentTaskQueue{}, fmt.Errorf("%w: %q", ErrBackgroundJobNotOutputOnly, in.Job)
	}
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
	// Every workspace settings write is an UPDATE of this row, so holding it to
	// commit serializes the workspace chain read and the insert with them. It is
	// the lock that does not block the FOR KEY SHARE of rows referencing it.
	if err := s.Queries.WithTx(tx).LockWorkspaceSettingsForWakeupDefinition(ctx, in.WorkspaceID); err != nil {
		return db.AgentTaskQueue{}, fmt.Errorf("lock workspace settings: %w", err)
	}
	target := BackgroundTarget{AgentID: in.AgentID, Source: in.Source, Slot: in.Slot, FallbackAgentID: in.FallbackAgentID}
	if in.Resolve != nil {
		var err error
		if target, err = in.Resolve(ctx); err != nil {
			return db.AgentTaskQueue{}, err
		}
	}
	if target.Source != bgagents.UserSource && target.Source != bgagents.WorkspaceSource {
		return db.AgentTaskQueue{}, fmt.Errorf("background task: chain source must be user or workspace, got %q", target.Source)
	}
	agent, err := s.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: target.AgentID, WorkspaceID: in.WorkspaceID})
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
		ChainSource: string(target.Source), Slot: string(target.Slot),
		ClaimSeconds: int(spec.ClaimDeadline.Seconds()), RunSeconds: int(spec.RunDeadline.Seconds()),
		Input: input,
	}
	if target.FallbackAgentID.Valid {
		payload.FallbackAgentID = util.UUIDToString(target.FallbackAgentID)
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
	q := s.Queries.WithTx(tx)
	if err := s.checkChainStillNames(ctx, q, in, target); err != nil {
		return db.AgentTaskQueue{}, err
	}
	task, err := q.CreateBackgroundTask(ctx, db.CreateBackgroundTaskParams{
		ID: dbid.NewV7(), AgentID: target.AgentID, RuntimeID: agent.RuntimeID, Priority: priorityToInt("low"),
		Context: contextJSON, Purpose: pgtype.Text{String: bgagents.Purpose(in.Job), Valid: true},
		OriginatorUserID: in.MemberID, AccountableUserID: attr.AccountableUserID,
		OriginatorSource: attrSource, TriggerEvidenceKind: attrEvidenceKind, TriggerEvidenceRefID: attrEvidenceRef,
	})
	if err != nil {
		return db.AgentTaskQueue{}, fmt.Errorf("create background task: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return db.AgentTaskQueue{}, fmt.Errorf("commit background enqueue: %w", err)
	}
	s.RecordHumanDelegation(ctx, attr)
	s.NotifyTaskEnqueued(ctx, task)
	return task, nil
}

// checkChainStillNames mirrors bgagents.Resolve on the stored values, in the
// insert transaction: the source's chain must hold the agent in the slot, and a
// workspace-sourced task needs the member to have no personal chain for the job.
// The member lock covers personal writes and the workspace row lock covers
// workspace writes; this re-read is the guard if a resolver still read stale.
func (s *TaskService) checkChainStillNames(ctx context.Context, q *db.Queries, in BackgroundTaskInput, t BackgroundTarget) error {
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
	if t.Source == bgagents.WorkspaceSource {
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
	if t.Slot == bgagents.Fallback {
		slotAgent = current.FallbackAgentID
	}
	if slotAgent == "" || slotAgent != util.UUIDToString(t.AgentID) {
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
