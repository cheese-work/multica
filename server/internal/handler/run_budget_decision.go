package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// serverDecisionFingerprint prefixes the request_fingerprint of a decision
// request the server opened itself (CHE-2318). A caller's fingerprint is a hash
// ("v2:" or bare hex), so no API caller can produce or collide with it. Every
// path that lets a caller change a request refuses these.
const serverDecisionFingerprint = "server:"

func isServerDecision(request db.DecisionRequest) bool {
	return strings.HasPrefix(request.RequestFingerprint, serverDecisionFingerprint)
}

// runBudgetAsker is the agent a budget request is asked as: the issue's agent
// assignee, else its squad's leader. ok is false when the issue has none.
func runBudgetAsker(ctx context.Context, q *db.Queries, issue db.Issue) (agent db.Agent, ok bool, err error) {
	id := issue.AssigneeID
	if issue.AssigneeType.String == "squad" && id.Valid {
		squad, err := q.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: id, WorkspaceID: issue.WorkspaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return agent, false, nil
		}
		if err != nil {
			return agent, false, err
		}
		id = squad.LeaderID
	} else if issue.AssigneeType.String != "agent" {
		return agent, false, nil
	}
	agent, err = q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{WorkspaceID: issue.WorkspaceID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return agent, false, nil
	}
	return agent, err == nil && !agent.ArchivedAt.Valid && agent.Kind != "system", err
}

// runBudgetDecisionAsk is the structured ask: reopen the budget by a status
// move or a top-up, or do nothing. ref names the issue in the top-up command.
func runBudgetDecisionAsk(issue db.Issue, ref string, notice db.Comment) decisiontx.Ask {
	return decisiontx.Ask{
		ContextELI10:  "This issue used up its run budget. The platform refused the next run, so nothing derived from this issue runs until a person opens a new run window.",
		AgentDid:      "The platform refused a new run on this issue because its run budget for the current window is spent. No agent is running on it.",
		Decision:      "Do you want to open a new run window for this issue?",
		StakesIfWrong: "If you do nothing, the issue stays stopped. If you reopen it while the work loops, it can spend a full new window of runs and tokens.",
		Options: []decisiontx.AskOption{
			{Label: "Move the status", WhatHappens: "You move the issue to another status on the board. That opens a new run window and the workflow runs again.", Pros: []string{"No command needed"}, Cons: []string{"Runs start at once"}, CostOrTime: "One click, then a new window of runs"},
			{Label: "Top up the window", WhatHappens: fmt.Sprintf("You run `multica issue budget top-up %s`. That opens a new run window and leaves the status where it is.", ref), Pros: []string{"The status stays as it is"}, Cons: []string{"Needs the CLI"}, CostOrTime: "One command, then a new window of runs"},
			{Label: "No action", WhatHappens: "Nothing changes. The issue stays stopped.", Pros: []string{"No new spend"}, Cons: []string{"The work stays stopped"}, CostOrTime: "Free"},
		},
		Recommendation:   "Read the issue's last comments first. Reopen it only when the earlier runs made progress.",
		DefaultIfNoReply: "No action. The issue stays stopped and nothing runs on it.",
		SourceIssueID:    uuidToString(issue.ID),
		SourceCommentID:  uuidToString(notice.ID),
	}
}

// openRunBudgetDecisions opens one decision request per recipient inside the
// exhausted notice's transaction. The notice's claim row is the idempotency
// key: only the transaction that inserted it gets here, so a concurrent
// refusal, a retried hook and a re-delivered event open nothing more, and the
// next window's new claim re-arms it. The request is asked as an agent but has
// no task behind it. A recipient who cannot be asked is skipped, not an error:
// the notice's comment and inbox item already reach them.
func (h *Handler) openRunBudgetDecisions(ctx context.Context, q *db.Queries, issue db.Issue, e service.RunBudgetEvent, notice db.Comment, recipients []pgtype.UUID) ([]decisionRequestResult, error) {
	asker, ok, err := runBudgetAsker(ctx, q, issue)
	if err != nil || !ok {
		return nil, err
	}
	workspace, err := q.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return nil, err
	}
	ref := h.issueBudgetRef(ctx, issue)
	ask := runBudgetDecisionAsk(issue, ref, notice)
	if err := ask.Validate(); err != nil {
		return nil, fmt.Errorf("budget ask: %w", err)
	}
	askJSON, err := json.Marshal(ask)
	if err != nil {
		return nil, err
	}
	ask.AskedBy = asker.Name
	input := decisionRequestInput{
		Ask: ask.Content(workspace.Slug), Options: ask.Options, AskJSON: askJSON, EscalationClass: "reversible",
		Fingerprint: serverDecisionFingerprint + "run_budget:" + e.Since.UTC().Format(time.RFC3339Nano),
	}
	var results []decisionRequestResult
	for _, human := range recipients {
		input.HumanUserID = human
		result, ok, err := h.openRunBudgetDecision(ctx, q, issue, asker, input)
		if err != nil {
			return nil, err
		}
		if !ok {
			slog.Warn("run budget: decision request skipped", "issue_id", uuidToString(issue.ID), "recipient", uuidToString(human))
			continue
		}
		results = append(results, result)
	}
	return results, nil
}

