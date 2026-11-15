package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
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

var errChoiceMismatch = decisionIntentRefusal{http.StatusBadRequest, "choice_mismatch: choose approve or reject for every item of the batch, and for no other"}

// batchSplit reads the human's per-item choice. No choices at all is the
// all-approve call of CHE-5409. A choices map must name every action of the
// batch once, each as "approve" or "reject". approved[i] is true for actions[i]
// chosen to approve.
func batchSplit(actions []db.DecisionAction, choices map[string]string) ([]bool, error) {
	approved := make([]bool, len(actions))
	if choices == nil {
		for index := range approved {
			approved[index] = true
		}
		return approved, nil
	}
	if len(choices) != len(actions) {
		return nil, errChoiceMismatch
	}
	for index, action := range actions {
		switch choices[uuidToString(action.ID)] {
		case "approve":
			approved[index] = true
		case "reject":
		default:
			return nil, errChoiceMismatch
		}
	}
	return approved, nil
}

// batchActions is the batch's actions in id order, which of them the human
// approves, and the hash that approval signs: the hash of the approved items
// only (empty when none is approved, and then no passkey is needed).
func (handler *Handler) batchActions(ctx context.Context, caller decisionActionCaller, choices map[string]string) ([]db.DecisionAction, []bool, string, error) {
	actions, err := handler.Queries.ListDecisionBatchActions(ctx, db.ListDecisionBatchActionsParams{WorkspaceID: caller.workspaceID, BatchID: caller.actionID})
	if err != nil {
		return nil, nil, "", err
	}
	if len(actions) == 0 {
		return nil, nil, "", errActionNotFound
	}
	approved, err := batchSplit(actions, choices)
	if err != nil {
		return nil, nil, "", err
	}
	var hashes []string
	for index, action := range actions {
		if approved[index] {
			hashes = append(hashes, action.PayloadSha256)
		}
	}
	if len(hashes) == 0 {
		return actions, approved, "", nil
	}
	return actions, approved, batchPayloadSHA256(hashes), nil
}

