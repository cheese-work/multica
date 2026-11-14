package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// maxApproveBody bounds the approval request: a payload hash, one assertion
// and a few secret values.
const maxApproveBody = 64 << 10

// ApprovalClaim is what the human's click presents for verification.
type ApprovalClaim struct {
	WorkspaceID, ActionID, UserID pgtype.UUID
	PayloadSHA256                 string
	Assertion                     json.RawMessage
}

// ApprovalVerifier proves the human approved exactly this payload. It runs
// inside the approval transaction, owns lock rows 10 and 12 (the assertion
// challenge and the passkey credential), and returns the credential it used.
// Without one the approve endpoint refuses every request.
type ApprovalVerifier func(ctx context.Context, queries *db.Queries, claim ApprovalClaim) (pgtype.UUID, error)

// actionFailure is a business refusal raised while executing an approved
// action. It ends the action as failed and undoes its writes; it is never a
// database error. Messages must not carry a secret value.
type actionFailure struct{ Code, Message string }

func (failure actionFailure) Error() string { return failure.Message }

// decisionActionExecution is everything an executor may touch: the caller's
// transaction, the rows the approval already locked, and the secret values
// supplied with this click (never stored by the approval itself).
type decisionActionExecution struct {
	Queries  *db.Queries
	Approver verifiedApprover
	Action   db.DecisionAction
	Agent    db.Agent
	Runtime  db.AgentRuntime
	Secrets  map[string]string
	// Workflow, Squad and Workspace are the locked, re-read target of an action
	// on that kind; only the one matching its target is set.
	Workflow  db.IssueWorkflow
	Squad     db.Squad
	Workspace db.Workspace
	// CredentialID is the passkey credential that signed this approval and
	// IssueID the issue the request sits on; both go into a config-change audit.
	CredentialID, IssueID pgtype.UUID
	// Tx is the approval transaction Queries runs in, for a write that needs
	// the transaction itself.
	Tx pgx.Tx
	// ProjectID is the project the approval locked: the one the issue was in
	// when the plan read it. Set for issue.workflow.set only.
	ProjectID pgtype.UUID
}

type decisionActionExecutor func(ctx context.Context, execution decisionActionExecution) (json.RawMessage, error)

// decisionActionExecutors maps an allowlisted action type to its core.
var decisionActionExecutors = map[string]decisionActionExecutor{
	"agent.env.patch":        executeEnvPatchAction,
	"agent.copy":             executeAgentCopyAction,
	agentPauseAction:         executeAgentPauseAction,
	agentResumeAction:        executeAgentResumeAction,
	"issue.run_budget.raise": executeRunBudgetRaiseAction,
	runBudgetTopUpType:       executeRunBudgetTopUpAction,
	issueWorkflowSetType:     executeIssueWorkflowSetAction,
	issueProjectSetType:      executeIssueProjectSetAction,
}

// decisionActionPermitted applies the rule of the action's own HTTP path to
// the approver's current role, read under the member lock: a passkey proves
// who clicked, not that they may do this.
func decisionActionPermitted(actionType string, target db.Agent, approver db.Member) bool {
	if actionType == "agent.env.patch" {
		return canManageAgentEnv(target, approver)
	}
	if isAgentSchedulingAction(actionType) {
		// The rule of requireSchedulingAdmin: workspace owner or admin only.
		return roleAllowed(approver.Role, "owner", "admin")
	}
	return true
}

var (
	errApprovalUnavailable = decisionIntentRefusal{http.StatusForbidden, "approval_proof_unavailable: this server cannot verify an approval yet"}
	errNotExecutable       = decisionIntentRefusal{http.StatusNotImplemented, "action_not_executable: this action type cannot be approved yet"}
	errPayloadMismatch     = decisionIntentRefusal{http.StatusConflict, "payload_mismatch: the approved payload is not the stored payload"}
	errPreconditionDrift   = decisionIntentRefusal{http.StatusConflict, "precondition_drift: the target changed since the request; ask again"}
	errActionExpired       = decisionIntentRefusal{http.StatusConflict, "expired: this action's approval window has closed"}
	errNeedsOwner          = decisionIntentRefusal{http.StatusForbidden, decisionClassNeedsOwner + ": this action needs the workspace owner"}
	errPreviewRequired     = decisionIntentRefusal{http.StatusConflict, "preview_required: approve with the digest of the preview you were shown"}
	errRefusalMismatch     = decisionIntentRefusal{http.StatusConflict, "refusal_mismatch: params.refusal_id must name a refusal of params.issue_id and params.window_id"}
	errNotPermitted        = decisionIntentRefusal{http.StatusForbidden, "not_permitted: you may not make this change to this agent"}
	errSecretsMismatch     = decisionIntentRefusal{http.StatusBadRequest, "secrets_mismatch: supply a value for exactly the secret slots, none masked or empty"}
)

