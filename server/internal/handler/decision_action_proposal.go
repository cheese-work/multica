package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ProposalGuard admits one proposal inside its transaction, after the lane is
// resolved and before the request is written. Nil refuses every proposal.
type ProposalGuard func(ctx context.Context, queries *db.Queries, proposal ProposalAdmission) error

// ProposalAdmission is what a guard may judge.
type ProposalAdmission struct {
	Issue  db.Issue
	Agent  db.Agent
	Lane   db.ChatSession
	Reason string
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

func (identity proposalIdentity) fingerprint() string {
	encoded, _ := json.Marshal(identity) // a flat struct of strings and an int
	digest := sha256.Sum256(encoded)
	return "p1:" + hex.EncodeToString(digest[:])
}

// proposalRequest rides on decisionRequestInput. Fingerprint is filled inside
// the transaction, once the lane is known.
type proposalRequest struct {
	ActionType   string
	Version      int
	ParamsSHA256 string
	Reason       string
	Fingerprint  string
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

// laneError reports an unavailable lane of a proposal as ineligible.
func (proposal *proposalRequest) laneError(err error) error {
	var refusal decisionIntentRefusal
	if proposal != nil && errors.As(err, &refusal) && refusal.status == http.StatusForbidden {
		return decisionIntentRefusal{http.StatusConflict, "proposal_ineligible: " + refusal.message}
	}
	return err
}

// replayProposal returns the open request holding this identity, if the
// recipient may still be asked it.
func (handler *Handler) replayProposal(request *http.Request, queries *db.Queries, issue db.Issue, agent db.Agent, input decisionRequestInput, session db.ChatSession) (decisionRequestResult, error) {
	ctx := request.Context()
	fingerprint := input.Proposal.identity(issue, agent.ID, input.HumanUserID, session.ID).fingerprint()
	prior, err := queries.FindOpenProposal(ctx, db.FindOpenProposalParams{WorkspaceID: issue.WorkspaceID, Fingerprint: pgtype.Text{String: fingerprint, Valid: true}})
	if errors.Is(err, pgx.ErrNoRows) {
		return decisionRequestResult{}, nil
	}
	if err != nil {
		return decisionRequestResult{}, err
	}
	err = handler.checkDecisionTarget(request, queries, issue, agent, input.HumanUserID)
	if err == nil {
		err = requireDecisionAuthority(ctx, queries, issue, input.HumanUserID, input.EscalationClass, http.StatusForbidden)
	}
	var refusal decisionIntentRefusal
	if errors.As(err, &refusal) {
		return decisionRequestResult{}, decisionIntentRefusal{http.StatusConflict, "proposal_ineligible: " + refusal.message}
	}
	return decisionRequestResult{Request: prior, Replayed: true}, err
}

// admitProposal fixes the identity of a new proposal and asks the guard.
func (handler *Handler) admitProposal(ctx context.Context, queries *db.Queries, issue db.Issue, agent db.Agent, lane db.ChatSession, input decisionRequestInput) (*proposalRequest, error) {
	if handler.ProposalGuard == nil {
		return nil, errProposalsUnavailable
	}
	proposal := *input.Proposal
	proposal.Fingerprint = proposal.identity(issue, agent.ID, input.HumanUserID, lane.ID).fingerprint()
	return &proposal, handler.ProposalGuard(ctx, queries, ProposalAdmission{Issue: issue, Agent: agent, Lane: lane, Reason: proposal.Reason})
}

var errProposalsUnavailable = decisionIntentRefusal{http.StatusServiceUnavailable, "proposals_unavailable: agent action proposals are not enabled on this server"}

// composeProposalAsk is the ask text of a proposal. The server writes it from
// the validated action; the agent's reason appears only as a labelled quote.
func composeProposalAsk(issueID string, draft *decisionActionDraft, reason string) (string, error) {
	params, err := json.MarshalIndent(draft.Params, "", "  ")
	if err != nil {
		return "", err
	}
	ask := "**Proposed action**\n`" + draft.Type + "` version " + strconv.Itoa(draft.Version) + " on [Issue](mention://issue/" + issueID + ")\n\n**Parameters**\n```json\n" + string(params) + "\n```"
	if reason = strings.TrimSpace(reason); reason != "" {
		ask += "\n\n**Agent's note (not verified)**\n> " + strings.ReplaceAll(strings.ReplaceAll(reason, "\r", ""), "\n", "\n> ")
	}
	return ask, nil
}

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
	}
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 8*maxCommentContentBytes)).Decode(&body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid action proposal")
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
	if _, taskErr := util.ParseUUID(request.Header.Get("X-Task-ID")); agentErr != nil || taskErr != nil {
		writeError(writer, http.StatusForbidden, "action proposal requires this issue's running task")
		return
	}
	recipient, ok := parseUUIDOrBadRequest(writer, body.For, "for")
	if !ok {
		return
	}
	if !recipient.Valid {
		writeError(writer, http.StatusBadRequest, "for: a named person is required")
		return
	}
	draft, err := draftDecisionAction(&decisionActionWire{Type: body.ActionType, Version: body.Version, Params: body.Params})
	if err != nil {
		writeDecisionError(writer, err, "action proposal")
		return
	}
	if recipient, err = handler.resolveDecisionRecipient(request.Context(), issue, recipient); err != nil {
		writeDecisionError(writer, err, "action proposal")
		return
	}
	ask, err := composeProposalAsk(uuidToString(issue.ID), draft, body.Reason)
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
		Proposal: &proposalRequest{ActionType: draft.Type, Version: draft.Version, ParamsSHA256: hex.EncodeToString(paramsHash[:]), Reason: body.Reason},
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