// ChallengeDecisionActionBatch opens the assertion challenge for approving the
// exact payloads the human chose to approve (all of them without choices) with
// one passkey. A batch with nothing to approve needs none.
func (handler *Handler) ChallengeDecisionActionBatch(writer http.ResponseWriter, request *http.Request) {
	// The route's id is the batch id; the caller's action id carries it.
	caller, ok := handler.authorizeDecisionActionCaller(writer, request)
	if !ok {
		return
	}
	var body struct {
		Choices map[string]string `json:"choices"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxApproveBody)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(writer, http.StatusBadRequest, "invalid request body")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), decisionActionDeadline)
	defer cancel()
	issued, err := handler.issueBatchChallenge(ctx, caller, body.Choices)
	if err != nil {
		writeDecisionActionError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, challengeResponse{
		ChallengeID: uuidToString(issued.ChallengeID), Options: issued.Options, ExpiresAt: issued.ExpiresAt,
	})
}

func (handler *Handler) issueBatchChallenge(ctx context.Context, caller decisionActionCaller, choices map[string]string) (issued passkey.Issued, err error) {
	actions, approved, hash, err := handler.batchActions(ctx, caller, choices)
	if err != nil {
		return issued, err
	}
	lead := slices.Index(approved, true)
	if lead < 0 {
		return issued, errNothingToSign
	}
	for index, action := range actions {
		if !approved[index] {
			continue
		}
		if _, err := handler.checkApprovable(ctx, decisionActionCaller{workspaceID: caller.workspaceID, actionID: action.ID, userID: caller.userID}); err != nil {
			return issued, err
		}
	}
	return handler.issueApproval(ctx, caller.userID, actions[lead].ID, hash)
}

var errNothingToSign = decisionIntentRefusal{http.StatusBadRequest, "nothing_to_sign: every item is rejected, so no passkey is needed"}

type batchApproveBody struct {
	PayloadSHA256 string          `json:"payload_sha256"`
	Assertion     json.RawMessage `json:"assertion"`
	// Choices maps each action id to "approve" or "reject". Absent: approve all.
	Choices map[string]string `json:"choices"`
}

// batchApproveResponse is the batch card's read: one outcome per item.
type batchApproveResponse struct {
	BatchID string                  `json:"batch_id"`
	Items   []decisionActionOutcome `json:"items"`
}

// ApproveDecisionActionBatch applies the human's per-item choice (every item
// approved when no choices come): the approved items with one passkey assertion
// over their hash, the rejected ones in the same transaction. Each approved item runs under its own savepoint: one
// that fails ends as failed with its own error code and the others still apply.
// Any other refusal (not pending, expired, not the human, recovery, owner-only,
// a wrong hash) refuses the whole batch and applies nothing. The batch enters
// the run-budget boundary once for all its top-ups. Items that need secret
// values or a preview digest are not batchable: the batch carries neither.
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
		if action.State == "rejected" {
			if err := handler.publishDecisionActionCardUpdated(context.WithoutCancel(ctx), action); err != nil {
				slog.Warn("decision action: card update not published", "action_id", uuidToString(action.ID), "error", err)
			}
			handler.DecisionWorker.Notify()
		} else {
			handler.finishApprovalAfterCommit(ctx, action)
		}
		outcome.Items[index] = newDecisionActionOutcome(action)
	}
	writeJSON(writer, http.StatusOK, outcome)
}

func (handler *Handler) approveDecisionActionBatch(ctx context.Context, caller decisionActionCaller, body batchApproveBody) ([]db.DecisionAction, error) {
	actions, approved, hash, err := handler.batchActions(ctx, caller, body.Choices)
	if err != nil {
		return nil, err
	}
	callers := make([]decisionActionCaller, len(actions))
	plans := make([]approvePlan, len(actions))
	for index, action := range actions {
		callers[index] = decisionActionCaller{workspaceID: caller.workspaceID, actionID: action.ID, userID: caller.userID}
		if approved[index] {
			plans[index], err = handler.planApprove(ctx, callers[index], nil)
		} else {
			_, plans[index].located, err = handler.locateDecisionAction(ctx, callers[index])
		}
		if err != nil {
			return nil, err
		}
	}
	// The stored hashes never change, so a wrong claim is refused before any
	// lock. With nothing approved there is nothing to sign.
	if hash != "" && body.PayloadSHA256 != hash {
		return nil, errPayloadMismatch
	}
	var boundary []pgtype.UUID
	for _, plan := range plans {
		if plan.boundaryIssue.Valid {
			boundary = append(boundary, plan.boundaryIssue)
		}
	}
	// One entry for every top-up's issue: a slot per item would exhaust them.
	if len(boundary) > 0 {
		leave, err := handler.enterRunBudgetBoundaries(ctx, boundary...)
		if err != nil {
			return nil, errBusyRetry
		}
		defer leave()
	}
	var finished []db.DecisionAction
	_, err = retryActionTx(ctx, "approve_batch", callers[0], plans[0].located.IssueID, func() (db.DecisionAction, error) {
		var err error
		finished, err = handler.approveBatchTx(ctx, body, callers, plans, approved, boundary)
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

// approveBatchTx locks and checks every item, verifies the one assertion over
// the approved items (none when all are rejected), then executes each approved
// item and rejects each rejected one. An approved item whose target drifted ends
// as failed without executing.
func (handler *Handler) approveBatchTx(ctx context.Context, body batchApproveBody, callers []decisionActionCaller, plans []approvePlan, approved []bool, boundary []pgtype.UUID) ([]db.DecisionAction, error) {
	transaction, err := handler.beginActionTx(ctx, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer transaction.Rollback(context.WithoutCancel(ctx))
	// The boundary locks come first, in id order, so two batches never wait on
	// each other; each item's own lock after them is re-entrant.
	slices.SortFunc(boundary, func(a, b pgtype.UUID) int { return bytes.Compare(a.Bytes[:], b.Bytes[:]) })
	for _, issueID := range slices.Compact(boundary) {
		if err := handler.Queries.WithTx(transaction).LockIssueRunBudgetBoundary(ctx, issueID); err != nil {
			return nil, err
		}
	}
	locked := make([]approveLocked, len(plans))
	targets := make([]approveTargets, len(plans))
	drifted := make([]bool, len(plans))
	for index, plan := range plans {
		if locked[index], err = handler.lockApproveRows(ctx, transaction, callers[index], plan); err != nil {
			return nil, err
		}
		if !approved[index] {
			if err := checkRejectable(ctx, locked[index].queries, callers[index], locked[index].current, locked[index].action); err != nil {
				return nil, err
			}
			continue
		}
		// The batch hash was checked as a whole; each item's own hash is
		// re-derived under the lock against its stored one.
		targets[index], err = checkApproveItem(ctx, locked[index], callers[index], plan.target, locked[index].action.PayloadSha256, "")
		if drifted[index] = errors.Is(err, errPreconditionDrift); err != nil && !drifted[index] {
			return nil, err
		}
	}
	// The first approved item leads: the passkey challenge names it.
	var credential pgtype.UUID
	if lead := slices.Index(approved, true); lead >= 0 {
		credential, err = handler.ApprovalVerifier(ctx, locked[lead].queries, ApprovalClaim{
			WorkspaceID: callers[lead].workspaceID, ActionID: callers[lead].actionID, UserID: callers[lead].userID,
			PayloadSHA256: body.PayloadSHA256, Assertion: body.Assertion,
		})
		if err != nil {
			return nil, err
		}
	}
	finished := make([]db.DecisionAction, len(plans))
	for index, plan := range plans {
		if !approved[index] {
			finished[index], err = rejectLockedAction(ctx, locked[index].queries, callers[index], locked[index].current, locked[index].action)
		} else if drifted[index] {
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
