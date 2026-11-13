package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"

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

// wakeupGuardPending is saveTx's request to run the S1 guard after it has
// rolled back, so no connection or row lock is held during the guard's writes.
type wakeupGuardPending struct {
	issue db.Issue
	in    WakeupInput
}

func (*wakeupGuardPending) Error() string { return "wakeup ask guard pending" }

// askGuardNeeded is S1: an agent may register a rule a member's action can
// fire only when it is bound to a decision request, declares purpose observe,
// or filters on agent actors. It reports whether the guard must see the rule.
func (s *IssueWakeupService) askGuardNeeded(in WakeupInput, binding pgtype.UUID) bool {
	rule := askguard.WakeupRule{Kind: in.Kind, EventTypes: in.EventTypes, FilterActorType: in.FilterActorType, FilterActorID: in.FilterActorID, Condition: hasWakeupCondition(in.Condition)}
	return s.AskGuard != nil && !binding.Valid && in.Purpose != askguard.PurposeObserve && rule.MemberEligible()
}

// askGuardHit records the rule as a guard event. It returns nil when the rule
// may be saved.
func (s *IssueWakeupService) askGuardHit(ctx context.Context, issue db.Issue, in WakeupInput) error {
	g := s.AskGuard
	v, err := g.Guard.Hit(ctx, issue.WorkspaceID, askguard.Op{
		IssueID: issue.ID, TaskID: g.TaskID, Kind: "wakeup",
		Payload: wakeupOperationPayload(issue, in), WarnOnly: g.WarnOnly,
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

// wakeupOperationPayload is the stable bytes of one request. Two waits that
// differ in any field are two asks and keep their own events; the same request
// retried finds the same event. The guard never reads these bytes, it only
// hashes them.
func wakeupOperationPayload(issue db.Issue, in WakeupInput) []byte {
	in.EventTypes = slices.Sorted(slices.Values(in.EventTypes))
	b, _ := json.Marshal(in)
	return append(b, util.UUIDToString(issue.ID)...)
}

// receiptMemberCause reports whether a member's event is among the inputs of
// the receipt: the merge keeps that mark when a later agent event replaces the
// payload.
func receiptMemberCause(r db.IssueWakeupReceipt) bool {
	var p struct {
		MemberCause *bool `json:"member_cause"`
	}
	_ = json.Unmarshal(r.Payload, &p)
	if p.MemberCause != nil {
		return *p.MemberCause
	}
	actorType, _, _ := receiptActor(r)
	return actorType == "member"
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
		byMember = byMember || receiptMemberCause(r)
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

// refuseTimerOpenPR refuses an agent's time wakeup while the issue has a linked
// open PR and the server can evaluate PR conditions. It reads structure only,
// never the instruction. A draft or merged PR, an issue with no PR, and a
// server with no GitHub snapshot client are all outside what a condition can
// report, so a timer stays allowed there.
func (s *IssueWakeupService) refuseTimerOpenPR(ctx context.Context, tx pgx.Tx, issueID pgtype.UUID, in WakeupInput) error {
	if !s.RefuseTimerOpenPR || !s.Tasks.PRSnapshotsEnabled || hasWakeupCondition(in.Condition) {
		return nil
	}
	if in.Kind != "at" && in.Kind != "every" && in.Kind != "cron" {
		return nil
	}
	var open bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM issue_pull_request ip JOIN github_pull_request p ON p.id=ip.pull_request_id WHERE ip.issue_id=$1 AND p.state='open')`, issueID).Scan(&open); err != nil {
		return err
	}
	if open {
		return ErrWakeupTimerOpenPR
	}
	return nil
}
