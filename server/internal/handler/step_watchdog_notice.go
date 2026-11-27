package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Outcomes of a notify ledger row.
const (
	stepNoticeDelivered   = "delivered"
	stepNoticeUnreachable = "notice_unreachable"
)

// deliverOwedStepNotices sends the notice every block_budget, redispatch and
// notify_only ledger row owes to the issue's requested_by (CHE-6116 S3). It
// reads the ledger, not the candidates, so a status change or a crash after the
// action cannot lose a notice. One failing row does not hold back the others.
func (h *Handler) deliverOwedStepNotices(ctx context.Context, limit int32) error {
	if limit < 1 {
		return nil
	}
	var failures error
	var after db.IssueStepWatchdogAction // zero: first page
	for {
		owed, err := h.Queries.ListOwedStepWatchdogNotices(ctx, db.ListOwedStepWatchdogNoticesParams{
			AfterID: after.ID, AfterCreatedAt: after.CreatedAt, RowLimit: limit,
		})
		if err != nil {
			return errors.Join(failures, err)
		}
		for _, action := range owed {
			if err := h.deliverStepNotice(ctx, action); err != nil {
				slog.Warn("step watchdog: notice failed", "err", err, "issue_id", uuidToString(action.IssueID))
				failures = errors.Join(failures, err)
			}
		}
		// A row that stays owed (no usable agent or lane yet) is paged past, so it
		// cannot hold back the rows behind it.
		if int32(len(owed)) < limit || ctx.Err() != nil {
			return errors.Join(failures, ctx.Err())
		}
		after = owed[len(owed)-1]
	}
}

func stepNoticeContent(identifier string, issue db.Issue, action db.IssueStepWatchdogAction, requester pgtype.UUID) string {
	var d struct {
		Status      string `json:"status"`
		InStepSince string `json:"in_step_since"`
		Refusal     string `json:"refusal"`
	}
	_ = json.Unmarshal(action.Details, &d)
	did := map[string]string{
		"block_budget": "A run budget refuses its handler's run, so the watchdog blocked the issue.",
		"redispatch":   "No run was active, so the watchdog started its handler once more.",
		"notify_only":  "A run of its handler cannot start (" + d.Refusal + "), so the watchdog only tells you.",
	}[action.Action]
	age := ""
	if since, err := time.Parse(time.RFC3339, d.InStepSince); err == nil {
		age = fmt.Sprintf(" for %d h", int(time.Since(since).Hours()))
	}
	return fmt.Sprintf("Step watchdog: [%s](mention://issue/%s) sat in `%s`%s with no run. Case: %s. %s\n\n[@requester](mention://member/%s) %s",
		identifier, uuidToString(issue.ID), d.Status, age, action.Action, did, uuidToString(requester), util.OpenIssueFooter(identifier, uuidToString(issue.ID)))
}

