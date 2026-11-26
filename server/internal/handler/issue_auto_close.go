package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Auto-close (CHE-6969, CHE-6970): a child issue entering agent_accepted or
// merged with a clean PASS and nothing left to do takes the step's done edge
// itself, so the step's agent does not start a run that only closes it.

const (
	statusAgentAccepted      = "agent_accepted"
	statusMerged             = "merged"
	postCloseWorkKey         = "post_close_work"
	deliveryStopKey          = "delivery_stop"
	cheeseStopPrefix         = "Blockers: Cheese stop"
	activityAutoClosed       = "auto_closed"
	activityAutoCloseWould   = "auto_close_would_close"
	activityAutoClosePending = "auto_close_pending_merge"
	activityAutoCloseEnded   = "auto_close_pending_ended"
	activityAutoCloseSkipped = "auto_close_skipped"

	closeModeSourceWorkspace = "workspace"
	closeModeSourceStep      = "step"
)

// validateAutoClosePatch checks the auto_close request field: an object holding
// exactly a mode of off, warn or block. The read side is autoCloseMode.
func validateAutoClosePatch(raw json.RawMessage) error {
	return decisionaction.ValidateModeSetting("auto_close", raw)
}

// autoCloseMode reads workspace settings.auto_close.mode. An unset key is the
// default, warn. A setting that cannot be read or is not off|warn|block is off,
// so a bad value never closes an issue.
func (h *Handler) autoCloseMode(ctx context.Context, workspaceID pgtype.UUID) string {
	workspace, err := h.Queries.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return "off"
	}
	var settings map[string]json.RawMessage
	if json.Unmarshal(workspace.Settings, &settings) != nil {
		return "off"
	}
	raw, set := settings["auto_close"]
	if !set {
		return "warn"
	}
	var setting struct {
		Mode string `json:"mode"`
	}
	if json.Unmarshal(raw, &setting) != nil || (setting.Mode != "warn" && setting.Mode != "block") {
		return "off"
	}
	return setting.Mode
}

// stepAutoClose returns the auto_close setting of the step the issue sits on,
// or nil when the step sets none or the workflow cannot be read.
func (h *Handler) stepAutoClose(ctx context.Context, issue db.Issue) *issueworkflow.AutoClose {
	def, err := h.issueWorkflowDef(ctx, issue)
	if err != nil || def == nil {
		return nil
	}
	if step, ok := def.Step(issue.Status); ok {
		return step.AutoClose
	}
	return nil
}

// autoCloseSettings is the mode and top_level that apply to the issue's step
// (CHE-7672). The workspace mode off is the kill switch; otherwise the step's
// mode wins, and a step with no auto_close follows the workspace.
func (h *Handler) autoCloseSettings(ctx context.Context, issue db.Issue) (mode string, topLevel bool) {
	mode, topLevel, _ = h.autoCloseSettingsSource(ctx, issue)
	return mode, topLevel
}

// autoCloseSettingsSource is autoCloseSettings with where the mode came from:
// "step" when the step's auto_close sets it, otherwise "workspace".
func (h *Handler) autoCloseSettingsSource(ctx context.Context, issue db.Issue) (mode string, topLevel bool, source string) {
	mode = h.autoCloseMode(ctx, issue.WorkspaceID)
	if mode == "off" {
		return mode, false, closeModeSourceWorkspace
	}
	if step := h.stepAutoClose(ctx, issue); step != nil {
		return step.Mode, step.TopLevel, closeModeSourceStep
	}
	return mode, false, closeModeSourceWorkspace
}

// entersAutoCloseStep reports whether entering status can close the issue.
func entersAutoCloseStep(status string) bool {
	return status == statusAgentAccepted || status == statusMerged
}

// closeOutcome is what closeOrDispatch decides for an issue on an auto-close step.
type closeOutcome string

const (
	// closeOutcomeClosed: every predicate holds; the issue takes its done edge and no run starts.
	closeOutcomeClosed closeOutcome = "closed"
	// closeOutcomePendingMerge: only the merge of the PASS's head is missing, and
	// the issue's own workflow_edges watcher waits for it; no run starts.
	closeOutcomePendingMerge closeOutcome = "pending_merge"
	// closeOutcomeDispatch: the step's handler run starts.
	closeOutcomeDispatch closeOutcome = "dispatch"
)