type approveBody struct {
	PayloadSHA256 string            `json:"payload_sha256"`
	Assertion     json.RawMessage   `json:"assertion"`
	Secrets       map[string]string `json:"secrets"`
	PreviewSHA256 string            `json:"preview_sha256"`
	Note          string            `json:"note"`
}

// ApproveDecisionAction runs a pending action after its human approves it.
// Execution and the terminal state commit in one transaction; a business
// failure undoes only the execution (a savepoint) and still records the click.
func (handler *Handler) ApproveDecisionAction(writer http.ResponseWriter, request *http.Request) {
	caller, ok := handler.authorizeDecisionActionCaller(writer, request)
	if !ok {
		return
	}
	var body approveBody
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxApproveBody)).Decode(&body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid request body")
		return
	}
	if caller.note = strings.TrimSpace(body.Note); len(caller.note) > maxDecisionNote {
		writeDecisionActionError(writer, errNoteTooLong)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), decisionActionDeadline)
	defer cancel()
	action, err := handler.approveDecisionAction(ctx, caller, body)
	if err != nil {
		writeDecisionActionError(writer, err)
		return
	}
	handler.finishApprovalAfterCommit(ctx, action)
	writeJSON(writer, http.StatusOK, newDecisionActionOutcome(action))
}

// approvePlan is what an approval reads before its transaction: the executor,
// the action's target and the keys its locks take.
type approvePlan struct {
	executor                         decisionActionExecutor
	located                          db.DecisionRequest
	target, runtimeID, boundaryIssue pgtype.UUID
	queueKey                         string
	catalogLock                      bool
	// workflowSet takes the status catalog (shared) and project locks of an
	// issue workflow move; project is the project the issue was in when read.
	workflowSet bool
	project     pgtype.UUID
}

// planApprove runs the pre-transaction refusals of one action's approval.
func (handler *Handler) planApprove(ctx context.Context, caller decisionActionCaller, secrets map[string]string) (approvePlan, error) {
	preview, located, err := handler.locateDecisionAction(ctx, caller)
	if err != nil {
		return approvePlan{}, err
	}
	if handler.ApprovalVerifier == nil {
		return approvePlan{}, errApprovalUnavailable
	}
	executor, found := decisionActionExecutors[preview.ActionType]
	var target pgtype.UUID
	var targeted bool
	switch actionTargetKind(preview) {
	case decisionaction.TargetIssue, decisionaction.TargetWorkflow, decisionaction.TargetSquad, decisionaction.TargetWorkspace:
		targeted = true
	default:
		target, targeted = decisionActionTarget(preview)
	}
	if !found || !targeted {
		return approvePlan{}, errNotExecutable
	}
	if err := checkSecretValues(preview, secrets); err != nil {
		return approvePlan{}, err
	}
	plan := approvePlan{executor: executor, located: located, target: target, runtimeID: decisionActionRuntime(preview)}
	change, configured := configChanges[preview.ActionType]
	if configured && change.QueueField != "" && target.Valid {
		plan.queueKey = configChangeLockKey(uuidToString(caller.workspaceID), change.QueueField, uuidToString(target))
	}
	plan.catalogLock = configured && change.CatalogLock
	// A top-up opens a run-budget window, so it takes its turn behind any move
	// that holds the issue's boundary.
	if preview.ActionType == runBudgetTopUpType {
		plan.boundaryIssue = located.IssueID
	}
	// A workflow move rebinds the budget, so it queues behind the boundary too,
	// and locks the project the issue is in now (checked again under the issue lock).
	if preview.ActionType == issueWorkflowSetType {
		issue, err := handler.Queries.GetIssue(ctx, located.IssueID)
		if err != nil {
			return approvePlan{}, errPreconditionDrift
		}
		plan.boundaryIssue, plan.workflowSet, plan.project = located.IssueID, true, issue.ProjectID
	}
	return plan, nil
}

func (handler *Handler) approveDecisionAction(ctx context.Context, caller decisionActionCaller, body approveBody) (db.DecisionAction, error) {
	empty := db.DecisionAction{}
	plan, err := handler.planApprove(ctx, caller, body.Secrets)
	if err != nil {
		return empty, err
	}
	// Waiting for the turn holds no connection.
	if plan.boundaryIssue.Valid {
		leave, err := handler.enterRunBudgetBoundary(ctx, plan.boundaryIssue)
		if err != nil {
			return empty, errBusyRetry
		}
		defer leave()
	}
	action, err := retryActionTx(ctx, "approve", caller, plan.located.IssueID, func() (db.DecisionAction, error) {
		return handler.approveDecisionActionTx(ctx, caller, body, plan)
	})
	var uncertain uncertainCommit
	if errors.As(err, &uncertain) {
		return handler.readbackAfterUncertainCommit(ctx, caller)
	}
	handler.markSuspectAfterRefusal(ctx, err)
	return action, err
}

