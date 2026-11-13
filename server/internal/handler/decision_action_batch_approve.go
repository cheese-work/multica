package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/passkey"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// batchPayloadSHA256 is the hash a human signs for a batch: the hash of the
// items' payload hashes, sorted, so the order of the card does not matter.
func batchPayloadSHA256(itemHashes []string) string {
	sorted := slices.Sorted(slices.Values(itemHashes))
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return hex.EncodeToString(sum[:])
}

// batchActions is the batch's actions in id order and the hash they sign. The
// first action leads: the passkey challenge names it.
func (handler *Handler) batchActions(ctx context.Context, caller decisionActionCaller) ([]db.DecisionAction, string, error) {
	actions, err := handler.Queries.ListDecisionBatchActions(ctx, db.ListDecisionBatchActionsParams{WorkspaceID: caller.workspaceID, BatchID: caller.actionID})
	if err != nil {
		return nil, "", err
	}
	if len(actions) == 0 {
		return nil, "", errActionNotFound
	}
	hashes := make([]string, len(actions))
	for index, action := range actions {
		hashes[index] = action.PayloadSha256
	}
	return actions, batchPayloadSHA256(hashes), nil
}

// ChallengeDecisionActionBatch opens the assertion challenge for approving the
// batch's exact payloads with one passkey.
func (handler *Handler) ChallengeDecisionActionBatch(writer http.ResponseWriter, request *http.Request) {
	// The route's id is the batch id; the caller's action id carries it.
	caller, ok := handler.authorizeDecisionActionCaller(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), decisionActionDeadline)
	defer cancel()
	issued, err := handler.issueBatchChallenge(ctx, caller)
	if err != nil {
		writeDecisionActionError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, challengeResponse{
		ChallengeID: uuidToString(issued.ChallengeID), Options: issued.Options, ExpiresAt: issued.ExpiresAt,
	})
}

func (handler *Handler) issueBatchChallenge(ctx context.Context, caller decisionActionCaller) (issued passkey.Issued, err error) {
	actions, hash, err := handler.batchActions(ctx, caller)
	if err != nil {
		return issued, err
	}
	for _, action := range actions {
		if _, err := handler.checkApprovable(ctx, decisionActionCaller{workspaceID: caller.workspaceID, actionID: action.ID, userID: caller.userID}); err != nil {
			return issued, err
		}
	}
	return handler.issueApproval(ctx, caller.userID, actions[0].ID, hash)
}

type batchApproveBody struct {
	PayloadSHA256 string          `json:"payload_sha256"`
	Assertion     json.RawMessage `json:"assertion"`
}

// batchApproveResponse is the batch card's read: one outcome per item.
type batchApproveResponse struct {
	BatchID string                  `json:"batch_id"`
	Items   []decisionActionOutcome `json:"items"`
}

// ApproveDecisionActionBatch approves every action of a batch with one passkey
// assertion over the batch hash. Each item runs under its own savepoint: one
// that fails ends as failed with its own error code and the others still apply.
// Any other refusal (not pending, expired, not the human, recovery, owner-only,
// a wrong hash) refuses the whole batch and applies nothing. Items that need
// secret values or a preview digest are not batchable and refuse the batch.
func (handler *Handler) ApproveDecisionActionBatch(writer http.ResponseWriter, request *http.Request) {
	caller, ok := handler.authorizeDecisionActionCaller(writer, request)
	if !ok {
		return
	}
	var body batchApproveBody
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxApproveBody)).Decode(&body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid request body")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), decisionActionDeadline)
	defer cancel()
	actions, err := handler.approveDecisionActionBatch(ctx, caller, body)
	if err != nil {
		writeDecisionActionError(writer, err)
		return
	}
	outcome := batchApproveResponse{BatchID: uuidToString(caller.actionID), Items: make([]decisionActionOutcome, len(actions))}
	for index, action := range actions {
		handler.finishApprovalAfterCommit(ctx, action)
		outcome.Items[index] = newDecisionActionOutcome(action)
	}
	writeJSON(writer, http.StatusOK, outcome)
}

