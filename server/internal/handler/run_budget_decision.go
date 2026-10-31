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

// runBudgetAsker is the agent a budget request is asked as: the workspace's
// built-in agent, else the issue's agent assignee or its squad's leader, else
// the oldest agent. It is only the name on the request; nothing runs as it and
// no recipient needs permission to invoke it. ok is false only when the
// workspace has no agent at all.
func runBudgetAsker(ctx context.Context, q *db.Queries, issue db.Issue) (agent db.Agent, ok bool, err error) {
	agent, err = q.GetAgentBySystemKey(ctx, db.GetAgentBySystemKeyParams{WorkspaceID: issue.WorkspaceID, SystemKey: pgtype.Text{String: service.MikaSystemKey, Valid: true}})
	if err == nil {
		return agent, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return agent, false, err
	}
	id := issue.AssigneeID
	if issue.AssigneeType.String == "squad" && id.Valid {
		squad, err := q.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: id, WorkspaceID: issue.WorkspaceID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return agent, false, err
		}
		id = squad.LeaderID
	} else if issue.AssigneeType.String != "agent" {
		id = pgtype.UUID{}
	}
	if id.Valid {
		agent, err = q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{WorkspaceID: issue.WorkspaceID, ID: id})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return agent, false, err
		}
		if err == nil && !agent.ArchivedAt.Valid && agent.Kind != "system" {
			return agent, true, nil
		}
	}
	agents, err := q.ListAgents(ctx, issue.WorkspaceID)
	if err != nil || len(agents) == 0 {
		return agent, false, err
	}
	return agents[0], true, nil
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

// runBudgetDecisionPlan is what the exhausted notice locks before it writes its
// comment, so its locks follow the order every decision transaction uses:
// workspace, topic lane and session, then the issue the comment locks.
type runBudgetDecisionPlan struct {
	asker db.Agent
	topic decisionTopic
	lanes []runBudgetLane
}

// runBudgetLane is one recipient and their existing topic DM, if any.
type runBudgetLane struct {
	human   pgtype.UUID
	session db.ChatSession
	reuse   bool
}

// planRunBudgetDecisions takes the decision locks for every recipient who is
// still a member. It returns an empty plan when the workspace has no agent to
// ask as; the notice's comment and inbox item still reach the recipients.
func (h *Handler) planRunBudgetDecisions(ctx context.Context, q *db.Queries, issue db.Issue, recipients []pgtype.UUID) (runBudgetDecisionPlan, error) {
	plan := runBudgetDecisionPlan{}
	asker, ok, err := runBudgetAsker(ctx, q, issue)
	if err != nil {
		return plan, err
	}
	if !ok {
		slog.Warn("run budget: no agent to ask the decision as", "issue_id", uuidToString(issue.ID))
		return plan, nil
	}
	if _, err := q.LockWorkspaceForChatSessionCreate(ctx, issue.WorkspaceID); err != nil {
		return plan, err
	}
	if plan.topic, err = resolveDecisionTopic(ctx, q, issue); err != nil {
		return plan, err
	}
	plan.asker = asker
	for _, human := range recipients {
		if _, err := q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: human, WorkspaceID: issue.WorkspaceID}); errors.Is(err, pgx.ErrNoRows) {
			continue
		} else if err != nil {
			return plan, err
		}
		session, reuse, err := topicLane(ctx, q, issue, plan.topic, human, asker.ID)
		if err != nil {
			return plan, err
		}
		plan.lanes = append(plan.lanes, runBudgetLane{human: human, session: session, reuse: reuse})
	}
	return plan, nil
}