// approveDecisionActionTx runs the approve lock sequence (plan 3.2, rows 1 to
// 12) and executes the action inside a savepoint.
func (handler *Handler) approveDecisionActionTx(ctx context.Context, caller decisionActionCaller, body approveBody, plan approvePlan) (db.DecisionAction, error) {
	empty := db.DecisionAction{}
	transaction, err := handler.beginActionTx(ctx, 2*time.Second)
	if err != nil {
		return empty, err
	}
	defer transaction.Rollback(context.WithoutCancel(ctx))
	locked, err := handler.lockApproveRows(ctx, transaction, caller, plan)
	if err != nil {
		return empty, err
	}
	targets, err := checkApproveItem(ctx, locked, caller, plan.target, body.PayloadSHA256, body.PreviewSHA256)
	if err != nil {
		return empty, err
	}
	credential, err := handler.ApprovalVerifier(ctx, locked.queries, ApprovalClaim{
		WorkspaceID: caller.workspaceID, ActionID: locked.action.ID, UserID: caller.userID,
		PayloadSHA256: body.PayloadSHA256, Assertion: body.Assertion,
	})
	if err != nil {
		return empty, err
	}
	finished, err := handler.executeApproved(ctx, transaction, caller, plan, locked, targets, body.Secrets, credential)
	if err != nil {
		return empty, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return empty, uncertainCommit{err}
	}
	return finished, nil
}

// approveLocked is what the lock sequence read: rows 1 to 9.
type approveLocked struct {
	queries *db.Queries
	runtime db.AgentRuntime
	agents  []db.Agent
	current db.DecisionRequest
	action  db.DecisionAction
}

// approveTargets is the locked, re-read target of the action's kind.
type approveTargets struct {
	agent     db.Agent
	workflow  db.IssueWorkflow
	squad     db.Squad
	workspace db.Workspace
}

