package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/attribution"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// EnqueueAppDecisionRun resumes an agent on an issue after a person decided
// on one of its app requests. The decision rides in the handoff note. A run
// of the agent that is still waiting to start for the same person takes the
// note along instead of a second run being queued.
func (s *TaskService) EnqueueAppDecisionRun(ctx context.Context, issueID, agentID, actorID, requestID pgtype.UUID, note string) error {
	issue, err := s.Queries.GetIssue(ctx, issueID)
	if err != nil {
		return fmt.Errorf("load issue: %w", err)
	}
	agent, err := s.Queries.GetAgent(ctx, agentID)
	if err != nil {
		return fmt.Errorf("load agent: %w", err)
	}
	if agent.ArchivedAt.Valid {
		return ErrChatTaskAgentArchived
	}
	if !agent.RuntimeID.Valid {
		return ErrChatTaskAgentNoRuntime
	}
	if err := s.guardRunProjectAccess(ctx, issue, agent, actorID, actorID); err != nil {
		return err
	}
	waiting, err := s.Queries.FindWaitingIssueRun(ctx, db.FindWaitingIssueRunParams{IssueID: issueID, AgentID: agentID, OriginatorUserID: actorID})
	if err == nil {
		folded, err := s.Queries.AppendQueuedTaskHandoffNote(ctx, db.AppendQueuedTaskHandoffNoteParams{ID: waiting, Note: note})
		if err != nil {
			return fmt.Errorf("fold decision into waiting run: %w", err)
		}
		if folded > 0 {
			return nil
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("find waiting run: %w", err)
	}
	if s.TxStarter == nil {
		return fmt.Errorf("decision run requires transaction support")
	}
	// The fork's run guard takes the issue's budget lock inside a transaction
	// and the debit lands in the same one, so a decision run counts against the
	// issue's run budget like every other derived run.
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin decision run: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := s.Queries.WithTx(tx)
	if err := guardIssueRunAllowed(ctx, q, issue.ID, OriginDerived, RunTarget{AgentID: agent.ID, Trigger: "app decision"}); err != nil {
		return err
	}
	overlay := s.buildRuntimeMCPOverlay(ctx, actorID, agent)
	task, err := q.CreateWakeupTask(ctx, db.CreateWakeupTaskParams{
		ID:                   dbid.NewV7(),
		AgentID:              agent.ID,
		RuntimeID:            agent.RuntimeID,
		IssueID:              issue.ID,
		Priority:             priorityToInt(issue.Priority),
		TriggerSummary:       pgtype.Text{String: "App request decided", Valid: true},
		HandoffNote:          pgtype.Text{String: note, Valid: true},
		OriginatorUserID:     actorID,
		AccountableUserID:    actorID,
		OriginatorSource:     pgtype.Text{String: string(attribution.SourceDirectHuman), Valid: true},
		TriggerEvidenceKind:  pgtype.Text{String: "app_action_request", Valid: true},
		TriggerEvidenceRefID: requestID,
		RuntimeMcpOverlay:    overlay.Overlay,
		RuntimeConnectedApps: overlay.ConnectedApps,
	})
	if err != nil {
		return fmt.Errorf("create decision run: %w", err)
	}
	if err := noteRunInserted(ctx, q, issue.ID, task.ID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit decision run: %w", err)
	}
	s.broadcastTaskEvent(ctx, protocol.EventTaskQueued, task)
	s.NotifyTaskEnqueued(ctx, task)
	return nil
}
