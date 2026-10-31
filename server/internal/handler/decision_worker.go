package handler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	decisionWorkerTick  = 5 * time.Second
	decisionLeaseRenew  = 10 * time.Second
	decisionBatchSize   = 100
	decisionMaxAttempts = 2
)

// Runtime teardown cancels tasks, unbinds user agents, then detaches task history.
// Decision consequences do not join those baseline transactions. A delivery merge
// can change trigger_comment_id, whose FK locks the trigger comment FOR KEY SHARE.
type DecisionWorker struct {
	handler *Handler
	holder  string
	token   pgtype.UUID
	notify  chan struct{}
	done    chan struct{}
	pass    sync.Mutex
}

func NewDecisionWorker(handler *Handler) *DecisionWorker {
	token := dbid.NewV7()
	return &DecisionWorker{
		handler: handler, holder: uuidToString(token), token: token,
		notify: make(chan struct{}, 1), done: make(chan struct{}),
	}
}

func (worker *DecisionWorker) Notify() {
	if worker == nil {
		return
	}
	select {
	case worker.notify <- struct{}{}:
	default:
	}
}

func (worker *DecisionWorker) Run(ctx context.Context) {
	if worker == nil {
		return
	}
	defer close(worker.done)
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := worker.handler.Queries.ReleaseDecisionWorkerLease(releaseCtx, db.ReleaseDecisionWorkerLeaseParams{Holder: worker.holder, Token: worker.token}); err != nil {
			slog.Warn("decision worker lease release failed", "error", err)
		}
	}()
	ticker := time.NewTicker(decisionWorkerTick)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if _, err := worker.ProcessPass(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("decision worker pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-worker.notify:
		case <-ticker.C:
		}
	}
}

func (worker *DecisionWorker) WaitWithTimeout(timeout time.Duration) bool {
	if worker == nil {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-worker.done:
		return true
	case <-timer.C:
		return false
	}
}

