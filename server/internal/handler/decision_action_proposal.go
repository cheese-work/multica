package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ProposalGuard admits one proposal inside its transaction, after the lane is
// resolved and before the request is written. Nil refuses every proposal.
type ProposalGuard func(ctx context.Context, queries *db.Queries, proposal ProposalAdmission) error

// ProposalAdmission is what a guard may judge: the resolved parties and the
// validated action, the same fields the fingerprint is made of, plus the
// composed ask and the agent's unverified note.
type ProposalAdmission struct {
	Issue         db.Issue
	Agent         db.Agent
	Lane          db.ChatSession
	RecipientID   pgtype.UUID
	ActionType    string
	ActionVersion int
	ParamsSHA256  string
	Ask           string
	Reason        string
	Fingerprint   string
}

// errProposalRaced means the open-proposal index refused a duplicate that
// another transaction committed first; the caller then replays the winner.
var errProposalRaced = errors.New("proposal raced")

// proposalIdentity is what makes two proposals the same ask. The lane and the
// recipient are the resolved ones, not the caller's words.
type proposalIdentity struct {
	WorkspaceID     string `json:"workspace_id"`
	AgentID         string `json:"agent_id"`
	RecipientUserID string `json:"recipient_user_id"`
	ChatSessionID   string `json:"chat_session_id"`
	IssueID         string `json:"issue_id"`
	ActionType      string `json:"action_type"`
	ActionVersion   int    `json:"action_version"`
	ParamsSHA256    string `json:"params_sha256"`
}

// fingerprint hashes the RFC 8785 form of the identity, so key order and
// spacing never change it.
func (identity proposalIdentity) fingerprint() (string, error) {
	encoded, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	if encoded, err = decisionaction.Canonical(encoded); err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "p1:" + hex.EncodeToString(digest[:]), nil
}

// proposalRequest rides on decisionRequestInput. Fingerprint is filled inside
// the transaction, once the lane is known.
type proposalRequest struct {
	ActionType   string
	Version      int
	ParamsSHA256 string
	Reason       string
	Fingerprint  string
	// RecipientErr is the refusal of the named person, held back so an open
	// proposal for that person is still reported as ineligible, not as a 400.
	RecipientErr error
	// ChatSessionID is the calling chat task's session when the proposal is
	// made from a DM: the card renders there, for the session's creator.
	ChatSessionID pgtype.UUID
}

// chatSession is nil-safe: most requests are not proposals.
func (proposal *proposalRequest) chatSession() pgtype.UUID {
	if proposal == nil {
		return pgtype.UUID{}
	}
	return proposal.ChatSessionID
}

func (proposal *proposalRequest) identity(issue db.Issue, agentID, recipientID, sessionID pgtype.UUID) proposalIdentity {
	return proposalIdentity{uuidToString(issue.WorkspaceID), uuidToString(agentID), uuidToString(recipientID), uuidToString(sessionID), uuidToString(issue.ID), proposal.ActionType, proposal.Version, proposal.ParamsSHA256}
}

// fingerprintText is the column value; nil-safe for requests that are not proposals.
func (proposal *proposalRequest) fingerprintText() pgtype.Text {
	if proposal == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: proposal.Fingerprint, Valid: true}
}

// proposalIneligible reports a refusal of an open proposal's replay as 409.
func proposalIneligible(err error) error {
	var refusal decisionIntentRefusal
	if errors.As(err, &refusal) {
		return decisionIntentRefusal{http.StatusConflict, "proposal_ineligible: " + refusal.message}
	}
	return err
}

// laneError reports an unavailable lane of a proposal as ineligible.
func (proposal *proposalRequest) laneError(err error) error {
	var refusal decisionIntentRefusal
	if proposal != nil && errors.As(err, &refusal) && refusal.status == http.StatusForbidden {
		return proposalIneligible(err)
	}
	return err
}

