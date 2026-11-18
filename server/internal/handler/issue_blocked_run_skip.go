package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Blocked-entry skip (CHE-6973): an issue entering blocked starts no run when
// metadata.blocked_on names a ref and the step owner already has a live
// condition wakeup for that same ref. The platform records the link; when the
// wakeup stops being a live continuation the sweep starts the blocked-step run.

const (
	blockedEntryLinkKey        = "blocked_entry_link"
	activityBlockedEntryLinked = "blocked_entry_linked"
	activityBlockedEntryWould  = "blocked_entry_would_skip"
	activityBlockedEntryRearm  = "blocked_entry_rearmed"
)

// blockedRunSkipMode reads workspace settings.blocked_run_skip.mode. An unset
// key is the default, warn. A setting that cannot be read or is not
// off|warn|block is off, so a bad value never skips a run.
func (h *Handler) blockedRunSkipMode(ctx context.Context, workspaceID pgtype.UUID) string {
	workspace, err := h.Queries.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return "off"
	}
	var settings map[string]json.RawMessage
	if json.Unmarshal(workspace.Settings, &settings) != nil {
		return "off"
	}
	raw, set := settings["blocked_run_skip"]
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

// blockedOnMatches reports whether a wakeup is the continuation a blocked_on
// ref names: the same flag, target and state, stage or kind.
func blockedOnMatches(ref string, w db.IssueWakeup) bool {
	if m := blockedOnDecisionRE.FindStringSubmatch(ref); m != nil {
		return w.DecisionRequestID.Valid && uuidToString(w.DecisionRequestID) == m[1]
	}
	var c service.WakeupCondition
	if len(w.Condition) == 0 || json.Unmarshal(w.Condition, &c) != nil {
		return false
	}
	switch {
	case blockedOnIssueRE.MatchString(ref):
		state := "done"
		if i := strings.IndexByte(ref, '@'); i >= 0 {
			state = ref[i+1:]
		}
		return c.Type == "other_issue" && c.IssueID == blockedOnIssueRE.FindStringSubmatch(ref)[1] && c.State == state
	case blockedOnPRRE.MatchString(ref):
		event := map[string]string{"pr:checks": "checks_finished"}[ref]
		if event == "" {
			event = strings.TrimPrefix(ref, "pr:")
		}
		return c.Type == "pull_request" && c.Event == event
	case blockedOnChildrenRE.MatchString(ref):
		var stage int32
		if i := strings.IndexByte(ref, '@'); i >= 0 {
			n, _ := strconv.Atoi(ref[i+1:])
			stage = int32(n)
		}
		var have int32
		if c.Stage != nil {
			have = *c.Stage
		}
		return c.Type == "children_done" && have == stage && !c.EachStage
	case blockedOnGHRunRE.MatchString(ref):
		i := strings.LastIndexByte(ref, ':')
		id, _ := strconv.ParseInt(ref[i+1:], 10, 64)
		return c.Type == "github_run" && c.RunID == id && c.Workflow == "" &&
			c.Repo == strings.ToLower(strings.TrimPrefix(ref[:i], "gh-run:")) && (c.State == "" || c.State == "completed")
	}
	return false
}

// blockedEntryLink returns the owner's wakeup that links this entry, if any.
// Admission is part of the link: a refused run would not be a continuation.
func (h *Handler) blockedEntryLink(ctx context.Context, issue db.Issue, agentID pgtype.UUID) (db.IssueWakeup, bool, error) {
	ref := metadataString(issue.Metadata, blockedOnMetadataKey)
	if ref == "" || issue.Status != "blocked" || !agentID.Valid {
		return db.IssueWakeup{}, false, nil
	}
	candidates, err := h.Queries.ListBlockedEntryLinkCandidates(ctx, db.ListBlockedEntryLinkCandidatesParams{IssueID: issue.ID, AgentID: agentID})
	if err != nil {
		return db.IssueWakeup{}, false, err
	}
	for _, w := range candidates {
		if blockedOnMatches(ref, w) {
			return w, service.WakeupRunAdmitted(ctx, h.Queries, issue.ID, agentID), nil
		}
	}
	return db.IssueWakeup{}, false, nil
}

