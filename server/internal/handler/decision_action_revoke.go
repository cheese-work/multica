package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Revoke (B v9 S6, CHE-5416) undoes a decided action while it is revocable: it
// was applied, its type registered an inverse, and it is not yet revoked. The
// inverse runs in the revoke transaction, the revoke record (who, when, why)
// commits with it, and the proposing agent is woken with outcome `revoked`.
// Authorization is Approve's: the request's human, the same role rule, and the
// same passkey verifier — with its own assertion, never the approval's.

var (
	errReasonRequired = decisionIntentRefusal{http.StatusBadRequest, "reason_required: say why you are revoking"}
	errReasonTooLong  = decisionIntentRefusal{http.StatusBadRequest, "reason_too_long: keep it to 1024 bytes"}

	errRevokeNoAction      = decisionIntentRefusal{http.StatusConflict, "no_action: this decision did not ask to run an action"}
	errRevokeRejected      = decisionIntentRefusal{http.StatusConflict, "rejected: a rejected action never ran, so there is nothing to revoke"}
	errRevokeExpired       = decisionIntentRefusal{http.StatusConflict, "expired: an expired action never ran, so there is nothing to revoke"}
	errRevokeNotApplied    = decisionIntentRefusal{http.StatusConflict, "not_applied: this action was not applied, so there is nothing to revoke"}
	errRevokeAlreadyDone   = decisionIntentRefusal{http.StatusConflict, "already_revoked: this action was already revoked"}
	errRevokeNoInverse     = decisionIntentRefusal{http.StatusConflict, "no_inverse: this action type cannot be undone"}
	errRevokeTargetChanged = decisionIntentRefusal{http.StatusConflict, "target_changed: the target is no longer in the state this action left it in"}
)

// revokeGuards say, per action type, whether its inverse still restores exactly
// what the approval did. A type with an Inverse needs a guard; the registry test
// pairs them.
var revokeGuards = map[string]func(action db.DecisionAction, target db.Agent) error{
	agentPauseAction: guardPauseRevoke,
}

// guardPauseRevoke allows resuming only an agent this approval paused and nobody
// has touched since: the pause changed the agent, and the pause time is still
// the one the approval recorded.
func guardPauseRevoke(action db.DecisionAction, target db.Agent) error {
	var result struct {
		Changed  bool   `json:"changed"`
		PausedAt string `json:"paused_at"`
	}
	if json.Unmarshal(action.Result, &result) != nil || !result.Changed {
		return errRevokeTargetChanged
	}
	pausedAt, err := time.Parse(time.RFC3339Nano, result.PausedAt)
	if err != nil || !target.SchedulingPausedAt.Valid || !target.SchedulingPausedAt.Time.Equal(pausedAt) {
		return errRevokeTargetChanged
	}
	return nil
}

// revokeRefusal is the state check of a revoke, shared with its challenge. It
// returns the definition of the action's inverse when the action is revocable.
func revokeRefusal(action db.DecisionAction) (*decisionaction.Definition, error) {
	switch action.State {
	case "rejected":
		return nil, errRevokeRejected
	case "expired":
		return nil, errRevokeExpired
	case "executed":
	default:
		return nil, errRevokeNotApplied
	}
	if action.RevokedAt.Valid {
		return nil, errRevokeAlreadyDone
	}
	definition, ok := decisionaction.Lookup(action.ActionType, int(action.ActionVersion))
	if !ok || definition.Inverse == "" {
		return nil, errRevokeNoInverse
	}
	return definition, nil
}

// revocableNow is the readback flag: a revoke sent now would pass every state
// refusal and the type's guard (the passkey and role checks are still ahead).
func (handler *Handler) revocableNow(ctx context.Context, action db.DecisionAction) bool {
	if _, err := revokeRefusal(action); err != nil {
		return false
	}
	if actionTargetKind(action) == decisionaction.TargetNone {
		return true
	}
	guard := revokeGuards[action.ActionType]
	if guard == nil {
		return false
	}
	target, ok := decisionActionTarget(action)
	if !ok {
		return false
	}
	agent, err := handler.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: target, WorkspaceID: action.WorkspaceID})
	return err == nil && guard(action, agent) == nil
}

// revokePayloadHash binds a revoke's passkey challenge to the action and to the
// word "revoke", so the assertion that approved an action cannot revoke it.
func revokePayloadHash(action db.DecisionAction) string {
	sum := sha256.Sum256([]byte("revoke:" + action.PayloadSha256))
	return hex.EncodeToString(sum[:])
}

