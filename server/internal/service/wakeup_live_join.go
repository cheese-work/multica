package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The live join (CHE-6992, plan CHE-6971): a condition firing of an agent's own
// wakeup that meets a running run of the same agent on the issue is handed to
// that run as a wakeup item, instead of queuing a repeat run behind it. The
// settlement, hold and fallback of the item live in issue_wakeup_live.go.

// wakeupLiveJoinSetting is the workspace.settings key (off | warn | block).
const wakeupLiveJoinSetting = "wakeup_live_join"

// wakeupLiveJoinMode reads workspace settings.wakeup_live_join.mode. Unset is
// "warn", the default. A value or a read that cannot be understood is "off", so
// a bad setting never joins a firing.
func wakeupLiveJoinMode(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID) string {
	workspace, err := q.GetWorkspace(ctx, workspaceID)
	if err != nil {
		slog.Warn("wakeup live join: read workspace settings", "workspace_id", util.UUIDToString(workspaceID), "error", err)
		return "off"
	}
	var settings map[string]json.RawMessage
	if len(workspace.Settings) == 0 || json.Unmarshal(workspace.Settings, &settings) != nil {
		return "off"
	}
	raw, ok := settings[wakeupLiveJoinSetting]
	if !ok {
		return "warn"
	}
	var setting struct {
		Mode string `json:"mode"`
	}
	if json.Unmarshal(raw, &setting) != nil {
		return "off"
	}
	switch setting.Mode {
	case "", "warn":
		return "warn"
	case "off", "block":
		return setting.Mode
	}
	return "off"
}

// liveJoinRuleEligible is eligibility 3: an agent created the wakeup (it has a
// source task), it is not bound to a decision, it is not a system rule, a
// default rule or a custom-rule instance, it is not paused, and it is not a
// merge_eligibility rule: that candidate is judged again at claim and start
// (CHE-5122) through the run's wakeup_merge context, which an item never
// passes through (CHE-9123).
func liveJoinRuleEligible(w db.IssueWakeup, custom *builtinWakeup) bool {
	return custom == nil && w.SourceTaskID.Valid && !w.DecisionRequestID.Valid &&
		!w.SystemRule.Valid && !w.DefaultRuleKey.Valid && !w.PausedReason.Valid &&
		!isMergeEligibilityCondition(w.Condition)
}

// onlyConditionReceipts is eligibility 2. Time, deadline, manual, comment,
// reaction and task receipts never join.
func onlyConditionReceipts(receipts []db.IssueWakeupReceipt) bool {
	for _, r := range receipts {
		if r.EventType != wakeupConditionEventType {
			return false
		}
	}
	return len(receipts) > 0
}

// liveJoinHost returns the run R a firing may join, or an invalid id: eligibility
// 4 to 7. The target has exactly one run on the issue that is waiting, claimed
// or running, and it is running (so nothing waits and the waiting-run join keeps
// priority), it runs as the person the wakeup's own run would run as, it takes
// wakeup items, it carries no other rule's isolated firing (the waiting-run
// join's mayJoin refuses the same run), and no receipt of the firing was ever
// in an item.
func liveJoinHost(ctx context.Context, q *db.Queries, w db.IssueWakeup, receipts []db.IssueWakeupReceipt) (pgtype.UUID, error) {
	if !w.CreatedBy.Valid {
		return pgtype.UUID{}, nil
	}
	runs, err := q.ListAgentIssueOpenRuns(ctx, db.ListAgentIssueOpenRunsParams{IssueID: w.IssueID, AgentID: w.AgentID})
	if err != nil || len(runs) != 1 {
		return pgtype.UUID{}, err
	}
	r := runs[0]
	if r.Status != "running" || !r.TakesWakeupItems || r.OriginatorUserID != w.CreatedBy {
		return pgtype.UUID{}, nil
	}
	host, err := q.GetAgentTask(ctx, r.ID)
	if err != nil {
		return pgtype.UUID{}, err
	}
	if carrierOfOtherRule(host, w).isolated {
		return pgtype.UUID{}, nil
	}
	items, err := q.ListWakeupDeliveryItems(ctx, w.ID)
	if err != nil {
		return pgtype.UUID{}, err
	}
	taken := map[pgtype.UUID]bool{}
	for _, item := range items {
		for _, id := range item.ReceiptIds {
			taken[id] = true
		}
	}
	for _, rec := range receipts {
		if taken[rec.ID] {
			return pgtype.UUID{}, nil
		}
	}
	return r.ID, nil
}

