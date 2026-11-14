package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// Step entry gates (CHE-2295): a status write into a step meets the step's
// entry gates, checked in the write transaction against the locked issue and
// the PR facts stored for it. A write is gated when the status it ends on
// differs from the issue's, whether the caller named it or a project move
// remapped it; every writer that sets workflowWrite.system (edge, CI and
// run-budget moves) is not.

const maxEntryOverrideReason = 500

// EntryGateError is a status write refused by a step's block gates.
type EntryGateError struct {
	Step   string
	Failed []issueworkflow.EntryFailure
}

func (e *EntryGateError) Error() string {
	if e.Step == "create" {
		// A refused create reads as one line that starts with the guard's fact.
		parts := make([]string, len(e.Failed))
		for i, f := range e.Failed {
			parts[i] = fmt.Sprintf("%s. Fix: %s", f.Fact, f.Fix)
		}
		return strings.Join(parts, "; ")
	}
	parts := make([]string, len(e.Failed))
	for i, f := range e.Failed {
		parts[i] = fmt.Sprintf("%s (%s; fix: %s)", f.Guard, f.Fact, f.Fix)
	}
	return fmt.Sprintf("status %s is not open to this write: %s", e.Step, strings.Join(parts, "; "))
}

// entryWrite is the caller's part of an entry-gate check: who writes, and the
// reason they give to pass a failed block gate.
type entryWrite struct {
	overrideReason string
	// human is a person's own credential. Task tokens and cloud PATs are
	// machines, whoever they act for, and never override.
	human     bool
	actorType string
	actorID   string
	// warnComment is set by verify when this write recorded the first warning
	// of a step; the caller posts it after the commit, as a comment bumps the
	// issue's revision.
	warnComment string
}

// validEntryOverrideReason trims a reason and reports whether it is 1-500 chars.
func validEntryOverrideReason(reason string) (string, bool) {
	reason = strings.TrimSpace(reason)
	n := utf8.RuneCountInString(reason)
	return reason, n >= 1 && n <= maxEntryOverrideReason
}

// checkEntry evaluates the entry gates of the step the write moves the issue
// into. It runs under the issue lock, in the write transaction.
func (w *workflowWrite) checkEntry(ctx context.Context, h *Handler, q *db.Queries, locked db.Issue, params db.UpdateIssueParams) error {
	w.entry.warnComment = ""
	if w.system || !params.Status.Valid || params.Status.String == locked.Status {
		return nil
	}
	def, _, err := issueworkflow.ForIssue(ctx, q, locked.WorkspaceID, locked.WorkflowID, params.ProjectID)
	if err != nil || def == nil {
		return err
	}
	step, ok := def.Step(params.Status.String)
	if !ok || len(step.Entry) == 0 {
		return nil
	}
	edges := make([]issueworkflow.Edge, len(step.Entry))
	for i, g := range step.Entry {
		edges[i] = issueworkflow.Edge{Guard: g.Guard}
	}
	facts, err := h.loadEdgeFacts(ctx, q, locked, edges, def, time.Time{}, nil)
	if err != nil {
		return err
	}
	return w.applyEntry(ctx, q, locked, params.Status.String, issueworkflow.EvaluateEntry(step.Entry, facts), facts)
}

// applyEntry turns failed gates into a refusal, an override or a warning.
func (w *workflowWrite) applyEntry(ctx context.Context, q *db.Queries, locked db.Issue, stepKey string, failed []issueworkflow.EntryFailure, facts issueworkflow.Facts) error {
	var blocking, warning []issueworkflow.EntryFailure
	for _, f := range failed {
		if f.Mode == issueworkflow.EntryModeBlock {
			blocking = append(blocking, f)
		} else {
			warning = append(warning, f)
		}
	}
	if len(blocking) > 0 {
		reason, valid := validEntryOverrideReason(w.entry.overrideReason)
		if !valid || !w.entry.human || !allOverridable(blocking) {
			return &EntryGateError{Step: stepKey, Failed: failed}
		}
		if err := w.recordEntry(ctx, q, locked, "entry_gate_override", map[string]any{"step": stepKey, "reason": reason, "guards": blocking}); err != nil {
			return err
		}
	}
	if len(warning) == 0 {
		return nil
	}
	head := openPRHeads(facts)
	seen, err := q.CountEntryGateWarnings(ctx, db.CountEntryGateWarningsParams{IssueID: locked.ID, Step: stepKey, Head: head})
	if err != nil {
		return err
	}
	if seen.SameHead > 0 {
		return nil
	}
	if err := w.recordEntry(ctx, q, locked, "entry_gate_warning", map[string]any{"step": stepKey, "head": head, "guards": warning}); err != nil {
		return err
	}
	if seen.Total == 0 {
		w.entry.warnComment = entryWarningText(stepKey, warning)
	}
	return nil
}

