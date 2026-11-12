package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
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
}

type decisionActionExecutor func(ctx context.Context, execution decisionActionExecution) (json.RawMessage, error)

// decisionActionExecutors maps an allowlisted action type to its core.
var decisionActionExecutors = map[string]decisionActionExecutor{
	"agent.env.patch":        executeEnvPatchAction,
	"agent.copy":             executeAgentCopyAction,
	"issue.run_budget.raise": executeRunBudgetRaiseAction,
	runBudgetTopUpType:       executeRunBudgetTopUpAction,
}

// decisionActionPermitted applies the rule of the action's own HTTP path to
// the approver's current role, read under the member lock: a passkey proves
// who clicked, not that they may do this.
func decisionActionPermitted(actionType string, target db.Agent, approver db.Member) bool {
	if actionType == "agent.env.patch" {
		return canManageAgentEnv(target, approver)
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

func (handler *Handler) approveDecisionAction(ctx context.Context, caller decisionActionCaller, body approveBody) (db.DecisionAction, error) {
	empty := db.DecisionAction{}
	preview, located, err := handler.locateDecisionAction(ctx, caller)
	if err != nil {
		return empty, err
	}
	if handler.ApprovalVerifier == nil {
		return empty, errApprovalUnavailable
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
		return empty, errNotExecutable
	}
	if err := checkSecretValues(preview, body.Secrets); err != nil {
		return empty, err
	}
	var queueKey string
	change, configured := configChanges[preview.ActionType]
	if configured && change.QueueField != "" && target.Valid {
		queueKey = configChangeLockKey(uuidToString(caller.workspaceID), change.QueueField, uuidToString(target))
	}
	// A top-up opens a run-budget window, so it takes its turn behind any move
	// that holds the issue's boundary, waiting before it holds a connection.
	var boundaryIssue pgtype.UUID
	if preview.ActionType == runBudgetTopUpType {
		boundaryIssue = located.IssueID
		leave, err := handler.enterRunBudgetBoundary(ctx, boundaryIssue)
		if err != nil {
			return empty, errBusyRetry
		}
		defer leave()
	}
	action, err := handler.approveDecisionActionTx(ctx, caller, body, executor, located, target, decisionActionRuntime(preview), queueKey, configured && change.CatalogLock, boundaryIssue)
	var uncertain uncertainCommit
	if errors.As(err, &uncertain) {
		return handler.readbackAfterUncertainCommit(ctx, caller)
	}
	handler.markSuspectAfterRefusal(ctx, err)
	return action, err
}

// approveDecisionActionTx runs the approve lock sequence (plan 3.2, rows 1 to
// 12) and executes the action inside a savepoint.
func (handler *Handler) approveDecisionActionTx(ctx context.Context, caller decisionActionCaller, body approveBody, executor decisionActionExecutor, located db.DecisionRequest, target, runtimeID pgtype.UUID, queueKey string, catalogLock bool, boundaryIssue pgtype.UUID) (db.DecisionAction, error) {
	empty := db.DecisionAction{}
	transaction, err := handler.beginActionTx(ctx, 2*time.Second)
	if err != nil {
		return empty, err
	}
	defer transaction.Rollback(context.WithoutCancel(ctx))
	// Before any other lock, so the wait cannot sit in a lock cycle; the
	// config change's own try-lock later in this transaction is re-entrant.
	// The catalog lock is first of all, the order a workflow save takes.
	if catalogLock {
		if err := handler.Queries.WithTx(transaction).LockIssueStatusCatalog(ctx, caller.workspaceID); err != nil {
			return empty, err
		}
	}
	if queueKey != "" {
		if _, err := transaction.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, queueKey); err != nil {
			return empty, err
		}
	}
	queries := handler.Queries.WithTx(transaction)
	// The boundary lock comes before every row lock, the order a status move
	// uses: it holds the boundary across its status write and its activity, so a
	// top-up that took the issue row first could read the window between them.
	if boundaryIssue.Valid {
		if err := queries.LockIssueRunBudgetBoundary(ctx, boundaryIssue); err != nil {
			return empty, err
		}
	}
	if _, err := queries.LockWorkspaceKeyShareNowait(ctx, caller.workspaceID); err != nil {
		return empty, err
	}
	var runtime db.AgentRuntime
	if runtimeID.Valid {
		runtime, err = queries.LockAgentRuntimeNowait(ctx, db.LockAgentRuntimeNowaitParams{ID: runtimeID, WorkspaceID: caller.workspaceID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return empty, err
		}
	}
	if _, err := queries.LockDecisionSessionNowait(ctx, db.LockDecisionSessionNowaitParams{WorkspaceID: caller.workspaceID, ID: located.ChatSessionID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	agents, err := queries.LockDecisionAgentsNowait(ctx, db.LockDecisionAgentsNowaitParams{WorkspaceID: caller.workspaceID, Ids: distinctUUIDs(located.AskingAgentID, target)})
	if err != nil {
		return empty, err
	}
	if err := lockDecisionThreadAndIssue(ctx, queries, caller.workspaceID, located); err != nil {
		return empty, err
	}
	current, err := queries.LockDecisionRequestNowait(ctx, db.LockDecisionRequestNowaitParams{WorkspaceID: caller.workspaceID, ID: located.ID})
	if err != nil {
		return empty, err
	}
	if _, err := queries.LockDecisionLiveAnswerNowait(ctx, db.LockDecisionLiveAnswerNowaitParams{WorkspaceID: caller.workspaceID, ID: current.ID}); err == nil {
		return empty, errNotPending
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	action, err := queries.LockDecisionActionNowait(ctx, db.LockDecisionActionNowaitParams{ID: caller.actionID, WorkspaceID: caller.workspaceID})
	if err != nil {
		return empty, err
	}
	if current.HumanUserID != caller.userID {
		return empty, errNotTheHuman
	}
	// Every action class: recovery revoked the person's keys and nothing
	// executable is approved until they hold two new ones.
	if err := refuseWhileRecovering(ctx, queries, caller.userID); err != nil {
		return empty, err
	}
	// A server-opened request is addressed to any member (CHE-2318), but an
	// action on it moves real state: it needs owner authority, not the member
	// shortcut.
	if isServerDecision(current) {
		issue, err := queries.GetIssue(ctx, current.IssueID)
		if err != nil {
			return empty, err
		}
		if err := requireDecisionAuthorityIn(ctx, queries, issue, caller.userID, "spend", pgtype.UUID{}, false, http.StatusForbidden); err != nil {
			return empty, err
		}
	}
	targetKind, ownerOnly := actionTargetKind(action), actionOwnerOnly(action)
	// An owner-only action refuses a non-owner before the passkey, whatever the
	// request's class or state: the role read under the member lock below
	// decides again.
	if ownerOnly {
		member, err := queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: caller.userID, WorkspaceID: caller.workspaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return empty, errNotTheHuman
		} else if err != nil {
			return empty, err
		}
		if member.Role != "owner" {
			return empty, errNeedsOwner
		}
	}
	// The lifecycle worker may have recorded the owner cancellation before this
	// approval; an owner-only action still answers with the owner refusal.
	if current.Status == "cancelled" && current.CancelReason.String == decisionClassNeedsOwner && ownerOnly {
		return empty, errNeedsOwner
	}
	if current.Status != "open" || action.State != "pending" {
		return empty, errNotPending
	}
	if expired, err := queries.DecisionActionExpired(ctx, db.DecisionActionExpiredParams{ID: action.ID, WorkspaceID: caller.workspaceID}); err != nil {
		return empty, err
	} else if expired {
		return empty, errActionExpired
	}
	// A committed issue close or asking-agent archive wins over a stale open
	// row: the sweeper that records it runs later, so check it under the locks.
	if reason, err := queries.DecisionLifecycleReason(ctx, db.DecisionLifecycleReasonParams{WorkspaceID: caller.workspaceID, ID: current.ID}); err != nil {
		return empty, err
	} else if reason == decisionClassNeedsOwner && ownerOnly {
		// The sweeper would cancel this request, but an owner-only action answers
		// a non-owner approver with the owner refusal, not a stale row.
		return empty, errNeedsOwner
	} else if reason != "" {
		return empty, errNotPending
	}
	// Row 6 already holds the issue lock to the end of this transaction, so an
	// issue-target executor runs under it once params.issue_id is that issue. A
	// workflow, squad or workspace target is resolved in the request's workspace
	// and locked here (row 9), before the passkey is checked.
	var targetAgent db.Agent
	var workflow db.IssueWorkflow
	var squad db.Squad
	var workspace db.Workspace
	switch targetKind {
	case decisionaction.TargetIssue:
		if err := checkIssueTarget(action.Params, current.IssueID, http.StatusConflict); err != nil {
			return empty, err
		}
		if err := checkRefusalBinding(ctx, queries, action.Params); err != nil {
			return empty, err
		}
	case decisionaction.TargetWorkflow:
		if workflow, err = loadWorkflowTarget(ctx, queries, caller.workspaceID, action.Params, http.StatusConflict, true); err != nil {
			return empty, err
		}
	case decisionaction.TargetSquad:
		if squad, err = loadSquadTarget(ctx, queries, caller.workspaceID, action.Params, http.StatusConflict, true); err != nil {
			return empty, err
		}
	case decisionaction.TargetWorkspace:
		if workspace, err = loadWorkspaceTarget(ctx, queries, caller.workspaceID, action.Params, http.StatusConflict, true); err != nil {
			return empty, err
		}
	}
	if err := checkPayloadHash(action, body.PayloadSHA256); err != nil {
		return empty, err
	}
	if err := checkPreviewDigest(action, body.PreviewSHA256); err != nil {
		return empty, err
	}
	if targetKind == decisionaction.TargetAgent {
		var ok bool
		if targetAgent, ok = agentByID(agents, target); !ok {
			return empty, errActionNotFound
		}
		if err := checkPreconditions(action, targetAgent); err != nil {
			return empty, err
		}
	}
	credential, err := handler.ApprovalVerifier(ctx, queries, ApprovalClaim{
		WorkspaceID: caller.workspaceID, ActionID: action.ID, UserID: caller.userID,
		PayloadSHA256: body.PayloadSHA256, Assertion: body.Assertion,
	})
	if err != nil {
		return empty, err
	}
	approver, err := queries.LockDecisionActionMemberNowait(ctx, db.LockDecisionActionMemberNowaitParams{WorkspaceID: caller.workspaceID, UserID: caller.userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, errNotTheHuman
	} else if err != nil {
		return empty, err
	}
	// The passkey check above can outlast a role change that committed after the
	// pre-lock reads, so the owner rule is applied again to the role read under
	// the member lock: for an owner-only action and for any server-opened
	// request.
	if (ownerOnly || isServerDecision(current)) && approver.Role != "owner" {
		return empty, errNeedsOwner
	}
	if targetKind == decisionaction.TargetAgent && !decisionActionPermitted(action.ActionType, targetAgent, approver) {
		return empty, errNotPermitted
	}
	finish, summary, err := executeInSavepoint(ctx, transaction, queries, executor, decisionActionExecution{
		Queries: queries, Approver: verifiedApprover{UserID: caller.userID, DecisionActionID: action.ID},
		Action: action, Agent: targetAgent, Runtime: runtime, Secrets: body.Secrets, Workflow: workflow, Squad: squad, Workspace: workspace,
		CredentialID: credential, IssueID: current.IssueID, Tx: transaction,
	})
	if err != nil {
		return empty, err
	}
	finish.ID, finish.WorkspaceID, finish.ApprovedByUserID, finish.WebauthnCredentialID = action.ID, caller.workspaceID, caller.userID, credential
	finished, err := queries.FinishDecisionActionRow(ctx, finish)
	if err != nil {
		return empty, err
	}
	if err := finishCardDecision(ctx, queries, caller, current, finished, "approved", "Approved "+finished.ActionType+": "+summary); err != nil {
		return empty, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return empty, uncertainCommit{err}
	}
	return finished, nil
}

// executeInSavepoint runs the executor so a business failure undoes only the
// execution. Any other error aborts the whole transaction.
func executeInSavepoint(ctx context.Context, transaction pgx.Tx, queries *db.Queries, executor decisionActionExecutor, execution decisionActionExecution) (db.FinishDecisionActionRowParams, string, error) {
	finish := db.FinishDecisionActionRowParams{}
	if _, err := transaction.Exec(ctx, "SAVEPOINT action_exec"); err != nil {
		return finish, "", err
	}
	result, err := executor(ctx, execution)
	var failure actionFailure
	switch {
	case errors.As(err, &failure):
		if _, err := transaction.Exec(ctx, "ROLLBACK TO SAVEPOINT action_exec"); err != nil {
			return finish, "", err
		}
		finish.State = "failed"
		finish.ErrorCode = pgtype.Text{String: failure.Code, Valid: true}
		finish.ErrorMessage = pgtype.Text{String: failure.Message, Valid: true}
		return finish, "failed (" + failure.Code + ").", nil
	case err != nil:
		return finish, "", err
	}
	if _, err := transaction.Exec(ctx, "RELEASE SAVEPOINT action_exec"); err != nil {
		return finish, "", err
	}
	finish.State, finish.Result = "executed", result
	return finish, "executed.", nil
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
		handler.notifyRunBudgetResume(ctx, action.ID)
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
	case "agent.env.patch", agentInstructionsAction:
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
				return errBusyRetry
			}
		}
	}
	if _, err := queries.LockDecisionIssueNowait(ctx, db.LockDecisionIssueNowaitParams{WorkspaceID: workspaceID, ID: request.IssueID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
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
	if action.ActionType == "agent.copy" {
		return handler.publishAgentCopyCreated(ctx, action)
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