// lockApproveRows takes the approve locks, rows 1 to 9, in the plan's order.
func (handler *Handler) lockApproveRows(ctx context.Context, transaction pgx.Tx, caller decisionActionCaller, plan approvePlan) (approveLocked, error) {
	located := plan.located
	var locked approveLocked
	// Before any other lock, so the wait cannot sit in a lock cycle; the
	// config change's own try-lock later in this transaction is re-entrant.
	// The catalog lock is first of all, the order a workflow save takes.
	if plan.catalogLock {
		if err := handler.Queries.WithTx(transaction).LockIssueStatusCatalog(ctx, caller.workspaceID); err != nil {
			return locked, err
		}
	}
	if plan.queueKey != "" {
		if _, err := transaction.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, plan.queueKey); err != nil {
			return locked, err
		}
	}
	queries := handler.Queries.WithTx(transaction)
	locked.queries = queries
	// The boundary lock comes before every row lock, the order a status move
	// uses: it holds the boundary across its status write and its activity, so a
	// top-up that took the issue row first could read the window between them.
	if plan.boundaryIssue.Valid {
		if err := queries.LockIssueRunBudgetBoundary(ctx, plan.boundaryIssue); err != nil {
			return locked, err
		}
	}
	// A workflow move takes the order every issue writer takes: catalog
	// (shared), project, then the issue row below.
	if plan.workflowSet {
		if err := queries.LockIssueStatusCatalogShared(ctx, caller.workspaceID); err != nil {
			return locked, err
		}
		if plan.project.Valid {
			if _, err := queries.LockProjectForIssueWrite(ctx, db.LockProjectForIssueWriteParams{ID: plan.project, WorkspaceID: caller.workspaceID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return locked, atStep("project", err)
			}
		}
	}
	if _, err := queries.LockWorkspaceKeyShareNowait(ctx, caller.workspaceID); err != nil {
		return locked, atStep("workspace", err)
	}
	var err error
	if plan.runtimeID.Valid {
		locked.runtime, err = queries.LockAgentRuntimeNowait(ctx, db.LockAgentRuntimeNowaitParams{ID: plan.runtimeID, WorkspaceID: caller.workspaceID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return locked, atStep("runtime", err)
		}
	}
	if _, err := queries.LockDecisionSessionNowait(ctx, db.LockDecisionSessionNowaitParams{WorkspaceID: caller.workspaceID, ID: located.ChatSessionID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return locked, atStep("chat_session", err)
	}
	if locked.agents, err = queries.LockDecisionAgentsNowait(ctx, db.LockDecisionAgentsNowaitParams{WorkspaceID: caller.workspaceID, Ids: distinctUUIDs(located.AskingAgentID, plan.target)}); err != nil {
		return locked, atStep("agents", err)
	}
	if err := lockDecisionThreadAndIssue(ctx, queries, caller.workspaceID, located); err != nil {
		return locked, err
	}
	if locked.current, err = queries.LockDecisionRequestNowait(ctx, db.LockDecisionRequestNowaitParams{WorkspaceID: caller.workspaceID, ID: located.ID}); err != nil {
		return locked, atStep("request", err)
	}
	if _, err := queries.LockDecisionLiveAnswerNowait(ctx, db.LockDecisionLiveAnswerNowaitParams{WorkspaceID: caller.workspaceID, ID: locked.current.ID}); err == nil {
		return locked, errNotPending
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return locked, atStep("answer", err)
	}
	if locked.action, err = queries.LockDecisionActionNowait(ctx, db.LockDecisionActionNowaitParams{ID: caller.actionID, WorkspaceID: caller.workspaceID}); err != nil {
		return locked, atStep("action", err)
	}
	return locked, nil
}

// checkApproveItem applies, under the locks and before the passkey, every
// refusal of one action: its human, recovery, owner, state, expiry, target,
// hashes and preconditions. A drifted target answers errPreconditionDrift.
func checkApproveItem(ctx context.Context, locked approveLocked, caller decisionActionCaller, target pgtype.UUID, payloadSHA256, previewSHA256 string) (approveTargets, error) {
	var targets approveTargets
	queries, current, action := locked.queries, locked.current, locked.action
	if current.HumanUserID != caller.userID {
		return targets, errNotTheHuman
	}
	// Every action class: recovery revoked the person's keys and nothing
	// executable is approved until they hold two new ones.
	if err := refuseWhileRecovering(ctx, queries, caller.userID); err != nil {
		return targets, err
	}
	// A server-opened request is addressed to any member (CHE-2318), but an
	// action on it moves real state: it needs owner authority, not the member
	// shortcut.
	if isServerDecision(current) {
		issue, err := queries.GetIssue(ctx, current.IssueID)
		if err != nil {
			return targets, err
		}
		if err := requireDecisionAuthorityIn(ctx, queries, issue, caller.userID, "spend", pgtype.UUID{}, false, http.StatusForbidden); err != nil {
			return targets, err
		}
	}
	targetKind, ownerOnly := actionTargetKind(action), actionOwnerOnly(action)
	// An owner-only action refuses a non-owner before the passkey, whatever the
	// request's class or state: the role read under the member lock below
	// decides again.
	if ownerOnly {
		member, err := queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: caller.userID, WorkspaceID: caller.workspaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return targets, errNotTheHuman
		} else if err != nil {
			return targets, err
		}
		if member.Role != "owner" {
			return targets, errNeedsOwner
		}
	}
	// The lifecycle worker may have recorded the owner cancellation before this
	// approval; an owner-only action still answers with the owner refusal.
	if current.Status == "cancelled" && current.CancelReason.String == decisionClassNeedsOwner && ownerOnly {
		return targets, errNeedsOwner
	}
	if current.Status != "open" || action.State != "pending" {
		return targets, errNotPending
	}
	if expired, err := queries.DecisionActionExpired(ctx, db.DecisionActionExpiredParams{ID: action.ID, WorkspaceID: caller.workspaceID}); err != nil {
		return targets, err
	} else if expired {
		return targets, errActionExpired
	}
	// A committed issue close or asking-agent archive wins over a stale open
	// row: the sweeper that records it runs later, so check it under the locks.
	if reason, err := queries.DecisionLifecycleReason(ctx, db.DecisionLifecycleReasonParams{WorkspaceID: caller.workspaceID, ID: current.ID}); err != nil {
		return targets, err
	} else if reason == decisionClassNeedsOwner && ownerOnly {
		// The sweeper would cancel this request, but an owner-only action answers
		// a non-owner approver with the owner refusal, not a stale row.
		return targets, errNeedsOwner
	} else if reason != "" {
		return targets, errNotPending
	}
	// Row 6 already holds the issue lock to the end of this transaction, so an
	// issue-target executor runs under it once params.issue_id is that issue. A
	// workflow, squad or workspace target is resolved in the request's workspace
	// and locked here (row 9), before the passkey is checked.
	var err error
	switch targetKind {
	case decisionaction.TargetIssue:
		if err := checkIssueTarget(action.Params, current.IssueID, http.StatusConflict); err != nil {
			return targets, err
		}
		if err := checkRefusalBinding(ctx, queries, action.Params); err != nil {
			return targets, err
		}
	case decisionaction.TargetWorkflow:
		if targets.workflow, err = loadWorkflowTarget(ctx, queries, caller.workspaceID, action.Params, http.StatusConflict, true); err != nil {
			return targets, err
		}
	case decisionaction.TargetSquad:
		if targets.squad, err = loadSquadTarget(ctx, queries, caller.workspaceID, action.Params, http.StatusConflict, true); err != nil {
			return targets, err
		}
	case decisionaction.TargetWorkspace:
		if targets.workspace, err = loadWorkspaceTarget(ctx, queries, caller.workspaceID, action.Params, http.StatusConflict, true); err != nil {
			return targets, err
		}
	}
	if err := checkPayloadHash(action, payloadSHA256); err != nil {
		return targets, err
	}
	if err := checkPreviewDigest(action, previewSHA256); err != nil {
		return targets, err
	}
	if targetKind == decisionaction.TargetAgent {
		var ok bool
		if targets.agent, ok = agentByID(locked.agents, target); !ok {
			return targets, errActionNotFound
		}
		if err := checkPreconditions(action, targets.agent); err != nil {
			return targets, err
		}
	}
	return targets, nil
}

// executeApproved runs after the passkey: it re-applies the owner rule under
// the member lock, executes the action in its savepoint and records the result.
func (handler *Handler) executeApproved(ctx context.Context, transaction pgx.Tx, caller decisionActionCaller, plan approvePlan, locked approveLocked, targets approveTargets, secrets map[string]string, credential pgtype.UUID) (db.DecisionAction, error) {
	empty := db.DecisionAction{}
	queries, current, action := locked.queries, locked.current, locked.action
	approver, err := queries.LockDecisionActionMemberNowait(ctx, db.LockDecisionActionMemberNowaitParams{WorkspaceID: caller.workspaceID, UserID: caller.userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, errNotTheHuman
	} else if err != nil {
		return empty, atStep("member", err)
	}
	// The passkey check can outlast a role change that committed after the
	// pre-lock reads, so the owner rule is applied again to the role read under
	// the member lock: for an owner-only action and for any server-opened
	// request.
	if (actionOwnerOnly(action) || isServerDecision(current)) && approver.Role != "owner" {
		return empty, errNeedsOwner
	}
	if actionTargetKind(action) == decisionaction.TargetAgent && !decisionActionPermitted(action.ActionType, targets.agent, approver) {
		return empty, errNotPermitted
	}
	finish, err := executeInSavepoint(ctx, transaction, queries, plan.executor, decisionActionExecution{
		Queries: queries, Approver: verifiedApprover{UserID: caller.userID, DecisionActionID: action.ID},
		Action: action, Agent: targets.agent, Runtime: locked.runtime, Secrets: secrets, Workflow: targets.workflow, Squad: targets.squad, Workspace: targets.workspace,
		CredentialID: credential, IssueID: current.IssueID, Tx: transaction, ProjectID: plan.project,
	})
	if err != nil {
		return empty, err
	}
	return recordApproved(ctx, queries, caller, locked, finish, credential)
}

// recordApproved ends the action with the finish and answers its card.
func recordApproved(ctx context.Context, queries *db.Queries, caller decisionActionCaller, locked approveLocked, finish db.FinishDecisionActionRowParams, credential pgtype.UUID) (db.DecisionAction, error) {
	finish.ID, finish.WorkspaceID, finish.ApprovedByUserID, finish.WebauthnCredentialID = locked.action.ID, caller.workspaceID, caller.userID, credential
	finished, err := queries.FinishDecisionActionRow(ctx, finish)
	if err != nil {
		return db.DecisionAction{}, err
	}
	if err := finishCardDecision(ctx, queries, caller, locked.current, finished, "approved", withDecisionNote(decisionActionSummary(finished), "Note", caller.note)); err != nil {
		return db.DecisionAction{}, err
	}
	return finished, nil
}

// executeInSavepoint runs the executor so a business failure undoes only the
// execution. Any other error aborts the whole transaction.
func executeInSavepoint(ctx context.Context, transaction pgx.Tx, queries *db.Queries, executor decisionActionExecutor, execution decisionActionExecution) (db.FinishDecisionActionRowParams, error) {
	finish := db.FinishDecisionActionRowParams{}
	if _, err := transaction.Exec(ctx, "SAVEPOINT action_exec"); err != nil {
		return finish, err
	}
	result, err := executor(ctx, execution)
	var failure actionFailure
	switch {
	case errors.As(err, &failure):
		if _, err := transaction.Exec(ctx, "ROLLBACK TO SAVEPOINT action_exec"); err != nil {
			return finish, err
		}
		finish.State = "failed"
		finish.ErrorCode = pgtype.Text{String: failure.Code, Valid: true}
		finish.ErrorMessage = pgtype.Text{String: failure.Message, Valid: true}
		return finish, nil
	case err != nil:
		return finish, err
	}
	if _, err := transaction.Exec(ctx, "RELEASE SAVEPOINT action_exec"); err != nil {
		return finish, err
	}
	finish.State, finish.Result = "executed", result
	return finish, nil
}

// finishApprovalAfterCommit runs the post-commit work for a request that just
// committed. A failure never undoes the approval; the stamp stays empty and
// the worker retries (sweepDecisionActionPostCommit).
func (handler *Handler) finishApprovalAfterCommit(ctx context.Context, action db.DecisionAction) {
	if err := handler.completeDecisionActionPostCommit(context.WithoutCancel(ctx), action); err != nil {
		slog.Warn("decision action: post-commit work failed", "action_id", uuidToString(action.ID), "error", err)
	}
}

// completeDecisionActionPostCommit broadcasts the change, wakes the delivery
// worker and stamps the row. It is idempotent: a repeat only re-sends events.
func (handler *Handler) completeDecisionActionPostCommit(ctx context.Context, action db.DecisionAction) error {
	if err := handler.publishDecisionActionCardUpdated(ctx, action); err != nil {
		return err
	}
	if action.State == "executed" {
		if err := handler.publishDecisionActionExecuted(ctx, action); err != nil {
			return err
		}
		if err := handler.resumeRefusalsAfterBudgetChange(ctx, action); err != nil {
			return err
		}
	}
	handler.DecisionWorker.Notify()
	return handler.Queries.MarkDecisionActionPostCommitDone(ctx, db.MarkDecisionActionPostCommitDoneParams{ID: action.ID, WorkspaceID: action.WorkspaceID})
}

// publishDecisionActionCardUpdated tells open clients the action left pending:
// the DM card re-reads its action state on this event, so every client drops
// the approve and reject buttons and shows the outcome. The click recorded a
// card answer in the same transaction as the state change.
func (handler *Handler) publishDecisionActionCardUpdated(ctx context.Context, action db.DecisionAction) error {
	request, err := handler.Queries.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: action.WorkspaceID, ID: action.DecisionRequestID})
	if err != nil {
		return err
	}
	answer, err := handler.Queries.GetDecisionCardAnswer(ctx, action.DecisionRequestID)
	if err != nil {
		return err
	}
	handler.publishDecisionAnswerUpdated(uuidToString(action.WorkspaceID), "member", uuidToString(answer.AnsweredByUserID), answer.ID, request.IssueID, decisionAnswerState(answer.WokenAt, answer.DisclaimedAt))
	return nil
}