func allOverridable(failed []issueworkflow.EntryFailure) bool {
	for _, f := range failed {
		if !f.MemberOverride {
			return false
		}
	}
	return true
}

// openPRHeads is the key that makes a warning once per set of heads: the
// newest head of each open or draft PR in scope, sorted. It reads each PR's
// own head, not its checks-snapshot head, which lags a push.
func openPRHeads(f issueworkflow.Facts) string {
	var heads []string
	for _, p := range f.PRs {
		if p.PRHead != "" && (p.State == "open" || p.State == "draft") {
			heads = append(heads, p.PRHead)
		}
	}
	slices.Sort(heads)
	return strings.Join(slices.Compact(heads), ",")
}

func (w *workflowWrite) recordEntry(ctx context.Context, q *db.Queries, issue db.Issue, action string, details map[string]any) error {
	return recordEntryActivity(ctx, q, issue, w.entry.actorType, w.entry.actorID, action, details)
}

func recordEntryActivity(ctx context.Context, q *db.Queries, issue db.Issue, actorType, actorID, action string, details map[string]any) error {
	body, err := json.Marshal(details)
	if err != nil {
		return err
	}
	params := db.CreateActivityParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, Action: action, Details: body,
		ActorType: pgtype.Text{String: actorType, Valid: actorType != ""},
	}
	if id, err := util.ParseUUID(actorID); err == nil {
		params.ActorID = id
	}
	_, err = q.CreateActivity(ctx, params)
	return err
}

func entryWarningText(step string, warning []issueworkflow.EntryFailure) string {
	parts := make([]string, len(warning))
	for i, f := range warning {
		parts[i] = fmt.Sprintf("%s (%s)", f.Guard, f.Fact)
	}
	return fmt.Sprintf("Entry gate warning: this issue moved to %s while %s. The gate is in warn mode, so the move was allowed.", step, strings.Join(parts, "; "))
}

// postEntryWarning posts the first-warning comment of a committed write and
// returns the issue as it stands after it: the comment bumps the revision, and
// the response and event must carry the revision the next edit expects.
func (h *Handler) postEntryWarning(ctx context.Context, w *workflowWrite, issue db.Issue) db.Issue {
	if w == nil || w.entry.warnComment == "" {
		return issue
	}
	h.noteTriggerFailure(ctx, issue, w.entry.warnComment, pgtype.UUID{})
	current, err := h.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID})
	if err != nil {
		return issue
	}
	return current
}

// createEntryGate checks a new issue against the entry gates of the step it
// lands on. A new issue has no PR and no child, so only a gate on another
// issue's status can hold. A failed block gate refuses the create. A failed
// warn gate is recorded once the issue row exists, in the create transaction:
// the same activity row and first-warning comment as a move into the step.
func (h *Handler) createEntryGate(workspaceID pgtype.UUID, actorType, actorID string) service.CreateEntryGate {
	return func(ctx context.Context, q *db.Queries, step issueworkflow.Step) (service.RecordCreateEntry, error) {
		if len(step.Entry) == 0 {
			return nil, nil
		}
		edges := make([]issueworkflow.Edge, len(step.Entry))
		for i, g := range step.Entry {
			edges[i] = issueworkflow.Edge{Guard: g.Guard}
		}
		statuses, err := h.guardedIssueStatuses(ctx, q, workspaceID, edges)
		if err != nil {
			return nil, err
		}
		var blocking, warning []issueworkflow.EntryFailure
		for _, f := range issueworkflow.EvaluateEntry(step.Entry, issueworkflow.Facts{IssueStatuses: statuses}) {
			if f.Mode == issueworkflow.EntryModeBlock {
				blocking = append(blocking, f)
			} else {
				warning = append(warning, f)
			}
		}
		if len(blocking) > 0 {
			return nil, &EntryGateError{Step: step.StatusKey, Failed: blocking}
		}
		if len(warning) == 0 {
			return nil, nil
		}
		return func(ctx context.Context, q *db.Queries, issue db.Issue) error {
			details := map[string]any{"step": step.StatusKey, "head": "", "guards": warning}
			if err := recordEntryActivity(ctx, q, issue, actorType, actorID, "entry_gate_warning", details); err != nil {
				return err
			}
			_, err := q.CreateComment(ctx, db.CreateCommentParams{
				ID: dbid.NewV7(), IssueID: issue.ID, WorkspaceID: issue.WorkspaceID,
				AuthorType: "system", AuthorID: pgtype.UUID{Valid: true},
				Content: entryWarningText(step.StatusKey, warning), Type: "system",
			})
			return err
		}, nil
	}
}