func (worker *DecisionWorker) ProcessPass(ctx context.Context) (bool, error) {
	if !worker.pass.TryLock() {
		return false, nil
	}
	defer worker.pass.Unlock()
	_, err := worker.handler.Queries.AcquireDecisionWorkerLease(ctx, db.AcquireDecisionWorkerLeaseParams{Holder: worker.holder, Token: worker.token})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	passCtx, cancel := context.WithCancel(ctx)
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		ticker := time.NewTicker(decisionLeaseRenew)
		defer ticker.Stop()
		for {
			select {
			case <-passCtx.Done():
				return
			case <-ticker.C:
				renewCtx, stopRenew := context.WithTimeout(passCtx, decisionWorkerTick)
				rows, err := worker.handler.Queries.RenewDecisionWorkerLease(renewCtx, db.RenewDecisionWorkerLeaseParams{Holder: worker.holder, Token: worker.token})
				stopRenew()
				if err != nil || rows != 1 {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-renewed }()
	return true, worker.processConsequences(passCtx)
}

func decisionAttempt(ctx context.Context, operation func() error) error {
	var err error
	for attempt := 0; attempt < decisionMaxAttempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err = operation()
		if !decisiontx.IsRetryable(err) {
			return err
		}
	}
	return err
}

func (worker *DecisionWorker) processConsequences(ctx context.Context) error {
	handler := worker.handler
	scopes, err := handler.Queries.ListDecisionOrphanScopes(ctx, decisionBatchSize)
	if err != nil {
		return err
	}
	var failures error
	for _, scope := range scopes {
		failures = errors.Join(failures, decisionAttempt(ctx, func() error {
			return handler.cleanupDecisionOrphans(ctx, scope.WorkspaceID, scope.IssueID)
		}))
	}
	for _, stage := range []struct {
		list  func(context.Context, int32) ([]db.DecisionRequest, error)
		apply func(context.Context, pgtype.UUID, pgtype.UUID) (decisiontx.Withdrawal, error)
	}{
		{handler.Queries.ListDecisionLifecycleCancellations, handler.CancelDecisionLifecycle},
		{handler.Queries.ListDecisionSourceChecks, handler.CheckDecisionSource},
		{handler.Queries.ListDecisionFinalizations, handler.FinalizeDecisionAnswer},
	} {
		requests, err := stage.list(ctx, decisionBatchSize)
		if err != nil {
			return errors.Join(failures, err)
		}
		for _, request := range requests {
			failures = errors.Join(failures, decisionAttempt(ctx, func() error {
				result, err := stage.apply(ctx, request.WorkspaceID, request.ID)
				if err == nil && len(result.Comments) > 0 {
					worker.publishWithdrawal(result)
				}
				return err
			}))
		}
	}
	intents, err := handler.Queries.ListDecisionSealIntents(ctx, decisionBatchSize)
	if err != nil {
		return errors.Join(failures, err)
	}
	for _, intent := range intents {
		failures = errors.Join(failures, decisionAttempt(ctx, func() error {
			_, err := handler.SealDecisionIntent(ctx, intent.WorkspaceID, intent.ID)
			return err
		}))
	}
	notices, err := handler.Queries.ListDecisionVetoNotices(ctx, decisionBatchSize)
	if err != nil {
		return errors.Join(failures, err)
	}
	for _, answer := range notices {
		failures = errors.Join(failures, decisionAttempt(ctx, func() error {
			return handler.deliverDecisionVetoNotice(ctx, answer.WorkspaceID, answer.ID)
		}))
	}
	requests, err := handler.Queries.ListDecisionPingRetries(ctx, decisionBatchSize)
	if err != nil {
		return errors.Join(failures, err)
	}
	for _, request := range requests {
		failures = errors.Join(failures, decisionAttempt(ctx, func() error {
			item, err := decisiontx.NotifyPing(ctx, handler.TxStarter, handler.Queries, request.WorkspaceID, request.ID)
			if err == nil && item != nil {
				handler.publish(protocol.EventInboxNew, uuidToString(request.WorkspaceID), "agent", uuidToString(request.AskingAgentID), map[string]any{"item": inboxToResponse(*item)})
			}
			return err
		}))
	}
	orphans, err := handler.Queries.ListDecisionActionsAwaitingPostCommit(ctx, decisionBatchSize)
	if err != nil {
		return errors.Join(failures, err)
	}
	for _, action := range orphans {
		failures = errors.Join(failures, decisionAttempt(ctx, func() error {
			return handler.completeDecisionActionPostCommit(ctx, action)
		}))
	}
	wakes, err := handler.Queries.ListDecisionWakeCandidates(ctx, decisionBatchSize)
	if err != nil {
		return errors.Join(failures, err)
	}
	for _, request := range wakes {
		failures = errors.Join(failures, decisionAttempt(ctx, func() error {
			return handler.DeliverDecisionWake(ctx, request.WorkspaceID, request.ID)
		}))
	}
	retractions, err := handler.Queries.ListDecisionRetractionCandidates(ctx, decisionBatchSize)
	if err != nil {
		return errors.Join(failures, err)
	}
	for _, answer := range retractions {
		failures = errors.Join(failures, decisionAttempt(ctx, func() error {
			return handler.deliverDecisionItem(ctx, answer.WorkspaceID, answer.ID, "retraction")
		}))
	}
	reasks, err := handler.Queries.ListDecisionReaskCandidates(ctx, decisionBatchSize)
	if err != nil {
		return errors.Join(failures, err)
	}
	for _, request := range reasks {
		failures = errors.Join(failures, decisionAttempt(ctx, func() error {
			return handler.deliverDecisionItem(ctx, request.WorkspaceID, request.ID, "reask")
		}))
	}
	recipients, err := handler.Queries.ListDecisionExpiryDigestRecipients(ctx, decisionBatchSize)
	if err != nil {
		return errors.Join(failures, err)
	}
	for _, recipient := range recipients {
		failures = errors.Join(failures, decisionAttempt(ctx, func() error {
			return handler.deliverDecisionExpiryDigest(ctx, recipient.WorkspaceID, recipient.HumanUserID)
		}))
	}
	return errors.Join(failures, handler.checkDecisionWaits(ctx))
}

func (worker *DecisionWorker) publishWithdrawal(result decisiontx.Withdrawal) {
	workspaceID, agentID := uuidToString(result.Request.WorkspaceID), uuidToString(result.Request.AskingAgentID)
	for _, comment := range result.Comments {
		worker.handler.publish(protocol.EventCommentCreated, workspaceID, "agent", agentID, map[string]any{"comment": commentToResponse(comment, nil, nil)})
	}
	worker.handler.TaskService.PublishThreadUnresolvedOnReply(result.ClearedComments, workspaceID, "agent", agentID)
	if result.Answer.DisclaimedAt.Valid {
		worker.handler.publishDecisionAnswerUpdated(workspaceID, "agent", agentID, result.Answer.ID, result.Request.IssueID, decisionStateDisclaimed)
	}
}

func (handler *Handler) cleanupDecisionOrphans(ctx context.Context, workspaceID, issueID pgtype.UUID) error {
	transaction, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer transaction.Rollback(ctx)
	queries := handler.Queries.WithTx(transaction)
	if _, err := queries.LockWorkspaceForChatSessionCreate(ctx, workspaceID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	intents, err := queries.LockDecisionOrphanIntents(ctx, db.LockDecisionOrphanIntentsParams{WorkspaceID: workspaceID, IssueID: issueID})
	if err != nil {
		return err
	}
	for _, intentID := range intents {
		if err := queries.DeleteDecisionOrphanIntent(ctx, db.DeleteDecisionOrphanIntentParams{WorkspaceID: workspaceID, ID: intentID}); err != nil {
			return err
		}
	}
	requests, err := queries.LockDecisionOrphanRequests(ctx, db.LockDecisionOrphanRequestsParams{WorkspaceID: workspaceID, IssueID: issueID})
	if err != nil {
		return err
	}
	for _, requestID := range requests {
		if err := queries.DeleteDecisionOrphanRequestAction(ctx, requestID); err != nil {
			return err
		}
		if err := queries.DeleteDecisionOrphanBoundWrite(ctx, db.DeleteDecisionOrphanBoundWriteParams{WorkspaceID: workspaceID, ID: requestID}); err != nil {
			return err
		}
		if err := queries.DeleteDecisionOrphanAgentScope(ctx, db.DeleteDecisionOrphanAgentScopeParams{WorkspaceID: workspaceID, ID: requestID}); err != nil {
			return err
		}
		if err := queries.DeleteDecisionOrphanRequest(ctx, db.DeleteDecisionOrphanRequestParams{WorkspaceID: workspaceID, ID: requestID}); err != nil {
			return err
		}
	}
	return transaction.Commit(ctx)
}