// decisionActionTarget is the one existing agent an action reads or changes.
func decisionActionTarget(action db.DecisionAction) (pgtype.UUID, bool) {
	var params struct {
		AgentID       string `json:"agent_id"`
		SourceAgentID string `json:"source_agent_id"`
	}
	if json.Unmarshal(action.Params, &params) != nil {
		return pgtype.UUID{}, false
	}
	var raw string
	switch action.ActionType {
	case "agent.env.patch", agentInstructionsAction, agentSkillsAction, agentPauseAction, agentResumeAction:
		raw = params.AgentID
	case "agent.copy":
		raw = params.SourceAgentID
	}
	id, err := parseUUIDString(raw)
	return id, err == nil
}

// decisionActionRuntime is the runtime an action binds an agent to, or an
// invalid UUID when the action touches none (lock sequence row 2).
func decisionActionRuntime(action db.DecisionAction) pgtype.UUID {
	var params struct {
		RuntimeID string `json:"runtime_id"`
	}
	if action.ActionType != "agent.copy" || json.Unmarshal(action.Params, &params) != nil {
		return pgtype.UUID{}
	}
	id, _ := parseUUIDString(params.RuntimeID)
	return id
}

func agentByID(agents []db.Agent, id pgtype.UUID) (db.Agent, bool) {
	for _, agent := range agents {
		if agent.ID == id {
			return agent, true
		}
	}
	return db.Agent{}, false
}