// openRunBudgetDecisions opens one decision request per planned recipient inside
// the exhausted notice's transaction, after the notice comment exists. The
// notice's claim row is the idempotency key: only the transaction that inserted
// it gets here, so a concurrent refusal, a retried hook and a re-delivered event
// open nothing more, and the next window's new claim re-arms it. The request is
// asked as an agent but has no task behind it.
func (h *Handler) openRunBudgetDecisions(ctx context.Context, q *db.Queries, issue db.Issue, e service.RunBudgetEvent, notice db.Comment, plan runBudgetDecisionPlan) ([]decisionRequestResult, error) {
	if len(plan.lanes) == 0 {
		return nil, nil
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
	ask.AskedBy = plan.asker.Name
	input := decisionRequestInput{
		Ask: ask.Content(workspace.Slug), Options: ask.Options, AskJSON: askJSON, EscalationClass: "reversible",
		Fingerprint: serverDecisionFingerprint + "run_budget:" + e.Since.UTC().Format(time.RFC3339Nano),
	}
	var results []decisionRequestResult
	for _, lane := range plan.lanes {
		input.HumanUserID = lane.human
		result, err := h.openRunBudgetDecision(ctx, q, issue, plan, lane, input)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
}

// openRunBudgetDecision is createSingleCallDecisionTx's body minus the running
// task: the intent it seals carries the zero task id, and its target is
// consumed by the request it anchors. The recipient needs no permission to
// invoke the asker: nothing is invoked, and a run keeps its own invocation checks.
func (h *Handler) openRunBudgetDecision(ctx context.Context, q *db.Queries, issue db.Issue, plan runBudgetDecisionPlan, lane runBudgetLane, input decisionRequestInput) (decisionRequestResult, error) {
	empty := decisionRequestResult{}
	session := lane.session
	var created *db.ChatSession
	if !lane.reuse {
		steward, err := h.decisionSteward(ctx, q, plan.topic, plan.asker, lane.human)
		if err != nil {
			return empty, err
		}
		if session, err = createDecisionDM(ctx, q, issue, db.ChatSession{AgentID: steward.ID, CreatorID: lane.human, Title: "Decisions: " + plan.topic.Title, DecisionTopicID: plan.topic.ID}); err != nil {
			return empty, err
		}
		created = &session
	}
	commentID := dbid.NewV7()
	intent, err := q.CreateDecisionIntent(ctx, db.CreateDecisionIntentParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, TaskID: pgtype.UUID{Valid: true}, AgentID: plan.asker.ID, CommentID: commentID})
	if err != nil {
		return empty, err
	}
	if _, err := q.CreateDecisionIntentTarget(ctx, db.CreateDecisionIntentTargetParams{DecisionIntentID: intent.ID, HumanUserID: lane.human}); err != nil {
		return empty, err
	}
	result, err := insertDecisionAsk(ctx, q, issue, input, intent, session, created != nil, nil, pgtype.UUID{}, commentID, nil)
	if err != nil {
		return empty, err
	}
	if result.PingItem, err = decisiontx.NotifyPingTx(ctx, q, issue.WorkspaceID, result.Request.ID); err != nil {
		return empty, err
	}
	result.PingRecorded = true
	result.CreatedSession = created
	return result, nil
}

// publishRunBudgetDecisions announces committed server-origin requests the way
// the request endpoint announces an agent's: chat events go to the person who
// owns the DM.
func (h *Handler) publishRunBudgetDecisions(issue db.Issue, results []decisionRequestResult) {
	workspaceID := uuidToString(issue.WorkspaceID)
	for _, result := range results {
		agentID, humanID := uuidToString(result.Request.AskingAgentID), uuidToString(result.Request.HumanUserID)
		sessionID := uuidToString(result.Request.ChatSessionID)
		if session := result.CreatedSession; session != nil {
			h.publishChatFor(protocol.EventChatSessionCreated, workspaceID, "agent", agentID, humanID, sessionID, protocol.ChatSessionCreatedPayload{
				WorkspaceID: workspaceID, ChatSessionID: sessionID, AgentID: uuidToString(session.AgentID), CreatorID: humanID, Title: session.Title,
			})
		}
		if item := result.PingItem; item != nil {
			h.publish(protocol.EventInboxNew, workspaceID, "agent", agentID, map[string]any{"item": inboxToResponse(*item)})
		}
		h.publish(protocol.EventCommentCreated, workspaceID, "agent", agentID, map[string]any{
			"comment": commentToResponse(result.Comment, nil, nil), "issue_title": issue.Title, "issue_status": issue.Status,
			"decision_intent_targets": []string{humanID},
		})
		h.publishChatFor(protocol.EventChatMessage, workspaceID, "agent", agentID, humanID, sessionID, protocol.ChatMessagePayload{
			MessageKind:   result.Message.MessageKind,
			ChatSessionID: sessionID, MessageID: uuidToString(result.Message.ID),
			Role: result.Message.Role, Content: result.Message.Content, CreatedAt: timestampToString(result.Message.CreatedAt),
		})
	}
	if len(results) > 0 {
		h.DecisionWorker.Notify()
	}
}