// lockProposalDM locks the calling chat task's session and proves it is the
// agent's own plain DM: active, not an intro, not a channel's.
func lockProposalDM(ctx context.Context, queries *db.Queries, issue db.Issue, sessionID, agentID pgtype.UUID) (db.ChatSession, error) {
	deny := decisionIntentRefusal{http.StatusForbidden, "decision request window or lane is unavailable"}
	if _, err := queries.LockDecisionSession(ctx, db.LockDecisionSessionParams{WorkspaceID: issue.WorkspaceID, ID: sessionID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.ChatSession{}, deny
		}
		return db.ChatSession{}, err
	}
	session, err := queries.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{ID: sessionID, WorkspaceID: issue.WorkspaceID})
	if err != nil {
		return db.ChatSession{}, err
	}
	if session.AgentID != agentID || session.Status != "active" || session.ChannelOriginAt.Valid || session.IsAgentIntro {
		return db.ChatSession{}, deny
	}
	if _, err := queries.GetChannelChatSessionBindingBySessionAny(ctx, session.ID); err == nil {
		return db.ChatSession{}, deny
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return db.ChatSession{}, err
	}
	return session, nil
}

// findOpenProposal returns the open request holding this identity.
func findOpenProposal(ctx context.Context, queries *db.Queries, issue db.Issue, agentID pgtype.UUID, input decisionRequestInput, session db.ChatSession) (db.DecisionRequest, bool, error) {
	fingerprint, err := input.Proposal.identity(issue, agentID, input.HumanUserID, session.ID).fingerprint()
	if err != nil {
		return db.DecisionRequest{}, false, err
	}
	prior, err := queries.FindOpenProposal(ctx, db.FindOpenProposalParams{WorkspaceID: issue.WorkspaceID, Fingerprint: pgtype.Text{String: fingerprint, Valid: true}})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.DecisionRequest{}, false, nil
	}
	return prior, err == nil, err
}

// errProposalLaneGone is the reason a held proposal can no longer be asked
// again in a new lane.
var errProposalLaneGone = decisionIntentRefusal{http.StatusForbidden, "decision lane is not available"}

// holdsOpenProposal reports whether the agent has an open proposal of this
// action and params on the issue for the named person, in any lane. The lane
// is part of the identity, so a lane that is no longer found still holds it.
// When the named person did not resolve, an open proposal whose recipient is
// no longer an active member counts too: a removed member's ID cannot be
// mapped back to the user.
func holdsOpenProposal(ctx context.Context, queries *db.Queries, issue db.Issue, agentID pgtype.UUID, input decisionRequestInput) (bool, error) {
	open, err := queries.ListOpenProposals(ctx, db.ListOpenProposalsParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, AgentID: agentID})
	if err != nil {
		return false, err
	}
	for _, request := range open {
		fingerprint, err := input.Proposal.identity(issue, agentID, request.HumanUserID, request.ChatSessionID).fingerprint()
		if err != nil {
			return false, err
		}
		if fingerprint != request.ProposalFingerprint.String {
			continue
		}
		if request.HumanUserID == input.HumanUserID {
			return true, nil
		}
		if input.Proposal.RecipientErr == nil {
			continue
		}
		_, err = queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: request.HumanUserID, WorkspaceID: issue.WorkspaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
	}
	return false, nil
}

// replayProposal returns the open request holding this identity, if the
// recipient may still be asked it.
func (handler *Handler) replayProposal(request *http.Request, queries *db.Queries, issue db.Issue, agent db.Agent, input decisionRequestInput, session db.ChatSession) (decisionRequestResult, error) {
	ctx := request.Context()
	prior, found, err := findOpenProposal(ctx, queries, issue, agent.ID, input, session)
	if err != nil || !found {
		return decisionRequestResult{}, err
	}
	err = handler.checkDecisionTarget(request, queries, issue, agent, input.HumanUserID)
	if err == nil {
		err = requireDecisionAuthority(ctx, queries, issue, input.HumanUserID, input.EscalationClass, http.StatusForbidden)
	}
	if err != nil {
		return decisionRequestResult{}, proposalIneligible(err)
	}
	return decisionRequestResult{Request: prior, Replayed: true}, nil
}