// closeDecision is one close check. WouldClose is set in warn mode when block
// would have closed: the run still starts, and the caller logs the difference.
type closeDecision struct {
	Outcome    closeOutcome
	Predicates string
	WouldClose bool
}

// closeOrDispatch is the one close check (CHE-7671). It changes no issue row; a
// rejected close in warn or block leaves one auto_close_skipped activity with the
// inputs the predicates read (CHE-9266). The status-write dispatch, the edge
// transaction and the pr.merged bridge call it before they enqueue a run on an
// auto-close step. Only block mode closes or waits: off and warn always dispatch.
func (h *Handler) closeOrDispatch(ctx context.Context, issue db.Issue) closeDecision {
	mode, topLevel, source := h.autoCloseSettingsSource(ctx, issue)
	if mode == "off" {
		return closeDecision{Outcome: closeOutcomeDispatch}
	}
	inputs, ev := h.collectCloseInputs(ctx, issue, topLevel, false)
	state := ev.State
	predicates := strings.Join(ev.Read, ",")
	if state == closeStateRejected {
		h.recordCloseSkipped(ctx, issue, mode, source, predicates, inputs, ev)
	}
	switch {
	case state == closeStateHolds && mode == "block":
		return closeDecision{Outcome: closeOutcomeClosed, Predicates: predicates}
	case state == closeStateHolds:
		return closeDecision{Outcome: closeOutcomeDispatch, Predicates: predicates, WouldClose: true}
	case state == closeStateAwaitingMerge && mode == "block":
		return closeDecision{Outcome: closeOutcomePendingMerge, Predicates: predicates}
	}
	return closeDecision{Outcome: closeOutcomeDispatch}
}

// autoCloseOnEntry runs for an issue that just entered agent_accepted or merged. It
// returns done = true when no run should start: block mode closed the issue, or
// it waits for the merge. Otherwise it returns a func the caller runs after its
// own dispatch (warn mode: one activity row naming the run that followed), or nil.
func (h *Handler) autoCloseOnEntry(ctx context.Context, issue db.Issue, intentID pgtype.UUID) (done bool, after func(started bool, agentID pgtype.UUID)) {
	d := h.closeOrDispatch(ctx, issue)
	switch {
	case d.Outcome == closeOutcomePendingMerge:
		h.recordReviewActivity(ctx, issue.ID, "system", "", activityAutoClosePending, map[string]string{"predicates": d.Predicates})
		h.skipHandoffIntent(ctx, intentID, "pending_merge")
		return true, nil
	case d.Outcome == closeOutcomeClosed:
		if !h.closeIssue(ctx, issue, d.Predicates) {
			return false, nil
		}
		h.skipHandoffIntent(ctx, intentID, "auto_closed")
		return true, nil
	case d.WouldClose:
		return false, func(started bool, agentID pgtype.UUID) {
			runID := ""
			if started {
				if id, err := h.Queries.GetNewestIssueRunByAgent(ctx, db.GetNewestIssueRunByAgentParams{IssueID: issue.ID, AgentID: agentID}); err == nil {
					runID = uuidToString(id)
				}
			}
			h.recordReviewActivity(ctx, issue.ID, "system", "", activityAutoCloseWould, map[string]string{"predicates": d.Predicates, "run_id": runID})
		}
	}
	return false, nil
}

// pendingMergeLive reports whether the issue entered its step without a run to
// wait for the merge: the newest of its pending, pending-ended and status rows
// is the pending one.
func (h *Handler) pendingMergeLive(ctx context.Context, issueID pgtype.UUID) bool {
	rows, err := h.Queries.ListActivitiesForIssue(ctx, db.ListActivitiesForIssueParams{IssueID: issueID, Limit: 200})
	if err != nil {
		return false
	}
	for i := len(rows) - 1; i >= 0; i-- {
		switch rows[i].Action {
		case activityAutoClosePending:
			return true
		case activityAutoCloseEnded, "status_changed":
			return false
		}
	}
	return false
}

