package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// maxProposalBatchItems caps one batch call.
const maxProposalBatchItems = 25

// batchItem is one validated proposal of a batch call.
type batchItem struct {
	issue db.Issue
	draft *decisionActionDraft
}

// batchGroup is the items of one action type: they share one card.
type batchGroup struct {
	actionType string
	items      []batchItem
}

// batchProposalResponse is one card: its batch id and one request per item.
type batchProposalResponse struct {
	BatchID    string                    `json:"batch_id"`
	ActionType string                    `json:"action_type"`
	Replayed   bool                      `json:"replayed"`
	Requests   []decisionRequestResponse `json:"requests"`
}

// composeBatchAsk is the text of the shared card. The goal is the agent's
// word and is quoted as such; the item lines are the server's.
func composeBatchAsk(goal, actionType string, items []batchItem) string {
	var ask strings.Builder
	fmt.Fprintf(&ask, "**Proposed batch**\n%d × `%s`\n\n**Goal (agent's words, not verified)**\n> %s\n\n**Items**", len(items), actionType, strings.ReplaceAll(goal, "\n", "\n> "))
	for index, item := range items {
		fmt.Fprintf(&ask, "\n%d. version %d on [Issue](mention://issue/%s)", index+1, item.draft.Version, uuidToString(item.issue.ID))
	}
	return ask.String()
}

