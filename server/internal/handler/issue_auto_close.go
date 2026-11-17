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

// Auto-close (CHE-6969): a child issue entering agent_accepted with a clean
// PASS and nothing left to do takes the step's done edge itself, so the step's
// agent does not start a run that only closes it.

const (
	statusAgentAccepted    = "agent_accepted"
	postCloseWorkKey       = "post_close_work"
	deliveryStopKey        = "delivery_stop"
	cheeseStopPrefix       = "Blockers: Cheese stop"
	activityAutoClosed     = "auto_closed"
	activityAutoCloseWould = "auto_close_would_close"
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

// autoCloseOnAccept runs for an issue that just entered agent_accepted. It
// returns done = true when block mode closed the issue, so the caller starts no
// run; otherwise it returns a func the caller runs after its own dispatch (warn
// mode: one activity row naming the run that followed), or nil.
func (h *Handler) autoCloseOnAccept(ctx context.Context, issue db.Issue, intentID pgtype.UUID) (done bool, after func(started bool, agentID pgtype.UUID)) {
	mode := h.autoCloseMode(ctx, issue.WorkspaceID)
	if mode == "off" {
		return false, nil
	}
	read, ok := h.autoCloseCheck(ctx, issue)
	if !ok {
		return false, nil
	}
	predicates := strings.Join(read, ",")
	if mode == "warn" {
		return false, func(started bool, agentID pgtype.UUID) {
			runID := ""
			if started {
				if id, err := h.Queries.GetNewestIssueRunByAgent(ctx, db.GetNewestIssueRunByAgentParams{IssueID: issue.ID, AgentID: agentID}); err == nil {
					runID = uuidToString(id)
				}
			}
			h.recordReviewActivity(ctx, issue.ID, "system", "", activityAutoCloseWould, map[string]string{"predicates": predicates, "run_id": runID})
		}
	}
	moved, _, err := h.tryMoveIssueOnEdge(ctx, issue, "done", "auto_close", map[string]any{"predicates": predicates}, false)
	if err != nil || !moved {
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("auto close: move failed; the step's run starts", "err", err, "issue_id", uuidToString(issue.ID))
		}
		return false, nil
	}
	h.recordReviewActivity(ctx, issue.ID, "system", "", activityAutoClosed, map[string]string{"predicates": predicates})
	h.skipHandoffIntent(ctx, intentID, "auto_closed")
	return true, nil
}

// autoCloseCheck evaluates the predicates in order and stops at the first that
// does not hold or cannot be read. It returns the predicates read and held.
func (h *Handler) autoCloseCheck(ctx context.Context, issue db.Issue) (read []string, ok bool) {
	q := h.Queries
	checks := []struct {
		name string
		hold func() (bool, error)
	}{
		{"done_edge", func() (bool, error) { return h.hasDoneEdge(ctx, issue) }},
		{"has_parent", func() (bool, error) { return issue.ParentIssueID.Valid, nil }},
		{"verdict_pass_current", func() (bool, error) { return h.autoCloseVerdictHolds(ctx, issue) }},
		{"no_open_child", func() (bool, error) { open, err := h.hasOpenChildren(ctx, issue); return !open, err }},
		{"no_open_decision", func() (bool, error) {
			open, err := q.HasOpenDecisionRequestForIssue(ctx, db.HasOpenDecisionRequestForIssueParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID})
			return !open, err
		}},
		{"no_live_wakeup", func() (bool, error) {
			n, err := q.CountIssueWakeupsLiveAtEntry(ctx, issue.ID)
			return n == 0, err
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
			return read, false
		}
		if !held {
			return read, false
		}
		read = append(read, c.name)
	}
	return read, true
}

func (h *Handler) hasDoneEdge(ctx context.Context, issue db.Issue) (bool, error) {
	def, err := h.issueWorkflowDef(ctx, issue)
	if err != nil || def == nil {
		return false, err
	}
	for _, e := range def.EdgesOf(statusAgentAccepted) {
		if e.To == "done" && e.Guard == issueworkflow.GuardAgentDone {
			return true, nil
		}
	}
	return false, nil
}

// autoCloseVerdictHolds: the newest verdict row is a PASS without a P0/P1
// finding (a PASS write may carry findings, so the action name is not enough),
// nothing voided it since, and the candidate is the one it reviewed: every
// linked PR merged at the head the PASS names, or, with no PR, no attachment,
// description revision or comment (other than the reviewer's) newer than the
// PASS. Anything unreadable keeps the handler run.
func (h *Handler) autoCloseVerdictHolds(ctx context.Context, issue db.Issue) (bool, error) {
	verdict, err := h.Queries.GetLatestIssueVerdict(ctx, issue.ID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && verdict.Action != activityPassed) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var details struct {
		Findings string `json:"findings"`
		HeadSHA  string `json:"head_sha"`
	}
	var findings []ReviewFinding
	if err := json.Unmarshal(verdict.Details, &details); err != nil {
		return false, err
	}
	if err := json.Unmarshal([]byte(details.Findings), &findings); err != nil {
		return false, err
	}
	for _, f := range findings {
		if f.Severity == "P0" || f.Severity == "P1" {
			return false, nil
		}
	}
	voided, err := h.Queries.CountIssueVerdictBreaksSince(ctx, db.CountIssueVerdictBreaksSinceParams{IssueID: issue.ID, Since: verdict.CreatedAt})
	if err != nil || voided > 0 {
		return false, err
	}
	prs, err := h.Queries.ListIssuePullRequestHeads(ctx, issue.ID)
	if err != nil {
		return false, err
	}
	if len(prs) == 0 {
		newer, err := h.Queries.CountIssueTextCandidateChangesSince(ctx, db.CountIssueTextCandidateChangesSinceParams{IssueID: issue.ID, Since: verdict.CreatedAt, ReviewerID: verdict.ActorID})
		return newer == 0, err
	}
	for _, pr := range prs {
		if details.HeadSHA == "" || pr.State != "merged" || pr.HeadSha != details.HeadSHA {
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