func distinctUUIDs(ids ...pgtype.UUID) []pgtype.UUID {
	unique := make([]pgtype.UUID, 0, len(ids))
	for _, id := range ids {
		if id.Valid && !slices.Contains(unique, id) {
			unique = append(unique, id)
		}
	}
	return unique
}

// checkPayloadHash proves the stored row still hashes to the stored hash and
// that the human approved that same hash.
func checkPayloadHash(action db.DecisionAction, claimed string) error {
	var slots []decisionaction.SecretSlot
	if err := json.Unmarshal(action.SecretSlots, &slots); err != nil {
		return err
	}
	hash, err := decisionaction.Payload{
		ActionType: action.ActionType, ActionVersion: int(action.ActionVersion),
		WorkspaceID: uuidToString(action.WorkspaceID), DecisionRequestID: uuidToString(action.DecisionRequestID),
		Params: action.Params, Preconditions: action.Preconditions, SecretSlots: slots, ExpiresAt: action.ExpiresAt.Time,
	}.Hash()
	if err != nil {
		return err
	}
	if hash != action.PayloadSha256 || claimed != action.PayloadSha256 {
		return errPayloadMismatch
	}
	return nil
}

// checkPreviewDigest requires, for an action that RequiresPreview, the digest of
// the preview stored in its (hash-covered) preconditions: a client that did not
// show the diff cannot approve.
func checkPreviewDigest(action db.DecisionAction, claimed string) error {
	definition, found := decisionaction.Lookup(action.ActionType, int(action.ActionVersion))
	if !found || !definition.RequiresPreview {
		return nil
	}
	var stored struct {
		PreviewSHA256 string `json:"preview_sha256"`
	}
	if err := json.Unmarshal(action.Preconditions, &stored); err != nil {
		return err
	}
	if stored.PreviewSHA256 == "" || claimed != stored.PreviewSHA256 {
		return errPreviewRequired
	}
	return nil
}