// admitProposal fixes the identity of a new proposal and asks the guard.
func (handler *Handler) admitProposal(ctx context.Context, queries *db.Queries, issue db.Issue, agent db.Agent, lane db.ChatSession, input decisionRequestInput) (*proposalRequest, error) {
	if handler.ProposalGuard == nil {
		return nil, errProposalsUnavailable
	}
	proposal := *input.Proposal
	var err error
	if proposal.Fingerprint, err = proposal.identity(issue, agent.ID, input.HumanUserID, lane.ID).fingerprint(); err != nil {
		return nil, err
	}
	return &proposal, handler.ProposalGuard(ctx, queries, ProposalAdmission{
		Issue: issue, Agent: agent, Lane: lane, RecipientID: input.HumanUserID,
		ActionType: proposal.ActionType, ActionVersion: proposal.Version, ParamsSHA256: proposal.ParamsSHA256,
		Ask: input.Ask, Reason: proposal.Reason, Fingerprint: proposal.Fingerprint,
	})
}

var errProposerSetsPasskey = decisionIntentRefusal{http.StatusBadRequest, "requires_passkey_not_proposable: the registry sets whether an action needs a passkey"}

var errProposalsUnavailable = decisionIntentRefusal{http.StatusServiceUnavailable, "proposals_unavailable: agent action proposals are not enabled on this server"}