// ProposeDecisionActionBatch opens one card for proposals of one action type
// and one goal (CHE-5412). Each item keeps its own request, action and
// fingerprint; the requests share a batch id and the card's chat message.
// Items of another type open their own card. Approving the batch is S2b.
func (handler *Handler) ProposeDecisionActionBatch(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("X-Actor-Source") != "task_token" {
		writeError(writer, http.StatusForbidden, "batch proposal requires the agent's running task")
		return
	}
	if handler.ProposalGuard == nil {
		writeDecisionError(writer, errProposalsUnavailable, "batch proposal")
		return
	}
	var body struct {
		Goal  string `json:"goal"`
		For   string `json:"for"`
		Items []struct {
			ActionType string          `json:"action_type"`
			Version    int             `json:"version"`
			Params     json.RawMessage `json:"params"`
			IssueID    string          `json:"issue_id"`
		} `json:"items"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 8*maxCommentContentBytes)).Decode(&body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid batch proposal")
		return
	}
	goal := strings.TrimSpace(cleanProposalReason(body.Goal))
	switch {
	case goal == "":
		writeError(writer, http.StatusBadRequest, "goal is required")
		return
	case len(body.Goal) > maxProposalReasonBytes:
		writeError(writer, http.StatusBadRequest, "goal is too long (1 KiB at most)")
		return
	case len(body.Items) == 0 || len(body.Items) > maxProposalBatchItems:
		writeError(writer, http.StatusBadRequest, fmt.Sprintf("items: 1 to %d proposals per batch", maxProposalBatchItems))
		return
	}
	agentID, agentErr := util.ParseUUID(request.Header.Get("X-Agent-ID"))
	taskID, taskErr := util.ParseUUID(request.Header.Get("X-Task-ID"))
	if agentErr != nil || taskErr != nil {
		writeError(writer, http.StatusForbidden, "batch proposal requires the agent's running task")
		return
	}
	var groups []*batchGroup
	byType := map[string]*batchGroup{}
	for _, wire := range body.Items {
		issue, ok := handler.loadIssueForUser(writer, request, wire.IssueID)
		if !ok {
			return
		}
		if len(groups) > 0 && issue.WorkspaceID != groups[0].items[0].issue.WorkspaceID {
			writeError(writer, http.StatusBadRequest, "items: every issue must be in one workspace")
			return
		}
		actionWire := &decisionActionWire{Type: wire.ActionType, Version: wire.Version, Params: wire.Params}
		if err := withIssueTarget(actionWire, issue.ID); err != nil {
			writeError(writer, http.StatusBadRequest, "invalid batch proposal")
			return
		}
		draft, err := draftDecisionAction(actionWire)
		if err != nil {
			writeDecisionError(writer, err, "batch proposal")
			return
		}
		group := byType[draft.Type]
		if group == nil {
			group = &batchGroup{actionType: draft.Type}
			byType[draft.Type] = group
			groups = append(groups, group)
		}
		group.items = append(group.items, batchItem{issue, draft})
	}
	anchor := groups[0].items[0].issue
	if _, ok := handler.requireWorkspaceMember(writer, request, uuidToString(anchor.WorkspaceID), "workspace not found"); !ok {
		return
	}
	recipient, chatSession, ok := handler.proposalRecipient(writer, request, anchor, taskID, body.For)
	if !ok {
		return
	}
	if !chatSession.Valid {
		// An issue task is bound to its own issue; a batch spans issues.
		writeError(writer, http.StatusForbidden, "batch proposal requires a chat task")
		return
	}
	var recipientErr error
	if resolved, err := handler.resolveDecisionRecipient(request.Context(), anchor, recipient); errors.As(err, new(decisionIntentRefusal)) {
		recipientErr = err
	} else if err != nil {
		writeDecisionError(writer, err, "batch proposal")
		return
	} else {
		recipient = resolved
	}
	created := false
	responses := make([]batchProposalResponse, 0, len(groups))
	for _, group := range groups {
		response, groupCreated, err := handler.proposeBatchGroup(request, group, goal, recipient, chatSession, recipientErr, agentID)
		if err != nil {
			writeDecisionError(writer, err, "batch proposal")
			return
		}
		created = created || groupCreated
		responses = append(responses, response)
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(writer, status, map[string]any{"batches": responses})
}

// proposeBatchGroup writes one card for the group in one transaction: all of
// its requests or none. A group whose requests all replay an open batch
// returns it; a group that overlaps one only in part is refused.
func (handler *Handler) proposeBatchGroup(request *http.Request, group *batchGroup, goal string, recipient, chatSession pgtype.UUID, recipientErr error, agentID pgtype.UUID) (batchProposalResponse, bool, error) {
	card := &batchCard{ID: dbid.NewV7(), MessageID: dbid.NewV7(), Ask: composeBatchAsk(goal, group.actionType, group.items), Lead: true}
	inputs := make([]decisionRequestInput, len(group.items))
	for index, item := range group.items {
		ask, err := composeProposalAsk(uuidToString(item.issue.ID), item.draft, handler.proposalChange(request.Context(), item.issue, agentID, recipient, item.draft), goal)
		if err != nil {
			return batchProposalResponse{}, false, err
		}
		if len(ask) > maxCommentContentBytes {
			return batchProposalResponse{}, false, decisionIntentRefusal{http.StatusBadRequest, "goal is too long"}
		}
		paramsHash := sha256.Sum256(item.draft.Params)
		class, err := handler.requestClassFor(request.Context(), item.issue, "reversible", item.draft)
		if err != nil {
			return batchProposalResponse{}, false, err
		}
		itemCard := *card
		itemCard.Lead = index == 0
		inputs[index] = decisionRequestInput{
			HumanUserID: recipient, Ask: ask, EscalationClass: class, Action: item.draft,
			Proposal: &proposalRequest{ActionType: item.draft.Type, Version: item.draft.Version, ParamsSHA256: hex.EncodeToString(paramsHash[:]), Reason: goal, RecipientErr: recipientErr, ChatSessionID: chatSession},
			Batch:    &itemCard,
		}
	}
	var results []decisionRequestResult
	var err error
	// A duplicate that committed first makes the second pass replay it.
	for attempt := 0; attempt < 2; attempt++ {
		_, err = retryDecisionTx(request, func() (decisionRequestResult, error) {
			results, err = handler.createBatchTx(request, group, inputs, agentID)
			return decisionRequestResult{}, err
		})
		if !errors.Is(err, errProposalRaced) {
			break
		}
	}
	if err != nil {
		return batchProposalResponse{}, false, err
	}
	response := batchProposalResponse{ActionType: group.actionType, Requests: make([]decisionRequestResponse, len(results))}
	response.Replayed = results[0].Replayed
	for index, result := range results {
		item := group.items[index]
		if result.Replayed {
			action, err := handler.replayedDecisionAction(request.Context(), item.issue, result.Request)
			if err != nil {
				return batchProposalResponse{}, false, err
			}
			response.Requests[index] = decisionRequestResponse{DecisionRequest: result.Request, Action: action}
			continue
		}
		handler.publishDecisionResult(request, item.issue, &results[index])
		var action *decisionActionResponse
		if result.Action != nil {
			action = newDecisionActionResponse(*result.Action)
		}
		response.Requests[index] = decisionRequestResponse{DecisionRequest: results[index].Request, Action: action}
	}
	response.BatchID = uuidToString(response.Requests[0].BatchID)
	return response, !response.Replayed, nil
}

// createBatchTx runs the single-call path for every item of the group in one
// transaction. Replayed items must be the whole of one open batch.
func (handler *Handler) createBatchTx(request *http.Request, group *batchGroup, inputs []decisionRequestInput, agentID pgtype.UUID) ([]decisionRequestResult, error) {
	ctx := request.Context()
	transaction, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer transaction.Rollback(ctx)
	queries := handler.Queries.WithTx(transaction)
	results := make([]decisionRequestResult, len(inputs))
	for index, input := range inputs {
		if results[index], err = handler.singleCallDecisionIn(request, queries, group.items[index].issue, input, agentID); err != nil {
			return nil, err
		}
		replayed := results[index].Replayed
		if replayed != results[0].Replayed || replayed && (!results[index].Request.BatchID.Valid || results[index].Request.BatchID != results[0].Request.BatchID) {
			return nil, decisionIntentRefusal{http.StatusConflict, "proposal_conflict: some items are already open in another proposal; send the same items or none of them"}
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		return nil, err
	}
	handler.DecisionWorker.Notify()
	return results, nil
}