func checkPreconditions(action db.DecisionAction, agent db.Agent) error {
	if _, ok := configChanges[action.ActionType]; ok {
		// The config-change core checks its own digests under the row lock.
		return nil
	}
	if isAgentSchedulingAction(action.ActionType) {
		// Pause and resume are idempotent; there is no state to drift from.
		return nil
	}
	var drifted bool
	if action.ActionType == "agent.copy" {
		var err error
		if drifted, err = copyPreconditionDrifted(action, agent); err != nil {
			return err
		}
	} else {
		var wanted struct {
			CustomEnvVersion int64 `json:"custom_env_version"`
		}
		if err := json.Unmarshal(action.Preconditions, &wanted); err != nil {
			return err
		}
		drifted = agent.CustomEnvVersion != wanted.CustomEnvVersion
	}
	if drifted {
		return errPreconditionDrift
	}
	return nil
}

// checkSecretValues requires a usable value for exactly the declared slots.
func checkSecretValues(action db.DecisionAction, secrets map[string]string) error {
	var slots []decisionaction.SecretSlot
	if err := json.Unmarshal(action.SecretSlots, &slots); err != nil {
		return err
	}
	if len(secrets) != len(slots) {
		return errSecretsMismatch
	}
	for _, slot := range slots {
		if value, ok := secrets[slot.Slot]; !ok || value == "" || value == envSentinel {
			return errSecretsMismatch
		}
	}
	return nil
}

// executeEnvPatchAction is the env.patch core: the same write path as the
// PATCH endpoint, with the secret values filled into their slot keys.
func executeEnvPatchAction(ctx context.Context, execution decisionActionExecution) (json.RawMessage, error) {
	var params decisionaction.EnvPatchParams
	if err := json.Unmarshal(execution.Action.Params, &params); err != nil {
		return nil, err
	}
	if execution.Agent.ArchivedAt.Valid {
		return nil, actionFailure{Code: "agent_archived", Message: "the target agent is archived"}
	}
	set := make(map[string]string, len(params.Set)+len(execution.Secrets))
	for key, value := range params.Set {
		set[key] = value
	}
	for key, value := range execution.Secrets {
		set[key] = value
	}
	updated, _, err := applyAgentEnvPatch(ctx, execution.Queries, execution.Agent, PatchAgentEnvRequest{Set: set, Unset: params.Unset}, execution.Approver)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"agent_id": uuidToString(updated.ID), "custom_env_version": updated.CustomEnvVersion})
}