// locateRevoke resolves the request named by the route and its action, for the
// request's human only.
func (handler *Handler) locateRevoke(ctx context.Context, caller decisionActionCaller) (db.DecisionAction, db.DecisionRequest, error) {
	located, err := handler.Queries.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: caller.workspaceID, ID: caller.actionID})
	if errors.Is(err, pgx.ErrNoRows) || err == nil && located.HumanUserID != caller.userID {
		return db.DecisionAction{}, located, errNotTheHuman
	} else if err != nil {
		return db.DecisionAction{}, located, err
	}
	action, err := handler.Queries.GetDecisionActionByRequest(ctx, db.GetDecisionActionByRequestParams{DecisionRequestID: located.ID, WorkspaceID: caller.workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return action, located, errRevokeNoAction
	}
	return action, located, err
}

type revokeBody struct {
	Reason    string          `json:"reason"`
	Assertion json.RawMessage `json:"assertion"`
}

// RevokeDecisionAction undoes an applied action. {id} is the decision request.
func (handler *Handler) RevokeDecisionAction(writer http.ResponseWriter, request *http.Request) {
	caller, ok := handler.authorizeDecisionActionCaller(writer, request)
	if !ok {
		return
	}
	var body revokeBody
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxApproveBody)).Decode(&body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid request body")
		return
	}
	reason := strings.TrimSpace(body.Reason)
	switch {
	case reason == "":
		writeDecisionActionError(writer, errReasonRequired)
		return
	case len(reason) > maxDecisionNote:
		writeDecisionActionError(writer, errReasonTooLong)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), decisionActionDeadline)
	defer cancel()
	revoked, err := handler.revokeDecisionAction(ctx, caller, reason, body.Assertion)
	if err != nil {
		writeDecisionActionError(writer, err)
		return
	}
	handler.publishRevoked(ctx, caller, revoked)
	writeJSON(writer, http.StatusOK, map[string]any{
		"outcome": "revoked", "wake": revoked.wake, "action_id": uuidToString(revoked.action.ID), "revoked_at": timestampToString(revoked.action.RevokedAt),
	})
}

// revoked is what a committed revoke leaves to publish.
type revokedAction struct {
	action  db.DecisionAction
	inverse string
	comment db.Comment
	queued  db.AgentTaskQueue
	wake    string
}

func (handler *Handler) revokeDecisionAction(ctx context.Context, caller decisionActionCaller, reason string, assertion json.RawMessage) (revokedAction, error) {
	preview, located, err := handler.locateRevoke(ctx, caller)
	if err != nil {
		return revokedAction{}, err
	}
	if handler.ApprovalVerifier == nil {
		return revokedAction{}, errApprovalUnavailable
	}
	if _, err := revokeRefusal(preview); err != nil {
		return revokedAction{}, err
	}
	var revoked revokedAction
	_, err = retryActionTx(ctx, "revoke", caller, located.IssueID, func() (db.DecisionAction, error) {
		var err error
		revoked, err = handler.revokeDecisionActionTx(ctx, caller, preview, reason, assertion)
		return revoked.action, err
	})
	handler.markSuspectAfterRefusal(ctx, err)
	return revoked, err
}

