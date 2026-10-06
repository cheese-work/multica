package service

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Custom event and condition definitions (CHE-1082 L10). A definition names its
// trigger; each issue the rule applies to gets its own runtime instance (the
// issue_wakeup row), created by the L9 drain on the first matching event or by
// the activation sweep for a condition, and executed by the ordinary dispatch.

// wakeupCustomFireDefault is how many runs a new repeating custom rule may
// start per issue when it names no limit.
const wakeupCustomFireDefault = 20

// withRootFireDefault gives a newly created repeating custom event or condition
// rule the per-issue fire cap it did not set. Only creation calls it: updates,
// built-ins and overrides never gain a default.
func withRootFireDefault(p *WakeupConfigPatch) {
	spec, ok := triggerSpec(p.Trigger)
	if !ok || spec.Kind != wakeupTriggerKindEvent && spec.Kind != wakeupTriggerKindCondition {
		return
	}
	if !p.MaxFires.Set && !(p.Mode.Set && !p.Mode.Null && p.Mode.Value == "once") {
		p.MaxFires = wakeupField[int]{Set: true, Value: wakeupCustomFireDefault}
	}
}

// normalizeWakeupConditionTrigger validates a condition trigger's predicate
// against the workspace and stores its canonical form, so every instance reads
// the same predicate whatever spelling the client sent. A reference the
// workspace does not own is refused as unknown. Other triggers are untouched.
func normalizeWakeupConditionTrigger(ctx context.Context, tx pgx.Tx, ws pgtype.UUID, p *WakeupConfigPatch) error {
	spec, ok := triggerSpec(p.Trigger)
	if !ok || spec.Kind != wakeupTriggerKindCondition {
		return nil
	}
	normalized, _, err := validateCondition(ctx, tx, db.Issue{WorkspaceID: ws}, spec.Condition)
	if err != nil {
		return err
	}
	spec.Condition = normalized
	raw, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	p.Trigger.Value = raw
	return nil
}

// normalizeConditionTriggerReadOnly is the same check for a preview, which has no
// transaction of its own and persists nothing.
func (s *IssueWakeupService) normalizeConditionTriggerReadOnly(ctx context.Context, ws pgtype.UUID, p *WakeupConfigPatch) error {
	if spec, ok := triggerSpec(p.Trigger); !ok || spec.Kind != wakeupTriggerKindCondition {
		return nil
	}
	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	return normalizeWakeupConditionTrigger(ctx, tx, ws, p)
}

// wakeupPreviewExamineLimit bounds how many of a scope's issues a preview
// evaluates.
const wakeupPreviewExamineLimit = 200

// WakeupSatisfiedPreview is what enabling a condition rule would meet at once:
// of the eligible (open, not backlog) issues among the first Examined issues of
// the scope, how many already satisfy the predicate. A new rule acts on those
// the first time it is evaluated. Truncated says the scope has more issues than
// the preview looked at.
type WakeupSatisfiedPreview struct {
	Satisfied, Examined int
	Truncated           bool
}

// previewSatisfied counts the already-satisfied issues of a proposed condition
// rule in the scope it would be written at; nil for any other trigger.
func (s *IssueWakeupService) previewSatisfied(ctx context.Context, ref WakeupScopeRef, trigger wakeupObject) (*WakeupSatisfiedPreview, error) {
	cond := customCondition(trigger)
	if cond == nil {
		return nil, nil
	}
	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	q := s.Tasks.Queries.WithTx(tx)
	var ids []pgtype.UUID
	switch ref.Kind {
	case WakeupScopeIssue:
		ids = []pgtype.UUID{ref.ID}
	case WakeupScopeProject:
		ids, err = q.ListSweepProjectIssues(ctx, db.ListSweepProjectIssuesParams{ProjectID: ref.ID, WorkspaceID: ref.WorkspaceID, After: sweepAfter(pgtype.UUID{}), BatchSize: wakeupPreviewExamineLimit + 1})
	default:
		ids, err = q.ListSweepWorkspaceIssues(ctx, db.ListSweepWorkspaceIssuesParams{WorkspaceID: ref.WorkspaceID, After: sweepAfter(pgtype.UUID{}), BatchSize: wakeupPreviewExamineLimit + 1})
	}
	if err != nil {
		return nil, err
	}
	out := &WakeupSatisfiedPreview{Truncated: len(ids) > wakeupPreviewExamineLimit}
	ids = ids[:min(len(ids), wakeupPreviewExamineLimit)]
	for _, id := range ids {
		issue, err := q.GetIssue(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if active, err := wakeupIssueActive(ctx, q, issue); err != nil {
			return nil, err
		} else if !active || issuestatus.Effective(ctx, q, issue.WorkspaceID, issue.Status) == "backlog" {
			continue
		}
		out.Examined++
		met, _, _, err := evaluateCondition(ctx, tx, q, db.IssueWakeup{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, Condition: cond})
		if err != nil {
			return nil, err
		}
		if met {
			out.Satisfied++
		}
	}
	return out, nil
}
