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
	overlay := s.buildRuntimeMCPOverlay(ctx, actorID, agent)
	// The fork's run gate (triage, paused agent, run budget) and the budget
	// debit share one transaction, so the budget lock the guard takes is held
	// until the run is debited.
	q, commit, rollback := s.Queries, func(context.Context) error { return nil }, func(context.Context) {}
	if s.TxStarter != nil {
		tx, err := s.TxStarter.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin decision run: %w", err)
		}
		q, commit, rollback = s.Queries.WithTx(tx), tx.Commit, func(ctx context.Context) { _ = tx.Rollback(ctx) }
	}
	defer rollback(ctx)
	if err := guardIssueRunAllowed(ctx, q, issue.ID, OriginDerived, RunTarget{AgentID: agent.ID, Trigger: "app decision", ActorUserID: actorID}); err != nil {
		return err
	}
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
	if err := commit(ctx); err != nil {
		return fmt.Errorf("commit decision run: %w", err)
	}
	s.broadcastTaskEvent(ctx, protocol.EventTaskQueued, task)
	s.NotifyTaskEnqueued(ctx, task)
	return nil
}