// deliverStepNotice writes one notify row and, when the requester can be
// reached, the notice, in one transaction. The unique ledger key makes a second
// sweeper, or a retry after a crash, write nothing. Only a missing issue,
// requester or membership is recorded as notice_unreachable. A notice with no
// usable agent or lane yet stays owed until one is back.
func (h *Handler) deliverStepNotice(ctx context.Context, action db.IssueStepWatchdogAction) error {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := h.Queries.WithTx(tx)
	if _, err := q.LockWorkspaceForChatSessionCreate(ctx, action.WorkspaceID); err != nil {
		return err
	}
	claim := func(id pgtype.UUID, outcome string, details map[string]any) error {
		body, err := json.Marshal(details)
		if err != nil {
			return err
		}
		_, err = q.ClaimStepWatchdogAction(ctx, db.ClaimStepWatchdogActionParams{
			ID: id, WorkspaceID: action.WorkspaceID, IssueID: action.IssueID, OccurrenceKey: action.OccurrenceKey, Action: "notify", Outcome: outcome, Details: body,
		})
		return err
	}
	unreachable := func(reason string) error {
		if err := claim(dbid.NewV7(), stepNoticeUnreachable, map[string]any{"for": action.Action, "reason": reason}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return tx.Commit(ctx)
	}
	issue, err := q.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: action.WorkspaceID, ID: action.IssueID})
	if errors.Is(err, pgx.ErrNoRows) {
		return unreachable("issue_missing")
	}
	if err != nil {
		return err
	}
	requester := issue.RequestedByID
	if !requester.Valid {
		return unreachable("no_requester")
	}
	// ponytail: access is active workspace membership; the repo has no per-issue ACL.
	if _, err := q.LockDecisionMember(ctx, db.LockDecisionMemberParams{WorkspaceID: issue.WorkspaceID, UserID: requester}); errors.Is(err, pgx.ErrNoRows) {
		return unreachable("requester_not_member")
	} else if err != nil {
		return err
	}
	agents, err := h.stepNoticeAgents(ctx, q, issue, action)
	if err != nil {
		return err
	}
	// Nothing usable yet is not final: an agent can come back, so the row stays owed.
	if len(agents) == 0 {
		return deferNotice(action, "no_agent")
	}
	workspace, err := q.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return err
	}
	content := stepNoticeContent(fmt.Sprintf("%s-%d", workspace.IssuePrefix, issue.Number), issue, action, requester)
	link := func(commentID, messageID, sessionID pgtype.UUID) (string, pgtype.UUID, error) {
		id := dbid.NewV7()
		err := claim(id, stepNoticeDelivered, map[string]any{"for": action.Action, "comment_id": uuidToString(commentID), "chat_message_id": uuidToString(messageID)})
		if errors.Is(err, pgx.ErrNoRows) {
			return "", id, errStepWatchdogClaimed
		}
		return "step_watchdog_action_id", id, err
	}
	// A refused lane writes nothing, so the next agent can be tried in the same transaction.
	var sent sentMemberNotice
	var agent db.Agent
	var refusal decisionIntentRefusal
	for _, candidate := range agents {
		sent, err = h.writeMemberNotice(ctx, q, memberNotice{
			Issue: issue, Agent: candidate, MemberID: requester, Content: content,
			Kind: protocol.ChatMessageKindStepWatchdog, AuthorType: "system", AuthorID: pgtype.UUID{Valid: true}, Link: link,
		})
		if errors.As(err, &refusal) {
			continue
		}
		agent = candidate
		break
	}
	switch {
	case errors.Is(err, errStepWatchdogClaimed):
		return nil
	case err != nil && !errors.As(err, &refusal):
		return err
	case !agent.ID.Valid:
		return deferNotice(action, "lane_unavailable")
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	h.publishMemberNotice(issue.WorkspaceID, "agent", agent.ID, requester, sent)
	return nil
}

// stepNoticeAgents lists, in order, the agents that can own the notice's DM
// lane: the handler the watchdog recorded with the action, then the issue's
// current handler. The recorded one comes first so that a later assignee or
// status change cannot lose or redirect an owed notice.
func (h *Handler) stepNoticeAgents(ctx context.Context, q *db.Queries, issue db.Issue, action db.IssueStepWatchdogAction) ([]db.Agent, error) {
	var recorded struct {
		AgentID string `json:"agent_id"`
	}
	_ = json.Unmarshal(action.Details, &recorded)
	ids := []pgtype.UUID{}
	if id, err := util.ParseUUID(recorded.AgentID); err == nil {
		ids = append(ids, id)
	}
	if id := squadLeaderOrAgent(ctx, q, issue.WorkspaceID, issue.AssigneeType.String, issue.AssigneeID); id.Valid {
		ids = append(ids, id)
	}
	var agents []db.Agent
	seen := map[pgtype.UUID]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{WorkspaceID: issue.WorkspaceID, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !agent.ArchivedAt.Valid {
			agents = append(agents, agent)
		}
	}
	return agents, nil
}

// deferNotice leaves a notice that cannot be sent now owed: the caller's
// transaction rolls back, and a later pass tries again. It never expires, since
// an agent outage says nothing about whether the requester can be reached.
func deferNotice(action db.IssueStepWatchdogAction, reason string) error {
	slog.Debug("step watchdog: notice deferred", "reason", reason, "issue_id", uuidToString(action.IssueID))
	return nil
}