// revokeDecisionActionTx locks what the approve locked for this target, in the
// same order, re-checks every refusal, verifies the proof, then runs the
// inverse and records the revoke in one transaction.
func (handler *Handler) revokeDecisionActionTx(ctx context.Context, caller decisionActionCaller, preview db.DecisionAction, reason string, assertion json.RawMessage) (revokedAction, error) {
	var empty revokedAction
	transaction, err := handler.beginActionTx(ctx, 2*time.Second)
	if err != nil {
		return empty, err
	}
	defer transaction.Rollback(context.WithoutCancel(ctx))
	queries := handler.Queries.WithTx(transaction)
	located, err := queries.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: caller.workspaceID, ID: preview.DecisionRequestID})
	if err != nil {
		return empty, err
	}
	target, _ := decisionActionTarget(preview)
	if _, err := queries.LockWorkspaceKeyShareNowait(ctx, caller.workspaceID); err != nil {
		return empty, atStep("workspace", err)
	}
	agents, err := queries.LockDecisionAgentsNowait(ctx, db.LockDecisionAgentsNowaitParams{WorkspaceID: caller.workspaceID, Ids: distinctUUIDs(located.AskingAgentID, target)})
	if err != nil {
		return empty, atStep("agents", err)
	}
	if err := lockDecisionThreadAndIssue(ctx, queries, caller.workspaceID, located); err != nil {
		return empty, err
	}
	current, err := queries.LockDecisionRequestNowait(ctx, db.LockDecisionRequestNowaitParams{WorkspaceID: caller.workspaceID, ID: located.ID})
	if err != nil {
		return empty, atStep("request", err)
	}
	action, err := queries.LockDecisionActionNowait(ctx, db.LockDecisionActionNowaitParams{ID: preview.ID, WorkspaceID: caller.workspaceID})
	if err != nil {
		return empty, atStep("action", err)
	}
	definition, err := handler.checkRevoke(ctx, queries, caller, current, action, agents, target)
	if err != nil {
		return empty, err
	}
	credential, err := handler.ApprovalVerifier(ctx, queries, ApprovalClaim{
		WorkspaceID: caller.workspaceID, ActionID: action.ID, UserID: caller.userID,
		PayloadSHA256: revokePayloadHash(action), Assertion: assertion,
	})
	if err != nil {
		return empty, err
	}
	approver, err := queries.LockDecisionActionMemberNowait(ctx, db.LockDecisionActionMemberNowaitParams{WorkspaceID: caller.workspaceID, UserID: caller.userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, errNotTheHuman
	} else if err != nil {
		return empty, atStep("member", err)
	}
	targetAgent, _ := agentByID(agents, target)
	if (actionOwnerOnly(action) || isServerDecision(current)) && approver.Role != "owner" {
		return empty, errNeedsOwner
	}
	if actionTargetKind(action) == decisionaction.TargetAgent && !decisionActionPermitted(action.ActionType, targetAgent, approver) {
		return empty, errNotPermitted
	}
	inverse := decisionActionExecutors[definition.Inverse]
	if inverse == nil {
		return empty, errNotExecutable
	}
	finish, err := executeInSavepoint(ctx, transaction, queries, inverse, decisionActionExecution{
		Queries: queries, Approver: verifiedApprover{UserID: caller.userID, DecisionActionID: action.ID},
		Action: action, Agent: targetAgent, CredentialID: credential, IssueID: current.IssueID, Tx: transaction,
	})
	if err != nil {
		return empty, err
	}
	if finish.State != "executed" {
		// The inverse refused (for example the agent was archived since): nothing
		// ran, and the action stays applied and revocable.
		return empty, errRevokeTargetChanged
	}
	revokedRow, err := queries.RevokeDecisionActionRow(ctx, db.RevokeDecisionActionRowParams{
		ID: action.ID, WorkspaceID: caller.workspaceID, RevokedByUserID: caller.userID,
		RevokeReason: pgtype.Text{String: reason, Valid: true}, RevokeWebauthnCredentialID: credential,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, errRevokeAlreadyDone
	} else if err != nil {
		return empty, err
	}
	woken, err := handler.recordRevoke(ctx, transaction, queries, caller, current, revokedRow, definition.Inverse, reason)
	if err != nil {
		return empty, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return empty, uncertainCommit{err}
	}
	woken.action, woken.inverse = revokedRow, definition.Inverse
	return woken, nil
}

// checkRevoke applies, under the locks and before the passkey, every refusal of
// a revoke: Approve's person, recovery and owner rules, then the action's state
// and whether its inverse still restores what the approval did.
func (handler *Handler) checkRevoke(ctx context.Context, queries *db.Queries, caller decisionActionCaller, current db.DecisionRequest, action db.DecisionAction, agents []db.Agent, target pgtype.UUID) (*decisionaction.Definition, error) {
	if current.HumanUserID != caller.userID {
		return nil, errNotTheHuman
	}
	if err := refuseWhileRecovering(ctx, queries, caller.userID); err != nil {
		return nil, err
	}
	if isServerDecision(current) {
		issue, err := queries.GetIssue(ctx, current.IssueID)
		if err != nil {
			return nil, err
		}
		if err := requireDecisionAuthorityIn(ctx, queries, issue, caller.userID, "spend", pgtype.UUID{}, false, http.StatusForbidden); err != nil {
			return nil, err
		}
	}
	definition, err := revokeRefusal(action)
	if err != nil {
		return nil, err
	}
	if actionTargetKind(action) == decisionaction.TargetAgent {
		agent, ok := agentByID(agents, target)
		if !ok {
			return nil, errActionNotFound
		}
		guard := revokeGuards[action.ActionType]
		if guard == nil {
			return nil, errRevokeNoInverse
		}
		if err := guard(action, agent); err != nil {
			return nil, err
		}
	}
	return definition, nil
}

// revokeWakeSummary is the handoff the proposing agent reads: the outcome, who
// and when (ICT), and why.
func revokeWakeSummary(action db.DecisionAction, who string, reason string) string {
	ict := time.FixedZone("ICT", 7*60*60)
	return "Decision outcome: revoked. The person revoked the approved action " + action.ActionType +
		" and its inverse ran; the change is undone.\nRevoked by: " + who +
		"\nRevoked at: " + action.RevokedAt.Time.In(ict).Format("2006-01-02 15:04 ICT") +
		"\nReason: " + reason
}

// recordRevoke writes the activity row and the thread comment, then wakes the
// proposing agent the way a discussion message does.
func (handler *Handler) recordRevoke(ctx context.Context, transaction pgx.Tx, queries *db.Queries, caller decisionActionCaller, current db.DecisionRequest, action db.DecisionAction, inverse, reason string) (revokedAction, error) {
	var empty revokedAction
	details, err := json.Marshal(map[string]any{
		"decision_request_id": uuidToString(current.ID), "decision_action_id": uuidToString(action.ID),
		"action_type": action.ActionType, "inverse": inverse, "reason": reason,
	})
	if err != nil {
		return empty, err
	}
	if _, err := queries.CreateActivity(ctx, db.CreateActivityParams{
		WorkspaceID: caller.workspaceID, IssueID: current.IssueID, ActorType: pgtype.Text{String: "member", Valid: true},
		ActorID: caller.userID, Action: "decision_action_revoked", Details: details,
	}); err != nil {
		return empty, err
	}
	who := uuidToString(caller.userID)
	if user, err := queries.GetUser(ctx, caller.userID); err == nil && user.Name != "" {
		who = user.Name
	}
	issue, err := queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: caller.workspaceID, ID: current.IssueID})
	if err != nil {
		return empty, err
	}
	created, err := queries.CreateComment(ctx, db.CreateCommentParams{
		ID: dbid.NewV7(), WorkspaceID: caller.workspaceID, IssueID: current.IssueID, ParentID: current.IssueCommentID,
		AuthorType: "member", AuthorID: caller.userID, Type: "comment", Content: "Revoked " + action.ActionType + ". Reason: " + reason,
	})
	if err != nil {
		return empty, err
	}
	woken := revokedAction{comment: created.Comment(), wake: "skipped_ineligible"}
	snapshot := decisionWakeSnapshot{issue: issue, request: current}
	if err := handler.loadDecisionRecipient(ctx, queries, &snapshot, current.AskingAgentID); err != nil {
		return empty, err
	}
	mayInvoke, err := handler.personMayInvoke(ctx, queries, current.AskingAgentID, caller.userID, caller.workspaceID)
	if err != nil {
		return empty, err
	}
	if !mayInvoke || !snapshot.recipient.RuntimeID.Valid || snapshot.recipient.ArchivedAt.Valid {
		return woken, nil
	}
	wake, queued, err := handler.dispatchDecisionInputs(ctx, queries, snapshot, woken.comment, []pgtype.UUID{woken.comment.ID}, revokeWakeSummary(action, who, reason))
	if err != nil {
		return empty, err
	}
	if wake != "" {
		woken.wake, woken.queued = wake, queued
	}
	return woken, nil
}

