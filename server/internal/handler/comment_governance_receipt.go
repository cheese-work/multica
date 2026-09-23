package handler

import (
	"log/slog"
	"net/http"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/governance"
	"github.com/multica-ai/multica/server/internal/governance/receipt"
	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// observeGovernanceReceipt is the CHE-685 post-commit, best-effort Jev
// governance receipt-capture hook. It is called from CreateComment and
// UpdateComment AFTER their own transaction has committed and AFTER
// triggerTasksForComment has run — never inside either transaction, and
// never able to influence either's outcome:
//
//   - It reads r.Context() only for the feature-flag EvalContext already
//     attached to the request; it never uses that context to bound its own
//     work (see receipt.Observer.Observe's doc on why).
//   - It runs synchronously, on the request goroutine, AFTER writeJSON has
//     not yet been called by the caller — deliberately: this hook itself is
//     bounded to receipt.Budget (50ms) by Observe, and running it inline
//     rather than on a detached goroutine means a panic inside governance
//     code is caught by chi's Recoverer like the rest of the request instead
//     of needing its own recover() (Observe still has one anyway, as
//     defense in depth for the provider goroutine it spawns internally).
//     50ms inline is an acceptable, bounded addition to p95 comment latency
//     for an off-by-default hook; a detached goroutine would remove even
//     that bound from the request but would reintroduce exactly the
//     "request context already cancelled" coupling risk Observe's doc
//     rejects for the persistence write, since chi's Recoverer / connection
//     lifecycle no longer supervises a goroutine outstanding after the
//     handler returns.
//   - Every outcome — including o.Store or o.Provider being nil, the flag
//     being off, and any persistence failure — is swallowed here or inside
//     Observe. Nothing this function does can change the HTTP response
//     already written for this request, because it never writes to w and
//     never returns a value its caller inspects.
//
// issue and comment are passed by value (not re-fetched) so this hook
// never issues its own extra read against data the caller already holds
// from inside the just-committed request.
func (h *Handler) observeGovernanceReceipt(r *http.Request, issue db.Issue, comment db.Comment, trig receipt.Trigger) {
	if h == nil || h.GovernanceReceipts == nil {
		return
	}
	ctx := r.Context()
	if !featureflags.JevReceiptsEnabled(ctx, h.FeatureFlags) {
		return
	}
	// A machine-authored comment type (status_change, system) never carries
	// a next-work request a human or agent needs routed to them — observing
	// it would only burn the cap-1 slot and a budget window on input the
	// evaluator's own no_candidates/no_spans abstention would reject anyway
	// once real snapshot construction lands. isNoteComment's /note opt-out
	// applies here too, for the same reason triggerTasksForComment skips it.
	if comment.Type != "comment" && comment.Type != "progress_update" {
		return
	}
	if isNoteComment(comment.Content) {
		return
	}

	in, ok := buildGovernanceObservationInput(issue, comment, trig)
	if !ok {
		return
	}

	result := h.GovernanceReceipts.Observe(ctx, in)
	if result.Status != "decided" {
		slog.Info("governance receipt observation did not reach a decision",
			append(logger.RequestAttrs(r),
				"issue_id", uuidToString(issue.ID),
				"comment_id", uuidToString(comment.ID),
				"status", result.Status,
				"shed_reason", string(result.ShedReason),
			)...)
	}
}

// buildGovernanceObservationInput builds the minimal governance.Input this
// D03 delivery observes with. It is deliberately NOT a real eligibility
// snapshot: candidate visibility/permission filtering, evidence-span
// extraction, and mechanical-preparation eligibility are explicitly out of
// scope for CHE-685 (owned by the later C01/C02/D05 deliveries the issue
// names) — building them here would silently take on that scope under a
// receipt-capture ticket. Instead this offers exactly one candidate (the
// issue's current accountable assignee, if any) and one span (the comment's
// own content), which is enough to exercise the full Evaluate contract
// (including its real abstention paths — no_candidates when unassigned,
// low_confidence / model_mismatch / etc. against a scripted fake) without
// claiming to have solved candidate/span construction.
//
// ok is false when the issue has no assignee at all: Evaluate's own
// contract abstains with ReasonNoCandidates for an empty Candidates slice,
// but returning early here avoids spending the cap-1 slot and budget window
// on a call whose outcome is already fully determined before it starts.
func buildGovernanceObservationInput(issue db.Issue, comment db.Comment, trig receipt.Trigger) (receipt.Input, bool) {
	if !issue.AssigneeID.Valid || !issue.AssigneeType.Valid || issue.AssigneeType.String == "" {
		return receipt.Input{}, false
	}

	candidate := governance.Candidate{
		ID:          uuidToString(issue.AssigneeID),
		IsSquad:     issue.AssigneeType.String == "squad",
		DisplayName: issue.AssigneeType.String + ":" + uuidToString(issue.AssigneeID),
		Description: "current accountable assignee for this issue",
	}
	span := governance.Span{
		Text:        comment.Content,
		StartOffset: 0,
		EndOffset:   len(comment.Content),
	}

	return receipt.Input{
		WorkspaceID: issue.WorkspaceID,
		IssueID:     issue.ID,
		CommentID:   comment.ID,
		Trigger:     trig,
		Eval: governance.Input{
			State:            comment.Content,
			Candidates:       []governance.Candidate{candidate},
			Spans:            []governance.Span{span},
			AccountableIndex: 0,
			// MechanicalPreparationEligible deliberately stays false: that
			// eligibility must come from deterministic PR/check evidence
			// (see governance.Input's own doc), which this delivery does
			// not build. Leaving it false only forecloses the
			// agent_preparation action path (it abstains
			// mechanical_preparation_ineligible instead) — mention_owner
			// and every other abstention path are unaffected.
		},
	}, true
}