// openRunBudgetDecision is createSingleCallDecisionTx's body minus the running
// task: the intent it seals carries the zero task id, and its target is
// consumed by the request it anchors.
func (h *Handler) openRunBudgetDecision(ctx context.Context, q *db.Queries, issue db.Issue, asker db.Agent, input decisionRequestInput) (decisionRequestResult, bool, error) {
	empty := decisionRequestResult{}
	if _, err := q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: input.HumanUserID, WorkspaceID: issue.WorkspaceID}); errors.Is(err, pgx.ErrNoRows) {
		return empty, false, nil
	} else if err != nil {
		return empty, false, err
	}
	if !h.canInvokeAgent(ctx, asker, "member", uuidToString(input.HumanUserID), "", uuidToString(issue.WorkspaceID)) {
		return empty, false, nil
	}
	if _, err := q.LockWorkspaceForChatSessionCreate(ctx, issue.WorkspaceID); err != nil {
		return empty, false, err
	}
	topic, err := resolveDecisionTopic(ctx, q, issue)
	if err != nil {
		return empty, false, err
	}
	session, reuse, err := topicLane(ctx, q, issue, topic, input.HumanUserID, asker.ID)
	if err != nil {
		return empty, false, err
	}
	var created *db.ChatSession
	if !reuse {
		steward, err := h.decisionSteward(ctx, q, topic, asker, input.HumanUserID)
		if err != nil {
			return empty, false, err
		}
		if session, err = createDecisionDM(ctx, q, issue, db.ChatSession{AgentID: steward.ID, CreatorID: input.HumanUserID, Title: "Decisions: " + topic.Title, DecisionTopicID: topic.ID}); err != nil {
			return empty, false, err
		}
		created = &session
	}
	commentID := dbid.NewV7()
	intent, err := q.CreateDecisionIntent(ctx, db.CreateDecisionIntentParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, TaskID: pgtype.UUID{Valid: true}, AgentID: asker.ID, CommentID: commentID})
	if err != nil {
		return empty, false, err
	}
	if _, err := q.CreateDecisionIntentTarget(ctx, db.CreateDecisionIntentTargetParams{DecisionIntentID: intent.ID, HumanUserID: input.HumanUserID}); err != nil {
		return empty, false, err
	}
	result, err := insertDecisionAsk(ctx, q, issue, input, intent, session, created != nil, nil, pgtype.UUID{}, commentID, nil)
	if err != nil {
		return empty, false, err
	}
	if result.PingItem, err = decisiontx.NotifyPingTx(ctx, q, issue.WorkspaceID, result.Request.ID); err != nil {
		return empty, false, err
	}
	result.PingRecorded = true
	result.CreatedSession = created
	return result, true, nil
}

// publishRunBudgetDecisions announces committed server-origin requests the way
// the request endpoint announces an agent's.
func (h *Handler) publishRunBudgetDecisions(issue db.Issue, results []decisionRequestResult) {
	workspaceID := uuidToString(issue.WorkspaceID)
	for _, result := range results {
		agentID := uuidToString(result.Request.AskingAgentID)
		if session := result.CreatedSession; session != nil {
			h.publishChat(protocol.EventChatSessionCreated, workspaceID, "agent", agentID, uuidToString(session.ID), protocol.ChatSessionCreatedPayload{
				WorkspaceID: workspaceID, ChatSessionID: uuidToString(session.ID), AgentID: agentID, CreatorID: uuidToString(session.CreatorID), Title: session.Title,
			})
		}
		if item := result.PingItem; item != nil {
			h.publish(protocol.EventInboxNew, workspaceID, "agent", agentID, map[string]any{"item": inboxToResponse(*item)})
		}
		h.publish(protocol.EventCommentCreated, workspaceID, "agent", agentID, map[string]any{
			"comment": commentToResponse(result.Comment, nil, nil), "issue_title": issue.Title, "issue_status": issue.Status,
			"decision_intent_targets": []string{uuidToString(result.Request.HumanUserID)},
		})
	}
	if len(results) > 0 {
		h.DecisionWorker.Notify()
	}
}