// ProposeDecisionAction opens a decision request that asks a person to approve
// one allowlisted action. The agent names the action and its parameters; the
// server writes the ask. Nothing runs until the recipient approves.
func (handler *Handler) ProposeDecisionAction(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("X-Actor-Source") != "task_token" {
		writeError(writer, http.StatusForbidden, "action proposal requires this issue's running task")
		return
	}
	if handler.ProposalGuard == nil {
		writeDecisionError(writer, errProposalsUnavailable, "action proposal")
		return
	}
	var body struct {
		ActionType string          `json:"action_type"`
		Version    int             `json:"version"`
		Params     json.RawMessage `json:"params"`
		IssueID    string          `json:"issue_id"`
		For        string          `json:"for"`
		Reason     string          `json:"reason"`
		// The registry sets the passkey flag per action type; a proposer that
		// names it is refused (B v9 S5, CHE-5415).
		RequiresPasskeySnake *json.RawMessage `json:"requires_passkey"`
		RequiresPasskeyCamel *json.RawMessage `json:"requiresPasskey"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 8*maxCommentContentBytes)).Decode(&body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid action proposal")
		return
	}
	if body.RequiresPasskeySnake != nil || body.RequiresPasskeyCamel != nil {
		writeDecisionError(writer, errProposerSetsPasskey, "action proposal")
		return
	}
	issue, ok := handler.loadIssueForUser(writer, request, body.IssueID)
	if !ok {
		return
	}
	if _, ok := handler.requireWorkspaceMember(writer, request, uuidToString(issue.WorkspaceID), "workspace not found"); !ok {
		return
	}
	agentID, agentErr := util.ParseUUID(request.Header.Get("X-Agent-ID"))
	taskID, taskErr := util.ParseUUID(request.Header.Get("X-Task-ID"))
	if agentErr != nil || taskErr != nil {
		writeError(writer, http.StatusForbidden, "action proposal requires this issue's running task")
		return
	}
	var owner pgtype.UUID
	if body.For == "" && isRunBudgetActionType(body.ActionType) {
		owners, err := handler.Queries.ListWorkspaceOwnerUserIDs(request.Context(), issue.WorkspaceID)
		if err != nil {
			writeDecisionError(writer, err, "action proposal")
			return
		}
		if len(owners) > 0 {
			owner = owners[0]
		}
	}
	recipient, chatSession, ok := handler.proposalRecipient(writer, request, issue, taskID, body.For, owner)
	if !ok {
		return
	}
	draft, err := draftDecisionAction(&decisionActionWire{Type: body.ActionType, Version: body.Version, Params: body.Params})
	if err != nil {
		writeDecisionError(writer, err, "action proposal")
		return
	}
	if open, found, err := handler.openRunBudgetProposal(request.Context(), issue, draft); err != nil {
		writeDecisionError(writer, err, "action proposal")
		return
	} else if found {
		handler.writeDecisionResult(writer, request, issue, decisionRequestResult{Request: open, Replayed: true}, nil)
		return
	}
	var recipientErr error
	if resolved, err := handler.resolveDecisionRecipient(request.Context(), issue, recipient); errors.As(err, new(decisionIntentRefusal)) {
		recipientErr = err // judged in the transaction: an open proposal for this person replays as 409
	} else if err != nil {
		writeDecisionError(writer, err, "action proposal")
		return
	} else {
		recipient = resolved
	}
	if len(body.Reason) > maxProposalReasonBytes {
		writeError(writer, http.StatusBadRequest, "reason is too long (1 KiB at most)")
		return
	}
	body.Reason = cleanProposalReason(body.Reason)
	change := handler.proposalChange(request.Context(), issue, agentID, recipient, draft)
	ask, err := composeProposalAsk(uuidToString(issue.ID), draft, change, body.Reason)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "failed to compose proposal")
		return
	}
	if len(ask) > maxCommentContentBytes {
		writeError(writer, http.StatusBadRequest, "reason is too long")
		return
	}
	paramsHash := sha256.Sum256(draft.Params)
	input := decisionRequestInput{
		HumanUserID: recipient, Ask: ask, EscalationClass: actionRequestClass("reversible", draft.Definition.MinClass), Action: draft,
		Proposal: &proposalRequest{ActionType: draft.Type, Version: draft.Version, ParamsSHA256: hex.EncodeToString(paramsHash[:]), Reason: body.Reason, RecipientErr: recipientErr, ChatSessionID: chatSession},
	}
	// A duplicate that committed first makes the second pass replay it.
	var result decisionRequestResult
	for attempt := 0; attempt < 2; attempt++ {
		result, err = retryDecisionTx(request, func() (decisionRequestResult, error) {
			return handler.createSingleCallDecisionTx(request, issue, input, agentID)
		})
		if !errors.Is(err, errProposalRaced) {
			break
		}
	}
	if err != nil {
		writeDecisionError(writer, err, "action proposal")
		return
	}
	handler.writeDecisionResult(writer, request, issue, result, nil)
}

// proposalRecipient names who is asked. An issue task names a person. A chat
// task asks the creator of its own session, and may name only that person.
//
// An issue task that names no one asks fallback when it is valid.
func (handler *Handler) proposalRecipient(writer http.ResponseWriter, request *http.Request, issue db.Issue, taskID pgtype.UUID, named string, fallback pgtype.UUID) (recipient, chatSession pgtype.UUID, ok bool) {
	task, err := handler.Queries.GetAgentTask(request.Context(), taskID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeError(writer, http.StatusInternalServerError, "failed to load task")
		return recipient, chatSession, false
	}
	if err == nil && task.ChatSessionID.Valid {
		session, err := handler.Queries.GetChatSessionInWorkspace(request.Context(), db.GetChatSessionInWorkspaceParams{ID: task.ChatSessionID, WorkspaceID: issue.WorkspaceID})
		if err != nil {
			writeError(writer, http.StatusForbidden, "decision request window or lane is unavailable")
			return recipient, chatSession, false
		}
		if named != "" {
			if recipient, ok = parseUUIDOrBadRequest(writer, named, "for"); !ok {
				return recipient, chatSession, false
			}
			if resolved, err := handler.resolveDecisionRecipient(request.Context(), issue, recipient); err != nil || resolved != session.CreatorID {
				writeError(writer, http.StatusForbidden, "for: a request from a chat goes to the chat's creator only")
				return recipient, chatSession, false
			}
		}
		return session.CreatorID, session.ID, true
	}
	if named == "" && fallback.Valid {
		return fallback, chatSession, true
	}
	if recipient, ok = parseUUIDOrBadRequest(writer, named, "for"); !ok {
		return recipient, chatSession, false
	}
	if !recipient.Valid {
		writeError(writer, http.StatusBadRequest, "for: a named person is required")
		return recipient, chatSession, false
	}
	return recipient, chatSession, true
}
