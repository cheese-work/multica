package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
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
)

var errAutoCloseShape = errors.New(`auto_close must be an object {"mode": "off"|"warn"|"block"}`)

// validateAutoClosePatch checks the auto_close request field: an object holding
// exactly a mode of off, warn or block. The read side is autoCloseMode.
func validateAutoClosePatch(raw json.RawMessage) error {
	var patch map[string]any
	if err := json.Unmarshal(raw, &patch); err != nil || len(patch) != 1 {
		return errAutoCloseShape
	}
	mode, _ := patch["mode"].(string)
	if mode != "off" && mode != "warn" && mode != "block" {
		return errAutoCloseShape
	}
	return nil
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

// closeOrDispatch is the one close check (CHE-7671), read-only. The status-write
// dispatch, the edge transaction and the pr.merged bridge call it before they
// enqueue a run on an auto-close step. Only block mode closes or waits: off and
// warn always dispatch.
func (h *Handler) closeOrDispatch(ctx context.Context, issue db.Issue) closeDecision {
	mode := h.autoCloseMode(ctx, issue.WorkspaceID)
	if mode == "off" {
		return closeDecision{Outcome: closeOutcomeDispatch}
	}
	read, state := h.autoCloseCheck(ctx, issue)
	predicates := strings.Join(read, ",")
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

// closeState is how far the predicates hold.
type closeState int

const (
	closeStateRejected closeState = iota
	closeStateAwaitingMerge
	closeStateHolds
)

// autoCloseCheck evaluates the predicates in order and stops at the first that
// does not hold or cannot be read. It returns the predicates read and held.
// closeStateAwaitingMerge: a clean PASS names the head of an open PR, every
// other predicate holds, and the issue's own watcher and a pr.merged done edge
// will deliver the merge.
func (h *Handler) autoCloseCheck(ctx context.Context, issue db.Issue) (read []string, state closeState) {
	q := h.Queries
	var (
		mergeEdge     bool
		awaitingMerge bool
		ownWatcher    int64
	)
	checks := []struct {
		name string
		hold func() (bool, error)
	}{
		{"done_edge", func() (bool, error) {
			agentDone, onMerge, err := h.doneEdges(ctx, issue)
			mergeEdge = onMerge
			return agentDone || onMerge, err
		}},
		{"has_parent", func() (bool, error) { return issue.ParentIssueID.Valid, nil }},
		{"verdict_pass_current", func() (bool, error) {
			holds, awaiting, err := h.autoCloseVerdictHolds(ctx, issue)
			awaitingMerge = awaiting
			return holds || awaiting, err
		}},
		{"prs_merged_into_default", func() (bool, error) { return h.linkedPRsMergedIntoDefault(ctx, issue) }},
		{"no_open_child", func() (bool, error) { open, err := h.hasOpenChildren(ctx, issue); return !open, err }},
		{"no_open_decision", func() (bool, error) {
			open, err := q.HasOpenDecisionRequestForIssue(ctx, db.HasOpenDecisionRequestForIssueParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID})
			return !open, err
		}},
		{"no_live_wakeup", func() (bool, error) {
			n, err := q.CountIssueWakeupsLiveAtEntry(ctx, db.CountIssueWakeupsLiveAtEntryParams{IssueID: issue.ID, Step: issue.Status})
			ownWatcher = n.Own
			return n.Other == 0, err
		}},
		{"no_cheese_stop", func() (bool, error) { return !hasCheeseStopLine(issue.Description.String), nil }},
		{"parent_not_stopped", func() (bool, error) {
			parent, err := q.GetIssue(ctx, issue.ParentIssueID)
			return err == nil && metadataString(parent.Metadata, deliveryStopKey) != "stopped", err
		}},
		{"post_close_work_none", func() (bool, error) { return metadataString(issue.Metadata, postCloseWorkKey) == "none", nil }},
	}
	for _, c := range checks {
		held, err := c.hold()
		if err != nil {
			slog.Warn("auto close: predicate unreadable; the step's run starts", "predicate", c.name, "err", err, "issue_id", uuidToString(issue.ID))
			return read, closeStateRejected
		}
		if !held {
			return read, closeStateRejected
		}
		if c.name == "verdict_pass_current" && awaitingMerge {
			continue
		}
		read = append(read, c.name)
	}
	if awaitingMerge {
		if mergeEdge && ownWatcher > 0 {
			return read, closeStateAwaitingMerge
		}
		return read, closeStateRejected
	}
	return read, closeStateHolds
}

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

// autoCloseVerdictHolds: the newest verdict row is a PASS without a P0/P1
// finding (a PASS write may carry findings, so the action name is not enough),
// nothing voided it since, and the candidate is the one it reviewed: every
// linked PR merged at the head the PASS names, or, with no PR, no attachment,
// description revision or comment (other than the reviewer's) newer than the
// PASS. awaitingMerge: the same, except that a linked PR is still open or draft
// at the PASS's head. Anything unreadable keeps the handler run.
func (h *Handler) autoCloseVerdictHolds(ctx context.Context, issue db.Issue) (holds, awaitingMerge bool, err error) {
	verdict, err := h.Queries.GetLatestIssueVerdict(ctx, issue.ID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && verdict.Action != activityPassed) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	var details struct {
		Findings string `json:"findings"`
		HeadSHA  string `json:"head_sha"`
	}
	var findings []ReviewFinding
	if err := json.Unmarshal(verdict.Details, &details); err != nil {
		return false, false, err
	}
	if err := json.Unmarshal([]byte(details.Findings), &findings); err != nil {
		return false, false, err
	}
	for _, f := range findings {
		if f.Severity == "P0" || f.Severity == "P1" {
			return false, false, nil
		}
	}
	voided, err := h.Queries.CountIssueVerdictBreaksSince(ctx, db.CountIssueVerdictBreaksSinceParams{IssueID: issue.ID, Since: verdict.CreatedAt})
	if err != nil || voided > 0 {
		return false, false, err
	}
	prs, err := h.Queries.ListIssuePullRequestHeads(ctx, issue.ID)
	if err != nil {
		return false, false, err
	}
	if len(prs) == 0 {
		newer, err := h.Queries.CountIssueTextCandidateChangesSince(ctx, db.CountIssueTextCandidateChangesSinceParams{IssueID: issue.ID, Since: verdict.CreatedAt, ReviewerID: verdict.ActorID})
		return newer == 0, false, err
	}
	for _, pr := range prs {
		if details.HeadSHA == "" || pr.HeadSha != details.HeadSHA {
			return false, false, nil
		}
		switch pr.State {
		case "merged":
		case "open", "draft":
			awaitingMerge = true
		default:
			return false, false, nil
		}
	}
	return !awaitingMerge, awaitingMerge, nil
}

// linkedPRsMergedIntoDefault: on entry to merged, every linked PR merged into
// its repository's default branch. A PR merged into another branch is a stack
// layer, and a delivery that did not carry the base or the default branch is
// unreadable: both keep the handler run. Entry to agent_accepted needs no PR
// merged, so it passes.
func (h *Handler) linkedPRsMergedIntoDefault(ctx context.Context, issue db.Issue) (bool, error) {
	if issue.Status != statusMerged {
		return true, nil
	}
	prs, err := h.Queries.ListIssuePullRequestHeads(ctx, issue.ID)
	if err != nil {
		return false, err
	}
	for _, pr := range prs {
		if pr.State != "merged" || pr.BaseRef == "" || pr.BaseRef != pr.DefaultBranch {
			return false, nil
		}
	}
	return true, nil
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