// settlePendingMerge runs when a pr.merged edge to done fires for an issue that
// is waiting for its merge without a run (CHE-7671). The predicates are read
// again: a new head without a PASS, a new live wakeup or any other broken
// predicate dispatches the step's handler instead of closing, and the watcher
// is dropped because that handler now owns the step. It reports whether it
// handled the edge; false leaves the edge to its old path.
func (h *Handler) settlePendingMerge(ctx context.Context, issue db.Issue, watcherID pgtype.UUID) (bool, error) {
	if !entersAutoCloseStep(issue.Status) || !h.pendingMergeLive(ctx, issue.ID) {
		return false, nil
	}
	d := h.closeOrDispatch(ctx, issue)
	switch {
	case d.Outcome == closeOutcomePendingMerge:
		return true, nil
	case d.Outcome == closeOutcomeClosed && h.closeIssue(ctx, issue, d.Predicates):
		return true, nil
	}
	started := h.dispatchIssueRun(ctx, issue, service.IssueRunTrigger{
		IssueID: issue.ID, AgentID: issue.AssigneeID, AssigneeType: issue.AssigneeType.String,
	}, "system", "", "")
	if !started {
		return true, nil // the pending state stays: the next look tries again
	}
	h.recordReviewActivity(ctx, issue.ID, "system", "", activityAutoCloseEnded, map[string]string{"predicates": d.Predicates, "outcome": "dispatch"})
	return true, h.Queries.DeleteWorkflowEdges(ctx, watcherID)
}

// closeIssue takes the issue's done edge in its own transaction and records it.
// A failed move keeps the step's run.
func (h *Handler) closeIssue(ctx context.Context, issue db.Issue, predicates string) bool {
	moved, _, err := h.tryMoveIssueOnEdge(ctx, issue, "done", "auto_close", map[string]any{"predicates": predicates}, false)
	if err != nil || !moved {
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("auto close: move failed; the step's run starts", "err", err, "issue_id", uuidToString(issue.ID))
		}
		return false
	}
	h.recordReviewActivity(ctx, issue.ID, "system", "", activityAutoClosed, map[string]string{"predicates": predicates})
	return true
}

// recordCloseSkipped leaves one activity naming the first failed predicate and
// the inputs every predicate up to it read.
func (h *Handler) recordCloseSkipped(ctx context.Context, issue db.Issue, mode, source, predicates string, inputs closeInputs, ev closeEvaluation) {
	h.recordActivityJSON(ctx, issue.ID, "system", "", activityAutoCloseSkipped, map[string]any{
		"first_failure": ev.FirstFailure,
		"predicates":    predicates,
		"mode":          closeModeView{Value: mode, Source: source},
		"checked_at":    time.Now().UTC().Format(time.RFC3339Nano),
		"inputs":        inputs,
	})
}

// closeState is how far the predicates hold.
type closeState int

const (
	closeStateRejected closeState = iota
	closeStateAwaitingMerge
	closeStateHolds
)

// doneEdges reports which done edges the issue's step has: agent_done, and
// pr.merged (the merge wait that the issue's own watcher carries).
func (h *Handler) doneEdges(ctx context.Context, issue db.Issue) (agentDone, onMerge bool, err error) {
	def, err := h.issueWorkflowDef(ctx, issue)
	if err != nil || def == nil {
		return false, false, err
	}
	for _, e := range def.EdgesOf(issue.Status) {
		if e.To != "done" {
			continue
		}
		if e.Guard == issueworkflow.GuardAgentDone {
			agentDone = true
		}
		if g, err := issueworkflow.ParseGuard(e.Guard); err == nil && g.Kind == issueworkflow.GuardPRMerged {
			onMerge = true
		}
	}
	return agentDone, onMerge, nil
}

func hasCheeseStopLine(description string) bool {
	for _, line := range strings.Split(description, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), cheeseStopPrefix) {
			return true
		}
	}
	return false
}

// metadataString reads one string value of issue metadata; "" when absent.
func metadataString(metadata []byte, key string) string {
	var m map[string]any
	if json.Unmarshal(metadata, &m) != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}
