package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/askguard"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// wakeupBinding checks the decision request a rule binds to: on this issue, in
// the asked or answered state, and for the filtered member when the rule names
// one. It returns the zero UUID for an unbound rule.
func wakeupBinding(ctx context.Context, q *db.Queries, issue db.Issue, in WakeupInput, filterActor pgtype.UUID) (pgtype.UUID, error) {
	if in.DecisionRequestID == "" {
		return pgtype.UUID{}, nil
	}
	id, err := wakeupUUID(in.DecisionRequestID)
	if err != nil {
		return id, err
	}
	req, err := q.GetWakeupDecisionRequest(ctx, db.GetWakeupDecisionRequestParams{ID: id, IssueID: issue.ID, WorkspaceID: issue.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return id, fmt.Errorf("%w: decision request not found on this issue, or neither asked nor answered", ErrWakeupInput)
	}
	if err != nil {
		return id, err
	}
	if in.FilterActorType == "member" && filterActor.Valid && filterActor != req.HumanUserID {
		return id, fmt.Errorf("%w: the member filter differs from the member the decision request asks", ErrWakeupInput)
	}
	return id, nil
}

// askGuardWakeup is S1: an agent may register a rule a member's action can
// fire only when it is bound to a decision request, declares purpose observe,
// or filters on agent actors. It returns nil when the rule may be saved.
func (s *IssueWakeupService) askGuardWakeup(ctx context.Context, issue db.Issue, in WakeupInput, binding pgtype.UUID) error {
	g := s.AskGuard
	rule := askguard.WakeupRule{Kind: in.Kind, EventTypes: in.EventTypes, FilterActorType: in.FilterActorType, FilterActorID: in.FilterActorID, Condition: hasWakeupCondition(in.Condition)}
	if g == nil || binding.Valid || in.Purpose == askguard.PurposeObserve || !rule.MemberEligible() {
		return nil
	}
	v, err := g.Guard.Hit(ctx, issue.WorkspaceID, askguard.Op{
		IssueID: issue.ID, TaskID: g.TaskID, Kind: "wakeup",
		Payload: append(rule.Payload(), util.UUIDToString(issue.ID)...), WarnOnly: g.WarnOnly,
	})
	if err != nil {
		slog.Warn("ask guard hit failed", "error", err, "kind", "wakeup")
		return ErrWakeupAskGuardUnavailable
	}
	if v.Block {
		return ErrWakeupAskGuardBlocked
	}
	return nil
}

// auditObserveFire records the S1-fire event: a rule declared observe, yet a
// member's action started a run. It is audit only and never refuses.
func auditObserveFire(ctx context.Context, q *db.Queries, w db.IssueWakeup, task db.AgentTaskQueue, receipts []db.IssueWakeupReceipt) error {
	if w.Purpose.String != askguard.PurposeObserve {
		return nil
	}
	var keys []byte
	byMember := false
	for _, r := range receipts {
		if actorType, _, _ := receiptActor(r); actorType == "member" {
			byMember = true
		}
		keys = append(keys, r.EventKey...)
		keys = append(keys, 0)
	}
	if !byMember {
		return nil
	}
	_, err := askguard.Guard{Queries: q}.Hit(ctx, w.WorkspaceID, askguard.Op{
		IssueID: w.IssueID, TaskID: task.ID, Kind: "wakeup_observe_fire", Payload: keys, WarnOnly: true,
	})
	return err
}