// publishRevoked runs after the commit: a failure here never undoes the revoke.
func (handler *Handler) publishRevoked(ctx context.Context, caller decisionActionCaller, revoked revokedAction) {
	background := context.WithoutCancel(ctx)
	if isAgentSchedulingAction(revoked.inverse) {
		resumed := revoked.action
		resumed.ActionType, resumed.ApprovedByUserID = revoked.inverse, caller.userID
		_ = handler.publishAgentSchedulingChanged(background, resumed)
	}
	handler.publish(protocol.EventCommentCreated, uuidToString(caller.workspaceID), "member", uuidToString(caller.userID), map[string]any{"comment": commentToResponse(revoked.comment, nil, nil)})
	if revoked.queued.ID.Valid {
		handler.TaskService.PublishTaskEnqueued(background, revoked.queued)
	}
}

// ChallengeDecisionRevoke opens the assertion challenge for revoking this
// request's action. It refuses what the revoke refuses, so the person is never
// asked to sign for something that will fail.
func (handler *Handler) ChallengeDecisionRevoke(writer http.ResponseWriter, request *http.Request) {
	caller, ok := handler.authorizeDecisionActionCaller(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), decisionActionDeadline)
	defer cancel()
	action, _, err := handler.locateRevoke(ctx, caller)
	if err == nil && handler.Passkeys == nil {
		err = errApprovalUnavailable
	}
	if err == nil {
		_, err = revokeRefusal(action)
	}
	if err != nil {
		writeDecisionActionError(writer, err)
		return
	}
	issued, err := handler.issueApproval(ctx, caller.userID, action.ID, revokePayloadHash(action))
	if err != nil {
		writeDecisionActionError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, challengeResponse{
		ChallengeID: uuidToString(issued.ChallengeID), Options: issued.Options, ExpiresAt: issued.ExpiresAt,
	})
}
