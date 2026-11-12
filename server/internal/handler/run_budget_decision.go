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
// the oldest active agent, else the oldest archived one. It is only the name on
// the request; delivery runs it for an answer only when the person can invoke
// it. ok is false only when the workspace has no agent row at all.
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
	// An archived agent is still a name for the request: nothing runs as the
	// asker, and the lifecycle pass keeps a server-origin request whose asker
	// is archived.
	agents, err := q.ListAllAgents(ctx, issue.WorkspaceID)
	if err != nil || len(agents) == 0 {
		return agent, false, err
	}
	for _, candidate := range agents {
		if !candidate.ArchivedAt.Valid {
			return candidate, true, nil
		}
	}
	return agents[0], true, nil
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

// openRunBudgetAsk opens ask for every planned recipient under one fingerprint.
func (h *Handler) openRunBudgetAsk(ctx context.Context, q *db.Queries, issue db.Issue, ask decisiontx.Ask, fingerprint string, plan runBudgetDecisionPlan) ([]decisionRequestResult, error) {
	if len(plan.lanes) == 0 {
		return nil, nil
	}
	workspace, err := q.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return nil, err
	}
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
		Fingerprint: fingerprint,
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

// runBudgetCardHolds reports whether an open top-up card still stands for a
// refusal: the window it names is the issue's current window and still refuses
// a run. A status move or top-up opens a new window and leaves the card behind;
// a card that no longer holds blocks neither a decision ask nor done (CHE-5303).
// ok is false for a request that is not a platform top-up card.
func runBudgetCardHolds(ctx context.Context, q *db.Queries, request db.DecisionRequest) (holds, ok bool, err error) {
	if request.Status != "open" || !strings.HasPrefix(request.RequestFingerprint, serverDecisionFingerprint+"run_budget_action:") {
		return false, false, nil
	}
	action, err := q.GetDecisionActionByRequest(ctx, db.GetDecisionActionByRequestParams{DecisionRequestID: request.ID, WorkspaceID: request.WorkspaceID})
	if err != nil {
		return false, true, err
	}
	var params struct {
		WindowID string `json:"window_id"`
	}
	if err := json.Unmarshal(action.Params, &params); err != nil {
		return false, true, err
	}
	since, err := time.Parse(time.RFC3339Nano, params.WindowID)
	if err != nil {
		return false, true, nil
	}
	holds, err = service.RunBudgetStillRefuses(ctx, q, request.IssueID, since)
	return holds, true, err
}