func (handler *Handler) approveDecisionActionBatch(ctx context.Context, caller decisionActionCaller, body batchApproveBody) ([]db.DecisionAction, error) {
	actions, hash, err := handler.batchActions(ctx, caller)
	if err != nil {
		return nil, err
	}
	callers := make([]decisionActionCaller, len(actions))
	plans := make([]approvePlan, len(actions))
	for index, action := range actions {
		callers[index] = decisionActionCaller{workspaceID: caller.workspaceID, actionID: action.ID, userID: caller.userID}
		if plans[index], err = handler.planApprove(ctx, callers[index], nil); err != nil {
			return nil, err
		}
		if boundary := plans[index].boundaryIssue; boundary.Valid {
			leave, err := handler.enterRunBudgetBoundary(ctx, boundary)
			if err != nil {
				return nil, errBusyRetry
			}
			defer leave()
		}
	}
	// The stored hashes never change, so a wrong claim is refused before any lock.
	if body.PayloadSHA256 != hash {
		return nil, errPayloadMismatch
	}
	var finished []db.DecisionAction
	_, err = retryActionTx(ctx, "approve_batch", callers[0], plans[0].located.IssueID, func() (db.DecisionAction, error) {
		var err error
		finished, err = handler.approveBatchTx(ctx, body, callers, plans)
		return db.DecisionAction{}, err
	})
	var uncertain uncertainCommit
	if errors.As(err, &uncertain) {
		// One transaction commits every item, so the lead's state is the batch's.
		if _, err := handler.readbackAfterUncertainCommit(ctx, callers[0]); err != nil {
			return nil, err
		}
		return handler.Queries.ListDecisionBatchActions(ctx, db.ListDecisionBatchActionsParams{WorkspaceID: caller.workspaceID, BatchID: caller.actionID})
	}
	handler.markSuspectAfterRefusal(ctx, err)
	return finished, err
}

// approveBatchTx locks and checks every item, verifies the one assertion, then
// executes each item in its own savepoint. An item whose target drifted ends as
// failed without executing.
func (handler *Handler) approveBatchTx(ctx context.Context, body batchApproveBody, callers []decisionActionCaller, plans []approvePlan) ([]db.DecisionAction, error) {
	transaction, err := handler.beginActionTx(ctx, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer transaction.Rollback(context.WithoutCancel(ctx))
	locked := make([]approveLocked, len(plans))
	targets := make([]approveTargets, len(plans))
	drifted := make([]bool, len(plans))
	for index, plan := range plans {
		if locked[index], err = handler.lockApproveRows(ctx, transaction, callers[index], plan); err != nil {
			return nil, err
		}
		// The batch hash was checked as a whole; each item's own hash is
		// re-derived under the lock against its stored one.
		targets[index], err = checkApproveItem(ctx, locked[index], callers[index], plan.target, locked[index].action.PayloadSha256, "")
		if drifted[index] = errors.Is(err, errPreconditionDrift); err != nil && !drifted[index] {
			return nil, err
		}
	}
	credential, err := handler.ApprovalVerifier(ctx, locked[0].queries, ApprovalClaim{
		WorkspaceID: callers[0].workspaceID, ActionID: callers[0].actionID, UserID: callers[0].userID,
		PayloadSHA256: body.PayloadSHA256, Assertion: body.Assertion,
	})
	if err != nil {
		return nil, err
	}
	finished := make([]db.DecisionAction, len(plans))
	for index, plan := range plans {
		if drifted[index] {
			failure := db.FinishDecisionActionRowParams{
				State: "failed", ErrorCode: pgtype.Text{String: "precondition_drift", Valid: true},
				ErrorMessage: pgtype.Text{String: errPreconditionDrift.message, Valid: true},
			}
			finished[index], err = recordApproved(ctx, locked[index].queries, callers[index], locked[index], failure, credential)
		} else {
			finished[index], err = handler.executeApproved(ctx, transaction, callers[index], plan, locked[index], targets[index], nil, credential)
		}
		if err != nil {
			return nil, err
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		return nil, uncertainCommit{err}
	}
	return finished, nil
}