// liveJoinReason is the rule's last_error while its firing is delivered into R.
func liveJoinReason(host pgtype.UUID) pgtype.Text {
	return pgtype.Text{String: "Delivering into running run " + util.UUIDToString(host) + "; inputs retained", Valid: true}
}

// liveJoinedWakeupNote is what the running run is told about a firing that
// joined it: the form of joinedWakeupNote for a run that is already running.
func liveJoinedWakeupNote(w db.IssueWakeup, receipts []db.IssueWakeupReceipt) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Wakeup %s fired while this run is running, so it did not start a run of its own; handle it in this run. Instruction:\n%s\n", util.UUIDToString(w.ID), w.Instruction)
	writeTriggerFacts(&b, receipts)
	return b.String()
}

// liveJoinOutcome is what dispatch does with a firing at the join point.
type liveJoinOutcome int

const (
	liveJoinNone   liveJoinOutcome = iota // the firing runs as today
	liveJoinJoined                        // the item is written; the pass ends here
)

// tryLiveJoin applies eligibility 1 to 9 just before run creation. In block
// mode it writes the item and reports liveJoinJoined; the caller then commits
// the item with the advanced timers and last_error. In warn mode it notes
// would_join_live and reports liveJoinNone, so the firing runs as today. Any
// read error in the setting runs the firing as today; any other error is the
// caller's.
func tryLiveJoin(ctx context.Context, q *db.Queries, issue db.Issue, w db.IssueWakeup, custom *builtinWakeup, receipts []db.IssueWakeupReceipt, note func(string, map[string]any) error) (pgtype.UUID, liveJoinOutcome, error) {
	if !liveJoinRuleEligible(w, custom) || !onlyConditionReceipts(receipts) {
		return pgtype.UUID{}, liveJoinNone, nil
	}
	mode := wakeupLiveJoinMode(ctx, q, issue.WorkspaceID)
	if mode == "off" {
		return pgtype.UUID{}, liveJoinNone, nil
	}
	host, err := liveJoinHost(ctx, q, w, receipts)
	if err != nil || !host.Valid {
		return pgtype.UUID{}, liveJoinNone, err
	}
	if mode == "warn" {
		slog.Info("wakeup live join (warn): would deliver into the running run", "wakeup_id", util.UUIDToString(w.ID), "task_id", util.UUIDToString(host))
		return host, liveJoinNone, note(wakeupActivityWouldJoinLive, map[string]any{
			"wakeup_id": util.UUIDToString(w.ID), "task_id": util.UUIDToString(host), "receipts": len(receipts),
		})
	}
	ids := make([]pgtype.UUID, 0, len(receipts))
	for _, r := range receipts {
		ids = append(ids, r.ID)
	}
	_, err = q.EnqueueTaskWakeupDelivery(ctx, db.EnqueueTaskWakeupDeliveryParams{
		TaskID: host, WakeupID: w.ID, WakeupRevision: int32(w.Revision), ReceiptIds: ids, Note: liveJoinedWakeupNote(w, receipts),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// R is no longer running, no longer takes items, or a receipt is in an item.
		return pgtype.UUID{}, liveJoinNone, nil
	}
	if err != nil {
		return pgtype.UUID{}, liveJoinNone, err
	}
	return host, liveJoinJoined, nil
}