// blockedEntryOnDispatch runs for a blocked entry before its run starts. It
// returns skip = true when block mode links the entry to the owner's wakeup, so
// the caller starts no run; otherwise it returns a func the caller runs after
// its own dispatch (warn mode: one activity row naming the run that followed),
// or nil. A read error means the run happens as today.
func (h *Handler) blockedEntryOnDispatch(ctx context.Context, issue db.Issue, agentID pgtype.UUID, intentID pgtype.UUID) (skip bool, after func(started bool)) {
	mode := h.blockedRunSkipMode(ctx, issue.WorkspaceID)
	if mode == "off" {
		return false, nil
	}
	w, linked, err := h.blockedEntryLink(ctx, issue, agentID)
	if err != nil {
		slog.Warn("blocked run skip: link read failed; the run starts", "err", err, "issue_id", uuidToString(issue.ID))
		return false, nil
	}
	if !linked {
		return false, nil
	}
	details := map[string]string{"blocked_on": metadataString(issue.Metadata, blockedOnMetadataKey), "wakeup_id": uuidToString(w.ID)}
	if mode == "warn" {
		return false, func(started bool) {
			details["run_id"] = ""
			if started {
				if id, err := h.Queries.GetNewestIssueRunByAgent(ctx, db.GetNewestIssueRunByAgentParams{IssueID: issue.ID, AgentID: agentID}); err == nil {
					details["run_id"] = uuidToString(id)
				}
			}
			h.recordReviewActivity(ctx, issue.ID, "system", "", activityBlockedEntryWould, details)
		}
	}
	if _, err := setIssueMetadataValue(ctx, h.Queries, issue.WorkspaceID, issue.ID, blockedEntryLinkKey, uuidToString(w.ID)); err != nil {
		slog.Warn("blocked run skip: link not recorded; the run starts", "err", err, "issue_id", uuidToString(issue.ID))
		return false, nil
	}
	h.recordReviewActivity(ctx, issue.ID, "system", "", activityBlockedEntryLinked, details)
	h.skipHandoffIntent(ctx, intentID, "blocked_entry_linked")
	return true, nil
}

// SweepBlockedEntryLinks starts the blocked-step run for every linked entry
// whose wakeup is no longer a live continuation: disabled, deleted, errored,
// expired or refused without a run (the engine disables or errors a refused wakeup), or edited away
// from blocked_on.
func (h *Handler) SweepBlockedEntryLinks(ctx context.Context) error {
	issues, err := h.Queries.ListBlockedEntryLinkedIssues(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, issue := range issues {
		if reason := h.blockedEntryLinkBroken(ctx, issue); reason != "" {
			if err := h.rearmBlockedEntry(ctx, issue, reason); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// blockedEntryLinkBroken returns why the issue's linked wakeup no longer holds
// the continuation, or "" while it does or when that cannot be read.
func (h *Handler) blockedEntryLinkBroken(ctx context.Context, issue db.Issue) string {
	id, err := parseUUIDString(metadataString(issue.Metadata, blockedEntryLinkKey))
	if err != nil {
		return "link unreadable"
	}
	w, err := h.Queries.GetIssueWakeup(ctx, db.GetIssueWakeupParams{ID: id, WorkspaceID: issue.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return "wakeup deleted"
	}
	if err != nil {
		return ""
	}
	if w.Enabled {
		switch {
		case w.LastError.Valid:
			return "wakeup has an error"
		case !blockedOnMatches(metadataString(issue.Metadata, blockedOnMetadataKey), w) || !w.AgentID.Valid || w.AgentID != issue.AssigneeID:
			return "wakeup no longer matches blocked_on"
		case w.ExpiresAt.Valid && time.Now().After(w.ExpiresAt.Time):
			return "wakeup expired"
		}
		return ""
	}
	has, err := h.Queries.WakeupHasRun(ctx, uuidToString(w.ID))
	if err != nil || has {
		return ""
	}
	return "wakeup ended without a run"
}

// rearmBlockedEntry drops the link and starts the blocked-step run the entry
// skipped. A refusal from admission is recorded on the timeline, not retried.
func (h *Handler) rearmBlockedEntry(ctx context.Context, issue db.Issue, reason string) error {
	// No row means another pass already took the link: it owns the run.
	if _, err := h.Queries.DeleteIssueMetadataKey(ctx, db.DeleteIssueMetadataKeyParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID, Key: blockedEntryLinkKey}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	result := "run started"
	if issue.AssigneeType.Valid && issue.AssigneeType.String == "agent" {
		_, err := h.TaskService.EnqueueTaskForIssueWithHandoff(ctx, issue, "", pgtype.UUID{})
		switch {
		case err == nil, errors.Is(err, service.ErrDuplicatePendingTask):
		default:
			result = "run refused: " + err.Error()
		}
	} else {
		result = "no agent assignee"
	}
	h.recordReviewActivity(ctx, issue.ID, "system", "", activityBlockedEntryRearm, map[string]string{"reason": reason, "result": result})
	return nil
}
