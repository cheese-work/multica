package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// Receipts for server status moves (CHE-9267, plan CHE-9122 step 2): a move the
// platform made, not a person or an agent through the issue API, leaves a
// system comment naming it, so the next reader of the issue sees why its status
// changed. The writer sits in RecordStatusChangeWithEvent, the one function
// every recorded move passes through, and uses the move's own queries, so the
// receipt commits with the status or not at all.

// Causes of the edge writers, the kinds edgeMoveCause maps a source to.
const (
	StatusCauseWorkflowEdge       = "workflow_edge"
	StatusCauseCIAdvance          = "ci_advance"
	StatusCauseWatchdog           = "watchdog"
	StatusCauseStepWatchdogBudget = "step_watchdog_budget"
	StatusCauseSystem             = "system"
)

// StatusCauseKinds lists every cause kind a status_changed row carries.
func StatusCauseKinds() []string {
	return []string{
		StatusCauseUIOrAPI, StatusCauseWorkflowRemap, StatusCauseTaskReset, StatusCauseRunBudget,
		StatusCausePRMerged, StatusCauseQoder, StatusCauseDecision, StatusCauseWorkflowEdge,
		StatusCauseCIAdvance, StatusCauseWatchdog, StatusCauseStepWatchdogBudget, StatusCauseSystem,
	}
}

// StatusCauseOwesReceipt reports whether a move of this cause kind leaves a
// receipt: every kind but a person's or an agent's own move through the issue
// API and the administrative workflow remap. A kind nobody registered owes one.
func StatusCauseOwesReceipt(kind string) bool {
	return kind != StatusCauseUIOrAPI && kind != StatusCauseWorkflowRemap
}

const (
	autoMoveReceiptSetting   = "auto_move_receipt"
	activityReceiptWouldPost = "auto_move_receipt_would_post"
	activityReceiptPosted    = "auto_move_receipt_posted"
)

// autoMoveReceiptMode reads workspace settings.auto_move_receipt.mode. Unset is
// "warn", the default. A value or a read that cannot be understood is "off", so
// a bad setting never writes a receipt.
func autoMoveReceiptMode(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID) string {
	workspace, err := q.GetWorkspace(ctx, workspaceID)
	if err != nil {
		slog.Warn("auto move receipt: read workspace settings", "workspace_id", util.UUIDToString(workspaceID), "error", err)
		return "off"
	}
	var settings map[string]json.RawMessage
	if len(workspace.Settings) == 0 || json.Unmarshal(workspace.Settings, &settings) != nil {
		return "off"
	}
	raw, ok := settings[autoMoveReceiptSetting]
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

// recordAutoMoveReceipt writes the receipt of the move row, when its cause owes
// one. warn writes one would-post activity naming the move; block writes one
// system comment and one posted activity naming the move and the comment. The
// comment is written by query: it starts no run and parses no mention. An error
// is the caller's to return, so a failed receipt fails the whole status write.
func recordAutoMoveReceipt(ctx context.Context, qtx *db.Queries, issue db.Issue, from, to string, trigger StatusTrigger, handoff map[string]string, move db.ActivityLog) error {
	if !StatusCauseOwesReceipt(trigger.Cause.Kind) {
		return nil
	}
	mode := autoMoveReceiptMode(ctx, qtx, issue.WorkspaceID)
	if mode == "off" {
		return nil
	}
	details := map[string]any{"move_id": util.UUIDToString(move.ID), "from": from, "to": to, "cause": trigger.Cause.Kind}
	action := activityReceiptWouldPost
	if mode == "block" {
		action = activityReceiptPosted
		commentID := dbid.NewV7()
		if _, err := qtx.CreateMoveReceiptComment(ctx, db.CreateMoveReceiptCommentParams{
			ID:          commentID,
			IssueID:     issue.ID,
			WorkspaceID: issue.WorkspaceID,
			AuthorID:    pgtype.UUID{Valid: true},
			Content:     autoMoveReceiptContent(issue, from, to, trigger, handoff, move),
		}); err != nil {
			return fmt.Errorf("create move receipt comment: %w", err)
		}
		details["comment_id"] = util.UUIDToString(commentID)
	}
	body, err := json.Marshal(details)
	if err != nil {
		return err
	}
	if _, err := qtx.CreateActivity(ctx, db.CreateActivityParams{
		ID:          dbid.NewV7(),
		WorkspaceID: issue.WorkspaceID,
		IssueID:     issue.ID,
		ActorType:   util.StrToText("system"),
		Action:      action,
		Details:     body,
	}); err != nil {
		return fmt.Errorf("record move receipt: %w", err)
	}
	return nil
}

// autoMoveReceiptContent is the receipt's plain text. It carries no mention
// link, so nothing in it can address an agent.
func autoMoveReceiptContent(issue db.Issue, from, to string, trigger StatusTrigger, handoff map[string]string, move db.ActivityLog) string {
	actor := trigger.Cause.By.Type
	if trigger.Cause.By.ID != "" {
		actor += " " + trigger.Cause.By.ID
	}
	lines := []string{
		fmt.Sprintf("Status moved: %s -> %s", from, to),
		"Cause: " + trigger.Cause.Kind,
		"Actor: " + actor,
	}
	if predicates := trigger.Cause.Ref["predicates"]; predicates != "" {
		lines = append(lines, "Predicates: "+predicates)
	}
	if handoff["to_type"] != "" {
		lines = append(lines, fmt.Sprintf("New assignee: %s %s", handoff["to_type"], handoff["to_id"]))
	}
	lines = append(lines,
		fmt.Sprintf("Revision: %d", issue.Revision),
		"Move: "+util.UUIDToString(move.ID),
	)
	return strings.Join(lines, "\n")
}