// finishCardDecision records the human's click: the request is answered by a
// card answer that is scheduled to wake the issue, a system comment states
// the outcome, and an activity row names the actor.
func finishCardDecision(ctx context.Context, queries *db.Queries, caller decisionActionCaller, current db.DecisionRequest, action db.DecisionAction, verb, summary string) error {
	if _, err := queries.MarkDecisionRequestAnswered(ctx, db.MarkDecisionRequestAnsweredParams{WorkspaceID: caller.workspaceID, ID: current.ID}); err != nil {
		return err
	}
	commentID := dbid.NewV7()
	answer, err := queries.CreateDecisionCardAnswer(ctx, db.CreateDecisionCardAnswerParams{
		DecisionRequestID: current.ID, AnsweredByUserID: caller.userID,
		ContentSnapshot: summary, EffectiveAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		IssueCommentID: commentID,
	})
	if err != nil {
		return err
	}
	if _, err := queries.CreateComment(ctx, db.CreateCommentParams{
		ID: commentID, WorkspaceID: caller.workspaceID, IssueID: current.IssueID, ParentID: current.IssueCommentID,
		AuthorType: "system", AuthorID: pgtype.UUID{Valid: true}, Type: "comment", Content: summary,
		DecisionAnswerID: answer.ID, DecisionCommentKind: pgtype.Text{String: "quote", Valid: true},
	}); err != nil {
		return err
	}
	details, err := json.Marshal(map[string]any{"decision_request_id": uuidToString(current.ID), "decision_action_id": uuidToString(action.ID), "action_type": action.ActionType, "payload_sha256": action.PayloadSha256})
	if err != nil {
		return err
	}
	_, err = queries.CreateActivity(ctx, db.CreateActivityParams{
		WorkspaceID: caller.workspaceID, IssueID: current.IssueID, ActorType: pgtype.Text{String: "member", Valid: true},
		ActorID: caller.userID, Action: "decision_action_" + verb, Details: details,
	})
	return err
}

// lockDecisionThreadAndIssue takes rows 5 and 6: the ask comment's thread
// (advisory, try only) and the issue the system comment lands on.
func lockDecisionThreadAndIssue(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, request db.DecisionRequest) error {
	if request.IssueCommentID.Valid {
		root, err := queries.GetThreadRoot(ctx, db.GetThreadRootParams{CommentID: request.IssueCommentID, WorkspaceID: workspaceID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			if locked, err := queries.TryLockDecisionCommentThread(ctx, root.ID); err != nil {
				return err
			} else if !locked {
				return atStep("comment_thread", errBusyRetry)
			}
		}
	}
	if _, err := queries.LockDecisionIssueNowait(ctx, db.LockDecisionIssueNowaitParams{WorkspaceID: workspaceID, ID: request.IssueID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return atStep("issue", err)
	}
	return nil
}

// publishDecisionActionExecuted sends the realtime event of what the action
// changed. It is idempotent.
func (handler *Handler) publishDecisionActionExecuted(ctx context.Context, action db.DecisionAction) error {
	if change, ok := configChanges[action.ActionType]; ok {
		if change.Publish == nil {
			return nil
		}
		return change.Publish(ctx, handler, action)
	}
	if action.ActionType == issueProjectSetType {
		return handler.publishIssueProjectSet(ctx, action)
	}
	if action.ActionType == "agent.copy" {
		return handler.publishAgentCopyCreated(ctx, action)
	}
	if action.ActionType == issueWorkflowSetType {
		return handler.publishIssueWorkflowSet(ctx, action)
	}
	if isAgentSchedulingAction(action.ActionType) {
		return handler.publishAgentSchedulingChanged(ctx, action)
	}
	agentID, ok := decisionActionTarget(action)
	if !ok {
		return nil
	}
	agent, err := handler.Queries.GetAgent(ctx, agentID)
	if err != nil {
		return err
	}
	return handler.publishAgentEnvUpdated(ctx, agent, action.ApprovedByUserID)
}

// checkRefusalBinding applies the run-budget rule to an issue action that names
// a refusal: it must be a refusal of the action's own issue and window. An
// action without params.refusal_id passes.
func checkRefusalBinding(ctx context.Context, queries *db.Queries, params json.RawMessage) error {
	var named struct {
		IssueID   string `json:"issue_id"`
		WindowID  string `json:"window_id"`
		RefusalID string `json:"refusal_id"`
	}
	if json.Unmarshal(params, &named) != nil || named.RefusalID == "" {
		return nil
	}
	refusalID, refusalErr := parseUUIDString(named.RefusalID)
	issueID, issueErr := parseUUIDString(named.IssueID)
	if refusalErr != nil || issueErr != nil {
		return errRefusalMismatch
	}
	err := service.RunBudgetRefusalBinds(ctx, queries, refusalID, issueID, named.WindowID)
	if errors.Is(err, service.ErrRunBudgetRefusalMismatch) {
		return errRefusalMismatch
	}
	return err
}
